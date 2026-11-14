package js

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/report"
)

// statusServer answers /status/N with status N.
func statusServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/status/"))
		if err != nil {
			code = http.StatusBadRequest
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A status of 400 or higher fails the call, not the iteration: the script
// still gets the answer and carries on.
func TestHTTPGlobal_StatusFailsTheCallNotTheIteration(t *testing.T) {
	srv := statusServer(t)
	path := writeScript(t, "scenario.js", `
		export default function () {
			const ok = http.get("`+srv.URL+`/status/200");
			const redirect = http.get("`+srv.URL+`/status/304");
			const bad = http.get("`+srv.URL+`/status/500");
			const missing = http.get("`+srv.URL+`/status/404");
			const wanted = http.get("`+srv.URL+`/status/404", { expectedStatuses: [404] });
			const unwanted = http.request("POST", "`+srv.URL+`/status/200", { expectedStatuses: [201] });
			if (ok.status !== 200 || redirect.status !== 304 || bad.status !== 500 || missing.status !== 404 || wanted.status !== 404 || unwanted.status !== 200) {
				throw new Error("statuses: " + [ok.status, redirect.status, bad.status, missing.status, wanted.status, unwanted.status]);
			}
			if (bad.ok || !ok.ok) throw new Error("ok is still 2xx only");
		}
	`)
	script, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	vu, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer vu.Close()
	col := report.NewCollector()
	vu.SetCallRecorder(col)
	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatalf("Iteration = %v, want no error: a failed status does not throw", err)
	}

	p, ok := col.Finish("fixed-vus", time.Second).Protocol("http")
	if !ok || p.Total != 6 || p.Failed != 3 {
		t.Fatalf("http = %+v, want 6 calls with 3 failed (500, 404, and 200 when 201 was expected)", p)
	}
	get, _ := p.Operation("GET")
	post, _ := p.Operation("POST")
	if get.Failed != 2 || post.Failed != 1 {
		t.Errorf("GET failed %d, POST failed %d, want 2 and 1", get.Failed, post.Failed)
	}
}

func TestHTTPGlobal_BadExpectedStatusesThrows(t *testing.T) {
	srv := statusServer(t)
	for _, opt := range []string{`[]`, `404`, `"404"`, `["404"]`, `[200.5]`, `[99]`, `[600]`, `{}`} {
		path := writeScript(t, "scenario.js", `
			export default function () {
				try {
					http.get("`+srv.URL+`/status/200", { expectedStatuses: `+opt+` });
				} catch (e) {
					if (String(e).indexOf("expectedStatuses") < 0) throw new Error("unclear error: " + e);
					return;
				}
				throw new Error("accepted");
			}
		`)
		script, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		vu, err := script.NewVU(nil, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		col := report.NewCollector()
		vu.SetCallRecorder(col)
		if err := vu.Iteration(context.Background()); err != nil {
			t.Errorf("expectedStatuses: %s: %v", opt, err)
		}
		if res := col.Finish("fixed-vus", time.Second); res.Protocols != nil {
			t.Errorf("expectedStatuses: %s: a refused option sent a call: %+v", opt, res.Protocols)
		}
		vu.Close()
	}
}

// null and undefined mean the option was not given.
func TestHTTPGlobal_ExpectedStatusesNullIsTheDefault(t *testing.T) {
	srv := statusServer(t)
	path := writeScript(t, "scenario.js", `
		export default function () {
			http.get("`+srv.URL+`/status/200", { expectedStatuses: null });
			http.get("`+srv.URL+`/status/500", { expectedStatuses: undefined });
		}
	`)
	script, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	vu, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer vu.Close()
	col := report.NewCollector()
	vu.SetCallRecorder(col)
	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p, _ := col.Finish("fixed-vus", time.Second).Protocol("http"); p.Total != 2 || p.Failed != 1 {
		t.Errorf("http = %+v, want 2 calls, the 500 failed", p)
	}
}
