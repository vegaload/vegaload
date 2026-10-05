package python

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/vegaload/vegaload/internal/scripting/netapi"
)

func skipIfNoPython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(pythonBin); err != nil {
		t.Skipf("%s not on PATH, skipping", pythonBin)
	}
}

func writeScript(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "scenario.py")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("writing test script: %v", err)
	}
	return path
}

func TestLoad_And_NewVU_RunsIterations(t *testing.T) {
	skipIfNoPython(t)
	path := writeScript(t, `
def iteration():
    pass
`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	vu, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}
	defer vu.Close()

	for i := 0; i < 3; i++ {
		if err := vu.Iteration(context.Background()); err != nil {
			t.Fatalf("Iteration #%d returned error: %v", i, err)
		}
	}
}

func TestIteration_PropagatesPythonException(t *testing.T) {
	skipIfNoPython(t)
	path := writeScript(t, `
def iteration():
    raise ValueError("boom")
`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	vu, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}
	defer vu.Close()

	err = vu.Iteration(context.Background())
	if err == nil {
		t.Fatal("expected Iteration to return an error for a raised exception")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "boom")
	}

	// The interpreter survives one failed iteration and keeps serving
	// the protocol for the next one.
	if err := vu.Iteration(context.Background()); err == nil {
		t.Fatal("expected the second iteration to also raise")
	}
}

func TestIteration_RecoversAfterFailedIteration(t *testing.T) {
	skipIfNoPython(t)
	path := writeScript(t, `
calls = 0

def iteration():
    global calls
    calls += 1
    if calls == 1:
        raise ValueError("first call fails")
`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	vu, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}
	defer vu.Close()

	if err := vu.Iteration(context.Background()); err == nil {
		t.Fatal("expected the first iteration to fail")
	}
	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatalf("expected the second iteration to succeed, got: %v", err)
	}
}

func TestNewVU_MissingIterationFunction(t *testing.T) {
	skipIfNoPython(t)
	path := writeScript(t, `
def helper():
    pass
`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if _, err := script.NewVU(nil, 5*time.Second); err == nil {
		t.Fatal("expected NewVU to return an error when there is no iteration() function")
	}
}

func TestNewVU_ScriptFailsToImport(t *testing.T) {
	skipIfNoPython(t)
	path := writeScript(t, `this is not valid python {{{`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if _, err := script.NewVU(nil, 5*time.Second); err == nil {
		t.Fatal("expected NewVU to return an error when the script fails to import")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	skipIfNoPython(t)
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.py")); err == nil {
		t.Fatal("expected Load to return an error for a missing file")
	}
}

func TestLoad_InterpreterNotFound(t *testing.T) {
	path := writeScript(t, "def iteration():\n    pass\n")

	orig := pythonBin
	pythonBin = "vegaload-nonexistent-interpreter"
	defer func() { pythonBin = orig }()

	if _, err := Load(path); err == nil {
		t.Fatal("expected Load to return an error when the interpreter is not on PATH")
	}
}

func TestIteration_RespectsContextCancellation(t *testing.T) {
	skipIfNoPython(t)
	path := writeScript(t, `
import time

def iteration():
    time.sleep(2)
`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	vu, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}
	defer vu.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- vu.Iteration(ctx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected Iteration to return an error when its context is cancelled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Iteration did not return promptly after context cancellation")
	}
}

func TestVU_Close(t *testing.T) {
	skipIfNoPython(t)
	path := writeScript(t, "def iteration():\n    pass\n")

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	vu, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}

	if err := vu.Close(); err != nil {
		t.Errorf("Close returned error: %v", err)
	}
}

// FR-CLI-08: real HTTP and WebSocket access from a scenario script,
// proxied over the subprocess's stdin/stdout pipe.

func TestHTTPGlobal_GetAndCarryValue(t *testing.T) {
	skipIfNoPython(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 42}) //nolint:errcheck
	}))
	defer srv.Close()

	path := writeScript(t, `
import json

def iteration():
    resp = http.post("`+srv.URL+`", body=json.dumps({"name": "x"}))
    if resp.status != 201:
        raise ValueError("status = %d" % resp.status)
    data = resp.json()
    if data["id"] != 42:
        raise ValueError("id = %r, want 42 -- the value a script must carry into its next call" % data["id"])
`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	vu, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}
	defer vu.Close()

	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatalf("Iteration returned error: %v", err)
	}
}

func TestHTTPGlobal_Headers(t *testing.T) {
	skipIfNoPython(t)
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	path := writeScript(t, `
def iteration():
    http.get("`+srv.URL+`", headers={"Authorization": "Bearer tok"})
`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	vu, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}
	defer vu.Close()

	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatalf("Iteration returned error: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer tok")
	}
}

func TestHTTPGlobal_SafetyCheckRejectsAndIsCatchable(t *testing.T) {
	skipIfNoPython(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should never be reached when the safety check refuses the host")
	}))
	defer srv.Close()

	path := writeScript(t, `
def iteration():
    try:
        http.get("`+srv.URL+`")
        raise AssertionError("expected http.get to raise")
    except AssertionError:
        raise
    except Exception as e:
        if "host not allowed" not in str(e):
            raise ValueError("unexpected error: %s" % e)
`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	refuse := func(host string) error { return errors.New("host not allowed: " + host) }
	vu, err := script.NewVU(netapi.SafetyCheck(refuse), 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}
	defer vu.Close()

	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatalf("Iteration returned error: %v (script's own try/except should have handled the raised error)", err)
	}
}

func TestHTTPGlobal_SafetyCheckRejectionPropagatesWhenUncaught(t *testing.T) {
	skipIfNoPython(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	path := writeScript(t, `
def iteration():
    http.get("`+srv.URL+`")
`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	refuse := func(host string) error { return errors.New("host not allowed: " + host) }
	vu, err := script.NewVU(netapi.SafetyCheck(refuse), 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}
	defer vu.Close()

	err = vu.Iteration(context.Background())
	if err == nil {
		t.Fatal("expected Iteration to return an error when the script doesn't catch the safety rejection")
	}
	if !strings.Contains(err.Error(), "host not allowed") {
		t.Errorf("error = %q, want it to mention the safety check's reason", err.Error())
	}
}

// pyWsEchoServer upgrades every request to WebSocket and echoes back
// whatever it receives, closing normally when the client closes.
func pyWsEchoServer(t *testing.T) *httptest.Server {
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

func pyWsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func TestWSGlobal_SendReceiveLoopUntilClose(t *testing.T) {
	skipIfNoPython(t)
	srv := pyWsEchoServer(t)
	defer srv.Close()

	path := writeScript(t, `
def iteration():
    conn = ws.connect("`+pyWsURL(srv.URL)+`")
    for frame in ["ignite", "status?", "abort"]:
        conn.send(frame)
        got = conn.receive(timeout_ms=2000)
        if got != frame:
            raise ValueError("got %r, want echo of %r" % (got, frame))
    conn.close()
`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	vu, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}
	defer vu.Close()

	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatalf("Iteration returned error: %v", err)
	}
}

func TestWSGlobal_ReceiveReturnsNoneOnClose(t *testing.T) {
	skipIfNoPython(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	}))
	defer srv.Close()

	path := writeScript(t, `
def iteration():
    conn = ws.connect("`+pyWsURL(srv.URL)+`")
    got = conn.receive(timeout_ms=2000)
    if got is not None:
        raise ValueError("expected None on close, got %r" % got)
`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	vu, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}
	defer vu.Close()

	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatalf("Iteration returned error: %v", err)
	}
}

func TestVU_CloseClosesOpenConnections(t *testing.T) {
	skipIfNoPython(t)
	srv := pyWsEchoServer(t)
	defer srv.Close()

	path := writeScript(t, `
def iteration():
    ws.connect("`+pyWsURL(srv.URL)+`")
    # deliberately never closed -- VU.Close must clean it up
`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	vu, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}

	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatalf("Iteration returned error: %v", err)
	}
	if len(vu.conns) != 1 {
		t.Fatalf("len(conns) = %d, want 1", len(vu.conns))
	}
	if err := vu.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
}

// TestLaunchPadStyleFlow mirrors FR-CLI-08's own example: create a
// resource over HTTP, carry its ID into a WebSocket URL, then read
// frames until the flow reports completion.
func TestLaunchPadStyleFlow(t *testing.T) {
	skipIfNoPython(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/launches", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "launch-7"}) //nolint:errcheck
	})
	upgrader := websocket.Upgrader{}
	mux.HandleFunc("/launches/launch-7/ignite", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage() // the "ignite" send
		frames := []string{`{"status":"running"}`, `{"status":"running"}`, `{"status":"complete"}`}
		for _, f := range frames {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(f)); err != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	path := writeScript(t, `
import json

def iteration():
    created = http.post("`+srv.URL+`/launches")
    launch = created.json()
    conn = ws.connect("`+pyWsURL(srv.URL)+`/launches/" + launch["id"] + "/ignite")
    conn.send(json.dumps({"action": "ignite"}))
    frame_count = 0
    while True:
        frame = conn.receive(timeout_ms=2000)
        if frame is None:
            break
        frame_count += 1
        msg = json.loads(frame)
        if msg["status"] in ("complete", "aborted"):
            break
    conn.close()
    if frame_count != 3:
        raise ValueError("frame_count = %d, want 3" % frame_count)
`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	vu, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}
	defer vu.Close()

	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatalf("Iteration returned error: %v", err)
	}
}
