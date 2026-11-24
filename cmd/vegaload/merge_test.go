package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vegaload/vegaload/internal/report"
)

// runToFile runs a short paced -protocol load against url and returns the
// path of its JSON report.
func runToFile(t *testing.T, dir, name, url string, extra ...string) string {
	t.Helper()
	out := filepath.Join(dir, name)
	args := append([]string{
		"-target", url, "-protocol", "http1",
		"-executor", "constant-arrival-rate", "-rate", "40", "-max-vus", "4", "-duration", "1s",
		"-no-report", "-no-live", "-audit-log", filepath.Join(dir, "audit.log"), "-out", out,
	}, extra...)
	if code, _, stderr := captureCmd(t, func() int { return cmdRun(args) }); code != 0 {
		t.Fatalf("run %s exited %d: %s", name, code, stderr)
	}
	return out
}

func readResult(t *testing.T, path string) *report.Result {
	t.Helper()
	res, err := loadReportJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestCmdMerge_EndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fail") != "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	files := []string{
		runToFile(t, dir, "a.json", srv.URL),
		runToFile(t, dir, "b.json", srv.URL+"/?fail=1"),
		runToFile(t, dir, "c.json", srv.URL, "-exact-percentiles"),
	}
	var total, failed int64
	for _, f := range files {
		r := readResult(t, f)
		if r.Schema != report.Schema || r.Latency.Histogram == nil || r.Latency.Histogram.Count() != r.Total {
			t.Fatalf("%s has no histogram for its %d requests (schema %q)", f, r.Total, r.Schema)
		}
		total += r.Total
		failed += r.Failed
	}

	merged := filepath.Join(dir, "merged.json")
	html := filepath.Join(dir, "merged.html")
	code, stdout, stderr := captureCmd(t, func() int {
		return cmdMerge(append([]string{"-output", "json", "-out", merged, "-report", html, "-threshold", "total >= 1"}, files...))
	})
	if code != 0 {
		t.Fatalf("merge exited %d: %s", code, stderr)
	}
	var printed report.Result
	if err := json.Unmarshal([]byte(stdout), &printed); err != nil {
		t.Fatalf("-output json is not a report: %v\n%s", err, stdout)
	}
	res := readResult(t, merged)
	if res.MergedFrom != 3 || res.Total != total || res.Failed != failed || printed.Total != total {
		t.Fatalf("merged_from %d total %d failed %d, want 3, %d, %d", res.MergedFrom, res.Total, res.Failed, total, failed)
	}
	if res.Latency.Histogram.Count() != total || res.ThresholdsPassed == nil || !*res.ThresholdsPassed {
		t.Fatalf("merged latency %+v thresholds %v", res.Latency, res.ThresholdsPassed)
	}
	if len(res.FailureReasons) != 1 || res.FailureReasons[0].Reason != "status 503" || res.FailureReasons[0].Count != failed {
		t.Fatalf("failure reasons %+v", res.FailureReasons)
	}
	if _, err := os.Stat(html); err != nil {
		t.Fatalf("-report: %v", err)
	}

	// The text summary is run's.
	code, stdout, _ = captureCmd(t, func() int { return cmdMerge(files) })
	if code != 0 || !strings.Contains(stdout, "merged 3 reports") || !strings.Contains(stdout, "constant-arrival-rate summary") {
		t.Fatalf("text output (exit %d):\n%s", code, stdout)
	}

	// A threshold the merged result breaks exits 3.
	code, _, stderr = captureCmd(t, func() int { return cmdMerge(append([]string{"-threshold", "error_rate < 1%"}, files...)) })
	if code != exitThresholdsBreached || !strings.Contains(stderr, "thresholds breached: error_rate") {
		t.Fatalf("a breached threshold exited %d: %s", code, stderr)
	}

	// A merged report merges again.
	code, _, stderr = captureCmd(t, func() int { return cmdMerge([]string{"-out", filepath.Join(dir, "again.json"), merged, files[0]}) })
	if code != 0 || readResult(t, filepath.Join(dir, "again.json")).MergedFrom != 4 {
		t.Fatalf("merging a merged report exited %d: %s", code, stderr)
	}
}

func TestCmdMerge_RefusesAnOlderReport(t *testing.T) {
	dir := t.TempDir()
	cur := filepath.Join(dir, "new.json")
	c := report.NewCollector()
	c.RecordCall("http", "GET", 1, nil, 0, 0)
	if err := report.WriteJSON(cur, c.Finish("fixed-vus", 1)); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "old.json")
	legacy := `{"executor":"fixed-vus","total":10,"failed":0,"error_rate":0,"elapsed_ns":10000000000,"latency":{"p95_ns":50000000,"p50_ns":20000000},"time_series":[]}`
	if err := os.WriteFile(old, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := captureCmd(t, func() int { return cmdMerge([]string{cur, old}) })
	if code != 1 || !strings.Contains(stderr, old) || !strings.Contains(stderr, "older vegaload, without latency histograms") {
		t.Fatalf("an older report exited %d: %s", code, stderr)
	}
	// It still loads for compare and -baseline.
	if res, err := loadReportJSON(old); err != nil || res.Latency.P95 == 0 {
		t.Fatalf("loadReportJSON: %v", err)
	}
}

func TestCmdMerge_UsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"one.json"},
		{"-output", "yaml", "a.json", "b.json"},
		{"-threshold", "p95 <", "a.json", "b.json"},
		{"-no-such-flag", "a.json", "b.json"},
	} {
		if code, _, _ := captureCmd(t, func() int { return cmdMerge(args) }); code != 2 {
			t.Errorf("merge %q exited %d, want 2", args, code)
		}
	}
	if code, _, _ := captureCmd(t, func() int { return cmdMerge([]string{"/nonexistent/a.json", "/nonexistent/b.json"}) }); code != 1 {
		t.Errorf("missing files exited %d, want 1", code)
	}
	if code := run([]string{"merge", "-h"}); code != 0 {
		t.Errorf("merge -h exited %d", code)
	}
}

func TestRunExactPercentilesFlag(t *testing.T) {
	cfg, err := parseRunArgs([]string{"-target", "http://127.0.0.1:1", "-protocol", "http1", "-exact-percentiles"})
	if err != nil || !cfg.ExactPercentiles {
		t.Fatalf("parse: %v %+v", err, cfg)
	}
	cfg, err = parseRunArgs([]string{"-target", "http://127.0.0.1:1", "-protocol", "http1"})
	if err != nil || cfg.ExactPercentiles {
		t.Fatalf("the default should estimate from histograms: %v", err)
	}
}
