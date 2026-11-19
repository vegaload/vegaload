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
	"net/url"
	"slices"
	"time"

	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/tlsconf"
	"github.com/vegaload/vegaload/internal/urlerr"
)

// Options are the -opt keys this driver accepts.
var Options = slices.Clone(tlsconf.Keys)

// The idle pool never shrinks below these, even when target.MaxConns is
// unknown. Every VU of a run shares one Driver, and Go's default of two
// idle connections per host would close and reopen a connection for
// almost every call once more than two VUs run, leaving thousands of
// sockets in TIME_WAIT.
const (
	minIdleConns        = 100
	minIdleConnsPerHost = 2
)

// Driver is an HTTP/1.1 protocol.Protocol. It reuses one *http.Client
// (and the connection pool behind it) across every call to Do, so
// repeated iterations against the same target benefit from keep-alive
// the way a real client would. The pool keeps up to target.MaxConns idle
// connections per host, so each VU in flight can keep its own;
// target.NoConnectionReuse turns keep-alive off instead.
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
//
// New returns an error for a bad URL or bad TLS files. A target that is
// merely unreachable is reported by Do.
func New(target protocol.Target, timeout time.Duration) (*Driver, error) {
	u, err := url.Parse(target.URL)
	if err != nil {
		return nil, fmt.Errorf("http1: parsing target URL: %w", urlerr.Inner(err))
	}
	tlsOn := u.Scheme == "https"
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
		return nil, fmt.Errorf("http1: %w", err)
	}
	transport := &http.Transport{
		// An empty, non-nil map (rather than the default nil, which
		// lets net/http populate it with an http2 upgrader when
		// available) is what keeps this transport on HTTP/1.1 even
		// against a server that offers HTTP/2 over TLS via ALPN.
		TLSNextProto:        make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		MaxIdleConns:        max(target.MaxConns, minIdleConns),
		MaxIdleConnsPerHost: max(target.MaxConns, minIdleConnsPerHost),
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   target.NoConnectionReuse,
	}
	if cfg != nil {
		transport.TLSClientConfig = cfg
	}
	return &Driver{
		target:    target,
		client:    &http.Client{Transport: transport, Timeout: timeout},
		transport: transport,
	}, nil
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

	req, fb := protocol.TraceFirstByte(req)
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
