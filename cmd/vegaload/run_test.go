package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/engine"
	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/safety"
)

func TestParseStages(t *testing.T) {
	stages, err := parseStages("10:30s, 0:10s")
	if err != nil {
		t.Fatalf("parseStages returned error: %v", err)
	}
	if len(stages) != 2 {
		t.Fatalf("len(stages) = %d, want 2", len(stages))
	}
	if stages[0].Target != 10 || stages[0].Duration != 30*time.Second {
		t.Errorf("stages[0] = %+v, want {10, 30s}", stages[0])
	}
	if stages[1].Target != 0 || stages[1].Duration != 10*time.Second {
		t.Errorf("stages[1] = %+v, want {0, 10s}", stages[1])
	}
}

func TestParseStages_InvalidForm(t *testing.T) {
	cases := []string{"10", "10:30s:extra", "x:30s", "10:notaduration"}
	for _, c := range cases {
		if _, err := parseStages(c); err == nil {
			t.Errorf("parseStages(%q) = nil error, want an error", c)
		}
	}
}

func TestParseRunArgs_RequiresScenarioOrTarget(t *testing.T) {
	if _, err := parseRunArgs([]string{}); err == nil {
		t.Error("expected an error when neither a scenario file nor -target/-protocol is given")
	}
}

func TestParseRunArgs_RejectsBothModes(t *testing.T) {
	// Flags must precede the positional scenario file — Go's flag
	// package stops parsing flags at the first non-flag argument, so
	// "-target"/"-protocol" placed after the scenario path would be
	// silently left as extra positional args instead of being parsed.
	_, err := parseRunArgs([]string{"-target", "http://x", "-protocol", "http1", "scenario.vl.js"})
	if err == nil {
		t.Error("expected an error when both a scenario file and -target/-protocol are given")
	}
}

func TestParseRunArgs_ProtocolDirectMode(t *testing.T) {
	cfg, err := parseRunArgs([]string{
		"-target", "http://example.invalid",
		"-protocol", "http1",
		"-vus", "5",
		"-duration", "2s",
		"-header", "X-Test: 1",
	})
	if err != nil {
		t.Fatalf("parseRunArgs returned error: %v", err)
	}
	if cfg.Target.URL != "http://example.invalid" || cfg.Protocol != "http1" {
		t.Errorf("unexpected target/protocol: %+v", cfg)
	}
	if cfg.VUs != 5 || cfg.Duration != 2*time.Second {
		t.Errorf("unexpected vus/duration: %+v", cfg)
	}
	if cfg.Target.Headers["X-Test"] != "1" {
		t.Errorf("header not parsed: %+v", cfg.Target.Headers)
	}
}

func TestBuildExecutor_Validation(t *testing.T) {
	cases := []struct {
		name    string
		cfg     runConfig
		wantErr bool
	}{
		{"fixed-vus ok", runConfig{Executor: "fixed-vus", VUs: 1, Duration: time.Second}, false},
		{"ramp missing stages", runConfig{Executor: "ramp"}, true},
		{"constant-arrival-rate missing rate", runConfig{Executor: "constant-arrival-rate"}, true},
		{"unknown executor", runConfig{Executor: "bogus"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := buildExecutor(&c.cfg)
			if c.wantErr && err == nil {
				t.Error("expected an error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestRunScenario_ProtocolDirectHTTP1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &runConfig{
		Executor: "fixed-vus",
		VUs:      2,
		Duration: 150 * time.Millisecond,
		Protocol: "http1",
		Target:   protocol.Target{URL: srv.URL},
		Timeout:  time.Second,
	}

	result, err := runScenario(cfg)
	if err != nil {
		t.Fatalf("runScenario returned error: %v", err)
	}
	if result.Total == 0 {
		t.Error("expected at least one recorded iteration")
	}
	if result.Failed != 0 {
		t.Errorf("Failed = %d, want 0 (run-end cancellations must not count as failures)", result.Failed)
	}
}

func TestRunScenario_ScriptedJS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scenario.vl.js")
	src := `export default function () {}`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("writing scenario: %v", err)
	}

	cfg := &runConfig{
		Executor:     "fixed-vus",
		VUs:          3,
		Duration:     150 * time.Millisecond,
		ScenarioPath: path,
		Timeout:      time.Second,
	}

	result, err := runScenario(cfg)
	if err != nil {
		t.Fatalf("runScenario returned error: %v", err)
	}
	if result.Total == 0 {
		t.Error("expected at least one recorded iteration")
	}
	if result.Failed != 0 {
		t.Errorf("Failed = %d, want 0 (run-end cancellations must not count as failures)", result.Failed)
	}
}

func TestRunScenario_ScriptedJS_BadScriptFailsFast(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scenario.vl.js")
	if err := os.WriteFile(path, []byte("no default export here"), 0o644); err != nil {
		t.Fatalf("writing scenario: %v", err)
	}

	cfg := &runConfig{
		Executor:     "fixed-vus",
		VUs:          1,
		Duration:     time.Second,
		ScenarioPath: path,
	}

	if _, err := runScenario(cfg); err == nil {
		t.Error("expected runScenario to fail fast for a script with no default export")
	}
}

func TestParseRunArgs_FlagsAfterPositionalAreNotParsed(t *testing.T) {
	// Documents a real limitation of Go's flag package rather than
	// hiding it: flags given after the scenario file are left as
	// extra positional args, not parsed. -vus keeps its default (1)
	// here instead of becoming 10.
	cfg, err := parseRunArgs([]string{"scenario.vl.js", "-vus", "10"})
	if err != nil {
		t.Fatalf("parseRunArgs returned error: %v", err)
	}
	if cfg.VUs != 1 {
		t.Errorf("VUs = %d, want 1 (the default) — -vus after the positional arg should not be parsed", cfg.VUs)
	}
}

func noConfirm(t *testing.T) func(io.Reader, io.Writer, string) bool {
	return func(io.Reader, io.Writer, string) bool {
		t.Fatal("confirm should not have been called")
		return false
	}
}

func TestEnforceSafety_LocalTargetNoPrompt(t *testing.T) {
	cfg := &runConfig{
		VUs: 1, Duration: time.Second, Limits: safety.DefaultLimits,
		Target: protocol.Target{URL: "http://localhost:8080"},
	}
	if err := enforceSafety(cfg, strings.NewReader(""), &bytes.Buffer{}, noConfirm(t)); err != nil {
		t.Errorf("localhost target should not require confirmation: %v", err)
	}
}

func TestEnforceSafety_NonLocalRequiresConfirmation(t *testing.T) {
	cfg := &runConfig{
		VUs: 1, Duration: time.Second, Limits: safety.DefaultLimits,
		Target: protocol.Target{URL: "http://example.com"},
	}

	// Declined: refuse.
	err := enforceSafety(cfg, strings.NewReader("n\n"), &bytes.Buffer{}, promptConfirm)
	if err == nil {
		t.Error("expected an error when confirmation is declined")
	}

	// Accepted: proceed.
	err = enforceSafety(cfg, strings.NewReader("y\n"), &bytes.Buffer{}, promptConfirm)
	if err != nil {
		t.Errorf("expected no error when confirmation is accepted, got: %v", err)
	}
}

func TestEnforceSafety_YesSkipsPrompt(t *testing.T) {
	cfg := &runConfig{
		VUs: 1, Duration: time.Second, Limits: safety.DefaultLimits,
		Target: protocol.Target{URL: "http://example.com"},
		Yes:    true,
	}
	if err := enforceSafety(cfg, strings.NewReader(""), &bytes.Buffer{}, noConfirm(t)); err != nil {
		t.Errorf("unexpected error with -yes: %v", err)
	}
}

func TestEnforceSafety_AllowTarget(t *testing.T) {
	cfg := &runConfig{
		VUs: 1, Duration: time.Second, Limits: safety.DefaultLimits,
		Target:       protocol.Target{URL: "http://example.com"},
		AllowTargets: []string{"example.com"},
	}
	if err := enforceSafety(cfg, strings.NewReader(""), &bytes.Buffer{}, noConfirm(t)); err != nil {
		t.Errorf("unexpected error with -allow-target: %v", err)
	}
}

func TestEnforceSafety_RejectsOverCap(t *testing.T) {
	cfg := &runConfig{
		VUs: 999999, Duration: time.Second, Limits: safety.DefaultLimits,
	}
	if err := enforceSafety(cfg, strings.NewReader(""), &bytes.Buffer{}, promptConfirm); err == nil {
		t.Error("expected an error for a vus count over the cap")
	}
}

func TestEnforceSafety_RampPeakOverCap(t *testing.T) {
	limits := safety.Limits{MaxVUs: 50, MaxDuration: time.Hour, MaxRate: 1000}
	cfg := &runConfig{
		Executor: "ramp",
		VUs:      1,
		Duration: time.Minute,
		Stages:   []engine.Stage{{Target: 100, Duration: 30 * time.Second}},
		Limits:   limits,
	}
	if err := enforceSafety(cfg, strings.NewReader(""), &bytes.Buffer{}, promptConfirm); err == nil {
		t.Error("expected an error when a ramp stage's target VU count exceeds the cap")
	}
}

func TestRunJSONL_EmitsStartedProgressAndFinished(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &runConfig{
		Executor: "fixed-vus", VUs: 2, Duration: 1200 * time.Millisecond,
		Protocol: "http1", Target: protocol.Target{URL: srv.URL}, Timeout: time.Second,
	}

	var buf bytes.Buffer
	result, err := runJSONL(cfg, &buf)
	if err != nil {
		t.Fatalf("runJSONL returned error: %v", err)
	}
	if result.Total == 0 {
		t.Error("expected at least one recorded iteration")
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("got %d JSONL lines, want at least run_started and run_finished", len(lines))
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first line is not valid JSON: %v", err)
	}
	if first["type"] != "run_started" {
		t.Errorf("first line type = %v, want run_started", first["type"])
	}
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("last line is not valid JSON: %v", err)
	}
	if last["type"] != "run_finished" {
		t.Errorf("last line type = %v, want run_finished", last["type"])
	}
}

func TestWriteAndMaybeOpenReport_WritesSelfContainedHTML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.html")
	cfg := &runConfig{OutputMode: "text", ReportPath: path, NoOpen: true}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	runCfg := &runConfig{Executor: "fixed-vus", VUs: 1, Duration: 50 * time.Millisecond, Protocol: "http1", Target: protocol.Target{URL: srv.URL}, Timeout: time.Second}
	result, err := runScenario(runCfg)
	if err != nil {
		t.Fatalf("runScenario: %v", err)
	}

	if err := writeAndMaybeOpenReport(cfg, result); err != nil {
		t.Fatalf("writeAndMaybeOpenReport: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("report file was not written: %v", err)
	}
}

func TestParseRunArgs_OutputModeValidation(t *testing.T) {
	if _, err := parseRunArgs([]string{"-output", "bogus", "-target", "http://localhost", "-protocol", "http1"}); err == nil {
		t.Error("expected an error for an invalid -output mode")
	}
	cfg, err := parseRunArgs([]string{"-output", "jsonl", "-target", "http://localhost", "-protocol", "http1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OutputMode != "jsonl" {
		t.Errorf("OutputMode = %q, want jsonl", cfg.OutputMode)
	}
}

func TestCmdRun_ExitCodes(t *testing.T) {
	if code := cmdRun([]string{}); code == 0 {
		t.Error("expected a non-zero exit code when no mode is given")
	}
	if code := cmdRun([]string{"-h"}); code != 0 {
		t.Errorf("exit code for -h = %d, want 0", code)
	}
}
