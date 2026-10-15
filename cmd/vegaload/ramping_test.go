package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/engine"
	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/safety"
)

func rampingStages() []engine.Stage {
	return []engine.Stage{
		{Target: 40, Duration: 3 * time.Second},
		{Target: 500, Duration: 3 * time.Second},
		{Target: 40, Duration: 4 * time.Second},
	}
}

func TestBuildExecutor_RampingArrivalRate(t *testing.T) {
	if _, err := buildExecutor(&runConfig{Executor: "ramping-arrival-rate"}); err == nil || !strings.Contains(err.Error(), "-stages") {
		t.Errorf("without stages: err = %v, want one that asks for -stages", err)
	}
	if _, err := buildExecutor(&runConfig{Executor: "ramping-arrival-rate", Stages: rampingStages(), Rate: -1}); err == nil {
		t.Error("a negative starting rate should be refused")
	}
	ex, err := buildExecutor(&runConfig{Executor: "ramping-arrival-rate", Stages: rampingStages(), Rate: 10, MaxVUs: 50})
	if err != nil {
		t.Fatalf("buildExecutor: %v", err)
	}
	r, ok := ex.(engine.RampingArrivalRate)
	if !ok {
		t.Fatalf("executor is %T, want engine.RampingArrivalRate", ex)
	}
	if r.StartRate != 10 || r.MaxVUs != 50 || len(r.Stages) != 3 {
		t.Errorf("executor = %+v, want start rate 10, max VUs 50 and the 3 stages", r)
	}
	if got, want := ex.Duration(), 10*time.Second; got != want {
		t.Errorf("Duration() = %v, want %v (the stages' total, not -duration)", got, want)
	}
}

func TestConcurrencyHint_RampingArrivalRateUsesMaxVUs(t *testing.T) {
	if got := concurrencyHint(&runConfig{Executor: "ramping-arrival-rate", MaxVUs: 200, Stages: rampingStages()}); got != 200 {
		t.Errorf("concurrencyHint = %d, want 200 (-max-vus, not the stage targets, which are rates)", got)
	}
	if got := concurrencyHint(&runConfig{Executor: "ramping-arrival-rate", Stages: rampingStages()}); got != 1 {
		t.Errorf("concurrencyHint without -max-vus = %d, want 1", got)
	}
}

func TestEnforceSafety_RampingArrivalRateChecksThePeakRateAndTheStagesLength(t *testing.T) {
	base := func() *runConfig {
		return &runConfig{
			Executor: "ramping-arrival-rate", MaxVUs: 10, VUs: 1, Duration: time.Second,
			Stages: rampingStages(), Limits: safety.DefaultLimits,
			Target: protocol.Target{URL: "http://localhost:8080"},
		}
	}
	if err := enforceSafety(base(), strings.NewReader(""), &bytes.Buffer{}, noConfirm(t)); err != nil {
		t.Errorf("a modest ramp was refused: %v", err)
	}

	cfg := base()
	cfg.Limits.MaxRate = 100 // the stages reach 500 per second
	if err := enforceSafety(cfg, strings.NewReader(""), &bytes.Buffer{}, noConfirm(t)); err == nil || !strings.Contains(err.Error(), "rate") {
		t.Errorf("a peak above -cap-rate: err = %v, want a rate error", err)
	}

	cfg = base()
	cfg.Limits.MaxDuration = 5 * time.Second // the stages last 10 s, -duration says 1 s
	if err := enforceSafety(cfg, strings.NewReader(""), &bytes.Buffer{}, noConfirm(t)); err == nil || !strings.Contains(err.Error(), "duration") {
		t.Errorf("stages longer than -cap-duration: err = %v, want a duration error", err)
	}
}

func TestBuildRunStarted_RampingArrivalRateCarriesTheShape(t *testing.T) {
	cfg := &runConfig{Executor: "ramping-arrival-rate", MaxVUs: 200, ScenarioPath: "loadtests/x.vl.js", Stages: rampingStages()}
	rs := buildRunStarted(cfg, "ramping-arrival-rate", 10*time.Second)
	if rs.Executor != "ramping-arrival-rate" || rs.VUs != 200 || rs.Rate != 500 {
		t.Errorf("run.started = executor %q vus %d rate %v, want ramping-arrival-rate, 200 and the peak rate 500", rs.Executor, rs.VUs, rs.Rate)
	}
	if len(rs.Stages) != 3 || rs.Stages[1].Target != 500 || rs.Stages[1].Duration != "3s" {
		t.Errorf("stages = %+v, want the three configured stages", rs.Stages)
	}
	if rs.Duration != "10s" {
		t.Errorf("duration = %q, want 10s", rs.Duration)
	}
}
