package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/hosts"
	"github.com/vegaload/vegaload/internal/mcp"
	"github.com/vegaload/vegaload/internal/mcpprobe"
)

// testEnv builds an Env over temp directories with fakes for everything
// that touches the network or starts processes.
func testEnv(t *testing.T) Env {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	dir := filepath.Join(root, "proj")
	for _, d := range []string{home, dir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	exe := filepath.Join(root, "bin", "vegaload")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Env{
		// Only directories under the temp root exist, so host detection
		// never sees what is really installed on the machine running the tests.
		DirExists: func(p string) bool {
			fi, err := os.Stat(p)
			return err == nil && fi.IsDir() && strings.HasPrefix(p, root)
		},
		Version: "1.2.3", Exe: exe, OS: "darwin", Arch: "arm64", Home: home, Dir: dir,
		Getenv:   func(string) string { return "" },
		LookPath: func(n string) (string, error) { return "", errors.New("not found") },
		LookupHost: func(ctx context.Context, h string) ([]string, error) {
			return []string{"10.0.0.1"}, nil
		},
		DialContext: (&net.Dialer{Timeout: time.Second}).DialContext,
		HTTPClient:  &http.Client{Timeout: 2 * time.Second},
		RunCmd: func(ctx context.Context, name string, args ...string) (string, error) {
			return "vegaload 1.2.3", nil
		},
		Probe: func(ctx context.Context, c mcpprobe.Command) (mcpprobe.Info, error) {
			return mcpprobe.Info{ServerName: "vegaload", Tools: append([]string(nil), mcp.CoreToolNames...)}, nil
		},
	}
}

// jsonString returns s as a JSON string literal, with the quotes. A Windows
// path has backslashes, so it cannot be pasted into JSON as it is.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func byID(t *testing.T, rep Report, id string) Result {
	t.Helper()
	for _, r := range rep.Results {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no result %q in %v", id, ids(rep))
	return Result{}
}

func ids(rep Report) []string {
	var out []string
	for _, r := range rep.Results {
		out = append(out, r.ID)
	}
	return out
}

func run(t *testing.T, env Env, opt Options) Report {
	t.Helper()
	rep, err := Run(context.Background(), env, opt)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rep
}

func TestCLIOnly_NoHostsIsHealthy(t *testing.T) {
	rep := run(t, testEnv(t), Options{Hosts: []string{"auto"}})
	if r := byID(t, rep, "host.none"); r.Status != Pass {
		t.Errorf("host.none = %+v", r)
	}
	if rep.ExitCode(false) != 0 {
		t.Errorf("a CLI-only machine must exit 0, got summary %+v", rep.Summary)
	}
}

func TestHostNone_ReportsNoHostChecks(t *testing.T) {
	rep := run(t, testEnv(t), Options{Hosts: []string{"none"}})
	for _, r := range rep.Results {
		if r.Category == "host" {
			t.Errorf("-host none produced host result %s", r.ID)
		}
	}
}

func TestAutoDetectedHostWithoutVegaloadIsWarnNotFail(t *testing.T) {
	env := testEnv(t)
	if err := os.MkdirAll(filepath.Join(env.Home, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	rep := run(t, env, Options{Only: []string{"host.cursor"}})
	cfg := byID(t, rep, "host.cursor.config")
	if cfg.Status != Warn {
		t.Fatalf("auto-detected Cursor with no VegaLoad files: config = %+v, want warn", cfg)
	}
	if rep.ExitCode(false) != 0 {
		t.Errorf("CLI-only with Cursor installed must exit 0, summary %+v", rep.Summary)
	}
	if byID(t, run(t, env, Options{Hosts: []string{"cursor"}, Only: []string{"host.cursor.config"}}), "host.cursor.config").Status != Fail {
		t.Error("-host cursor (explicit) should still fail when VegaLoad is not registered")
	}
}

func TestMissingConfigFailsThenFixRegistersAndHandshakePasses(t *testing.T) {
	env := testEnv(t)
	opt := Options{Hosts: []string{"cursor"}, Only: []string{"host"}}

	rep := run(t, env, opt)
	cfg := byID(t, rep, "host.cursor.config")
	if cfg.Status != Fail || !cfg.Fixable {
		t.Fatalf("config check = %+v", cfg)
	}
	if byID(t, rep, "host.cursor.handshake").Status != Skip {
		t.Error("handshake should skip when nothing is registered")
	}
	if rep.ExitCode(false) != 1 {
		t.Error("a failed check must exit 1")
	}

	opt.Fix = true
	rep = run(t, env, opt)
	cfg = byID(t, rep, "host.cursor.config")
	if cfg.Status != Pass || !cfg.Fixed {
		t.Fatalf("after fix: %+v", cfg)
	}
	if h := byID(t, rep, "host.cursor.handshake"); h.Status != Pass {
		t.Errorf("handshake after fix = %+v", h)
	}
	if rules := byID(t, rep, "host.cursor.rules"); rules.Status != Pass || !rules.Fixed {
		t.Errorf("rules after fix = %+v", rules)
	}
	st := hosts.Inspect(hosts.MCPConfig{Path: filepath.Join(env.Dir, ".cursor", "mcp.json")})
	if st.Entry == nil || st.Entry.Command != env.Exe {
		t.Errorf("entry on disk = %+v", st.Entry)
	}
	if rep.ExitCode(false) != 0 {
		t.Errorf("exit after fix = 1, summary %+v", rep.Summary)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	env := testEnv(t)
	rep := run(t, env, Options{Hosts: []string{"cursor"}, Only: []string{"host.cursor"}, Fix: true, DryRun: true})
	if r := byID(t, rep, "host.cursor.config"); r.FixPlan == "" || r.Fixed {
		t.Errorf("dry run result = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(env.Dir, ".cursor")); !os.IsNotExist(err) {
		t.Error("dry run must not create files")
	}
}

func TestStaleCommandIsFixedByRewritingEntry(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Dir, ".mcp.json")
	if err := os.WriteFile(cfg, []byte(`{"mcpServers":{"vegaload":{"command":"/gone/vegaload","args":["mcp","serve"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	opt := Options{Hosts: []string{"claude-code"}, Only: []string{"host.claude-code.command", "host.claude-code.handshake"}}
	rep := run(t, env, opt)
	if r := byID(t, rep, "host.claude-code.command"); r.Status != Fail || !r.Fixable {
		t.Fatalf("command = %+v", r)
	}
	if h := byID(t, rep, "host.claude-code.handshake"); h.Status != Skip {
		t.Errorf("handshake should skip for a missing command, got %+v", h)
	}
	opt.Fix = true
	rep = run(t, env, opt)
	if r := byID(t, rep, "host.claude-code.command"); r.Status != Pass || !r.Fixed {
		t.Errorf("after fix = %+v", r)
	}
}

func TestWrongArgsFail(t *testing.T) {
	env := testEnv(t)
	if err := os.WriteFile(filepath.Join(env.Dir, ".mcp.json"),
		[]byte(`{"mcpServers":{"vegaload":{"command":`+jsonString(env.Exe)+`,"args":["run"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := run(t, env, Options{Hosts: []string{"claude-code"}, Only: []string{"host.claude-code.command"}})
	if r := byID(t, rep, "host.claude-code.command"); r.Status != Fail || !strings.Contains(r.Message, "mcp serve") {
		t.Errorf("command = %+v", r)
	}
}

func TestVersionMismatchWarns(t *testing.T) {
	env := testEnv(t)
	env.RunCmd = func(ctx context.Context, name string, args ...string) (string, error) { return "vegaload 0.9.0", nil }
	if _, err := hosts.MergeConfigFile(filepath.Join(env.Dir, ".mcp.json"), env.Exe, false); err != nil {
		t.Fatal(err)
	}
	rep := run(t, env, Options{Hosts: []string{"claude-code"}, Only: []string{"host.claude-code.command"}})
	if r := byID(t, rep, "host.claude-code.command"); r.Status != Warn || !strings.Contains(r.Message, "0.9.0") {
		t.Errorf("command = %+v", r)
	}
	if rep.ExitCode(false) != 0 || rep.ExitCode(true) != 1 {
		t.Errorf("warn should exit 0, or 1 under strict")
	}
}

func TestHandshakeMissingToolsAndErrors(t *testing.T) {
	env := testEnv(t)
	if _, err := hosts.MergeConfigFile(filepath.Join(env.Dir, ".mcp.json"), env.Exe, false); err != nil {
		t.Fatal(err)
	}
	opt := Options{Hosts: []string{"claude-code"}, Only: []string{"host.claude-code.handshake"}}

	env.Probe = func(ctx context.Context, c mcpprobe.Command) (mcpprobe.Info, error) {
		return mcpprobe.Info{Tools: []string{"run_test"}}, nil
	}
	r := byID(t, run(t, env, opt), "host.claude-code.handshake")
	if r.Status != Fail || !strings.Contains(r.Message, "create_scenario") {
		t.Errorf("missing tools = %+v", r)
	}

	env.Probe = func(ctx context.Context, c mcpprobe.Command) (mcpprobe.Info, error) {
		return mcpprobe.Info{}, errors.New("server crashed")
	}
	r = byID(t, run(t, env, opt), "host.claude-code.handshake")
	if r.Status != Fail || !strings.Contains(r.Message, "server crashed") {
		t.Errorf("probe error = %+v", r)
	}
}

func TestRulesMissingAndStale(t *testing.T) {
	env := testEnv(t)
	opt := Options{Hosts: []string{"cursor"}, Only: []string{"host.cursor.rules"}}
	if r := byID(t, run(t, env, opt), "host.cursor.rules"); r.Status != Warn || !r.Fixable {
		t.Fatalf("missing rules = %+v", r)
	}
	p := filepath.Join(env.Dir, ".cursor", "rules", "vegaload.mdc")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("my own edits"), 0o644); err != nil {
		t.Fatal(err)
	}
	opt.Fix = true
	r := byID(t, run(t, env, opt), "host.cursor.rules")
	if r.Status != Warn || r.Fixable {
		t.Errorf("stale rules must warn and never be auto-overwritten: %+v", r)
	}
	if data, _ := os.ReadFile(p); string(data) != "my own edits" {
		t.Error("--fix overwrote a user-edited rules file")
	}
}

func TestUserScopeFilesOnlyRepairedWhenHostNamed(t *testing.T) {
	env := testEnv(t)
	// Project-scope Cursor config absent and user-scope present but empty of vegaload.
	// Auto-selected host: only the project file may be written.
	if err := os.MkdirAll(filepath.Join(env.Dir, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.Dir, ".cursor", "rules.keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := run(t, env, Options{Hosts: []string{"claude-desktop"}, Only: []string{"host.claude-desktop.config"}, Fix: true})
	// Explicitly named: the user-scope Desktop config may be created.
	if r := byID(t, rep, "host.claude-desktop.config"); r.Status != Pass || !r.Fixed {
		t.Errorf("explicit desktop fix = %+v", r)
	}
	// Claude Code's ~/.claude.json is never rewritten.
	if err := os.WriteFile(filepath.Join(env.Home, ".claude.json"), []byte(`{"projects":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rep = run(t, env, Options{Hosts: []string{"claude-code"}, Only: []string{"host.claude-code.config"}, Fix: true})
	if data, _ := os.ReadFile(filepath.Join(env.Home, ".claude.json")); string(data) != `{"projects":{}}` {
		t.Errorf("~/.claude.json was modified: %s", data)
	}
	if _, err := os.Stat(filepath.Join(env.Dir, ".mcp.json")); err != nil {
		t.Errorf("project .mcp.json should have been created: %v", err)
	}
	_ = rep
}

func TestClaudeCodeUserEntryCountsAsRegistered(t *testing.T) {
	env := testEnv(t)
	body := `{"projects":{` + jsonString(env.Dir) + `:{"mcpServers":{"vegaload":{"command":` + jsonString(env.Exe) + `,"args":["mcp","serve"]}}}}}`
	if err := os.WriteFile(filepath.Join(env.Home, ".claude.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := run(t, env, Options{Hosts: []string{"claude-code"}, Only: []string{"host.claude-code.config"}})
	if r := byID(t, rep, "host.claude-code.config"); r.Status != Pass {
		t.Errorf("config = %+v", r)
	}
}

func TestDesktopUnsupportedOnLinux(t *testing.T) {
	env := testEnv(t)
	env.OS = "linux"
	rep := run(t, env, Options{Hosts: []string{"claude-desktop"}})
	r := byID(t, rep, "host.claude-desktop.detected")
	if r.Status != Skip || !strings.Contains(r.Message, "linux") {
		t.Errorf("desktop on linux = %+v", r)
	}
}

func TestUnknownHostIsAnError(t *testing.T) {
	if _, err := Run(context.Background(), testEnv(t), Options{Hosts: []string{"vim"}}); err == nil {
		t.Fatal("expected an error for an unknown host")
	}
}

func TestCustomMCPConfig(t *testing.T) {
	env := testEnv(t)
	p := filepath.Join(env.Dir, "tool-mcp.json")
	if _, err := hosts.MergeConfigFile(p, env.Exe, false); err != nil {
		t.Fatal(err)
	}
	rep := run(t, env, Options{Hosts: []string{"none"}, MCPConfig: p, Only: []string{"host.custom"}})
	if r := byID(t, rep, "host.custom.handshake"); r.Status != Pass {
		t.Errorf("custom handshake = %+v", r)
	}
}

func TestOnlyAndSkipFilters(t *testing.T) {
	env := testEnv(t)
	rep := run(t, env, Options{Hosts: []string{"none"}, Only: []string{"core"}, Skip: []string{"core.python", "core.dirs"}})
	for _, r := range rep.Results {
		if r.Category != "core" || r.ID == "core.python" || r.ID == "core.dirs" {
			t.Errorf("unexpected result %s", r.ID)
		}
	}
	if len(rep.Results) != 3 {
		t.Errorf("got %v", ids(rep))
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in, scheme, host, port string
		ok                     bool
	}{
		{"http://localhost:8080/x", "http", "localhost", "8080", true},
		{"https://api.example.com", "https", "api.example.com", "443", true},
		{"ws://h/ws", "ws", "h", "80", true},
		{"grpc://h:9090", "grpc", "h", "9090", true},
		{"grpc://h", "", "", "", false},
		{"host:50051", "", "host", "50051", true},
		{"justahost", "", "", "", false},
		{"http://", "", "", "", false},
	}
	for _, c := range cases {
		got, err := parseTarget(c.in)
		if (err == nil) != c.ok {
			t.Errorf("%q: err=%v", c.in, err)
			continue
		}
		if c.ok && (got.Scheme != c.scheme || got.Host != c.host || got.Port != c.port) {
			t.Errorf("%q: got %+v", c.in, got)
		}
	}
}

func TestSchemeTable(t *testing.T) {
	for name, sc := range schemes {
		if sc.http && sc.protocol == "" {
			t.Errorf("scheme %q is probed over HTTP but names no protocol", name)
		}
	}
	for _, in := range []string{"tcp://h", "udp://h"} {
		if _, err := parseTarget(in); err == nil {
			t.Errorf("%s has no default port and should be refused", in)
		}
	}
	if got, err := parseTarget("tcp://h:7"); err != nil || got.protocol() != "tcp" {
		t.Errorf("tcp://h:7 = %+v, %v", got, err)
	}
	if got, err := parseTarget("udp://h:7"); err != nil || got.protocol() != "udp" {
		t.Errorf("udp://h:7 = %+v, %v", got, err)
	}
	for in, want := range map[string]string{"mqtt://h": "1883", "mqtts://h": "8883"} {
		got, err := parseTarget(in)
		if err != nil || got.Port != want || got.protocol() != "mqtt" {
			t.Errorf("%s = %+v, %v; want port %s and protocol mqtt", in, got, err, want)
		}
	}
	for in, want := range map[string]string{"kafka://h": "9092", "kafkas://h": "9093"} {
		got, err := parseTarget(in)
		if err != nil || got.Port != want || got.protocol() != "kafka" {
			t.Errorf("%s = %+v, %v; want port %s and protocol kafka", in, got, err, want)
		}
	}
	for _, in := range []string{"postgres://h", "postgresql://h/db"} {
		got, err := parseTarget(in)
		if err != nil || got.Port != "5432" || got.protocol() != "postgres" {
			t.Errorf("%s = %+v, %v; want port 5432 and protocol postgres", in, got, err)
		}
	}
	for _, in := range []string{"mysql://h", "mariadb://h/db"} {
		got, err := parseTarget(in)
		if err != nil || got.Port != "3306" || got.protocol() != "mysql" {
			t.Errorf("%s = %+v, %v; want port 3306 and protocol mysql", in, got, err)
		}
	}
	for _, in := range []string{"redis://h", "rediss://h/2"} {
		got, err := parseTarget(in)
		if err != nil || got.Port != "6379" || got.protocol() != "redis" {
			t.Errorf("%s = %+v, %v; want port 6379 and protocol redis", in, got, err)
		}
	}
	for _, tc := range []struct{ in, port string }{{"amqp://h", "5672"}, {"amqps://h/orders", "5671"}} {
		got, err := parseTarget(tc.in)
		if err != nil || got.Port != tc.port || got.protocol() != "rabbitmq" {
			t.Errorf("%s = %+v, %v; want port %s and protocol rabbitmq", tc.in, got, err, tc.port)
		}
	}
	for _, tc := range []struct{ in, port string }{{"ftp://h", "21"}, {"ftps://h/files", "990"}} {
		got, err := parseTarget(tc.in)
		if err != nil || got.Port != tc.port || got.protocol() != "ftp" {
			t.Errorf("%s = %+v, %v; want port %s and protocol ftp", tc.in, got, err, tc.port)
		}
	}
	got, err := parseTarget("grpcs://h")
	if err != nil || got.Port != "443" || got.protocol() != "grpc" {
		t.Errorf("grpcs://h = %+v, %v", got, err)
	}
}

// A UDP target has no connection to open, so the connect check is
// skipped, not failed.
func TestTargetConnectSkippedForUDP(t *testing.T) {
	rep := run(t, testEnv(t), Options{Hosts: []string{"none"}, Only: []string{"target.connect"}, Target: "udp://127.0.0.1:9"})
	if r := byID(t, rep, "target.connect"); r.Status != Skip {
		t.Errorf("target.connect for UDP = %+v, want Skip", r)
	}
}

func TestTargetChecksAgainstLiveServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	rep := run(t, testEnv(t), Options{Hosts: []string{"none"}, Only: []string{"target"}, Target: srv.URL})
	for _, id := range []string{"target.parse", "target.allowlist", "target.dns", "target.connect", "target.probe"} {
		if r := byID(t, rep, id); r.Status != Pass {
			t.Errorf("%s = %+v", id, r)
		}
	}
}

func TestTargetDownAndServerError(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	_ = l.Close()
	rep := run(t, testEnv(t), Options{Hosts: []string{"none"}, Only: []string{"target"}, Target: "http://" + addr})
	if r := byID(t, rep, "target.connect"); r.Status != Fail {
		t.Errorf("connect to closed port = %+v", r)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	rep = run(t, testEnv(t), Options{Hosts: []string{"none"}, Only: []string{"target.probe"}, Target: srv.URL})
	if r := byID(t, rep, "target.probe"); r.Status != Warn {
		t.Errorf("503 probe = %+v", r)
	}
}

func TestTargetAllowlist(t *testing.T) {
	env := testEnv(t)
	env.LookupHost = func(ctx context.Context, h string) ([]string, error) { return nil, errors.New("no such host") }
	opt := Options{Hosts: []string{"none"}, Only: []string{"target.allowlist", "target.dns"}, Target: "https://api.example.com/v1"}
	rep := run(t, env, opt)
	if r := byID(t, rep, "target.allowlist"); r.Status != Warn {
		t.Errorf("remote host should warn: %+v", r)
	}
	if r := byID(t, rep, "target.dns"); r.Status != Fail {
		t.Errorf("dns failure = %+v", r)
	}
	opt.AllowTargets = []string{"api.example.com"}
	if r := byID(t, run(t, env, opt), "target.allowlist"); r.Status != Pass {
		t.Errorf("allowlisted host should pass: %+v", r)
	}
}

func TestSmokeIsOffByDefaultAndGatedByAllowlist(t *testing.T) {
	env := testEnv(t)
	called := false
	env.RunCmd = func(ctx context.Context, name string, args ...string) (string, error) {
		called = true
		return "", nil
	}
	rep := run(t, env, Options{Hosts: []string{"none"}, Target: "http://127.0.0.1:1"})
	for _, id := range ids(rep) {
		if id == "runtime.smoke" {
			t.Fatal("smoke ran without -smoke")
		}
	}
	rep = run(t, env, Options{Hosts: []string{"none"}, Only: []string{"runtime"}, Target: "https://api.example.com", Smoke: true})
	if r := byID(t, rep, "runtime.smoke"); r.Status != Skip || called {
		t.Errorf("smoke against a non-allowlisted host must skip without running: %+v called=%v", r, called)
	}
	rep = run(t, env, Options{Hosts: []string{"none"}, Only: []string{"runtime"}, Target: "http://127.0.0.1:9", Smoke: true})
	if r := byID(t, rep, "runtime.smoke"); r.Status != Pass || !called {
		t.Errorf("smoke against localhost = %+v called=%v", r, called)
	}
}

type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("unexpected HTTP request to %s", r.URL)
	return nil, errors.New("blocked")
}

func TestHarnessPlaceholderMakesNoCalls(t *testing.T) {
	env := testEnv(t)
	env.HTTPClient = &http.Client{Transport: failTransport{t}}
	rep := run(t, env, Options{Hosts: []string{"none"}, Only: []string{"harness"}})
	if r := byID(t, rep, "harness.off"); r.Status != Skip {
		t.Errorf("harness.off = %+v", r)
	}
	rep = run(t, env, Options{Hosts: []string{"none"}, Only: []string{"harness"}, Harness: true})
	if r := byID(t, rep, "harness.placeholder"); r.Status != Skip {
		t.Errorf("harness.placeholder = %+v, want skip (no network)", r)
	}
	for _, id := range ids(rep) {
		if id == "harness.credentials" || id == "harness.reachable" {
			t.Errorf("did not expect %s on placeholder harness check", id)
		}
	}
}

func TestOutputsRedactHomeAndKeepContract(t *testing.T) {
	env := testEnv(t)
	rep := run(t, env, Options{Hosts: []string{"none"}, Only: []string{"core.dirs"}})

	var text bytes.Buffer
	WriteText(&text, rep, env.Home, true)
	if strings.Contains(text.String(), env.Home) || !strings.Contains(text.String(), "~"+string(filepath.Separator)+".vegaload") {
		t.Errorf("text output not redacted:\n%s", text.String())
	}

	var js bytes.Buffer
	if err := WriteJSON(&js, rep, env.Home); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"summary"`, `"results"`, `"id": "core.dirs"`, `"status": "pass"`} {
		if !strings.Contains(js.String(), want) {
			t.Errorf("JSON missing %s:\n%s", want, js.String())
		}
	}
	if strings.Contains(js.String(), env.Home) {
		t.Error("JSON output leaks the home directory")
	}

	var jl bytes.Buffer
	if err := WriteJSONL(&jl, rep, env.Home); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(jl.String()), "\n")
	if len(lines) != len(rep.Results)+1 || !strings.Contains(lines[len(lines)-1], `"type":"summary"`) {
		t.Errorf("JSONL = %q", jl.String())
	}
}

func TestPathCheck(t *testing.T) {
	env := testEnv(t)
	rep := run(t, env, Options{Hosts: []string{"none"}, Only: []string{"core.path"}})
	if r := byID(t, rep, "core.path"); r.Status != Warn {
		t.Errorf("not on PATH should warn: %+v", r)
	}
	env.LookPath = func(string) (string, error) { return env.Exe, nil }
	if r := byID(t, run(t, env, Options{Hosts: []string{"none"}, Only: []string{"core.path"}}), "core.path"); r.Status != Pass {
		t.Errorf("on PATH = %+v", r)
	}
	other := filepath.Join(t.TempDir(), "vegaload")
	_ = os.WriteFile(other, []byte("x"), 0o755)
	env.LookPath = func(string) (string, error) { return other, nil }
	if r := byID(t, run(t, env, Options{Hosts: []string{"none"}, Only: []string{"core.path"}}), "core.path"); r.Status != Warn {
		t.Errorf("different binary on PATH should warn: %+v", r)
	}
}

func TestCheckTimeoutIsEnforced(t *testing.T) {
	env := testEnv(t)
	if _, err := hosts.MergeConfigFile(filepath.Join(env.Dir, ".mcp.json"), env.Exe, false); err != nil {
		t.Fatal(err)
	}
	env.Probe = func(ctx context.Context, c mcpprobe.Command) (mcpprobe.Info, error) {
		<-ctx.Done()
		return mcpprobe.Info{}, ctx.Err()
	}
	start := time.Now()
	rep := run(t, env, Options{Hosts: []string{"claude-code"}, Only: []string{"host.claude-code.handshake"}, Timeout: 200 * time.Millisecond})
	if r := byID(t, rep, "host.claude-code.handshake"); r.Status != Fail {
		t.Errorf("hung server should fail: %+v", r)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("the timeout did not cut the check short")
	}
}

func TestIsPathCommand(t *testing.T) {
	for cmd, want := range map[string]bool{
		"vegaload": false, "vegaload.exe": false,
		"/usr/local/bin/vegaload": true, `C:\bin\vegaload.exe`: true, `.\vegaload`: true, "bin/vegaload": true,
	} {
		if got := isPathCommand(cmd); got != want {
			t.Errorf("isPathCommand(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestPythonCheck_WindowsDoesNotTrustAStoreAlias(t *testing.T) {
	env := testEnv(t)
	env.OS = "windows"
	env.LookPath = func(n string) (string, error) {
		if n == "python3" || n == "python" {
			return `C:\bin\` + n + ".exe", nil
		}
		return "", errors.New("not found")
	}
	// python3 is the Store alias: it is on the PATH but does not run.
	env.RunCmd = func(ctx context.Context, name string, args ...string) (string, error) {
		if strings.HasSuffix(name, "python3.exe") {
			return "", errors.New("exit status 9009")
		}
		return "", nil
	}
	rep := run(t, env, Options{Hosts: []string{"none"}, Only: []string{"core.python"}})
	r := byID(t, rep, "core.python")
	if r.Status != Pass || !strings.Contains(r.Message, "python.exe") {
		t.Errorf("want pass with python.exe, got %+v", r)
	}

	// Nothing real is left once python is gone too: skip, not pass.
	env.RunCmd = func(ctx context.Context, name string, args ...string) (string, error) {
		return "", errors.New("does not run")
	}
	rep = run(t, env, Options{Hosts: []string{"none"}, Only: []string{"core.python"}})
	if r := byID(t, rep, "core.python"); r.Status != Skip {
		t.Errorf("a PATH entry that does not run must not pass: %+v", r)
	}
}

func TestParseTarget_ErrorsDoNotRepeatThePassword(t *testing.T) {
	const secret = "hunter2-secret"
	for _, raw := range []string{
		"alice:" + secret + "@host:443",     // not a URL and not host:port
		"foo://alice:" + secret + "@host",   // parses, no port and no default
		"redis://alice:" + secret + "@",     // parses, no host
		"redis://alice:" + secret + "@[::1", // does not parse
		"alice:" + secret + "@host",         // no port at all
	} {
		_, err := parseTarget(raw)
		if err == nil {
			t.Errorf("want an error for the case with %d bytes", len(raw))
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("an error repeats the password for the case with %d bytes", len(raw))
		}
	}
}
