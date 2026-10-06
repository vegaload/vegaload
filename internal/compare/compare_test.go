package compare

import (
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/report"
)

func baseResult() *report.Result {
	return &report.Result{
		Executor:  "fixed-vus",
		Elapsed:   10 * time.Second,
		Total:     1000,
		Failed:    10,
		ErrorRate: 0.01,
		Latency: report.Latency{
			Mean: 20 * time.Millisecond,
			P50:  18 * time.Millisecond,
			P90:  40 * time.Millisecond,
			P95:  50 * time.Millisecond,
			P99:  80 * time.Millisecond,
		},
	}
}

func TestCompare_NoRegression(t *testing.T) {
	base := baseResult()
	cand := baseResult()
	cand.ErrorRate = 0.005
	cand.Failed = 5
	cand.Latency.P95 = 45 * time.Millisecond

	got := Compare(base, cand, Options{})
	if got.Regressed {
		t.Fatalf("Regressed = true, want false; notes=%v", got.Notes)
	}
	if len(got.Notes) == 0 || got.Notes[0] != "no regressions against the baseline" {
		t.Errorf("Notes = %v, want reassuring note", got.Notes)
	}
}

func TestCompare_ErrorRateRegression(t *testing.T) {
	base := baseResult()
	cand := baseResult()
	cand.ErrorRate = 0.05
	cand.Failed = 50

	got := Compare(base, cand, Options{})
	if !got.Regressed {
		t.Fatal("expected Regressed true for higher error rate")
	}
	if m := metric(got, "error_rate"); m == nil || !m.Regressed {
		t.Errorf("error_rate metric = %+v, want Regressed true", m)
	}
}

func TestCompare_P95Regression(t *testing.T) {
	base := baseResult()
	cand := baseResult()
	cand.Latency.P95 = 80 * time.Millisecond

	got := Compare(base, cand, Options{})
	if !got.Regressed {
		t.Fatal("expected Regressed true for higher p95")
	}
	if m := metric(got, "latency.p95"); m == nil || !m.Regressed {
		t.Errorf("latency.p95 metric = %+v, want Regressed true", m)
	}
}

func TestCompare_P95RatioAllowsHeadroom(t *testing.T) {
	base := baseResult()
	cand := baseResult()
	cand.Latency.P95 = 55 * time.Millisecond // 10% over 50ms

	got := Compare(base, cand, Options{P95Ratio: 1.2})
	if got.Regressed {
		t.Fatalf("Regressed = true with 1.2 ratio and 10%% p95 rise; notes=%v", got.Notes)
	}
	if m := metric(got, "latency.p95"); m == nil || m.Regressed {
		t.Errorf("latency.p95 should not regress under ratio slack: %+v", m)
	}
}

func TestCompare_ErrorRateDeltaSlack(t *testing.T) {
	base := baseResult()
	cand := baseResult()
	cand.ErrorRate = 0.015 // +0.5pp

	got := Compare(base, cand, Options{ErrorRateDelta: 0.01})
	if got.Regressed {
		t.Fatalf("Regressed = true with 1pp slack and 0.5pp rise; notes=%v", got.Notes)
	}
}

func TestCompare_HigherP99AloneIsNotRegression(t *testing.T) {
	base := baseResult()
	cand := baseResult()
	cand.Latency.P99 = 200 * time.Millisecond

	got := Compare(base, cand, Options{})
	if got.Regressed {
		t.Fatalf("p99-only change should be informational, got Regressed; notes=%v", got.Notes)
	}
	if m := metric(got, "latency.p99"); m == nil || m.Delta <= 0 {
		t.Errorf("expected positive p99 delta, got %+v", m)
	}
}

func metric(r Result, name string) *Metric {
	for i := range r.Metrics {
		if r.Metrics[i].Name == name {
			return &r.Metrics[i]
		}
	}
	return nil
}
