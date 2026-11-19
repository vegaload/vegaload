package http2

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/vegaload/vegaload/internal/protocol"
)

// newH2Server starts an httptest TLS server with HTTP/2 explicitly
// configured, so Driver has something genuinely speaking h2-over-TLS to
// talk to, not just https with the server left to decide.
func newH2Server(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	if err := http2.ConfigureServer(srv.Config, nil); err != nil {
		t.Fatalf("http2.ConfigureServer: %v", err)
	}
	srv.TLS = srv.Config.TLSConfig
	srv.StartTLS()
	return srv
}

// newH2CServer starts a plaintext (no TLS) server that speaks HTTP/2
// directly, using golang.org/x/net/http2/h2c's server-side helper — the
// counterpart to Driver's client-side h2c support for http:// targets.
func newH2CServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	h2s := &http2.Server{}
	srv := httptest.NewServer(h2c.NewHandler(handler, h2s))
	return srv
}

func TestDriver_Do_SuccessOverTLS(t *testing.T) {
	var gotProto string
	srv := newH2Server(t, func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Proto
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello"))
	})
	defer srv.Close()

	d, err := New(protocol.Target{URL: srv.URL, InsecureSkipVerify: true}, time.Second)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	defer d.Close()

	res, err := d.Do(context.Background())
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	if !res.Success {
		t.Errorf("expected Success true, got Result %+v", res)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if res.BytesReceived != 5 {
		t.Errorf("BytesReceived = %d, want 5", res.BytesReceived)
	}
	if gotProto != "HTTP/2.0" {
		t.Errorf("server saw proto %q, want HTTP/2.0 — the TLS connection did not actually negotiate HTTP/2", gotProto)
	}
}

func TestDriver_Do_H2CPlaintext(t *testing.T) {
	var gotProto string
	srv := newH2CServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Proto
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()

	d, err := New(protocol.Target{URL: srv.URL}, time.Second)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	defer d.Close()

	res, err := d.Do(context.Background())
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	if !res.Success {
		t.Errorf("expected Success true, got Result %+v", res)
	}
	if gotProto != "HTTP/2.0" {
		t.Errorf("server saw proto %q, want HTTP/2.0 over plaintext (h2c)", gotProto)
	}
	if res.TTFB <= 0 || res.TTFB > time.Second {
		t.Errorf("TTFB = %v, want a measured time", res.TTFB)
	}
}

func TestDriver_Do_FailureOnServerError(t *testing.T) {
	srv := newH2Server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer srv.Close()

	d, err := New(protocol.Target{URL: srv.URL, InsecureSkipVerify: true}, time.Second)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	defer d.Close()

	res, err := d.Do(context.Background())
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	if res.Success {
		t.Error("expected Success false for a 500 response")
	}
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want %d", res.StatusCode, http.StatusInternalServerError)
	}
}

func TestDriver_Do_ConnectionErrorIsNotAFatalError(t *testing.T) {
	d, err := New(protocol.Target{URL: "https://127.0.0.1:1"}, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	defer d.Close()

	res, doErr := d.Do(context.Background())
	if doErr != nil {
		t.Fatalf("Do returned a fatal error for a connection failure: %v", doErr)
	}
	if res.Success {
		t.Error("expected Success false when the connection fails")
	}
	if res.Err == nil {
		t.Error("expected Result.Err to describe the connection failure")
	}
}

func TestNew_FatalErrorOnMalformedURL(t *testing.T) {
	if _, err := New(protocol.Target{URL: "://not a url"}, time.Second); err == nil {
		t.Error("expected a fatal error for a malformed target URL")
	}
}

func TestDriver_Name(t *testing.T) {
	d, err := New(protocol.Target{URL: "http://example.invalid"}, time.Second)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	defer d.Close()

	if d.Name() != "http2" {
		t.Errorf("Name() = %q, want %q", d.Name(), "http2")
	}
}
