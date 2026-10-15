package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/live"
	"github.com/vegaload/vegaload/internal/stream"
)

// fakeBridge stands in for the live-view service: it answers the health check
// and records what is pushed to a project.
type fakeBridge struct {
	srv *httptest.Server

	mu       sync.Mutex
	pushes   int
	auth     []string
	paths    []string
	queries  []string
	received strings.Builder
}

func newFakeBridge(t *testing.T) *fakeBridge {
	t.Helper()
	b := &fakeBridge{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/health":
			_, _ = w.Write([]byte(`{"status":"ok","projects":true}`))
		case strings.HasPrefix(r.URL.Path, "/v1/projects/") && strings.HasSuffix(r.URL.Path, "/ingest"):
			body, _ := io.ReadAll(r.Body)
			b.mu.Lock()
			b.pushes++
			b.auth = append(b.auth, r.Header.Get("Authorization"))
			b.paths = append(b.paths, r.URL.Path)
			b.queries = append(b.queries, r.URL.RawQuery)
			b.received.Write(body)
			b.mu.Unlock()
			_, _ = w.Write([]byte(`{"accepted":1,"duplicates":0,"ignored":0}`))
		case strings.HasPrefix(r.URL.Path, "/p/"):
			_, _ = w.Write([]byte(`<html>live view</html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *fakeBridge) snapshot() (pushes int, auth, paths, queries []string, body string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pushes, append([]string(nil), b.auth...), append([]string(nil), b.paths...), append([]string(nil), b.queries...), b.received.String()
}

// liveEnv turns the live view on for one test (TestMain turns it off for the
// package), points it at base, isolates its files, and captures the browser.
// Every URL or path the browser is asked to open is appended to the result.
func liveEnv(t *testing.T, base string) (opened *[]string) {
	t.Helper()
	clearStreamEnv(t)
	t.Setenv("VEGALOAD_CONFIG_DIR", t.TempDir())
	t.Setenv("VEGALOAD_LIVE", "")
	t.Setenv("VEGALOAD_OPEN", "")
	t.Setenv("VEGALOAD_LIVE_URL", base)
	t.Setenv("CI", "")
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	var urls []string
	prev := live.Opener
	live.Opener = func(u string) error { urls = append(urls, u); return nil }
	t.Cleanup(func() { live.Opener = prev })
	return &urls
}

type liveRunResult struct {
	streamed
	reportPath string
}

// runLive runs a short test against a quiet server with the HTML report on.
func runLive(t *testing.T, extra ...string) liveRunResult {
	t.Helper()
	srv := quietServer(t)
	dir := t.TempDir()
	res := liveRunResult{reportPath: filepath.Join(dir, "report.html")}
	res.outPath = filepath.Join(dir, "out.json")
	auditPath := filepath.Join(dir, "audit.log")
	args := []string{"-target", srv.URL, "-protocol", "http1", "-vus", "2", "-duration", "300ms",
		"-audit-log", auditPath, "-out", res.outPath, "-report", res.reportPath}
	res.code, res.stdout, res.stderr = captureCmd(t, func() int { return cmdRun(append(args, extra...)) })
	if b, err := os.ReadFile(res.outPath); err == nil {
		res.outRaw = b
	}
	if b, err := os.ReadFile(auditPath); err == nil {
		res.audit = string(b)
	}
	return res
}

func TestRunShowsTheLiveViewByDefault(t *testing.T) {
	bridge := newFakeBridge(t)
	opened := liveEnv(t, bridge.srv.URL)

	r := runLive(t)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	id, err := live.Load()
	if err != nil || id.Created {
		t.Fatalf("the run should have made the identity: %+v %v", id, err)
	}
	page := bridge.srv.URL + "/p/" + id.ProjectID

	// The line a person (or an agent) relays, on stderr.
	if !strings.Contains(r.stderr, "live view: "+page) {
		t.Fatalf("no live view line in stderr:\n%s", r.stderr)
	}
	// The browser opens once, on the project page. The HTML report is written
	// and is not opened as well.
	if len(*opened) != 1 || (*opened)[0] != page {
		t.Fatalf("opened %v, want only %s", *opened, page)
	}
	if _, err := os.Stat(r.reportPath); err != nil {
		t.Fatalf("the HTML report was not written: %v", err)
	}

	// The events went to that project, with its own token and no other.
	pushes, auth, paths, queries, body := bridge.snapshot()
	if pushes == 0 {
		t.Fatal("nothing was pushed")
	}
	for i := range auth {
		if auth[i] != "Bearer "+id.PushToken {
			t.Fatalf("push %d authorised with %q", i, auth[i])
		}
		if paths[i] != "/v1/projects/"+id.ProjectID+"/ingest" || queries[i] != "source=vegaload" {
			t.Fatalf("push %d to %s?%s", i, paths[i], queries[i])
		}
	}
	for _, want := range []string{`"type":"run.started"`, `"type":"run.finished"`, `"schema":"vegaload.io/stream/v1"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("no %s in what was pushed", want)
		}
	}

	// Identity file: mode 0600 on systems that have modes.
	if st, err := os.Stat(id.Path); err != nil || (st.Mode().Perm() != 0o600 && os.PathSeparator == '/') {
		t.Fatalf("identity file %v %v", st, err)
	}

	// The audit log notes the live view by host and nothing else.
	if !strings.Contains(r.audit, `"stream":"live:127.0.0.1:`) {
		t.Fatalf("audit does not name the live view: %s", r.audit)
	}
	// Neither the token nor the project id (the page's secret) is anywhere it
	// should not be: not the report, the audit log, the events (the project id
	// is in the URL they were sent to, not in them), stdout, or any error text.
	rep, _ := os.ReadFile(r.reportPath)
	for name, content := range map[string]string{"-out": string(r.outRaw), "html report": string(rep), "audit": r.audit, "events": body, "stdout": r.stdout} {
		if strings.Contains(content, id.PushToken) {
			t.Fatalf("the push token is in the %s", name)
		}
		if strings.Contains(content, id.ProjectID) {
			t.Fatalf("the project id is in the %s", name)
		}
	}
	if strings.Contains(r.stderr, id.PushToken) {
		t.Fatalf("the push token is on stderr:\n%s", r.stderr)
	}
	if strings.Count(r.stderr, id.ProjectID) != 1 {
		t.Fatalf("the project id should appear once on stderr, in the live view URL:\n%s", r.stderr)
	}
}

// stdout stays what -output json promises, with the live view on: the note and
// the browser tab can never break a parser (the MCP run_test tool is one).
func TestLiveViewOnKeepsStdoutValidJSON(t *testing.T) {
	bridge := newFakeBridge(t)
	liveEnv(t, bridge.srv.URL)

	r := runLive(t, "-output", "json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stderr, "live view: ") {
		t.Fatalf("no live view note:\n%s", r.stderr)
	}
	var v map[string]any
	dec := json.NewDecoder(strings.NewReader(r.stdout))
	if err := dec.Decode(&v); err != nil || v["total"] == nil {
		t.Fatalf("stdout is not the result JSON: %v\n%q", err, r.stdout)
	}
	if dec.More() {
		t.Fatalf("stdout holds more than the JSON: %q", r.stdout)
	}
	if strings.Contains(r.stdout, "live view") {
		t.Fatalf("a live view note on stdout: %q", r.stdout)
	}
}

func TestEveryRunOpensTheBrowserOnTheSameProject(t *testing.T) {
	bridge := newFakeBridge(t)
	opened := liveEnv(t, bridge.srv.URL)
	for i := 0; i < 2; i++ {
		if r := runLive(t); r.code != 0 {
			t.Fatalf("run %d: exit %d", i, r.code)
		}
	}
	if len(*opened) != 2 || (*opened)[0] != (*opened)[1] {
		t.Fatalf("opened %v: want the same page twice", *opened)
	}
}

func TestLiveViewCanBeTurnedOffAndTheReportThenOpensAsBefore(t *testing.T) {
	cases := map[string]struct {
		args []string
		env  map[string]string
	}{
		"flag":               {args: []string{"-no-live"}},
		"env":                {env: map[string]string{"VEGALOAD_LIVE": "off"}},
		"ci":                 {env: map[string]string{"CI": "true"}},
		"stream-file chosen": {args: []string{"-stream-file", "auto"}},
		"stream-push chosen": {}, // filled in below with a receiver
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			bridge := newFakeBridge(t)
			opened := liveEnv(t, bridge.srv.URL)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			args := c.args
			if name == "stream-push chosen" {
				args = []string{"-stream-push", newReceiver(t).srv.URL + "/ingest"}
			}
			wd, _ := os.Getwd()
			t.Cleanup(func() { _ = os.Chdir(wd) })
			_ = os.Chdir(t.TempDir()) // -stream-file auto writes under the cwd

			r := runLive(t, args...)
			if r.code != 0 {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			if pushes, _, _, _, _ := bridge.snapshot(); pushes != 0 {
				t.Fatalf("%d pushes with the live view off", pushes)
			}
			if _, err := os.Stat(filepath.Join(os.Getenv("VEGALOAD_CONFIG_DIR"), "live.json")); err == nil {
				t.Fatal("an identity was made although the live view is off")
			}
			if strings.Contains(r.stderr, "live view:") {
				t.Fatalf("a live view line with the view off:\n%s", r.stderr)
			}
			// Nothing about the report changed: it is opened, as before, and
			// it is the only tab.
			if len(*opened) != 1 || (*opened)[0] != r.reportPath {
				t.Fatalf("opened %v, want only the report %s", *opened, r.reportPath)
			}
		})
	}
}

func TestLiveFlagAddsTheViewToAnotherDestinationAndBeatsCI(t *testing.T) {
	bridge := newFakeBridge(t)
	opened := liveEnv(t, bridge.srv.URL)
	t.Setenv("CI", "true")
	file := filepath.Join(t.TempDir(), "run.ndjson")
	if r := runLive(t, "-live", "-stream-file", file); r.code != 0 {
		t.Fatalf("exit %d", r.code)
	}
	if pushes, _, _, _, _ := bridge.snapshot(); pushes == 0 {
		t.Fatal("-live did not push")
	}
	if _, err := stream.ReadFile(file); err != nil {
		t.Fatalf("the file destination was dropped: %v", err)
	}
	// CI never opens the live page: -live asks for the stream, not a window.
	for _, o := range *opened {
		if strings.Contains(o, "/p/") {
			t.Fatalf("opened the live page in CI: %v", *opened)
		}
	}
	// But VEGALOAD_LIVE=off still wins over -live.
	t.Setenv("VEGALOAD_LIVE", "off")
	before, _, _, _, _ := bridge.snapshot()
	if r := runLive(t, "-live"); r.code != 0 {
		t.Fatal("exit")
	}
	if after, _, _, _, _ := bridge.snapshot(); after != before {
		t.Fatal("-live beat VEGALOAD_LIVE=off")
	}
}

// -no-open now stops every tab: the live page and the HTML report. The run is
// still streamed, and the report is still written.
func TestNoOpenStopsEveryTabButStillStreams(t *testing.T) {
	bridge := newFakeBridge(t)
	opened := liveEnv(t, bridge.srv.URL)
	r := runLive(t, "-no-open")
	if r.code != 0 {
		t.Fatal("exit")
	}
	if pushes, _, _, _, _ := bridge.snapshot(); pushes == 0 {
		t.Fatal("nothing pushed")
	}
	if len(*opened) != 0 {
		t.Fatalf("opened %v", *opened)
	}
	if _, err := os.Stat(r.reportPath); err != nil {
		t.Fatalf("no report: %v", err)
	}
	if !strings.Contains(r.stderr, "live view: ") {
		t.Fatalf("the URL is still printed:\n%s", r.stderr)
	}

	// And with the view off, -no-open keeps meaning what it did.
	*opened = nil
	if r := runLive(t, "-no-open", "-no-live"); r.code != 0 || len(*opened) != 0 {
		t.Fatalf("exit %d, opened %v", r.code, *opened)
	}
}

func TestOpenEnvOffStopsEveryTab(t *testing.T) {
	bridge := newFakeBridge(t)
	opened := liveEnv(t, bridge.srv.URL)
	t.Setenv("VEGALOAD_OPEN", "off")
	if r := runLive(t); r.code != 0 {
		t.Fatal("exit")
	}
	if r := runLive(t, "-no-live"); r.code != 0 {
		t.Fatal("exit")
	}
	if len(*opened) != 0 {
		t.Fatalf("opened %v", *opened)
	}
}

// A run that an agent started never opens the report, with the live view on or off.
func TestAnAgentRunNeverOpensTheReport(t *testing.T) {
	bridge := newFakeBridge(t)
	opened := liveEnv(t, bridge.srv.URL)
	if r := runLive(t, "-trigger", "mcp", "-no-live"); r.code != 0 {
		t.Fatal("exit")
	}
	if len(*opened) != 0 {
		t.Fatalf("opened %v", *opened)
	}
	// With the view on it still opens the live page: that is the point of it.
	if r := runLive(t, "-trigger", "mcp"); r.code != 0 {
		t.Fatal("exit")
	}
	if len(*opened) != 1 || !strings.Contains((*opened)[0], "/p/") {
		t.Fatalf("opened %v", *opened)
	}
}

func TestUnreachableBridgeNeverFailsTheRun(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	url := gone.URL
	gone.Close()
	opened := liveEnv(t, url)

	start := time.Now()
	r := runLive(t)
	if r.code != 0 {
		t.Fatalf("exit %d: a missing bridge must not fail the run", r.code)
	}
	if !strings.Contains(r.stderr, "live view: off") {
		t.Fatalf("no reason on stderr:\n%s", r.stderr)
	}
	// Only the report opens, as it always did.
	if len(*opened) != 1 || (*opened)[0] != r.reportPath {
		t.Fatalf("opened %v", *opened)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("took %s", d)
	}
}

func TestRemoteLiveViewNeedsHTTPS(t *testing.T) {
	opened := liveEnv(t, "http://live.example.com")
	r := runLive(t)
	if r.code != 0 {
		t.Fatal("a refused live view must not fail the run")
	}
	if !strings.Contains(r.stderr, "live view: off") {
		t.Fatalf("no reason on stderr:\n%s", r.stderr)
	}
	for _, o := range *opened {
		if strings.Contains(o, "live.example.com") {
			t.Fatalf("opened %v", *opened)
		}
	}
}

func TestLiveURLFlagBeatsTheEnvironment(t *testing.T) {
	good := newFakeBridge(t)
	other := newFakeBridge(t)
	liveEnv(t, other.srv.URL)
	if r := runLive(t, "-live-url", good.srv.URL); r.code != 0 {
		t.Fatal("exit")
	}
	if p, _, _, _, _ := good.snapshot(); p == 0 {
		t.Fatal("the flag was ignored")
	}
	if p, _, _, _, _ := other.snapshot(); p != 0 {
		t.Fatal("the environment beat the flag")
	}
}

func TestLiveCommand(t *testing.T) {
	bridge := newFakeBridge(t)
	opened := liveEnv(t, bridge.srv.URL)

	capture := func(args ...string) string {
		code, out, _ := captureCmd(t, func() int { return run(append([]string{"live"}, args...)) })
		if code != 0 {
			t.Fatalf("live %v: exit %d", args, code)
		}
		return strings.TrimSpace(out)
	}

	first := capture()
	id, _ := live.Load()
	if first != bridge.srv.URL+"/p/"+id.ProjectID {
		t.Fatalf("url %q", first)
	}
	if again := capture(); again != first {
		t.Fatalf("not stable: %q %q", again, first)
	}
	if len(*opened) != 0 {
		t.Fatal("opened without -open")
	}
	capture("-open")
	if len(*opened) != 1 || (*opened)[0] != first {
		t.Fatalf("opened %v", *opened)
	}
	if after := capture("reset"); after == first {
		t.Fatal("reset kept the project")
	}
	if code, _, _ := captureCmd(t, func() int { return run([]string{"live", "bogus"}) }); code != 2 {
		t.Fatalf("bogus argument: exit %d", code)
	}
	// The push token is never printed.
	code, out, errOut := captureCmd(t, func() int { return run([]string{"live"}) })
	id, _ = live.Load()
	if code != 0 || strings.Contains(out+errOut, id.PushToken) {
		t.Fatalf("exit %d, the token is in the output of live", code)
	}
}

func TestUsageNamesTheLiveCommandAndFlags(t *testing.T) {
	code, out, _ := captureCmd(t, func() int { return run([]string{"help"}) })
	if code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"live ", "VEGALOAD_LIVE", "-stream-file", "-stream-push", "-stream-insecure", "-run-id", "-label", "-live-url", "VEGALOAD_STREAM_TOKEN", "VEGALOAD_OPEN", "VEGALOAD_CONFIG_DIR"} {
		if !strings.Contains(out, want) {
			t.Errorf("vegaload help does not mention %q", want)
		}
	}
}

func TestRunHelpListsEveryNewFlag(t *testing.T) {
	_, _, errOut := captureCmd(t, func() int { return cmdRun([]string{"-h"}) })
	for _, want := range []string{"-stream-file", "-stream-push", "-stream-insecure", "-run-id", "-label", "\n  -live\n", "-no-live", "-live-url", "-no-open",
		"VEGALOAD_STREAM_FILE", "VEGALOAD_STREAM_PUSH", "VEGALOAD_STREAM_TOKEN", "VEGALOAD_RUN_ID", "VEGALOAD_LIVE=off", "VEGALOAD_LIVE_URL", "VEGALOAD_OPEN=off"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("run -h does not mention %q", want)
		}
	}
	if strings.Contains(strings.ToLower(errOut), "goja") {
		t.Error("run -h names the scripting engine")
	}
}
