// eval.go implements `vegaload mcp eval`: FR-MCP-04's versioned MCP
// tool-calling evaluation suite, run against this same compiled binary.
//
// It resolves this binary's own path and builds the real
// internal/mcp.Tool set from it (exactly what `vegaload mcp serve`
// would register), writes two fixture files into a temp working
// directory (a report.Result and a one-endpoint OpenAPI spec, with
// values the embedded v1 suite's expectations are written against —
// see internal/eval/v1/cases.json), substitutes "${WORKDIR}" in the
// embedded suite for that directory, and runs every case through
// internal/eval.Run.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/vegaload/vegaload/internal/eval"
	"github.com/vegaload/vegaload/internal/mcp"
	"github.com/vegaload/vegaload/internal/report"
)

// cmdMCPEval runs the embedded v1 eval suite and reports pass/fail per
// case, exiting non-zero if any case failed.
func cmdMCPEval(args []string) int {
	output := "text"
	for i := 0; i < len(args); i++ {
		if args[i] == "-output" && i+1 < len(args) {
			output = args[i+1]
			i++
			continue
		}
		fmt.Fprintf(os.Stderr, "vegaload mcp eval: unrecognized argument %q\n", args[i])
		fmt.Fprintln(os.Stderr, "Usage: vegaload mcp eval [-output text|json]")
		return 2
	}
	if output != "text" && output != "json" {
		fmt.Fprintf(os.Stderr, "vegaload mcp eval: -output %q: want text or json\n", output)
		return 2
	}

	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload mcp eval: locating the vegaload binary: %v\n", err)
		return 1
	}

	workdir, err := os.MkdirTemp("", "vegaload-eval-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload mcp eval: creating a working directory: %v\n", err)
		return 1
	}
	defer os.RemoveAll(workdir) //nolint:errcheck

	if err := writeEvalFixtures(workdir); err != nil {
		fmt.Fprintf(os.Stderr, "vegaload mcp eval: writing fixtures: %v\n", err)
		return 1
	}

	suiteJSON := bytes.ReplaceAll(eval.RawV1(), []byte("${WORKDIR}"), []byte(workdir))
	suite, err := eval.ParseSuite(suiteJSON)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload mcp eval: parsing the embedded suite: %v\n", err)
		return 1
	}

	caller := toolCaller(mcp.NewTools(exePath))
	results := eval.Run(context.Background(), caller, suite.Cases)

	failed := false
	for _, r := range results {
		if !r.Passed {
			failed = true
		}
	}

	if output == "json" {
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{
			"version": suite.Version,
			"results": results,
			"passed":  !failed,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload mcp eval: %v\n", err)
			return 1
		}
	} else {
		for _, r := range results {
			status := "PASS"
			if !r.Passed {
				status = "FAIL"
			}
			if r.Detail != "" {
				fmt.Printf("%-4s %s (%s)\n", status, r.Name, r.Detail)
			} else {
				fmt.Printf("%-4s %s\n", status, r.Name)
			}
		}
		fmt.Printf("\n%s: %d/%d cases passed (suite %s)\n", map[bool]string{true: "FAIL", false: "ok"}[failed], passCount(results), len(results), suite.Version)
	}

	if failed {
		return 1
	}
	return 0
}

// toolCaller adapts an internal/mcp.Tool slice into an eval.Caller,
// looking the tool up by name on every call rather than once, since
// eval.Caller's signature carries only the tool's name, not a resolved
// handler.
func toolCaller(tools []mcp.Tool) eval.Caller {
	byName := make(map[string]mcp.Tool, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}
	return func(ctx context.Context, name string, arguments json.RawMessage) (any, error) {
		t, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("eval: no such tool %q", name)
		}
		return t.Handler(ctx, arguments)
	}
}

func passCount(results []eval.CaseResult) int {
	n := 0
	for _, r := range results {
		if r.Passed {
			n++
		}
	}
	return n
}

// writeEvalFixtures writes the fixture files the embedded v1 suite
// references by "${WORKDIR}"-relative path:
//
//   - fixture-report.json: a report.Result whose exact numbers the
//     suite's get_results/suggest_thresholds cases are written against
//     (Total=100, Failed=0, ErrorRate=0, Latency.P95=50ms — which
//     SuggestThresholds turns into a "60ms" latency_p95: 50ms * 1.2
//     rounded up to the nearest 10ms).
//   - fixture-report-worse.json: same shape with a higher error rate,
//     for the compare_reports case that expects regressed=true.
//   - fixture-spec.json: a one-endpoint OpenAPI document, for the
//     generate_from_spec case that expects "endpoints": 1.
func writeEvalFixtures(workdir string) error {
	res := &report.Result{
		Executor:  "fixed-vus",
		StartedAt: time.Now(),
		Elapsed:   30 * time.Second,
		Total:     100,
		Failed:    0,
		ErrorRate: 0,
		Latency: report.Latency{
			Min:  5 * time.Millisecond,
			Mean: 20 * time.Millisecond,
			Max:  80 * time.Millisecond,
			P50:  18 * time.Millisecond,
			P90:  45 * time.Millisecond,
			P95:  50 * time.Millisecond,
			P99:  70 * time.Millisecond,
		},
	}
	if err := report.WriteJSON(workdir+"/fixture-report.json", res); err != nil {
		return fmt.Errorf("fixture-report.json: %w", err)
	}

	worse := *res
	worse.Failed = 10
	worse.ErrorRate = 0.1
	worse.Latency.P95 = 120 * time.Millisecond
	if err := report.WriteJSON(workdir+"/fixture-report-worse.json", &worse); err != nil {
		return fmt.Errorf("fixture-report-worse.json: %w", err)
	}

	const spec = `{
  "openapi": "3.0.0",
  "info": {"title": "Widgets API"},
  "servers": [{"url": "https://api.example.com"}],
  "paths": {
    "/widgets": {
      "get": {"summary": "List widgets"}
    }
  }
}`
	if err := os.WriteFile(workdir+"/fixture-spec.json", []byte(spec), 0o644); err != nil {
		return fmt.Errorf("fixture-spec.json: %w", err)
	}
	return nil
}
