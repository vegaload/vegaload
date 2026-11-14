package netapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/failure"
)

// connServer counts the connections a client opens to it, and the ones that
// have closed.
type connServer struct {
	*httptest.Server
	opened, closed atomic.Int64
}

func newConnServer(t *testing.T, tlsOn bool, h http.HandlerFunc) *connServer {
	t.Helper()
	if h == nil {
		h = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }
	}
	cs := &connServer{Server: httptest.NewUnstartedServer(h)}
	cs.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			cs.opened.Add(1)
		case http.StateClosed, http.StateHijacked:
			cs.closed.Add(1)
		}
	}
	if tlsOn {
		cs.StartTLS()
	} else {
		cs.Start()
	}
	t.Cleanup(cs.Close)
	return cs
}

func get(t *testing.T, c *HTTPClient, rawURL string, opts Options) *HTTPResponse {
	t.Helper()
	resp, err := c.Do(context.Background(), http.MethodGet, rawURL, nil, opts)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	return resp
}

// N VUs, each with its own client, making many calls at once open about N
// connections, not one per call. Before, every VU shared
// http.DefaultTransport, which keeps two idle connections per host.
func TestHTTPClient_VUsReuseTheirConnections(t *testing.T) {
	srv := newConnServer(t, false, nil)
	const vus, calls = 8, 40
	var wg sync.WaitGroup
	for i := 0; i < vus; i++ {
		c := NewHTTPClient(nil, 5*time.Second)
		defer c.Close()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < calls; j++ {
				if _, err := c.Do(context.Background(), http.MethodGet, srv.URL, nil, Options{}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if n := srv.opened.Load(); n > vus {
		t.Errorf("%d VUs making %d calls each opened %d connections, want at most %d", vus, calls, n, vus)
	}
}

func TestHTTPClient_InsecureReusesOneConnection(t *testing.T) {
	srv := newConnServer(t, true, nil)
	c := NewHTTPClient(nil, 5*time.Second)
	defer c.Close()
	for i := 0; i < 20; i++ {
		get(t, c, srv.URL, Options{InsecureSkipVerify: true})
	}
	if n := srv.opened.Load(); n != 1 {
		t.Errorf("20 insecure calls opened %d connections, want 1", n)
	}
	// Without insecure the self-signed certificate is refused: the pools
	// stay apart.
	if _, err := c.Do(context.Background(), http.MethodGet, srv.URL, nil, Options{}); err == nil || failure.Reason(err) != failure.TLS {
		t.Errorf("a verified call to a self-signed server: %v, want a tls failure", err)
	}
}

func TestHTTPClient_NoConnectionReuseOpensOnePerCall(t *testing.T) {
	srv := newConnServer(t, false, nil)
	c := NewHTTPClient(nil, 5*time.Second, NoConnectionReuse())
	defer c.Close()
	for i := 0; i < 10; i++ {
		get(t, c, srv.URL, Options{})
	}
	if n := srv.opened.Load(); n != 10 {
		t.Errorf("10 calls with no reuse opened %d connections, want 10", n)
	}
}

func TestHTTPClient_CloseClosesIdleConnections(t *testing.T) {
	srv := newConnServer(t, false, nil)
	c := NewHTTPClient(nil, 5*time.Second)
	get(t, c, srv.URL, Options{})
	get(t, c, srv.URL, Options{InsecureSkipVerify: true})
	c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for srv.closed.Load() < srv.opened.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if o, cl := srv.opened.Load(), srv.closed.Load(); cl != o {
		t.Errorf("after Close, %d of %d connections are closed, want all", cl, o)
	}
}

func TestHTTPClient_TimeoutOptionReplacesTheClientLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}))
	defer srv.Close()

	// Shorter than the client's limit: the call times out at its own.
	c := NewHTTPClient(nil, 5*time.Second)
	defer c.Close()
	log := &callLog{}
	c.SetCallRecorder(log)
	start := time.Now()
	if _, err := c.Do(context.Background(), http.MethodGet, srv.URL, nil, Options{Timeout: 40 * time.Millisecond}); err == nil {
		t.Fatal("a 40ms call to a 200ms server did not time out")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the call took %s, want about 40ms", d)
	}
	if r := failure.Reason(log.last(t)); r != failure.Timeout {
		t.Errorf("reason %q, want timeout", r)
	}

	// Longer than the client's limit: the call has time to finish.
	short := NewHTTPClient(nil, 40*time.Millisecond)
	defer short.Close()
	if _, err := short.Do(context.Background(), http.MethodGet, srv.URL, nil, Options{Timeout: 2 * time.Second}); err != nil {
		t.Errorf("a 2s call to a 200ms server: %v", err)
	}
}

// hopServer answers /hop/N with a redirect to /hop/N-1, and /hop/0 with 200.
func hopServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hop/"))
		if n > 0 {
			http.Redirect(w, r, "/hop/"+strconv.Itoa(n-1), http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("done"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPClient_RedirectsOption(t *testing.T) {
	srv := hopServer(t)
	c := NewHTTPClient(nil, 5*time.Second)
	defer c.Close()
	n := func(i int) *int { return &i }

	if resp := get(t, c, srv.URL+"/hop/3", Options{Redirects: n(0)}); resp.StatusCode != http.StatusFound || resp.Headers.Get("Location") != "/hop/2" {
		t.Errorf("redirects 0: status %d, Location %q, want the 302 itself", resp.StatusCode, resp.Headers.Get("Location"))
	}
	if resp := get(t, c, srv.URL+"/hop/3", Options{Redirects: n(3)}); resp.StatusCode != 200 {
		t.Errorf("redirects 3 on three hops: status %d, want 200", resp.StatusCode)
	}
	_, err := c.Do(context.Background(), http.MethodGet, srv.URL+"/hop/3", nil, Options{Redirects: n(2)})
	if err == nil || !strings.Contains(err.Error(), "stopped after 2 redirects") {
		t.Errorf("redirects 2 on three hops: %v, want stopped after 2 redirects", err)
	}
	// The default follows up to 10.
	if resp := get(t, c, srv.URL+"/hop/10", Options{}); resp.StatusCode != 200 {
		t.Errorf("default on ten hops: status %d, want 200", resp.StatusCode)
	}
	if _, err := c.Do(context.Background(), http.MethodGet, srv.URL+"/hop/11", nil, Options{}); err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Errorf("default on eleven hops: %v, want stopped after 10 redirects", err)
	}
}

// A redirect connects to a host of its own, so it passes the safety check
// too.
func TestHTTPClient_RedirectToARefusedHostIsRefused(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) }))
	defer target.Close()
	u, _ := url.Parse(target.URL)
	other := "http://localhost:" + u.Port() + "/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other, http.StatusFound)
	}))
	defer srv.Close()
	refused := errors.New("host refused")
	c := NewHTTPClient(func(host string) error {
		if host != "127.0.0.1" {
			return refused
		}
		return nil
	}, 5*time.Second)
	defer c.Close()
	_, err := c.Do(context.Background(), http.MethodGet, srv.URL, nil, Options{})
	if !errors.Is(err, refused) {
		t.Errorf("a redirect to a refused host: %v, want it refused", err)
	}
}

func TestHTTPClient_ParamsAreAddedToTheQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.RawQuery))
	}))
	defer srv.Close()
	c := NewHTTPClient(nil, 5*time.Second)
	defer c.Close()
	params := url.Values{"q": {"a b&c"}, "page": {"2"}, "tag": {"x", "y"}}
	if got := string(get(t, c, srv.URL+"/s", Options{Params: params}).Body); got != "page=2&q=a+b%26c&tag=x&tag=y" {
		t.Errorf("query %q", got)
	}
	// A query already in the URL is kept as written.
	if got := string(get(t, c, srv.URL+"/s?keep=1&flag", Options{Params: url.Values{"page": {"3"}}}).Body); got != "keep=1&flag&page=3" {
		t.Errorf("query %q", got)
	}
}

func TestParseTimeout(t *testing.T) {
	for _, tc := range []struct {
		v    any
		unit time.Duration
		want time.Duration
	}{
		{nil, time.Millisecond, 0},
		{int64(250), time.Millisecond, 250 * time.Millisecond},
		{1.5, time.Second, 1500 * time.Millisecond},
		{"750ms", time.Second, 750 * time.Millisecond},
		{"2s", time.Millisecond, 2 * time.Second},
	} {
		got, err := ParseTimeout(tc.v, tc.unit)
		if err != nil || got != tc.want {
			t.Errorf("ParseTimeout(%v, %s) = %s, %v; want %s", tc.v, tc.unit, got, err, tc.want)
		}
	}
	for _, bad := range []any{int64(0), -1.0, "0s", "-1s", "soon", true, []any{1}} {
		if _, err := ParseTimeout(bad, time.Millisecond); err == nil {
			t.Errorf("ParseTimeout(%v) accepted it", bad)
		}
	}
}

func TestParseRedirects(t *testing.T) {
	if r, err := ParseRedirects(nil); r != nil || err != nil {
		t.Errorf("nil: %v, %v", r, err)
	}
	for v, want := range map[any]int{int64(0): 0, 3.0: 3, int64(20): 20} {
		if r, err := ParseRedirects(v); err != nil || r == nil || *r != want {
			t.Errorf("ParseRedirects(%v) = %v, %v; want %d", v, r, err, want)
		}
	}
	for _, bad := range []any{-1.0, 1.5, "3", true} {
		if _, err := ParseRedirects(bad); err == nil {
			t.Errorf("ParseRedirects(%v) accepted it", bad)
		}
	}
}

func TestParseParams(t *testing.T) {
	got, err := ParseParams(map[string]any{"a": "x", "n": int64(2), "f": 1.5, "b": true, "l": []any{"p", 3.0}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Encode() != "a=x&b=true&f=1.5&l=p&l=3&n=2" {
		t.Errorf("Encode() = %q", got.Encode())
	}
	for _, bad := range []any{"a=1", []any{"a"}, map[string]any{"a": nil}, map[string]any{"a": map[string]any{}}} {
		if _, err := ParseParams(bad); err == nil {
			t.Errorf("ParseParams(%v) accepted it", bad)
		}
	}
}
