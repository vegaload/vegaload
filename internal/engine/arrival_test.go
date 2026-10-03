package engine

import (
	"context"
	"testing"
	"time"
)

func TestConstantArrivalRate_ApproximatesRate(t *testing.T) {
	rec := NewSummary()
	iter := func(ctx context.Context) error {
		time.Sleep(time.Millisecond)
		return nil
	}

	c := ConstantArrivalRate{Rate: 50, Dur: 200 * time.Millisecond, MaxVUs: 20}
	start := time.Now()
	if err := c.Run(context.Background(), iter, rec); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < c.Dur {
		t.Errorf("Run returned before its duration elapsed: %v < %v", elapsed, c.Dur)
	}

	// At 50/sec for ~200ms we expect roughly 10 arrivals. Allow a wide
	// margin since timers aren't exact, but it should be in the
	// right ballpark, not off by an order of magnitude.
	total := rec.Total()
	if total < 4 || total > 20 {
		t.Errorf("Total() = %d, want roughly 10 (4-20 allowed)", total)
	}
}

func TestConstantArrivalRate_DropsWhenVUsSaturated(t *testing.T) {
	rec := NewSummary()
	block := make(chan struct{})
	iter := func(ctx context.Context) error {
		<-block // stays busy until released below, forcing saturation
		return nil
	}

	c := ConstantArrivalRate{Rate: 100, Dur: 100 * time.Millisecond, MaxVUs: 1}

	// Run blocks until its context times out AND every in-flight
	// iteration has returned (it calls wg.Wait() before returning), so
	// the single running iteration must be released concurrently with
	// Run, not after it — otherwise Run's internal wg.Wait() never
	// unblocks and the test hangs forever.
	released := make(chan struct{})
	go func() {
		time.Sleep(150 * time.Millisecond) // after c.Dur elapses
		close(block)
		close(released)
	}()

	if err := c.Run(context.Background(), iter, rec); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	<-released

	// With MaxVUs: 1 and iterations that stay busy throughout the run,
	// only the first arrival should ever start; every later tick
	// should be dropped and recorded as a failed iteration with VUID -1.
	if rec.Total() < 2 {
		t.Fatalf("Total() = %d, want at least 2 (the running one plus at least one drop)", rec.Total())
	}
	if rec.Failed() == 0 {
		t.Error("expected dropped iterations to be recorded as failures")
	}
}

func TestConstantArrivalRate_RejectsNonPositiveRate(t *testing.T) {
	rec := NewSummary()
	c := ConstantArrivalRate{Rate: 0, Dur: 10 * time.Millisecond}
	if err := c.Run(context.Background(), func(ctx context.Context) error { return nil }, rec); err == nil {
		t.Error("expected an error for Rate <= 0, got nil")
	}
}

func TestConstantArrivalRate_DoesNotRecordRunEndCancellation(t *testing.T) {
	rec := NewSummary()
	iter := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	c := ConstantArrivalRate{Rate: 50, Dur: 50 * time.Millisecond, MaxVUs: 10}
	if err := c.Run(context.Background(), iter, rec); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if rec.Failed() != 0 {
		t.Fatalf("Failed() = %d, want 0 (in-flight arrivals cut off at run end)", rec.Failed())
	}
	if rec.Total() != 0 {
		t.Fatalf("Total() = %d, want 0", rec.Total())
	}
}
