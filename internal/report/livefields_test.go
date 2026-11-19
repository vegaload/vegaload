package report

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/engine"
	"github.com/vegaload/vegaload/internal/failure"
)

// liveSecond plays one controlled second of a scenario run into c: three VUs
// start and one stops, two iterations start and end, one arrival is dropped,
// three calls (one failing on its status, one timing out), one TTFB, two
// checks and a step.
func liveSecond(c *Collector) {
	c.VUStarted()
	c.VUStarted()
	c.VUStarted()
	c.VUStopped()
	at := c.start.Add(100 * time.Millisecond)
	c.Begin(at)
	c.Begin(at)
	c.RecordCall("http", "GET /a", 10*time.Millisecond, nil, 100, 1000)
	c.RecordCall("http", "GET /a", 20*time.Millisecond, fmt.Errorf("http: %w", failure.Status(503)), 100, 50)
	c.RecordCall("redis", "command", 5*time.Millisecond, fmt.Errorf("redis: %w", context.DeadlineExceeded), 7, 0)
	c.RecordMeasure("http", TTFBMeasure, 4*time.Millisecond)
	c.RecordCheck("status is 200", true)
	c.RecordCheck("body has id", false)
	c.RecordStep("login", 30*time.Millisecond, false)
	c.Record(engine.IterationResult{Start: at, Duration: 40 * time.Millisecond})
	c.Record(engine.IterationResult{Start: at, Duration: 40 * time.Millisecond, Err: errLive})
	c.End(at)
	c.End(at)
	c.Record(engine.IterationResult{VUID: -1, Start: at, Dropped: true})
}

func TestLivePointCarriesTheFullSecond(t *testing.T) {
	c := NewCollector()
	c.RequestsFromCalls()
	c.SetPlannedVUs(5)
	lb := c.LiveBuckets()
	liveSecond(c)

	b, ok := lb.Next(2*time.Second, 0)
	if !ok {
		t.Fatal("second 0 was not handed out")
	}
	if b.Requests != 3 || b.Failed != 2 || b.Total != 3 || b.TotalFailed != 2 {
		t.Fatalf("old fields: %+v", b)
	}
	if b.ActiveVUs != 3 || b.MaxVUs != 5 {
		t.Errorf("vus: active=%d max=%d, want 3 and 5", b.ActiveVUs, b.MaxVUs)
	}
	if b.IterationsStarted != 2 || b.IterationsCompleted != 2 || b.IterationsDropped != 1 {
		t.Errorf("iterations: %d/%d/%d", b.IterationsStarted, b.IterationsCompleted, b.IterationsDropped)
	}
	if b.IterationsStartedTotal != 2 || b.IterationsCompletedTotal != 2 || b.IterationsDroppedTotal != 1 {
		t.Errorf("iteration totals: %d/%d/%d", b.IterationsStartedTotal, b.IterationsCompletedTotal, b.IterationsDroppedTotal)
	}
	if b.Min != 5*time.Millisecond || b.Max != 20*time.Millisecond || b.Mean == 0 || b.P90 == 0 {
		t.Errorf("latency: min=%v mean=%v max=%v p90=%v", b.Min, b.Mean, b.Max, b.P90)
	}
	if b.BytesSent != 207 || b.BytesReceived != 1050 || b.BytesSentTotal != 207 || b.BytesReceivedTotal != 1050 {
		t.Errorf("bytes: %d/%d totals %d/%d", b.BytesSent, b.BytesReceived, b.BytesSentTotal, b.BytesReceivedTotal)
	}
	if b.TTFBCount != 1 || b.TTFBP50 != 4*time.Millisecond || b.TTFBP95 != 4*time.Millisecond {
		t.Errorf("ttfb: n=%d p50=%v p95=%v", b.TTFBCount, b.TTFBP50, b.TTFBP95)
	}
	if b.ChecksPassed != 1 || b.ChecksFailed != 1 || b.ChecksPassedTotal != 1 || b.ChecksFailedTotal != 1 {
		t.Errorf("checks: %+v", b)
	}
	if len(b.Steps) != 1 || b.Steps[0] != (BucketStep{Name: "login", Count: 1, P95: 30 * time.Millisecond}) {
		t.Errorf("steps: %+v", b.Steps)
	}
	if len(b.Rows) != 2 {
		t.Fatalf("rows: %+v", b.Rows)
	}
	get := b.Rows[0]
	if get.Protocol != "http" || get.Name != "GET /a" || get.Method != "GET" || get.Endpoint != "/a" || get.Count != 2 || get.Failed != 1 || get.P99 != 20*time.Millisecond {
		t.Errorf("first row: %+v", get)
	}
	if r := b.Rows[1]; r.Protocol != "redis" || r.Name != "command" || r.Method != "" || r.Count != 1 {
		t.Errorf("second row: %+v", r)
	}
	want := map[string]int64{"status 503": 1, failure.Timeout: 1, failure.CheckFailed: 1}
	if fmt.Sprint(b.Errors) != fmt.Sprint(want) {
		t.Errorf("errors: %v, want %v", b.Errors, want)
	}
	if len(c.live) != 0 {
		t.Errorf("the second's live state was kept: %d", len(c.live))
	}
}

// A second in which no VU starts or stops has the VUs the last one ended with.
func TestLivePointCarriesTheVUCount(t *testing.T) {
	c := NewCollector()
	lb := c.LiveBuckets()
	c.VUStarted()
	c.VUStarted()
	c.VUStopped()
	c.VUStarted()
	c.VUStopped()
	for sec := 0; sec < 3; sec++ {
		c.Record(engine.IterationResult{Start: c.start.Add(time.Duration(sec) * time.Second), Duration: time.Millisecond})
	}
	var got []int64
	for _, b := range lb.Rest() {
		got = append(got, b.ActiveVUs)
	}
	if fmt.Sprint(got) != "[2 1 1]" {
		t.Fatalf("active VUs by second = %v, want [2 1 1]", got)
	}
	if c.ActiveVUs() != 1 || c.PeakVUs() != 2 || c.MaxVUs() != 2 {
		t.Fatalf("active=%d peak=%d max=%d", c.ActiveVUs(), c.PeakVUs(), c.MaxVUs())
	}
}

// A point carries the MaxPointRows busiest rows, most calls first.
func TestLivePointCapsItsRows(t *testing.T) {
	c := NewCollector()
	c.RequestsFromCalls()
	lb := c.LiveBuckets()
	for i := 0; i < MaxPointRows+20; i++ {
		for k := 0; k <= i%3; k++ {
			c.RecordCall("http", fmt.Sprintf("GET /r%d", i), time.Millisecond, nil, 0, 0)
		}
	}
	b := lb.Rest()[0]
	if len(b.Rows) != MaxPointRows {
		t.Fatalf("%d rows, want %d", len(b.Rows), MaxPointRows)
	}
	for i := 1; i < len(b.Rows); i++ {
		if b.Rows[i].Count > b.Rows[i-1].Count {
			t.Fatalf("rows not most first: %+v then %+v", b.Rows[i-1], b.Rows[i])
		}
	}
	if b.Rows[0].Count != 3 || b.Requests != int64(c.liveTotal.Load()) {
		t.Fatalf("first row %+v, requests %d", b.Rows[0], b.Requests)
	}
}

func TestFinishCarriesTheNewTotals(t *testing.T) {
	c := NewCollector()
	c.RequestsFromCalls()
	c.SetPlannedVUs(5)
	liveSecond(c)
	c.Begin(c.start) // cut off by the end of the run: started, never completed
	res := c.Finish("fixed-vus", time.Second)

	if res.BytesSent != 207 || res.BytesReceived != 1050 {
		t.Errorf("bytes: %d/%d", res.BytesSent, res.BytesReceived)
	}
	if res.TTFB == nil || res.TTFB.P50 != 4*time.Millisecond || res.TTFB.Histogram.Count() != 1 {
		t.Errorf("ttfb: %+v", res.TTFB)
	}
	if res.VUsPeak != 3 || res.VUsMax != 5 {
		t.Errorf("vus: peak=%d max=%d", res.VUsPeak, res.VUsMax)
	}
	if res.IterationsStarted != 3 || res.IterationsCompleted != 2 || res.DroppedIterations != 1 {
		t.Errorf("iterations: started=%d completed=%d dropped=%d", res.IterationsStarted, res.IterationsCompleted, res.DroppedIterations)
	}
	want := []ErrorKind{
		{Kind: failure.CheckFailed, Count: 1, Sample: "body has id"},
		{Kind: "status 503", Count: 1, Sample: "http: status 503"},
		{Kind: failure.Timeout, Count: 1, Sample: "redis: context deadline exceeded"},
	}
	if fmt.Sprint(res.Errors) != fmt.Sprint(want) {
		t.Errorf("errors:\n got %+v\nwant %+v", res.Errors, want)
	}
	http, _ := res.Protocol("http")
	row, _ := http.Operation("GET /a")
	if len(row.Errors) != 1 || row.Errors[0] != (ErrorKind{Kind: "status 503", Count: 1, Sample: "http: status 503"}) {
		t.Errorf("row errors: %+v", row.Errors)
	}
	if ok, _ := http.Operation("GET /a"); ok.Latency.P90 == 0 || ok.Latency.Min == 0 || ok.Latency.Mean == 0 {
		t.Errorf("a row lacks its latency figures: %+v", ok.Latency)
	}
	// No cursor: the live state is never kept.
	if c.live != nil {
		t.Errorf("kept live state with no cursor: %d", len(c.live))
	}
}

func TestViewsShowTheNewNumbers(t *testing.T) {
	c := NewCollector()
	c.RequestsFromCalls()
	c.SetPlannedVUs(5)
	liveSecond(c)
	res := c.Finish("fixed-vus", time.Second)

	page := renderHTML(res)
	md := MarkdownSummary(res)
	for _, want := range []string{"peak / max VUs", "iterations started / completed", "sent / received", "TTFB p50 / p95", "Errors by kind", "redis: context deadline exceeded", "body has id"} {
		if !strings.Contains(page, want) {
			t.Errorf("HTML lacks %q", want)
		}
	}
	for _, want := range []string{"**Load:** peak 3 of 5 VUs; 2 iterations started, 2 completed; 207 B sent, 1.0 KiB received; TTFB p50 4ms", "### Errors by kind", "| status 503 | 1 | http: status 503 |"} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown lacks %q:\n%s", want, md)
		}
	}
}

// A failure keeps one sample per kind, and counts the rest.
func TestErrorKindsKeepTheFirstSample(t *testing.T) {
	c := NewCollector()
	for i := 0; i < 5; i++ {
		c.Record(engine.IterationResult{Start: c.start, Duration: time.Millisecond, Err: fmt.Errorf("call %d: %w", i, context.DeadlineExceeded)})
	}
	res := c.Finish("fixed-vus", time.Second)
	if len(res.Errors) != 1 || res.Errors[0] != (ErrorKind{Kind: failure.Timeout, Count: 5, Sample: "call 0: context deadline exceeded"}) {
		t.Fatalf("errors: %+v", res.Errors)
	}
}
