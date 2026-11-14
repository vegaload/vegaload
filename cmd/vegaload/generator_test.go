package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/genload"
	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/report"
	"github.com/vegaload/vegaload/internal/stream"
)

// fakeGenerator makes runs in this test measure a generator that uses
// cpuPercent of two CPUs, sampled every 20ms, instead of the real process.
func fakeGenerator(t *testing.T, cpuPercent float64) {
	t.Helper()
	oldSrc, oldInterval, oldCPUs := generatorSource, generatorInterval, generatorCPUs
	t.Cleanup(func() { generatorSource, generatorInterval, generatorCPUs = oldSrc, oldInterval, oldCPUs })
	start := time.Now()
	generatorSource = func() genload.Snapshot {
		now := time.Now()
		return genload.Snapshot{At: now, CPUOK: true, CPU: time.Duration(float64(now.Sub(start)) * 2 * cpuPercent / 100)}
	}
	generatorInterval = 20 * time.Millisecond
	generatorCPUs = func() int { return 2 }
}

func TestOverloadWarning(t *testing.T) {
	busy := genload.Stats{CPUs: 8, Samples: 10, BusySamples: 4, CPUPeakPercent: 99}
	behind := genload.Stats{Samples: 10, SlowSamples: 3, SchedPeakP99: 42 * time.Millisecond}
	for _, tc := range []struct {
		name    string
		s       genload.Stats
		dropped float64
		want    []string
	}{
		{"quiet", genload.Stats{Samples: 10, BusySamples: 2, SlowSamples: 2}, 1, nil},
		{"cpu", busy, 0, []string{"CPU use was at or above 90% of 8 CPUs for 4 of 10 seconds (peak 99%)", "this machine's limits"}},
		{"sched", behind, 0, []string{"goroutines waited 20ms or more to run for 3 of 10 seconds (peak p99 42ms)", "this machine's limits"}},
		{"dropped", genload.Stats{}, 5.5, []string{"5.50% of the planned iterations were dropped", "the target got less load than planned"}},
	} {
		w := overloadWarning(tc.s, tc.dropped)
		if tc.want == nil {
			if w != "" {
				t.Errorf("%s: %q, want no warning", tc.name, w)
			}
			continue
		}
		if !strings.HasPrefix(w, overloadPrefix) {
			t.Errorf("%s: %q does not start with %q", tc.name, w, overloadPrefix)
		}
		for _, part := range tc.want {
			if !strings.Contains(w, part) {
				t.Errorf("%s: %q, want it to say %q", tc.name, w, part)
			}
		}
	}
}

// An overloaded generator is in the JSON, the text output, the HTML report
// and the stream, from the one warning in Result.Warnings.
func TestRun_OverloadedGeneratorWarnsEverywhere(t *testing.T) {
	clearStreamEnv(t)
	fakeGenerator(t, 97)
	srv := quietServer(t)
	dir := t.TempDir()
	out, html, events := filepath.Join(dir, "out.json"), filepath.Join(dir, "r.html"), filepath.Join(dir, "run.ndjson")
	code, stdout, stderr := captureCmd(t, func() int {
		return cmdRun([]string{"-target", srv.URL, "-protocol", "http1", "-vus", "1", "-duration", "300ms",
			"-audit-log", filepath.Join(dir, "audit.log"), "-out", out, "-report", html, "-stream-file", events})
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var res report.Result
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if g := res.Generator; g == nil || !g.Overloaded || g.CPUs != 2 || g.CPUPeakPercent < 90 {
		t.Fatalf("generator = %+v, want overloaded at about 97%% of 2 CPUs", res.Generator)
	}
	var warning string
	for _, w := range res.Warnings {
		if strings.HasPrefix(w, overloadPrefix) {
			warning = w
		}
	}
	if warning == "" {
		t.Fatalf("warnings %q, want the overload warning", res.Warnings)
	}

	if !strings.Contains(stdout, "WARNING:  "+warning) {
		t.Errorf("the text output does not show the warning:\n%s", stdout)
	}
	page, err := os.ReadFile(html)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(page, []byte("WARNING</span> generator overloaded: CPU use was at or above 90%")) {
		t.Errorf("the HTML report does not show the warning")
	}
	rec, err := stream.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	var streamed []string
	for _, e := range rec.Events {
		if e.Type == stream.TypeWarning {
			streamed = append(streamed, dataOf(t, e)["message"].(string))
		}
	}
	if len(streamed) != 1 || streamed[0] != warning {
		t.Errorf("stream warnings %q, want the overload warning", streamed)
	}
	if last := rec.Events[len(rec.Events)-1]; last.Type != stream.TypeRunFinished {
		t.Errorf("last event %s, want run.finished after the warning", last.Type)
	}
}

func TestRun_QuietGeneratorDoesNotWarn(t *testing.T) {
	fakeGenerator(t, 10)
	srv := quietServer(t)
	res, err := runScenario(&runConfig{Executor: "fixed-vus", VUs: 1, Duration: 300 * time.Millisecond,
		Protocol: "http1", Target: protocol.Target{URL: srv.URL}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if g := res.Generator; g == nil || g.Overloaded || g.CPUAvgPercent < 5 || g.CPUAvgPercent > 15 {
		t.Errorf("generator = %+v, want about 10%% and not overloaded", res.Generator)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings %q, want none", res.Warnings)
	}
}

func TestParseRunArgs_NoConnectionReuse(t *testing.T) {
	cfg, err := parseRunArgs([]string{"-no-connection-reuse", "-target", "http://127.0.0.1:1", "-protocol", "http1"})
	if err != nil || !cfg.NoConnectionReuse {
		t.Fatalf("cfg %+v, err %v", cfg, err)
	}
	if _, err := parseRunArgs([]string{"-no-connection-reuse", "-target", "127.0.0.1:1", "-protocol", "tcp"}); err == nil || !strings.Contains(err.Error(), "-no-connection-reuse is for HTTP") {
		t.Errorf("-protocol tcp: %v, want it refused", err)
	}
}

// connCountingServer counts the connections it accepts.
func connCountingServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var opened atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &opened
}

// A run reuses its connections: N VUs open about N, in -protocol http1 and
// in a scenario, however many requests they send.
func TestRun_VUsReuseConnections(t *testing.T) {
	const vus = 12
	srv, opened := connCountingServer(t)
	res, err := runScenario(&runConfig{Executor: "fixed-vus", VUs: vus, Duration: 500 * time.Millisecond,
		Protocol: "http1", Target: protocol.Target{URL: srv.URL}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if n := opened.Load(); n > vus || res.Total < 10*vus {
		t.Errorf("-protocol http1: %d requests opened %d connections, want at most %d", res.Total, n, vus)
	}

	srv2, opened2 := connCountingServer(t)
	path := writeScenario(t, "s.vl.js", `export default function () { http.get("`+srv2.URL+`/"); }`)
	res, err = runScenario(&runConfig{Executor: "fixed-vus", VUs: vus, Duration: 500 * time.Millisecond, ScenarioPath: path, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if n := opened2.Load(); n > vus || res.Total < 10*vus {
		t.Errorf("scenario: %d requests opened %d connections, want at most %d", res.Total, n, vus)
	}
}

// -no-connection-reuse opens a connection per request. The runs are paced,
// so they do not use up the ephemeral ports.
func TestRun_NoConnectionReuseOpensOnePerRequest(t *testing.T) {
	srv, opened := connCountingServer(t)
	res, err := runScenario(&runConfig{Executor: "constant-arrival-rate", Rate: 40, Duration: 500 * time.Millisecond, MaxVUs: 2,
		Protocol: "http1", Target: protocol.Target{URL: srv.URL}, Timeout: time.Second, NoConnectionReuse: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := opened.Load(); res.Total < 10 || n < res.Total {
		t.Errorf("-protocol http1: %d requests opened %d connections, want one each", res.Total, n)
	}

	srv2, opened2 := connCountingServer(t)
	path := writeScenario(t, "s.vl.js", `export default function () { http.get("`+srv2.URL+`/"); }`)
	res, err = runScenario(&runConfig{Executor: "constant-arrival-rate", Rate: 40, Duration: 500 * time.Millisecond, MaxVUs: 2,
		ScenarioPath: path, Timeout: time.Second, NoConnectionReuse: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := opened2.Load(); res.Total < 10 || n < res.Total {
		t.Errorf("scenario: %d requests opened %d connections, want one each", res.Total, n)
	}
}
