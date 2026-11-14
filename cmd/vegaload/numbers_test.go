package main

// Tests that a run reports the right success, failure and latency numbers:
// HTTP statuses in a scenario, per-call latency, timeouts, dropped
// arrivals, and the planned duration every output shows.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/engine"
	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/report"
	"github.com/vegaload/vegaload/internal/safety"
	"github.com/vegaload/vegaload/internal/stream"
)

// numbersServer answers /status/N with status N, /redirect/N with a 302 to
// /status/N, and /slow/MS with 200 after MS milliseconds.
func numbersServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if code, ok := strings.CutPrefix(path, "/redirect/"); ok {
			http.Redirect(w, r, "/status/"+code, http.StatusFound)
			return
		}
		if ms, ok := strings.CutPrefix(path, "/slow/"); ok {
			n, _ := strconv.Atoi(ms)
			select {
			case <-time.After(time.Duration(n) * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			return
		}
		code, err := strconv.Atoi(strings.TrimPrefix(path, "/status/"))
		if err != nil {
			code = http.StatusBadRequest
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func reasonCounts(res *report.Result) map[string]int64 {
	out := map[string]int64{}
	for _, fr := range res.FailureReasons {
		out[fr.Reason] = fr.Count
	}
	return out
}

// Each iteration makes five calls: 200, a redirect to 200, 404, 500, and a
// 404 it expects. Two of every five fail, and the iteration does not.
func checkStatusRun(t *testing.T, res *report.Result) {
	t.Helper()
	if res.Total < 5 || res.Iterations == nil || res.Iterations.Total == 0 {
		t.Fatalf("total %d, iterations %+v: want at least one full iteration", res.Total, res.Iterations)
	}
	if res.Iterations.Failed != 0 {
		t.Errorf("Iterations.Failed = %d, want 0: a failed status does not fail the iteration", res.Iterations.Failed)
	}
	reasons := reasonCounts(res)
	if len(reasons) != 2 || reasons["status 404"] == 0 || reasons["status 500"] == 0 {
		t.Fatalf("FailureReasons = %+v, want only status 404 and status 500", res.FailureReasons)
	}
	if d := reasons["status 404"] - reasons["status 500"]; d < -1 || d > 1 {
		t.Errorf("status 404 %d, status 500 %d: want the same count (the expected 404 is not a failure)", reasons["status 404"], reasons["status 500"])
	}
	if res.Failed != reasons["status 404"]+reasons["status 500"] {
		t.Errorf("Failed = %d, the reasons add up to %d", res.Failed, reasons["status 404"]+reasons["status 500"])
	}
	if res.ErrorRate < 0.3 || res.ErrorRate > 0.5 {
		t.Errorf("ErrorRate = %v, want about 0.4 (2 of 5 calls)", res.ErrorRate)
	}
}

func TestRunScenario_ScriptHTTPStatusesJS(t *testing.T) {
	srv := numbersServer(t)
	path := writeScenario(t, "s.vl.js", `export default function () {
		const base = "`+srv.URL+`";
		http.get(base + "/status/200");
		http.get(base + "/redirect/200");
		http.get(base + "/status/404");
		http.get(base + "/status/500");
		http.get(base + "/status/404", { expectedStatuses: [404] });
	}`)
	res, err := runScenario(&runConfig{Executor: "fixed-vus", VUs: 1, Duration: 300 * time.Millisecond, ScenarioPath: path, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	checkStatusRun(t, res)
}

func TestRunScenario_ScriptHTTPStatusesPython(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH, skipping")
	}
	srv := numbersServer(t)
	path := writeScenario(t, "s.py", `def iteration():
    base = "`+srv.URL+`"
    http.get(base + "/status/200")
    http.get(base + "/redirect/200")
    http.get(base + "/status/404")
    http.get(base + "/status/500")
    http.get(base + "/status/404", expected_statuses=[404])
`)
	res, err := runScenario(&runConfig{Executor: "fixed-vus", VUs: 1, Duration: 500 * time.Millisecond, ScenarioPath: path, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	checkStatusRun(t, res)
}

// The run's latency is the latency of one call, and an iteration of three
// calls is counted, and timed, on its own.
func TestRunScenario_ScriptLatencyIsPerCall(t *testing.T) {
	srv := numbersServer(t)
	path := writeScenario(t, "s.vl.js", `export default function () {
		for (let i = 0; i < 3; i++) http.get("`+srv.URL+`/slow/20");
	}`)
	res, err := runScenario(&runConfig{Executor: "fixed-vus", VUs: 1, Duration: 700 * time.Millisecond, ScenarioPath: path, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	it := res.Iterations
	if it == nil || it.Total == 0 {
		t.Fatalf("Iterations = %+v", it)
	}
	if res.Latency.P50 < 20*time.Millisecond || res.Latency.P50 > 50*time.Millisecond {
		t.Errorf("request p50 = %s, want one call's time (about 20ms)", res.Latency.P50)
	}
	if it.Duration.P50 < 60*time.Millisecond {
		t.Errorf("iteration p50 = %s, want three calls' time (60ms or more)", it.Duration.P50)
	}
	if res.Total < 3*it.Total || res.Total > 3*it.Total+3 {
		t.Errorf("total %d calls for %d iterations, want 3 per iteration", res.Total, it.Total)
	}
	if p, _ := res.Protocol("http"); p.Latency.P50 != res.Latency.P50 || p.Total != res.Total {
		t.Errorf("the http breakdown (%d calls, p50 %s) differs from the run (%d, %s)", p.Total, p.Latency.P50, res.Total, res.Latency.P50)
	}
}

// A call that runs out of -timeout fails with the reason "timeout", and its
// latency is the time it waited, in a scenario and in a -protocol run.
func TestRun_TimeoutIsAFailureWithItsRealTime(t *testing.T) {
	srv := numbersServer(t)
	scenario := writeScenario(t, "s.vl.js", `export default function () {
		try { http.get("`+srv.URL+`/slow/2000"); } catch (e) {}
	}`)
	for name, cfg := range map[string]*runConfig{
		"scenario": {ScenarioPath: scenario},
		"protocol": {Protocol: "http1", Target: protocol.Target{URL: srv.URL + "/slow/2000"}},
	} {
		cfg.Executor, cfg.VUs, cfg.Duration, cfg.Timeout = "fixed-vus", 2, 500*time.Millisecond, 80*time.Millisecond
		res, err := runScenario(cfg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Total == 0 || res.Failed != res.Total {
			t.Fatalf("%s: total %d failed %d, want every request failed", name, res.Total, res.Failed)
		}
		if reasons := reasonCounts(res); len(reasons) != 1 || reasons["timeout"] != res.Failed {
			t.Errorf("%s: FailureReasons = %+v, want only timeout", name, res.FailureReasons)
		}
		if res.Latency.Min < 80*time.Millisecond || res.Latency.Max > time.Second {
			t.Errorf("%s: latency %s..%s, want the time each call waited (about 80ms)", name, res.Latency.Min, res.Latency.Max)
		}
	}
}

func TestRun_ProtocolStatusFailureIsNamed(t *testing.T) {
	srv := numbersServer(t)
	res, err := runScenario(&runConfig{Executor: "fixed-vus", VUs: 1, Duration: 200 * time.Millisecond,
		Protocol: "http1", Target: protocol.Target{URL: srv.URL + "/status/503"}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total == 0 || res.Failed != res.Total {
		t.Fatalf("total %d failed %d, want every request failed", res.Total, res.Failed)
	}
	if reasons := reasonCounts(res); len(reasons) != 1 || reasons["status 503"] != res.Failed {
		t.Errorf("FailureReasons = %+v, want only status 503", res.FailureReasons)
	}
	if res.Iterations != nil {
		t.Errorf("Iterations = %+v, want nil: in a -protocol run an iteration is one request", res.Iterations)
	}
}

// An arrival that finds every VU busy is dropped and counted, not recorded
// as a 0 ms failure: the latency stays the latency of the calls that ran.
func TestRun_ArrivalRateDropsAreNotFailures(t *testing.T) {
	srv := numbersServer(t)
	for name, cfg := range map[string]*runConfig{
		"constant": {Executor: "constant-arrival-rate", Rate: 200, Duration: time.Second},
		"ramping":  {Executor: "ramping-arrival-rate", Rate: 200, Stages: []engine.Stage{{Target: 200, Duration: time.Second}}},
	} {
		cfg.MaxVUs, cfg.Timeout = 1, time.Second
		cfg.Protocol, cfg.Target = "http1", protocol.Target{URL: srv.URL + "/slow/15"}
		res, err := runScenario(cfg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.DroppedIterations < 50 {
			t.Errorf("%s: DroppedIterations = %d, want most of 200 arrivals dropped with one VU", name, res.DroppedIterations)
		}
		if res.Failed != 0 || res.ErrorRate != 0 {
			t.Errorf("%s: failed %d, error rate %v, want 0: a dropped arrival is not a failed request", name, res.Failed, res.ErrorRate)
		}
		if res.Latency.Min < 15*time.Millisecond || res.Latency.P50 < 15*time.Millisecond {
			t.Errorf("%s: latency min %s p50 %s, want every request at 15ms or more (no 0 ms drops)", name, res.Latency.Min, res.Latency.P50)
		}
		// The drop warning, then the generator overload warning naming the
		// drops among its causes.
		if len(res.Warnings) != 2 || !strings.Contains(res.Warnings[0], "were dropped because every VU was busy") ||
			!strings.HasPrefix(res.Warnings[1], overloadPrefix) || !strings.Contains(res.Warnings[1], "iterations were dropped") {
			t.Errorf("%s: Warnings = %q, want the drop warning and the overload warning", name, res.Warnings)
		}
		if res.Generator == nil || !res.Generator.Overloaded {
			t.Errorf("%s: Generator = %+v, want overloaded", name, res.Generator)
		}
	}
}

// Enough VUs means no drop and no warning.
func TestRun_ArrivalRateWithEnoughVUsDropsNothing(t *testing.T) {
	srv := numbersServer(t)
	res, err := runScenario(&runConfig{Executor: "constant-arrival-rate", Rate: 50, Duration: 600 * time.Millisecond, MaxVUs: 20,
		Protocol: "http1", Target: protocol.Target{URL: srv.URL + "/slow/5"}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.DroppedIterations != 0 || len(res.Warnings) != 0 {
		t.Errorf("dropped %d, warnings %q, want none", res.DroppedIterations, res.Warnings)
	}
}

// A scenario under arrival rate counts its drops apart from its iterations.
func TestRunScenario_ScriptArrivalRateDrops(t *testing.T) {
	srv := numbersServer(t)
	path := writeScenario(t, "s.vl.js", `export default function () { http.get("`+srv.URL+`/slow/15"); }`)
	res, err := runScenario(&runConfig{Executor: "constant-arrival-rate", Rate: 200, Duration: time.Second, MaxVUs: 1, ScenarioPath: path, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.DroppedIterations < 50 || res.Iterations == nil || res.Iterations.Total == 0 {
		t.Fatalf("dropped %d, iterations %+v", res.DroppedIterations, res.Iterations)
	}
	if res.Failed != 0 || res.Latency.Min < 15*time.Millisecond {
		t.Errorf("failed %d, latency min %s, want 0 failed and no 0 ms request", res.Failed, res.Latency.Min)
	}
}

// The drop warning reaches the stream after the last point and before
// run.finished, and the terminal summary shows the drops.
func TestRun_DropWarningIsStreamed(t *testing.T) {
	clearStreamEnv(t)
	srv := numbersServer(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "run.ndjson")
	code, stdout, stderr := captureCmd(t, func() int {
		return cmdRun([]string{"-target", srv.URL + "/slow/15", "-protocol", "http1",
			"-executor", "constant-arrival-rate", "-rate", "200", "-max-vus", "1", "-duration", "1s",
			"-no-report", "-audit-log", filepath.Join(dir, "audit.log"), "-stream-file", file})
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "dropped:") || !strings.Contains(stdout, "WARNING:") {
		t.Errorf("the summary does not show the drops:\n%s", stdout)
	}
	rec, err := stream.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	n := len(rec.Events)
	if n < 3 || rec.Events[n-2].Type != stream.TypeWarning || rec.Events[n-1].Type != stream.TypeRunFinished {
		types := make([]string, n)
		for i, e := range rec.Events {
			types[i] = e.Type
		}
		t.Fatalf("events %v, want ... point, warning, run.finished", types)
	}
	if msg := dataOf(t, rec.Events[n-2])["message"]; !strings.Contains(msg.(string), "were dropped because every VU was busy") {
		t.Errorf("warning = %v", msg)
	}
	if fin := dataOf(t, rec.Events[n-1]); fin["dropped_iterations"] == nil {
		t.Errorf("run.finished has no dropped_iterations: %v", fin)
	}
}

// The planned duration is what the executor runs for, for every shape.
func TestPlannedDurationIsTheExecutorDuration(t *testing.T) {
	stages := []engine.Stage{{Target: 20, Duration: 10 * time.Second}, {Target: 20, Duration: 10 * time.Second}}
	for _, cfg := range []*runConfig{
		{Executor: "fixed-vus", VUs: 5, Duration: 7 * time.Second},
		{Executor: "constant-arrival-rate", Rate: 10, Duration: 7 * time.Second},
		{Executor: "ramp", Duration: 10 * time.Second, Stages: stages},
		{Executor: "step", Duration: 10 * time.Second, Stages: stages},
		{Executor: "ramping-arrival-rate", Duration: 10 * time.Second, Stages: stages},
	} {
		ex, err := buildExecutor(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := plannedDuration(cfg); got != ex.Duration() {
			t.Errorf("%s: plannedDuration = %s, the executor runs %s", cfg.Executor, got, ex.Duration())
		}
		if rs := buildRunStarted(cfg, ex.Name(), ex.Duration()); !strings.Contains(shapeSummary(cfg), "duration="+rs.Duration) {
			t.Errorf("%s: summary %q, stream duration %s", cfg.Executor, shapeSummary(cfg), rs.Duration)
		}
	}
	// A shape that cannot be built falls back to -duration.
	if got := plannedDuration(&runConfig{Executor: "ramp", Duration: 3 * time.Second}); got != 3*time.Second {
		t.Errorf("ramp with no stages: %s, want -duration", got)
	}
}

func TestShapeSummary(t *testing.T) {
	stages := []engine.Stage{{Target: 20, Duration: 10 * time.Second}, {Target: 20, Duration: 10 * time.Second}}
	for _, c := range []struct {
		cfg  *runConfig
		want string
	}{
		{&runConfig{Executor: "fixed-vus", VUs: 5, Duration: 30 * time.Second}, "vus=5, duration=30s"},
		{&runConfig{Executor: "ramp", VUs: 1, Duration: 10 * time.Second, Stages: stages}, "peak vus=20, duration=20s"},
		{&runConfig{Executor: "step", VUs: 1, Duration: 10 * time.Second, Stages: stages}, "peak vus=20, duration=20s"},
		{&runConfig{Executor: "constant-arrival-rate", Rate: 200, MaxVUs: 2, Duration: 3 * time.Second}, "rate=200/s, max-vus=2, duration=3s"},
		{&runConfig{Executor: "constant-arrival-rate", Rate: 5, Duration: 3 * time.Second}, "rate=5/s, max-vus=1, duration=3s"},
		{&runConfig{Executor: "ramping-arrival-rate", MaxVUs: 50, Duration: 10 * time.Second, Stages: []engine.Stage{{Target: 50, Duration: 4 * time.Second}, {Target: 600, Duration: 3 * time.Second}}}, "peak rate=600/s, max-vus=50, duration=7s"},
	} {
		if got := shapeSummary(c.cfg); got != c.want {
			t.Errorf("%s: %q, want %q", c.cfg.Executor, got, c.want)
		}
	}
}

// The terminal, the JSONL run_started line, the stream, the audit log and
// the report all give a ramp run the sum of its stages, not -duration.
func TestRampDurationIsTheSameEverywhere(t *testing.T) {
	clearStreamEnv(t)
	srv := numbersServer(t)
	dir := t.TempDir()
	file, auditPath, out := filepath.Join(dir, "run.ndjson"), filepath.Join(dir, "audit.log"), filepath.Join(dir, "out.json")
	ramp := []string{"-executor", "ramp", "-stages", "2:400ms,2:400ms", "-target", srv.URL + "/status/200", "-protocol", "http1", "-no-report", "-audit-log", auditPath}

	code, stdout, stderr := captureCmd(t, func() int {
		return cmdRun(append(append([]string{}, ramp...), "-stream-file", file, "-out", out))
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if first, _, _ := strings.Cut(stdout, "\n"); first != "running ramp (peak vus=2, duration=800ms)..." {
		t.Errorf("first line %q", first)
	}
	rec, err := stream.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if d := dataOf(t, rec.Events[0])["duration"]; d != "800ms" {
		t.Errorf("run.started duration %v, want 800ms", d)
	}
	entries := readAudit(t, auditPath)
	if got := entries[len(entries)-1].Duration; got != 800*time.Millisecond {
		t.Errorf("audit duration %s, want 800ms", got)
	}
	res, err := loadReportJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Elapsed < 800*time.Millisecond || res.Elapsed > 2*time.Second {
		t.Errorf("report elapsed %s, want about 800ms", res.Elapsed)
	}

	code, stdout, stderr = captureCmd(t, func() int { return cmdRun(append(append([]string{}, ramp...), "-output", "jsonl")) })
	if code != 0 {
		t.Fatalf("jsonl exit %d: %s", code, stderr)
	}
	var started struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	line, _, _ := bufio.NewReader(bytes.NewBufferString(stdout)).ReadLine()
	if err := json.Unmarshal(line, &started); err != nil || started.Type != "run_started" {
		t.Fatalf("first jsonl line %q: %v", line, err)
	}
	if got := started.Data["duration_ns"]; got != float64(800*time.Millisecond) {
		t.Errorf("run_started duration_ns %v, want %d", got, 800*time.Millisecond)
	}
}

// The duration cap applies to a ramp's stages, not to -duration.
func TestEnforceSafety_CapsTheRampStages(t *testing.T) {
	limits := safety.DefaultLimits
	limits.MaxDuration = time.Minute
	cfg := &runConfig{Executor: "ramp", VUs: 1, Duration: 10 * time.Second, Limits: limits,
		Stages: []engine.Stage{{Target: 2, Duration: 50 * time.Second}, {Target: 2, Duration: 50 * time.Second}}}
	if err := enforceSafety(cfg, nil, nil, nil); err == nil {
		t.Error("a 100s ramp passed a 1m duration cap")
	}
	cfg.Stages = []engine.Stage{{Target: 2, Duration: 20 * time.Second}}
	if err := enforceSafety(cfg, nil, nil, nil); err != nil {
		t.Errorf("a 20s ramp under a 1m cap was refused: %v", err)
	}
}

func TestPrintResult_ShowsReasonsDropsWarningsAndIterations(t *testing.T) {
	var b bytes.Buffer
	printResult(&b, &report.Result{
		Executor: "constant-arrival-rate", Total: 30, Failed: 4,
		FailureReasons:    []report.FailureReason{{Reason: "status 503", Count: 3}, {Reason: "timeout", Count: 1}},
		DroppedIterations: 12,
		Warnings:          []string{"12 of 22 planned iterations (54.55%) were dropped"},
		Iterations:        &report.IterationStats{Total: 10, Failed: 2, ErrorRate: 0.2},
	})
	out := b.String()
	for _, want := range []string{
		"  failures: 3 status 503, 1 timeout\n",
		"  dropped:  12 iterations (every VU was busy; not in the numbers above)\n",
		"  WARNING:  12 of 22 planned iterations (54.55%) were dropped\n",
		"iterations (the numbers above are each call the scenario made)\n  10 ran, 2 failed (20.00%)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	b.Reset()
	printResult(&b, &report.Result{Executor: "fixed-vus", Total: 5})
	for _, absent := range []string{"failures:", "dropped:", "WARNING:", "iterations"} {
		if strings.Contains(b.String(), absent) {
			t.Errorf("a clean -protocol run shows %q:\n%s", absent, b.String())
		}
	}
}
