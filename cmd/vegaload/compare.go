package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/vegaload/vegaload/internal/compare"
	"github.com/vegaload/vegaload/internal/report"
)

// cmdCompare implements `vegaload compare <baseline.json> <candidate.json>`:
// a thin file-vs-file regression check over the metrics already in
// report.Result. Exit 0 when the candidate is no worse than the baseline
// (under optional slack flags); exit 1 when a gated metric regressed or
// a report can't be read; exit 2 for usage errors.
func cmdCompare(args []string) int {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	output := fs.String("output", "text", "output mode: text or json")
	errorRateDelta := fs.Float64("error-rate-delta", 0, "absolute error-rate slack (0.01 = one percentage point) before counting as a regression")
	p95Ratio := fs.Float64("p95-ratio", 1, "max allowed candidate/baseline p95 ratio (1.2 allows 20% headroom)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: vegaload compare [flags] <baseline.json> <candidate.json>")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Diff two JSON reports from `vegaload run -out`. Prints deltas for")
		fmt.Fprintln(fs.Output(), "error rate, latency percentiles, totals, and overall RPS. Exits")
		fmt.Fprintln(fs.Output(), "non-zero when error rate or p95 latency regresses beyond the")
		fmt.Fprintln(fs.Output(), "optional slack flags (defaults are strict: any increase fails).")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *output != "text" && *output != "json" {
		fmt.Fprintf(os.Stderr, "vegaload compare: -output %q: want text or json\n", *output)
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintf(os.Stderr, "vegaload compare: want exactly two report paths (baseline, candidate), got %d\n", fs.NArg())
		fs.Usage()
		return 2
	}

	baselinePath, candidatePath := fs.Arg(0), fs.Arg(1)
	baseline, err := loadReportJSON(baselinePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload compare: baseline: %v\n", err)
		return 1
	}
	candidate, err := loadReportJSON(candidatePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload compare: candidate: %v\n", err)
		return 1
	}

	result := compare.Compare(baseline, candidate, compare.Options{
		ErrorRateDelta: *errorRateDelta,
		P95Ratio:       *p95Ratio,
	})
	result.BaselinePath = baselinePath
	result.CandidatePath = candidatePath

	if *output == "json" {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload compare: %v\n", err)
			return 1
		}
	} else {
		printCompare(os.Stdout, result)
	}

	if result.Regressed {
		return 1
	}
	return 0
}

func loadReportJSON(path string) (*report.Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var res report.Result
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("%s does not look like a vegaload JSON report: %w", path, err)
	}
	return &res, nil
}

func printCompare(w *os.File, r compare.Result) {
	fmt.Fprintf(w, "baseline:   %s\n", r.BaselinePath)
	fmt.Fprintf(w, "candidate:  %s\n", r.CandidatePath)
	if r.Regressed {
		fmt.Fprintln(w, "result:     REGRESSED")
	} else {
		fmt.Fprintln(w, "result:     ok")
	}
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "%-14s %14s %14s %14s\n", "metric", "baseline", "candidate", "delta")
	for _, m := range r.Metrics {
		mark := ""
		if m.Regressed {
			mark = " *"
		}
		fmt.Fprintf(w, "%-14s %14s %14s %14s%s\n",
			m.Name, formatMetric(m.Baseline, m.Unit), formatMetric(m.Candidate, m.Unit), formatDelta(m), mark)
	}
	fmt.Fprintln(w, "notes:")
	for _, n := range r.Notes {
		fmt.Fprintf(w, "  - %s\n", n)
	}
}

func formatMetric(v float64, unit string) string {
	switch unit {
	case "ratio":
		return fmt.Sprintf("%.2f%%", v*100)
	case "ns":
		return time.Duration(v).Round(1e3).String()
	case "rps":
		return fmt.Sprintf("%.1f", v)
	case "count":
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprintf("%g", v)
	}
}

func formatDelta(m compare.Metric) string {
	switch m.Unit {
	case "ratio":
		return fmt.Sprintf("%+.2fpp", m.Delta*100)
	case "ns":
		d := time.Duration(m.Delta).Round(1e3)
		if m.Delta >= 0 {
			return "+" + d.String()
		}
		return d.String()
	case "rps":
		return fmt.Sprintf("%+.1f", m.Delta)
	case "count":
		return fmt.Sprintf("%+.0f", m.Delta)
	default:
		return fmt.Sprintf("%+g", m.Delta)
	}
}
