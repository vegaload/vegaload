package report

import (
	"sort"
	"time"

	"github.com/vegaload/vegaload/internal/hist"
	"github.com/vegaload/vegaload/internal/urlname"
)

// liveSec is what a live cursor needs of one second that it has not handed
// out yet, beyond the counts of the final series: histograms and the
// counters of the extra point fields. It is kept only while a cursor exists,
// and dropped when the cursor hands the second out, so a long run's memory
// stays flat. Collector.mu guards it.
type liveSec struct {
	lat  hist.Histogram // requests that started in this second
	ttfb hist.Histogram

	started, completed, dropped int64

	// vuPeak is the most VUs running at once in this second and vuEnd the
	// count after the last change in it. vuSet is false when no VU started
	// or stopped in it: the count then is the previous second's.
	vuPeak, vuEnd int64
	vuSet         bool

	sent, received int64

	checksPassed, checksFailed int64

	errors map[string]int64
	steps  map[string]*liveRuns
	rows   map[rowKey]*liveRuns
}

// liveRuns is one step's or one row's calls in one second.
type liveRuns struct {
	count, failed int64
	h             hist.Histogram
}

func (r *liveRuns) add(d time.Duration, failed bool) {
	r.count++
	if failed {
		r.failed++
	}
	r.h.Add(d)
}

type rowKey struct{ protocol, op string }

// liveSecLocked returns the live state of second idx, creating it, or nil
// when no cursor exists or the cursor has already handed idx out. The
// caller holds c.mu.
func (c *Collector) liveSecLocked(idx int64) *liveSec {
	if idx < c.liveNext || idx < 0 || !c.tracking.Load() {
		return nil
	}
	s := c.live[idx]
	if s == nil {
		if c.live == nil {
			c.live = map[int64]*liveSec{}
		}
		s = &liveSec{}
		c.live[idx] = s
	}
	return s
}

// secOf is the one-second bucket t falls in.
func (c *Collector) secOf(t time.Time) int64 {
	return int64(max(t.Sub(c.start), 0) / bucketWidth)
}

// withLive runs f on the live state of the second t falls in, when a cursor
// exists and has not handed that second out yet.
func (c *Collector) withLive(t time.Time, f func(*liveSec)) {
	if !c.tracking.Load() {
		return
	}
	c.mu.Lock()
	if s := c.liveSecLocked(c.secOf(t)); s != nil {
		f(s)
	}
	c.mu.Unlock()
}

// MaxPointRows is how many rows one live point carries: the ones with the
// most calls in that second.
const MaxPointRows = 50

// BucketStep is one step's runs in one second.
type BucketStep struct {
	Name   string
	Count  int64
	Failed int64
	P95    time.Duration
}

// BucketRow is one protocol row's calls in one second: an HTTP endpoint
// ("GET /users/{id}", with Method and Endpoint set) or another protocol's
// operation.
type BucketRow struct {
	Protocol string
	Name     string
	Method   string
	Endpoint string
	Count    int64
	Failed   int64
	P50      time.Duration
	P95      time.Duration
	P99      time.Duration
}

func (s *liveSec) bucketSteps() []BucketStep {
	if len(s.steps) == 0 {
		return nil
	}
	out := make([]BucketStep, 0, len(s.steps))
	for name, r := range s.steps {
		out = append(out, BucketStep{Name: name, Count: r.count, Failed: r.failed, P95: r.h.Percentile(95)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// bucketRows is the MaxPointRows rows with the most calls, most first.
func (s *liveSec) bucketRows() []BucketRow {
	if len(s.rows) == 0 {
		return nil
	}
	keys := make([]rowKey, 0, len(s.rows))
	for k := range s.rows {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := s.rows[keys[i]], s.rows[keys[j]]
		if a.count != b.count {
			return a.count > b.count
		}
		if keys[i].protocol != keys[j].protocol {
			return keys[i].protocol < keys[j].protocol
		}
		return keys[i].op < keys[j].op
	})
	if len(keys) > MaxPointRows {
		keys = keys[:MaxPointRows]
	}
	out := make([]BucketRow, 0, len(keys))
	for _, k := range keys {
		r := s.rows[k]
		row := BucketRow{
			Protocol: k.protocol, Name: k.op, Count: r.count, Failed: r.failed,
			P50: r.h.Percentile(50), P95: r.h.Percentile(95), P99: r.h.Percentile(99),
		}
		if IsHTTP(k.protocol) {
			if m, n, ok := urlname.Split(k.op); ok {
				row.Method, row.Endpoint = m, n
			}
		}
		out = append(out, row)
	}
	return out
}
