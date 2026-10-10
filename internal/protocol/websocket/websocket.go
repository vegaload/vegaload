// Package websocket implements a protocol.Protocol driver for WebSocket.
//
// Unlike the HTTP and gRPC drivers, a WebSocket connection is not request
// paced — once open, either side can send at any time, independent of
// the other. Phase 0 keeps this driver to the simplest useful shape: each
// call to Do opens its own connection, optionally sends one message, and
// (if it sent one) waits for exactly one reply before closing — a
// handshake-and-echo probe, not a persistent streaming session. A
// scenario that needs a long-lived connection shared across many
// messages is out of scope here; see AGENTS.md's Phase 0 non-goals.
package websocket

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"

	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/urlerr"
)

// Driver is a WebSocket protocol.Protocol.
type Driver struct {
	target protocol.Target
	dialer *websocket.Dialer
}

// New returns a ready-to-use Driver for target. target.URL must be a
// ws:// or wss:// URL. timeout bounds the handshake and, if a message is
// sent, the wait for its reply.
//
// New returns an error only for a configuration problem — an unparseable
// URL or a scheme other than ws/wss — never for the target being
// unreachable, which Do reports per call instead.
func New(target protocol.Target, timeout time.Duration) (*Driver, error) {
	u, err := url.Parse(target.URL)
	if err != nil {
		return nil, fmt.Errorf("websocket: parsing target URL: %w", urlerr.Inner(err))
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("websocket: unsupported scheme %q, want ws:// or wss://", u.Scheme)
	}

	return &Driver{
		target: target,
		dialer: &websocket.Dialer{
			HandshakeTimeout: timeout,
			TLSClientConfig:  &tls.Config{InsecureSkipVerify: target.InsecureSkipVerify}, //nolint:gosec // opt-in, see protocol.Target
		},
	}, nil
}

// Name implements protocol.Protocol.
func (d *Driver) Name() string { return "websocket" }

// Do implements protocol.Protocol.
func (d *Driver) Do(ctx context.Context) (protocol.Result, error) {
	header := make(http.Header, len(d.target.Headers))
	for k, v := range d.target.Headers {
		header.Set(k, v)
	}

	conn, resp, err := d.dialer.DialContext(ctx, d.target.URL, header)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		// A failed handshake (refused connection, non-101 response,
		// ...) is a property of the target for this iteration, not a
		// reason to stop the whole run.
		return protocol.Result{Success: false, Err: err}, nil
	}
	defer conn.Close()

	if len(d.target.Body) == 0 {
		// No payload configured: treat Do as a bare connect-and-close
		// probe, useful for testing whether the handshake itself
		// succeeds under load.
		return protocol.Result{Success: true}, nil
	}

	if err := conn.WriteMessage(websocket.TextMessage, d.target.Body); err != nil {
		return protocol.Result{Success: false, Err: err}, nil
	}
	sent := int64(len(d.target.Body))

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(dl)
	}
	_, reply, err := conn.ReadMessage()
	if err != nil {
		return protocol.Result{Success: false, BytesSent: sent, Err: err}, nil
	}

	// Best-effort graceful close; a failure here doesn't change the
	// result of an iteration that already got its reply.
	_ = conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))

	return protocol.Result{
		Success:       true,
		BytesSent:     sent,
		BytesReceived: int64(len(reply)),
	}, nil
}

// Close implements protocol.Protocol. Do opens and closes its own
// connection every call, so Driver itself holds nothing to release.
func (d *Driver) Close() error { return nil }
