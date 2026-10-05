package js

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/vegaload/vegaload/internal/scripting/netapi"
)

// writeScript creates a temporary file containing src and returns its
// path. name picks the extension, which is what Load uses to decide
// whether to treat the file as TypeScript.
func writeScript(t *testing.T, name, src string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("writing test script: %v", err)
	}
	return path
}

func TestLoad_JS_DefaultExportRuns(t *testing.T) {
	path := writeScript(t, "scenario.js", `
		export default function () {
			// one iteration, nothing to do
		}
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
}

func TestLoad_TS_StripsTypesAndRuns(t *testing.T) {
	path := writeScript(t, "scenario.ts", `
		function add(a: number, b: number): number {
			return a + b;
		}
		export default function (): void {
			const sum: number = add(1, 2);
			if (sum !== 3) {
				throw new Error("arithmetic is broken: " + sum);
			}
		}
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
}

func TestIteration_PropagatesThrownError(t *testing.T) {
	path := writeScript(t, "scenario.js", `
		export default function () {
			throw new Error("boom");
		}
	`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	vu, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}

	err = vu.Iteration(context.Background())
	if err == nil {
		t.Fatal("expected Iteration to return an error for a thrown exception")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "boom")
	}
}

func TestNewVU_MissingDefaultExport(t *testing.T) {
	path := writeScript(t, "scenario.js", `
		function helper() {}
	`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if _, err := script.NewVU(nil, 5*time.Second); err == nil {
		t.Fatal("expected NewVU to return an error when there is no default export")
	}
}

func TestLoad_SyntaxError(t *testing.T) {
	path := writeScript(t, "scenario.js", `this is not valid javascript {{{`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected Load to return an error for invalid syntax")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.js")); err == nil {
		t.Fatal("expected Load to return an error for a missing file")
	}
}

func TestVU_IsolatedPerInstance(t *testing.T) {
	path := writeScript(t, "scenario.js", `
		let calls = 0;
		globalThis.calls = calls;
		export default function () {
			calls++;
			globalThis.calls = calls;
		}
	`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	vu1, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}
	vu2, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}

	if err := vu1.Iteration(context.Background()); err != nil {
		t.Fatalf("vu1 Iteration returned error: %v", err)
	}
	if err := vu1.Iteration(context.Background()); err != nil {
		t.Fatalf("vu1 Iteration returned error: %v", err)
	}
	if err := vu2.Iteration(context.Background()); err != nil {
		t.Fatalf("vu2 Iteration returned error: %v", err)
	}

	got1 := vu1.vm.Get("calls").ToInteger()
	got2 := vu2.vm.Get("calls").ToInteger()
	if got1 != 2 {
		t.Errorf("vu1's calls = %d, want 2", got1)
	}
	if got2 != 1 {
		t.Errorf("vu2's calls = %d, want 1 (vu1 and vu2 must not share state)", got2)
	}
}

func TestIteration_InterruptsOnContextCancellation(t *testing.T) {
	path := writeScript(t, "scenario.js", `
		export default function () {
			while (true) {
				// spin forever unless interrupted
			}
		}
	`)

	script, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	vu, err := script.NewVU(nil, 5*time.Second)
	if err != nil {
		t.Fatalf("NewVU returned error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- vu.Iteration(ctx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected Iteration to return an error when interrupted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Iteration did not return after context cancellation")
	}
}

// FR-CLI-08: real HTTP and WebSocket access from a scenario script.

func TestHTTPGlobal_GetAndCarryValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 42}) //nolint:errcheck
	}))
	defer srv.Close()

	path := writeScript(t, "scenario.js", `
		export default function () {
			const resp = http.post("`+srv.URL+`", { body: JSON.stringify({name: "x"}) });
			if (resp.status !== 201) {
				throw new Error("status = " + resp.status);
			}
			const data = resp.json();
			if (data.id !== 42) {
				throw new Error("id = " + data.id + ", want 42 -- the value a script must carry into its next call");
			}
		}
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
}

func TestHTTPGlobal_Headers(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	path := writeScript(t, "scenario.js", `
		export default function () {
			http.get("`+srv.URL+`", { headers: { "Authorization": "Bearer tok" } });
		}
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
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer tok")
	}
}

func TestHTTPGlobal_SafetyCheckRejectsAndIsCatchable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should never be reached when the safety check refuses the host")
	}))
	defer srv.Close()

	path := writeScript(t, "scenario.js", `
		export default function () {
			try {
				http.get("`+srv.URL+`");
				throw new Error("expected http.get to throw");
			} catch (e) {
				if (!String(e).includes("host not allowed")) {
					throw new Error("unexpected error: " + e);
				}
			}
		}
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
	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatalf("Iteration returned error: %v (script's own try/catch should have handled the thrown error)", err)
	}
}

func TestHTTPGlobal_SafetyCheckRejectionPropagatesWhenUncaught(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	path := writeScript(t, "scenario.js", `
		export default function () {
			http.get("`+srv.URL+`");
		}
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
	err = vu.Iteration(context.Background())
	if err == nil {
		t.Fatal("expected Iteration to return an error when the script doesn't catch the safety rejection")
	}
	if !strings.Contains(err.Error(), "host not allowed") {
		t.Errorf("error = %q, want it to mention the safety check's reason", err.Error())
	}
}

// wsEchoServer upgrades every request to WebSocket and echoes back
// whatever it receives, closing normally when the client closes.
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

func TestWSGlobal_SendReceiveLoopUntilClose(t *testing.T) {
	srv := wsEchoServer(t)
	defer srv.Close()

	path := writeScript(t, "scenario.js", `
		export default function () {
			const conn = ws.connect("`+wsURL(srv.URL)+`");
			const frames = ["ignite", "status?", "abort"];
			for (const frame of frames) {
				conn.send(frame);
				const got = conn.receive(2000);
				if (got !== frame) {
					throw new Error("got " + got + ", want echo of " + frame);
				}
			}
			conn.close();
		}
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
}

func TestWSGlobal_ReceiveReturnsNullOnClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// A graceful close handshake (as opposed to just dropping the
		// TCP connection) is what makes Receive report a clean close
		// rather than an abnormal-closure error.
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	}))
	defer srv.Close()

	path := writeScript(t, "scenario.js", `
		export default function () {
			const conn = ws.connect("`+wsURL(srv.URL)+`");
			const got = conn.receive(2000);
			if (got !== null) {
				throw new Error("expected null on close, got " + got);
			}
		}
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
}

func TestVU_CloseClosesOpenConnections(t *testing.T) {
	srv := wsEchoServer(t)
	defer srv.Close()

	path := writeScript(t, "scenario.js", `
		export default function () {
			ws.connect("`+wsURL(srv.URL)+`");
			// deliberately never closed -- VU.Close must clean it up
		}
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
	if len(vu.openConns) != 1 {
		t.Fatalf("len(openConns) = %d, want 1", len(vu.openConns))
	}
	if err := vu.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
}

// TestLaunchPadStyleFlow mirrors FR-CLI-08's own example: create a
// resource over HTTP, carry its ID into a WebSocket URL, then read
// frames until the flow reports completion.
func TestLaunchPadStyleFlow(t *testing.T) {
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

	path := writeScript(t, "scenario.js", `
		export default function () {
			const created = http.post("`+srv.URL+`/launches", {});
			const launch = created.json();
			const conn = ws.connect("`+wsURL(srv.URL)+`/launches/" + launch.id + "/ignite");
			conn.send(JSON.stringify({action: "ignite"}));
			let frameCount = 0;
			for (;;) {
				const frame = conn.receive(2000);
				if (frame === null) break;
				frameCount++;
				const msg = JSON.parse(frame);
				if (msg.status === "complete" || msg.status === "aborted") break;
			}
			conn.close();
			if (frameCount !== 3) {
				throw new Error("frameCount = " + frameCount + ", want 3");
			}
		}
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
}
