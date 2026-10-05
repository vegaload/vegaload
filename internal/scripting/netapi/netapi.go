// Package netapi gives VegaLoad's scripting runtimes (internal/scripting/js
// and /python) the real HTTP and WebSocket access FR-CLI-08 asks for: a
// scenario script can make an actual network call and read the response
// (status, headers, body) back into the script, unlike
// internal/protocol's drivers, which exist to repeat one fixed Target and
// discard the response body entirely -- see protocol.Result's doc
// comment, and http1.Driver.Do, which io.Copy's the body straight to
// io.Discard. Without the body coming back, a script has nothing to
// carry from one call into the next, which is the entire point of
// FR-CLI-08.
//
// netapi knows nothing about JavaScript or Python: js.go and python.go
// are responsible for presenting these types in whatever shape their
// language needs (a goja.Object for JS; a proxied RPC call for Python,
// whose subprocess can't hold a Go value at all). It also knows nothing
// about FR-CLI-06's allowlist policy itself -- SafetyCheck is a hook the
// caller (cmd/vegaload) supplies, so the policy decision lives in one
// place (internal/safety) and this package only ever enforces whatever
// decision it's given.
package netapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

// SafetyCheck is called with the bare host netapi is about to connect
// to, before every HTTP request and WebSocket dial. Returning a non-nil
// error refuses the call; the caller (cmd/vegaload) builds this from
// FR-CLI-06's allowlist and the run's -yes/-allow-target flags, enforced
// per call since a scripted target is only known once the script runs --
// see run.go's doc comment for why that differs from protocol-direct
// mode's one pre-run check.
//
// A nil SafetyCheck allows every host unchecked; HTTPClient and Dial
// treat it that way, so a test (or a future caller with no policy of its
// own) doesn't have to supply a no-op function.
type SafetyCheck func(host string) error

// Options are per-call overrides shared by HTTP and WebSocket calls, the
// scripting-layer equivalent of protocol.Target's own option fields.
type Options struct {
	Headers            map[string]string
	InsecureSkipVerify bool
}

// HTTPResponse is what one HTTPClient.Do call hands back to a script --
// enough to inspect a status, read a header, and -- the entire point of
// FR-CLI-08 -- parse a body to carry a value into the next call.
type HTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// HTTPClient is one VU's HTTP client: one underlying *http.Client (and
// its connection pool) reused across every call for the life of the VU,
// the same keep-alive behaviour internal/protocol/http1.Driver gives a
// fixed target, just now available for a script's own ad hoc URLs.
type HTTPClient struct {
	client  *http.Client
	check   SafetyCheck
	timeout time.Duration
}

// NewHTTPClient returns an HTTPClient that runs check (if non-nil)
// against every call's target host before connecting. timeout bounds
// each individual request the same way protocol/http1.New's timeout
// does; 0 means no per-request timeout beyond the context passed to Do.
func NewHTTPClient(check SafetyCheck, timeout time.Duration) *HTTPClient {
	return &HTTPClient{
		client:  &http.Client{Timeout: timeout},
		check:   check,
		timeout: timeout,
	}
}

// Do performs one HTTP request. method defaults to GET if empty; body
// may be nil. opts.InsecureSkipVerify, when set, uses a dedicated
// one-shot transport for this call rather than mutating the client's
// shared one, since a script may hit several hosts with different TLS
// needs across one iteration.
func (c *HTTPClient) Do(ctx context.Context, method, rawURL string, body []byte, opts Options) (*HTTPResponse, error) {
	if method == "" {
		method = http.MethodGet
	}
	if err := checkHost(c.check, rawURL); err != nil {
		return nil, err
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("netapi: %w", err)
	}
	for k, v := range opts.Headers {
		req.Header.Set(k, v)
	}

	client := c.client
	if opts.InsecureSkipVerify {
		client = &http.Client{
			Timeout:   c.timeout,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // opt-in, see Options
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("netapi: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("netapi: reading response body: %w", err)
	}

	return &HTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       data,
	}, nil
}

// Close releases the client's idle connections. Safe to call even if no
// request was ever made.
func (c *HTTPClient) Close() {
	if t, ok := c.client.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

// WSConn is one open WebSocket connection, kept alive across multiple
// Send/Receive calls -- unlike internal/protocol/websocket.Driver, which
// opens, sends at most one message, reads at most one reply, and closes
// again on every single Do call. FR-CLI-08's own example (read frames
// until a flow completes or aborts) needs exactly this: a connection a
// script can read from repeatedly.
type WSConn struct {
	conn *websocket.Conn
}

// Dial opens a WebSocket connection to rawURL (ws:// or wss://), after
// running check against its host. timeout bounds the handshake only --
// Receive has no deadline of its own unless the caller gives one, since
// a long-lived connection reading many frames shouldn't inherit a
// one-shot handshake budget.
func Dial(ctx context.Context, check SafetyCheck, rawURL string, timeout time.Duration, opts Options) (*WSConn, error) {
	if err := checkHost(check, rawURL); err != nil {
		return nil, err
	}

	header := make(http.Header, len(opts.Headers))
	for k, v := range opts.Headers {
		header.Set(k, v)
	}
	dialer := &websocket.Dialer{
		HandshakeTimeout: timeout,
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: opts.InsecureSkipVerify}, //nolint:gosec // opt-in, see Options
	}
	conn, resp, err := dialer.DialContext(ctx, rawURL, header)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return nil, fmt.Errorf("netapi: dialing %s: %w", rawURL, err)
	}
	return &WSConn{conn: conn}, nil
}

// Send writes one message. text selects a text frame (as opposed to
// binary) -- scripts sending JSON, the common case, want text.
func (c *WSConn) Send(ctx context.Context, data []byte, text bool) error {
	if dl, ok := ctx.Deadline(); ok {
		_ = c.conn.SetWriteDeadline(dl)
	} else {
		_ = c.conn.SetWriteDeadline(time.Time{})
	}
	msgType := websocket.BinaryMessage
	if text {
		msgType = websocket.TextMessage
	}
	if err := c.conn.WriteMessage(msgType, data); err != nil {
		return fmt.Errorf("netapi: sending websocket message: %w", err)
	}
	return nil
}

// Receive waits for the next message and returns its payload. closed is
// true, with a nil error, on a normal close -- so a script's "read
// frames until it completes" loop can just check for that instead of
// treating the expected end of the stream as an error. A non-nil error
// means something other than a clean close: a read past ctx's deadline,
// or a protocol error.
//
// Receive honors ctx's deadline if it has one; otherwise it blocks until
// a message arrives, the connection closes, or ctx is cancelled for some
// other reason (the run ending).
func (c *WSConn) Receive(ctx context.Context) (data []byte, closed bool, err error) {
	if dl, ok := ctx.Deadline(); ok {
		_ = c.conn.SetReadDeadline(dl)
	} else {
		_ = c.conn.SetReadDeadline(time.Time{})
	}

	type result struct {
		msg []byte
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		_, msg, err := c.conn.ReadMessage()
		resCh <- result{msg, err}
	}()

	select {
	case r := <-resCh:
		if r.err != nil {
			if websocket.IsCloseError(r.err,
				websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				return nil, true, nil
			}
			return nil, false, fmt.Errorf("netapi: receiving websocket message: %w", r.err)
		}
		return r.msg, false, nil
	case <-ctx.Done():
		return nil, false, fmt.Errorf("netapi: receiving websocket message: %w", ctx.Err())
	}
}

// Close closes the connection, sending a normal-closure frame on a
// best-effort basis first. Safe to call more than once.
func (c *WSConn) Close() error {
	_ = c.conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	return c.conn.Close()
}

// checkHost runs check against rawURL's host, if check is non-nil.
func checkHost(check SafetyCheck, rawURL string) error {
	if check == nil {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("netapi: parsing URL %q: %w", rawURL, err)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("netapi: URL %q has no host", rawURL)
	}
	return check(host)
}
