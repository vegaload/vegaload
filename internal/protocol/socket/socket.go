// Package socket implements the "tcp" and "udp" protocol.Protocol drivers:
// raw sockets, for a service that speaks its own wire protocol (a custom
// binary protocol, SMTP, Redis, a syslog or DNS port) and has no driver of
// its own.
//
// Like the WebSocket driver, each call to Do opens its own connection,
// sends the body once, optionally reads a reply, and closes. That measures
// connect, send and reply time under load. It is not a long-lived session.
//
// Options (set with -opt key=value):
//
//	escape   true (default) or false. When true, the text in -body,
//	         expect and until may use \n \r \t \0 \\ and \xNN.
//	read     TCP only. Read exactly this many bytes after sending.
//	until    TCP only. Read until this text has been received.
//	expect   The reply must contain this text. With TCP and neither read
//	         nor until, it reads until the text appears.
//	max      TCP only. Most bytes to read (default 1048576).
//	tls      TCP only. true to wrap the connection in TLS.
//	reply    UDP only. true to wait for one reply datagram.
package socket

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/urlerr"
)

const (
	defaultMaxRead = 1 << 20
	maxDatagram    = 64 * 1024
)

// TCPOptions and UDPOptions are the -opt keys each driver accepts. The
// command's protocol table uses the same lists, so the two cannot differ.
var (
	TCPOptions = []string{"escape", "expect", "max", "read", "tls", "until"}
	UDPOptions = []string{"escape", "expect", "reply"}
)

// Driver is a raw TCP or UDP protocol.Protocol.
type Driver struct {
	network string // "tcp" or "udp"
	addr    string // host:port
	host    string
	target  protocol.Target
	timeout time.Duration

	body   []byte
	expect []byte
	until  []byte
	read   int  // TCP: exact bytes to read, 0 if not set
	max    int  // TCP: read cap
	useTLS bool // TCP
	reply  bool // UDP: wait for a datagram
}

// NewTCP returns a Driver for target. target.URL is tcp://host:port or a
// bare host:port.
func NewTCP(target protocol.Target, timeout time.Duration) (*Driver, error) {
	return newDriver("tcp", target, timeout)
}

// NewUDP returns a Driver for target. target.URL is udp://host:port or a
// bare host:port. A UDP run needs a body to send.
func NewUDP(target protocol.Target, timeout time.Duration) (*Driver, error) {
	return newDriver("udp", target, timeout)
}

func newDriver(network string, target protocol.Target, timeout time.Duration) (*Driver, error) {
	d := &Driver{network: network, target: target, timeout: timeout}

	raw := target.URL
	if !strings.Contains(raw, "://") {
		raw = network + "://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: parsing target: %w", network, urlerr.Inner(err))
	}
	if u.Scheme != network {
		return nil, fmt.Errorf("%s: unsupported scheme %q, want %s:// or host:port", network, u.Scheme, network)
	}
	if u.Hostname() == "" || u.Port() == "" {
		return nil, fmt.Errorf("%s: target %q needs a host and a port, such as %s://localhost:9000", network, urlerr.Mask(target.URL), network)
	}
	d.host = u.Hostname()
	d.addr = net.JoinHostPort(u.Hostname(), u.Port())

	allowed := UDPOptions
	if network == "tcp" {
		allowed = TCPOptions
	}
	if err := target.RejectUnknownOptions(allowed...); err != nil {
		return nil, fmt.Errorf("%s: %w", network, err)
	}

	esc, err := target.OptionBool("escape", true)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", network, err)
	}
	text := func(s string) ([]byte, error) {
		if !esc {
			return []byte(s), nil
		}
		return Unescape(s)
	}

	if d.body, err = text(string(target.Body)); err != nil {
		return nil, fmt.Errorf("%s: -body: %w", network, err)
	}
	if v, ok := target.Options["expect"]; ok {
		if d.expect, err = text(v); err != nil {
			return nil, fmt.Errorf("%s: option expect: %w", network, err)
		}
	}

	switch network {
	case "tcp":
		if v, ok := target.Options["until"]; ok {
			if d.until, err = text(v); err != nil {
				return nil, fmt.Errorf("tcp: option until: %w", err)
			}
		}
		if d.read, err = target.OptionInt("read", 0); err != nil {
			return nil, fmt.Errorf("tcp: %w", err)
		}
		if d.max, err = target.OptionInt("max", defaultMaxRead); err != nil {
			return nil, fmt.Errorf("tcp: %w", err)
		}
		if d.useTLS, err = target.OptionBool("tls", false); err != nil {
			return nil, fmt.Errorf("tcp: %w", err)
		}
		if d.read < 0 || d.max <= 0 {
			return nil, errors.New("tcp: read must not be negative and max must be positive")
		}
		if d.read > d.max {
			return nil, fmt.Errorf("tcp: read=%d is more than max=%d", d.read, d.max)
		}
		if d.read > 0 && len(d.until) > 0 {
			return nil, errors.New("tcp: use read or until, not both")
		}
	case "udp":
		if len(d.body) == 0 {
			return nil, errors.New("udp: a datagram to send is required (pass -body)")
		}
		if d.reply, err = target.OptionBool("reply", false); err != nil {
			return nil, fmt.Errorf("udp: %w", err)
		}
		if len(d.expect) > 0 {
			d.reply = true
		}
	}
	return d, nil
}

// Name implements protocol.Protocol.
func (d *Driver) Name() string { return d.network }

// Do implements protocol.Protocol.
//
// -timeout is one budget for the whole call: dial, send and read together.
// It is applied once, as a context around the call, the way the gRPC driver
// does it. The same context ends the call when the run ends, because
// context.AfterFunc closes the socket as soon as the context is done.
func (d *Driver) Do(parent context.Context) (protocol.Result, error) {
	res, _ := d.Run(parent)
	return res, nil
}

// Run is Do, and it also returns the reply that was read, even when the
// call failed after some bytes came in. A load test does not need the
// reply. A scenario script does.
func (d *Driver) Run(parent context.Context) (protocol.Result, []byte) {
	ctx, cancel := context.WithTimeout(parent, d.timeout)
	defer cancel()

	var (
		res   protocol.Result
		reply []byte
	)
	if d.network == "udp" {
		res, reply = d.doUDP(ctx)
	} else {
		res, reply = d.doTCP(ctx)
	}
	// Say "timeout" only when -timeout ran out, and not when the run ended.
	// The clock is checked, not ctx.Err(): the timer that cancels a context
	// can fire a few milliseconds after its deadline.
	if !res.Success && !ended(parent) && ended(ctx) {
		res.Err = fmt.Errorf("%s: no answer within -timeout %s: %w", d.network, d.timeout, res.Err)
	}
	return res, reply
}

// ended reports whether ctx is done, or its deadline has passed.
func ended(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	dl, ok := ctx.Deadline()
	return ok && !time.Now().Before(dl)
}

// Close implements protocol.Protocol. Do opens and closes its own
// connection every call, so Driver holds nothing to release.
func (d *Driver) Close() error { return nil }

func fail(sent, got int64, err error) protocol.Result {
	return protocol.Result{Success: false, BytesSent: sent, BytesReceived: got, Err: err}
}

func (d *Driver) doTCP(ctx context.Context) (protocol.Result, []byte) {
	var dialer net.Dialer // no Timeout of its own: ctx carries the one budget
	var (
		conn net.Conn
		err  error
	)
	if d.useTLS {
		td := &tls.Dialer{NetDialer: &dialer, Config: &tls.Config{
			ServerName:         d.host,
			InsecureSkipVerify: d.target.InsecureSkipVerify, //nolint:gosec // opt-in, see protocol.Target
		}}
		conn, err = td.DialContext(ctx, "tcp", d.addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", d.addr)
	}
	if err != nil {
		return fail(0, 0, err), nil
	}
	defer conn.Close()
	// A run that ends mid-call closes the connection so Do returns now.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	var sent int64
	if len(d.body) > 0 {
		n, err := conn.Write(d.body)
		sent = int64(n)
		if err != nil {
			return fail(sent, 0, err), nil
		}
	}

	if d.read == 0 && len(d.until) == 0 && len(d.expect) == 0 {
		return protocol.Result{Success: true, BytesSent: sent}, nil
	}

	reply, err := d.readTCP(conn)
	got := int64(len(reply))
	if err != nil {
		return fail(sent, got, err), reply
	}
	if len(d.expect) > 0 && !bytes.Contains(reply, d.expect) {
		return fail(sent, got, fmt.Errorf("tcp: reply did not contain %q", d.expect)), reply
	}
	return protocol.Result{Success: true, BytesSent: sent, BytesReceived: got}, reply
}

// readTCP reads the reply the options ask for.
func (d *Driver) readTCP(conn net.Conn) ([]byte, error) {
	if d.read > 0 {
		buf := make([]byte, d.read)
		n, err := io.ReadFull(conn, buf)
		if err != nil {
			return buf[:n], fmt.Errorf("tcp: wanted %d bytes, got %d: %w", d.read, n, err)
		}
		return buf, nil
	}

	// Read until the delimiter (until, else expect) has arrived.
	delim := d.until
	if len(delim) == 0 {
		delim = d.expect
	}
	var reply []byte
	chunk := make([]byte, 4096)
	for {
		if len(reply) >= d.max {
			return reply, fmt.Errorf("tcp: read max=%d bytes without seeing %q", d.max, delim)
		}
		// Never read past max, so the cap is exact.
		n, err := conn.Read(chunk[:min(len(chunk), d.max-len(reply))])
		reply = append(reply, chunk[:n]...)
		if bytes.Contains(reply, delim) {
			return reply, nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return reply, fmt.Errorf("tcp: connection closed before %q arrived", delim)
			}
			return reply, err
		}
	}
}

func (d *Driver) doUDP(ctx context.Context) (protocol.Result, []byte) {
	var dialer net.Dialer // no Timeout of its own: ctx carries the one budget
	conn, err := dialer.DialContext(ctx, "udp", d.addr)
	if err != nil {
		return fail(0, 0, err), nil
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	n, err := conn.Write(d.body)
	sent := int64(n)
	if err != nil {
		return fail(sent, 0, err), nil
	}
	if !d.reply {
		return protocol.Result{Success: true, BytesSent: sent}, nil
	}

	buf := make([]byte, maxDatagram)
	n, err = conn.Read(buf)
	if err != nil {
		return fail(sent, 0, fmt.Errorf("udp: no reply: %w", err)), nil
	}
	reply := buf[:n]
	if len(d.expect) > 0 && !bytes.Contains(reply, d.expect) {
		return fail(sent, int64(n), fmt.Errorf("udp: reply did not contain %q", d.expect)), reply
	}
	return protocol.Result{Success: true, BytesSent: sent, BytesReceived: int64(n)}, reply
}

// Unescape turns \n, \r, \t, \0, \\ and \xNN in s into the bytes they
// stand for. Any other backslash sequence is an error, so a typo is not
// sent silently.
func Unescape(s string) ([]byte, error) {
	if !strings.Contains(s, `\`) {
		return []byte(s), nil
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			out = append(out, c)
			continue
		}
		i++
		if i >= len(s) {
			return nil, errors.New(`text ends with a lone backslash (write \\ for one backslash)`)
		}
		switch s[i] {
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case '0':
			out = append(out, 0)
		case '\\':
			out = append(out, '\\')
		case 'x':
			if i+2 >= len(s) {
				return nil, errors.New(`\x needs two hex digits`)
			}
			hi, ok1 := hexVal(s[i+1])
			lo, ok2 := hexVal(s[i+2])
			if !ok1 || !ok2 {
				return nil, fmt.Errorf(`\x%s is not two hex digits`, s[i+1:i+3])
			}
			out = append(out, hi<<4|lo)
			i += 2
		default:
			return nil, fmt.Errorf(`unknown escape \%c (use \n \r \t \0 \\ or \xNN, or pass -opt escape=false)`, s[i])
		}
	}
	return out, nil
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
