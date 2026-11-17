package python

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

func newRecordedVU(t *testing.T, src string) (*VU, *report.Collector) {
	t.Helper()
	script, err := Load(writeScript(t, src))
	if err != nil {
		t.Fatal(err)
	}
	vu, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vu.Close() })
	col := report.NewCollector()
	vu.SetCallRecorder(col)
	return vu, col
}

// A status of 400 or higher fails the call, not the iteration, the same as
// in JavaScript.
func TestHTTPGlobal_StatusFailsTheCallNotTheIteration(t *testing.T) {
	skipIfNoPython(t)
	srv := statusServer(t)
	vu, col := newRecordedVU(t, `
def iteration():
    base = "`+srv.URL+`/status/"
    got = [
        http.get(base + "200").status,
        http.get(base + "304").status,
        http.get(base + "500").status,
        http.get(base + "404").status,
        http.get(base + "404", expected_statuses=[404]).status,
        http.get(base + "503", expected_statuses=range(500, 600)).status,
        http.post(base + "200", expected_statuses=(201,)).status,
    ]
    if got != [200, 304, 500, 404, 404, 503, 200]:
        raise ValueError("statuses: %r" % got)
`)
	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatalf("Iteration = %v, want no error: a failed status does not raise", err)
	}
	p, ok := col.Finish("fixed-vus", time.Second).Protocol("http")
	if !ok || p.Total != 7 || p.Failed != 3 {
		t.Fatalf("http = %+v, want 7 calls with 3 failed (500, 404, and 200 when 201 was expected)", p)
	}
}

func TestHTTPGlobal_BadExpectedStatusesRaises(t *testing.T) {
	skipIfNoPython(t)
	srv := statusServer(t)
	for _, opt := range []string{`[]`, `404`, `["404"]`, `[200.5]`, `[True]`, `[99]`, `[600]`} {
		vu, col := newRecordedVU(t, `
def iteration():
    try:
        http.get("`+srv.URL+`/status/200", expected_statuses=`+opt+`)
    except Exception as e:
        if "expected_statuses" not in str(e):
            raise ValueError("unclear error: %s" % e)
        return
    raise ValueError("accepted")
`)
		if err := vu.Iteration(context.Background()); err != nil {
			t.Errorf("expected_statuses=%s: %v", opt, err)
		}
		if res := col.Finish("fixed-vus", time.Second); res.Protocols != nil {
			t.Errorf("expected_statuses=%s: a refused option sent a call: %+v", opt, res.Protocols)
		}
	}
}
