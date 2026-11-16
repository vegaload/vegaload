package netapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSleepArgs(t *testing.T) {
	d, err := SleepArgs([]any{1.5})
	if err != nil || d != 1500*time.Millisecond {
		t.Fatalf("one argument: %v %v", d, err)
	}
	d, err = SleepArgs([]any{int64(2)})
	if err != nil || d != 2*time.Second {
		t.Fatalf("whole number: %v %v", d, err)
	}
	for range 50 {
		d, err = SleepArgs([]any{0.1, 0.3})
		if err != nil || d < 100*time.Millisecond || d > 300*time.Millisecond {
			t.Fatalf("range: %v %v", d, err)
		}
	}
	d, err = SleepArgs([]any{0.2, 0.2})
	if err != nil || d != 200*time.Millisecond {
		t.Fatalf("equal ends: %v %v", d, err)
	}
	for _, bad := range []struct {
		args []any
		want string
	}{
		{nil, "want sleep(seconds) or sleep(min, max)"},
		{[]any{1.0, 2.0, 3.0}, "want sleep(seconds) or sleep(min, max)"},
		{[]any{-1.0}, "zero or more"},
		{[]any{"1s"}, "a number of seconds"},
		{[]any{true}, "a number of seconds"},
		{[]any{2.0, 1.0}, "min 2 is more than max 1"},
		{[]any{1.0, -1.0}, "zero or more"},
	} {
		if _, err := SleepArgs(bad.args); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("SleepArgs(%v) = %v, want an error with %q", bad.args, err, bad.want)
		}
	}
}

func TestSleep_EndsEarlyWhenTheRunEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if Sleep(ctx, 10*time.Second) {
		t.Fatal("Sleep reported a full wait")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Sleep took %v after the run ended", took)
	}
	if !Sleep(context.Background(), 10*time.Millisecond) {
		t.Fatal("a short sleep did not finish")
	}
}

// cookieServer sets a cookie on /login and says on /me which cookie came back.
func cookieServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "abc", Path: "/"})
			return
		}
		c, err := r.Cookie("sid")
		if err != nil {
			_, _ = w.Write([]byte("none"))
			return
		}
		_, _ = w.Write([]byte(c.Value))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPClient_CookieJar(t *testing.T) {
	srv := cookieServer(t)
	ctx := context.Background()
	me := func(c *HTTPClient) string {
		resp, err := c.Do(ctx, "GET", srv.URL+"/me", nil, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return string(resp.Body)
	}

	c := NewHTTPClient(nil, time.Second)
	defer c.Close()
	if _, err := c.Do(ctx, "GET", srv.URL+"/login", nil, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := me(c); got != "abc" {
		t.Fatalf("the cookie was not sent back: %q", got)
	}
	c.ResetCookies()
	if got := me(c); got != "none" {
		t.Fatalf("the cookie survived ResetCookies: %q", got)
	}

	off := NewHTTPClient(nil, time.Second, NoCookies())
	defer off.Close()
	if _, err := off.Do(ctx, "GET", srv.URL+"/login", nil, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := me(off); got != "none" {
		t.Fatalf("NoCookies still sent the cookie: %q", got)
	}
	resp, err := off.Do(ctx, "GET", srv.URL+"/me", nil, Options{Headers: map[string]string{"Cookie": "sid=own"}})
	if err != nil || string(resp.Body) != "own" {
		t.Fatalf("a Cookie header the script set: %v %q", err, resp.Body)
	}
}
