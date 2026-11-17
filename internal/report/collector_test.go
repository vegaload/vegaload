package report

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/engine"
	"github.com/vegaload/vegaload/internal/failure"
)

func TestCollector_FinishEmpty(t *testing.T) {
	c := NewCollector()
	res := c.Finish("fixed-vus", 0)
	if res.Total != 0 {
		t.Errorf("Total = %d, want 0", res.Total)
	}
	if len(res.TimeSeries) != 0 {
		t.Errorf("TimeSeries should be empty for a run with no samples")
	}
}

func TestCollector_FinishComputesLatencyAndErrorRate(t *testing.T) {
	c := NewCollector()
	start := c.start

	// Five iterations, all in bucket 0 (within the first second), one
	// of which fails. Durations: 10ms,20ms,30ms,40ms,50ms (sorted).
	durs := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond, 40 * time.Millisecond, 50 * time.Millisecond}
	for i, d := range durs {
		var err error
		if i == len(durs)-1 {
			err = errBoom
		}
		c.Record(engine.IterationResult{VUID: i, Start: start, Duration: d, Err: err})
	}

	res := c.Finish("fixed-vus", 100*time.Millisecond)
	if res.Total != 5 {
		t.Fatalf("Total = %d, want 5", res.Total)
	}
	if res.Failed != 1 {
		t.Errorf("Failed = %d, want 1", res.Failed)
	}
	if res.ErrorRate != 0.2 {
		t.Errorf("ErrorRate = %v, want 0.2", res.ErrorRate)
	}
	if res.Latency.Min != 10*time.Millisecond {
		t.Errorf("Latency.Min = %v, want 10ms", res.Latency.Min)
	}
	if res.Latency.Max != 50*time.Millisecond {
		t.Errorf("Latency.Max = %v, want 50ms", res.Latency.Max)
	}
	if len(res.TimeSeries) != 1 {
		t.Fatalf("len(TimeSeries) = %d, want 1 (all samples in bucket 0)", len(res.TimeSeries))
	}
	if res.TimeSeries[0].Requests != 5 || res.TimeSeries[0].Failed != 1 {
		t.Errorf("TimeSeries[0] = %+v, want {Requests:5 Failed:1 ...}", res.TimeSeries[0])
	}
}

func TestCollector_FinishBucketsByOffset(t *testing.T) {
	c := NewCollector()
	start := c.start

	c.Record(engine.IterationResult{Start: start, Duration: time.Millisecond})
	c.Record(engine.IterationResult{Start: start.Add(1500 * time.Millisecond), Duration: time.Millisecond})
	c.Record(engine.IterationResult{Start: start.Add(3200 * time.Millisecond), Duration: time.Millisecond})

	res := c.Finish("fixed-vus", 4*time.Second)
	// Samples land in buckets 0, 1, and 3 — bucket 2 has zero requests
	// but must still appear (dense series), per buildSeries's doc comment.
	if len(res.TimeSeries) != 4 {
		t.Fatalf("len(TimeSeries) = %d, want 4 (dense through the last non-empty bucket)", len(res.TimeSeries))
	}
	if res.TimeSeries[2].Requests != 0 {
		t.Errorf("TimeSeries[2].Requests = %d, want 0 (empty bucket)", res.TimeSeries[2].Requests)
	}
	if res.TimeSeries[3].Requests != 1 {
		t.Errorf("TimeSeries[3].Requests = %d, want 1", res.TimeSeries[3].Requests)
	}
}

func TestCollector_DropsAreNotRequests(t *testing.T) {
	c := NewCollector()
	start := c.start
	for i := 0; i < 4; i++ {
		c.Record(engine.IterationResult{VUID: i, Start: start, Duration: 20 * time.Millisecond})
	}
	for i := 0; i < 6; i++ {
		c.Record(engine.IterationResult{VUID: -1, Start: start, Dropped: true})
	}

	res := c.Finish("constant-arrival-rate", time.Second)
	if res.Total != 4 || res.Failed != 0 || res.ErrorRate != 0 {
		t.Errorf("total %d failed %d error rate %v, want 4, 0, 0: a drop is not a request", res.Total, res.Failed, res.ErrorRate)
	}
	if res.Latency.Min != 20*time.Millisecond || res.Latency.P50 != 20*time.Millisecond {
		t.Errorf("latency = %+v, want every percentile at 20ms: a drop has no latency", res.Latency)
	}
	if res.DroppedIterations != 6 {
		t.Errorf("DroppedIterations = %d, want 6", res.DroppedIterations)
	}
	if res.TimeSeries[0].Requests != 4 {
		t.Errorf("TimeSeries[0].Requests = %d, want 4", res.TimeSeries[0].Requests)
	}
	if total, failed := c.Counts(); total != 4 || failed != 0 {
		t.Errorf("Counts() = %d, %d, want 4, 0", total, failed)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "6 of 10 planned iterations (60.00%) were dropped") {
		t.Errorf("Warnings = %q, want the drop warning", res.Warnings)
	}
	if res.Iterations != nil {
		t.Errorf("Iterations = %+v, want nil outside a scenario run", res.Iterations)
	}
}

func TestCollector_DropWarningAboveOnePercent(t *testing.T) {
	for _, c := range []struct {
		ran, dropped int64
		warn         bool
	}{
		{100, 0, false},
		{99, 1, false},    // exactly 1% of 100 planned
		{1000, 10, false}, // 0.99% of 1010 planned
		{990, 11, true},   // 1.1% of 1001 planned
		{0, 5, true},      // nothing ran at all
	} {
		if got := dropWarning(c.ran, c.dropped) != ""; got != c.warn {
			t.Errorf("dropWarning(ran %d, dropped %d) warned %v, want %v", c.ran, c.dropped, got, c.warn)
		}
	}
}

func TestCollector_FailureReasons(t *testing.T) {
	c := NewCollector()
	start := c.start
	add := func(n int, err error) {
		for i := 0; i < n; i++ {
			c.Record(engine.IterationResult{Start: start, Duration: time.Millisecond, Err: err})
		}
	}
	add(5, nil)
	add(3, failure.Status(503))
	add(2, fmt.Errorf("http1: %w", context.DeadlineExceeded))
	add(2, errBoom)

	res := c.Finish("fixed-vus", time.Second)
	want := []FailureReason{{"status 503", 3}, {"error", 2}, {"timeout", 2}}
	if !reflect.DeepEqual(res.FailureReasons, want) {
		t.Errorf("FailureReasons = %+v, want %+v (most first, then by name)", res.FailureReasons, want)
	}
	var sum int64
	for _, r := range res.FailureReasons {
		sum += r.Count
	}
	if sum != res.Failed {
		t.Errorf("reasons add up to %d, want Failed = %d", sum, res.Failed)
	}
}

func TestCollector_TimeoutKeepsItsRealTime(t *testing.T) {
	c := NewCollector()
	c.Record(engine.IterationResult{Start: c.start, Duration: 250 * time.Millisecond, Err: context.DeadlineExceeded})
	res := c.Finish("fixed-vus", time.Second)
	if res.Failed != 1 || res.Latency.Max != 250*time.Millisecond {
		t.Errorf("failed %d max %s, want 1 failure at 250ms", res.Failed, res.Latency.Max)
	}
	if len(res.FailureReasons) != 1 || res.FailureReasons[0].Reason != "timeout" {
		t.Errorf("FailureReasons = %+v, want one timeout", res.FailureReasons)
	}
}

func TestCollector_RequestsFromCalls(t *testing.T) {
	c := NewCollector()
	c.RequestsFromCalls()
	start := c.start

	// Two iterations of three calls each. The iteration takes the sum of
	// its calls; the request latency is the latency of one call.
	for it := 0; it < 2; it++ {
		c.RecordCall("http", "GET", 10*time.Millisecond, nil, 0, 2)
		c.RecordCall("http", "GET", 20*time.Millisecond, failure.Status(500), 0, 2)
		c.RecordCall("redis", "command", 30*time.Millisecond, nil, 3, 4)
		c.Record(engine.IterationResult{Start: start, Duration: 60 * time.Millisecond})
	}
	c.Record(engine.IterationResult{Start: start, Duration: 5 * time.Millisecond, Err: errBoom})
	c.Record(engine.IterationResult{VUID: -1, Start: start, Dropped: true})

	res := c.Finish("fixed-vus", time.Second)
	if res.Total != 6 || res.Failed != 2 {
		t.Fatalf("total %d failed %d, want 6 calls and 2 failed", res.Total, res.Failed)
	}
	if res.Latency.Min != 10*time.Millisecond || res.Latency.Max != 30*time.Millisecond || res.Latency.Mean != 20*time.Millisecond {
		t.Errorf("latency = %+v, want the per-call 10ms..30ms, mean 20ms", res.Latency)
	}
	if want := []FailureReason{{"status 500", 2}}; !reflect.DeepEqual(res.FailureReasons, want) {
		t.Errorf("FailureReasons = %+v, want %+v", res.FailureReasons, want)
	}
	it := res.Iterations
	if it == nil {
		t.Fatal("Iterations is nil for a run whose requests are its calls")
	}
	if it.Total != 3 || it.Failed != 1 || it.Duration.Max != 60*time.Millisecond || it.Duration.Min != 5*time.Millisecond {
		t.Errorf("Iterations = %+v, want 3 iterations, 1 failed, 5ms..60ms", it)
	}
	if res.DroppedIterations != 1 {
		t.Errorf("DroppedIterations = %d, want 1", res.DroppedIterations)
	}
	if len(res.TimeSeries) != 1 || res.TimeSeries[0].Requests != 6 || res.TimeSeries[0].Failed != 2 {
		t.Errorf("TimeSeries = %+v, want one second with the 6 calls", res.TimeSeries)
	}
	if total, failed := c.Counts(); total != 6 || failed != 2 {
		t.Errorf("Counts() = %d, %d, want 6, 2", total, failed)
	}
	// The protocol breakdown still has every call.
	if p, _ := res.Protocol("http"); p.Total != 4 || p.Failed != 2 {
		t.Errorf("http protocol = %+v, want 4 calls, 2 failed", p)
	}
	// The live points read the same calls as the final series.
	var live int64
	for _, b := range c.LiveBuckets().Rest() {
		live += b.Requests
	}
	if live != res.Total {
		t.Errorf("live buckets have %d requests, want %d", live, res.Total)
	}
}

func TestCollector_NewFieldsOmittedWhenEmpty(t *testing.T) {
	c := NewCollector()
	c.Record(engine.IterationResult{Start: c.start, Duration: time.Millisecond})
	raw, err := json.Marshal(c.Finish("fixed-vus", time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"failure_reasons", "iterations", "dropped_iterations", "warnings"} {
		if jsonHasKey(raw, key) {
			t.Errorf("a clean -protocol run has %q: %s", key, raw)
		}
	}
}

func errIf(failed bool) error {
	if failed {
		return errBoom
	}
	return nil
}

type testErr string

func (e testErr) Error() string { return string(e) }

const errBoom = testErr("boom")
