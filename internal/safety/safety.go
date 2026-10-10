// Package safety implements FR-CLI-06: a target allowlist plus hard caps
// on RPS, duration, and concurrency, enforced by default on every run.
//
// It knows nothing about the engine, protocols, or scripting — it only
// answers two questions a caller (cmd/vegaload's run command today, the
// MCP run_test tool tomorrow) must act on: is this target one the run is
// allowed to hit without extra confirmation, and does this run's shape
// exceed the hard caps.
package safety

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vegaload/vegaload/internal/urlerr"
)

// Limits are the hard caps enforced on every run, regardless of target.
// They exist to stop a typo or a runaway agent-generated scenario from
// becoming a much larger run than anyone intended. The caller may raise
// them explicitly (see cmd/vegaload's -cap-* flags); they are never
// silently skipped.
type Limits struct {
	MaxVUs      int
	MaxDuration time.Duration
	MaxRate     float64
}

// DefaultLimits is deliberately generous for local development while
// still catching an obvious mistake (an extra zero on -vus, a -duration
// of "30h" instead of "30s").
var DefaultLimits = Limits{
	MaxVUs:      10000,
	MaxDuration: 24 * time.Hour,
	MaxRate:     100000,
}

// CheckLimits returns an error describing the first cap a run's shape
// exceeds, or nil if it stays within limits. rate may be 0 for executors
// that don't use a rate (fixed-vus, ramp, step).
func CheckLimits(vus int, dur time.Duration, rate float64, limits Limits) error {
	if limits.MaxVUs > 0 && vus > limits.MaxVUs {
		return fmt.Errorf("vus %d exceeds the cap of %d (raise it with -cap-vus if this is intentional)", vus, limits.MaxVUs)
	}
	if limits.MaxDuration > 0 && dur > limits.MaxDuration {
		return fmt.Errorf("duration %s exceeds the cap of %s (raise it with -cap-duration if this is intentional)", dur, limits.MaxDuration)
	}
	if limits.MaxRate > 0 && rate > limits.MaxRate {
		return fmt.Errorf("rate %g exceeds the cap of %g (raise it with -cap-rate if this is intentional)", rate, limits.MaxRate)
	}
	return nil
}

// localHostnames are treated as local without needing an explicit
// -allow-target entry.
var localHostnames = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"::1":       true,
	"0.0.0.0":   true,
}

// TargetHost extracts the bare host (no port) a run's target string
// resolves to. raw may be a full URL (http://host:8080/path,
// grpcs://host:443) or a bare host:port (the form gRPC and WebSocket
// targets sometimes use). An empty raw (a scripted run with no
// protocol-direct target) returns "" and no error — callers should treat
// that as "nothing to check".
func TargetHost(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}

	candidate := raw
	if !strings.Contains(raw, "://") {
		// Bare host:port, or ws:// got abbreviated by a caller — give
		// url.Parse a scheme so it reliably splits host from port.
		candidate = "scheme://" + raw
	}

	u, err := url.Parse(candidate)
	if err != nil {
		return "", fmt.Errorf("safety: parsing target %q: %w", urlerr.Mask(raw), urlerr.Inner(err))
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("safety: target %q has no host", urlerr.Mask(raw))
	}
	return host, nil
}

// IsAllowed reports whether host may be hit without extra confirmation:
// a local hostname/loopback/link-local address, or a case-insensitive
// match against extra (the caller's -allow-target list).
func IsAllowed(host string, extra []string) bool {
	if host == "" {
		return true // nothing to check (scripted run, no protocol-direct target)
	}
	if localHostnames[strings.ToLower(host)] {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return true
	}
	for _, a := range extra {
		if strings.EqualFold(a, host) {
			return true
		}
		// Allow an -allow-target entry that includes a port
		// (api.example.com:8080) to match the bare host too.
		if h, _, err := net.SplitHostPort(a); err == nil && strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// ParseExtraPort is a small helper so cmd/vegaload can accept
// -allow-target entries with or without a port and still compare
// sensibly; kept here so the matching rule lives in one place.
func ParseExtraPort(hostport string) (host string, port string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return h, p
	}
	return hostport, ""
}

// FormatLimits renders limits for a confirmation prompt or error message.
func FormatLimits(l Limits) string {
	return fmt.Sprintf("vus<=%d duration<=%s rate<=%s", l.MaxVUs, l.MaxDuration, strconv.FormatFloat(l.MaxRate, 'g', -1, 64))
}
