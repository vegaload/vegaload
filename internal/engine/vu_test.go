package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunVU_StopsOnContextCancel(t *testing.T) {
	rec := NewSummary()
	ctx, cancel := context.WithCancel(context.Background())
	iter := func(ctx context.Context) error { return nil }

	done := make(chan struct{})
	go func() {
		runVU(ctx, 0, iter, rec)
		close(done)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runVU did not stop after context cancellation")
	}

	if rec.Total() == 0 {
		t.Error("expected at least one iteration to have been recorded before cancellation")
	}
}

func TestRunVU_RecordsIterationErrors(t *testing.T) {
	rec := NewSummary()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	iter := func(ctx context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("boom")
		}
		cancel() // stop after the successful second iteration
		return nil
	}

	runVU(ctx, 0, iter, rec)

	if rec.Total() != 2 {
		t.Fatalf("Total() = %d, want 2", rec.Total())
	}
	if rec.Failed() != 1 {
		t.Fatalf("Failed() = %d, want 1 (only the boom iteration)", rec.Failed())
	}
}

func TestRunVU_DoesNotRecordRunEndCancellation(t *testing.T) {
	rec := NewSummary()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	iter := func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}

	done := make(chan struct{})
	go func() {
		runVU(ctx, 0, iter, rec)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("iteration did not start")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runVU did not stop after context cancellation")
	}

	if rec.Total() != 0 {
		t.Fatalf("Total() = %d, want 0 (run-end cancellation must not be recorded)", rec.Total())
	}
	if rec.Failed() != 0 {
		t.Fatalf("Failed() = %d, want 0", rec.Failed())
	}
}

func TestFixedVUs_DoesNotCountRunEndCancellationsAsFailures(t *testing.T) {
	rec := NewSummary()
	// Each VU blocks until the run deadline cancels ctx, then returns
	// that cancellation — the failure mode this bug used to inflate.
	iter := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	f := FixedVUs{VUs: 5, Dur: 50 * time.Millisecond}
	if err := f.Run(context.Background(), iter, rec); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if rec.Failed() != 0 {
		t.Fatalf("Failed() = %d, want 0 (in-flight VUs cut off at run end)", rec.Failed())
	}
	if rec.Total() != 0 {
		t.Fatalf("Total() = %d, want 0", rec.Total())
	}
}
