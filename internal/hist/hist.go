// Package hist is a fixed-memory latency histogram whose buckets are the
// ones the Harness k6 sidecar uses: bucket 0 holds (0, 0.5 ms], and bucket
// i holds (0.5·1.05^(i-1), 0.5·1.05^i] ms, so each bucket is 5% wider than
// the one before it. Two histograms merge by adding their counts, which is
// what lets results from many load generators be combined into one
// correct set of percentiles.
//
// A Histogram keeps one counter per bucket up to the slowest value it has
// seen (about 330 buckets for an hour), plus the exact count, sum, minimum
// and maximum. Its memory does not grow with the number of values added.
package hist

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"time"
)

// BaseMs and Growth define the bucket ladder. They are the sidecar's
// HIST_BASE_MS and HIST_GROWTH.
const (
	BaseMs = 0.5
	Growth = 1.05
)

// MaxError is the largest relative error of a percentile from a bucket
// above the first: the estimate is the bucket's geometric centre, at most
// sqrt(1.05)-1 (about 2.47%) from any value in the bucket. Values in
// bucket 0 (0.5 ms and under) have no such bound.
var MaxError = math.Sqrt(Growth) - 1

const baseNs = 500_000 // BaseMs in nanoseconds

// edges[i] is the largest whole number of nanoseconds in bucket i:
// floor(0.5 ms · 1.05^i). A duration d is in bucket i when
// edges[i-1] < d <= edges[i]. They are worked out with exact fractions, so
// a duration on a bucket's edge never lands in the wrong bucket because of
// floating-point rounding. centers[i] is the estimate for bucket i.
var edges, centers = ladder()

func ladder() ([]int64, []int64) {
	num := big.NewInt(baseNs)
	den := big.NewInt(1)
	q := new(big.Int)
	limit := big.NewInt(math.MaxInt64)
	var es []int64
	for {
		q.Quo(num, den)
		if q.Cmp(limit) > 0 {
			break
		}
		es = append(es, q.Int64())
		num.Mul(num, big.NewInt(21))
		den.Mul(den, big.NewInt(20))
	}
	cs := make([]int64, len(es))
	cs[0] = baseNs / 2
	for i := 1; i < len(es); i++ {
		cs[i] = int64(math.Round(float64(baseNs) * math.Pow(Growth, float64(i)-0.5)))
	}
	return es, cs
}

// Index is the bucket a duration falls in. Zero and negative durations are
// in bucket 0, and so is anything up to and including 0.5 ms.
func Index(d time.Duration) int {
	if d <= baseNs {
		return 0
	}
	i := sort.Search(len(edges), func(i int) bool { return edges[i] >= int64(d) })
	if i == len(edges) {
		i = len(edges) - 1
	}
	return i
}

// UpperBound is the largest duration in bucket i.
func UpperBound(i int) time.Duration {
	if i < 0 {
		return 0
	}
	if i >= len(edges) {
		i = len(edges) - 1
	}
	return time.Duration(edges[i])
}

// Histogram counts durations in the sidecar's buckets, and keeps their
// exact count, sum, minimum and maximum. The zero value is empty and ready
// to use. It is not safe for concurrent use.
type Histogram struct {
	counts []int64 // counts[i] is how many values fell in bucket i
	n      int64
	sum    int64 // nanoseconds
	min    time.Duration
	max    time.Duration
}

// New returns an empty histogram.
func New() *Histogram { return &Histogram{} }

// Add counts one duration. A negative duration counts as zero.
func (h *Histogram) Add(d time.Duration) {
	if d < 0 {
		d = 0
	}
	i := Index(d)
	if i >= len(h.counts) {
		h.grow(i)
	}
	h.counts[i]++
	if h.n == 0 || d < h.min {
		h.min = d
	}
	if d > h.max {
		h.max = d
	}
	h.n++
	h.sum += int64(d)
}

func (h *Histogram) grow(i int) {
	c := make([]int64, i+1, i+1+8)
	copy(c, h.counts)
	h.counts = c
}

// Merge adds every value of o to h, as if each had been added to h.
func (h *Histogram) Merge(o *Histogram) {
	if o == nil || o.n == 0 {
		return
	}
	if len(o.counts) > len(h.counts) {
		h.grow(len(o.counts) - 1)
	}
	for i, c := range o.counts {
		h.counts[i] += c
	}
	if h.n == 0 || o.min < h.min {
		h.min = o.min
	}
	if o.max > h.max {
		h.max = o.max
	}
	h.n += o.n
	h.sum += o.sum
}

// Clone returns a copy of h that shares nothing with it.
func (h *Histogram) Clone() *Histogram {
	c := *h
	c.counts = append([]int64(nil), h.counts...)
	return &c
}

// Count is how many values were added.
func (h *Histogram) Count() int64 { return h.n }

// Sum is the exact total of every value added.
func (h *Histogram) Sum() time.Duration { return time.Duration(h.sum) }

// Min is the exact smallest value added, or 0 when there is none.
func (h *Histogram) Min() time.Duration { return h.min }

// Max is the exact largest value added, or 0 when there is none.
func (h *Histogram) Max() time.Duration { return h.max }

// Mean is the exact mean of the values added, or 0 when there is none.
func (h *Histogram) Mean() time.Duration {
	if h.n == 0 {
		return 0
	}
	return time.Duration(h.sum / h.n)
}

// Percentile estimates the p-th percentile (0 to 100) by the nearest-rank
// rule: the value at 0-based rank floor(p·n/100) of the sorted values,
// clamped to the last. The first and last ranks are the exact minimum and
// maximum. Any other rank's bucket gives the estimate: its geometric
// centre, kept within the exact minimum and maximum. In bucket 0,
// which has no lower edge, the estimate is spread by rank between the
// minimum and the top of the bucket.
func (h *Histogram) Percentile(p float64) time.Duration {
	if h.n == 0 {
		return 0
	}
	rank := int64(math.Floor(p * float64(h.n) / 100))
	if rank < 0 {
		rank = 0
	}
	if rank >= h.n-1 {
		return h.max
	}
	if rank == 0 {
		return h.min
	}
	var seen int64
	for i, c := range h.counts {
		if rank >= seen+c {
			seen += c
			continue
		}
		var est time.Duration
		if i == 0 {
			hi := min(h.max, baseNs)
			est = h.min + time.Duration(float64(hi-h.min)*(float64(rank-seen)+0.5)/float64(c))
		} else {
			est = time.Duration(centers[i])
		}
		return max(h.min, min(h.max, est))
	}
	return h.max
}

// IndexCounts is the sidecar's form of the counts: bucket index, as a
// decimal string, to count, with empty buckets left out.
func (h *Histogram) IndexCounts() map[string]int64 {
	out := make(map[string]int64)
	for i, c := range h.counts {
		if c != 0 {
			out[strconv.Itoa(i)] = c
		}
	}
	return out
}

// FromIndexCounts builds a histogram from the sidecar's form of the counts.
// That form has no exact minimum, maximum or sum, so they are estimated
// from the buckets: the minimum is the lower edge of the first non-empty
// bucket, the maximum the upper edge of the last, and the sum adds each
// bucket's estimate once per value.
func FromIndexCounts(counts map[string]int64) (*Histogram, error) {
	h, err := fromCounts(counts)
	if err != nil {
		return nil, err
	}
	if h.n == 0 {
		return h, nil
	}
	first := true
	for i, c := range h.counts {
		if c == 0 {
			continue
		}
		if first && i > 0 {
			h.min = time.Duration(edges[i-1] + 1)
		}
		first = false
		h.max = time.Duration(edges[i])
		h.sum += c * centers[i]
	}
	return h, nil
}

func fromCounts(counts map[string]int64) (*Histogram, error) {
	h := &Histogram{}
	for k, c := range counts {
		i, err := strconv.Atoi(k)
		if err != nil || i < 0 || i >= len(edges) || strconv.Itoa(i) != k {
			return nil, fmt.Errorf("histogram bucket %q: want a bucket index from 0 to %d", k, len(edges)-1)
		}
		if c < 0 {
			return nil, fmt.Errorf("histogram bucket %s: count %d is negative", k, c)
		}
		if c == 0 {
			continue
		}
		if i >= len(h.counts) {
			h.grow(i)
		}
		h.counts[i] = c
		h.n += c
	}
	return h, nil
}

// wire is the JSON form of a Histogram.
type wire struct {
	BaseMs float64          `json:"base_ms"`
	Growth float64          `json:"growth"`
	Count  int64            `json:"count"`
	SumNs  int64            `json:"sum_ns"`
	MinNs  int64            `json:"min_ns"`
	MaxNs  int64            `json:"max_ns"`
	Counts map[string]int64 `json:"counts"`
}

// MarshalJSON writes the ladder, the exact count, sum, minimum and maximum,
// and the counts in the sidecar's sparse {"index": count} form.
func (h *Histogram) MarshalJSON() ([]byte, error) {
	return json.Marshal(wire{
		BaseMs: BaseMs, Growth: Growth,
		Count: h.n, SumNs: h.sum, MinNs: int64(h.min), MaxNs: int64(h.max),
		Counts: h.IndexCounts(),
	})
}

// UnmarshalJSON reads what MarshalJSON writes. It refuses a histogram on a
// different ladder, since its counts cannot be added to these, and one
// whose count does not match its buckets.
func (h *Histogram) UnmarshalJSON(data []byte) error {
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	if w.BaseMs != BaseMs || w.Growth != Growth {
		return fmt.Errorf("histogram has buckets from %g ms growing by %g; want %g ms growing by %g", w.BaseMs, w.Growth, BaseMs, Growth)
	}
	got, err := fromCounts(w.Counts)
	if err != nil {
		return err
	}
	if got.n != w.Count {
		return fmt.Errorf("histogram count %d does not match its buckets, which hold %d", w.Count, got.n)
	}
	if w.Count > 0 && (w.MinNs < 0 || w.MinNs > w.MaxNs || w.SumNs < 0) {
		return fmt.Errorf("histogram min %d, max %d and sum %d ns do not fit together", w.MinNs, w.MaxNs, w.SumNs)
	}
	got.sum, got.min, got.max = w.SumNs, time.Duration(w.MinNs), time.Duration(w.MaxNs)
	*h = *got
	return nil
}
