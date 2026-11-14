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
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/vegaload/vegaload/internal/failure"
	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/urlerr"
)

// RunEnded reports whether the run is over: ctx is done, or its deadline
// has passed. The timer that cancels a ctx at its deadline can fire a few
// milliseconds late, and ctx.Err() stays nil until it does. Scripts use
// this to tell a call that was cut off by the end of the run from a real
// failure, in that gap too.
func RunEnded(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if ctx.Err() != nil {
		return true
	}
	dl, ok := ctx.Deadline()
	return ok && !time.Now().Before(dl)
}

// CheckRecorder receives the outcome of every check a scenario script
// makes (FR-CLI-12): the check's name and whether it passed. The report
// Collector implements it. A nil recorder is allowed everywhere it is
// accepted, and means checks are evaluated but not counted.
type CheckRecorder interface {
	RecordCheck(name string, passed bool)
}

// StepRecorder receives every named step a scenario script runs (FR-CLI-18):
// the step's name, how long it took, and whether it failed. The report
// Collector implements it. A nil recorder is allowed everywhere it is
// accepted, and means steps still run but are not counted.
type StepRecorder interface {
	RecordStep(name string, d time.Duration, failed bool)
}

// CallRecorder receives every call a script makes to a protocol, and every
// call a protocol driver makes in -protocol mode. The report Collector
// implements it. A nil recorder is allowed everywhere it is accepted, and
// means the call still runs but is not counted. err is why the call
// failed, or nil when it worked; the recorder names the reason from it.
type CallRecorder interface {
	RecordCall(protocol, op string, d time.Duration, err error, sent, received int64)
	RecordMeasure(protocol, name string, d time.Duration)
}

// Inputs is what a scenario script can read from outside itself
// (FR-CLI-17): environment variables the run was told to expose, and data
// files that feed rows to virtual users. The cmd layer builds it from the
// run's flags; a nil Inputs means the script can read nothing, so `env` is
// empty and `data` has no files.
type Inputs interface {
	// Env returns the value of an exposed environment variable.
	Env(name string) (string, bool)
	// EnvNames lists the exposed variable names.
	EnvNames() []string
	// Dataset returns the data file registered under name.
	Dataset(name string) (Dataset, bool)
	// DatasetNames lists the registered data file names.
	DatasetNames() []string
}

// Dataset is one data file's rows. Next hands out the rows in order, one
// per call, shared by all virtual users, and starts over after the last.
// Random returns any row. Rows are read-only to the script.
type Dataset interface {
	Next() map[string]any
	Random() map[string]any
	Len() int
}

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
	// ExpectedStatuses are the HTTP statuses that count as a success for
	// this call. Nil means protocol.HTTPStatusOK's rule: 2xx and 3xx
	// pass, 400 or higher fails. Check it with CheckExpectedStatuses.
	// WebSocket ignores it.
	ExpectedStatuses []int
	// Recorder counts a WebSocket connect. Scripts do not set it. HTTP
	// uses the recorder on HTTPClient instead.
	Recorder CallRecorder
}

// CheckExpectedStatuses says what is wrong with a script's list of
// expected HTTP statuses: an empty list (no answer could pass), or a
// number that is not an HTTP status. A nil list is fine; it means the
// default rule. The caller prefixes the option's name in its language.
func CheckExpectedStatuses(statuses []int) error {
	if statuses == nil {
		return nil
	}
	if len(statuses) == 0 {
		return fmt.Errorf("the list is empty, so no answer could pass; leave it out for the default (2xx and 3xx pass)")
	}
	for _, s := range statuses {
		if s < 100 || s > 599 {
			return fmt.Errorf("%d is not an HTTP status (want 100 to 599)", s)
		}
	}
	return nil
}

// statusFailure is the failure of an HTTP answer with status code, or nil
// when the call accepts that status.
func statusFailure(code int, expected []int) error {
	if expected == nil {
		if protocol.HTTPStatusOK(code) {
			return nil
		}
	} else if slices.Contains(expected, code) {
		return nil
	}
	return failure.Status(code)
}

// ScriptHeaders is the header map a scenario sees. Keys keep Go's
// canonical form (Set-Cookie, X-Request-Id). Every Set-Cookie value is
// kept, joined with newlines, because a comma join would split an
// Expires date. Every other header keeps its first value.
func ScriptHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vals := range h {
		if len(vals) == 0 {
			continue
		}
		if http.CanonicalHeaderKey(k) == "Set-Cookie" {
			out[k] = strings.Join(vals, "\n")
			continue
		}
		out[k] = vals[0]
	}
	return out
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
	calls   CallRecorder
}

// SetCallRecorder counts this client's HTTP calls. Nil means not counted.
func (c *HTTPClient) SetCallRecorder(r CallRecorder) {
	if c != nil {
		c.calls = r
	}
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
//
// An answer is returned whatever its status, so the script can read it.
// A status the call does not accept (see Options.ExpectedStatuses) is
// counted as a failed call, but is not an error here.
func (c *HTTPClient) Do(ctx context.Context, method, rawURL string, body []byte, opts Options) (*HTTPResponse, error) {
	if method == "" {
		method = http.MethodGet
	}
	if err := CheckExpectedStatuses(opts.ExpectedStatuses); err != nil {
		return nil, fmt.Errorf("netapi: expected statuses: %w", err)
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

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		c.recordHTTP(ctx, method, time.Since(start), err, int64(len(body)), 0)
		return nil, fmt.Errorf("netapi: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	callErr := err
	if callErr == nil {
		callErr = statusFailure(resp.StatusCode, opts.ExpectedStatuses)
	}
	c.recordHTTP(ctx, method, time.Since(start), callErr, int64(len(body)), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("netapi: reading response body: %w", err)
	}

	return &HTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       data,
	}, nil
}

func (c *HTTPClient) recordHTTP(ctx context.Context, method string, d time.Duration, err error, sent, received int64) {
	if c == nil || c.calls == nil {
		return
	}
	if err != nil && RunEnded(ctx) {
		return
	}
	c.calls.RecordCall("http", httpOp(method), d, err, sent, received)
}

// httpOp keeps the operation set fixed. A verb outside the usual list is
// OTHER, so a script cannot store an arbitrary string as an operation name.
func httpOp(method string) string {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return strings.ToUpper(method)
	default:
		return "OTHER"
	}
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
	conn  *websocket.Conn
	calls CallRecorder
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
	start := time.Now()
	conn, resp, err := dialer.DialContext(ctx, rawURL, header)
	recordWS(opts.Recorder, ctx, "connect", time.Since(start), err, 0, 0)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return nil, fmt.Errorf("netapi: dialing %s: %w", urlerr.Mask(rawURL), urlerr.Inner(err))
	}
	return &WSConn{conn: conn, calls: opts.Recorder}, nil
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
	start := time.Now()
	err := c.conn.WriteMessage(msgType, data)
	recordWS(c.calls, ctx, "send", time.Since(start), err, int64(len(data)), 0)
	if err != nil {
		return fmt.Errorf("netapi: sending websocket message: %w", err)
	}
	return nil
}

func recordWS(rec CallRecorder, ctx context.Context, op string, d time.Duration, err error, sent, received int64) {
	if rec == nil {
		return
	}
	if err != nil && RunEnded(ctx) {
		return
	}
	rec.RecordCall("websocket", op, d, err, sent, received)
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
		return fmt.Errorf("netapi: parsing URL %q: %w", urlerr.Mask(rawURL), urlerr.Inner(err))
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("netapi: URL %q has no host", urlerr.Mask(rawURL))
	}
	return check(host)
}
