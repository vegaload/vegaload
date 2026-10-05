// Package compare diffs two report.Result values (typically JSON from
// `vegaload run -out`) so a later run can be checked against a baseline
// without a baseline registry or history store — just two files.
package compare

import (
	"fmt"
	"time"

	"github.com/vegaload/vegaload/internal/report"
)

// Options tunes what counts as a regression. Zero values mean strict
// compare: any increase in error rate or p95 latency is a regression.
type Options struct {
	// ErrorRateDelta is absolute slack on error rate (0.01 = one
	// percentage point). Candidate may exceed baseline by this much
	// without counting as a regression.
	ErrorRateDelta float64
	// P95Ratio is the maximum allowed candidate/baseline p95 ratio.
	// 1.0 means any p95 increase is a regression; 1.2 allows 20% headroom.
	// Values below 1.0 are treated as 1.0.
	P95Ratio float64
}

// Metric is one compared field: baseline vs candidate, with a delta and
// whether that field alone counts as a regression under Options.
type Metric struct {
	Name      string  `json:"name"`
	Baseline  float64 `json:"baseline"`
	Candidate float64 `json:"candidate"`
	Delta     float64 `json:"delta"` // candidate - baseline
	Unit      string  `json:"unit"`  // "ratio", "ns", "rps", "count"
	Regressed bool    `json:"regressed"`
}

// Result is the structured outcome of Compare — the same shape printed
// as text or JSON by `vegaload compare`, and returned by the MCP
// compare_reports tool.
type Result struct {
	BaselinePath  string   `json:"baseline_path,omitempty"`
	CandidatePath string   `json:"candidate_path,omitempty"`
	Metrics       []Metric `json:"metrics"`
	Regressed     bool     `json:"regressed"`
	Notes         []string `json:"notes"`
}

// Compare diffs candidate against baseline. Paths are optional labels
// filled in by the CLI; the comparison itself is purely numeric.
func Compare(baseline, candidate *report.Result, opts Options) Result {
	if opts.P95Ratio < 1 {
		opts.P95Ratio = 1
	}
	if opts.ErrorRateDelta < 0 {
		opts.ErrorRateDelta = 0
	}

	var out Result

	errRate := Metric{
		Name:      "error_rate",
		Baseline:  baseline.ErrorRate,
		Candidate: candidate.ErrorRate,
		Delta:     candidate.ErrorRate - baseline.ErrorRate,
		Unit:      "ratio",
	}
	if candidate.ErrorRate > baseline.ErrorRate+opts.ErrorRateDelta {
		errRate.Regressed = true
		out.Notes = append(out.Notes, fmt.Sprintf(
			"error rate rose from %.2f%% to %.2f%% (delta %+.2fpp)",
			baseline.ErrorRate*100, candidate.ErrorRate*100, errRate.Delta*100))
	}
	out.Metrics = append(out.Metrics, errRate)

	out.Metrics = append(out.Metrics, latencyMetric("latency.p50", baseline.Latency.P50, candidate.Latency.P50, false, 0))
	out.Metrics = append(out.Metrics, latencyMetric("latency.p90", baseline.Latency.P90, candidate.Latency.P90, false, 0))

	p95 := latencyMetric("latency.p95", baseline.Latency.P95, candidate.Latency.P95, true, opts.P95Ratio)
	if p95.Regressed {
		limit := baseline.Latency.P95
		if opts.P95Ratio > 1 {
			limit = time.Duration(float64(baseline.Latency.P95) * opts.P95Ratio)
		}
		out.Notes = append(out.Notes, fmt.Sprintf(
			"p95 latency rose from %s to %s (limit %s)",
			baseline.Latency.P95.Round(1e3), candidate.Latency.P95.Round(1e3), limit.Round(1e3)))
	}
	out.Metrics = append(out.Metrics, p95)

	out.Metrics = append(out.Metrics, latencyMetric("latency.p99", baseline.Latency.P99, candidate.Latency.P99, false, 0))
	out.Metrics = append(out.Metrics, latencyMetric("latency.mean", baseline.Latency.Mean, candidate.Latency.Mean, false, 0))

	out.Metrics = append(out.Metrics, Metric{
		Name:      "total",
		Baseline:  float64(baseline.Total),
		Candidate: float64(candidate.Total),
		Delta:     float64(candidate.Total - baseline.Total),
		Unit:      "count",
	})
	out.Metrics = append(out.Metrics, Metric{
		Name:      "failed",
		Baseline:  float64(baseline.Failed),
		Candidate: float64(candidate.Failed),
		Delta:     float64(candidate.Failed - baseline.Failed),
		Unit:      "count",
	})

	baseRPS := overallRPS(baseline)
	candRPS := overallRPS(candidate)
	out.Metrics = append(out.Metrics, Metric{
		Name:      "rps",
		Baseline:  baseRPS,
		Candidate: candRPS,
		Delta:     candRPS - baseRPS,
		Unit:      "rps",
	})

	for _, m := range out.Metrics {
		if m.Regressed {
			out.Regressed = true
			break
		}
	}
	if !out.Regressed {
		out.Notes = append(out.Notes, "no regressions against the baseline")
	}
	return out
}

func latencyMetric(name string, base, cand time.Duration, gate bool, ratio float64) Metric {
	m := Metric{
		Name:      name,
		Baseline:  float64(base),
		Candidate: float64(cand),
		Delta:     float64(cand - base),
		Unit:      "ns",
	}
	if gate && base > 0 && float64(cand) > float64(base)*ratio {
		m.Regressed = true
	}
	if gate && base == 0 && cand > 0 {
		m.Regressed = true
	}
	return m
}

func overallRPS(r *report.Result) float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.Total) / r.Elapsed.Seconds()
}
