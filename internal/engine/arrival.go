package engine

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ConstantArrivalRate starts iterations at a fixed rate, independent of
// how long each iteration takes — an open workload model. FixedVUs,
// Ramp, and Step all hold a VU count steady and let each VU run
// iterations back-to-back (a closed model); under that model, a slow
// target automatically throttles the iteration rate, because VUs spend
// longer per iteration. ConstantArrivalRate does not throttle: it keeps
// starting new iterations on schedule even if earlier ones are still
// running, which is the point of it — it answers "can this target
// sustain X requests per second", not "what throughput do N users
// produce".
//
// If an iteration takes longer than the arrival interval, more than
// one VU runs concurrently to keep up. MaxVUs caps how many will ever
// run at once; an arrival that would exceed MaxVUs is dropped and
// recorded as a failed iteration with VUID -1, rather than queued, so
// a struggling target doesn't cause unbounded VU growth.
type ConstantArrivalRate struct {
	// Rate is the target number of iterations per second.
	Rate float64
	// Dur is the total duration this executor runs for.
	Dur time.Duration
	// PreAllocatedVUs is accepted for forward compatibility with
	// k6-style arrival-rate configuration. The current implementation
	// spawns goroutines on demand up to MaxVUs rather than pre-warming
	// a fixed pool, since goroutines are cheap; this field is not yet
	// used.
	PreAllocatedVUs int
	// MaxVUs caps concurrently running iterations. Arrivals beyond
	// this cap are dropped. Defaults to 1 if left at 0.
	MaxVUs int
}

// Name implements Executor.
func (c ConstantArrivalRate) Name() string { return "constant-arrival-rate" }

// Duration implements Executor.
func (c ConstantArrivalRate) Duration() time.Duration { return c.Dur }

// Run implements Executor.
func (c ConstantArrivalRate) Run(ctx context.Context, iter IterationFunc, rec Recorder) error {
	if c.Rate <= 0 {
		return fmt.Errorf("engine: ConstantArrivalRate requires Rate > 0, got %v", c.Rate)
	}
	maxVUs := c.MaxVUs
	if maxVUs <= 0 {
		maxVUs = 1
	}

	ctx, cancel := context.WithTimeout(ctx, c.Dur)
	defer cancel()

	interval := time.Duration(float64(time.Second) / c.Rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	sem := make(chan struct{}, maxVUs)
	var wg sync.WaitGroup
	nextVUID := 0

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case tickTime := <-ticker.C:
			select {
			case sem <- struct{}{}:
				nextVUID++
				id := nextVUID
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					start := time.Now()
					err := iter(ctx)
					recordIteration(ctx, rec, IterationResult{
						VUID:     id,
						Start:    start,
						Duration: time.Since(start),
						Err:      err,
					})
				}()
			default:
				// MaxVUs are all busy: drop this arrival rather than
				// queue it, and record the drop so it is visible in
				// results instead of silently missing.
				rec.Record(IterationResult{
					VUID:     -1,
					Start:    tickTime,
					Duration: 0,
					Err:      fmt.Errorf("engine: dropped iteration, all %d VUs busy", maxVUs),
				})
			}
		}
	}
}
