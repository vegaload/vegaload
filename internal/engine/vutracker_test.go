package engine

import (
	"context"
	"sync"
	"testing"
	"time"
)

// vuRec is a Recorder that is also a VUTracker: it counts the VUs running
// now and the most that ran at once.
type vuRec struct {
	mu      sync.Mutex
	active  int
	peak    int
	starts  int
	stopped int
	bad     bool
}

func (r *vuRec) Record(IterationResult) {}
func (r *vuRec) VUStarted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts++
	r.active++
	r.peak = max(r.peak, r.active)
}
func (r *vuRec) VUStopped() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped++
	r.active--
	if r.active < 0 {
		r.bad = true
	}
}

func TestExecutorsTellAVUTrackerEveryVU(t *testing.T) {
	stages := []Stage{{Target: 3, Duration: 150 * time.Millisecond}}
	cases := map[string]struct {
		ex         Executor
		peakAtMost int
		exactPeak  bool
	}{
		"fixed-vus":             {FixedVUs{VUs: 3, Dur: 150 * time.Millisecond}, 3, true},
		"ramp":                  {Ramp{Stages: stages}, 3, false},
		"step":                  {Step{Stages: stages}, 3, true},
		"constant-arrival-rate": {ConstantArrivalRate{Rate: 200, Dur: 150 * time.Millisecond, MaxVUs: 2}, 2, false},
		"ramping-arrival-rate":  {RampingArrivalRate{StartRate: 200, Stages: []Stage{{Target: 200, Duration: 150 * time.Millisecond}}, MaxVUs: 2}, 2, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := &vuRec{}
			if err := tc.ex.Run(context.Background(), slowIter, rec); err != nil {
				t.Fatal(err)
			}
			if rec.bad || rec.active != 0 || rec.starts != rec.stopped {
				t.Fatalf("active=%d starts=%d stops=%d bad=%v", rec.active, rec.starts, rec.stopped, rec.bad)
			}
			if rec.peak == 0 || rec.peak > tc.peakAtMost {
				t.Fatalf("peak=%d, want 1..%d", rec.peak, tc.peakAtMost)
			}
			if tc.exactPeak && rec.peak != tc.peakAtMost {
				t.Fatalf("peak=%d, want %d", rec.peak, tc.peakAtMost)
			}
		})
	}
}

// A Recorder without VUTracker still runs every executor.
func TestVUTrackingIsOptional(t *testing.T) {
	s := NewSummary()
	if err := (FixedVUs{VUs: 2, Dur: 40 * time.Millisecond}).Run(context.Background(), slowIter, s); err != nil {
		t.Fatal(err)
	}
	if s.Total() == 0 {
		t.Fatal("no iteration recorded")
	}
}
