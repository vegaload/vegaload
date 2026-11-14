package python

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/failure"
	"github.com/vegaload/vegaload/internal/report"
)

// optionsServer answers /slow after 300ms, /hop/N with a redirect to
// /hop/N-1 (and /hop/0 with 200), and anything else with its raw query. It
// counts the connections it accepts.
func optionsServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var conns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/slow":
			select {
			case <-r.Context().Done():
			case <-time.After(300 * time.Millisecond):
			}
		case strings.HasPrefix(r.URL.Path, "/hop/"):
			n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hop/"))
			if n > 0 {
				http.Redirect(w, r, "/hop/"+strconv.Itoa(n-1), http.StatusFound)
			}
		default:
			_, _ = w.Write([]byte(r.URL.RawQuery))
		}
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &conns
}

func runHTTPOnce(t *testing.T, src string, opts ...Option) *report.Result {
	t.Helper()
	script, err := Load(writeScript(t, src))
	if err != nil {
		t.Fatal(err)
	}
	vu, err := script.NewVU(nil, 5*time.Second, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer vu.Close()
	col := report.NewCollector()
	col.RequestsFromCalls()
	vu.SetCallRecorder(col)
	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatalf("Iteration: %v", err)
	}
	return col.Finish("fixed-vus", time.Second)
}

// The same options as JavaScript, with timeout in seconds.
func TestHTTPGlobal_TimeoutRedirectsAndParams(t *testing.T) {
	skipIfNoPython(t)
	srv, _ := optionsServer(t)
	res := runHTTPOnce(t, `
def iteration():
    base = "`+srv.URL+`"
    for limit in (0.05, "50ms"):
        try:
            http.get(base + "/slow", timeout=limit)
        except Exception:
            pass
        else:
            raise ValueError("timeout=%r did not stop a 300ms call" % (limit,))
    if http.get(base + "/slow", timeout=2).status != 200:
        raise ValueError("a 2s limit failed")

    first = http.get(base + "/hop/2", redirects=0)
    if first.status != 302 or first.headers["Location"] != "/hop/1":
        raise ValueError("redirects=0 got %d" % first.status)
    if http.get(base + "/hop/2", redirects=2).status != 200:
        raise ValueError("redirects=2 did not follow two hops")
    try:
        http.get(base + "/hop/3", redirects=2)
    except Exception as e:
        if "stopped after 2 redirects" not in str(e):
            raise
    else:
        raise ValueError("redirects=2 followed three hops")

    q = http.get(base + "/q?keep=1", params={"page": 2, "tag": ("a b", "c"), "on": True}).body
    if q != "keep=1&on=true&page=2&tag=a+b&tag=c":
        raise ValueError("params: %s" % q)
`)
	p, ok := res.Protocol("http")
	if !ok || p.Total != 7 || p.Failed != 3 {
		t.Fatalf("http = %+v, want 7 calls with 3 failed", p)
	}
	reasons := map[string]int64{}
	for _, fr := range res.FailureReasons {
		reasons[fr.Reason] = fr.Count
	}
	if reasons[failure.Timeout] != 2 || reasons[failure.Other] != 1 {
		t.Errorf("failure reasons %+v, want 2 timeout and 1 error", res.FailureReasons)
	}
}

func TestHTTPGlobal_BadRequestOptionsRaise(t *testing.T) {
	skipIfNoPython(t)
	srv, conns := optionsServer(t)
	for _, tc := range []struct{ opt, name string }{
		{`timeout=0`, "timeout"},
		{`timeout=-1`, "timeout"},
		{`timeout="soon"`, "timeout"},
		{`timeout=True`, "timeout"},
		{`redirects=-1`, "redirects"},
		{`redirects=1.5`, "redirects"},
		{`redirects="2"`, "redirects"},
		{`params="a=1"`, "params"},
		{`params={"a": None}`, "params"},
		{`params={"a": {"b": 1}}`, "params"},
		{`params={"a": object()}`, "params"},
	} {
		res := runHTTPOnce(t, `
def iteration():
    try:
        http.get("`+srv.URL+`/", `+tc.opt+`)
    except Exception as e:
        if "`+tc.name+`" not in str(e):
            raise ValueError("unclear error: %s" % e)
        return
    raise ValueError("accepted")
`)
		if res.Protocols != nil {
			t.Errorf("%s: a refused option sent a call: %+v", tc.opt, res.Protocols)
		}
	}
	if n := conns.Load(); n != 0 {
		t.Errorf("refused options opened %d connections", n)
	}
}

func TestHTTPGlobal_ConnectionReuseAndOptOut(t *testing.T) {
	skipIfNoPython(t)
	src := func(url string) string {
		return "def iteration():\n    for _ in range(10):\n        http.get(\"" + url + "/\")\n"
	}
	srv, conns := optionsServer(t)
	runHTTPOnce(t, src(srv.URL))
	if n := conns.Load(); n != 1 {
		t.Errorf("10 calls from one VU opened %d connections, want 1", n)
	}
	srv2, conns2 := optionsServer(t)
	runHTTPOnce(t, src(srv2.URL), WithNoConnectionReuse())
	if n := conns2.Load(); n != 10 {
		t.Errorf("10 calls with WithNoConnectionReuse opened %d connections, want 10", n)
	}
}
