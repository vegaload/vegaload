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
	"time"

	"golang.org/x/net/http2"

	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/urlerr"
)

// Driver is an HTTP/2 protocol.Protocol.
type Driver struct {
	target    protocol.Target
	client    *http.Client
	transport *http2.Transport
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

	transport := &http2.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: target.InsecureSkipVerify}, //nolint:gosec // opt-in, see protocol.Target
	}

	if u.Scheme == "http" {
		// h2c: speak HTTP/2 framing directly over a plain TCP
		// connection, with no TLS handshake and therefore no ALPN to
		// negotiate it. AllowHTTP plus a DialTLSContext override that
		// just dials TCP is the documented way to get http2.Transport
		// to do this for a plaintext target.
		transport.AllowHTTP = true
		transport.DialTLSContext = func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	}

	return &Driver{
		target:    target,
		client:    &http.Client{Transport: transport, Timeout: timeout},
		transport: transport,
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

	resp, err := d.client.Do(req)
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
		Success:       resp.StatusCode >= 200 && resp.StatusCode < 400,
		StatusCode:    resp.StatusCode,
		BytesSent:     int64(len(d.target.Body)),
		BytesReceived: received,
	}, nil
}

// Close implements protocol.Protocol.
func (d *Driver) Close() error {
	d.transport.CloseIdleConnections()
	return nil
}
