package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// trackRec is a Recorder that is also an IterationTracker. It checks the
// rules the doc comment of IterationTracker sets: Begin and End come in pairs
// with the same start time, and End comes after the iteration's Record (when
// it is recorded at all).
type trackRec struct {
	mu       sync.Mutex
	begun    map[time.Time]int
	recorded map[time.Time]bool
	begins   int
	ends     int
	records  int
	problems []string
}

func newTrackRec() *trackRec {
	return &trackRec{begun: map[time.Time]int{}, recorded: map[time.Time]bool{}}
}

func (r *trackRec) Record(res IterationResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records++
	if res.VUID >= 0 { // not a dropped arrival
		r.recorded[res.Start] = true
	}
}
func (r *trackRec) Begin(start time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.begins++
	r.begun[start]++
}
func (r *trackRec) End(start time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends++
	if r.begun[start] == 0 {
		r.problems = append(r.problems, "End without Begin")
	}
	r.begun[start]--
}

func slowIter(ctx context.Context) error {
	select {
	case <-time.After(15 * time.Millisecond):
		return nil
	case <-ctx.Done():
		return errors.New("cut off")
	}
}

func TestExecutorsTellATrackingRecorderAboutEveryIteration(t *testing.T) {
	cases := map[string]Executor{
		"fixed-vus":             FixedVUs{VUs: 3, Dur: 150 * time.Millisecond},
		"constant-arrival-rate": ConstantArrivalRate{Rate: 100, Dur: 150 * time.Millisecond, MaxVUs: 10},
		"ramp":                  Ramp{Stages: []Stage{{Target: 3, Duration: 150 * time.Millisecond}}},
		"step":                  Step{Stages: []Stage{{Target: 3, Duration: 150 * time.Millisecond}}},
	}
	for name, ex := range cases {
		t.Run(name, func(t *testing.T) {
			rec := newTrackRec()
			if err := ex.Run(context.Background(), slowIter, rec); err != nil {
				t.Fatal(err)
			}
			if rec.begins == 0 || rec.begins != rec.ends {
				t.Fatalf("begins=%d ends=%d", rec.begins, rec.ends)
			}
			for start, n := range rec.begun {
				if n != 0 {
					t.Fatalf("start %v left %d running", start, n)
				}
			}
			if len(rec.problems) != 0 {
				t.Fatal(rec.problems)
			}
		})
	}
}

// End is after Record: at the moment End runs, the iteration is already counted.
func TestEndComesAfterRecord(t *testing.T) {
	rec := &orderRec{}
	finishes := func(context.Context) error { time.Sleep(15 * time.Millisecond); return nil }
	if err := (FixedVUs{VUs: 2, Dur: 100 * time.Millisecond}).Run(context.Background(), finishes, rec); err != nil {
		t.Fatal(err)
	}
	if rec.bad != "" {
		t.Fatal(rec.bad)
	}
	if rec.ends == 0 {
		t.Fatal("no iteration ended")
	}
}

type orderRec struct {
	mu       sync.Mutex
	recorded map[time.Time]bool
	ends     int
	bad      string
}

func (r *orderRec) Record(res IterationResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recorded == nil {
		r.recorded = map[time.Time]bool{}
	}
	r.recorded[res.Start] = true
}
func (r *orderRec) Begin(time.Time) {}
func (r *orderRec) End(start time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends++
	if !r.recorded[start] {
		r.bad = "End ran before the iteration was recorded"
	}
}

// An iteration cut off by the end of the run is not recorded, and is still ended.
func TestACutOffIterationIsStillEnded(t *testing.T) {
	rec := newTrackRec()
	hang := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	if err := (FixedVUs{VUs: 2, Dur: 60 * time.Millisecond}).Run(context.Background(), hang, rec); err != nil {
		t.Fatal(err)
	}
	if rec.records != 0 || rec.begins != 2 || rec.ends != 2 {
		t.Fatalf("records=%d begins=%d ends=%d", rec.records, rec.begins, rec.ends)
	}
}
