package report

import (
	"slices"
	"time"

	"github.com/vegaload/vegaload/internal/hist"
)

// dist is one latency distribution of a run: a histogram, and, only in
// exact mode, every value too. Its owner guards it with a lock.
type dist struct {
	h     hist.Histogram
	exact []time.Duration
	keep  bool // exact mode: keep every value
}

func newDist(exact bool) *dist { return &dist{keep: exact} }

func (d *dist) add(v time.Duration) {
	if v < 0 {
		v = 0
	}
	d.h.Add(v)
	if d.keep {
		d.exact = append(d.exact, v)
	}
}

// snapshot copies d, so its latency can be worked out after the lock is
// released.
func (d *dist) snapshot() *dist {
	s := &dist{h: *d.h.Clone(), keep: d.keep}
	if d.keep {
		s.exact = slices.Clone(d.exact)
	}
	return s
}

// merge adds o to a snapshot, as if every value of o had been added to it.
func (d *dist) merge(o *dist) {
	d.h.Merge(&o.h)
	if d.keep {
		d.exact = append(d.exact, o.exact...)
	}
}

// latency works out a snapshot's Latency. In exact mode the percentiles
// come from the sorted values; otherwise from the histogram. Either way
// the histogram goes with it, so the result can be merged with others. It
// sorts the snapshot's values in place.
func (d *dist) latency() Latency {
	if d.h.Count() == 0 {
		return Latency{}
	}
	if !d.keep {
		return latencyOf(d.h.Clone())
	}
	slices.Sort(d.exact)
	h := d.h.Clone()
	return Latency{
		Min:       d.exact[0],
		Max:       d.exact[len(d.exact)-1],
		Mean:      h.Mean(),
		P50:       percentile(d.exact, 50),
		P90:       percentile(d.exact, 90),
		P95:       percentile(d.exact, 95),
		P99:       percentile(d.exact, 99),
		Histogram: h,
	}
}

// latencyOf is the Latency a histogram gives, with the histogram attached.
// Merge uses it too, so a merged result is worked out exactly as one run's
// would be.
func latencyOf(h *hist.Histogram) Latency {
	if h == nil || h.Count() == 0 {
		return Latency{}
	}
	return Latency{
		Min:       h.Min(),
		Max:       h.Max(),
		Mean:      h.Mean(),
		P50:       h.Percentile(50),
		P90:       h.Percentile(90),
		P95:       h.Percentile(95),
		P99:       h.Percentile(99),
		Histogram: h,
	}
}

// percentile returns the p-th percentile (0-100) of sorted, a slice
// already sorted ascending. It uses the nearest-rank method, the same rule
// hist.Histogram.Percentile estimates; no interpolation.
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p * len(sorted)) / 100
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}
