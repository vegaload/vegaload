package engine

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// rampingArrivalTick is how often RampingArrivalRate re-reads the target
// rate and starts the iterations that came due since the last tick.
const rampingArrivalTick = 5 * time.Millisecond

// RampingArrivalRate starts iterations at a rate that changes over time:
// an open workload model like ConstantArrivalRate, but the rate follows
// Stages. Each stage moves the rate in a straight line from where the
// previous stage ended (StartRate for the first) to the stage's Target,
// in iterations per second, over the stage's Duration. A stage whose
// Target equals the previous one holds the rate.
//
// It answers "what happens as the arrival rate climbs, spikes and falls",
// and like ConstantArrivalRate it does not throttle when the target is
// slow: iterations keep arriving on schedule, up to MaxVUs running at
// once, and an arrival beyond that is dropped and recorded as a failed
// iteration with VUID -1.
type RampingArrivalRate struct {
	// StartRate is the rate, in iterations per second, at the start of
	// the first stage. Zero means the first stage ramps up from nothing.
	StartRate float64
	// Stages are the segments of the shape; Target is iterations per
	// second here, not a VU count.
	Stages []Stage
	// MaxVUs caps concurrently running iterations. Defaults to 1 if 0.
	MaxVUs int
}

// Name implements Executor.
func (r RampingArrivalRate) Name() string { return "ramping-arrival-rate" }

// Duration implements Executor.
func (r RampingArrivalRate) Duration() time.Duration { return totalStageDuration(r.Stages) }

// rateAt returns the arrival rate, in iterations per second, elapsed into
// the run. At or past the end of the last stage it is that stage's target.
func (r RampingArrivalRate) rateAt(elapsed time.Duration) float64 {
	var t time.Duration
	prev := r.StartRate
	for _, s := range r.Stages {
		end := t + s.Duration
		if elapsed < end || s.Duration == 0 {
			if s.Duration == 0 {
				return float64(s.Target)
			}
			frac := float64(elapsed-t) / float64(s.Duration)
			return prev + frac*(float64(s.Target)-prev)
		}
		t = end
		prev = float64(s.Target)
	}
	return prev
}

// Run implements Executor.
func (r RampingArrivalRate) Run(ctx context.Context, iter IterationFunc, rec Recorder) error {
	if len(r.Stages) == 0 {
		return fmt.Errorf("engine: RampingArrivalRate requires at least one stage")
	}
	if r.StartRate < 0 {
		return fmt.Errorf("engine: RampingArrivalRate requires StartRate >= 0, got %v", r.StartRate)
	}
	for _, s := range r.Stages {
		if s.Target < 0 {
			return fmt.Errorf("engine: RampingArrivalRate stage targets must be >= 0, got %d", s.Target)
		}
	}
	maxVUs := r.MaxVUs
	if maxVUs <= 0 {
		maxVUs = 1
	}

	ctx, cancel := context.WithTimeout(ctx, r.Duration())
	defer cancel()

	ticker := time.NewTicker(rampingArrivalTick)
	defer ticker.Stop()

	tr, _ := rec.(IterationTracker)
	sem := make(chan struct{}, maxVUs)
	var wg sync.WaitGroup
	nextVUID := 0

	begin := time.Now()
	last := begin
	// owed is the fractional number of iterations that have come due but
	// have not been started yet. Each tick adds the average rate over the
	// tick times its length; whole iterations are started, the rest carries.
	owed := 0.0

	launch := func(at time.Time) {
		select {
		case sem <- struct{}{}:
			nextVUID++
			id := nextVUID
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				start := time.Now()
				if tr != nil {
					tr.Begin(start)
				}
				err := iter(ctx)
				recordIteration(ctx, rec, IterationResult{
					VUID:     id,
					Start:    start,
					Duration: time.Since(start),
					Err:      err,
				})
				if tr != nil {
					tr.End(start)
				}
			}()
		default:
			rec.Record(IterationResult{
				VUID:     -1,
				Start:    at,
				Duration: 0,
				Err:      fmt.Errorf("engine: dropped iteration, all %d VUs busy", maxVUs),
			})
		}
	}

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case now := <-ticker.C:
			from, to := last.Sub(begin), now.Sub(begin)
			last = now
			if to <= from {
				continue
			}
			avg := (r.rateAt(from) + r.rateAt(to)) / 2
			owed += avg * (to - from).Seconds()
			for owed >= 1 {
				owed--
				launch(now)
			}
		}
	}
}
