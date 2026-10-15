package stream

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidatePushURL(t *testing.T) {
	ok := []struct {
		url      string
		insecure bool
	}{
		{"https://bridge.example.com/ingest", false},
		{"http://127.0.0.1:7080/x", false},
		{"http://localhost:7080/x", false},
		{"http://[::1]:7080/x", false},
		{"http://bridge.example.com/x", true},
	}
	for _, c := range ok {
		if _, err := ValidatePushURL(c.url, c.insecure); err != nil {
			t.Errorf("%s refused: %v", c.url, err)
		}
	}
	bad := []string{
		"http://bridge.example.com/x",        // remote http
		"ftp://bridge.example.com/x",         // not http
		"https://user:pw@bridge.example.com", // credentials
		"https://",                           // no host
		"bridge.example.com/x",               // no scheme
	}
	for _, u := range bad {
		_, err := ValidatePushURL(u, false)
		if err == nil {
			t.Errorf("%s accepted", u)
			continue
		}
		if strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: the error repeats the password: %v", u, err)
		}
	}
	// The path or query of a URL can itself be a secret: it is not repeated.
	_, err := ValidatePushURL("http://remote.example.com/secret-path?token=abc", false)
	if err == nil || strings.Contains(err.Error(), "secret-path") || strings.Contains(err.Error(), "abc") {
		t.Errorf("error = %v", err)
	}
}

type recv struct {
	mu      sync.Mutex
	hdr     []http.Header
	bodies  []string
	status  []int // answers in order; the last repeats
	calls   int32
	retryAf string
}

func (r *recv) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	i := int(atomic.AddInt32(&r.calls, 1)) - 1
	r.hdr = append(r.hdr, req.Header.Clone())
	r.bodies = append(r.bodies, string(b))
	st := 200
	if len(r.status) > 0 {
		st = r.status[min(i, len(r.status)-1)]
	}
	r.mu.Unlock()
	if r.retryAf != "" {
		w.Header().Set("Retry-After", r.retryAf)
	}
	w.WriteHeader(st)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func newPush(t *testing.T, h http.Handler, token string) (*PushSink, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL + "/ingest?source=test")
	if err != nil {
		t.Fatal(err)
	}
	return NewPushSink(PushOptions{
		URL: u, Token: token, Version: "9.9", MaxDelay: 20 * time.Millisecond,
		BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
	}), srv
}

func line(seq int) []byte {
	return []byte(fmt.Sprintf(`{"schema":"%s","runId":"r1","seq":%d,"type":"point","data":{}}`, Schema, seq))
}

func TestPushSendsBatchesWithHeadersAndBearerToken(t *testing.T) {
	rc := &recv{}
	p, _ := newPush(t, rc, "tok-123")
	rec := NewRecorder(RecorderOptions{RunID: "r1"}, p)
	rec.Started(time.Now(), startedAt())
	rec.Point(Point{Requests: 1})
	rec.Finished(map[string]int{}, true, 0)
	if stuck := rec.Close(3 * time.Second); len(stuck) != 0 {
		t.Fatalf("stuck: %v", stuck)
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.hdr) == 0 {
		t.Fatal("nothing was sent")
	}
	h := rc.hdr[0]
	if h.Get("Authorization") != "Bearer tok-123" {
		t.Errorf("Authorization = %q", h.Get("Authorization"))
	}
	if h.Get("Content-Type") != "application/x-ndjson" || h.Get("User-Agent") != "vegaload/9.9" {
		t.Errorf("headers: %v", h)
	}
	if h.Get("X-Vegaload-Run-Id") != "r1" || h.Get("X-Vegaload-Seq-From") != "1" {
		t.Errorf("run/seq headers: %v", h)
	}
	var lines []string
	for _, b := range rc.bodies {
		for _, l := range strings.Split(strings.TrimSpace(b), "\n") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 3 {
		t.Errorf("%d events received, want 3", len(lines))
	}
	if d, lost := p.Delivered(); d != 3 || lost != 0 {
		t.Errorf("delivered=%d lost=%d", d, lost)
	}
}

func TestPushRetriesServerErrorsWithTheSameSequenceRange(t *testing.T) {
	rc := &recv{status: []int{503, 429, 200}}
	p, _ := newPush(t, rc, "")
	_ = p.Write(line(1))
	_ = p.Write(line(2))
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.bodies) != 3 {
		t.Fatalf("%d attempts, want 3", len(rc.bodies))
	}
	for i, h := range rc.hdr {
		if h.Get("X-Vegaload-Seq-From") != "1" || h.Get("X-Vegaload-Seq-To") != "2" {
			t.Errorf("attempt %d range = %s..%s", i, h.Get("X-Vegaload-Seq-From"), h.Get("X-Vegaload-Seq-To"))
		}
		if rc.bodies[i] != rc.bodies[0] {
			t.Errorf("attempt %d body differs", i)
		}
	}
	if len(p.Warnings()) != 1 || !strings.Contains(p.Warnings()[0], "retrying") {
		t.Errorf("warnings = %v", p.Warnings())
	}
	if d, lost := p.Delivered(); d != 2 || lost != 0 {
		t.Errorf("delivered=%d lost=%d", d, lost)
	}
}

func TestPushDropsABatchTheReceiverRefusesAndKeepsGoing(t *testing.T) {
	rc := &recv{status: []int{400, 200}}
	p, _ := newPush(t, rc, "")
	_ = p.Write(line(1))
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = p.Write(line(2))
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	if rc.calls != 2 {
		t.Errorf("calls = %d, want 2 (a 400 is not retried)", rc.calls)
	}
	if d, lost := p.Delivered(); d != 1 || lost != 1 {
		t.Errorf("delivered=%d lost=%d, want 1 and 1", d, lost)
	}
	if w := p.Warnings(); len(w) != 1 || !strings.Contains(w[0], "400") {
		t.Errorf("warnings = %v", w)
	}
}

// A redirect could take the token to another host: it is an answer, not a path.
func TestPushDoesNotFollowRedirectsOrSendTheTokenOn(t *testing.T) {
	var elsewhere int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&elsewhere, 1)
	}))
	defer other.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer first.Close()
	u, _ := url.Parse(first.URL + "/ingest")
	p := NewPushSink(PushOptions{URL: u, Token: "SECRET-TOKEN"})
	_ = p.Write(line(1))
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&elsewhere) != 0 {
		t.Fatal("the redirect was followed")
	}
	if d, lost := p.Delivered(); d != 0 || lost != 1 {
		t.Errorf("delivered=%d lost=%d: a 307 is not an acknowledgement", d, lost)
	}
	for _, w := range p.Warnings() {
		if strings.Contains(w, "SECRET-TOKEN") {
			t.Errorf("warning has the token: %s", w)
		}
	}
}

func TestPushErrorsNeverContainTheTokenOrThePath(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(srv.URL + "/very/secret/path?key=QUERYSECRET")
	srv.Close() // connection refused
	p := NewPushSink(PushOptions{URL: u, Token: "TOKENSECRET", BackoffMin: time.Millisecond, BackoffMax: 2 * time.Millisecond})
	_ = p.Write(line(1))
	done := make(chan error, 1)
	go func() { done <- p.Flush() }()
	time.Sleep(60 * time.Millisecond)
	p.Abort()
	if err := <-done; err == nil {
		t.Fatal("an aborted flush should report it")
	}
	all := strings.Join(p.Warnings(), "\n") + p.Name()
	for _, secret := range []string{"TOKENSECRET", "QUERYSECRET", "very/secret"} {
		if strings.Contains(all, secret) {
			t.Errorf("%q leaked: %s", secret, all)
		}
	}
	if len(p.Warnings()) == 0 {
		t.Error("an unreachable receiver should leave a warning")
	}
}

// At exit the terminal event must get a few quick tries, not doubling waits.
func TestPushDrainShortensTheRetryWait(t *testing.T) {
	rc := &recv{status: []int{503, 503, 503, 200}}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	p := NewPushSink(PushOptions{URL: u, BackoffMin: 20 * time.Millisecond, BackoffMax: time.Hour})
	_ = p.Write(line(1))
	p.Drain()
	start := time.Now()
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v: the waits doubled instead of staying short", took)
	}
	if d, _ := p.Delivered(); d != 1 {
		t.Errorf("delivered = %d", d)
	}
}
