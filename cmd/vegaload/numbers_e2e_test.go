package main

// End-to-end checks that a scenario run's numbers, now counted per call,
// reach every output and gate the same way: the stream, JSONL progress,
// thresholds, -abort-on-breach, and calls other than HTTP.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/report"
	"github.com/vegaload/vegaload/internal/stream"
)

// In a scenario run the live points are the calls: they add up to the
// result's total and failed, and match its time series second by second.
func TestStream_ScenarioPointsAreTheCalls(t *testing.T) {
	clearStreamEnv(t)
	srv := numbersServer(t)
	dir := t.TempDir()
	scenario := writeScenario(t, "s.vl.js", `export default function () {
		http.get("`+srv.URL+`/status/200");
		http.get("`+srv.URL+`/status/500");
		http.get("`+srv.URL+`/slow/5");
	}`)
	file, out := filepath.Join(dir, "run.ndjson"), filepath.Join(dir, "out.json")
	code, _, stderr := captureCmd(t, func() int {
		return cmdRun([]string{"-vus", "2", "-duration", "2300ms", "-no-report", "-audit-log", filepath.Join(dir, "a.log"),
			"-stream-file", file, "-out", out, scenario})
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	res, err := loadReportJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Iterations == nil || res.Total < 3*res.Iterations.Total || res.Failed == 0 {
		t.Fatalf("total %d failed %d iterations %+v: want every call counted", res.Total, res.Failed, res.Iterations)
	}
	rec, err := stream.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var total, failed int64
	i := 0
	for _, e := range rec.Events {
		if e.Type != stream.TypePoint {
			continue
		}
		p := dataOf(t, e)
		total += int64(p["requests"].(float64))
		failed += int64(p["failed"].(float64))
		if i < len(res.TimeSeries) {
			s := res.TimeSeries[i]
			if int64(p["requests"].(float64)) != s.Requests || int64(p["failed"].(float64)) != s.Failed {
				t.Errorf("point %d %v differs from the series %+v", i, p, s)
			}
		}
		i++
	}
	if total != res.Total || failed != res.Failed {
		t.Errorf("points add up to %d/%d, the result has %d/%d", total, failed, res.Total, res.Failed)
	}
}

// -output jsonl progress lines count calls too, and end at the result's
// total.
func TestRunJSONL_ScenarioProgressCountsCalls(t *testing.T) {
	srv := numbersServer(t)
	scenario := writeScenario(t, "s.vl.js", `export default function () {
		http.get("`+srv.URL+`/slow/5");
		http.get("`+srv.URL+`/status/503");
	}`)
	var buf bytes.Buffer
	res, err := runJSONL(&runConfig{Executor: "fixed-vus", VUs: 1, Duration: 1300 * time.Millisecond, ScenarioPath: scenario, Timeout: time.Second}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	var lastProgress map[string]any
	sc := bufio.NewScanner(&buf)
	for sc.Scan() {
		var ev struct {
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type == "progress" {
			lastProgress = ev.Data
		}
	}
	if lastProgress == nil {
		t.Fatal("no progress line")
	}
	got, gotFailed := int64(lastProgress["total"].(float64)), int64(lastProgress["failed"].(float64))
	if got == 0 || got > res.Total || gotFailed == 0 || gotFailed > res.Failed {
		t.Errorf("last progress %d/%d, result %d/%d: progress must count calls", got, gotFailed, res.Total, res.Failed)
	}
	if res.Iterations == nil || got <= res.Iterations.Total/2 {
		t.Errorf("progress total %d looks like iterations (%+v), not calls", got, res.Iterations)
	}
}

// A scenario that gets HTTP 500 now breaks an error_rate threshold, and a
// healthy one does not.
func TestCmdRun_ScenarioThresholdSeesFailedStatuses(t *testing.T) {
	srv := numbersServer(t)
	for path, want := range map[string]int{"/status/500": exitThresholdsBreached, "/status/200": 0} {
		scenario := writeScenario(t, "s.vl.js", `export default function () { http.get("`+srv.URL+path+`"); }`)
		code, _, stderr := captureCmd(t, func() int {
			return cmdRun([]string{"-vus", "1", "-duration", "300ms", "-no-report", "-audit-log", filepath.Join(t.TempDir(), "a.log"),
				"-threshold", "error_rate < 1%", scenario})
		})
		if code != want {
			t.Errorf("%s: exit %d, want %d: %s", path, code, want, stderr)
		}
	}
}

// -abort-on-breach stops a scenario run as soon as failed statuses break a
// "failed <" threshold.
func TestCmdRun_ScenarioAbortsOnFailedStatuses(t *testing.T) {
	srv := numbersServer(t)
	dir := t.TempDir()
	scenario := writeScenario(t, "s.vl.js", `export default function () { http.get("`+srv.URL+`/status/500"); }`)
	out := filepath.Join(dir, "out.json")
	start := time.Now()
	code, _, stderr := captureCmd(t, func() int {
		return cmdRun([]string{"-vus", "1", "-duration", "20s", "-no-report", "-audit-log", filepath.Join(dir, "a.log"),
			"-abort-on-breach", "-threshold", "failed < 5", "-out", out, scenario})
	})
	if code != exitThresholdsBreached {
		t.Fatalf("exit %d, want %d: %s", code, exitThresholdsBreached, stderr)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the run took %s; it should stop at the first look after the 5th failed call", took)
	}
	res, err := loadReportJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Aborted == nil || res.Aborted.Threshold != "failed < 5" {
		t.Errorf("Aborted = %+v", res.Aborted)
	}
	if reasons := reasonCounts(res); reasons["status 500"] != res.Failed || res.Failed < 5 {
		t.Errorf("failed %d, reasons %+v", res.Failed, res.FailureReasons)
	}
}

// Calls other than HTTP are requests too, and an error the call reports
// only as text still gets a reason.
func TestRunScenario_NonHTTPCallsAreRequests(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := ln.Addr().String()
	ln.Close()

	upgrader := websocket.Upgrader{}
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer ws.Close()

	scenario := writeScenario(t, "s.vl.js", `export default function () {
		const r = tcp.send("tcp://`+closedAddr+`", { body: "PING\r\n" });
		if (r.ok) throw new Error("a closed port answered");
		const conn = ws.connect("ws://`+strings.TrimPrefix(ws.URL, "http://")+`");
		conn.send("hello");
		conn.close();
	}`)
	// tcp opens a socket per call, so the run is paced (see AGENTS.md).
	res, err := runScenario(&runConfig{Executor: "constant-arrival-rate", Rate: 20, MaxVUs: 2, Duration: 500 * time.Millisecond,
		ScenarioPath: scenario, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	tcpRes, _ := res.Protocol("tcp")
	wsRes, _ := res.Protocol("websocket")
	if tcpRes.Total == 0 || wsRes.Total == 0 {
		t.Fatalf("tcp %+v websocket %+v, want both recorded", tcpRes, wsRes)
	}
	if res.Total != tcpRes.Total+wsRes.Total {
		t.Errorf("total %d, want the tcp (%d) and websocket (%d) calls", res.Total, tcpRes.Total, wsRes.Total)
	}
	if res.Failed != tcpRes.Total || wsRes.Failed != 0 {
		t.Errorf("failed %d (websocket %d), want only the %d tcp calls", res.Failed, wsRes.Failed, tcpRes.Total)
	}
	if reasons := reasonCounts(res); reasons["connection refused"] != res.Failed {
		t.Errorf("FailureReasons = %+v, want every failure to be connection refused", res.FailureReasons)
	}
	if res.Iterations == nil || res.Iterations.Failed != 0 {
		t.Errorf("Iterations = %+v, want none failed: a tcp reply with ok false does not throw", res.Iterations)
	}
}

// A Python scenario's timeout is a failed call with its real time, the
// same as JavaScript's.
func TestRunScenario_PythonTimeout(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH, skipping")
	}
	srv := numbersServer(t)
	scenario := writeScenario(t, "s.py", `def iteration():
    try:
        http.get("`+srv.URL+`/slow/2000")
    except Exception:
        pass
`)
	res, err := runScenario(&runConfig{Executor: "fixed-vus", VUs: 1, Duration: 600 * time.Millisecond, ScenarioPath: scenario, Timeout: 80 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total == 0 || res.Failed != res.Total || reasonCounts(res)["timeout"] != res.Failed {
		t.Fatalf("total %d failed %d reasons %+v, want every call a timeout", res.Total, res.Failed, res.FailureReasons)
	}
	if res.Latency.Min < 80*time.Millisecond {
		t.Errorf("latency min %s, want the time each call waited", res.Latency.Min)
	}
	if res.Iterations == nil || res.Iterations.Failed != 0 {
		t.Errorf("Iterations = %+v, want none failed: the script caught the error", res.Iterations)
	}
}

// http2 judges a status the same way as http1, and names the failure.
func TestRun_HTTP2StatusFailureIsNamed(t *testing.T) {
	srv := httptest.NewServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			w.WriteHeader(http.StatusHTTPVersionNotSupported)
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}), &http2.Server{}))
	defer srv.Close()
	res, err := runScenario(&runConfig{Executor: "fixed-vus", VUs: 1, Duration: 200 * time.Millisecond,
		Protocol: "http2", Target: protocol.Target{URL: srv.URL}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total == 0 || res.Failed != res.Total {
		t.Fatalf("total %d failed %d", res.Total, res.Failed)
	}
	if want := []report.FailureReason{{Reason: "status 429", Count: res.Failed}}; len(res.FailureReasons) != 1 || res.FailureReasons[0] != want[0] {
		t.Errorf("FailureReasons = %+v, want %+v", res.FailureReasons, want)
	}
}
