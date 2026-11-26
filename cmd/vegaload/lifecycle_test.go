package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/report"
	"github.com/vegaload/vegaload/internal/secrets"
)

// lifeServer counts the calls of setup (/setup), teardown (/teardown) and
// the iterations (/it), and keeps the token each one carried.
type lifeServer struct {
	*httptest.Server
	mu     sync.Mutex
	counts map[string]int
	tokens map[string]map[string]bool
}

func newLifeServer(t *testing.T) *lifeServer {
	t.Helper()
	ls := &lifeServer{counts: map[string]int{}, tokens: map[string]map[string]bool{}}
	ls.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.mu.Lock()
		defer ls.mu.Unlock()
		ls.counts[r.URL.Path]++
		if ls.tokens[r.URL.Path] == nil {
			ls.tokens[r.URL.Path] = map[string]bool{}
		}
		ls.tokens[r.URL.Path][r.URL.Query().Get("token")] = true
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(ls.Close)
	return ls
}

func (ls *lifeServer) count(path string) int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.counts[path]
}

func (ls *lifeServer) onlyToken(path string) string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if len(ls.tokens[path]) != 1 {
		return ""
	}
	for tok := range ls.tokens[path] {
		return tok
	}
	return ""
}

func lifeScenarioJS(base, iterPath string) string {
	return `
export function setup() {
  http.get("` + base + `/setup");
  return { token: "tok-" + Math.floor(Math.random() * 1e9) };
}
export default function (data) {
  http.get("` + base + iterPath + `?token=" + data.token);
}
export function teardown(data) {
  http.get("` + base + `/teardown?token=" + data.token);
}
`
}

func lifeScenarioPy(base, iterPath string) string {
	return `
import random

def setup():
    http.get("` + base + `/setup")
    return {"token": "tok-%d" % random.randint(0, 10**9)}

def iteration(data):
    http.get("` + base + iterPath + `?token=" + data["token"])

def teardown(data):
    http.get("` + base + `/teardown?token=" + data["token"])
`
}

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func skipWithoutPython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
}

func runParsed(t *testing.T, args ...string) (*report.Result, error) {
	t.Helper()
	cfg, err := parseRunArgs(append([]string{"-no-live", "-no-report"}, args...))
	if err != nil {
		t.Fatalf("parseRunArgs: %v", err)
	}
	return runScenario(cfg)
}

func checkLifecycle(t *testing.T, ls *lifeServer, res *report.Result) {
	t.Helper()
	if n := ls.count("/setup"); n != 1 {
		t.Errorf("setup ran %d times, want 1", n)
	}
	if n := ls.count("/teardown"); n != 1 {
		t.Errorf("teardown ran %d times, want 1", n)
	}
	tok := ls.onlyToken("/it")
	if tok == "" || ls.onlyToken("/teardown") != tok {
		t.Errorf("the setup data did not reach every iteration and teardown: %v", ls.tokens)
	}
	if res == nil {
		return
	}
	// setup and teardown are not part of the load.
	for _, row := range report.Endpoints(res) {
		if strings.Contains(row.Name, "setup") || strings.Contains(row.Name, "teardown") {
			t.Errorf("row %q is from setup or teardown", row.Name)
		}
	}
	if res.Total != int64(ls.count("/it")) {
		t.Errorf("total %d, but the iterations made %d calls", res.Total, ls.count("/it"))
	}
}

func TestRun_SetupOnceDataEverywhereTeardownOnce(t *testing.T) {
	for _, lang := range []string{"js", "py"} {
		t.Run(lang, func(t *testing.T) {
			ls := newLifeServer(t)
			var path string
			if lang == "py" {
				skipWithoutPython(t)
				path = writeTemp(t, "life.py", lifeScenarioPy(ls.URL, "/it"))
			} else {
				path = writeTemp(t, "life.vl.js", lifeScenarioJS(ls.URL, "/it"))
			}
			res, err := runParsed(t, "-executor", "constant-arrival-rate", "-rate", "40", "-max-vus", "4", "-duration", "700ms", path)
			if err != nil {
				t.Fatal(err)
			}
			checkLifecycle(t, ls, res)
		})
	}
}

func TestRun_TeardownAfterAbortOnBreach(t *testing.T) {
	for _, lang := range []string{"js", "py"} {
		t.Run(lang, func(t *testing.T) {
			ls := newLifeServer(t)
			var path string
			if lang == "py" {
				skipWithoutPython(t)
				path = writeTemp(t, "life.py", lifeScenarioPy(ls.URL, "/fail"))
			} else {
				path = writeTemp(t, "life.vl.js", lifeScenarioJS(ls.URL, "/fail"))
			}
			start := time.Now()
			res, err := runParsed(t, "-executor", "constant-arrival-rate", "-rate", "20", "-max-vus", "2", "-duration", "20s",
				"-abort-on-breach", "-threshold", "failed < 1", path)
			if err != nil {
				t.Fatal(err)
			}
			if res.Aborted == nil {
				t.Fatal("the run was not stopped by the threshold")
			}
			if took := time.Since(start); took > 10*time.Second {
				t.Fatalf("the run took %v", took)
			}
			if n := ls.count("/setup"); n != 1 {
				t.Errorf("setup ran %d times", n)
			}
			if n := ls.count("/teardown"); n != 1 {
				t.Errorf("teardown ran %d times after -abort-on-breach, want 1", n)
			}
		})
	}
}

func TestRun_TeardownAfterTheRunIsStopped(t *testing.T) {
	for _, lang := range []string{"js", "py"} {
		t.Run(lang, func(t *testing.T) {
			ls := newLifeServer(t)
			var path string
			if lang == "py" {
				skipWithoutPython(t)
				path = writeTemp(t, "life.py", lifeScenarioPy(ls.URL, "/it"))
			} else {
				path = writeTemp(t, "life.vl.js", lifeScenarioJS(ls.URL, "/it"))
			}
			cfg, err := parseRunArgs([]string{"-no-live", "-no-report", "-executor", "constant-arrival-rate", "-rate", "20", "-max-vus", "2", "-duration", "30s", path})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cfg.runCtx = ctx
			time.AfterFunc(700*time.Millisecond, cancel)
			start := time.Now()
			_, err = runScenario(cfg)
			if err != errInterrupted {
				t.Fatalf("got %v, want errInterrupted", err)
			}
			if took := time.Since(start); took > 10*time.Second {
				t.Fatalf("the stopped run took %v", took)
			}
			checkLifecycle(t, ls, nil)
		})
	}
}

func TestRun_FailingSetupStopsTheRun(t *testing.T) {
	ls := newLifeServer(t)
	path := writeTemp(t, "bad.vl.js", `
export function setup() { throw new Error("no login"); }
export default function () { http.get("`+ls.URL+`/it"); }
export function teardown() { http.get("`+ls.URL+`/teardown"); }
`)
	dir := t.TempDir()
	code, _, stderr := captureCmd(t, func() int {
		return cmdRun([]string{"-no-live", "-no-report", "-audit-log", filepath.Join(dir, "a.log"), "-duration", "1s", path})
	})
	if code != 1 || !strings.Contains(stderr, "setup: js: Error: no login") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if ls.count("/it") != 0 || ls.count("/teardown") != 0 {
		t.Fatalf("the load or teardown ran after setup failed: %v", ls.counts)
	}
}

func TestRun_FailingTeardownIsAWarning(t *testing.T) {
	ls := newLifeServer(t)
	path := writeTemp(t, "td.vl.js", `
export default function () { http.get("`+ls.URL+`/it"); sleep(0.05); }
export function teardown() { throw new Error("cleanup failed"); }
`)
	res, err := runParsed(t, "-duration", "300ms", path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "teardown: js: Error: cleanup failed") {
		t.Fatalf("warnings %v", res.Warnings)
	}
}

func TestRun_ClockStartsAfterSetup(t *testing.T) {
	ls := newLifeServer(t)
	path := writeTemp(t, "slow.vl.js", `
export function setup() { sleep(1); return 1; }
export default function () { http.get("`+ls.URL+`/it"); sleep(0.05); }
`)
	res, err := runParsed(t, "-duration", "500ms", path)
	if err != nil {
		t.Fatal(err)
	}
	if res.Elapsed > 900*time.Millisecond {
		t.Fatalf("elapsed %v includes the 1s setup", res.Elapsed)
	}
}

func TestRun_SleepIsNotInCallLatency(t *testing.T) {
	ls := newLifeServer(t)
	path := writeTemp(t, "sleepy.vl.js", `
export default function () {
  group("page", () => {
    http.get("`+ls.URL+`/it");
    sleep(0.2);
  });
}
`)
	res, err := runParsed(t, "-duration", "1s", path)
	if err != nil {
		t.Fatal(err)
	}
	if res.Latency.P95 > 100*time.Millisecond {
		t.Errorf("call p95 %v holds the sleep", res.Latency.P95)
	}
	if res.Iterations == nil || res.Iterations.Duration.Mean < 200*time.Millisecond {
		t.Errorf("iterations %+v do not hold the sleep", res.Iterations)
	}
	if len(res.Steps) != 1 || res.Steps[0].Latency.Mean < 200*time.Millisecond {
		t.Errorf("the group %+v does not hold the sleep", res.Steps)
	}
}

func TestParseRunArgs_ScenarioOptions(t *testing.T) {
	js := writeTemp(t, "opts.vl.js", `
export const options = {
  executor: "ramp", vus: 3, duration: "2s", maxVUs: 7, rate: 5, timeout: "4s",
  stages: [{ target: 5, duration: "1s" }, { target: 0, duration: "500ms" }],
  thresholds: ["p95 < 300ms", "error_rate < 1%"],
};
export default function () {}
`)
	cfg, err := parseRunArgs([]string{js})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Executor != "ramp" || cfg.VUs != 3 || cfg.Duration != 2*time.Second || cfg.MaxVUs != 7 || cfg.Rate != 5 ||
		cfg.Timeout != 4*time.Second || len(cfg.Stages) != 2 || cfg.Stages[0].Target != 5 || len(cfg.Thresholds) != 2 {
		t.Fatalf("options not applied: %+v", cfg)
	}

	cfg, err = parseRunArgs([]string{"-vus", "9", "-executor", "fixed-vus", "-threshold", "p99 < 1s", "-stages", "2:1s", js})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VUs != 9 || cfg.Executor != "fixed-vus" || len(cfg.Thresholds) != 1 || cfg.Thresholds[0].Name != "p99 < 1s" ||
		len(cfg.Stages) != 1 || cfg.Duration != 2*time.Second {
		t.Fatalf("flags did not win: %+v", cfg)
	}

	str := writeTemp(t, "str.vl.js", `export const options = { stages: "10:30s,0:5s" }; export default function () {}`)
	if cfg, err = parseRunArgs([]string{str}); err != nil || len(cfg.Stages) != 2 {
		t.Fatalf("stages as text: %v %+v", err, cfg)
	}

	for src, want := range map[string]string{
		`export const options = { vu: 3 };`:                       `options: unknown key "vu" (allowed: executor, vus, duration, stages, rate, maxVUs, timeout, thresholds)`,
		`export const options = { vus: 2.5 };`:                    "options.vus: want a whole number",
		`export const options = { duration: 30 };`:                `options.duration: want a duration, like "30s"`,
		`export const options = { thresholds: ["p95 <"] };`:       "options.thresholds:",
		`export const options = { stages: [{ target: 1 }] };`:     "stage 1: duration",
		`export const options = { stages: [{ to: 1 }] };`:         `stage 1: unknown key "to"`,
		`export const options = "fast";`:                          "options must be an object",
		`export const options = { abortOnBreach: true, vus: 1 };`: `unknown key "abortOnBreach"`,
	} {
		p := writeTemp(t, "bad.vl.js", src+" export default function () {}")
		if _, err := parseRunArgs([]string{p}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", src, err, want)
		}
	}

	abort := writeTemp(t, "abort.vl.js", `export const options = { thresholds: ["failed < 1"] }; export default function () {}`)
	if _, err := parseRunArgs([]string{"-abort-on-breach", abort}); err != nil {
		t.Fatalf("thresholds from options should satisfy -abort-on-breach: %v", err)
	}
	// A script that cannot load is reported by the run, not here.
	broken := writeTemp(t, "broken.vl.js", "this is not ( valid")
	if _, err := parseRunArgs([]string{broken}); err != nil {
		t.Fatalf("a broken script failed at parse time: %v", err)
	}
}

func TestParseRunArgs_PythonOptions(t *testing.T) {
	skipWithoutPython(t)
	py := writeTemp(t, "opts.py", `
options = {"vus": 4, "duration": "3s", "max_vus": 6, "thresholds": ["p95 < 1s"]}

def iteration():
    pass
`)
	cfg, err := parseRunArgs([]string{"-duration", "1s", py})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VUs != 4 || cfg.Duration != time.Second || cfg.MaxVUs != 6 || len(cfg.Thresholds) != 1 {
		t.Fatalf("%+v", cfg)
	}
	bad := writeTemp(t, "bad.py", "options = {\"vu\": 1}\ndef iteration():\n    pass\n")
	if _, err := parseRunArgs([]string{bad}); err == nil || !strings.Contains(err.Error(), `unknown key "vu"`) {
		t.Fatalf("got %v", err)
	}
}

func TestParseRunArgs_NoCookies(t *testing.T) {
	if _, err := parseRunArgs([]string{"-no-cookies", "-target", "http://127.0.0.1:1", "-protocol", "http1"}); err == nil || !strings.Contains(err.Error(), "-no-cookies is for a scenario") {
		t.Fatalf("got %v", err)
	}
	p := writeTemp(t, "c.vl.js", "export default function () {}")
	cfg, err := parseRunArgs([]string{"-no-cookies", p})
	if err != nil || !cfg.NoCookies {
		t.Fatalf("%v %+v", err, cfg)
	}
}

func TestRun_NoCookiesFlag(t *testing.T) {
	var mu sync.Mutex
	got := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "s1", Path: "/"})
			return
		}
		c, _ := r.Cookie("sid")
		mu.Lock()
		if c != nil {
			got[c.Value]++
		} else {
			got["none"]++
		}
		mu.Unlock()
	}))
	defer srv.Close()
	scenarios := []string{writeTemp(t, "c.vl.js", `export default function () { http.get("`+srv.URL+`/login"); http.get("`+srv.URL+`/me"); sleep(0.05); }`)}
	if _, err := exec.LookPath("python3"); err == nil {
		scenarios = append(scenarios, writeTemp(t, "c.py", "def iteration():\n    http.get(\""+srv.URL+"/login\")\n    http.get(\""+srv.URL+"/me\")\n    sleep(0.05)\n"))
	}
	for _, path := range scenarios {
		mu.Lock()
		got = map[string]int{}
		mu.Unlock()
		if _, err := runParsed(t, "-duration", "300ms", path); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		if got["s1"] == 0 || got["none"] != 0 {
			t.Errorf("%s with the jar: %v", filepath.Base(path), got)
		}
		got = map[string]int{}
		mu.Unlock()
		if _, err := runParsed(t, "-no-cookies", "-duration", "300ms", path); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		if got["s1"] != 0 || got["none"] == 0 {
			t.Errorf("%s with -no-cookies: %v", filepath.Base(path), got)
		}
		mu.Unlock()
	}
}

func TestRunSignals_FirstStopsSecondExits(t *testing.T) {
	ch := make(chan os.Signal, 2)
	exited := make(chan int, 1)
	var stderr strings.Builder
	rs := newRunSignals(ch, nil, &stderr, func(code int) { exited <- code })
	defer rs.stop()
	if rs.received() != nil {
		t.Fatal("a signal before any was sent")
	}
	ch <- syscall.SIGTERM
	select {
	case <-rs.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the first signal did not stop the run")
	}
	if rs.received() != syscall.SIGTERM || signalExitCode(rs.received()) != 143 {
		t.Fatalf("received %v", rs.received())
	}
	select {
	case code := <-exited:
		t.Fatalf("exited with %d on the first signal", code)
	default:
	}
	ch <- os.Interrupt
	select {
	case code := <-exited:
		if code != 130 {
			t.Fatalf("exit %d, want 130", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second signal did not exit")
	}
	if !strings.Contains(stderr.String(), "again to quit") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestRun_SecretsStayOutOfNamesAndOutput(t *testing.T) {
	secrets.Reset()
	t.Cleanup(secrets.Reset)
	t.Setenv("VL_TOKEN", "sekret-value-42")
	ls := newLifeServer(t)
	js := writeTemp(t, "s.vl.js", `
export default function () {
  console.log("js says " + env.VL_TOKEN);
  group("g-" + env.VL_TOKEN, () => {
    http.get("`+ls.URL+`/it", { name: "/it/" + env.VL_TOKEN });
    check(1, { ["c-" + env.VL_TOKEN]: () => true });
  });
  sleep(0.05);
}
`)
	scenarios := []string{js}
	if _, err := exec.LookPath("python3"); err == nil {
		scenarios = append(scenarios, writeTemp(t, "s.py", `
def iteration():
    print("py says " + env.VL_TOKEN)
    with group("g-" + env.VL_TOKEN):
        http.get("`+ls.URL+`/it", name="/it/" + env.VL_TOKEN)
    sleep(0.05)
`))
	}
	for _, p := range scenarios {
		dir := t.TempDir()
		out := filepath.Join(dir, "r.json")
		code, stdout, stderr := captureCmd(t, func() int {
			return cmdRun([]string{"-no-live", "-no-report", "-secret-env", "VL_TOKEN", "-audit-log", filepath.Join(dir, "a.log"), "-duration", "300ms", "-out", out, p})
		})
		raw, _ := os.ReadFile(out)
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", p, code, stderr)
		}
		for what, text := range map[string]string{"stdout": stdout, "stderr": stderr, "json": string(raw)} {
			if strings.Contains(text, "sekret-value-42") {
				t.Errorf("%s: the secret is in %s:\n%s", filepath.Base(p), what, text)
			}
		}
		if !strings.Contains(stdout, " says ") {
			t.Errorf("%s: the printed line is missing:\n%s", filepath.Base(p), stdout)
		}
	}

	// A Python exception that holds the secret.
	if _, err := exec.LookPath("python3"); err == nil {
		bad := writeTemp(t, "bad.py", "def iteration():\n    raise RuntimeError('token ' + env.VL_TOKEN)\n")
		code, stdout, stderr := validate(t, "-secret-env", "VL_TOKEN", bad)
		if code != 1 || strings.Contains(stdout+stderr, "sekret-value-42") || !strings.Contains(stdout+stderr, "RuntimeError") {
			t.Errorf("validate exit %d:\n%s\n%s", code, stdout, stderr)
		}
	}
}

func TestValidate_RunsSetupIterationTeardown(t *testing.T) {
	ls := newLifeServer(t)
	path := writeTemp(t, "life.vl.js", lifeScenarioJS(ls.URL, "/it"))
	code, stdout, stderr := validate(t, path)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, stdout, stderr)
	}
	if ls.count("/setup") != 1 || ls.count("/it") != 1 || ls.count("/teardown") != 1 || ls.onlyToken("/it") != ls.onlyToken("/teardown") {
		t.Fatalf("calls %v tokens %v", ls.counts, ls.tokens)
	}

	for src, stage := range map[string]string{
		`export function setup() { throw new Error("x"); } export default function () {}`:    "(setup)",
		`export function teardown() { throw new Error("x"); } export default function () {}`: "(teardown)",
		`export const options = { vu: 1 }; export default function () {}`:                    "(load)",
	} {
		p := writeTemp(t, "v.vl.js", src)
		code, stdout, _ := validate(t, p)
		if code != 1 || !strings.Contains(stdout, stage) {
			t.Errorf("%s: exit %d, %s", src, code, stdout)
		}
	}
}
