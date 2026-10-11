// Package rabbitmq implements the "rabbitmq" protocol.Protocol driver, for
// load testing a RabbitMQ 3.12 or later broker (and other brokers that speak
// AMQP 0-9-1). It uses a pure Go client.
//
// A connection holds many light channels, and every call uses exactly one
// channel. It does not take a second channel while it holds one, so a slow
// call cannot deadlock the rest. Publish reuses a healthy confirm-mode
// channel. Consume, roundtrip and admin open a fresh channel and close it,
// because they leave consumer state behind.
//
// The client library's methods wait for the broker with no deadline of their
// own. Each call starts a watchdog that closes the channel when the budget
// ends, and closes the connection if that close does not finish. Run waits
// for that watchdog before it returns. The next call on the shared connection
// would otherwise meet a close that is still in progress.
//
// Publishing is allowed without a flag, the same as the other message
// brokers. Consuming with ack=ack, and the admin actions that change the
// broker, are not. A RabbitMQ user with limited permissions is the real lock.
package rabbitmq

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/vegaload/vegaload/internal/protocol"
)

// Options are the -opt keys this driver accepts. The command's protocol
// table uses the same list, so the two cannot differ.
var Options = []string{
	"mode", "exchange", "routing_key", "queue", "bind_key", "count",
	"confirm", "mandatory", "persistent", "content_type", "priority",
	"expiration", "headers", "ack", "prefetch", "expect", "action",
	"durable", "auto_delete", "queue_type", "exchange_type",
	"username", "password_env", "vhost", "tls", "connection", "channels",
	"heartbeat", "connection_name", "allow_writes", "allow_admin",
}

const (
	modePublish   = "publish"
	modeConsume   = "consume"
	modeRoundtrip = "roundtrip"
	modeAdmin     = "admin"

	maxCount = 10000
	maxBody  = 16 * 1024 * 1024
	maxName  = 100
)

// Message is one message a consume or roundtrip read.
type Message struct {
	Exchange    string
	RoutingKey  string
	Body        string
	MessageID   string
	Redelivered bool
}

// Reply is what a call read, besides the result. Text is the admin answer.
type Reply struct {
	Messages []Message
	Text     string
}

// Driver is a RabbitMQ protocol.Protocol.
type Driver struct {
	target  protocol.Target
	timeout time.Duration

	host, port    string
	username      string
	password      string
	vhost         string
	useTLS        bool
	skipVerify    bool
	insecure      bool
	perCall       bool
	channels      int
	heartbeat     time.Duration
	connName      string
	allowWrites   bool
	allowAdmin    bool
	script        bool
	urlVhost      string
	urlVhostGiven bool

	mode, exchange, routingKey, queue, bindKey string
	count                                      int
	confirm, mandatory, persistent             bool
	contentType                                string
	priority                                   int
	prioritySet                                bool
	expiration                                 string
	headers                                    amqp.Table
	ack                                        string
	prefetch                                   int
	expect                                     string
	action                                     string
	durable, autoDelete                        bool
	queueType, exchangeType                    string

	link *link
	owns bool
}

type link struct {
	mu        sync.Mutex
	conn      *amqp.Connection
	slots     chan struct{}
	idle      []*pubCh
	dialing   bool
	wait      chan struct{}
	closed    bool
	blocked   string
	lastClose string
	salt      string
	seq       atomic.Uint64
}

type pubCh struct {
	ch    *amqp.Channel
	rets  chan amqp.Return
	close *atomic.Pointer[amqp.Error]
}

// New builds a driver for one target. It does not connect: the first call does.
func New(target protocol.Target, timeout time.Duration) (*Driver, error) {
	return build(target, timeout, nil, false)
}

// NewConn is New for a scenario script. It makes only the connection: the
// broker, the login and the connection options. It needs no mode. The job of
// each call is then set with Call. password is the password, which the script
// already holds, so password_env is not used with it.
func NewConn(target protocol.Target, timeout time.Duration, password *string) (*Driver, error) {
	return build(target, timeout, password, true)
}

// Call returns a Driver for one call of a script: the same connection as d,
// with the job that opts and body describe. Closing it does nothing. Closing
// d closes the connection.
func (d *Driver) Call(opts map[string]string, body []byte, timeout time.Duration) (*Driver, error) {
	nd := &Driver{
		target:      d.target,
		timeout:     timeout,
		host:        d.host,
		port:        d.port,
		username:    d.username,
		password:    d.password,
		vhost:       d.vhost,
		useTLS:      d.useTLS,
		skipVerify:  d.skipVerify,
		insecure:    d.insecure,
		perCall:     d.perCall,
		channels:    d.channels,
		heartbeat:   d.heartbeat,
		connName:    d.connName,
		allowWrites: d.allowWrites,
		allowAdmin:  d.allowAdmin,
		script:      d.script,
		link:        d.link,
		owns:        false,
	}
	nd.target.Body = body
	nd.target.Options = opts
	if err := nd.target.RejectUnknownOptions(Options...); err != nil {
		return nil, fmt.Errorf("rabbitmq: %w", err)
	}
	if err := nd.readJob(); err != nil {
		return nil, err
	}
	return nd, nil
}

func build(target protocol.Target, timeout time.Duration, password *string, connOnly bool) (*Driver, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("rabbitmq: %w", err)
	}
	d := &Driver{
		target:    target,
		timeout:   timeout,
		script:    connOnly,
		owns:      true,
		vhost:     "/",
		channels:  10,
		heartbeat: 10 * time.Second,
		connName:  "vegaload",
		link:      &link{salt: hex.EncodeToString(b)},
	}
	if password != nil {
		d.password = *password
	}
	if err := target.RejectUnknownOptions(Options...); err != nil {
		return nil, fmt.Errorf("rabbitmq: %w", err)
	}
	if err := d.parseURL(); err != nil {
		return nil, err
	}
	if err := d.readConn(password != nil); err != nil {
		return nil, err
	}
	if !connOnly {
		if err := d.readJob(); err != nil {
			return nil, err
		}
	}
	if !d.perCall {
		d.link.slots = make(chan struct{}, d.channels)
	}
	return d, nil
}

// Name implements protocol.Protocol.
func (d *Driver) Name() string { return "rabbitmq" }

// Close implements protocol.Protocol. A Call driver shares the connection
// and closes nothing.
func (d *Driver) Close() error {
	if !d.owns || d.link == nil {
		return nil
	}
	d.link.mu.Lock()
	d.link.closed = true
	c := d.link.conn
	d.link.conn = nil
	d.link.idle = nil
	d.link.mu.Unlock()
	if c != nil && !c.IsClosed() {
		return c.CloseDeadline(time.Now().Add(time.Second))
	}
	return nil
}

// Do implements protocol.Protocol.
func (d *Driver) Do(parent context.Context) (protocol.Result, error) {
	res, _ := d.Run(parent)
	return res, nil
}

// Run runs one call and returns the reply a scenario script reads.
func (d *Driver) Run(parent context.Context) (protocol.Result, Reply) {
	if err := d.refuse(); err != nil {
		return protocol.Result{Err: err}, Reply{}
	}
	ctx, cancel := context.WithTimeout(parent, d.timeout)
	defer cancel()

	var cur atomic.Pointer[amqp.Channel]
	var held atomic.Pointer[amqp.Connection]
	// AfterFunc's stop does not wait for the callback. finished is closed
	// when the callback returns, and exec waits on it when stop returns false.
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(finished)
		d.closeCall(cur.Load(), held.Load())
	})

	sent, got, rep, err := d.exec(ctx, &cur, &held, stop, finished)
	if err != nil {
		return protocol.Result{BytesSent: sent, BytesReceived: got, Err: d.explain(parent, ctx, err)}, rep
	}
	return protocol.Result{Success: true, BytesSent: sent, BytesReceived: got}, rep
}

type leash struct {
	ch   *atomic.Pointer[amqp.Channel]
	conn *atomic.Pointer[amqp.Connection]
}

func (l leash) use(ch *amqp.Channel, conn *amqp.Connection) {
	if l.ch != nil {
		l.ch.Store(ch)
	}
	if l.conn != nil && conn != nil {
		l.conn.Store(conn)
	}
}

// closeCallChannel and closeCallConn are the closes the watchdog uses.
// Tests replace them, while holding closeCallMu, to hold a close open.
var closeCallMu sync.Mutex
var closeCallChannel = func(ch *amqp.Channel) error { return ch.Close() }
var closeCallConn = func(c *amqp.Connection) error {
	return c.CloseDeadline(time.Now().Add(time.Second))
}

func invokeCloseChannel(ch *amqp.Channel) error {
	closeCallMu.Lock()
	fn := closeCallChannel
	closeCallMu.Unlock()
	return fn(ch)
}

func invokeCloseConn(c *amqp.Connection) error {
	closeCallMu.Lock()
	fn := closeCallConn
	closeCallMu.Unlock()
	return fn(c)
}

// closeCall unblocks a library call that has no deadline of its own.
// channel.open and confirm.select run before this call has a channel to
// close, so those calls close the connection at once. A channel close that
// is still going after a second closes the connection too.
func (d *Driver) closeCall(ch *amqp.Channel, c *amqp.Connection) {
	if ch == nil {
		d.dropLink(c, "closed by the call watchdog before a channel was open")
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = invokeCloseChannel(ch)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		d.dropLink(c, "closed by the call watchdog because the channel close did not finish")
	}
}

func (d *Driver) dropLink(c *amqp.Connection, reason string) {
	if c == nil {
		return
	}
	if d.link != nil {
		d.link.retire(c, reason)
	}
	if !c.IsClosed() {
		_ = invokeCloseConn(c)
	}
}

func waitWatch(stop func() bool, finished <-chan struct{}) {
	if !stop() {
		<-finished
	}
}

func (d *Driver) exec(ctx context.Context, cur *atomic.Pointer[amqp.Channel], held *atomic.Pointer[amqp.Connection], stop func() bool, finished <-chan struct{}) (sent, got int64, rep Reply, err error) {
	if !d.perCall {
		if err = d.link.take(ctx); err != nil {
			waitWatch(stop, finished)
			return 0, 0, Reply{}, err
		}
		defer d.link.give()
	}
	// Run does not return until the watchdog has finished, and this call
	// keeps its slot until then. The next call cannot start on a connection
	// this watchdog is still closing.
	defer waitWatch(stop, finished)
	l := leash{ch: cur, conn: held}
	id := d.link.salt + "-" + strconv.FormatUint(d.link.seq.Add(1), 10)
	switch d.mode {
	case modePublish:
		sent, err = d.publish(ctx, l, id)
	case modeConsume:
		got, rep.Messages, err = d.consume(ctx, l, id)
	case modeRoundtrip:
		sent, got, rep.Messages, err = d.roundtrip(ctx, l, id)
	case modeAdmin:
		rep.Text, err = d.admin(ctx, l, id)
	default:
		err = fmt.Errorf("rabbitmq: unknown mode %q", d.mode)
	}
	return sent, got, rep, err
}

func (d *Driver) refuse() error {
	switch d.mode {
	case modeConsume:
		if d.ack == "ack" && !d.allowWrites {
			return fmt.Errorf("rabbitmq: ack=ack removes messages from the queue: %s", d.writesHint())
		}
	case modeAdmin:
		switch d.action {
		case "queue_declare", "exchange_declare", "queue_lifecycle":
			if !d.allowWrites {
				return fmt.Errorf("rabbitmq: action %s needs allow_writes=true: %s", d.action, d.writesHint())
			}
		case "queue_delete", "exchange_delete", "queue_purge":
			if !d.allowWrites || !d.allowAdmin {
				return fmt.Errorf("rabbitmq: action %s needs allow_admin=true as well as allow_writes=true", d.action)
			}
		}
	}
	return nil
}

func (d *Driver) writesHint() string {
	if d.script {
		return "pass allow_writes: true in the connection options"
	}
	return "use -opt allow_writes=true to allow it"
}

func (l *link) take(ctx context.Context) error {
	if l.slots == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case l.slots <- struct{}{}:
		return nil
	}
}

func (l *link) give() {
	if l.slots == nil {
		return
	}
	select {
	case <-l.slots:
	default:
	}
}

func (l *link) blockReason() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.blocked
}

func (l *link) closeReason() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastClose
}

// retire forgets c if it is the shared connection, and records why it is
// closing. The next call dials again instead of using a connection whose
// close is still in progress.
func (l *link) retire(c *amqp.Connection, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if reason != "" {
		l.lastClose = reason
	}
	if l.conn == c {
		l.conn = nil
		l.idle = nil
	}
}

func (d *Driver) connection(ctx context.Context) (*amqp.Connection, bool, error) {
	if d.perCall {
		c, err := d.dial(ctx)
		return c, true, err
	}
	c, err := d.link.get(ctx, func() (*amqp.Connection, error) { return d.dial(ctx) })
	return c, false, err
}

func (l *link) get(ctx context.Context, dial func() (*amqp.Connection, error)) (*amqp.Connection, error) {
	for {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return nil, amqp.ErrClosed
		}
		if l.conn != nil && !l.conn.IsClosed() {
			c := l.conn
			l.mu.Unlock()
			return c, nil
		}
		if l.dialing {
			w := l.wait
			l.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-w:
			}
			continue
		}
		l.dialing = true
		w := make(chan struct{})
		l.wait = w
		l.mu.Unlock()

		c, err := dial()
		l.mu.Lock()
		l.dialing = false
		if err == nil && l.closed {
			l.mu.Unlock()
			_ = c.Close()
			close(w)
			return nil, amqp.ErrClosed
		}
		if err == nil {
			l.arm(c)
			l.conn = c
		}
		close(w)
		l.mu.Unlock()
		return c, err
	}
}

func (l *link) arm(c *amqp.Connection) {
	closes := make(chan *amqp.Error, 1)
	c.NotifyClose(closes)
	blocks := make(chan amqp.Blocking, 1)
	c.NotifyBlocked(blocks)
	go func() {
		for b := range blocks {
			l.mu.Lock()
			if b.Active {
				l.blocked = b.Reason
			} else {
				l.blocked = ""
			}
			l.mu.Unlock()
		}
	}()
	go func() {
		err, ok := <-closes
		l.mu.Lock()
		if ok && err != nil {
			l.lastClose = err.Reason
		}
		if l.conn == c {
			l.conn = nil
		}
		l.mu.Unlock()
	}()
}

func (d *Driver) dial(ctx context.Context) (*amqp.Connection, error) {
	_ = ctx
	scheme := "amqp"
	if d.useTLS {
		scheme = "amqps"
	}
	raw := scheme + "://" + net.JoinHostPort(d.host, d.port) + "/"
	hb := d.heartbeat
	if hb == 0 {
		// The library treats a zero config heartbeat as "10s". A query value
		// of 0 is the only way it keeps zero. The caller's own URL cannot
		// carry a query; this one has no user, password or vhost.
		raw += "?heartbeat=0"
	}
	props := amqp.NewConnectionProperties()
	props.SetClientConnectionName(d.connName)
	cfg := amqp.Config{
		SASL:       []amqp.Authentication{&amqp.PlainAuth{Username: d.username, Password: d.password}},
		Vhost:      d.vhost,
		Heartbeat:  hb,
		Locale:     "en_US",
		Properties: props,
		Dial:       amqp.DefaultDial(d.timeout),
	}
	if d.useTLS {
		cfg.TLSClientConfig = &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ServerName:         d.host,
			InsecureSkipVerify: d.skipVerify,
		}
	}
	return amqp.DialConfig(raw, cfg)
}

func (l *link) borrow(conn *amqp.Connection) (*pubCh, error) {
	l.mu.Lock()
	for len(l.idle) > 0 {
		p := l.idle[len(l.idle)-1]
		l.idle = l.idle[:len(l.idle)-1]
		l.mu.Unlock()
		if p != nil && p.ch != nil && !p.ch.IsClosed() {
			return p, nil
		}
		l.mu.Lock()
	}
	l.mu.Unlock()
	return openPublish(conn)
}

func openPublish(conn *amqp.Connection) (*pubCh, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, err
	}
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		return nil, err
	}
	p := &pubCh{ch: ch, rets: make(chan amqp.Return, maxCount), close: &atomic.Pointer[amqp.Error]{}}
	ch.NotifyReturn(p.rets)
	watchClose(ch, p.close)
	return p, nil
}

func watchClose(ch *amqp.Channel, dst *atomic.Pointer[amqp.Error]) {
	closed := make(chan *amqp.Error, 1)
	ch.NotifyClose(closed)
	go func() {
		for e := range closed {
			if e != nil {
				dst.Store(e)
			}
		}
	}()
}

func (l *link) giveBack(p *pubCh, healthy bool) {
	if p == nil || p.ch == nil {
		return
	}
	if !healthy || p.ch.IsClosed() {
		_ = p.ch.Close()
		return
	}
	l.mu.Lock()
	l.idle = append(l.idle, p)
	l.mu.Unlock()
}

func drainReturns(ch chan amqp.Return) (amqp.Return, bool) {
	var got amqp.Return
	ok := false
	for {
		select {
		case r, open := <-ch:
			if !open {
				return got, ok
			}
			got = r
			ok = true
		default:
			return got, ok
		}
	}
}

func (d *Driver) explain(parent, ctx context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if callTimedOut(ctx, err) {
		msg := fmt.Sprintf("rabbitmq: timed out after %s", d.timeout)
		if reason := d.link.blockReason(); reason != "" {
			msg += ": the broker has blocked this connection (" + reason + ")"
		}
		return errors.New(d.redact(msg))
	}
	if errors.Is(err, amqp.ErrCredentials) || strings.Contains(err.Error(), "Login was refused") {
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= time.Millisecond {
			return errors.New(d.redact(fmt.Sprintf("rabbitmq: timed out after %s", d.timeout)))
		}
		return fmt.Errorf("rabbitmq: the broker refused the login (user %q)", d.username)
	}
	var ae *amqp.Error
	if errors.As(err, &ae) {
		return errors.New(d.redact(fmt.Sprintf("rabbitmq: %s (%d)", ae.Reason, ae.Code)))
	}
	if errors.Is(err, amqp.ErrClosed) {
		reason := d.link.closeReason()
		if reason == "" {
			reason = "the broker closed it"
		}
		return errors.New(d.redact(fmt.Sprintf("rabbitmq: the connection to %s was closed: %s", net.JoinHostPort(d.host, d.port), reason)))
	}
	if isDial(err) {
		return fmt.Errorf("rabbitmq: could not connect to %s: %s", net.JoinHostPort(d.host, d.port), d.redact(err.Error()))
	}
	if strings.HasPrefix(err.Error(), "rabbitmq:") {
		return errors.New(d.redact(err.Error()))
	}
	return fmt.Errorf("rabbitmq: %s", d.redact(err.Error()))
}

func callTimedOut(ctx context.Context, err error) bool {
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	if !isNetTimeout(err) {
		return false
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return false
	}
	return time.Until(deadline) <= time.Millisecond
}

func isNetTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func isDial(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") || strings.Contains(s, "no such host") || strings.HasPrefix(s, "dial ")
}

func (d *Driver) redact(s string) string {
	if d.password == "" || d.password == d.username {
		return s
	}
	return strings.ReplaceAll(s, d.password, "redacted")
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func applyTokens(s, id string, n int) string {
	s = strings.ReplaceAll(s, "{id}", id)
	if n > 0 {
		s = strings.ReplaceAll(s, "{n}", strconv.Itoa(n))
	}
	return s
}

func notRouted(exchange, key string) error {
	return fmt.Errorf("rabbitmq: the message was not routed to any queue (exchange %q, routing key %q): check the binding, or use -opt mandatory=false", cut(exchange, maxName), cut(key, maxName))
}

func nackErr() error {
	return errors.New("rabbitmq: the broker did not accept the message (nack)")
}
