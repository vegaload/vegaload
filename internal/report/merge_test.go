package report

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/engine"
	"github.com/vegaload/vegaload/internal/failure"
)

// splitLoad plays one deterministic run into one collector, and the same
// run split call by call across parts collectors, as if parts machines had
// each sent a share of the load at the same time.
func splitLoad(t *testing.T, parts int, scenario bool) (*Result, []*Result) {
	t.Helper()
	start := time.Now()
	mk := func() *Collector {
		c := NewCollector()
		c.start = start
		if scenario {
			c.RequestsFromCalls()
		}
		return c
	}
	one := mk()
	split := make([]*Collector, parts)
	for i := range split {
		split[i] = mk()
	}
	rng := rand.New(rand.NewSource(42))
	ops := []string{"GET", "POST"}
	for i := 0; i < 60_000; i++ {
		part := split[rng.Intn(parts)]
		d := time.Duration(rng.ExpFloat64()*float64(40*time.Millisecond)) + 200*time.Microsecond
		var err error
		switch {
		case i%101 == 0:
			err = failure.Status(503)
		case i%257 == 0:
			err = errBoom
		}
		at := start.Add(time.Duration(rng.Int63n(int64(30 * time.Second))))
		for _, c := range []*Collector{one, part} {
			if !scenario {
				c.Record(engine.IterationResult{Start: at, Duration: d, Err: err})
			} else {
				c.Record(engine.IterationResult{Start: at, Duration: 3 * d, Err: err})
			}
			c.RecordCall("http", ops[i%2], d, err, 100, 1000)
			if i%3 == 0 {
				c.RecordCall("redis", "command", d/4, nil, 10, 20)
				c.RecordMeasure("http", "ttfb", d/2)
			}
			c.RecordStep([]string{"login", "browse", "buy"}[i%3], 2*d, err != nil)
			c.RecordCheck("status is 200", err == nil)
			if i%7 == 0 {
				c.RecordCheck("fast", d < 50*time.Millisecond)
			}
		}
	}
	const elapsed = 30 * time.Second
	var results []*Result
	for _, c := range split {
		results = append(results, c.Finish("constant-arrival-rate", elapsed))
	}
	return one.Finish("constant-arrival-rate", elapsed), results
}

// Merging N split runs gives the same result as one run of the same total
// load. Every count, percentile, min, max and mean is exactly
// equal, since the buckets add.
func TestMergeOfSplitRunsEqualsOneRun(t *testing.T) {
	for _, scenario := range []bool{false, true} {
		for _, parts := range []int{2, 3, 7} {
			one, split := splitLoad(t, parts, scenario)
			merged, err := Merge(split)
			if err != nil {
				t.Fatal(err)
			}
			if merged.MergedFrom != parts {
				t.Errorf("merged_from = %d, want %d", merged.MergedFrom, parts)
			}
			merged.MergedFrom = 0
			if scenario {
				// A call's request time is when it was recorded, so the
				// series of a scenario run is not the same in every part.
				one.TimeSeries, merged.TimeSeries = nil, nil
			}
			sortByName(one)
			sortByName(merged)
			if !reflect.DeepEqual(one, merged) {
				t.Fatalf("scenario=%v, %d parts: the merged result differs from one run:\n%s", scenario, parts, firstDiff(one, merged))
			}
			if one.Latency.P95 == 0 || len(one.Steps) != 3 || len(one.Protocols) != 2 || len(one.Checks) != 2 || len(one.FailureReasons) != 2 {
				t.Fatalf("the run is missing what this test means to compare: %+v", one)
			}
		}
	}
}

// The CLI reads each file back from JSON before it merges. That loses
// nothing a merge needs.
func TestMergeAfterAJSONRoundTrip(t *testing.T) {
	one, split := splitLoad(t, 3, false)
	var back []*Result
	for _, r := range split {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var got Result
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		back = append(back, &got)
	}
	merged, err := Merge(back)
	if err != nil {
		t.Fatal(err)
	}
	merged.MergedFrom = 0
	merged.StartedAt = one.StartedAt // JSON keeps the instant, not the monotonic clock
	sortByName(one)
	sortByName(merged)
	a, _ := json.Marshal(one)
	b, _ := json.Marshal(merged)
	if string(a) != string(b) {
		t.Fatalf("a merge of JSON copies differs from one run:\n%s", firstDiff(one, merged))
	}
}

// sortByName puts every named list in name order. Merge keeps the order
// names were first seen in, file by file, which need not be the order one
// run saw them in.
func sortByName(r *Result) {
	sort.Slice(r.Steps, func(i, j int) bool { return r.Steps[i].Name < r.Steps[j].Name })
	sort.Slice(r.Checks, func(i, j int) bool { return r.Checks[i].Name < r.Checks[j].Name })
	sort.Slice(r.Protocols, func(i, j int) bool { return r.Protocols[i].Name < r.Protocols[j].Name })
	for _, p := range r.Protocols {
		sort.Slice(p.Operations, func(i, j int) bool { return p.Operations[i].Name < p.Operations[j].Name })
		sort.Slice(p.Measures, func(i, j int) bool { return p.Measures[i].Name < p.Measures[j].Name })
	}
}

// firstDiff shows the first line where two results' indented JSON differ.
func firstDiff(a, b *Result) string {
	ja, _ := json.MarshalIndent(a, "", " ")
	jb, _ := json.MarshalIndent(b, "", " ")
	la, lb := strings.Split(string(ja), "\n"), strings.Split(string(jb), "\n")
	for i := 0; i < len(la) && i < len(lb); i++ {
		if la[i] != lb[i] {
			lo := max(0, i-6)
			return fmt.Sprintf("line %d\none:    %s\nmerged: %s\ncontext:\n%s", i, la[i], lb[i], strings.Join(la[lo:i+1], "\n"))
		}
	}
	if len(la) != len(lb) {
		return fmt.Sprintf("one has %d lines, merged %d", len(la), len(lb))
	}
	return "the JSON is the same; a field outside it differs"
}

func TestMergeLinesUpTheTimeSeriesByStart(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mk := func(startOffset time.Duration, elapsed time.Duration, perSec ...int64) *Result {
		c := NewCollector()
		c.start = base.Add(startOffset)
		for sec, n := range perSec {
			for k := int64(0); k < n; k++ {
				var err error
				if k == 0 {
					err = errBoom
				}
				c.Record(engine.IterationResult{Start: c.start.Add(time.Duration(sec) * time.Second), Duration: time.Millisecond, Err: err})
			}
		}
		c.RecordCall("http", "GET", time.Millisecond, nil, 600, 600)
		return c.Finish("fixed-vus", elapsed)
	}
	a := mk(0, 3*time.Second, 10, 10, 10)
	b := mk(1400*time.Millisecond, 4*time.Second, 5, 5, 5, 5) // rounds to one second later
	merged, err := Merge([]*Result{b, a})
	if err != nil {
		t.Fatal(err)
	}
	if !merged.StartedAt.Equal(base) || merged.Elapsed != 5400*time.Millisecond {
		t.Fatalf("started %v elapsed %v, want %v and 5.4s", merged.StartedAt, merged.Elapsed, base)
	}
	want := []int64{10, 15, 15, 5, 5}
	if len(merged.TimeSeries) != len(want) {
		t.Fatalf("series %+v", merged.TimeSeries)
	}
	for i, p := range merged.TimeSeries {
		if p.Requests != want[i] || p.Offset != time.Duration(i)*time.Second || p.RPS != float64(want[i]) {
			t.Errorf("point %d = %+v, want %d requests", i, p, want[i])
		}
	}
	if p := merged.TimeSeries[1]; p.Failed != 2 || p.ErrorRate != 2.0/15 {
		t.Errorf("point 1 = %+v, want 2 failed of 15", p)
	}
	if http, _ := merged.Protocol("http"); http.BytesPerSec != float64(2400)/merged.Elapsed.Seconds() {
		t.Errorf("bytes per second %v, want the bytes over the merged elapsed time", http.BytesPerSec)
	}
	if merged.Executor != "fixed-vus" {
		t.Errorf("executor %q", merged.Executor)
	}
}

func TestMergeDropsPerFileVerdictsAndKeepsTheRest(t *testing.T) {
	_, split := splitLoad(t, 2, false)
	passed := true
	split[0].Thresholds = []ThresholdResult{{Name: "p95 < 1ms", Passed: false}}
	split[0].ThresholdsPassed = &passed
	split[0].Baseline = &BaselineResult{Path: "base.json"}
	split[0].Aborted = &AbortInfo{Threshold: "p95 < 1ms"}
	split[0].DroppedIterations = 4
	split[1].DroppedIterations = 6
	split[0].Warnings = []string{"same", "first"}
	split[1].Warnings = []string{"same", "second"}
	split[0].DataFiles = []DataFile{{Name: "users.csv", Rows: 3}}
	split[1].DataFiles = []DataFile{{Name: "users.csv", Rows: 3}, {Name: "items.csv", Rows: 9}}
	split[1].Executor = "ramping-arrival-rate"

	merged, err := Merge(split)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Thresholds != nil || merged.ThresholdsPassed != nil || merged.Baseline != nil || merged.Aborted != nil {
		t.Errorf("a per-file verdict was carried over: %+v %v %+v %+v", merged.Thresholds, merged.ThresholdsPassed, merged.Baseline, merged.Aborted)
	}
	if merged.DroppedIterations != 10 {
		t.Errorf("dropped %d, want 10", merged.DroppedIterations)
	}
	if len(merged.Warnings) != 4 || merged.Warnings[0] != "same" || merged.Warnings[2] != "second" || !strings.Contains(merged.Warnings[3], "1 of the 2 merged reports were stopped early") {
		t.Errorf("warnings %q", merged.Warnings)
	}
	if len(merged.DataFiles) != 2 {
		t.Errorf("data files %+v", merged.DataFiles)
	}
	if merged.Executor != "constant-arrival-rate,ramping-arrival-rate" {
		t.Errorf("executor %q", merged.Executor)
	}
	if merged.Schema != Schema {
		t.Errorf("schema %q", merged.Schema)
	}

	// A merged result merges again, and counts every run it came from.
	again, err := Merge([]*Result{merged, split[0]})
	if err != nil || again.MergedFrom != 3 || again.Total != merged.Total+split[0].Total {
		t.Fatalf("merging a merged result: %v, merged_from %d", err, again.MergedFrom)
	}
}

func TestMergeRefusesAReportWithoutHistograms(t *testing.T) {
	_, split := splitLoad(t, 2, false)
	old := *split[1]
	old.Schema = ""
	if _, err := Merge([]*Result{split[0], &old}); err == nil || !strings.Contains(err.Error(), "result 2") || !strings.Contains(err.Error(), "without latency histograms") {
		t.Fatalf("err = %v", err)
	}

	stripped := *split[1]
	stripped.Steps = append([]StepResult(nil), split[1].Steps...)
	stripped.Steps[0].Latency.Histogram = nil
	if err := Mergeable(&stripped); err == nil || !strings.Contains(err.Error(), "step login") {
		t.Fatalf("a step without a histogram: %v", err)
	}
	future := *split[1]
	future.Schema = "vegaload.io/report/v9"
	if err := Mergeable(&future); err == nil {
		t.Fatal("an unknown schema was accepted")
	}
	if _, err := Merge(nil); err == nil {
		t.Fatal("merged nothing")
	}

	// A report from before histograms still loads: compare and -baseline read it.
	var legacy Result
	raw := `{"executor":"fixed-vus","total":3,"failed":0,"error_rate":0,"latency":{"min_ns":1,"mean_ns":2,"max_ns":3,"p50_ns":2,"p90_ns":3,"p95_ns":3,"p99_ns":3},"time_series":[]}`
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil || legacy.Latency.P95 != 3 || legacy.Latency.Histogram != nil {
		t.Fatalf("legacy report: %v %+v", err, legacy.Latency)
	}
	if Mergeable(&legacy) == nil {
		t.Fatal("a legacy report was mergeable")
	}
}

func TestFinishWritesTheSchemaAndHistograms(t *testing.T) {
	c := NewCollector()
	c.Record(engine.IterationResult{Start: c.start, Duration: 3 * time.Millisecond})
	raw, err := json.Marshal(c.Finish("fixed-vus", time.Second))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, `"schema":"vegaload.io/report/v2"`) || !strings.Contains(s, `"histogram":{"base_ms":0.5,"growth":1.05,"count":1,`) {
		t.Fatalf("JSON = %s", s)
	}
	if strings.Contains(s, "merged_from") {
		t.Fatalf("a single run has merged_from: %s", s)
	}
}
