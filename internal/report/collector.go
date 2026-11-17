// Package report implements FR-RPT-01 through 04 and FR-CLI-05: turning
// a run's recorded iterations into the structured data a self-contained
// HTML report, a JSON summary, and (eventually) the MCP get_results
// tool all read from. It implements engine.Recorder, exactly the seam
// internal/engine's own doc comment on Recorder sets aside for it, and
// knows nothing about protocol drivers, scripting, or the CLI. A
// protocol call arrives as a name, the same way a step does.
package report

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vegaload/vegaload/internal/engine"
	"github.com/vegaload/vegaload/internal/failure"
	"github.com/vegaload/vegaload/internal/secrets"
)

// sample is the minimal per-request record Collector keeps. Keeping
// only what percentile and time-bucket computation need (not the full
// engine.IterationResult) keeps memory bounded even at NFR-03's
// 1,000,000-request scale: roughly 32 bytes/sample, ~32MB at that size.
type sample struct {
	offset time.Duration // time since the run started
	dur    time.Duration
	failed bool
}

// Collector implements engine.Recorder, accumulating every request of a
// run so Finish can compute the percentiles and time-bucketed series a
// Result needs. A request is one iteration, or, for a run that called
// RequestsFromCalls, one protocol call. It is safe for concurrent use —
// Run calls Record from every VU goroutine at once.
type Collector struct {
	mu      sync.Mutex
	start   time.Time
	samples []sample
	// reasons counts the failed requests in samples by failure.Reason. It
	// is guarded by mu, so the two always agree.
	reasons map[string]int64
	// maxBucket is the highest one-second bucket any sample started in, or
	// -1 before the first sample. It is the last index Finish's time series
	// has, which lets a live view know which buckets are left at the end
	// without scanning every sample (see LiveBuckets).
	maxBucket int64

	// callsAreRequests is set by RequestsFromCalls. Each recorded call is
	// then a request, and each iteration goes to iterations instead.
	callsAreRequests atomic.Bool
	// iterations holds every iteration of such a run, guarded by iterMu
	// and packed like a step.
	iterMu     sync.Mutex
	iterations []uint64
	// ran counts the iterations that ran, in either mode, and dropped the
	// arrivals that found every VU busy. Their sum is the planned load.
	ran     atomic.Int64
	dropped atomic.Int64

	// tracking is set once a live cursor exists. Only then are the starts of
	// running iterations kept (inflight, guarded by mu, counts them by the
	// bucket they started in), so a run with no live view pays nothing for it.
	tracking atomic.Bool
	inflight map[int64]int

	// liveTotal/liveFailed mirror the counts in samples but are safe to
	// read cheaply (no lock, no copy) while a run is still in progress —
	// cmd/vegaload's -output jsonl mode polls these for periodic
	// "progress" events (see run.go) without contending with Record on
	// every one of potentially a million iterations.
	liveTotal  atomic.Int64
	liveFailed atomic.Int64

	// checks holds the per-name totals of check() calls (FR-CLI-12),
	// guarded by checkMu rather than mu so a busy scenario that checks
	// every response does not contend with Record.
	checkMu    sync.Mutex
	checkOrder []string
	checks     map[string]*CheckResult

	// steps holds every run of each named step (FR-CLI-18), guarded by
	// stepMu for the same reason as checks. Each run is packed in one
	// uint64 (duration in nanoseconds shifted left by one, with the lowest
	// bit set when the step failed), so a million iterations of three
	// steps stay near 24 MB.
	stepMu    sync.Mutex
	stepOrder []string
	steps     map[string][]uint64

	// calls holds every recorded protocol call, guarded by callMu. Each
	// call is packed the same way as a step. Byte counts are summed, not
	// stored per call. Measures are phase times and are not calls.
	callMu    sync.Mutex
	callOrder []string
	calls     map[string]*callProto
}

// NewCollector returns a Collector ready to record a run that is
// starting now.
func NewCollector() *Collector {
	return &Collector{start: time.Now(), maxBucket: -1}
}

// Start is the instant the run's clock started. Every offset in the series
// (and in a live point) counts from it.
func (c *Collector) Start() time.Time { return c.start }

// RequestsFromCalls makes each protocol call one request, instead of each
// iteration: the latency, counts, error rate, failure reasons and time
// series are then the calls', and the iterations are counted on their own
// in Result.Iterations. A scenario run, whose iteration may make any
// number of calls, needs this; a -protocol run, whose iteration is one
// call, does not. Call it before the run starts.
func (c *Collector) RequestsFromCalls() { c.callsAreRequests.Store(true) }

// Begin and End implement engine.IterationTracker. They keep the start of each
// running iteration, only while a live cursor exists (see LiveBuckets), so the
// cursor can hold a bucket back until the calls that started in it have ended.
func (c *Collector) Begin(start time.Time) {
	if !c.tracking.Load() {
		return
	}
	idx := int64(start.Sub(c.start) / bucketWidth)
	c.mu.Lock()
	if c.inflight == nil {
		c.inflight = map[int64]int{}
	}
	c.inflight[idx]++
	c.mu.Unlock()
}

// End undoes the Begin made with the same start. The engine calls it after the
// iteration was recorded.
func (c *Collector) End(start time.Time) {
	if !c.tracking.Load() {
		return
	}
	idx := int64(start.Sub(c.start) / bucketWidth)
	c.mu.Lock()
	if n := c.inflight[idx]; n > 1 {
		c.inflight[idx] = n - 1
	} else {
		delete(c.inflight, idx)
	}
	c.mu.Unlock()
}

// runningThrough reports whether an iteration that started in bucket idx or an
// earlier one is still running.
func (c *Collector) runningThrough(idx int64) bool {
	running, _ := c.liveState(idx)
	return running
}

// liveState is runningThrough plus the highest bucket a recorded call started
// in (-1 when there is none), read together.
func (c *Collector) liveState(idx int64) (running bool, last int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.inflight {
		if k <= idx {
			running = true
			break
		}
	}
	return running, c.maxBucket
}

// Record implements engine.Recorder. A dropped arrival is only counted: it
// never ran, so it has no latency and is not a failed request.
func (c *Collector) Record(r engine.IterationResult) {
	if r.Dropped {
		c.dropped.Add(1)
		return
	}
	c.ran.Add(1)
	if c.callsAreRequests.Load() {
		packed := packRun(r.Duration, r.Err != nil)
		c.iterMu.Lock()
		c.iterations = append(c.iterations, packed)
		c.iterMu.Unlock()
		return
	}
	c.addRequest(r.Start, r.Duration, r.Err)
}

// addRequest records one request that started at start, took d, and failed
// with err (nil when it worked).
func (c *Collector) addRequest(start time.Time, d time.Duration, err error) {
	s := sample{
		offset: max(start.Sub(c.start), 0),
		dur:    d,
		failed: err != nil,
	}
	reason := failure.Reason(err)
	c.mu.Lock()
	c.samples = append(c.samples, s)
	if idx := int64(s.offset / bucketWidth); idx > c.maxBucket {
		c.maxBucket = idx
	}
	if s.failed {
		if c.reasons == nil {
			c.reasons = map[string]int64{}
		}
		c.reasons[reason]++
	}
	c.mu.Unlock()

	c.liveTotal.Add(1)
	if s.failed {
		c.liveFailed.Add(1)
	}
}

// packRun packs a duration and a failed flag into one uint64: the duration
// in nanoseconds shifted left by one, with the lowest bit set on failure.
func packRun(d time.Duration, failed bool) uint64 {
	if d < 0 {
		d = 0
	}
	packed := uint64(d) << 1
	if failed {
		packed |= 1
	}
	return packed
}

// MaxCheckNames bounds how many distinct check names a run keeps. Further
// names are counted together under OtherChecksName.
const MaxCheckNames = 100

// OtherChecksName is the bucket for check names past MaxCheckNames.
const OtherChecksName = "(other checks)"

// RecordCheck counts one evaluation of the named check (FR-CLI-12). It
// implements netapi.CheckRecorder and is safe for concurrent use.
func (c *Collector) RecordCheck(name string, passed bool) {
	// A check name is text the script chose, so it may carry a secret the
	// script read (FR-CLI-17).
	name = secrets.Redact(name)
	c.checkMu.Lock()
	defer c.checkMu.Unlock()
	if c.checks == nil {
		c.checks = map[string]*CheckResult{}
	}
	cr := c.checks[name]
	if cr == nil && len(c.checks) >= MaxCheckNames {
		// A name built from an id or URL would grow this map with every
		// iteration. Past the cap, new names share one bucket, so the
		// counts (and check_rate) stay right and memory stays fixed.
		name = OtherChecksName
		cr = c.checks[name]
	}
	if cr == nil {
		cr = &CheckResult{Name: name}
		c.checks[name] = cr
		c.checkOrder = append(c.checkOrder, name)
	}
	if passed {
		cr.Passes++
	} else {
		cr.Fails++
	}
}

// MaxStepNames bounds how many distinct step names a run keeps. Further
// names are counted together under OtherStepsName.
const MaxStepNames = 100

// OtherStepsName is the bucket for step names past MaxStepNames.
const OtherStepsName = "(other steps)"

// RecordStep counts one run of the named step (FR-CLI-18). It implements
// netapi.StepRecorder and is safe for concurrent use.
func (c *Collector) RecordStep(name string, d time.Duration, failed bool) {
	// A step name is text the script chose, so it may carry a secret the
	// script read (FR-CLI-17).
	name = secrets.Redact(name)
	packed := packRun(d, failed)
	c.stepMu.Lock()
	defer c.stepMu.Unlock()
	if c.steps == nil {
		c.steps = map[string][]uint64{}
	}
	runs, known := c.steps[name]
	if !known && len(c.steps) >= MaxStepNames {
		// A name built from an id would grow this map with every
		// iteration. Past the cap, new names share one bucket.
		name = OtherStepsName
		runs, known = c.steps[name]
	}
	if !known {
		c.stepOrder = append(c.stepOrder, name)
	}
	c.steps[name] = append(runs, packed)
}

// snapshotSteps computes each step's totals and latency, in first-seen
// order.
func (c *Collector) snapshotSteps() []StepResult {
	c.stepMu.Lock()
	order := append([]string(nil), c.stepOrder...)
	runs := make(map[string][]uint64, len(order))
	for _, n := range order {
		runs[n] = append([]uint64(nil), c.steps[n]...)
	}
	c.stepMu.Unlock()
	if len(order) == 0 {
		return nil
	}
	out := make([]StepResult, 0, len(order))
	for _, n := range order {
		packed := runs[n]
		durations := make([]time.Duration, len(packed))
		var sum time.Duration
		var failed int64
		for i, p := range packed {
			durations[i] = time.Duration(p >> 1)
			sum += durations[i]
			if p&1 == 1 {
				failed++
			}
		}
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		sr := StepResult{Name: n, Total: int64(len(durations)), Failed: failed}
		if len(durations) > 0 {
			sr.ErrorRate = float64(failed) / float64(len(durations))
			sr.Latency = Latency{
				Min:  durations[0],
				Max:  durations[len(durations)-1],
				Mean: sum / time.Duration(len(durations)),
				P50:  percentile(durations, 50),
				P90:  percentile(durations, 90),
				P95:  percentile(durations, 95),
				P99:  percentile(durations, 99),
			}
		}
		out = append(out, sr)
	}
	return out
}

// snapshotChecks returns a copy of the check totals, in first-seen order.
func (c *Collector) snapshotChecks() []CheckResult {
	c.checkMu.Lock()
	defer c.checkMu.Unlock()
	if len(c.checkOrder) == 0 {
		return nil
	}
	out := make([]CheckResult, 0, len(c.checkOrder))
	for _, n := range c.checkOrder {
		out = append(out, *c.checks[n])
	}
	return out
}

// Counts returns the total and failed request counts recorded so far.
// Safe to call concurrently with Record, including while the run it
// belongs to is still in progress.
func (c *Collector) Counts() (total, failed int64) {
	return c.liveTotal.Load(), c.liveFailed.Load()
}

// bucketWidth is how wide each point in a Result's TimeSeries is. One
// second gives a readable RPS/error-rate line for a run of any length
// this phase's hard caps allow (see internal/safety), without the
// series itself becoming the thing that makes the report large.
const bucketWidth = time.Second

// Finish computes a Result from every sample recorded so far. Call it
// once, after the executor has returned; executor identifies the load
// shape (ex.Name()) and elapsed is the run's measured wall-clock time,
// both of which the caller already has from runScenario.
func (c *Collector) Finish(executor string, elapsed time.Duration) *Result {
	c.mu.Lock()
	samples := make([]sample, len(c.samples))
	copy(samples, c.samples)
	reasons := sortedReasons(c.reasons)
	c.mu.Unlock()

	res := &Result{
		Executor:          executor,
		StartedAt:         c.start,
		Elapsed:           elapsed,
		Total:             int64(len(samples)),
		DroppedIterations: c.dropped.Load(),
		FailureReasons:    reasons,
		Iterations:        c.snapshotIterations(),
		Checks:            c.snapshotChecks(),
		Steps:             c.snapshotSteps(),
		Protocols:         c.snapshotProtocols(elapsed),
	}
	if w := dropWarning(c.ran.Load(), res.DroppedIterations); w != "" {
		res.Warnings = append(res.Warnings, w)
	}
	if len(samples) == 0 {
		return res
	}

	durations := make([]time.Duration, len(samples))
	var sum time.Duration
	var failed int64
	buckets := map[int64]*bucketAccum{}

	for i, s := range samples {
		durations[i] = s.dur
		sum += s.dur
		if s.failed {
			failed++
		}
		idx := int64(s.offset / bucketWidth)
		b := buckets[idx]
		if b == nil {
			b = &bucketAccum{}
			buckets[idx] = b
		}
		b.count++
		if s.failed {
			b.failed++
		}
	}

	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	res.Failed = failed
	res.Latency = Latency{
		Min:  durations[0],
		Max:  durations[len(durations)-1],
		Mean: sum / time.Duration(len(durations)),
		P50:  percentile(durations, 50),
		P90:  percentile(durations, 90),
		P95:  percentile(durations, 95),
		P99:  percentile(durations, 99),
	}
	if res.Total > 0 {
		res.ErrorRate = float64(failed) / float64(res.Total)
	}
	res.TimeSeries = buildSeries(buckets)
	return res
}

// DropWarnPercent is the share of planned iterations that may be dropped
// before a result warns that the run sent less load than it planned.
const DropWarnPercent = 1.0

// dropWarning is the warning of a run that dropped more than
// DropWarnPercent of its planned iterations, or "". Planned is every
// iteration that ran plus every one that was dropped.
func dropWarning(ran, dropped int64) string {
	planned := ran + dropped
	if dropped == 0 || float64(dropped)*100 <= DropWarnPercent*float64(planned) {
		return ""
	}
	return fmt.Sprintf("%d of %d planned iterations (%.2f%%) were dropped because every VU was busy, so the run sent less load than planned. Raise -max-vus, or lower the rate.",
		dropped, planned, 100*float64(dropped)/float64(planned))
}

// sortedReasons lists the failure counts, most first, then by name.
func sortedReasons(counts map[string]int64) []FailureReason {
	if len(counts) == 0 {
		return nil
	}
	out := make([]FailureReason, 0, len(counts))
	for r, n := range counts {
		out = append(out, FailureReason{Reason: r, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// snapshotIterations is the iteration totals of a run whose requests are
// its calls, or nil for any other run.
func (c *Collector) snapshotIterations() *IterationStats {
	if !c.callsAreRequests.Load() {
		return nil
	}
	c.iterMu.Lock()
	packed := append([]uint64(nil), c.iterations...)
	c.iterMu.Unlock()
	total, failed, lat := unpackCalls(packed)
	return &IterationStats{Total: total, Failed: failed, ErrorRate: rateOf(failed, total), Duration: lat}
}

type bucketAccum struct {
	count  int64
	failed int64
}

// percentile returns the p-th percentile (0-100) of sorted, a slice
// already sorted ascending. It uses the nearest-rank method — simple,
// and exact enough for a load-testing report; no interpolation.
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p * len(sorted)) / 100
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// buildSeries turns the bucket map into a dense, time-ordered slice —
// dense so a chart renderer never has to guess whether a missing index
// means "no bucket" or "zero requests that second".
func buildSeries(buckets map[int64]*bucketAccum) []Point {
	if len(buckets) == 0 {
		return nil
	}
	var maxIdx int64
	for idx := range buckets {
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	series := make([]Point, maxIdx+1)
	for idx := int64(0); idx <= maxIdx; idx++ {
		p := Point{Offset: time.Duration(idx) * bucketWidth}
		if b, ok := buckets[idx]; ok {
			p.Requests = b.count
			p.Failed = b.failed
			p.RPS = float64(b.count) / bucketWidth.Seconds()
			if b.count > 0 {
				p.ErrorRate = float64(b.failed) / float64(b.count)
			}
		}
		series[idx] = p
	}
	return series
}
