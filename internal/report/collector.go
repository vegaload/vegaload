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
	"github.com/vegaload/vegaload/internal/hist"
	"github.com/vegaload/vegaload/internal/secrets"
)

// Options changes how a Collector keeps a run's latencies.
type Options struct {
	// Exact keeps every latency, so the percentiles are exact instead of
	// estimated from a histogram (within hist.MaxError above 0.5 ms).
	// Memory then grows with the number of requests: it is for small
	// local runs. The result carries the histograms either way.
	Exact bool
}

// Collector implements engine.Recorder. Every latency of a run (requests,
// iterations, steps, protocol calls and measures) goes into a histogram,
// and every count into a counter, so the memory a run needs does not grow
// with the number of requests. A request is one iteration, or, for a run
// that called RequestsFromCalls, one protocol call. It is safe for
// concurrent use — Run calls Record from every VU goroutine at once.
type Collector struct {
	exact bool // set once, by NewCollectorWith

	mu    sync.Mutex
	start time.Time
	reqs  *dist
	// secs counts the requests that started in each one-second bucket: the
	// final time series. Its last index is the last second a request
	// started in, which lets a live view know which buckets are left at
	// the end (see LiveBuckets).
	secs []bucketAccum
	// reasons counts the failed requests by failure.Reason.
	reasons map[string]int64
	// liveHists is the latency of each second a live cursor has not handed
	// out yet. It is kept only while a cursor exists (tracking), from the
	// cursor's next second (liveNext) on, and a second's histogram is
	// dropped when the cursor hands the second out.
	liveHists map[int64]*hist.Histogram
	liveNext  int64

	// callsAreRequests is set by RequestsFromCalls. Each recorded call is
	// then a request, and each iteration goes to iterations instead.
	callsAreRequests atomic.Bool
	// iterations holds the iterations of such a run, guarded by iterMu.
	iterMu     sync.Mutex
	iterations *runStats
	// ran counts the iterations that ran, in either mode, and dropped the
	// arrivals that found every VU busy. Their sum is the planned load.
	ran     atomic.Int64
	dropped atomic.Int64

	// tracking is set once a live cursor exists. Only then are the starts of
	// running iterations kept (inflight, guarded by mu, counts them by the
	// bucket they started in) and the per-second histograms, so a run with
	// no live view pays nothing for it.
	tracking atomic.Bool
	inflight map[int64]int

	// liveTotal/liveFailed mirror the request counts but are safe to
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

	// steps holds each named step's runs (FR-CLI-18), guarded by stepMu
	// for the same reason as checks.
	stepMu    sync.Mutex
	stepOrder []string
	steps     map[string]*runStats

	// calls holds every recorded protocol call, guarded by callMu. Byte
	// counts are summed. Measures are phase times and are not calls.
	callMu    sync.Mutex
	callOrder []string
	calls     map[string]*callProto
}

// runStats is the runs of one thing (an iteration, a step) and how many
// of them failed.
type runStats struct {
	d      *dist
	failed int64
}

func (r *runStats) add(d time.Duration, failed bool) {
	r.d.add(d)
	if failed {
		r.failed++
	}
}

// NewCollector returns a Collector ready to record a run that is
// starting now, with latencies kept in histograms.
func NewCollector() *Collector { return NewCollectorWith(Options{}) }

// NewCollectorWith is NewCollector with options.
func NewCollectorWith(o Options) *Collector {
	return &Collector{
		exact:      o.Exact,
		start:      time.Now(),
		reqs:       newDist(o.Exact),
		iterations: &runStats{d: newDist(o.Exact)},
	}
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
	return running, int64(len(c.secs)) - 1
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
		c.iterMu.Lock()
		c.iterations.add(r.Duration, r.Err != nil)
		c.iterMu.Unlock()
		return
	}
	c.addRequest(r.Start, r.Duration, r.Err)
}

// addRequest records one request that started at start, took d, and failed
// with err (nil when it worked).
func (c *Collector) addRequest(start time.Time, d time.Duration, err error) {
	idx := int64(max(start.Sub(c.start), 0) / bucketWidth)
	failed := err != nil
	var reason string
	if failed {
		reason = failure.Reason(err)
	}
	c.mu.Lock()
	c.reqs.add(d)
	if n := int64(len(c.secs)); idx >= n {
		c.secs = append(c.secs, make([]bucketAccum, idx+1-n)...)
	}
	b := &c.secs[idx]
	b.count++
	if failed {
		b.failed++
		if c.reasons == nil {
			c.reasons = map[string]int64{}
		}
		c.reasons[reason]++
	}
	if idx >= c.liveNext && c.tracking.Load() {
		h := c.liveHists[idx]
		if h == nil {
			if c.liveHists == nil {
				c.liveHists = map[int64]*hist.Histogram{}
			}
			h = hist.New()
			c.liveHists[idx] = h
		}
		h.Add(d)
	}
	c.mu.Unlock()

	c.liveTotal.Add(1)
	if failed {
		c.liveFailed.Add(1)
	}
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
	c.stepMu.Lock()
	defer c.stepMu.Unlock()
	if c.steps == nil {
		c.steps = map[string]*runStats{}
	}
	st, known := c.steps[name]
	if !known && len(c.steps) >= MaxStepNames {
		// A name built from an id would grow this map with every
		// iteration. Past the cap, new names share one bucket.
		name = OtherStepsName
		st, known = c.steps[name]
	}
	if !known {
		st = &runStats{d: newDist(c.exact)}
		c.steps[name] = st
		c.stepOrder = append(c.stepOrder, name)
	}
	st.add(d, failed)
}

// snapshotSteps computes each step's totals and latency, in first-seen
// order.
func (c *Collector) snapshotSteps() []StepResult {
	c.stepMu.Lock()
	order := append([]string(nil), c.stepOrder...)
	snaps := make([]runStats, len(order))
	for i, n := range order {
		st := c.steps[n]
		snaps[i] = runStats{d: st.d.snapshot(), failed: st.failed}
	}
	c.stepMu.Unlock()
	if len(order) == 0 {
		return nil
	}
	out := make([]StepResult, 0, len(order))
	for i, n := range order {
		s := snaps[i]
		total := s.d.h.Count()
		out = append(out, StepResult{
			Name:      n,
			Total:     total,
			Failed:    s.failed,
			ErrorRate: rateOf(s.failed, total),
			Latency:   s.d.latency(),
		})
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

// Finish computes a Result from everything recorded so far. executor
// identifies the load shape (ex.Name()) and elapsed is the run's measured
// wall-clock time, both of which the caller already has from runScenario.
// It copies histograms and counters, not requests, so it stays cheap
// enough for the abort watcher to call while the run goes on (in exact
// mode it copies and sorts every latency).
func (c *Collector) Finish(executor string, elapsed time.Duration) *Result {
	c.mu.Lock()
	reqs := c.reqs.snapshot()
	secs := append([]bucketAccum(nil), c.secs...)
	reasons := sortedReasons(c.reasons)
	c.mu.Unlock()

	res := &Result{
		Schema:            Schema,
		Executor:          executor,
		StartedAt:         c.start,
		Elapsed:           elapsed,
		Total:             reqs.h.Count(),
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
	if res.Total == 0 {
		return res
	}
	for _, b := range secs {
		res.Failed += b.failed
	}
	res.ErrorRate = rateOf(res.Failed, res.Total)
	res.Latency = reqs.latency()
	res.TimeSeries = buildSeries(secs)
	return res
}

// DropWarnPercent is the share of planned iterations that may be dropped
// before a result warns that the run sent less load than it planned.
const DropWarnPercent = 1.0

// DroppedPercent is the share of planned iterations (those that ran plus
// those that were dropped) that were dropped so far, from 0 to 100.
func (c *Collector) DroppedPercent() float64 {
	ran, dropped := c.ran.Load(), c.dropped.Load()
	if dropped == 0 {
		return 0
	}
	return 100 * float64(dropped) / float64(ran+dropped)
}

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
	d, failed := c.iterations.d.snapshot(), c.iterations.failed
	c.iterMu.Unlock()
	total := d.h.Count()
	return &IterationStats{Total: total, Failed: failed, ErrorRate: rateOf(failed, total), Duration: d.latency()}
}

type bucketAccum struct {
	count  int64
	failed int64
}

// buildSeries turns the per-second counts into a dense, time-ordered
// slice — dense so a chart renderer never has to guess whether a missing
// index means "no bucket" or "zero requests that second".
func buildSeries(secs []bucketAccum) []Point {
	if len(secs) == 0 {
		return nil
	}
	series := make([]Point, len(secs))
	for idx, b := range secs {
		series[idx] = Point{
			Offset:    time.Duration(idx) * bucketWidth,
			Requests:  b.count,
			Failed:    b.failed,
			RPS:       float64(b.count) / bucketWidth.Seconds(),
			ErrorRate: rateOf(b.failed, b.count),
		}
	}
	return series
}
