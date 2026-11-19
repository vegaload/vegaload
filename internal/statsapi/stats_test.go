package statsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/engine"
	"github.com/vegaload/vegaload/internal/failure"
	"github.com/vegaload/vegaload/internal/report"
)

// k6SidecarKeys are the top-level keys of the Harness k6 stats sidecar's
// snapshot, with the JSON type each has there.
var k6SidecarKeys = map[string]string{
	"totalRequests": "number", "totalFailures": "number", "avgResponseMs": "number",
	"p50Ms": "number", "p95Ms": "number", "p99Ms": "number", "errorRate": "number",
	"avgRps": "number", "iterations": "number", "latencyHist": "object",
	"latencyHistParams": "object", "requestStats": "object", "checkStats": "object",
}

// The other sidecars' keys, and VegaLoad's own.
var extraKeys = map[string]string{
	"currentUsers": "number", "peakUsers": "number", "runDurationSec": "number",
	"dataSentBytes": "number", "dataReceivedBytes": "number",
	"avgLatencyMs": "number", "p50LatencyMs": "number", "p95LatencyMs": "number", "p99LatencyMs": "number",
	"state": "string", "maxUsers": "number", "iterationsStarted": "number", "iterationsCompleted": "number",
	"droppedIterations": "number", "errors": "object", "steps": "object",
}

var requestStatKeys = map[string]string{
	"name": "string", "method": "string", "numRequests": "number", "numFailures": "number",
	"avgResponseMs": "number", "minResponseMs": "number", "maxResponseMs": "number",
	"p50Ms": "number", "p95Ms": "number", "p99Ms": "number", "requestsPerSec": "number",
	"avgSizeBytes": "number", "latencyHist": "object",
}

var checkStatKeys = map[string]string{"name": "string", "passes": "number", "fails": "number"}

func jsonType(v any) string {
	switch v.(type) {
	case float64:
		return "number"
	case string:
		return "string"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case bool:
		return "bool"
	}
	return "null"
}

func hasKeys(t *testing.T, where string, m map[string]any, want map[string]string) {
	t.Helper()
	for k, typ := range want {
		v, ok := m[k]
		if !ok {
			t.Errorf("%s: no %q", where, k)
			continue
		}
		if got := jsonType(v); got != typ {
			t.Errorf("%s: %q is %s, want %s", where, k, got, typ)
		}
	}
}

// sampleRun records a small scenario run: two HTTP rows, a redis row, checks,
// a step, a TTFB, and failures of two kinds.
func sampleRun(c *report.Collector) {
	c.VUStarted()
	c.Begin(c.Start())
	for i := 0; i < 3; i++ {
		c.RecordCall("http", "GET /users/{id}", time.Duration(10+i)*time.Millisecond, nil, 10, 100)
	}
	c.RecordCall("http", "POST /orders", 40*time.Millisecond, fmt.Errorf("x: %w", failure.Status(503)), 50, 20)
	c.RecordCall("redis", "command", 2*time.Millisecond, fmt.Errorf("redis: %w", context.DeadlineExceeded), 5, 0)
	c.RecordMeasure("http", report.TTFBMeasure, 8*time.Millisecond)
	c.RecordCheck("status is 200", true)
	c.RecordCheck("status is 200", false)
	c.RecordStep("login", 60*time.Millisecond, false)
	c.Record(engine.IterationResult{Start: c.Start(), Duration: 70 * time.Millisecond})
	c.End(c.Start())
}

func TestStatsHasEverySidecarKey(t *testing.T) {
	c := report.NewCollector()
	c.RequestsFromCalls()
	sampleRun(c)
	st := FromResult(c.Finish("fixed-vus", 2*time.Second), c.ActiveVUs(), StateRunning)
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	hasKeys(t, "stats", m, k6SidecarKeys)
	hasKeys(t, "stats", m, extraKeys)
	if len(m) != len(k6SidecarKeys)+len(extraKeys) {
		t.Errorf("%d keys, want %d: %s", len(m), len(k6SidecarKeys)+len(extraKeys), raw)
	}
	hasKeys(t, "latencyHistParams", m["latencyHistParams"].(map[string]any), map[string]string{"baseMs": "number", "growth": "number"})

	rows := m["requestStats"].(map[string]any)
	if len(rows) != 3 {
		t.Fatalf("requestStats: %v", rows)
	}
	for key, r := range rows {
		hasKeys(t, "requestStats "+key, r.(map[string]any), requestStatKeys)
	}
	for key, ch := range m["checkStats"].(map[string]any) {
		hasKeys(t, "checkStats "+key, ch.(map[string]any), checkStatKeys)
	}
}

func TestStatsValues(t *testing.T) {
	c := report.NewCollector()
	c.RequestsFromCalls()
	sampleRun(c)
	st := FromResult(c.Finish("fixed-vus", 2*time.Second), c.ActiveVUs(), StateRunning)

	if st.TotalRequests != 5 || st.TotalFailures != 2 || st.ErrorRate != 40 {
		t.Errorf("totals %d/%d, errorRate %v (want 40, a percent)", st.TotalRequests, st.TotalFailures, st.ErrorRate)
	}
	if st.AvgRps != 2.5 || st.RunDurationSec != 2 || st.Iterations != 1 || st.IterationsStarted != 1 {
		t.Errorf("avgRps %v, duration %v, iterations %d/%d", st.AvgRps, st.RunDurationSec, st.Iterations, st.IterationsStarted)
	}
	if st.LatencyHistParams != (HistParams{BaseMs: 0.5, Growth: 1.05}) {
		t.Errorf("params %+v", st.LatencyHistParams)
	}
	if n := sum(st.LatencyHist); n != st.TotalRequests {
		t.Errorf("latencyHist adds up to %d, want %d", n, st.TotalRequests)
	}

	get, ok := st.RequestStats["GET:/users/{id}"]
	if !ok || get.Name != "/users/{id}" || get.Method != "GET" || get.NumRequests != 3 || get.AvgSizeBytes != 100 || get.RequestsPerSec != 1.5 {
		t.Errorf("GET row %+v (ok %v)", get, ok)
	}
	if get.MinResponseMs != 10 || get.MaxResponseMs != 12 || sum(get.LatencyHist) != 3 {
		t.Errorf("GET row latency %+v", get)
	}
	if post := st.RequestStats["POST:/orders"]; post.NumFailures != 1 {
		t.Errorf("POST row %+v", post)
	}
	if r, ok := st.RequestStats["redis command"]; !ok || r.Method != "" || r.Name != "redis command" {
		t.Errorf("redis row %+v (ok %v)", r, ok)
	}

	if ch := st.CheckStats["status is 200"]; ch.Passes != 1 || ch.Fails != 1 {
		t.Errorf("check %+v", ch)
	}
	if st.CurrentUsers != 1 || st.PeakUsers != 1 || st.MaxUsers != 1 {
		t.Errorf("users %d/%d/%d", st.CurrentUsers, st.PeakUsers, st.MaxUsers)
	}
	if st.DataSentBytes != 85 || st.DataReceivedBytes != 320 {
		t.Errorf("bytes %d/%d", st.DataSentBytes, st.DataReceivedBytes)
	}
	if st.P50LatencyMs != 8 || st.AvgLatencyMs != 8 {
		t.Errorf("ttfb %v/%v", st.P50LatencyMs, st.AvgLatencyMs)
	}
	if e := st.Errors["status 503"]; e.Count != 1 || e.Sample != "x: status 503" {
		t.Errorf("errors %+v", st.Errors)
	}
	if e := st.Errors[failure.CheckFailed]; e.Count != 1 || e.Sample != "status is 200" {
		t.Errorf("check failed %+v", e)
	}
	if s := st.Steps["login"]; s.NumRequests != 1 || s.P95Ms != 60 {
		t.Errorf("steps %+v", st.Steps)
	}
}

func sum(m map[string]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}

func get(t *testing.T, method, url string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServerRoutes(t *testing.T) {
	s, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := "http://" + s.Addr()

	if code, body := get(t, "GET", base+"/healthz"); code != 200 || strings.TrimSpace(body) != `{"ok":true}` {
		t.Errorf("/healthz: %d %s", code, body)
	}
	code, body := get(t, "GET", base+"/stats")
	if code != 200 {
		t.Fatalf("/stats: %d", code)
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st["state"] != StateStarting || st["totalRequests"] != float64(0) || jsonType(st["requestStats"]) != "object" || jsonType(st["latencyHist"]) != "object" {
		t.Errorf("before the run: %s", body)
	}
	for _, path := range []string{"/", "/stats/", "/metrics"} {
		if code, _ := get(t, "GET", base+path); code != 404 {
			t.Errorf("GET %s: %d, want 404", path, code)
		}
	}
	for _, path := range []string{"/stats", "/healthz"} {
		if code, _ := get(t, "POST", base+path); code != 405 {
			t.Errorf("POST %s: %d, want 405", path, code)
		}
	}
}

// Every counter is cumulative: a later read never has less, so a missed read
// loses nothing.
func TestStatsAreCumulativeAcrossReads(t *testing.T) {
	c := report.NewCollector()
	c.RequestsFromCalls()
	s, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetSource(func() Stats {
		return FromResult(c.Finish("fixed-vus", time.Since(c.Start())), c.ActiveVUs(), StateRunning)
	})
	read := func() Stats {
		_, body := get(t, "GET", "http://"+s.Addr()+"/stats")
		var st Stats
		if err := json.Unmarshal([]byte(body), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	sampleRun(c)
	first := read()
	sampleRun(c)
	second := read()
	if second.TotalRequests != 2*first.TotalRequests || second.TotalFailures != 2*first.TotalFailures {
		t.Fatalf("totals %d/%d then %d/%d", first.TotalRequests, first.TotalFailures, second.TotalRequests, second.TotalFailures)
	}
	for key, r := range first.RequestStats {
		if second.RequestStats[key].NumRequests != 2*r.NumRequests {
			t.Errorf("%s: %d then %d", key, r.NumRequests, second.RequestStats[key].NumRequests)
		}
	}
	for k, n := range first.LatencyHist {
		if second.LatencyHist[k] < n {
			t.Errorf("latencyHist[%s] went down: %d then %d", k, n, second.LatencyHist[k])
		}
	}
	if second.CheckStats["status is 200"].Passes != 2 || second.DataReceivedBytes != 2*first.DataReceivedBytes {
		t.Errorf("checks %+v bytes %d", second.CheckStats, second.DataReceivedBytes)
	}

	final := read()
	s.Freeze(FromResult(c.Finish("fixed-vus", time.Second), 0, StateFinished))
	sampleRun(c) // after the freeze, /stats keeps the final numbers
	if got := read(); got.State != StateFinished || got.TotalRequests != final.TotalRequests {
		t.Fatalf("after Freeze: %s %d, want finished %d", got.State, got.TotalRequests, final.TotalRequests)
	}
}
