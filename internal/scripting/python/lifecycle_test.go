package python

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
	skipIfNoPython(t)
	script, err := Load(writeScript(t, src))
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
def iteration():
    sleep(0.05)
    sleep(0.02, 0.04)
    sleep(0)
`)
	start := time.Now()
	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 70*time.Millisecond || took > 3*time.Second {
		t.Fatalf("the iteration took %v, want about 70-90ms", took)
	}
}

func TestSleep_BadArgumentsAreCatchable(t *testing.T) {
	vu := newTestVU(t, `
def message(*args):
    try:
        sleep(*args)
    except Exception as e:
        return str(e)
    return "no error"

def iteration():
    got = [message(-1), message("1s"), message(2, 1), message(), message(True)]
    raise RuntimeError(" | ".join(got))
`)
	err := vu.Iteration(context.Background())
	for _, want := range []string{"zero or more seconds", "a number of seconds, like 1.5, not 1s", "min 2 is more than max 1", "want sleep(seconds) or sleep(min, max)", "not true"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q in %v", want, err)
		}
	}
}

func TestSleep_EndsWhenTheRunEnds(t *testing.T) {
	vu := newTestVU(t, `
def iteration():
    sleep(30)
`)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := vu.Iteration(ctx); err == nil {
		t.Fatal("a cut-off sleep returned no error")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the sleep held the VU for %v after the run ended", took)
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
def iteration():
    before = http.get("` + srv.URL + `/me").body
    assert before == "none", "started with cookie " + before
    http.get("` + srv.URL + `/login")
    after = http.get("` + srv.URL + `/me").body
    assert after == WANT, "after login: " + after
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
def setup():
    return {"token": "t-1", "ids": [1, 2]}

def iteration(data):
    assert data["token"] == "t-1" and data["ids"][1] == 2, repr(data)
    data["ids"].append(3)  # this VU's own copy

def teardown(data):
    raise RuntimeError("teardown got " + json.dumps(data, separators=(",", ":")))

import json
`)
	if !vu.HasSetup() || !vu.HasTeardown() {
		t.Fatal("setup and teardown were not found")
	}
	data, err := vu.Setup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil || got["token"] != "t-1" {
		t.Fatalf("setup data %s %v", data, err)
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

func TestIterationWithoutAParameterStillWorks(t *testing.T) {
	vu := newTestVU(t, `
def setup():
    return 1

def iteration():
    pass
`)
	data, err := vu.Setup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := vu.SetData(data); err != nil {
		t.Fatal(err)
	}
	if err := vu.Iteration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if vu.HasTeardown() {
		t.Fatal("found a teardown that is not there")
	}
}

func TestSetup_ValueMustBeJSON(t *testing.T) {
	vu := newTestVU(t, `
def setup():
    return {"when": object()}

def iteration():
    pass
`)
	if _, err := vu.Setup(context.Background()); err == nil || !strings.Contains(err.Error(), "setup must return a value JSON can hold") {
		t.Fatalf("got %v", err)
	}
}

func TestOptions(t *testing.T) {
	vu := newTestVU(t, `
options = {"vus": 3, "duration": "2s", "max_vus": 4, "thresholds": ["p95 < 300ms"]}

def iteration():
    pass
`)
	got, err := vu.Options()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if string(b) != `{"duration":"2s","max_vus":4,"thresholds":["p95 \u003c 300ms"],"vus":3}` {
		t.Fatalf("options %s", b)
	}

	skipIfNoPython(t)
	script, err := Load(writeScript(t, "options = [1]\ndef iteration():\n    pass\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := script.NewVU(nil, time.Second); err == nil || !strings.Contains(err.Error(), "options must be a dict") {
		t.Fatalf("a list as options: %v", err)
	}
}

func TestPrint_GoesToTheScreenWithoutSecrets(t *testing.T) {
	secrets.Reset()
	t.Cleanup(secrets.Reset)
	secrets.Register("hunter2-secret")
	var vu *VU
	loaded := captureStdout(t, func() {
		vu = newTestVU(t, `
import sys
print("loading")

def iteration():
    print("token is hunter2-secret")
    sys.stdout.write("no newline")
    raise RuntimeError("failed with hunter2-secret")
`)
	})
	if !strings.Contains(loaded, "loading") {
		t.Fatalf("a print while loading wrote %q", loaded)
	}
	var err error
	out := captureStdout(t, func() { err = vu.Iteration(context.Background()) })
	if strings.Contains(out, "hunter2-secret") || !strings.Contains(out, "token is") || !strings.Contains(out, "no newline") {
		t.Fatalf("print wrote %q", out)
	}
	if err == nil || !strings.Contains(err.Error(), "RuntimeError") {
		t.Fatalf("the iteration error was lost: %v", err)
	}
	if got := secrets.Redact(err.Error()); strings.Contains(got, "hunter2-secret") {
		t.Fatalf("the error keeps the secret after Redact: %q", got)
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
