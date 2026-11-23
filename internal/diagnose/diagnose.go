// Package diagnose implements the rule-based half of FR-MCP-05's
// diagnose_failure / vegaload diagnose: a set of fixed heuristics over a
// report.Result that flag the failure patterns a human reviewing a load
// test would look for first, plus a pure-arithmetic threshold suggester.
// It knows nothing about the CLI, MCP, or any LLM — internal/llm's
// narrative connector is a separate, optional layer that takes these
// Findings as input (see cmd/vegaload/diagnose.go for how the two
// combine).
package diagnose

import (
	"fmt"
	"strings"

	"github.com/vegaload/vegaload/internal/report"
)

// Findings is the rule-based diagnosis of a single report.Result: a set
// of independently-triggered observations, each already phrased as a
// sentence a report or MCP tool response can show verbatim.
type Findings struct {
	ZeroIterations  bool     `json:"zero_iterations"`
	FailurePercent  float64  `json:"failure_percent"`
	HighFailureRate bool     `json:"high_failure_rate"` // > 50%
	LatencyLongTail bool     `json:"latency_long_tail"` // p99 > 5x p50
	WorstBucket     *Bucket  `json:"worst_bucket,omitempty"`
	Transient       bool     `json:"transient,omitempty"` // failures concentrated in one bucket vs. spread out
	Notes           []string `json:"notes"`
}

// Bucket identifies the single worst time-series point by error rate,
// letting a caller point at *when* a run went bad rather than just that
// it did.
type Bucket struct {
	OffsetSeconds float64 `json:"offset_seconds"`
	ErrorRate     float64 `json:"error_rate"`
	Requests      int64   `json:"requests"`
	Failed        int64   `json:"failed"`
}

// Analyze runs every heuristic against res and returns the findings that
// triggered, each with a human-readable note appended to Notes. It never
// returns an error: an empty or all-successful Result simply yields
// Findings with nothing set and a single reassuring note.
func Analyze(res *report.Result) Findings {
	var f Findings

	if d := res.DroppedIterations; d > 0 {
		planned := d + res.Total
		if res.Iterations != nil {
			planned = d + res.Iterations.Total
		}
		f.Notes = append(f.Notes, fmt.Sprintf("%d of %d planned iterations (%.1f%%) were dropped because every VU was busy — the target got less load than planned, and the numbers only cover what ran; raise -max-vus", d, planned, 100*float64(d)/float64(planned)))
	}

	if res.Total == 0 {
		f.ZeroIterations = true
		if it := res.Iterations; it != nil && it.Total > 0 {
			f.Notes = append(f.Notes, fmt.Sprintf("the scenario ran %d iterations (%d failed) but no protocol call was recorded — check that it makes its calls, and that the safety rules allow their hosts", it.Total, it.Failed))
			return f
		}
		f.Notes = append(f.Notes, "the run recorded zero iterations — check that the scenario's default export ran and that the target was reachable")
		return f
	}

	f.FailurePercent = res.ErrorRate * 100
	if res.ErrorRate > 0.5 {
		f.HighFailureRate = true
		f.Notes = append(f.Notes, fmt.Sprintf("%.1f%% of requests failed — more than half; this usually points at the target rejecting load rather than isolated errors", f.FailurePercent))
	} else if res.Failed > 0 {
		f.Notes = append(f.Notes, fmt.Sprintf("%.1f%% of requests failed (%d of %d)", f.FailurePercent, res.Failed, res.Total))
	} else {
		f.Notes = append(f.Notes, "no failed requests")
	}
	if len(res.FailureReasons) > 0 {
		parts := make([]string, len(res.FailureReasons))
		for i, fr := range res.FailureReasons {
			parts[i] = fmt.Sprintf("%d %s", fr.Count, fr.Reason)
		}
		f.Notes = append(f.Notes, "failed requests by reason: "+strings.Join(parts, ", "))
	}
	if it := res.Iterations; it != nil && it.Failed > 0 {
		f.Notes = append(f.Notes, fmt.Sprintf("%d of %d iterations failed (%.1f%%) — an iteration fails on an error the scenario throws, which is not always a failed call, so the error rate above may not show it", it.Failed, it.Total, it.ErrorRate*100))
	}

	if res.Latency.P50 > 0 && res.Latency.P99 > res.Latency.P50*5 {
		f.LatencyLongTail = true
		f.Notes = append(f.Notes, fmt.Sprintf("p99 latency (%s) is more than 5x p50 (%s) — a long tail, suggesting a subset of requests hit something much slower (GC pause, cold cache, a slow dependency) rather than uniformly slow responses", res.Latency.P99.Round(1e3), res.Latency.P50.Round(1e3)))
	}

	if worst := worstBucket(res); worst != nil {
		f.WorstBucket = worst
		if worst.ErrorRate > 0 {
			f.Transient = isTransient(res, worst)
			shape := "concentrated in one period"
			if !f.Transient {
				shape = "spread across the run, not isolated to one period"
			}
			f.Notes = append(f.Notes, fmt.Sprintf("the worst second was at +%.0fs with a %.1f%% error rate (%d of %d requests) — failures look %s", worst.OffsetSeconds, worst.ErrorRate*100, worst.Failed, worst.Requests, shape))
		}
	}

	return f
}

// worstBucket finds the time-series point with the highest error rate,
// breaking ties toward the earliest occurrence so repeated runs produce
// a stable answer.
func worstBucket(res *report.Result) *Bucket {
	var worst *Bucket
	for _, p := range res.TimeSeries {
		if p.Requests == 0 {
			continue
		}
		if worst == nil || p.ErrorRate > worst.ErrorRate {
			worst = &Bucket{
				OffsetSeconds: p.Offset.Seconds(),
				ErrorRate:     p.ErrorRate,
				Requests:      p.Requests,
				Failed:        p.Failed,
			}
		}
	}
	return worst
}

// isTransient reports whether failures are concentrated around the
// worst bucket rather than spread evenly through the run: true when the
// worst bucket's error rate is at least double the run's overall error
// rate, which only happens when other buckets are comparatively clean.
func isTransient(res *report.Result, worst *Bucket) bool {
	if res.ErrorRate == 0 {
		return worst.ErrorRate > 0
	}
	return worst.ErrorRate >= res.ErrorRate*2
}

// Thresholds is the output of SuggestThresholds: pass/fail criteria a
// future run of the same scenario could be gated on, derived from this
// run's own observed performance.
type Thresholds struct {
	LatencyP95 string  `json:"latency_p95"`
	ErrorRate  float64 `json:"error_rate"`
}

// SuggestThresholds derives conservative pass/fail thresholds from a
// clean baseline run: p95 latency headroom'd by 20% and rounded to the
// nearest 10ms (so the suggestion reads as a round number, not a
// spurious-precision artifact of one run), and an error-rate ceiling of
// 1.5x what was observed, floored at 1% so a zero-failure baseline still
// yields a usable, nonzero threshold.
func SuggestThresholds(res *report.Result) Thresholds {
	p95ms := res.Latency.P95.Milliseconds()
	suggestedMs := roundUpToNearest10(float64(p95ms) * 1.2)

	errRate := res.ErrorRate * 1.5
	if errRate < 0.01 {
		errRate = 0.01
	}

	return Thresholds{
		LatencyP95: fmt.Sprintf("%dms", suggestedMs),
		ErrorRate:  errRate,
	}
}

func roundUpToNearest10(ms float64) int64 {
	if ms <= 0 {
		return 10
	}
	n := int64(ms)
	rem := n % 10
	if rem == 0 && float64(n) >= ms {
		return n
	}
	return n - rem + 10
}
