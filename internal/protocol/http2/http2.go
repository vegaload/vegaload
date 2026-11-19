// Package http2 implements a protocol.Protocol driver that speaks
// HTTP/2, over TLS (the normal case, negotiated via ALPN) or over plain
// TCP ("h2c") when the target is an http:// URL — many internal services
// run h2c precisely to skip TLS termination, and a load tester that only
// understood h2-over-TLS would be unable to test them at all.
//
// Go's standard net/http client only ever negotiates HTTP/2 via TLS, so
// this driver is built directly on golang.org/x/net/http2's Transport
// instead of net/http's default one — that is the whole reason this
// package exists separately from http1.
package http2

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"time"

	"golang.org/x/net/http2"

	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/tlsconf"
	"github.com/vegaload/vegaload/internal/urlerr"
)

// Options are the -opt keys this driver accepts.
var Options = slices.Clone(tlsconf.Keys)

// Driver is an HTTP/2 protocol.Protocol. Every VU shares its one
// transport, which multiplexes their calls as streams over a connection
// per host, so it needs no pool sized from target.MaxConns. With
// target.NoConnectionReuse each call gets a transport of its own,
// closed when the call ends, because HTTP/2 has no per-request way to
// close a connection.
type Driver struct {
	target       protocol.Target
	client       *http.Client
	transport    *http2.Transport
	timeout      time.Duration
	newTransport func() *http2.Transport
}

// New returns a ready-to-use Driver for target. timeout bounds each
// individual request. New returns an error only if target.URL cannot be
// parsed at all — a malformed scheme or host is a configuration problem,
// not a per-request failure.
func New(target protocol.Target, timeout time.Duration) (*Driver, error) {
	u, err := url.Parse(target.URL)
	if err != nil {
		return nil, fmt.Errorf("http2: parsing target URL: %w", urlerr.Inner(err))
	}

	tlsOn := u.Scheme != "http"
	var cfg *tls.Config
	if tlsOn {
		// ServerName stays empty so each request, including a redirect to
		// another host, is checked against that host. tls_server_name sets it.
		cfg = &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: target.InsecureSkipVerify, //nolint:gosec // opt-in, see protocol.Target
		}
	}
	if err := tlsconf.FromOptions(target.Options).Apply(cfg, target.InsecureSkipVerify, tlsOn); err != nil {
		return nil, fmt.Errorf("http2: %w", err)
	}
	h2c := u.Scheme == "http"
	newTransport := func() *http2.Transport {
		t := &http2.Transport{TLSClientConfig: cfg}
		if h2c {
			// h2c: speak HTTP/2 framing directly over a plain TCP
			// connection, with no TLS handshake and therefore no ALPN to
			// negotiate it. AllowHTTP plus a DialTLSContext override that
			// just dials TCP is the documented way to get http2.Transport
			// to do this for a plaintext target.
			t.AllowHTTP = true
			t.DialTLSContext = func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			}
		}
		return t
	}

	transport := newTransport()
	return &Driver{
		target:       target,
		client:       &http.Client{Transport: transport, Timeout: timeout},
		transport:    transport,
		timeout:      timeout,
		newTransport: newTransport,
	}, nil
}

// Name implements protocol.Protocol.
func (d *Driver) Name() string { return "http2" }

// Do implements protocol.Protocol.
func (d *Driver) Do(ctx context.Context) (protocol.Result, error) {
	method := d.target.Method
	if method == "" {
		method = http.MethodGet
	}

	var bodyReader io.Reader
	if len(d.target.Body) > 0 {
		bodyReader = bytes.NewReader(d.target.Body)
	}

	req, err := http.NewRequestWithContext(ctx, method, d.target.URL, bodyReader)
	if err != nil {
		return protocol.Result{}, fmt.Errorf("http2: %w", err)
	}
	for k, v := range d.target.Headers {
		req.Header.Set(k, v)
	}

	client := d.client
	if d.target.NoConnectionReuse {
		t := d.newTransport()
		defer t.CloseIdleConnections()
		client = &http.Client{Transport: t, Timeout: d.timeout}
	}
	req, fb := protocol.TraceFirstByte(req)
	resp, err := client.Do(req)
	if err != nil {
		// Includes the target not actually speaking HTTP/2 (TLS ALPN
		// negotiated http/1.1, or a plain server that isn't h2c) —
		// that is a property of the target, surfaced the same way a
		// connection refused or a timeout would be, as a failed
		// Result rather than a fatal error.
		return protocol.Result{
			Success: false,
			Err:     err,
		}, nil
	}
	defer resp.Body.Close()

	received, _ := io.Copy(io.Discard, resp.Body)

	return protocol.Result{
		Success:       protocol.HTTPStatusOK(resp.StatusCode),
		StatusCode:    resp.StatusCode,
		BytesSent:     int64(len(d.target.Body)),
		BytesReceived: received,
		TTFB:          fb.TTFB(),
	}, nil
}

// Close implements protocol.Protocol.
func (d *Driver) Close() error {
	d.transport.CloseIdleConnections()
	return nil
}
