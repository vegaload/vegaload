package engine

import (
	"context"
	"time"
)

// runVU runs iterations back-to-back on a single virtual user until ctx
// is cancelled. Each completed iteration's result is sent to rec. vuID
// identifies this VU in recorded results; it has no meaning beyond that.
//
// Iterations still in flight when the run ends (ctx cancelled, or its
// deadline passed) are not
// recorded: a cancel/deadline error from cutting them off is not a real
// failure and must not inflate error counts.
//
// runVU does not itself enforce a timeout on iter — that is the
// caller's job, typically by deriving ctx from context.WithTimeout at
// the executor level. A VU that never returns from iter will never
// stop; protocol implementations are expected to respect ctx.
func runVU(ctx context.Context, vuID int, iter IterationFunc, rec Recorder) {
	tr, _ := rec.(IterationTracker)
	for {
		if runEnded(ctx) {
			return
		}

		start := time.Now()
		if tr != nil {
			tr.Begin(start)
		}
		err := iter(ctx)
		recordIteration(ctx, rec, IterationResult{
			VUID:     vuID,
			Start:    start,
			Duration: time.Since(start),
			Err:      err,
		})
		if tr != nil {
			// After Record, so the iteration is never in neither place.
			tr.End(start)
		}
	}
}

// recordIteration sends r to rec unless the run has already ended and
// the iteration returned an error. In that case the iteration was cut
// off by shutdown (or raced with it), and recording it as a failure
// would report about one false failure per in-flight VU at run end.
// A successful return (err == nil) is still recorded even if ctx is
// done — the work finished.
func recordIteration(ctx context.Context, rec Recorder, r IterationResult) {
	if r.Err != nil && runEnded(ctx) {
		return
	}
	rec.Record(r)
}

// runEnded reports whether the run is over: ctx is done, or its deadline
// has passed. The second test matters. The timer that cancels a ctx at
// its deadline can fire a few milliseconds late, and ctx.Err() stays nil
// until it does. In that gap a driver sees the deadline at once and fails
// in microseconds, so a VU can record thousands of false failures before
// the cancel arrives. Checking the clock against the deadline closes the
// gap.
func runEnded(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	dl, ok := ctx.Deadline()
	return ok && !time.Now().Before(dl)
}
