// Package protocol defines the shared interface every wire protocol
// driver (HTTP/1.1, HTTP/2, gRPC, WebSocket, TCP, UDP) implements, plus the request
// and result types they exchange with the engine.
//
// This package, and the driver packages beneath it, are the only places
// in VegaLoad that know what HTTP, gRPC, or a WebSocket handshake actually
// look like on the wire. internal/engine calls a driver's Do method
// through this interface and otherwise knows nothing protocol-specific —
// see AGENTS.md's module boundaries.
package protocol

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Target describes what a driver connects to and how, in terms generic
// enough to cover every wire protocol VegaLoad supports. Each
// driver interprets the fields relevant to it and ignores the rest — for
// example Method is meaningless to the WebSocket and gRPC drivers.
type Target struct {
	// URL is the target address: an http(s):// URL for the HTTP
	// drivers, a ws(s):// URL for WebSocket, or a host:port for gRPC.
	URL string
	// Method is the HTTP verb (GET, POST, ...) for the HTTP drivers,
	// defaulting to GET if empty; for the gRPC driver it is instead the
	// full RPC method, e.g. "/package.Service/Method", which has no
	// default and must be set. Ignored by WebSocket.
	Method string
	// Headers are sent as request headers (HTTP) or connection
	// metadata (gRPC), depending on the driver.
	Headers map[string]string
	// Body is the request payload, if any.
	Body []byte
	// InsecureSkipVerify disables TLS certificate verification for
	// drivers that speak TLS. It exists for testing against targets
	// with self-signed or internal-CA certificates; it is false (verify
	// normally) by default and a scenario author has to opt in.
	InsecureSkipVerify bool
	// Options carries settings that only one driver understands, as
	// text key=value pairs, for example a TCP driver's "read" size or a
	// message queue driver's "topic". They come from repeated -opt
	// flags (or the MCP "options" argument). A driver reads the keys it
	// knows through the Option helpers below. Every driver must list the
	// keys it knows, and RejectUnknownOptions turns any other key into an
	// error, so a typo is never silently ignored. It is nil when none
	// were given.
	Options map[string]string
	// MaxConns is the most calls the run can have in flight at once
	// through one driver: its peak VU count. A driver shared by every VU
	// that keeps a pool of connections sizes the pool from it, so a busy
	// run reuses its connections instead of opening one per call. Zero
	// means unknown, and the driver keeps its own default.
	MaxConns int
	// NoConnectionReuse makes every call open a new connection and close
	// it afterwards (-no-connection-reuse), for a test that wants to
	// measure connection setup on purpose.
	NoConnectionReuse bool
}

// Option returns the value of an option key, or def if it was not set.
func (t Target) Option(key, def string) string {
	if v, ok := t.Options[key]; ok {
		return v
	}
	return def
}

// OptionInt returns an option as an integer, or def if it was not set.
// A value that is not a whole number is an error, so a typo is reported
// instead of silently using the default.
func (t Target) OptionInt(key string, def int) (int, error) {
	v, ok := t.Options[key]
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("option %s=%q: want a whole number", key, v)
	}
	return n, nil
}

// OptionDuration returns an option as a duration such as "250ms" or "2s",
// or def if it was not set.
func (t Target) OptionDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := t.Options[key]
	if !ok {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("option %s=%q: want a duration such as 500ms or 2s", key, v)
	}
	return d, nil
}

// Result is what a driver reports after one Do call — enough detail for
// internal/report (Phase 1) to build percentiles and pass/fail counts
// without needing to know which protocol produced it.
type Result struct {
	// Success is whether this iteration counts as a pass. A driver
	// decides this itself: the HTTP drivers treat 2xx/3xx as success
	// unless configured otherwise.
	Success bool
	// StatusCode is protocol-specific (HTTP status, gRPC status code,
	// ...). Zero if not applicable.
	StatusCode int
	// BytesSent and BytesReceived are best-effort byte counts for the
	// iteration. Zero if a driver doesn't track them.
	BytesSent     int64
	BytesReceived int64
	// Err describes why Success is false. It is informational only —
	// unlike Do's returned error, a non-nil Err does not stop the run.
	Err error
	// Refused is true when the call was stopped before any bytes moved.
	// That is an option or safety error, not a latency.
	Refused bool
	// TTFB is the time to the first byte of the answer (see FirstByte). Zero
	// if the driver does not measure it or no answer came.
	TTFB time.Duration
}

// HTTPStatusOK is the rule every HTTP call follows unless the call names
// the statuses it expects: a 2xx or 3xx answer is a success, and 400 or
// higher is a failure. The HTTP drivers and a scenario's http global both
// use it, so the two modes judge the same answer the same way.
func HTTPStatusOK(code int) bool {
	return code >= 200 && code < 400
}

// Protocol is implemented by each wire protocol driver. The engine package
// calls Do once per iteration and Close once the driver is no longer
// needed; it holds no protocol-specific knowledge of its own.
type Protocol interface {
	// Name identifies the driver, e.g. "http1", "grpc".
	Name() string

	// Do performs one iteration against the configured target. Return
	// a non-nil error only for a failure that should stop the whole
	// run, such as a misconfigured target — a failed individual
	// request is a Result with Success: false and Err set, not a
	// returned error.
	Do(ctx context.Context) (Result, error)

	// Close releases any resources the driver holds (connections,
	// clients). Called once, after every VU using this driver instance
	// has stopped.
	Close() error
}

// RejectUnknownOptions returns an error if the target has an option key
// that is not in allowed. A driver that takes no options passes none, and
// any option is then refused. The error lists the keys that are accepted.
func (t Target) RejectUnknownOptions(allowed ...string) error {
	ok := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		ok[a] = true
	}
	var unknown []string
	for k := range t.Options {
		if !ok[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	if len(allowed) == 0 {
		return fmt.Errorf("unknown option %s (this protocol takes no options)", strings.Join(unknown, ", "))
	}
	known := append([]string(nil), allowed...)
	sort.Strings(known)
	return fmt.Errorf("unknown option %s (this protocol accepts: %s)",
		strings.Join(unknown, ", "), strings.Join(known, ", "))
}

// OptionBool returns an option as a boolean ("true" or "false"), or def
// if it was not set.
func (t Target) OptionBool(key string, def bool) (bool, error) {
	v, ok := t.Options[key]
	if !ok {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("option %s=%q: want true or false", key, v)
	}
	return b, nil
}
