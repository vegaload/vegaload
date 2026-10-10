// Package mqtt implements the "mqtt" protocol.Protocol driver, for load
// testing an MQTT 3.1.1 broker. It uses the Eclipse Paho client.
//
// Each call to Do opens its own connection, does one job, and closes. That
// measures connect, publish and delivery time under load. It is not a
// long-lived session. The job is chosen with -opt mode=...:
//
//	publish    connect, publish the body, wait for the broker's ack. This
//	           is the default.
//	subscribe  connect, subscribe, wait for count messages.
//	roundtrip  connect, subscribe, publish the body, wait until this
//	           iteration's own message comes back. This measures delivery
//	           through the broker. The body must contain {id}, so the driver
//	           can tell its own message from another user's.
//
// Options (set with -opt key=value):
//
//	mode           publish (default), subscribe, or roundtrip.
//	topic          The topic. Required. {id} is replaced by this
//	               iteration's unique client id, so each iteration can
//	               have its own topic. Publish and roundtrip topics must
//	               not have wildcards.
//	qos            0 (default), 1, or 2.
//	retain         true to publish the message as retained.
//	username       The user name.
//	password_env   The name of an environment variable that holds the
//	               password. It needs username. The password is never put
//	               on the command line.
//	client_id      A prefix for the client ids (default "vegaload"). Each
//	               iteration adds a unique suffix, because a broker closes
//	               an older connection that has the same id.
//	count          subscribe and roundtrip: messages to wait for (default 1).
//	expect         subscribe: every message must contain this text.
//	keepalive      Keep-alive time, such as 30s (default 30s).
//
// Every iteration asks for a clean session, so the broker keeps nothing
// between iterations. The body is the message payload. {id} in it is
// replaced too. Use an mqtts:// target for TLS.
package mqtt

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/urlerr"
)

// Options are the -opt keys this driver accepts. The command's protocol
// table uses the same list, so the two cannot differ.
var Options = []string{
	"mode", "topic", "qos", "retain", "username", "password_env",
	"client_id", "count", "expect", "keepalive",
}

const (
	modePublish   = "publish"
	modeSubscribe = "subscribe"
	modeRoundtrip = "roundtrip"

	maxCount = 100000
)

// Driver is an MQTT protocol.Protocol.
type Driver struct {
	broker   string // tcp://host:port or ssl://host:port, as Paho wants it
	target   protocol.Target
	timeout  time.Duration
	mode     string
	topic    string
	qos      byte
	retain   bool
	username string
	password string
	prefix   string
	count    int
	expect   []byte
	alive    time.Duration

	salt string // random, per Driver, so two runs never share client ids
	seq  atomic.Uint64
}

// New returns a ready-to-use Driver for target. target.URL is mqtt://host
// or mqtts://host, with an optional :port (1883, or 8883 for mqtts).
//
// New returns an error only for a configuration problem, never for the
// broker being unreachable, which Do reports per call.
func New(target protocol.Target, timeout time.Duration) (*Driver, error) {
	return newDriver(target, timeout, nil)
}

// NewWithPassword is New for a caller that already holds the password, such
// as a scenario script that read it from an environment variable it was
// given. The command line never uses it: there the password is read from
// the environment variable that password_env names. password_env and a
// password given here are not allowed together.
func NewWithPassword(target protocol.Target, timeout time.Duration, password string) (*Driver, error) {
	return newDriver(target, timeout, &password)
}

func newDriver(target protocol.Target, timeout time.Duration, password *string) (*Driver, error) {
	d := &Driver{target: target, timeout: timeout}

	u, err := url.Parse(target.URL)
	if err != nil {
		return nil, fmt.Errorf("mqtt: parsing target URL: %w", urlerr.Inner(err))
	}
	port := u.Port()
	switch u.Scheme {
	case "mqtt":
		if port == "" {
			port = "1883"
		}
		d.broker = "tcp://" + net.JoinHostPort(u.Hostname(), port)
	case "mqtts":
		if port == "" {
			port = "8883"
		}
		d.broker = "ssl://" + net.JoinHostPort(u.Hostname(), port)
	default:
		return nil, fmt.Errorf("mqtt: unsupported scheme %q, want mqtt:// or mqtts://", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("mqtt: target %q has no host", target.URL)
	}

	if err := target.RejectUnknownOptions(Options...); err != nil {
		return nil, fmt.Errorf("mqtt: %w", err)
	}

	d.mode = target.Option("mode", modePublish)
	switch d.mode {
	case modePublish, modeSubscribe, modeRoundtrip:
	default:
		return nil, fmt.Errorf("mqtt: mode=%q, want publish, subscribe, or roundtrip", d.mode)
	}

	d.topic = target.Option("topic", "")
	if d.topic == "" {
		return nil, errors.New("mqtt: a topic is required (pass -opt topic=...)")
	}
	if d.mode != modeSubscribe && strings.ContainsAny(d.topic, "+#") {
		return nil, fmt.Errorf("mqtt: topic %q has a wildcard, which is only allowed in subscribe mode", d.topic)
	}

	qos, err := target.OptionInt("qos", 0)
	if err != nil {
		return nil, fmt.Errorf("mqtt: %w", err)
	}
	if qos < 0 || qos > 2 {
		return nil, fmt.Errorf("mqtt: qos=%d, want 0, 1, or 2", qos)
	}
	d.qos = byte(qos)

	if d.retain, err = target.OptionBool("retain", false); err != nil {
		return nil, fmt.Errorf("mqtt: %w", err)
	}
	if d.count, err = target.OptionInt("count", 1); err != nil {
		return nil, fmt.Errorf("mqtt: %w", err)
	}
	if d.count < 1 || d.count > maxCount {
		return nil, fmt.Errorf("mqtt: count=%d, want 1 to %d", d.count, maxCount)
	}
	if d.alive, err = target.OptionDuration("keepalive", 30*time.Second); err != nil {
		return nil, fmt.Errorf("mqtt: %w", err)
	}
	if d.alive < time.Second {
		return nil, fmt.Errorf("mqtt: keepalive=%s, want at least 1s", d.alive)
	}

	d.username = target.Option("username", "")
	if password != nil {
		if target.Option("password_env", "") != "" {
			return nil, errors.New("mqtt: give a password or password_env, not both")
		}
		if d.username == "" {
			return nil, errors.New("mqtt: a password needs username: a password is only sent together with a user name")
		}
		d.password = *password
	} else if env := target.Option("password_env", ""); env != "" {
		if d.username == "" {
			return nil, errors.New("mqtt: password_env needs username: a password is only sent together with a user name")
		}
		pw, ok := os.LookupEnv(env)
		if !ok || pw == "" {
			return nil, fmt.Errorf("mqtt: password_env=%s, but that environment variable is not set", env)
		}
		d.password = pw
	}
	d.prefix = target.Option("client_id", "vegaload")
	if v, ok := target.Options["expect"]; ok {
		d.expect = []byte(v)
	}
	if len(d.expect) > 0 && d.mode != modeSubscribe {
		return nil, errors.New("mqtt: expect is only used in subscribe mode")
	}
	if d.mode == modeRoundtrip && !bytes.Contains(target.Body, []byte("{id}")) {
		return nil, errors.New("mqtt: roundtrip needs {id} in -body, so each iteration can tell its own message from another user's")
	}

	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("mqtt: making a client id: %w", err)
	}
	d.salt = hex.EncodeToString(b[:])
	return d, nil
}

// Name implements protocol.Protocol.
func (d *Driver) Name() string { return "mqtt" }

// Close implements protocol.Protocol. Do opens and closes its own
// connection every call, so Driver holds nothing to release.
func (d *Driver) Close() error { return nil }

// clientID returns a new, unique id for one iteration.
func (d *Driver) clientID() string {
	return d.prefix + "-" + d.salt + "-" + strconv.FormatUint(d.seq.Add(1), 36)
}

func fail(sent, got int64, err error) protocol.Result {
	return protocol.Result{Success: false, BytesSent: sent, BytesReceived: got, Err: err}
}

var errTimeout = errors.New("mqtt: timed out")

// wait waits for a Paho token, the deadline, or the end of the run.
func wait(ctx context.Context, tok paho.Token, deadline time.Time, what string) error {
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case <-tok.Done():
		if err := tok.Error(); err != nil {
			return fmt.Errorf("mqtt: %s: %w", what, err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return fmt.Errorf("%w waiting for %s", errTimeout, what)
	}
}

// Message is one message that came from the broker.
type Message struct {
	Topic   string
	Payload []byte
}

// Do implements protocol.Protocol.
func (d *Driver) Do(parent context.Context) (protocol.Result, error) {
	res, _ := d.Run(parent)
	return res, nil
}

// Run is Do, and it also returns the messages that were received: the
// messages that satisfied a subscribe, or the message that came back in a
// roundtrip. A load test does not need them. A scenario script does.
func (d *Driver) Run(parent context.Context) (protocol.Result, []Message) {
	// One budget for the whole call. Cancelling ctx also closes the
	// connection (see open), so the end of the run, or the timeout, stops
	// a connect that is still in progress.
	ctx, cancel := context.WithTimeout(parent, d.timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()

	id := d.clientID()
	idb := []byte(id)
	topic := strings.ReplaceAll(d.topic, "{id}", id)
	payload := d.target.Body
	if bytes.Contains(payload, []byte("{id}")) {
		payload = bytes.ReplaceAll(payload, []byte("{id}"), idb)
	}

	opts := paho.NewClientOptions().
		AddBroker(d.broker).
		SetClientID(id).
		SetCleanSession(true).
		SetKeepAlive(d.alive).
		SetConnectTimeout(d.timeout).
		SetWriteTimeout(d.timeout).
		SetAutoReconnect(false).
		SetConnectRetry(false).
		SetOrderMatters(false).
		SetCustomOpenConnectionFn(open(ctx)).
		SetTLSConfig(&tls.Config{InsecureSkipVerify: d.target.InsecureSkipVerify}) //nolint:gosec // opt-in, see protocol.Target
	if d.username != "" {
		opts.SetUsername(d.username)
		opts.SetPassword(d.password)
	}

	// Messages that arrive for our subscription. In roundtrip mode only
	// this iteration's own message is kept, so other users' traffic on a
	// shared topic can never crowd it out. A surplus message is dropped,
	// so a busy topic can never block Paho's delivery.
	msgs := make(chan Message, d.count)
	handler := func(_ paho.Client, m paho.Message) {
		p := m.Payload()
		if d.mode == modeRoundtrip && !bytes.Equal(p, payload) {
			return
		}
		select {
		case msgs <- Message{Topic: m.Topic(), Payload: append([]byte(nil), p...)}:
		default:
		}
	}

	c := paho.NewClient(opts)
	if err := wait(ctx, c.Connect(), deadline, "the connection"); err != nil {
		return fail(0, 0, err), nil
	}
	defer c.Disconnect(100)

	var sent, got int64
	var recv []Message
	subscribe := func() error {
		tok := c.Subscribe(topic, d.qos, handler)
		if err := wait(ctx, tok, deadline, "the subscription"); err != nil {
			return err
		}
		if st, ok := tok.(*paho.SubscribeToken); ok {
			if code, found := st.Result()[topic]; found && code == 0x80 {
				return fmt.Errorf("mqtt: the broker refused the subscription to %q", topic)
			}
		}
		return nil
	}
	publish := func() error {
		if err := wait(ctx, c.Publish(topic, d.qos, d.retain, payload), deadline, "the publish"); err != nil {
			return err
		}
		sent = int64(len(payload))
		return nil
	}

	switch d.mode {
	case modePublish:
		if err := publish(); err != nil {
			return fail(sent, got, err), recv
		}

	case modeSubscribe:
		if err := subscribe(); err != nil {
			return fail(sent, got, err), recv
		}
		for n := 0; n < d.count; n++ {
			m, err := d.receive(ctx, msgs, deadline)
			if err != nil {
				return fail(sent, got, err), recv
			}
			got += int64(len(m.Payload))
			recv = append(recv, m)
			if len(d.expect) > 0 && !bytes.Contains(m.Payload, d.expect) {
				return fail(sent, got, fmt.Errorf("mqtt: message %q did not contain %q", clip(m.Payload), d.expect)), recv
			}
		}

	case modeRoundtrip:
		if err := subscribe(); err != nil {
			return fail(sent, got, err), recv
		}
		if err := publish(); err != nil {
			return fail(sent, got, err), recv
		}
		for n := 0; n < d.count; n++ {
			m, err := d.receive(ctx, msgs, deadline)
			if err != nil {
				return fail(sent, got, err), recv
			}
			got += int64(len(m.Payload))
			recv = append(recv, m)
		}
	}
	return protocol.Result{Success: true, BytesSent: sent, BytesReceived: got}, recv
}

// open makes Paho dial with ctx instead of with its own dialer. When ctx
// ends, by the run ending, the timeout, or Do returning, the connection is
// closed. That stops a connect or a CONNACK wait that is still in progress,
// which Paho's default dialer would let run to the end of its own timeout.
func open(ctx context.Context) func(*url.URL, paho.ClientOptions) (net.Conn, error) {
	return func(u *url.URL, o paho.ClientOptions) (net.Conn, error) {
		var dialer net.Dialer
		var (
			conn net.Conn
			err  error
		)
		switch u.Scheme {
		case "tcp":
			conn, err = dialer.DialContext(ctx, "tcp", u.Host)
		case "ssl":
			td := tls.Dialer{NetDialer: &dialer, Config: o.TLSConfig}
			conn, err = td.DialContext(ctx, "tcp", u.Host)
		default:
			return nil, fmt.Errorf("mqtt: unsupported broker scheme %q", u.Scheme)
		}
		if err != nil {
			return nil, err
		}
		context.AfterFunc(ctx, func() { _ = conn.Close() })
		return conn, nil
	}
}

// receive waits for the next message.
func (d *Driver) receive(ctx context.Context, msgs <-chan Message, deadline time.Time) (Message, error) {
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case m := <-msgs:
		return m, nil
	case <-ctx.Done():
		return Message{}, ctx.Err()
	case <-t.C:
		return Message{}, fmt.Errorf("%w waiting for a message", errTimeout)
	}
}

func clip(b []byte) string {
	if len(b) > 40 {
		return string(b[:40]) + "..."
	}
	return string(b)
}
