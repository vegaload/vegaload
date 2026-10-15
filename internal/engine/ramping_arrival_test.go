package engine

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"
)

func TestRampingArrivalRate_RateAtFollowsTheStages(t *testing.T) {
	r := RampingArrivalRate{
		StartRate: 10,
		Stages: []Stage{
			{Target: 110, Duration: 10 * time.Second}, // 10 -> 110
			{Target: 110, Duration: 5 * time.Second},  // hold
			{Target: 10, Duration: 10 * time.Second},  // 110 -> 10
		},
	}
	cases := []struct {
		at   time.Duration
		want float64
	}{
		{0, 10},
		{5 * time.Second, 60},
		{10 * time.Second, 110},
		{12 * time.Second, 110},
		{15 * time.Second, 110},
		{20 * time.Second, 60},
		{25 * time.Second, 10},
		{40 * time.Second, 10}, // past the end holds the last target
	}
	for _, c := range cases {
		if got := r.rateAt(c.at); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("rateAt(%v) = %v, want %v", c.at, got, c.want)
		}
	}
	if got, want := r.Duration(), 25*time.Second; got != want {
		t.Errorf("Duration() = %v, want %v", got, want)
	}
	if r.Name() != "ramping-arrival-rate" {
		t.Errorf("Name() = %q", r.Name())
	}
}

func TestRampingArrivalRate_StartsWhatTheShapeAsksFor(t *testing.T) {
	rec := NewSummary()
	var mu sync.Mutex
	var starts []time.Duration
	begin := time.Now()
	iter := func(ctx context.Context) error {
		mu.Lock()
		starts = append(starts, time.Since(begin))
		mu.Unlock()
		return nil
	}

	// 0 -> 400/s over 0.5 s, then 400/s for 0.5 s: about 100 + 200 = 300 arrivals.
	r := RampingArrivalRate{
		Stages: []Stage{{Target: 400, Duration: 500 * time.Millisecond}, {Target: 400, Duration: 500 * time.Millisecond}},
		MaxVUs: 50,
	}
	if err := r.Run(context.Background(), iter, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rec.Total(); got < 240 || got > 330 {
		t.Errorf("Total() = %d, want about 300 (240-330 allowed)", got)
	}
	if rec.Failed() != 0 {
		t.Errorf("Failed() = %d, want 0", rec.Failed())
	}

	// The climb is real: the first half-second must hold clearly fewer
	// arrivals than the second, which runs at the full rate.
	first, second := 0, 0
	for _, s := range starts {
		if s < 500*time.Millisecond {
			first++
		} else {
			second++
		}
	}
	if first >= second*3/4 {
		t.Errorf("arrivals in the ramp-up (%d) should be well below those at full rate (%d)", first, second)
	}
}

func TestRampingArrivalRate_DropsWhenVUsSaturated(t *testing.T) {
	rec := NewSummary()
	block := make(chan struct{})
	iter := func(ctx context.Context) error {
		<-block
		return nil
	}
	r := RampingArrivalRate{Stages: []Stage{{Target: 200, Duration: 100 * time.Millisecond}}, StartRate: 200, MaxVUs: 1}

	go func() {
		time.Sleep(150 * time.Millisecond)
		close(block)
	}()
	if err := r.Run(context.Background(), iter, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.Total() < 2 || rec.Failed() == 0 {
		t.Errorf("Total()=%d Failed()=%d, want dropped arrivals recorded as failures", rec.Total(), rec.Failed())
	}
}

func TestRampingArrivalRate_StopsWithItsContext(t *testing.T) {
	rec := NewSummary()
	ctx, cancel := context.WithCancel(context.Background())
	r := RampingArrivalRate{Stages: []Stage{{Target: 50, Duration: time.Minute}}, StartRate: 50, MaxVUs: 5}
	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(context.Context) error { return nil }, rec)
	}()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after its context was cancelled")
	}
}

func TestRampingArrivalRate_RejectsABadShape(t *testing.T) {
	iter := func(context.Context) error { return nil }
	for name, r := range map[string]RampingArrivalRate{
		"no stages":       {MaxVUs: 1},
		"negative start":  {StartRate: -1, Stages: []Stage{{Target: 1, Duration: time.Second}}},
		"negative target": {Stages: []Stage{{Target: -5, Duration: time.Second}}},
	} {
		if err := r.Run(context.Background(), iter, NewSummary()); err == nil {
			t.Errorf("%s: Run returned nil, want an error", name)
		}
	}
}
