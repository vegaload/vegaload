package netapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestHTTPClient_GetAndCarryValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "yes")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 42}) //nolint:errcheck
	}))
	defer srv.Close()

	c := NewHTTPClient(nil, 5*time.Second)
	resp, err := c.Do(context.Background(), http.MethodPost, srv.URL, []byte(`{"name":"x"}`), Options{})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("StatusCode = %d, want 201", resp.StatusCode)
	}
	if resp.Headers.Get("X-Custom") != "yes" {
		t.Errorf("missing X-Custom header in response")
	}
	var body struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("unmarshaling body: %v", err)
	}
	if body.ID != 42 {
		t.Errorf("ID = %d, want 42 -- this is the value a script needs to carry into its next call", body.ID)
	}
}

func TestHTTPClient_Headers(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewHTTPClient(nil, 5*time.Second)
	_, err := c.Do(context.Background(), http.MethodGet, srv.URL, nil, Options{
		Headers: map[string]string{"Authorization": "Bearer tok"},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer tok")
	}
}

func TestHTTPClient_SafetyCheckRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should never be reached when the safety check refuses the host")
	}))
	defer srv.Close()

	refuse := func(host string) error { return errors.New("host not allowed: " + host) }
	c := NewHTTPClient(refuse, 5*time.Second)
	_, err := c.Do(context.Background(), http.MethodGet, srv.URL, nil, Options{})
	if err == nil {
		t.Fatal("expected an error from the safety check")
	}
	if !strings.Contains(err.Error(), "host not allowed") {
		t.Errorf("error = %v, want it to mention the safety check's reason", err)
	}
}

func TestHTTPClient_SafetyCheckAllows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var checkedHost string
	allow := func(host string) error { checkedHost = host; return nil }
	c := NewHTTPClient(allow, 5*time.Second)
	resp, err := c.Do(context.Background(), http.MethodGet, srv.URL, nil, Options{})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if checkedHost == "" {
		t.Error("expected the safety check to have been called with a host")
	}
}

func TestHTTPClient_ConnectionRefusedIsAnError(t *testing.T) {
	c := NewHTTPClient(nil, 2*time.Second)
	_, err := c.Do(context.Background(), http.MethodGet, "http://127.0.0.1:1", nil, Options{})
	if err == nil {
		t.Fatal("expected an error for an unreachable target")
	}
}

// wsEchoServer upgrades every request to WebSocket and echoes back
// whatever it receives, closing normally when the client closes --
// exactly the shape examples/sample-app's /ws/echo endpoint has, and
// enough to exercise a multi-message exchange on one connection.
func wsEchoServer(t *testing.T) *httptest.Server {
	upgrader := websocket.Upgrader{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}))
}

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func TestWSConn_SendReceiveMultipleFrames(t *testing.T) {
	srv := wsEchoServer(t)
	defer srv.Close()

	conn, err := Dial(context.Background(), nil, wsURL(srv.URL), 5*time.Second, Options{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	for i, frame := range []string{"ignite", "status?", "abort"} {
		if err := conn.Send(context.Background(), []byte(frame), true); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
		data, closed, err := conn.Receive(context.Background())
		if err != nil {
			t.Fatalf("Receive %d: %v", i, err)
		}
		if closed {
			t.Fatalf("Receive %d: unexpected close", i)
		}
		if string(data) != frame {
			t.Errorf("Receive %d = %q, want echo of %q", i, data, frame)
		}
	}
}

func TestWSConn_ReceiveAfterClose(t *testing.T) {
	srv := wsEchoServer(t)
	defer srv.Close()

	conn, err := Dial(context.Background(), nil, wsURL(srv.URL), 5*time.Second, Options{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, _, err = conn.Receive(context.Background())
	if err == nil {
		t.Error("expected an error reading from a closed connection")
	}
}

func TestWSConn_ReceiveRespectsContextDeadline(t *testing.T) {
	srv := wsEchoServer(t)
	defer srv.Close()

	conn, err := Dial(context.Background(), nil, wsURL(srv.URL), 5*time.Second, Options{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, closed, err := conn.Receive(ctx)
	if err == nil {
		t.Fatal("expected a timeout error when nothing is sent")
	}
	if closed {
		t.Error("a deadline timeout should not be reported as closed")
	}
}

func TestDial_SafetyCheckRejects(t *testing.T) {
	srv := wsEchoServer(t)
	defer srv.Close()

	refuse := func(host string) error { return errors.New("host not allowed: " + host) }
	_, err := Dial(context.Background(), refuse, wsURL(srv.URL), 5*time.Second, Options{})
	if err == nil {
		t.Fatal("expected an error from the safety check")
	}
	if !strings.Contains(err.Error(), "host not allowed") {
		t.Errorf("error = %v, want it to mention the safety check's reason", err)
	}
}

func TestDial_BadURL(t *testing.T) {
	_, err := Dial(context.Background(), nil, "http://example.com", 5*time.Second, Options{})
	if err == nil {
		t.Fatal("expected an error dialing a non-ws(s) URL")
	}
}
