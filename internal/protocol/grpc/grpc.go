// Package grpc implements a protocol.Protocol driver for gRPC.
//
// VegaLoad has no compiled proto stubs for whatever service a scenario
// targets — asking an author to generate and vendor them just to load
// test a service would fight the "every test is a file" principle in
// AGENTS.md. Instead this driver sends target.Body as an already-encoded
// message verbatim and hands back the response the same way, using a
// pass-through codec instead of protobuf's. The scripting layer (built
// later in Phase 0) is what will give a scenario author a convenient way
// to produce those bytes; this driver only needs to move them.
package grpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/vegaload/vegaload/internal/protocol"
)

// rawCodec marshals and unmarshals gRPC messages as plain []byte, with no
// protobuf encoding step — what the driver sends is exactly the bytes in
// protocol.Target.Body, and what it gets back is exactly the bytes the
// server sent.
//
// The codec is applied with grpc.ForceCodec so VegaLoad can pass raw
// bytes without compiling proto stubs. CallContentSubtype("proto") is
// paired with it so the wire Content-Type stays application/grpc+proto
// (what stock servers expect). Using a custom subtype like vegaload-raw
// would make grpc-go warn today and reject the call in a future release.
type rawCodec struct{}

// Name is required by encoding.Codec. ForceCodec would advertise it as
// the content-subtype unless CallContentSubtype overrides it; we always
// override to "proto", so this value never appears on the wire.
func (rawCodec) Name() string { return "vegaload-raw" }

func (rawCodec) Marshal(v any) ([]byte, error) {
	b, ok := v.([]byte)
	if !ok {
		return nil, fmt.Errorf("grpc: rawCodec can only marshal []byte, got %T", v)
	}
	return b, nil
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	p, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("grpc: rawCodec can only unmarshal into *[]byte, got %T", v)
	}
	*p = append([]byte(nil), data...)
	return nil
}

// Driver is a gRPC protocol.Protocol. It dials once in New and reuses the
// same *grpc.ClientConn (and the HTTP/2 connection underneath it) across
// every call to Do.
type Driver struct {
	target  protocol.Target
	conn    *grpc.ClientConn
	timeout time.Duration
}

// New returns a ready-to-use Driver for target. target.URL is a bare
// "host:port", or "grpc://host:port" for the same thing written
// explicitly, or "grpcs://host:port" to dial over TLS. target.Method must
// be set to the full RPC method (e.g. "/package.Service/Method") — unlike
// the HTTP drivers there is no sensible default. timeout bounds each
// individual call.
//
// New returns an error for anything that is a configuration problem
// (a missing Method, an unparseable target, an unsupported scheme) —
// never for the target being unreachable, which Do reports per call
// instead, since dialing is lazy.
func New(target protocol.Target, timeout time.Duration) (*Driver, error) {
	if target.Method == "" {
		return nil, fmt.Errorf("grpc: target.Method must be set to the full RPC method, e.g. /package.Service/Method")
	}

	addr, creds, err := parseAddr(target.URL, target.InsecureSkipVerify)
	if err != nil {
		return nil, fmt.Errorf("grpc: %w", err)
	}

	// grpc.NewClient does not dial immediately — the first call to Do
	// is what actually connects, and a target that's unreachable at
	// that point surfaces as a failed Result, not an error here.
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(
			grpc.ForceCodec(rawCodec{}),
			grpc.CallContentSubtype("proto"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc: %w", err)
	}

	return &Driver{target: target, conn: conn, timeout: timeout}, nil
}

// parseAddr interprets target.URL as described on New, returning the bare
// address to dial and the transport credentials to use.
func parseAddr(target string, insecureSkipVerify bool) (addr string, creds credentials.TransportCredentials, err error) {
	if !strings.Contains(target, "://") {
		return target, insecure.NewCredentials(), nil
	}

	u, err := url.Parse(target)
	if err != nil {
		return "", nil, fmt.Errorf("parsing target: %w", err)
	}

	switch u.Scheme {
	case "grpc":
		return u.Host, insecure.NewCredentials(), nil
	case "grpcs":
		return u.Host, credentials.NewTLS(&tls.Config{InsecureSkipVerify: insecureSkipVerify}), nil //nolint:gosec // opt-in, see protocol.Target
	default:
		return "", nil, fmt.Errorf("unsupported scheme %q, want grpc:// or grpcs:// (or a bare host:port)", u.Scheme)
	}
}

// Name implements protocol.Protocol.
func (d *Driver) Name() string { return "grpc" }

// Do implements protocol.Protocol.
func (d *Driver) Do(ctx context.Context) (protocol.Result, error) {
	if d.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.timeout)
		defer cancel()
	}
	if len(d.target.Headers) > 0 {
		ctx = metadata.NewOutgoingContext(ctx, metadata.New(d.target.Headers))
	}

	var resp []byte
	req := d.target.Body
	err := d.conn.Invoke(ctx, d.target.Method, req, &resp)
	if err != nil {
		st, _ := status.FromError(err)
		return protocol.Result{
			Success:    false,
			StatusCode: int(st.Code()),
			BytesSent:  int64(len(req)),
			Err:        err,
		}, nil
	}

	return protocol.Result{
		Success:       true,
		StatusCode:    int(codes.OK),
		BytesSent:     int64(len(req)),
		BytesReceived: int64(len(resp)),
	}, nil
}

// Close implements protocol.Protocol.
func (d *Driver) Close() error {
	return d.conn.Close()
}
