package js

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/secrets"
)

func newTestVU(t *testing.T, src string, opts ...Option) *VU {
	t.Helper()
	script, err := Load(writeScript(t, "scenario.js", src))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	vu, err := script.NewVU(nil, 5*time.Second, opts...)
	if err != nil {
		t.Fatalf("NewVU: %v", err)
	}
	t.Cleanup(func() { _ = vu.Close() })
	return vu
}

func TestSleep_OneAndTwoArguments(t *testing.T) {
	vu := newTestVU(t, `
		export default function () {
			sleep(0.05);
			sleep(0.02, 0.04);
			sleep(0);
		}
	`)
	start := time.Now()
	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 70*time.Millisecond || took > 2*time.Second {
		t.Fatalf("the iteration took %v, want about 70-90ms", took)
	}
}

func TestSleep_BadArgumentsAreCatchable(t *testing.T) {
	vu := newTestVU(t, `
		function message(fn) {
			try { fn(); } catch (e) { return String(e); }
			return "no error";
		}
		export default function () {
			const got = [
				message(() => sleep(-1)),
				message(() => sleep("1s")),
				message(() => sleep(2, 1)),
				message(() => sleep()),
			];
			throw new Error(got.join(" | "));
		}
	`)
	err := vu.Iteration(context.Background())
	for _, want := range []string{"zero or more seconds", "a number of seconds", "min 2 is more than max 1", "want sleep(seconds) or sleep(min, max)"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q in %v", want, err)
		}
	}
}

func TestSleep_EndsWhenTheRunEnds(t *testing.T) {
	vu := newTestVU(t, `
		export default function () {
			try { sleep(30); } catch (e) {}
			throw new Error("the script went on after the run ended");
		}
	`)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := vu.Iteration(ctx)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the sleep held the VU for %v after the run ended", took)
	}
	if err == nil || strings.Contains(err.Error(), "went on") {
		t.Fatalf("want the run's end, got %v", err)
	}
}

func TestSleep_NotAtTheTopLevel(t *testing.T) {
	script, err := Load(writeScript(t, "scenario.js", `sleep(1); export default function () {}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := script.NewVU(nil, time.Second); err == nil || !strings.Contains(err.Error(), "only inside") {
		t.Fatalf("got %v", err)
	}
}

func cookieTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "s1", Path: "/"})
			return
		}
		if c, err := r.Cookie("sid"); err == nil {
			_, _ = w.Write([]byte(c.Value))
			return
		}
		_, _ = w.Write([]byte("none"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCookies_SentBackAndClearedEachIteration(t *testing.T) {
	srv := cookieTestServer(t)
	src := `
		let n = 0;
		export default function () {
			n++;
			const before = http.get("` + srv.URL + `/me").body;
			if (before !== "none") throw new Error("iteration " + n + " started with cookie " + before);
			http.get("` + srv.URL + `/login");
			const after = http.get("` + srv.URL + `/me").body;
			if (after !== WANT) throw new Error("after login: " + after);
		}
	`
	vu := newTestVU(t, strings.Replace(src, "WANT", `"s1"`, 1))
	for i := 0; i < 2; i++ {
		if err := vu.Iteration(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	off := newTestVU(t, strings.Replace(src, "WANT", `"none"`, 1), WithNoCookies())
	if err := off.Iteration(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSetupTeardownAndData(t *testing.T) {
	vu := newTestVU(t, `
		export function setup() { return { token: "t-1", ids: [1, 2] }; }
		export default function (data) {
			if (data.token !== "t-1" || data.ids[1] !== 2) throw new Error("data: " + JSON.stringify(data));
			data.ids.push(3); // this VU's own copy
		}
		export function teardown(data) {
			throw new Error("teardown got " + JSON.stringify(data));
		}
	`)
	if !vu.HasSetup() || !vu.HasTeardown() {
		t.Fatal("setup and teardown were not found")
	}
	data, err := vu.Setup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"token":"t-1","ids":[1,2]}` {
		t.Fatalf("setup data %s", data)
	}
	if err := vu.SetData(data); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := vu.Iteration(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	err = vu.Teardown(context.Background(), data)
	if err == nil || !strings.Contains(err.Error(), `teardown got {"token":"t-1","ids":[1,2]}`) {
		t.Fatalf("teardown: %v", err)
	}
}

func TestSetup_ValueMustBeJSON(t *testing.T) {
	vu := newTestVU(t, `
		export function setup() { const a = {}; a.self = a; return a; }
		export default function () {}
	`)
	if _, err := vu.Setup(context.Background()); err == nil || !strings.Contains(err.Error(), "setup must return a value JSON can hold") {
		t.Fatalf("got %v", err)
	}
}

func TestNoSetup_DefaultGetsUndefined(t *testing.T) {
	vu := newTestVU(t, `export default function (data) { if (data !== undefined) throw new Error("data"); }`)
	if vu.HasSetup() || vu.HasTeardown() {
		t.Fatal("found a setup that is not there")
	}
	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOptions(t *testing.T) {
	vu := newTestVU(t, `
		export const options = {
			vus: 3, duration: "2s", stages: [{ target: 5, duration: "1s" }],
			thresholds: ["p95 < 300ms"],
		};
		export default function () {}
	`)
	got, err := vu.Options()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if string(b) != `{"duration":"2s","stages":[{"duration":"1s","target":5}],"thresholds":["p95 \u003c 300ms"],"vus":3}` {
		t.Fatalf("options %s", b)
	}

	none := newTestVU(t, `export default function () {}`)
	if got, err := none.Options(); got != nil || err != nil {
		t.Fatalf("no options: %v %v", got, err)
	}
	bad := newTestVU(t, `export const options = [1]; export default function () {}`)
	if _, err := bad.Options(); err == nil || !strings.Contains(err.Error(), "options must be an object") {
		t.Fatalf("array options: %v", err)
	}
}

func TestConsoleLog_RemovesSecrets(t *testing.T) {
	secrets.Reset()
	t.Cleanup(secrets.Reset)
	secrets.Register("hunter2-secret")
	vu := newTestVU(t, `export default function () { console.log("token is hunter2-secret"); }`)
	out := captureStdout(t, func() {
		if err := vu.Iteration(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if strings.Contains(out, "hunter2-secret") || !strings.Contains(out, "token is") {
		t.Fatalf("console.log printed %q", out)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()
	fn()
	os.Stdout = old
	_ = w.Close()
	return <-done
}
