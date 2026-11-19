package report

import "time"

// BucketStat is what one one-second bucket of a run holds: the calls that
// started in that second. Requests, Failed, RPS and ErrorRate are the same
// numbers as the matching Point of Result.TimeSeries, worked out the same way.
// The latency percentiles are estimated from a histogram of the bucket's
// calls, by the same rule as Finish (see hist.Histogram.Percentile).
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

	// Min, Mean, Max and P90 are the bucket's other latency figures.
	Min, Mean, Max, P90 time.Duration

	// ActiveVUs is the most VUs that ran at once in the bucket's second.
	// MaxVUs is the planned maximum, or the most seen so far when higher.
	ActiveVUs, MaxVUs int64

	// The iterations that started in this second, those of them that
	// completed, and the arrivals due in it that were dropped. The *Total
	// fields add up this and every earlier bucket, as emitted.
	IterationsStarted, IterationsCompleted, IterationsDropped                int64
	IterationsStartedTotal, IterationsCompletedTotal, IterationsDroppedTotal int64

	// Bytes of the calls that started in this second, and the running totals.
	BytesSent, BytesReceived           int64
	BytesSentTotal, BytesReceivedTotal int64

	// TTFBCount is how many calls measured a time to first byte; TTFBP50 and
	// TTFBP95 are zero when none did.
	TTFBCount        int64
	TTFBP50, TTFBP95 time.Duration

	// Checks made in this second, and the running totals.
	ChecksPassed, ChecksFailed           int64
	ChecksPassedTotal, ChecksFailedTotal int64

	// Steps is each step that ran in this second, by name. Rows is the
	// MaxPointRows protocol rows with the most calls in it, most first.
	// Errors is the failed requests of this second by kind, and the failed
	// checks under "check failed".
	Steps  []BucketStep
	Rows   []BucketRow
	Errors map[string]int64

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

// LiveBuckets hands out the one-second buckets of a run that is still going,
// in order, each exactly once. A bucket's counts are the final time series'
// counters; its percentiles come from a histogram the collector keeps for
// each second the cursor has not handed out yet, and drops as it does, so a
// long run's memory stays flat.
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

	// The running totals of the extra fields, and the VUs running at the end
	// of the last bucket handed out.
	started, completed, dropped int64
	sent, received              int64
	passed, checkFails          int64
	vus                         int64
}

// LiveBuckets returns a cursor over the collector's buckets, from the first.
// Use one cursor per run, from one goroutine. Creating it makes the collector
// keep the start of every running iteration, and the latencies of each second,
// from then on, so create it before the executor starts.
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
	last := int64(len(l.c.secs)) - 1
	l.c.mu.Unlock()
	if last < l.next {
		return nil
	}
	return l.take(l.next, last)
}

// take computes buckets from..to (both included), drops their live state, and
// advances the cursor past them. A call recorded later for one of these
// seconds still counts in the final series, but no live state is kept for it.
func (l *LiveBuckets) take(from, to int64) []BucketStat {
	n := int(to - from + 1)
	counts := make([]bucketAccum, n)
	secs := make([]*liveSec, n)
	c := l.c
	c.mu.Lock()
	for k := 0; k < n; k++ {
		idx := from + int64(k)
		if idx < int64(len(c.secs)) {
			counts[k] = c.secs[idx]
		}
		if s, ok := c.live[idx]; ok {
			secs[k] = s
			delete(c.live, idx)
		}
	}
	if to+1 > c.liveNext {
		c.liveNext = to + 1
	}
	c.mu.Unlock()
	maxVUs := c.MaxVUs()

	out := make([]BucketStat, n)
	for k := 0; k < n; k++ {
		a := counts[k]
		b := BucketStat{
			Offset:    time.Duration(from+int64(k)) * bucketWidth,
			Requests:  a.count,
			Failed:    a.failed,
			RPS:       float64(a.count) / bucketWidth.Seconds(),
			ErrorRate: rateOf(a.failed, a.count),
			MaxVUs:    maxVUs,
			ActiveVUs: l.vus,
		}
		if s := secs[k]; s != nil {
			l.fill(&b, s)
		}
		l.total += b.Requests
		l.failed += b.Failed
		b.Total, b.TotalFailed = l.total, l.failed
		l.started += b.IterationsStarted
		l.completed += b.IterationsCompleted
		l.dropped += b.IterationsDropped
		b.IterationsStartedTotal, b.IterationsCompletedTotal, b.IterationsDroppedTotal = l.started, l.completed, l.dropped
		l.sent += b.BytesSent
		l.received += b.BytesReceived
		b.BytesSentTotal, b.BytesReceivedTotal = l.sent, l.received
		l.passed += b.ChecksPassed
		l.checkFails += b.ChecksFailed
		b.ChecksPassedTotal, b.ChecksFailedTotal = l.passed, l.checkFails
		out[k] = b
	}
	l.next = to + 1
	return out
}

// fill sets the fields of b that come from the second's live state.
func (l *LiveBuckets) fill(b *BucketStat, s *liveSec) {
	if h := &s.lat; h.Count() > 0 {
		b.P50, b.P95, b.P99 = h.Percentile(50), h.Percentile(95), h.Percentile(99)
		b.Min, b.Mean, b.Max, b.P90 = h.Min(), h.Mean(), h.Max(), h.Percentile(90)
	}
	if s.vuSet {
		// The VUs running as the second began ran in it too.
		b.ActiveVUs = max(l.vus, s.vuPeak)
		l.vus = s.vuEnd
	}
	b.IterationsStarted, b.IterationsCompleted, b.IterationsDropped = s.started, s.completed, s.dropped
	b.BytesSent, b.BytesReceived = s.sent, s.received
	if n := s.ttfb.Count(); n > 0 {
		b.TTFBCount = n
		b.TTFBP50, b.TTFBP95 = s.ttfb.Percentile(50), s.ttfb.Percentile(95)
	}
	b.ChecksPassed, b.ChecksFailed = s.checksPassed, s.checksFailed
	b.Steps = s.bucketSteps()
	b.Rows = s.bucketRows()
	if len(s.errors) > 0 {
		b.Errors = s.errors
	}
}
