package report

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/engine"
)

// NFR-03: local HTML report generation completes within 5 seconds of run
// completion for runs producing up to 1,000,000 requests.
//
// "Run completion" is the moment the last request finishes. From there
// the work left is turning the recorded requests into a Result (the
// percentiles and the per-second series), and writing the HTML report
// and the JSON summary. This test records 1,000,000 requests the way a
// run does, then times exactly that work. It fails if it takes longer
// than the PRD's limit, so the promise cannot quietly stop being true.
const (
	nfr03Requests = 1_000_000
	nfr03Limit    = 5 * time.Second
)

func TestNFR03_ReportFor1MRequestsWithin5Seconds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the 1,000,000-request report check in -short mode")
	}

	c := NewCollector()
	// Spread the requests over a 5-minute run, with latencies that vary
	// the way real ones do, and a few failures. The values are
	// deterministic so a failure is reproducible.
	const runLength = 5 * time.Minute
	seed := uint64(88172645463325252)
	for i := 0; i < nfr03Requests; i++ {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		dur := time.Duration(1+seed%400) * time.Millisecond
		var err error
		if seed%97 == 0 {
			err = os.ErrDeadlineExceeded
		}
		c.Record(engine.IterationResult{
			VUID:     i % 100,
			Start:    c.start.Add(runLength * time.Duration(i) / nfr03Requests),
			Duration: dur,
			Err:      err,
		})
	}

	dir := t.TempDir()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	begin := time.Now()
	res := c.Finish("fixed-vus", runLength)
	finished := time.Since(begin)
	if err := WriteHTML(filepath.Join(dir, "report.html"), res); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	if err := WriteJSON(filepath.Join(dir, "report.json"), res); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	total := time.Since(begin)

	runtime.ReadMemStats(&after)
	t.Logf("1,000,000 requests: Finish took %s, Finish + HTML + JSON took %s (limit %s); heap in use %d MB, %d MB allocated while finishing",
		finished.Round(time.Millisecond), total.Round(time.Millisecond), nfr03Limit,
		after.HeapInuse>>20, (after.TotalAlloc-before.TotalAlloc)>>20)

	if res.Total != nfr03Requests {
		t.Fatalf("Total = %d, want %d", res.Total, nfr03Requests)
	}
	if total > nfr03Limit {
		t.Errorf("NFR-03: report generation for %d requests took %s, the PRD limit is %s", nfr03Requests, total, nfr03Limit)
	}

	// The HTML report must stay small even for a run this large: the
	// per-second series is what it embeds, not the requests themselves.
	info, err := os.Stat(filepath.Join(dir, "report.html"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 2<<20 {
		t.Errorf("the HTML report for %d requests is %d bytes; it should stay well under 2 MB", nfr03Requests, info.Size())
	}
}

// NFR-03 for a scenario run, whose requests are its calls: 1,000,000 calls
// in 250,000 iterations, recorded through RecordCall, still report within
// the limit.
func TestNFR03_ScenarioReportFor1MCallsWithin5Seconds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the 1,000,000-call report check in -short mode")
	}
	c := NewCollector()
	c.RequestsFromCalls()
	for i := 0; i < nfr03Requests; i++ {
		var err error
		if i%97 == 0 {
			err = os.ErrDeadlineExceeded
		}
		c.RecordCall("http", "GET", time.Duration(1+i%400)*time.Millisecond, err, 10, 100)
		if i%4 == 3 {
			c.Record(engine.IterationResult{Start: c.start, Duration: time.Duration(4+i%400) * time.Millisecond})
		}
	}

	dir := t.TempDir()
	begin := time.Now()
	res := c.Finish("fixed-vus", 5*time.Minute)
	if err := WriteHTML(filepath.Join(dir, "report.html"), res); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(dir, "report.json"), res); err != nil {
		t.Fatal(err)
	}
	took := time.Since(begin)
	t.Logf("1,000,000 calls: Finish + HTML + JSON took %s (limit %s)", took.Round(time.Millisecond), nfr03Limit)

	if res.Total != nfr03Requests || res.Iterations == nil || res.Iterations.Total != nfr03Requests/4 {
		t.Fatalf("total %d iterations %+v", res.Total, res.Iterations)
	}
	if res.FailureReasons[0].Reason != "timeout" || res.FailureReasons[0].Count != res.Failed {
		t.Errorf("FailureReasons = %+v, failed %d", res.FailureReasons, res.Failed)
	}
	if took > nfr03Limit {
		t.Errorf("NFR-03: report generation for %d calls took %s, the PRD limit is %s", nfr03Requests, took, nfr03Limit)
	}
}

// heapAfterGC is the live heap, in bytes, once garbage is collected.
func heapAfterGC() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// Memory stays flat as the request count grows. A run of
// 10,000,000 requests holds about what it held after 1,000,000: every
// latency is in a histogram and every count in a counter. The per-second
// counters grow with a run's length, not its requests, so the requests
// here all start inside the same ten minutes.
func TestMemoryIsFlatFrom1MTo10MRequests(t *testing.T) {
	const (
		first = 1_000_000
		total = 10_000_000
		// The bound is a few histograms' worth: the buckets of the slowest
		// latencies seen so far, a map entry or two, and GC noise.
		bound = 512 << 10
	)
	ops := []string{"connect", "download", "upload", "list", "stat"}
	steps := []string{"login", "browse", "checkout"}

	plain := NewCollector()
	scenario := NewCollector()
	scenario.RequestsFromCalls()
	lb := scenario.LiveBuckets()
	record := func(from, to int) {
		for i := from; i < to; i++ {
			d := time.Duration(1+(i*7919)%2000) * 100 * time.Microsecond
			var err error
			if i%97 == 0 {
				err = os.ErrDeadlineExceeded
			}
			start := plain.start.Add(time.Duration(i%600) * time.Second)
			plain.Record(engine.IterationResult{Start: start, Duration: d, Err: err})
			scenario.RecordCall("ftp", ops[i%len(ops)], d, err, 10, 100)
			if i%5 == 4 {
				scenario.RecordMeasure("ftp", "connect_time", d/3)
				scenario.RecordStep(steps[i%len(steps)], 5*d, err != nil)
				scenario.Record(engine.IterationResult{Start: start, Duration: 5 * d})
			}
		}
		for {
			if _, ok := lb.Next(time.Since(scenario.start)+time.Hour, 0); !ok {
				break
			}
		}
	}

	record(0, first)
	at1M := heapAfterGC()
	record(first, total)
	at10M := heapAfterGC()
	runtime.KeepAlive(plain)
	runtime.KeepAlive(scenario)

	growth := int64(at10M) - int64(at1M)
	t.Logf("live heap after 1,000,000 requests: %d KB; after 10,000,000: %d KB (grew %d KB, bound %d KB)",
		at1M>>10, at10M>>10, growth>>10, bound>>10)
	if growth > bound {
		t.Errorf("the heap grew %d KB from 1M to 10M requests; it should stay flat (bound %d KB)", growth>>10, bound>>10)
	}

	res := plain.Finish("fixed-vus", 10*time.Minute)
	if res.Total != total || len(res.TimeSeries) != 600 {
		t.Fatalf("plain run: total %d, %d series points", res.Total, len(res.TimeSeries))
	}
	sres := scenario.Finish("fixed-vus", 10*time.Minute)
	if sres.Total != total || sres.Iterations.Total != total/5 || len(sres.Steps) != 3 {
		t.Fatalf("scenario run: total %d iterations %d steps %d", sres.Total, sres.Iterations.Total, len(sres.Steps))
	}
	if p, ok := sres.Protocol("ftp"); !ok || p.Total != total || len(p.Operations) != 5 || p.Measures[0].Count != total/5 {
		t.Fatalf("ftp = %+v", p)
	}
}

// In exact mode the collector keeps every latency, so memory grows with
// the requests. This is the cost -exact-percentiles documents.
func TestExactModeKeepsEveryLatency(t *testing.T) {
	c := NewCollectorWith(Options{Exact: true})
	before := heapAfterGC()
	for i := 0; i < 1_000_000; i++ {
		c.Record(engine.IterationResult{Start: c.start, Duration: time.Duration(1+i%400) * time.Millisecond})
	}
	grew := int64(heapAfterGC()) - int64(before)
	runtime.KeepAlive(c)
	if grew < 8_000_000 {
		t.Fatalf("exact mode grew the heap by only %d bytes for 1,000,000 latencies", grew)
	}
}

// BenchmarkFinish1M gives the same work as a number to compare between
// changes: go test -bench Finish1M -run '^$' ./internal/report
func BenchmarkFinish1M(b *testing.B) {
	c := NewCollector()
	const runLength = 5 * time.Minute
	for i := 0; i < nfr03Requests; i++ {
		c.Record(engine.IterationResult{
			Start:    c.start.Add(runLength * time.Duration(i) / nfr03Requests),
			Duration: time.Duration(1+i%400) * time.Millisecond,
		})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Finish("fixed-vus", runLength)
	}
}
