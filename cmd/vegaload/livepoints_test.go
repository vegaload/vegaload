package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/engine"
	"github.com/vegaload/vegaload/internal/report"
	"github.com/vegaload/vegaload/internal/stream"
)

// fakeEmitter keeps what the point feed hands to the stream, with the time each
// thing arrived (since the run's clock started).
type fakeEmitter struct {
	mu       sync.Mutex
	start    time.Time
	points   []stream.Point
	at       []time.Duration
	warnings []string
}

func (f *fakeEmitter) Started(time.Time, stream.RunStarted) {}
func (f *fakeEmitter) Point(p stream.Point) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.points = append(f.points, p)
	f.at = append(f.at, time.Since(f.start))
}
func (f *fakeEmitter) Warning(m string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.warnings = append(f.warnings, m)
}
func (f *fakeEmitter) Finished(any, bool, int) {}
func (f *fakeEmitter) Failed(string, any)      {}

// shortTimes makes the grace and the hold short for a test, and puts them back.
func shortTimes(t *testing.T, grace, hold time.Duration) {
	t.Helper()
	g, h := pointGrace, liveMaxHold
	pointGrace, liveMaxHold = grace, hold
	t.Cleanup(func() { pointGrace, liveMaxHold = g, h })
}

// Calls that take longer than the grace used to leave their second's point
// empty. Run through the real executor and the real collector, with an
// iteration that takes 300 ms (one call in each second straddles its end) against a 50 ms grace: every point is the
// matching bucket of the final series.
func TestLivePointsOfSlowCallsEqualTheFinalSeries(t *testing.T) {
	shortTimes(t, 50*time.Millisecond, report.LiveMaxHold)
	c := report.NewCollector()
	em := &fakeEmitter{start: c.Start()}
	feed := startPointFeed(em, c)

	iter := func(ctx context.Context) error {
		select {
		case <-time.After(300 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := (engine.FixedVUs{VUs: 4, Dur: 2300 * time.Millisecond}).Run(context.Background(), iter, c); err != nil {
		t.Fatal(err)
	}
	feed.finish()
	res := c.Finish("fixed-vus", 2300*time.Millisecond)

	if len(em.warnings) != 0 {
		t.Fatalf("warnings with nothing hung: %v", em.warnings)
	}
	if len(em.points) != len(res.TimeSeries) || len(em.points) < 2 {
		t.Fatalf("%d points for %d series buckets", len(em.points), len(res.TimeSeries))
	}
	var sum int64
	for i, p := range em.points {
		s := res.TimeSeries[i]
		if p.OffsetS != float64(i) {
			t.Fatalf("point %d is at offset %v", i, p.OffsetS)
		}
		if p.Requests == 0 || p.RPS == 0 {
			t.Errorf("point %d is empty: %+v", i, p)
		}
		if p.Requests != s.Requests || p.Failed != s.Failed || p.RPS != s.RPS || p.ErrorRate != s.ErrorRate {
			t.Errorf("point %d = %d/%d rps %v err %v, series = %d/%d rps %v err %v", i, p.Requests, p.Failed, p.RPS, p.ErrorRate, s.Requests, s.Failed, s.RPS, s.ErrorRate)
		}
		sum += p.Requests
		if p.Total != sum {
			t.Errorf("point %d total %d, want %d", i, p.Total, sum)
		}
	}
	if sum != res.Total {
		t.Errorf("points hold %d calls, the result %d", sum, res.Total)
	}
	// Point 0 waits for the call that started at the end of its second.
	if em.at[0] < time.Second {
		t.Errorf("first point arrived at %v, before its second closed", em.at[0])
	}
}

// A call that hangs holds the points only up to the cap. The point then goes
// out without it, one warning says so, and the final series has the call.
func TestAHungCallCapsTheLivePointsAndWarnsOnce(t *testing.T) {
	shortTimes(t, 50*time.Millisecond, 500*time.Millisecond)
	c := report.NewCollector()
	em := &fakeEmitter{start: c.Start()}
	feed := startPointFeed(em, c)

	var wg sync.WaitGroup
	call := func(d time.Duration) {
		start := time.Now()
		c.Begin(start)
		time.Sleep(d)
		c.Record(engine.IterationResult{Start: start, Duration: time.Since(start)})
		c.End(start)
	}
	wg.Add(2)
	go func() { // hangs from the start of second 0
		defer wg.Done()
		time.Sleep(100 * time.Millisecond)
		call(1900 * time.Millisecond)
	}()
	go func() { // quick calls all along
		defer wg.Done()
		for time.Since(c.Start()) < 2700*time.Millisecond {
			call(20 * time.Millisecond)
			time.Sleep(80 * time.Millisecond)
		}
	}()
	wg.Wait()
	feed.finish()
	res := c.Finish("fixed-vus", 2700*time.Millisecond)

	if len(em.warnings) != 1 || em.warnings[0] != slowCallsWarning {
		t.Fatalf("warnings = %q, want exactly one: %q", em.warnings, slowCallsWarning)
	}
	for _, bad := range []string{"http", "127.0.0.1", "://", "token", "secret"} {
		if strings.Contains(slowCallsWarning, bad) {
			t.Errorf("the warning names %q", bad)
		}
	}
	if len(em.points) != len(res.TimeSeries) {
		t.Fatalf("%d points for %d series buckets", len(em.points), len(res.TimeSeries))
	}
	for i, p := range em.points {
		if p.OffsetS != float64(i) {
			t.Fatalf("point %d is at offset %v", i, p.OffsetS)
		}
	}
	// Point 0 went out at about 1 s + the cap, while the call was still running.
	if em.at[0] < 1400*time.Millisecond || em.at[0] > 1900*time.Millisecond {
		t.Errorf("point 0 arrived at %v, want about 1.5 s (end of the second plus the cap)", em.at[0])
	}
	if em.points[0].Requests >= res.TimeSeries[0].Requests {
		t.Errorf("point 0 has %d calls and the series %d: the hung call should be only in the series", em.points[0].Requests, res.TimeSeries[0].Requests)
	}
}
