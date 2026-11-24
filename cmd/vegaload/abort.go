package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/vegaload/vegaload/internal/report"
	"github.com/vegaload/vegaload/internal/threshold"
)

// Defaults of -abort-on-breach (FR-CLI-13).
const (
	// abortInterval is how often the watcher looks at the run.
	abortInterval = time.Second
	// abortSustain is how many looks in a row a statistic must be broken
	// before the run is stopped. One slow second is not a breach.
	abortSustain = 3
	// abortMaxGrace caps the automatic warm-up. The watcher ignores
	// statistics for the first part of the run, while connections open and
	// caches fill.
	abortMaxGrace = 5 * time.Second
)

// autoGrace is the warm-up when -abort-grace is not given: five seconds,
// or a quarter of the run when that is shorter.
func autoGrace(runDuration time.Duration) time.Duration {
	g := runDuration / 4
	if g > abortMaxGrace {
		g = abortMaxGrace
	}
	return g
}

// breachWatcher stops a run as soon as one of its thresholds is broken
// beyond recovery (FR-CLI-13). It looks at the run while it goes on.
//
// Two kinds of threshold can be judged early. A "sticky" one, such as
// "failed < 5", cannot recover, so the run stops at once. A "sustained"
// one, such as "p95 < 300ms", is a statistic that moves, so the run stops
// only when it stays broken for sustain looks in a row, after the warm-up,
// and with enough samples. Thresholds that need the whole run, such as
// rps, are left to the end.
type breachWatcher struct {
	thresholds []threshold.Threshold
	collector  *report.Collector
	executor   string
	start      time.Time

	grace    time.Duration
	interval time.Duration
	sustain  int
}

// newBreachWatcher builds a watcher for the live-judgeable part of ts.
func newBreachWatcher(ts []threshold.Threshold, c *report.Collector, executor string, start time.Time, grace time.Duration) *breachWatcher {
	var live []threshold.Threshold
	for _, t := range ts {
		if t.Live() != threshold.LiveNone {
			live = append(live, t)
		}
	}
	return &breachWatcher{
		thresholds: live, collector: c, executor: executor, start: start,
		grace: grace, interval: abortInterval, sustain: abortSustain,
	}
}

// watch looks at the run until stop is closed, or until it finds a
// breach. On a breach it calls cancel to stop the run, and returns what it
// found. It returns nil when stop closes first.
func (w *breachWatcher) watch(stop <-chan struct{}, cancel context.CancelFunc) *report.AbortInfo {
	if len(w.thresholds) == 0 {
		<-stop
		return nil
	}
	streak := make([]int, len(w.thresholds))
	wait := w.interval
	for {
		select {
		case <-stop:
			return nil
		case <-time.After(wait):
		}
		began := time.Now()
		elapsed := began.Sub(w.start)
		// Finish copies histograms, which is cheap, but with
		// -exact-percentiles it copies and sorts every sample, so on a
		// big run it takes a while. Look less often then, to keep the
		// cost near a tenth of the run's own.
		res := w.collector.Finish(w.executor, elapsed)
		for i, t := range w.thresholds {
			observed, breached := t.LiveBreach(res)
			if !breached {
				streak[i] = 0
				continue
			}
			if t.Live() == threshold.LiveSticky {
				cancel()
				return &report.AbortInfo{Threshold: t.Name, Observed: observed, At: elapsed}
			}
			if elapsed < w.grace {
				streak[i] = 0
				continue
			}
			streak[i]++
			if streak[i] >= w.sustain {
				cancel()
				return &report.AbortInfo{Threshold: t.Name, Observed: observed, At: elapsed}
			}
		}
		wait = w.interval
		if d := 10 * time.Since(began); d > wait {
			wait = d
		}
	}
}

// applyAbort records an early stop on the result. A run that was stopped
// for a breach always fails that threshold, even if the statistic moved
// back under its limit in the moments between the last look and the end.
func applyAbort(result *report.Result, info *report.AbortInfo) {
	if info == nil {
		return
	}
	result.Aborted = info
	for i := range result.Thresholds {
		if result.Thresholds[i].Name == info.Threshold {
			result.Thresholds[i].Passed = false
			result.Thresholds[i].Observed = info.Observed
			break
		}
	}
	if result.Thresholds != nil {
		passed := false
		result.ThresholdsPassed = &passed
	}
}

// abortNote tells the user, before the run, which thresholds are left to
// the end because they cannot be judged while the run goes on.
func abortNote(w io.Writer, ts []threshold.Threshold) {
	rest := threshold.NotLive(ts)
	if len(rest) == 0 {
		return
	}
	names := make([]string, len(rest))
	for i, t := range rest {
		names[i] = t.Name
	}
	fmt.Fprintf(w, "vegaload run: -abort-on-breach cannot judge these until the run ends: %s\n", strings.Join(names, "; "))
}
