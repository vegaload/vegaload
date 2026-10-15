package report

import (
	"sort"
	"time"
)

// BucketStat is what one one-second bucket of a run holds: the calls that
// started in that second. Requests, Failed, RPS and ErrorRate are the same
// numbers as the matching Point of Result.TimeSeries, worked out the same way.
// The latency percentiles use the same nearest-rank rule as Finish.
type BucketStat struct {
	Offset      time.Duration // start of the bucket
	Requests    int64
	Failed      int64
	RPS         float64
	ErrorRate   float64
	P50         time.Duration
	P95         time.Duration
	P99         time.Duration
	Total       int64 // calls in this and every earlier bucket, as emitted
	TotalFailed int64
	// Capped is true when the bucket was handed out while a call that started
	// in it, or in an earlier second, was still running, because the hold hit
	// its limit (LiveMaxHold). The bucket then lacks that call; the final
	// Result.TimeSeries has it.
	Capped bool
}

// LiveMaxHold is the longest a bucket is held back for calls that started in
// it (or earlier) and have not ended: this long past the end of its second.
// After that it is handed out with what it has, so that one hung call cannot
// freeze a live view. The final Result.TimeSeries stays authoritative.
const LiveMaxHold = 10 * time.Second

// liveScanSlack is how far back in time the scan for a bucket's calls goes
// beyond the bucket's start. Samples are appended as the iterations end, in an
// order that is only roughly the order they ended in; a second is far more
// than that disorder.
const liveScanSlack = time.Second

// LiveBuckets hands out the one-second buckets of a run that is still going,
// in order, each exactly once. It reads the collector's samples without
// copying them all: a bucket's calls are found by scanning back from the end
// of the slice, which holds the newest.
//
// A call is recorded when it ends, but it belongs to the second it started in.
// A bucket is therefore handed out only when its second has closed, a grace
// period has passed (for a call that is just starting or just ending), and no
// call that started in that second or an earlier one is still running (the
// collector learns this from the engine through Begin and End). A call that
// runs longer than LiveMaxHold past the end of its second no longer holds the
// bucket: it is handed out without that call, marked Capped, and the final
// Result.TimeSeries, which has the call, is authoritative.
//
// The final series ends at the last second a recorded call started in. A
// bucket past that second is therefore not handed out while the run goes on
// (a quiet second is held until a later one has a call, or the run ends), so
// the points never cover a second the series does not. Rest hands out what is
// left. A Capped bucket is the one exception: it goes out as it is.
type LiveBuckets struct {
	c       *Collector
	next    int64
	total   int64
	failed  int64
	maxHold time.Duration
}

// LiveBuckets returns a cursor over the collector's buckets, from the first.
// Use one cursor per run, from one goroutine. Creating it makes the collector
// keep the start of every running iteration from then on, so create it before
// the executor starts.
func (c *Collector) LiveBuckets() *LiveBuckets {
	c.tracking.Store(true)
	return &LiveBuckets{c: c, maxHold: LiveMaxHold}
}

// SetMaxHold changes the hold limit (LiveMaxHold by default). It is for tests.
func (l *LiveBuckets) SetMaxHold(d time.Duration) { l.maxHold = d }

// Next returns the next bucket once it is due: now is the time since the
// collector started, and the bucket is due when now has passed the end of its
// second by grace and no call that started in it or earlier is running. A hold
// ends at the latest maxHold past the end of the second, and the bucket is then
// Capped. ok is false when it is not due yet.
func (l *LiveBuckets) Next(now, grace time.Duration) (BucketStat, bool) {
	end := time.Duration(l.next+1) * bucketWidth
	if now < end+grace {
		return BucketStat{}, false
	}
	capped := false
	running, last := l.c.liveState(l.next)
	switch {
	case running:
		if now < end+l.maxHold {
			return BucketStat{}, false
		}
		capped = true
	case l.next > last:
		return BucketStat{}, false
	}
	b := l.take(l.next, l.next)[0]
	b.Capped = capped
	return b, true
}

// Rest returns every bucket not handed out yet, up to the last second a call
// started in. Call it once the run has ended, before Finish's result is used,
// so that the points and the final time series cover the same seconds.
func (l *LiveBuckets) Rest() []BucketStat {
	l.c.mu.Lock()
	last := l.c.maxBucket
	l.c.mu.Unlock()
	if last < l.next {
		return nil
	}
	return l.take(l.next, last)
}

// take computes buckets from..to (both included) in one scan and advances the
// cursor past them.
func (l *LiveBuckets) take(from, to int64) []BucketStat {
	n := int(to - from + 1)
	durs := make([][]time.Duration, n)
	failed := make([]int64, n)

	lo := time.Duration(from) * bucketWidth
	hi := time.Duration(to+1) * bucketWidth
	c := l.c
	c.mu.Lock()
	for j := len(c.samples) - 1; j >= 0; j-- {
		s := c.samples[j]
		if s.offset+s.dur < lo-liveScanSlack {
			break
		}
		if s.offset < lo || s.offset >= hi {
			continue
		}
		k := int(int64(s.offset/bucketWidth) - from)
		durs[k] = append(durs[k], s.dur)
		if s.failed {
			failed[k]++
		}
	}
	c.mu.Unlock()

	out := make([]BucketStat, n)
	for k := 0; k < n; k++ {
		d := durs[k]
		sort.Slice(d, func(a, b int) bool { return d[a] < d[b] })
		b := BucketStat{
			Offset:   time.Duration(from+int64(k)) * bucketWidth,
			Requests: int64(len(d)),
			Failed:   failed[k],
			RPS:      float64(len(d)) / bucketWidth.Seconds(),
		}
		if len(d) > 0 {
			b.ErrorRate = float64(failed[k]) / float64(len(d))
			b.P50, b.P95, b.P99 = percentile(d, 50), percentile(d, 95), percentile(d, 99)
		}
		l.total += b.Requests
		l.failed += b.Failed
		b.Total, b.TotalFailed = l.total, l.failed
		out[k] = b
	}
	l.next = to + 1
	return out
}
