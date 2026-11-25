package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vegaload/vegaload/internal/live"
)

// hostedBridge stands in for a service in public mode: it issues projects, and
// only its own issued ids can be pushed to.
type hostedBridge struct {
	srv *httptest.Server

	mu       sync.Mutex
	issued   int
	gone     map[string]bool // view keys that now answer 404
	refuse   int             // when set, POST /v1/projects answers this
	accept   bool
	pushes   []string // "id token"
	creates  int
	received strings.Builder
}

func newHostedBridge(t *testing.T) *hostedBridge {
	t.Helper()
	b := &hostedBridge{gone: map[string]bool{}, accept: true}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/health":
			_, _ = w.Write([]byte(`{"status":"ok","projects":true}`))
		case r.URL.Path == "/v1/info":
			fmt.Fprintf(w, `{"name":"rt-bridge","mode":"public","acceptingProjects":%v,"limits":{"retentionSeconds":604800}}`, b.accept)
		case r.Method == "POST" && r.URL.Path == "/v1/projects":
			b.creates++
			if b.refuse != 0 {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(b.refuse)
				_, _ = w.Write([]byte(`{"error":"too many requests, slow down","code":"rate_limited"}`))
				return
			}
			b.issued++
			n := b.issued
			w.WriteHeader(http.StatusCreated)
			// pushUrl points elsewhere on purpose: a client must not use it.
			fmt.Fprintf(w, `{"id":"s-project%02dxxxxxxxxxxxxxxxx","pushToken":"push-token-%02d-0123456789abcdef","viewKey":"s-project%02dxxxxxxxxxxxxxxxx_viewkey%02d0123456789","pushUrl":"https://elsewhere.invalid/ingest"}`, n, n, n, n)
		case strings.HasPrefix(r.URL.Path, "/api/projects/"):
			key := strings.TrimPrefix(r.URL.Path, "/api/projects/")
			if b.gone[key] {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		case strings.HasPrefix(r.URL.Path, "/v1/projects/") && strings.HasSuffix(r.URL.Path, "/ingest"):
			body, _ := io.ReadAll(r.Body)
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/projects/"), "/ingest")
			b.pushes = append(b.pushes, id+" "+r.Header.Get("Authorization"))
			b.received.Write(body)
			_, _ = w.Write([]byte(`{"accepted":1,"duplicates":0,"ignored":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *hostedBridge) snapshot() (creates int, pushes []string, body string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.creates, append([]string(nil), b.pushes...), b.received.String()
}

func projectKey(n int) string {
	return fmt.Sprintf("s-project%02dxxxxxxxxxxxxxxxx_viewkey%02d0123456789", n, n)
}

func TestHostedServiceIssuesTheProjectAndTheRunShowsTheViewKeyPage(t *testing.T) {
	b := newHostedBridge(t)
	opened := liveEnv(t, b.srv.URL)

	r := runLive(t)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	// The page is the view key's, never the project id's or the push token's.
	if len(*opened) != 1 || (*opened)[0] != b.srv.URL+"/p/"+projectKey(1) {
		t.Fatalf("opened %v", *opened)
	}
	creates, pushes, body := b.snapshot()
	if creates != 1 || len(pushes) == 0 {
		t.Fatalf("creates %d, pushes %d", creates, len(pushes))
	}
	for _, p := range pushes {
		if p != "s-project01xxxxxxxxxxxxxxxx Bearer push-token-01-0123456789abcdef" {
			t.Fatalf("a push went as %q", p)
		}
	}
	if !strings.Contains(r.stderr, "Anyone with this link can see the run. The service keeps it for 7 days") {
		t.Fatalf("the person was not told who can see it and for how long:\n%s", r.stderr)
	}
	rep, _ := os.ReadFile(r.reportPath)
	for _, secret := range []string{"push-token-01-0123456789abcdef", projectKey(1), "s-project01xxxxxxxxxxxxxxxx"} {
		for name, content := range map[string]string{"-out": string(r.outRaw), "html report": string(rep), "audit": r.audit, "events": body, "stdout": r.stdout} {
			if strings.Contains(content, secret) {
				t.Errorf("%q is in the %s", secret, name)
			}
		}
	}
}

func TestSecondRunUsesTheSameHostedProject(t *testing.T) {
	b := newHostedBridge(t)
	opened := liveEnv(t, b.srv.URL)
	for i := 0; i < 2; i++ {
		if r := runLive(t); r.code != 0 {
			t.Fatalf("run %d: exit %d", i, r.code)
		}
	}
	if creates, _, _ := b.snapshot(); creates != 1 {
		t.Fatalf("%d projects asked for, want 1", creates)
	}
	if len(*opened) != 2 || (*opened)[0] != (*opened)[1] {
		t.Fatalf("opened %v", *opened)
	}
}

func TestDeletedHostedProjectIsReplacedNotPushedToForever(t *testing.T) {
	b := newHostedBridge(t)
	opened := liveEnv(t, b.srv.URL)
	if r := runLive(t); r.code != 0 {
		t.Fatal("first run")
	}
	b.mu.Lock()
	b.gone[projectKey(1)] = true
	b.mu.Unlock()
	if r := runLive(t); r.code != 0 {
		t.Fatal("second run")
	}
	if len(*opened) != 2 || (*opened)[1] != b.srv.URL+"/p/"+projectKey(2) {
		t.Fatalf("opened %v", *opened)
	}
	_, pushes, _ := b.snapshot()
	if last := pushes[len(pushes)-1]; !strings.HasPrefix(last, "s-project02") {
		t.Fatalf("the second run still pushed to the old project: %q", last)
	}
}

func TestHostedServiceThatRefusesNeverStopsTheRun(t *testing.T) {
	for _, status := range []int{429, 503, 507} {
		b := newHostedBridge(t)
		b.refuse = status
		opened := liveEnv(t, b.srv.URL)
		r := runLive(t)
		if r.code != 0 {
			t.Fatalf("%d: the run failed with exit %d", status, r.code)
		}
		// Without a live view the HTML report opens, as it always has.
		for _, u := range *opened {
			if strings.HasPrefix(u, b.srv.URL) {
				t.Fatalf("%d: a live page was opened: %v", status, *opened)
			}
		}
		if _, pushes, _ := b.snapshot(); len(pushes) != 0 {
			t.Fatalf("%d: pushed without a project", status)
		}
		want := fmt.Sprintf("answered %d", status)
		if !strings.Contains(r.stderr, want) || !strings.Contains(r.stderr, "try again in 2s") || !strings.Contains(r.stderr, "goes on without it") {
			t.Fatalf("%d: stderr does not say why:\n%s", status, r.stderr)
		}
	}
}

func TestProjectInTheEnvironmentNeedsNoFileAndNoRequest(t *testing.T) {
	b := newHostedBridge(t)
	opened := liveEnv(t, b.srv.URL)
	cfg := t.TempDir()
	t.Setenv("VEGALOAD_CONFIG_DIR", cfg)
	t.Setenv("VEGALOAD_LIVE_PROJECT", "s-fromci0123456789abcdefgh")
	t.Setenv("VEGALOAD_LIVE_TOKEN", "ci-token-0123456789abcdef")
	t.Setenv("VEGALOAD_LIVE_VIEW_KEY", "s-fromci0123456789abcdefgh_cikey0123456789")

	if r := runLive(t); r.code != 0 {
		t.Fatalf("run: %s", r.stderr)
	}
	creates, pushes, _ := b.snapshot()
	if creates != 0 || len(pushes) == 0 || pushes[0] != "s-fromci0123456789abcdefgh Bearer ci-token-0123456789abcdef" {
		t.Fatalf("creates %d, pushes %v", creates, pushes)
	}
	if len(*opened) != 1 || (*opened)[0] != b.srv.URL+"/p/s-fromci0123456789abcdefgh_cikey0123456789" {
		t.Fatalf("opened %v", *opened)
	}
	if _, err := os.Stat(filepath.Join(cfg, "live.json")); err == nil {
		t.Fatal("a file was written for a project that the environment names")
	}
}

func TestHalfAProjectInTheEnvironmentIsRefusedBeforeAnythingIsSent(t *testing.T) {
	b := newHostedBridge(t)
	liveEnv(t, b.srv.URL)
	t.Setenv("VEGALOAD_LIVE_PROJECT", "s-fromci0123456789abcdefgh")
	r := runLive(t)
	if r.code != 0 || !strings.Contains(r.stderr, "go together") {
		t.Fatalf("exit %d:\n%s", r.code, r.stderr)
	}
	if creates, pushes, _ := b.snapshot(); creates != 0 || len(pushes) != 0 {
		t.Fatal("something was sent")
	}
}

func TestServiceAnswerWithUnsafeValuesIsNotUsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			_, _ = w.Write([]byte(`{"status":"ok","projects":true}`))
		case "/v1/info":
			_, _ = w.Write([]byte(`{"mode":"public","acceptingProjects":true}`))
		case "/v1/projects":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":"../../x","pushToken":"push-token-0123456789abcdef","viewKey":"key-0123456789abcdefgh"}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	opened := liveEnv(t, srv.URL)
	r := runLive(t)
	for _, u := range *opened {
		if strings.HasPrefix(u, srv.URL) {
			t.Fatalf("a live page was opened: %v", *opened)
		}
	}
	if r.code != 0 || !strings.Contains(r.stderr, "does not accept") {
		t.Fatalf("exit %d:\n%s", r.code, r.stderr)
	}
}

func TestLiveCommandOnAHostedServicePrintsTheViewKeyPage(t *testing.T) {
	b := newHostedBridge(t)
	liveEnv(t, b.srv.URL)
	code, out, _ := captureCmd(t, func() int { return cmdLive(nil) })
	if code != 0 || strings.TrimSpace(out) != b.srv.URL+"/p/"+projectKey(1) {
		t.Fatalf("exit %d printed %q", code, out)
	}
	captureCmd(t, func() int { return cmdLive([]string{"reset"}) })
	if id, _ := live.Load(); id == nil || len(id.Issued) != 1 {
		t.Fatalf("identity after reset: %+v", id)
	}
	if creates, _, _ := b.snapshot(); creates != 2 {
		t.Fatalf("%d projects asked for after a reset, want 2", creates)
	}
}
