package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vegaload/vegaload/internal/report"
)

// argRecordingBinary writes an executable Go-free shell script that
// records the arguments it was called with (one per line, to a file
// next to it) and prints respStdout, so a tool's handler can be
// exercised end to end while letting the test assert on exactly which
// CLI flags that handler built.
func argRecordingBinary(t *testing.T, respStdout string, exitCode int) (binPath, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	binPath = filepath.Join(dir, "fake-vegaload")
	argsFile = filepath.Join(dir, "args.txt")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %s\nprintf '%%s' %s\nexit %d\n",
		shQuote(argsFile), shQuote(respStdout), exitCode)
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake binary: %v", err)
	}
	return binPath, argsFile
}

func readArgs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading recorded args: %v", err)
	}
	s := strings.TrimRight(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func callTool(t *testing.T, tools []Tool, name string, args map[string]any) (any, error) {
	t.Helper()
	var tool *Tool
	for i := range tools {
		if tools[i].Name == name {
			tool = &tools[i]
		}
	}
	if tool == nil {
		t.Fatalf("no tool named %q", name)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshaling arguments: %v", err)
	}
	return tool.Handler(context.Background(), raw)
}

func TestCreateScenarioTool_BuildsArgs(t *testing.T) {
	bin, argsFile := argRecordingBinary(t, `{"path":"scenario.vl.js","created":true}`, 0)
	tools := NewTools(bin)
	result, err := callTool(t, tools, "create_scenario", map[string]any{"name": "myscenario", "python": true, "force": true})
	if err != nil {
		t.Fatalf("create_scenario returned error: %v", err)
	}
	m := result.(map[string]any)
	if m["path"] != "scenario.vl.js" {
		t.Errorf("result = %v, want path scenario.vl.js", m)
	}
	args := readArgs(t, argsFile)
	wantSuffix := []string{"-python", "-force", "-output", "json", "myscenario"}
	if !endsWithArgs(args, wantSuffix) {
		t.Errorf("args = %v, want to end with %v (after \"new\")", args, wantSuffix)
	}
	if args[0] != "new" {
		t.Errorf("args[0] = %q, want \"new\"", args[0])
	}
}

func TestRunTestTool_RequiresScenarioOrTarget(t *testing.T) {
	bin, _ := argRecordingBinary(t, "{}", 0)
	tools := NewTools(bin)
	if _, err := callTool(t, tools, "run_test", map[string]any{}); err == nil {
		t.Error("expected an error when neither scenario_path nor target/protocol is given")
	}
}

func TestRunTestTool_RejectsBothModes(t *testing.T) {
	bin, _ := argRecordingBinary(t, "{}", 0)
	tools := NewTools(bin)
	_, err := callTool(t, tools, "run_test", map[string]any{
		"scenario_path": "s.vl.js", "target": "https://example.com", "protocol": "http1",
	})
	if err == nil {
		t.Error("expected an error when both scenario_path and target/protocol are given")
	}
}

func TestRunTestTool_BuildsArgsAndParsesResult(t *testing.T) {
	resultJSON, _ := json.Marshal(report.Result{Executor: "fixed-vus", Total: 10, Failed: 1, ErrorRate: 0.1})
	bin, argsFile := argRecordingBinary(t, string(resultJSON), 0)
	tools := NewTools(bin)

	out, err := callTool(t, tools, "run_test", map[string]any{
		"target": "https://example.com", "protocol": "http1", "vus": 5, "duration": "10s", "yes": true,
	})
	if err != nil {
		t.Fatalf("run_test returned error: %v", err)
	}
	m := out.(map[string]any)
	res := m["result"].(report.Result)
	if res.Total != 10 || res.Failed != 1 {
		t.Errorf("result = %+v, want Total=10 Failed=1", res)
	}
	if m["report_path"] == nil || m["report_path"].(string) == "" {
		t.Error("expected a generated report_path when none was given")
	}

	args := readArgs(t, argsFile)
	for _, want := range []string{"-target", "https://example.com", "-protocol", "http1", "-vus", "5", "-duration", "10s", "-yes", "-trigger", "mcp", "-no-open"} {
		if !containsArg(args, want) {
			t.Errorf("args %v missing %q", args, want)
		}
	}
}

func TestRunTestTool_NoReportSkipsReportFlag(t *testing.T) {
	resultJSON, _ := json.Marshal(report.Result{Executor: "fixed-vus"})
	bin, argsFile := argRecordingBinary(t, string(resultJSON), 0)
	tools := NewTools(bin)

	out, err := callTool(t, tools, "run_test", map[string]any{
		"target": "https://example.com", "protocol": "http1", "yes": true, "no_report": true,
	})
	if err != nil {
		t.Fatalf("run_test returned error: %v", err)
	}
	m := out.(map[string]any)
	if _, ok := m["report_path"]; ok {
		t.Errorf("did not expect report_path when no_report is set, got %v", m)
	}
	args := readArgs(t, argsFile)
	if containsArg(args, "-report") {
		t.Errorf("did not expect -report in args: %v", args)
	}
	if !containsArg(args, "-no-report") {
		t.Errorf("expected -no-report in args: %v", args)
	}
}

func TestGetResultsTool_ReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	res := &report.Result{Executor: "fixed-vus", Total: 42}
	if err := report.WriteJSON(path, res); err != nil {
		t.Fatalf("report.WriteJSON: %v", err)
	}
	tools := NewTools("/unused")
	out, err := callTool(t, tools, "get_results", map[string]any{"report_path": path})
	if err != nil {
		t.Fatalf("get_results returned error: %v", err)
	}
	got := out.(report.Result)
	if got.Total != 42 {
		t.Errorf("Total = %d, want 42", got.Total)
	}
}

func TestGetResultsTool_RequiresPath(t *testing.T) {
	tools := NewTools("/unused")
	if _, err := callTool(t, tools, "get_results", map[string]any{}); err == nil {
		t.Error("expected an error when report_path is missing")
	}
}

func TestSuggestThresholdsTool_ParsesNestedField(t *testing.T) {
	stdout := `{"report_path":"r.json","findings":{"notes":[]},"suggested_thresholds":{"latency_p95":"100ms","error_rate":0.01,"error_rate_pct":"1.0%"}}`
	bin, argsFile := argRecordingBinary(t, stdout, 0)
	tools := NewTools(bin)

	out, err := callTool(t, tools, "suggest_thresholds", map[string]any{"report_path": "r.json"})
	if err != nil {
		t.Fatalf("suggest_thresholds returned error: %v", err)
	}
	m := out.(map[string]any)
	if m["latency_p95"] != "100ms" {
		t.Errorf("latency_p95 = %v, want 100ms", m["latency_p95"])
	}
	args := readArgs(t, argsFile)
	if args[0] != "diagnose" || !containsArg(args, "-no-llm") || !containsArg(args, "r.json") {
		t.Errorf("args = %v, want diagnose ... -no-llm ... r.json", args)
	}
}

func TestDiagnoseFailureTool_BuildsArgs(t *testing.T) {
	stdout := `{"report_path":"r.json","findings":{"notes":["ok"]},"suggested_thresholds":{}}`
	bin, argsFile := argRecordingBinary(t, stdout, 0)
	tools := NewTools(bin)

	out, err := callTool(t, tools, "diagnose_failure", map[string]any{"report_path": "r.json"})
	if err != nil {
		t.Fatalf("diagnose_failure returned error: %v", err)
	}
	m := out.(map[string]any)
	if m["report_path"] != "r.json" {
		t.Errorf("report_path = %v, want r.json", m["report_path"])
	}
	args := readArgs(t, argsFile)
	if args[0] != "diagnose" || containsArg(args, "-no-llm") {
		t.Errorf("args = %v, want diagnose ... (no -no-llm by default)", args)
	}
}

func TestDiagnoseFailureTool_NoLLMFlag(t *testing.T) {
	bin, argsFile := argRecordingBinary(t, `{"report_path":"r.json"}`, 0)
	tools := NewTools(bin)
	if _, err := callTool(t, tools, "diagnose_failure", map[string]any{"report_path": "r.json", "no_llm": true}); err != nil {
		t.Fatalf("diagnose_failure returned error: %v", err)
	}
	args := readArgs(t, argsFile)
	if !containsArg(args, "-no-llm") {
		t.Errorf("expected -no-llm in args: %v", args)
	}
}

func TestCompareReportsTool_BuildsArgs(t *testing.T) {
	stdout := `{"baseline_path":"b.json","candidate_path":"c.json","regressed":false,"metrics":[],"notes":["no regressions against the baseline"]}`
	bin, argsFile := argRecordingBinary(t, stdout, 0)
	tools := NewTools(bin)

	out, err := callTool(t, tools, "compare_reports", map[string]any{
		"baseline_path":    "b.json",
		"candidate_path":   "c.json",
		"error_rate_delta": 0.01,
		"p95_ratio":        1.2,
	})
	if err != nil {
		t.Fatalf("compare_reports returned error: %v", err)
	}
	m := out.(map[string]any)
	if m["regressed"] != false {
		t.Errorf("regressed = %v, want false", m["regressed"])
	}
	args := readArgs(t, argsFile)
	if args[0] != "compare" || !containsArg(args, "-output") || !containsArg(args, "b.json") || !containsArg(args, "c.json") {
		t.Errorf("args = %v, want compare -output json ... b.json c.json", args)
	}
	if !containsArg(args, "-error-rate-delta") || !containsArg(args, "-p95-ratio") {
		t.Errorf("expected slack flags in args: %v", args)
	}
}

func TestCompareReportsTool_RegressionExitStillReturnsResult(t *testing.T) {
	stdout := `{"baseline_path":"b.json","candidate_path":"c.json","regressed":true,"metrics":[],"notes":["error rate rose"]}`
	bin, _ := argRecordingBinary(t, stdout, 1)
	tools := NewTools(bin)

	out, err := callTool(t, tools, "compare_reports", map[string]any{
		"baseline_path":  "b.json",
		"candidate_path": "c.json",
	})
	if err != nil {
		t.Fatalf("compare_reports should return structured result on regression exit, got error: %v", err)
	}
	m := out.(map[string]any)
	if m["regressed"] != true {
		t.Errorf("regressed = %v, want true", m["regressed"])
	}
}

func TestCompareReportsTool_RequiresPaths(t *testing.T) {
	tools := NewTools("/unused")
	if _, err := callTool(t, tools, "compare_reports", map[string]any{}); err == nil {
		t.Error("expected an error when paths are missing")
	}
}

func TestGenerateFromSpecTool_BuildsArgs(t *testing.T) {
	stdout := `{"path":"widgets.vegaload-plan.md","created":true,"endpoints":3}`
	bin, argsFile := argRecordingBinary(t, stdout, 0)
	tools := NewTools(bin)

	out, err := callTool(t, tools, "generate_from_spec", map[string]any{"spec_path": "spec.json", "name": "widgets", "force": true})
	if err != nil {
		t.Fatalf("generate_from_spec returned error: %v", err)
	}
	m := out.(map[string]any)
	if m["path"] != "widgets.vegaload-plan.md" {
		t.Errorf("path = %v, want widgets.vegaload-plan.md", m["path"])
	}
	args := readArgs(t, argsFile)
	wantSuffix := []string{"-output", "json", "-force", "widgets"}
	if args[0] != "new" || !containsArg(args, "-from-openapi") || !containsArg(args, "spec.json") || !endsWithArgs(args, wantSuffix) {
		t.Errorf("args = %v, want new -from-openapi spec.json ... %v", args, wantSuffix)
	}
}

func TestGenerateFromSpecTool_RequiresSpecPath(t *testing.T) {
	tools := NewTools("/unused")
	if _, err := callTool(t, tools, "generate_from_spec", map[string]any{}); err == nil {
		t.Error("expected an error when spec_path is missing")
	}
}

func TestRunTestTool_PropagatesCLIFailure(t *testing.T) {
	bin, _ := argRecordingBinary(t, "", 1)
	tools := NewTools(bin)
	if _, err := callTool(t, tools, "run_test", map[string]any{"target": "https://example.com", "protocol": "http1", "yes": true}); err == nil {
		t.Error("expected an error when the underlying CLI command exits non-zero")
	}
}

// --- small helpers ---

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func endsWithArgs(args []string, suffix []string) bool {
	if len(suffix) > len(args) {
		return false
	}
	return strings.Join(args[len(args)-len(suffix):], "\x00") == strings.Join(suffix, "\x00")
}
