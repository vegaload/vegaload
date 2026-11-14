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

// TestCallSamples_1MIterations5Calls checks the memory of the packed call
// record. 1,000,000 iterations of 5 calls is 5,000,000 uint64 samples,
// 40 MB of length. Slice capacity adds some overhead; the test logs the
// heap so a review can see the number.
func TestCallSamples_1MIterations5Calls(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the call-sample memory check in -short mode")
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	c := NewCollector()
	ops := []string{"connect", "download", "upload", "list", "stat"}
	for i := 0; i < nfr03Requests; i++ {
		for _, op := range ops {
			c.RecordCall("ftp", op, time.Millisecond, nil, 1, 1)
		}
	}
	packed := c.callSampleBytes()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	heap := after.HeapInuse - before.HeapInuse
	t.Logf("5,000,000 calls: packed samples %d MB, heap in use grew %d MB", packed>>20, heap>>20)

	const want = nfr03Requests * 5 * 8
	if packed != want {
		t.Fatalf("packed sample bytes = %d, want %d", packed, want)
	}
	res := c.Finish("fixed-vus", time.Second)
	p, ok := res.Protocol("ftp")
	if !ok || p.Total != nfr03Requests*5 || len(p.Operations) != 5 {
		t.Fatalf("protocol = %+v ok %v", p.Name, ok)
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
