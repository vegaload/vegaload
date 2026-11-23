package netapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/failure"
)

// statusServer answers /status/N with status N, and /redirect/N with a 302
// to /status/N.
func statusServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code, ok := strings.CutPrefix(r.URL.Path, "/redirect/"); ok {
			http.Redirect(w, r, "/status/"+code, http.StatusFound)
			return
		}
		code, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/status/"))
		if err != nil {
			code = http.StatusBadRequest
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte("body"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// callLog is a CallRecorder that keeps each call's error.
type callLog struct {
	mu   sync.Mutex
	errs []error
}

func (l *callLog) RecordCall(_, _ string, _ time.Duration, err error, _, _ int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, err)
}

func (l *callLog) RecordMeasure(string, string, time.Duration) {}

func (l *callLog) last(t *testing.T) error {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.errs) == 0 {
		t.Fatal("no call was recorded")
	}
	return l.errs[len(l.errs)-1]
}

func TestHTTPClient_StatusDecidesTheCallOutcome(t *testing.T) {
	srv := statusServer(t)
	c := NewHTTPClient(nil, 5*time.Second)
	defer c.Close()
	log := &callLog{}
	c.SetCallRecorder(log)

	cases := []struct {
		path     string
		expected []int
		status   int  // what the script sees
		failed   bool // what the report counts
	}{
		{"/status/200", nil, 200, false},
		{"/status/201", nil, 201, false},
		{"/status/204", nil, 204, false},
		{"/status/304", nil, 304, false},
		{"/redirect/200", nil, 200, false}, // a redirect is followed, the final answer counts
		{"/redirect/404", nil, 404, true},
		{"/status/400", nil, 400, true},
		{"/status/401", nil, 401, true},
		{"/status/404", nil, 404, true},
		{"/status/429", nil, 429, true},
		{"/status/500", nil, 500, true},
		{"/status/503", nil, 503, true},
		// expectedStatuses replaces the default rule for that call.
		{"/status/404", []int{404}, 404, false},
		{"/status/503", []int{200, 503}, 503, false},
		{"/status/200", []int{201}, 200, true},
		// A 302 with no Location is not followed, so the script sees it.
		{"/status/302", nil, 302, false},
		{"/status/302", []int{200}, 302, true},
	}
	for _, tc := range cases {
		opts := Options{ExpectedStatuses: tc.expected}
		resp, err := c.Do(context.Background(), http.MethodGet, srv.URL+tc.path, nil, opts)
		if err != nil {
			t.Fatalf("%s %v: Do returned an error, want the answer whatever its status: %v", tc.path, tc.expected, err)
		}
		if resp.StatusCode != tc.status {
			t.Errorf("%s %v: status %d, want %d", tc.path, tc.expected, resp.StatusCode, tc.status)
		}
		// 204 and 304 have no body by definition; every other answer keeps it.
		if tc.status != 204 && tc.status != 304 && string(resp.Body) != "body" {
			t.Errorf("%s %v: body %q, want the body of a failed answer too", tc.path, tc.expected, resp.Body)
		}
		got := log.last(t)
		if (got != nil) != tc.failed {
			t.Errorf("%s expected=%v: recorded error %v, want failed=%v", tc.path, tc.expected, got, tc.failed)
			continue
		}
		if tc.failed {
			if reason := failure.Reason(got); reason != "status "+strconv.Itoa(tc.status) {
				t.Errorf("%s: reason %q, want %q", tc.path, reason, "status "+strconv.Itoa(tc.status))
			}
		}
	}
}

func TestHTTPClient_TimeoutIsAFailedCallWithItsRealTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	c := NewHTTPClient(nil, 80*time.Millisecond)
	defer c.Close()
	var got struct {
		d   time.Duration
		err error
	}
	c.SetCallRecorder(recorderFunc(func(d time.Duration, err error) { got.d, got.err = d, err }))

	if _, err := c.Do(context.Background(), http.MethodGet, srv.URL, nil, Options{}); err == nil {
		t.Fatal("the call did not time out")
	}
	if failure.Reason(got.err) != failure.Timeout {
		t.Errorf("reason = %q (%v), want timeout", failure.Reason(got.err), got.err)
	}
	if got.d < 80*time.Millisecond || got.d > time.Second {
		t.Errorf("recorded duration %s, want the time it took (about 80ms), not 0", got.d)
	}
}

type recorderFunc func(d time.Duration, err error)

func (f recorderFunc) RecordCall(_, _ string, d time.Duration, err error, _, _ int64) { f(d, err) }
func (f recorderFunc) RecordMeasure(string, string, time.Duration)                    {}

func TestCheckExpectedStatuses(t *testing.T) {
	for _, ok := range [][]int{nil, {200}, {100, 599}, {204, 404, 503}} {
		if err := CheckExpectedStatuses(ok); err != nil {
			t.Errorf("CheckExpectedStatuses(%v) = %v, want nil", ok, err)
		}
	}
	for list, want := range map[string][]int{
		"the list is empty":         {},
		"99 is not an HTTP status":  {99},
		"600 is not an HTTP status": {200, 600},
		"-1 is not an HTTP status":  {-1},
	} {
		err := CheckExpectedStatuses(want)
		if err == nil || !strings.Contains(err.Error(), list) {
			t.Errorf("CheckExpectedStatuses(%v) = %v, want an error with %q", want, err, list)
		}
	}
}

func TestHTTPClient_RefusesABadExpectedStatusesBeforeSending(t *testing.T) {
	var sent bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent = true }))
	defer srv.Close()
	c := NewHTTPClient(nil, time.Second)
	defer c.Close()
	log := &callLog{}
	c.SetCallRecorder(log)
	if _, err := c.Do(context.Background(), http.MethodGet, srv.URL, nil, Options{ExpectedStatuses: []int{}}); err == nil {
		t.Fatal("an empty expectedStatuses was accepted")
	}
	if sent || len(log.errs) != 0 {
		t.Errorf("sent %v, recorded %d calls; a refused option must not send or count a call", sent, len(log.errs))
	}
}
