package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/report"
)

func writeCompareFixture(t *testing.T, dir, name string, res *report.Result) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := report.WriteJSON(path, res); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	return path
}

func TestCmdCompare_OkAndRegressed(t *testing.T) {
	dir := t.TempDir()
	base := &report.Result{
		Executor:  "fixed-vus",
		Elapsed:   10 * time.Second,
		Total:     100,
		Failed:    0,
		ErrorRate: 0,
		Latency:   report.Latency{P95: 50 * time.Millisecond, P50: 20 * time.Millisecond},
	}
	ok := *base
	worse := *base
	worse.ErrorRate = 0.1
	worse.Failed = 10

	baseline := writeCompareFixture(t, dir, "baseline.json", base)
	candidateOK := writeCompareFixture(t, dir, "ok.json", &ok)
	candidateBad := writeCompareFixture(t, dir, "bad.json", &worse)

	if code := cmdCompare([]string{baseline, candidateOK}); code != 0 {
		t.Errorf("compare ok pair exited %d, want 0", code)
	}
	if code := cmdCompare([]string{"-output", "json", baseline, candidateBad}); code != 1 {
		t.Errorf("compare regressed pair exited %d, want 1", code)
	}
}

func TestCmdCompare_UsageErrors(t *testing.T) {
	if code := cmdCompare([]string{}); code != 2 {
		t.Errorf("no args exited %d, want 2", code)
	}
	if code := cmdCompare([]string{"-output", "bogus", "a.json", "b.json"}); code != 2 {
		t.Errorf("bad -output exited %d, want 2", code)
	}
	if code := cmdCompare([]string{"/nonexistent/a.json", "/nonexistent/b.json"}); code != 1 {
		t.Errorf("missing files exited %d, want 1", code)
	}
}
