package http1

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/protocol"
)

func mustNew(t *testing.T, tg protocol.Target, timeout time.Duration) *Driver {
	t.Helper()
	d, err := New(tg, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDriver_Do_SuccessOn2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello"))
	}))
	defer srv.Close()

	d := mustNew(t, protocol.Target{URL: srv.URL}, time.Second)
	defer d.Close()

	res, err := d.Do(context.Background())
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	if !res.Success {
		t.Error("expected Success true for a 200 response")
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if res.BytesReceived != 5 {
		t.Errorf("BytesReceived = %d, want 5", res.BytesReceived)
	}
}

func TestDriver_Do_FailureOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := mustNew(t, protocol.Target{URL: srv.URL}, time.Second)
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
	// Nothing is listening on this target: the request should fail at
	// the transport level, which Do reports as a failed Result, not a
	// returned error — only a config-level problem (e.g. a malformed
	// URL) should stop the whole run.
	d := mustNew(t, protocol.Target{URL: "http://127.0.0.1:1"}, 200*time.Millisecond)
	defer d.Close()

	res, err := d.Do(context.Background())
	if err != nil {
		t.Fatalf("Do returned a fatal error for a connection failure: %v", err)
	}
	if res.Success {
		t.Error("expected Success false when the connection fails")
	}
	if res.Err == nil {
		t.Error("expected Result.Err to describe the connection failure")
	}
}

func TestDriver_Do_FatalErrorOnMalformedTarget(t *testing.T) {
	d := mustNew(t, protocol.Target{URL: "not a url", Method: "BAD METHOD"}, time.Second)
	defer d.Close()

	if _, err := d.Do(context.Background()); err == nil {
		t.Error("expected a fatal error for a malformed method/URL")
	}
}

func TestDriver_Do_SendsMethodHeadersAndBody(t *testing.T) {
	var gotMethod, gotHeader string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotHeader = r.Header.Get("X-VegaLoad")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	d := mustNew(t, protocol.Target{
		URL:     srv.URL,
		Method:  http.MethodPost,
		Headers: map[string]string{"X-VegaLoad": "test"},
		Body:    []byte(`{"n":1}`),
	}, time.Second)
	defer d.Close()

	res, err := d.Do(context.Background())
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	if !res.Success || res.StatusCode != http.StatusCreated {
		t.Errorf("unexpected result: %+v", res)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("server saw method %q, want POST", gotMethod)
	}
	if gotHeader != "test" {
		t.Errorf("server saw X-VegaLoad header %q, want %q", gotHeader, "test")
	}
	if string(gotBody) != `{"n":1}` {
		t.Errorf("server saw body %q, want %q", gotBody, `{"n":1}`)
	}
	if res.BytesSent != int64(len(`{"n":1}`)) {
		t.Errorf("BytesSent = %d, want %d", res.BytesSent, len(`{"n":1}`))
	}
}

func TestDriver_StaysOnHTTP1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := mustNew(t, protocol.Target{URL: srv.URL}, time.Second)
	defer d.Close()

	// TLSNextProto being a non-nil, empty map is what stops the
	// transport from ever upgrading to HTTP/2 over TLS, which is the
	// whole point of this driver existing separately from http2. This
	// httptest server is plain HTTP so the response is already
	// HTTP/1.1 regardless; this assertion checks the configuration
	// directly rather than relying on that incidental fact.
	if d.transport.TLSNextProto == nil {
		t.Fatal("transport.TLSNextProto is nil, want a non-nil empty map to force HTTP/1.1")
	}
	if len(d.transport.TLSNextProto) != 0 {
		t.Errorf("transport.TLSNextProto has %d entries, want 0", len(d.transport.TLSNextProto))
	}

	if d.Name() != "http1" {
		t.Errorf("Name() = %q, want %q", d.Name(), "http1")
	}
}

func TestDriver_Do_MeasuresTTFB(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.Write([]byte("hello"))
	}))
	defer srv.Close()

	d := mustNew(t, protocol.Target{URL: srv.URL}, time.Second)
	res, err := d.Do(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.TTFB < 20*time.Millisecond || res.TTFB > time.Second {
		t.Fatalf("TTFB = %v, want about 20ms", res.TTFB)
	}
}
