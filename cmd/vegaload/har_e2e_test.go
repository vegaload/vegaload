package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/report"
)

// journeyServer answers the calls of testdata/journey.har. /api/cart needs
// a multipart form it can parse, and /api/me needs the cookie /api/login
// set, so a call that is not rebuilt right fails.
type journeyServer struct {
	*httptest.Server
	mu       sync.Mutex
	issued   map[string]bool
	uploads  int
	rejected []string
}

func newJourneyServer(t *testing.T) *journeyServer {
	t.Helper()
	js := &journeyServer{issued: map[string]bool{}}
	reject := func(w http.ResponseWriter, code int, why string) {
		js.mu.Lock()
		js.rejected = append(js.rejected, why)
		js.mu.Unlock()
		http.Error(w, why, code)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>shop</html>")) })
	mux.HandleFunc("GET /api/products", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[{"id":42}]`)) })
	mux.HandleFunc("GET /api/products/{id}", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id":42}`)) })
	mux.HandleFunc("POST /api/cart", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			reject(w, http.StatusBadRequest, "multipart: "+err.Error())
			return
		}
		f, fh, err := r.FormFile("note")
		if err != nil {
			reject(w, http.StatusBadRequest, "no file: "+err.Error())
			return
		}
		defer f.Close()
		buf := make([]byte, 64)
		n, _ := f.Read(buf)
		if r.FormValue("sku") != "42" || r.FormValue("qty") != "1" || fh.Filename != "note.txt" || string(buf[:n]) != "gift wrap" {
			reject(w, http.StatusBadRequest, fmt.Sprintf("fields %v file %q %q", r.MultipartForm.Value, fh.Filename, buf[:n]))
			return
		}
		js.mu.Lock()
		js.uploads++
		js.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		sid := fmt.Sprintf("s%d", time.Now().UnixNano())
		js.mu.Lock()
		js.issued[sid] = true
		js.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: sid, Path: "/"})
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("sid")
		js.mu.Lock()
		ok := err == nil && js.issued[c.Value]
		js.mu.Unlock()
		if !ok {
			reject(w, http.StatusUnauthorized, "no session cookie")
			return
		}
		_, _ = w.Write([]byte(`{"user":"a"}`))
	})
	js.Server = httptest.NewServer(mux)
	t.Cleanup(js.Close)
	return js
}

// An imported HAR replays with its recorded think time, every row grouped
// and named, a rebuilt multipart form and the cookie jar, and the sleeps
// are in the iteration time but not in any call's latency.
func TestImportHAR_ReplaysGroupedNamedWithThinkTime(t *testing.T) {
	srv := newJourneyServer(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "journey.har"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	harPath := filepath.Join(dir, "journey.har")
	host := strings.TrimPrefix(srv.URL, "http://")
	if err := os.WriteFile(harPath, []byte(strings.ReplaceAll(string(raw), "HOST", host)), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "journey.vl.js")
	if code := cmdImport([]string{"har", harPath, "-o", script}); code != 0 {
		t.Fatalf("import exit %d", code)
	}
	src, _ := os.ReadFile(script)
	for _, want := range []string{
		"sleep(0.03);", "sleep(0.3);", "sleep(0.4);", "sleep(0.1);",
		`group("Home", () => {`, `group("Product", () => {`, `group("Checkout", () => {`,
		`name: "/api/products/{id}",`, `body: multipart(`,
	} {
		if !strings.Contains(string(src), want) {
			t.Fatalf("missing %q in\n%s", want, src)
		}
	}
	if strings.Contains(string(src), "app.css") || strings.Contains(string(src), "google-analytics") {
		t.Fatalf("a static file or an analytics call was kept:\n%s", src)
	}

	out := filepath.Join(dir, "r.json")
	code, stdout, stderr := captureCmd(t, func() int {
		return cmdRun([]string{"-no-live", "-no-report", "-out", out, "-audit-log", filepath.Join(dir, "a.log"),
			"-vus", "2", "-duration", "3s", script})
	})
	if code != 0 {
		t.Fatalf("run exit %d\n%s\n%s\nrejected: %v\n%s", code, stdout, stderr, srv.rejected, src)
	}
	res := readResult(t, out)
	if res.Failed != 0 || len(srv.rejected) > 0 {
		t.Fatalf("failed %d, rejected %v", res.Failed, srv.rejected)
	}

	var rows []string
	for _, row := range report.Endpoints(res) {
		rows = append(rows, row.Name)
	}
	sort.Strings(rows)
	want := []string{"GET /", "GET /api/me", "GET /api/products", "GET /api/products/{id}", "POST /api/cart", "POST /api/login"}
	if strings.Join(rows, ",") != strings.Join(want, ",") {
		t.Fatalf("rows %v, want %v", rows, want)
	}
	var steps []string
	for _, s := range res.Steps {
		steps = append(steps, s.Name)
	}
	if strings.Join(steps, ",") != "Home,Product,Checkout" {
		t.Fatalf("groups %v", steps)
	}

	srv.mu.Lock()
	uploads := srv.uploads
	srv.mu.Unlock()
	cart := int64(0)
	for _, row := range report.Endpoints(res) {
		if row.Name == "POST /api/cart" {
			cart = row.Total
		}
	}
	if uploads == 0 || int64(uploads) != cart {
		t.Fatalf("the server parsed %d multipart forms, the run sent %d", uploads, cart)
	}

	// 1.13s of sleeps per iteration: in the iteration, not in the calls.
	if res.Iterations == nil || res.Iterations.Duration.Mean < 1100*time.Millisecond {
		t.Fatalf("iterations %+v should hold the sleeps", res.Iterations)
	}
	if res.Latency.P95 > 100*time.Millisecond || res.Latency.P50 > 50*time.Millisecond {
		t.Fatalf("call p50 %v, p95 %v hold the sleeps", res.Latency.P50, res.Latency.P95)
	}
	t.Logf("calls p50=%v p95=%v; iterations mean=%v; rows=%v; groups=%v", res.Latency.P50, res.Latency.P95, res.Iterations.Duration.Mean, rows, steps)
}

func TestImportHAR_ThinkTimeFlag(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "journey.har"))
	if err != nil {
		t.Fatal(err)
	}
	harPath := writeTemp(t, "journey.har", strings.ReplaceAll(string(raw), "HOST", "127.0.0.1:1"))
	for mode, want := range map[string]string{"none": "", "fixed:1s": "sleep(1);", "capped:0.2s": "sleep(0.2);"} {
		out := filepath.Join(t.TempDir(), "j.vl.js")
		if code := cmdImport([]string{"har", harPath, "-think-time", mode, "-o", out}); code != 0 {
			t.Fatalf("%s: exit %d", mode, code)
		}
		src, _ := os.ReadFile(out)
		if want == "" && strings.Contains(string(src), "sleep(") || want != "" && !strings.Contains(string(src), want) {
			t.Errorf("-think-time %s:\n%s", mode, src)
		}
	}
	if code := cmdImport([]string{"har", harPath, "-think-time", "soon", "-o", "-"}); code != 2 {
		t.Errorf("a bad -think-time exits %d, want 2", code)
	}
}
