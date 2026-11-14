package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/vegaload/vegaload/internal/report"
	"github.com/vegaload/vegaload/internal/threshold"
)

// cmdMerge implements `vegaload merge a.json b.json ...`: combine the JSON reports of runs that sent load at the same time, from
// several machines, into one report, by adding their counts and latency
// histograms. Exit 0 when merged, 1 when a report cannot be read or
// merged, 2 for a usage error, 3 when the merged result breaks a
// threshold, the same codes as run.
func cmdMerge(args []string) int {
	fs := flag.NewFlagSet("merge", flag.ContinueOnError)
	outPath := fs.String("out", "", "write the merged JSON report to this path")
	htmlPath := fs.String("report", "", "also write a self-contained HTML report of the merged result to this path")
	output := fs.String("output", "text", "output mode: text or json")
	var exprs repeatedFlags
	fs.Var(&exprs, "threshold", "pass/fail threshold on the merged result, with the same syntax as run's -threshold (repeatable). A breach exits 3")
	thresholdsFile := fs.String("thresholds", "", "JSON file of thresholds to judge the merged result on, the same format as run's -thresholds")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: vegaload merge [flags] <report.json> <report.json> [...]")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Combine JSON reports from `vegaload run -out`, written by runs that sent load")
		fmt.Fprintln(fs.Output(), "at the same time from different machines, into one report. Counts and latency")
		fmt.Fprintln(fs.Output(), "histograms are added, and every percentile and rate is worked out again from")
		fmt.Fprintln(fs.Output(), "the sums. Each report's own thresholds and baseline are not carried over;")
		fmt.Fprintln(fs.Output(), "give -threshold here to judge the merged result. Exit 0 merged, 1 a report")
		fmt.Fprintln(fs.Output(), "cannot be read or merged, 2 usage error, 3 thresholds breached.")
		fmt.Fprintln(fs.Output(), "Flags must come before the report files.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *output != "text" && *output != "json" {
		fmt.Fprintf(os.Stderr, "vegaload merge: -output %q: want text or json\n", *output)
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintf(os.Stderr, "vegaload merge: want at least two report paths, got %d\n", fs.NArg())
		fs.Usage()
		return 2
	}
	var ts []threshold.Threshold
	if *thresholdsFile != "" {
		loaded, err := threshold.LoadFile(*thresholdsFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vegaload merge: -thresholds: %v\n", err)
			return 2
		}
		ts = append(ts, loaded...)
	}
	if len(exprs) > 0 {
		parsed, err := threshold.ParseAll([]string(exprs))
		if err != nil {
			fmt.Fprintf(os.Stderr, "vegaload merge: -threshold: %v\n", err)
			return 2
		}
		ts = append(ts, parsed...)
	}

	paths := fs.Args()
	results := make([]*report.Result, 0, len(paths))
	for _, p := range paths {
		res, err := loadReportJSON(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vegaload merge: %v\n", err)
			return 1
		}
		if err := report.Mergeable(res); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload merge: %s: %v\n", p, err)
			return 1
		}
		results = append(results, res)
	}
	merged, err := report.Merge(results)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload merge: %v\n", err)
		return 1
	}
	if len(ts) > 0 {
		merged.Thresholds = threshold.Evaluate(ts, merged)
		passed := threshold.AllPassed(merged.Thresholds)
		merged.ThresholdsPassed = &passed
	}

	switch *output {
	case "json":
		if err := json.NewEncoder(os.Stdout).Encode(merged); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload merge: encoding -output json: %v\n", err)
			return 1
		}
	default:
		fmt.Printf("merged %d reports: %s\n", len(paths), strings.Join(paths, ", "))
		printResult(os.Stdout, merged)
	}
	if *outPath != "" {
		if err := report.WriteJSON(*outPath, merged); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload merge: writing -out: %v\n", err)
			return 1
		}
	}
	if *htmlPath != "" {
		if err := report.WriteHTML(*htmlPath, merged); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload merge: writing -report: %v\n", err)
			return 1
		}
		if *output == "text" {
			fmt.Printf("report: %s\n", *htmlPath)
		}
	}
	if merged.ThresholdsPassed != nil && !*merged.ThresholdsPassed {
		fmt.Fprintf(os.Stderr, "vegaload merge: thresholds breached: %s\n", strings.Join(breachedNames(merged), "; "))
		return exitThresholdsBreached
	}
	return 0
}
