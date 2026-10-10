package doctor

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/vegaload/vegaload/internal/safety"
	"github.com/vegaload/vegaload/internal/urlerr"
)

type target struct {
	Scheme string // "" for a bare host:port
	Host   string
	Port   string
}

// scheme describes a URL scheme a -target may use. To teach doctor a new
// protocol, add one line to schemes.
type scheme struct {
	port     string // default port, "" if the URL must give one
	protocol string // the -protocol name that drives this scheme, "" if none
	udp      bool   // true if the target is reached over UDP, which has no connection to test
	http     bool   // true if doctor can probe it with an HTTP request
}

var schemes = map[string]scheme{
	"http":       {port: "80", protocol: "http1", http: true},
	"https":      {port: "443", protocol: "http1", http: true},
	"ws":         {port: "80", protocol: "websocket", http: true},
	"wss":        {port: "443", protocol: "websocket", http: true},
	"grpc":       {protocol: "grpc"},
	"grpcs":      {port: "443", protocol: "grpc"},
	"mqtt":       {port: "1883", protocol: "mqtt"},
	"mqtts":      {port: "8883", protocol: "mqtt"},
	"kafka":      {port: "9092", protocol: "kafka"},
	"kafkas":     {port: "9093", protocol: "kafka"},
	"postgres":   {port: "5432", protocol: "postgres"},
	"postgresql": {port: "5432", protocol: "postgres"},
	"mysql":      {port: "3306", protocol: "mysql"},
	"mariadb":    {port: "3306", protocol: "mysql"},
	"redis":      {port: "6379", protocol: "redis"},
	"rediss":     {port: "6379", protocol: "redis"},
	"amqp":       {port: "5672", protocol: "rabbitmq"},
	"amqps":      {port: "5671", protocol: "rabbitmq"},
	"ftp":        {port: "21", protocol: "ftp"},
	"ftps":       {port: "990", protocol: "ftp"},
	"tcp":        {protocol: "tcp"},
	"udp":        {protocol: "udp", udp: true},
}

// parseTarget accepts the same forms as `vegaload run -target`: a full
// URL, or a bare host:port.
func parseTarget(raw string) (target, error) {
	if !strings.Contains(raw, "://") {
		h, p, err := net.SplitHostPort(raw)
		if err != nil || h == "" || p == "" {
			return target{}, fmt.Errorf("%q is neither a URL (https://host:port/path) nor host:port", raw)
		}
		return target{Host: h, Port: p}, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return target{}, fmt.Errorf("parsing %q: %w", urlerr.Mask(raw), urlerr.Inner(err))
	}
	if u.Hostname() == "" {
		return target{}, fmt.Errorf("%q has no host", urlerr.Mask(raw))
	}
	t := target{Scheme: strings.ToLower(u.Scheme), Host: u.Hostname(), Port: u.Port()}
	if t.Port == "" {
		t.Port = schemes[t.Scheme].port
	}
	if t.Port == "" {
		return target{}, fmt.Errorf("%q has no port and %q has no default", raw, t.Scheme)
	}
	return t, nil
}

func (t target) protocol() string {
	return schemes[t.Scheme].protocol
}

func targetChecks(env Env, opt Options) []Check {
	if opt.Target == "" {
		return []Check{{ID: "target.none", Category: "target", Name: "Target", Run: func(context.Context) Result {
			return result(Skip, "no target given. Pass -target <url or host:port> to check one.")
		}}}
	}
	raw := opt.Target
	tgt, perr := parseTarget(raw)
	skipIfBad := func(run func(ctx context.Context) Result) func(ctx context.Context) Result {
		return func(ctx context.Context) Result {
			if perr != nil {
				return result(Skip, "target could not be parsed")
			}
			return run(ctx)
		}
	}
	addr := net.JoinHostPort(tgt.Host, tgt.Port)

	return []Check{
		{ID: "target.parse", Category: "target", Name: "Target address", Run: func(context.Context) Result {
			if perr != nil {
				r := result(Fail, perr.Error())
				r.Fix = "Use a URL such as https://localhost:8080/path, or host:port."
				return r
			}
			msg := fmt.Sprintf("%s (host %s, port %s", raw, tgt.Host, tgt.Port)
			if p := tgt.protocol(); p != "" {
				msg += ", protocol " + p
			}
			return result(Pass, msg+")")
		}},
		{ID: "target.allowlist", Category: "target", Name: "Allowlist and caps", Run: skipIfBad(func(context.Context) Result {
			caps := "hard caps: " + safety.FormatLimits(safety.DefaultLimits)
			if safety.IsAllowed(tgt.Host, opt.AllowTargets) {
				r := result(Pass, tgt.Host+" can be tested without confirmation")
				r.Detail = []string{caps}
				return r
			}
			r := result(Warn, tgt.Host+" is not localhost or on the allowlist, so a run will ask for confirmation")
			r.Detail = []string{caps}
			r.Fix = "Pass `-allow-target " + tgt.Host + "` to `vegaload run`, or `-yes` for a one-off run. Only test systems you own or have permission to test."
			return r
		})},
		{ID: "target.dns", Category: "target", Name: "Name lookup", Run: skipIfBad(func(ctx context.Context) Result {
			if net.ParseIP(tgt.Host) != nil {
				return result(Pass, tgt.Host+" is an IP address, no lookup needed")
			}
			addrs, err := env.LookupHost(ctx, tgt.Host)
			if err != nil {
				r := result(Fail, "cannot resolve "+tgt.Host+": "+err.Error())
				r.Fix = "Check the spelling, your network, and any VPN or proxy."
				return r
			}
			return result(Pass, fmt.Sprintf("%s resolves to %s", tgt.Host, strings.Join(addrs, ", ")))
		})},
		{ID: "target.connect", Category: "target", Name: "TCP connection", Run: skipIfBad(func(ctx context.Context) Result {
			if schemes[tgt.Scheme].udp {
				return result(Skip, "UDP has no connection to test. Doctor can check the name lookup only.")
			}
			conn, err := env.DialContext(ctx, "tcp", addr)
			if err != nil {
				r := result(Fail, "cannot connect to "+addr+": "+err.Error())
				r.Fix = "Is the app running and listening on that port? For a remote host, check the network, VPN, and firewall."
				return r
			}
			_ = conn.Close()
			return result(Pass, "connected to "+addr)
		})},
		{ID: "target.probe", Category: "target", Name: "Protocol probe", Run: skipIfBad(func(ctx context.Context) Result {
			return probeTarget(ctx, env, tgt, raw)
		})},
	}
}

func probeTarget(ctx context.Context, env Env, t target, raw string) Result {
	if !schemes[t.Scheme].http {
		return result(Skip, "TCP connect is the only check for this kind of target. Only HTTP and WebSocket targets are probed further.")
	}
	u, _ := url.Parse(raw)
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return result(Fail, err.Error())
	}
	req.Header.Set("User-Agent", "vegaload-doctor/"+env.Version)
	resp, err := env.HTTPClient.Do(req)
	if err != nil {
		r := result(Fail, "no HTTP response from "+u.String()+": "+err.Error())
		var unknown x509.UnknownAuthorityError
		var hostErr x509.HostnameError
		if errors.As(err, &unknown) || errors.As(err, &hostErr) {
			r.Fix = "The TLS certificate is not trusted. Trust it, or pass `-insecure` to `vegaload run` for a test environment."
		} else {
			r.Fix = "Check that the server speaks HTTP on this port."
		}
		return r
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 500 {
		r := result(Warn, fmt.Sprintf("%s answered HTTP %d", u.String(), resp.StatusCode))
		r.Fix = "The server is up but reports an error. A load test would mostly measure that error."
		return r
	}
	return result(Pass, fmt.Sprintf("%s answered HTTP %d", u.String(), resp.StatusCode))
}

// smokeCheck runs a one-user, one-second test through `vegaload run`.
// It is off unless -smoke is passed, because it generates real traffic.
func smokeCheck(env Env, opt Options) Check {
	return Check{ID: "runtime.smoke", Category: "runtime", Name: "Smoke run", Run: func(ctx context.Context) Result {
		if opt.Target == "" {
			return result(Skip, "-smoke needs -target")
		}
		t, err := parseTarget(opt.Target)
		if err != nil {
			return result(Skip, "target could not be parsed")
		}
		if t.protocol() != "http1" {
			return result(Skip, "the smoke run supports http:// and https:// targets. Use a scenario file for other protocols.")
		}
		if !safety.IsAllowed(t.Host, opt.AllowTargets) {
			return result(Skip, t.Host+" is not localhost or allowlisted. Add -allow-target "+t.Host+" to run a smoke test against it.")
		}
		args := []string{"run", "-target", opt.Target, "-protocol", "http1", "-vus", "1", "-duration", "1s",
			"-output", "json", "-no-report", "-no-open", "-trigger", "doctor"}
		for _, a := range opt.AllowTargets {
			args = append(args, "-allow-target", a)
		}
		out, err := env.RunCmd(ctx, env.Exe, args...)
		if err != nil {
			r := result(Fail, "the smoke run failed: "+err.Error())
			if out != "" {
				r.Detail = []string{lastLine(out)}
			}
			r.Fix = "Run `vegaload " + strings.Join(args[:6], " ") + " ...` yourself to see the full error."
			return r
		}
		r := result(Pass, "1 user for 1 second against "+opt.Target+" completed")
		r.Detail = []string{"recorded in the audit log with trigger \"doctor\""}
		return r
	}}
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}
