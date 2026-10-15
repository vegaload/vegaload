package report

import (
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/engine"
)

var errLive = errors.New("failed")

// feed records a deterministic run of seconds one-second buckets. Second 3 has
// no calls at all; every fourth call fails.
func feedLive(c *Collector, seconds int) {
	for sec := 0; sec < seconds; sec++ {
		if sec == 3 {
			continue
		}
		n := 3 + sec*2
		for k := 0; k < n; k++ {
			var err error
			if k%4 == 3 {
				err = errLive
			}
			c.Record(engine.IterationResult{
				VUID:     k,
				Start:    c.start.Add(time.Duration(sec)*time.Second + time.Duration(k)*7*time.Millisecond),
				Duration: time.Duration((k*13+sec*5)%90+10) * time.Millisecond,
				Err:      err,
			})
		}
	}
}

func TestLiveBucketsMatchTheFinalTimeSeries(t *testing.T) {
	c := NewCollector()
	feedLive(c, 7)

	lb := c.LiveBuckets()
	var live []BucketStat
	// Walk a clock past the end of the run: each bucket is due once, in order.
	for now := time.Duration(0); now < 10*time.Second; now += 100 * time.Millisecond {
		for {
			b, ok := lb.Next(now, 500*time.Millisecond)
			if !ok {
				break
			}
			// Not before its second has closed and the grace has passed.
			if now < b.Offset+bucketWidth+500*time.Millisecond {
				t.Fatalf("bucket %v handed out at %v", b.Offset, now)
			}
			live = append(live, b)
		}
	}
	live = append(live, lb.Rest()...)

	res := c.Finish("fixed-vus", 7*time.Second)
	// The walk went well past the last second a call started in. The points
	// still stop where the final series does.
	if len(live) != len(res.TimeSeries) {
		t.Fatalf("%d live points for %d series points", len(live), len(res.TimeSeries))
	}
	var total, failed int64
	for i, b := range live {
		if b.Offset != time.Duration(i)*time.Second {
			t.Fatalf("point %d is at %v: a bucket was missed or repeated", i, b.Offset)
		}
		total += b.Requests
		failed += b.Failed
		if b.Total != total || b.TotalFailed != failed {
			t.Fatalf("point %d totals %d/%d, want %d/%d", i, b.Total, b.TotalFailed, total, failed)
		}
		s := res.TimeSeries[i]
		if b.Requests != s.Requests || b.Failed != s.Failed || b.RPS != s.RPS || b.ErrorRate != s.ErrorRate || b.Offset != s.Offset {
			t.Fatalf("point %d: live %+v, series %+v", i, b, s)
		}
	}
	if total != res.Total || failed != res.Failed {
		t.Fatalf("live totals %d/%d, result %d/%d", total, failed, res.Total, res.Failed)
	}
	if live[3].Requests != 0 || live[3].P95 != 0 || live[3].ErrorRate != 0 {
		t.Fatalf("the empty second: %+v", live[3])
	}
}

func TestLiveBucketPercentilesUseTheSameRuleAsFinish(t *testing.T) {
	c := NewCollector()
	feedLive(c, 5)
	lb := c.LiveBuckets()
	got := lb.take(0, 4)

	for sec := 0; sec < 5; sec++ {
		var d []time.Duration
		for _, s := range c.samples {
			if int(s.offset/time.Second) == sec {
				d = append(d, s.dur)
			}
		}
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		if len(d) == 0 {
			continue
		}
		if got[sec].P50 != percentile(d, 50) || got[sec].P95 != percentile(d, 95) || got[sec].P99 != percentile(d, 99) {
			t.Fatalf("second %d percentiles %v %v %v, want %v %v %v", sec,
				got[sec].P50, got[sec].P95, got[sec].P99, percentile(d, 50), percentile(d, 95), percentile(d, 99))
		}
	}
}

// A call that is still running when its bucket is handed out is missing from
// that bucket and from Total, and is in the final time series. This is the
// late-call difference docs/streaming.md describes.
func TestLiveBucketLateCallIsInTheFinalSeriesOnly(t *testing.T) {
	c := NewCollector()
	c.Record(engine.IterationResult{Start: c.start.Add(100 * time.Millisecond), Duration: 10 * time.Millisecond})
	lb := c.LiveBuckets()
	b, ok := lb.Next(2*time.Second, 500*time.Millisecond)
	if !ok || b.Requests != 1 {
		t.Fatalf("%+v %v", b, ok)
	}
	// A call that started in second 0 ends after the point went out. Nothing
	// told the collector it was running (no Begin), so nothing held the point.
	c.Record(engine.IterationResult{Start: c.start.Add(900 * time.Millisecond), Duration: 1800 * time.Millisecond})
	res := c.Finish("fixed-vus", 3*time.Second)
	if res.TimeSeries[0].Requests != 2 {
		t.Fatalf("series %+v", res.TimeSeries[0])
	}
	if rest := lb.Rest(); len(rest) != 0 && rest[0].Offset == 0 {
		t.Fatalf("bucket 0 was handed out twice: %+v", rest)
	}
}

func TestLiveBucketsOfAnEmptyRun(t *testing.T) {
	c := NewCollector()
	lb := c.LiveBuckets()
	if rest := lb.Rest(); len(rest) != 0 {
		t.Fatalf("%+v", rest)
	}
	if b, ok := lb.Next(500*time.Millisecond, 500*time.Millisecond); ok {
		t.Fatalf("a bucket before its second closed: %+v", b)
	}
}

// The scan stays near the end of the samples: a long run does not make each
// tick slower. This is a guard against copying or walking every sample.
func TestLiveBucketScanStopsEarly(t *testing.T) {
	c := NewCollector()
	for sec := 0; sec < 600; sec++ {
		for k := 0; k < 200; k++ {
			c.Record(engine.IterationResult{Start: c.start.Add(time.Duration(sec)*time.Second + time.Duration(k)*time.Millisecond), Duration: time.Millisecond})
		}
	}
	lb := c.LiveBuckets()
	lb.next = 599
	start := time.Now()
	for i := 0; i < 1000; i++ {
		lb.next = 599
		lb.total, lb.failed = 0, 0
		if got := lb.take(599, 599); got[0].Requests != 200 {
			t.Fatalf("%+v", got[0])
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("1000 scans of the last bucket of 120000 samples took %v", d)
	}
}

// vcall is one call in a simulated run, in time since the run started.
type vcall struct {
	start, dur time.Duration
	failed     bool
}

type vpoint struct {
	at time.Duration // the simulated time it was handed out
	b  BucketStat
}

// simulate plays calls against a collector on a clock that moves 100 ms at a
// time, as the live feed's ticker does: a call is begun at its start and is
// recorded and ended at its end (both when the clock reaches them), and after
// each step every bucket that is due is taken. It returns what the cursor
// handed out while the run went on, and then what Rest handed out at the end.
// A call that would end after until is cut off, as the engine does when the run
// ends: it is ended but not recorded. No real time passes.
func simulate(c *Collector, lb *LiveBuckets, calls []vcall, until time.Duration) (live, rest []vpoint) {
	type ev struct {
		at   time.Duration
		end  bool
		cut  bool
		call vcall
	}
	var evs []ev
	for _, k := range calls {
		if k.start+k.dur > until {
			evs = append(evs, ev{k.start, false, false, k}, ev{until, true, true, k})
			continue
		}
		evs = append(evs, ev{k.start, false, false, k}, ev{k.start + k.dur, true, false, k})
	}
	sort.SliceStable(evs, func(i, j int) bool {
		if evs[i].at != evs[j].at {
			return evs[i].at < evs[j].at
		}
		return evs[i].end && !evs[j].end // an end before a start at the same instant
	})
	next := 0
	for now := time.Duration(0); now <= until; now += 100 * time.Millisecond {
		for next < len(evs) && evs[next].at <= now {
			e := evs[next]
			next++
			start := c.start.Add(e.call.start)
			if !e.end {
				c.Begin(start)
				continue
			}
			if !e.cut {
				var err error
				if e.call.failed {
					err = errLive
				}
				c.Record(engine.IterationResult{Start: start, Duration: e.call.dur, Err: err})
			}
			c.End(start)
		}
		for {
			b, ok := lb.Next(now, 500*time.Millisecond)
			if !ok {
				break
			}
			live = append(live, vpoint{now, b})
		}
	}
	for _, b := range lb.Rest() {
		rest = append(rest, vpoint{until, b})
	}
	return live, rest
}

// slowCalls is what a 5-user run against a target that adds 800 ms to each of
// two calls looks like: an iteration of 1.6 s, back to back, for 12 s. The
// users start 300 ms apart, so the calls of one second do not all end together.
func slowCalls() []vcall {
	var calls []vcall
	n := 0
	for vu := 0; vu < 5; vu++ {
		for at := time.Duration(vu) * 300 * time.Millisecond; at < 12*time.Second; at += 1600 * time.Millisecond {
			n++
			calls = append(calls, vcall{start: at, dur: 1600 * time.Millisecond, failed: n%7 == 0})
		}
	}
	return calls
}

// The defect: calls longer than the grace made every live point empty. A
// bucket is now held until the calls that started in it have ended, so every
// point equals the final series.
func TestLivePointsAreRightWhenCallsAreSlowerThanTheGrace(t *testing.T) {
	c := NewCollector()
	lb := c.LiveBuckets()
	live, rest := simulate(c, lb, slowCalls(), 12*time.Second)
	all := append(append([]vpoint{}, live...), rest...)

	res := c.Finish("fixed-vus", 12*time.Second)
	if len(all) != len(res.TimeSeries) {
		t.Fatalf("%d points for %d series buckets: Rest must make them cover the same seconds", len(all), len(res.TimeSeries))
	}
	var sum int64
	for i, p := range all {
		b, s := p.b, res.TimeSeries[i]
		if b.Offset != time.Duration(i)*time.Second {
			t.Fatalf("point %d is at %v: out of order, missed or repeated", i, b.Offset)
		}
		if b.Requests != s.Requests || b.Failed != s.Failed || b.RPS != s.RPS || b.ErrorRate != s.ErrorRate {
			t.Fatalf("point %d = %d/%d rps %v err %v, series = %d/%d rps %v err %v",
				i, b.Requests, b.Failed, b.RPS, b.ErrorRate, s.Requests, s.Failed, s.RPS, s.ErrorRate)
		}
		if b.Capped {
			t.Errorf("point %d was capped with nothing hung", i)
		}
		sum += b.Requests
		if b.Total != sum {
			t.Fatalf("point %d total %d, want %d", i, b.Total, sum)
		}
	}
	if sum != res.Total || sum == 0 {
		t.Fatalf("points hold %d calls, the result %d", sum, res.Total)
	}
	// The first second holds the calls of users 0 to 3 (started at 0, 0.3, 0.6
	// and 0.9 s); the last of them ends at 2.5 s. The point is due then, not at
	// the 1.5 s the grace alone would give, and it has all four.
	if first := live[0]; first.b.Offset != 0 || first.b.Requests != 4 || first.at < 2500*time.Millisecond {
		t.Fatalf("first point %+v handed out at %v", first.b, first.at)
	}
	// A point is never handed out while a call that started in it is running.
	for _, p := range live {
		for _, k := range slowCalls() {
			end := k.start + k.dur
			if end > 12*time.Second {
				end = 12 * time.Second // cut off when the run ended
			}
			if k.start/bucketWidth <= p.b.Offset/bucketWidth && end > p.at {
				t.Fatalf("point at %v handed out at %v while a call from %v to %v ran", p.b.Offset, p.at, k.start, end)
			}
		}
	}
}

// With fast calls the timing is what it was: a point is due 500 ms after its
// second closed, and not a tick later.
func TestLivePointsOfFastCallsKeepTheGraceTiming(t *testing.T) {
	c := NewCollector()
	lb := c.LiveBuckets()
	var calls []vcall
	for sec := 0; sec < 5; sec++ {
		for k := 0; k < 20; k++ {
			calls = append(calls, vcall{start: time.Duration(sec)*time.Second + time.Duration(k)*45*time.Millisecond, dur: 20 * time.Millisecond})
		}
	}
	live, _ := simulate(c, lb, calls, 8*time.Second)
	for i := 0; i < 4; i++ {
		if want := time.Duration(i+1)*time.Second + 500*time.Millisecond; live[i].at != want {
			t.Errorf("point %d handed out at %v, want %v", i, live[i].at, want)
		}
		if live[i].b.Requests != 20 {
			t.Errorf("point %d has %d calls", i, live[i].b.Requests)
		}
	}
}

// A call that hangs holds the live view for at most LiveMaxHold past the end of
// each bucket. The bucket then goes out with what it has, marked Capped, and
// the final series still has the hung call.
func TestAHungCallCapsTheHold(t *testing.T) {
	c := NewCollector()
	lb := c.LiveBuckets()
	hang := vcall{start: 200 * time.Millisecond, dur: 25 * time.Second}
	calls := []vcall{hang}
	for sec := 0; sec < 29; sec++ {
		calls = append(calls, vcall{start: time.Duration(sec)*time.Second + 300*time.Millisecond, dur: 50 * time.Millisecond})
	}
	live, rest := simulate(c, lb, calls, 29600*time.Millisecond)
	all := append(append([]vpoint{}, live...), rest...)

	// Bucket 0 is held until LiveMaxHold past its end, and not longer.
	first := live[0]
	if want := bucketWidth + LiveMaxHold; first.b.Offset != 0 || first.at != want || !first.b.Capped {
		t.Fatalf("first point: offset %v at %v capped=%v, want offset 0 at %v capped", first.b.Offset, first.at, first.b.Capped, want)
	}
	if first.b.Requests != 1 {
		t.Errorf("a capped point lacks the hung call: it has %d calls, want 1", first.b.Requests)
	}
	// Buckets behind it wait for their own cap while the call runs, one per
	// second, in order and once each.
	for i, p := range all {
		if p.b.Offset != time.Duration(i)*time.Second {
			t.Fatalf("point %d is at %v", i, p.b.Offset)
		}
	}
	for i := 0; i < 15; i++ {
		if want := time.Duration(i+1)*time.Second + LiveMaxHold; live[i].at != want || !live[i].b.Capped {
			t.Errorf("point %d handed out at %v capped=%v, want %v capped", i, live[i].at, live[i].b.Capped, want)
		}
	}
	// The call ends at 25.2 s. After that the rest are not held and not capped.
	for _, p := range all[15:] {
		if p.b.Capped {
			t.Errorf("point %v capped after the call ended", p.b.Offset)
		}
	}
	res := c.Finish("fixed-vus", 29600*time.Millisecond)
	if res.TimeSeries[0].Requests != 2 || all[0].b.Requests != 1 {
		t.Errorf("series bucket 0 has %d calls (want 2, the final series is authoritative); the point had %d", res.TimeSeries[0].Requests, all[0].b.Requests)
	}
	if len(all) != len(res.TimeSeries) {
		t.Errorf("%d points for %d series buckets", len(all), len(res.TimeSeries))
	}
}

func TestLiveMaxHoldIsTenSecondsAndCanBeChanged(t *testing.T) {
	if LiveMaxHold != 10*time.Second {
		t.Fatalf("LiveMaxHold = %v", LiveMaxHold)
	}
	c := NewCollector()
	lb := c.LiveBuckets()
	lb.SetMaxHold(2 * time.Second)
	c.Begin(c.start.Add(100 * time.Millisecond))
	if _, ok := lb.Next(2900*time.Millisecond, 500*time.Millisecond); ok {
		t.Fatal("handed out before the hold ended")
	}
	if b, ok := lb.Next(3*time.Second, 500*time.Millisecond); !ok || !b.Capped {
		t.Fatalf("%+v %v", b, ok)
	}
}

// Tracking costs nothing without a live cursor, and does not leak with one.
func TestTrackingIsOffWithoutACursorAndEmptiesWhenCallsEnd(t *testing.T) {
	c := NewCollector()
	start := c.start.Add(time.Second)
	c.Begin(start)
	c.End(start)
	if c.inflight != nil {
		t.Fatal("kept a start with no live cursor")
	}
	c.LiveBuckets()
	c.Begin(start)
	c.Begin(start)
	if !c.runningThrough(1) || c.runningThrough(0) {
		t.Fatal("a call that started in second 1 should hold second 1 and later, not second 0")
	}
	c.End(start)
	c.End(start)
	if c.runningThrough(5) || len(c.inflight) != 0 {
		t.Fatalf("left %v after every call ended", c.inflight)
	}
}

// The points never cover a second the final series does not: a quiet second
// at the end is not sent while the run goes on, and a quiet second in the
// middle is sent once a later second has a call.
func TestLivePointsStopWhereTheSeriesStops(t *testing.T) {
	c := NewCollector()
	lb := c.LiveBuckets()
	rec := func(sec int) {
		c.Record(engine.IterationResult{Start: c.start.Add(time.Duration(sec)*time.Second + 100*time.Millisecond), Duration: 10 * time.Millisecond})
	}
	rec(0)
	if b, ok := lb.Next(2*time.Second, 500*time.Millisecond); !ok || b.Offset != 0 {
		t.Fatalf("bucket 0: %+v %v", b, ok)
	}
	if b, ok := lb.Next(30*time.Second, 500*time.Millisecond); ok {
		t.Fatalf("an empty bucket past the last call was sent: %+v", b)
	}
	rec(3)
	var got []time.Duration
	for {
		b, ok := lb.Next(30*time.Second, 500*time.Millisecond)
		if !ok {
			break
		}
		got = append(got, b.Offset)
		if (b.Offset == 3*time.Second) != (b.Requests == 1) {
			t.Errorf("bucket %v has %d calls", b.Offset, b.Requests)
		}
	}
	if len(got) != 3 || got[0] != time.Second || got[2] != 3*time.Second {
		t.Fatalf("buckets after the gap: %v", got)
	}
	if rest := lb.Rest(); len(rest) != 0 {
		t.Fatalf("Rest: %+v", rest)
	}
}
