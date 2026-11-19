// Package engine implements VegaLoad's core execution engine: the virtual
// user (VU) scheduler and the load-shape executors that decide how many
// VUs run, and when, over the life of a scenario.
//
// This package knows nothing about HTTP, gRPC, WebSocket, JavaScript, or
// Harness RT. It schedules calls to an IterationFunc supplied by the
// caller — the protocol and scripting layers (built later in Phase 0)
// are what give that function its real behaviour. See AGENTS.md at the
// repository root for the module boundary this package is expected to
// hold.
package engine

import (
	"context"
	"sync/atomic"
	"time"
)

// IterationFunc runs one iteration of a scenario. It returns a non-nil
// error if the iteration failed — a connection error, a timeout, a
// threshold the scenario itself chose to enforce, and so on. A VU calls
// this repeatedly for as long as its executor keeps it alive.
//
// Implementations should respect ctx cancellation promptly: an executor
// cancels ctx when the load shape's duration elapses or the run is
// stopped, and a slow-to-return IterationFunc delays shutdown.
type IterationFunc func(ctx context.Context) error

// IterationResult is what the engine records after every iteration.
// VUID identifies which VU ran the iteration; it has no meaning beyond
// that.
//
// Dropped marks an arrival the arrival-rate executors could not start
// because every VU was busy. It never ran, so it has no duration and no
// error, VUID is -1, and Start is when it was due. A Recorder must count
// it apart from the iterations that ran: it is not a failed request.
type IterationResult struct {
	VUID     int
	Start    time.Time
	Duration time.Duration
	Err      error
	Dropped  bool
}

// Recorder receives one IterationResult per completed iteration, from
// any number of VUs concurrently. Implementations must be safe for
// concurrent use.
//
// This package ships only Summary, a minimal Recorder for tests and
// simple use. The real HTML/JSON report (Phase 1, internal/report) will
// implement this same interface with full percentile tracking — engine
// does not need to know that package exists.
type Recorder interface {
	Record(IterationResult)
}

// IterationTracker is an optional extra for a Recorder that wants to know
// which iterations are running, not only which have finished (a live view
// needs this to tell an empty second from one whose calls have not ended yet).
// An executor checks for it once and, when the Recorder has it, calls Begin
// with an iteration's start time just before the iteration runs, and End with
// the same start time once the iteration is over and has been given to Record
// (or dropped, when it was cut off by the end of the run). Begin and End are
// always paired and may be called from any goroutine. The engine does nothing
// else with it: it does not know what the Recorder uses it for.
type IterationTracker interface {
	Begin(start time.Time)
	End(start time.Time)
}

// VUTracker is an optional extra for a Recorder that wants to know how many
// VUs are running at once. An executor checks for it once and, when the
// Recorder has it, calls VUStarted when a VU starts and VUStopped when it has
// stopped, always paired, from any goroutine. In FixedVUs, Ramp and Step a VU
// is one looping goroutine; in the arrival-rate executors each running
// iteration is one VU, so the count there is the iterations in flight.
type VUTracker interface {
	VUStarted()
	VUStopped()
}

// vuTracking returns rec's VUTracker, or one that does nothing.
func vuTracking(rec Recorder) VUTracker {
	if vt, ok := rec.(VUTracker); ok {
		return vt
	}
	return noVUs{}
}

type noVUs struct{}

func (noVUs) VUStarted() {}
func (noVUs) VUStopped() {}

// Summary is a minimal Recorder that keeps running counts: total
// iterations, failed iterations, dropped arrivals, and mean duration. It
// exists so the engine and its tests have something to record into in
// Phase 0, before internal/report exists. It is not a substitute for that
// package's real percentile tracking.
type Summary struct {
	total   atomic.Int64
	failed  atomic.Int64
	dropped atomic.Int64
	nsSum   atomic.Int64
}

// NewSummary returns an empty Summary, ready to use.
func NewSummary() *Summary {
	return &Summary{}
}

// Record implements Recorder.
func (s *Summary) Record(r IterationResult) {
	if r.Dropped {
		s.dropped.Add(1)
		return
	}
	s.total.Add(1)
	s.nsSum.Add(r.Duration.Nanoseconds())
	if r.Err != nil {
		s.failed.Add(1)
	}
}

// Total returns the number of iterations that ran and were recorded so
// far. Dropped arrivals are not in it.
func (s *Summary) Total() int64 { return s.total.Load() }

// Failed returns the number of recorded iterations that had a non-nil
// error.
func (s *Summary) Failed() int64 { return s.failed.Load() }

// Dropped returns the number of arrivals an arrival-rate executor could
// not start because every VU was busy.
func (s *Summary) Dropped() int64 { return s.dropped.Load() }

// MeanDuration returns the mean iteration duration across every
// recorded iteration, including failed ones. It returns 0 if nothing
// has been recorded yet.
func (s *Summary) MeanDuration() time.Duration {
	total := s.total.Load()
	if total == 0 {
		return 0
	}
	return time.Duration(s.nsSum.Load() / total)
}

// recordDrop tells rec that the arrival due at was not started because
// every VU was busy. It is not queued, so a struggling target does not
// cause unbounded VU growth, and it is recorded so it is visible in the
// results instead of silently missing.
func recordDrop(rec Recorder, at time.Time) {
	rec.Record(IterationResult{VUID: -1, Start: at, Dropped: true})
}
