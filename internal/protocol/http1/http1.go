// Package http1 implements a protocol.Protocol driver that speaks plain
// HTTP/1.1. It is the simplest of the four Phase 0 drivers and the
// reference implementation the others follow.
package http1

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/urlerr"
)

// Driver is an HTTP/1.1 protocol.Protocol. It reuses one *http.Client
// (and the connection pool behind it) across every call to Do, so
// repeated iterations against the same target benefit from keep-alive
// the way a real client would.
//
// Driver deliberately disables HTTP/2 on its transport, even if the
// target would otherwise negotiate it over TLS. That is the difference
// between this driver and internal/protocol/http2: a scenario author who
// picks http1 is asking to test the HTTP/1.1 path specifically, and
// should get it even against a server that prefers HTTP/2.
type Driver struct {
	target    protocol.Target
	client    *http.Client
	transport *http.Transport
}

// New returns a ready-to-use Driver for target. timeout bounds each
// individual request; it should normally be shorter than the executor's
// own run duration. A timeout of 0 means no per-request timeout beyond
// ctx's own deadline.
func New(target protocol.Target, timeout time.Duration) *Driver {
	transport := &http.Transport{
		// An empty, non-nil map (rather than the default nil, which
		// lets net/http populate it with an http2 upgrader when
		// available) is what keeps this transport on HTTP/1.1 even
		// against a server that offers HTTP/2 over TLS via ALPN.
		TLSNextProto:    make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: target.InsecureSkipVerify}, //nolint:gosec // opt-in, see protocol.Target
	}
	return &Driver{
		target:    target,
		client:    &http.Client{Transport: transport, Timeout: timeout},
		transport: transport,
	}
}

// Name implements protocol.Protocol.
func (d *Driver) Name() string { return "http1" }

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
		// A request that can't even be constructed (bad URL, bad
		// method) is a configuration problem, not a per-request
		// failure — it will fail identically on every iteration, so
		// stop the run instead of recording the same error forever.
		return protocol.Result{}, fmt.Errorf("http1: %w", urlerr.Inner(err))
	}
	for k, v := range d.target.Headers {
		req.Header.Set(k, v)
	}

	resp, err := d.client.Do(req)
	if err != nil {
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
