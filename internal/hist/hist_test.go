package hist

import (
	"encoding/json"
	"math"
	"math/big"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"
)

// inBucket says, with exact fractions, whether d is in bucket i:
// 0.5 ms·1.05^(i-1) < d <= 0.5 ms·1.05^i, and bucket 0 is (-inf, 0.5 ms].
func inBucket(d time.Duration, i int) bool {
	edge := func(k int) *big.Rat {
		r := new(big.Rat).SetInt64(baseNs)
		g := big.NewRat(21, 20)
		for j := 0; j < k; j++ {
			r.Mul(r, g)
		}
		return r
	}
	v := new(big.Rat).SetInt64(int64(d))
	if v.Cmp(edge(i)) > 0 {
		return false
	}
	return i == 0 || v.Cmp(edge(i-1)) > 0
}

// sidecarIndex is the sidecar's hist_index, in float64 like the original.
func sidecarIndex(ms float64) int {
	if ms <= BaseMs {
		return 0
	}
	return int(math.Ceil(math.Log(ms/BaseMs) / math.Log(Growth)))
}

func TestIndexAtTheEdges(t *testing.T) {
	cases := map[time.Duration]int{
		-time.Second:       0,
		0:                  0,
		1:                  0,
		499_999:            0,
		500_000:            0, // 0.5 ms is the top of bucket 0
		500_001:            1,
		525_000:            1, // 0.5·1.05 ms exactly
		525_001:            2,
		551_250:            2, // 0.5·1.05² ms exactly
		551_251:            3,
		time.Millisecond:   15,
		time.Second:        156,
		time.Hour:          324,
		time.Duration(1e3): 0,
	}
	for d, want := range cases {
		if got := Index(d); got != want {
			t.Errorf("Index(%d ns) = %d, want %d", int64(d), got, want)
		}
	}
	for i := 0; i < 450; i++ {
		e := UpperBound(i)
		for _, d := range []time.Duration{e - 1, e, e + 1} {
			got := Index(d)
			if !inBucket(d, got) {
				t.Fatalf("Index(%d ns) = %d, but exact arithmetic puts it elsewhere (edge of bucket %d)", int64(d), got, i)
			}
		}
		if Index(e) != i || Index(e+1) != i+1 {
			t.Fatalf("bucket %d: its top %d ns is in %d and the next ns in %d", i, int64(e), Index(e), Index(e+1))
		}
	}
}

// Away from an edge, Index is the sidecar's formula. On an edge the float
// formula can go either way; Index follows the exact definition.
func TestIndexMatchesTheSidecarFormula(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for n := 0; n < 200_000; n++ {
		d := time.Duration(math.Exp(rng.Float64()*math.Log(float64(time.Hour)))) + 1
		ms := float64(d) / 1e6
		want := sidecarIndex(ms)
		got := Index(d)
		if got == want {
			continue
		}
		lo, hi := UpperBound(min(got, want)), UpperBound(min(got, want))+1
		if d < lo-1 || d > hi {
			t.Fatalf("Index(%v) = %d, the sidecar formula gives %d, and it is not on an edge", d, got, want)
		}
	}
}

func exactPercentile(sorted []time.Duration, p float64) time.Duration {
	rank := int(math.Floor(p * float64(len(sorted)) / 100))
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

func TestPercentilesAreWithinTwoAndAHalfPercent(t *testing.T) {
	const n = 200_000
	rng := rand.New(rand.NewSource(7))
	dists := map[string]func() time.Duration{
		"uniform 1ms-1s": func() time.Duration {
			return time.Millisecond + time.Duration(rng.Int63n(int64(999*time.Millisecond)))
		},
		"log-normal around 40ms": func() time.Duration {
			return time.Duration(math.Exp(math.Log(40e6)+rng.NormFloat64()*0.8)) + time.Millisecond
		},
		"bimodal 5ms and 800ms": func() time.Duration {
			if rng.Intn(10) < 8 {
				return time.Duration(5e6 * (1 + rng.NormFloat64()*0.05))
			}
			return time.Duration(800e6 * (1 + rng.NormFloat64()*0.1))
		},
		"constant 37ms":    func() time.Duration { return 37 * time.Millisecond },
		"constant 200µs":   func() time.Duration { return 200 * time.Microsecond },
		"exponential 20ms": func() time.Duration { return time.Duration(rng.ExpFloat64()*20e6) + 600*time.Microsecond },
	}
	ps := []float64{0, 1, 10, 25, 50, 75, 90, 95, 99, 99.9, 100}
	for name, gen := range dists {
		h := New()
		vals := make([]time.Duration, n)
		var sum time.Duration
		for i := range vals {
			vals[i] = gen()
			h.Add(vals[i])
			sum += vals[i]
		}
		sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
		if h.Min() != vals[0] || h.Max() != vals[n-1] || h.Sum() != sum || h.Count() != n || h.Mean() != sum/n {
			t.Errorf("%s: min %v max %v sum %v count %d, want exact %v %v %v %d", name, h.Min(), h.Max(), h.Sum(), h.Count(), vals[0], vals[n-1], sum, n)
		}
		for _, p := range ps {
			exact := exactPercentile(vals, p)
			got := h.Percentile(p)
			if rel := math.Abs(float64(got-exact)) / float64(exact); rel > 0.025 {
				t.Errorf("%s: p%g = %v, exact %v, off by %.3f%%", name, p, got, exact, rel*100)
			}
		}
	}
}

func TestPercentileOfFewValuesUsesTheNearestRankRule(t *testing.T) {
	h := New()
	for _, ms := range []int{10, 20, 30, 40, 50} {
		h.Add(time.Duration(ms) * time.Millisecond)
	}
	// rank = floor(p·5/100): p50 is the third value, p95 and p99 the fifth.
	for p, want := range map[float64]time.Duration{0: 10 * time.Millisecond, 50: 30 * time.Millisecond, 95: 50 * time.Millisecond, 99: 50 * time.Millisecond} {
		got := h.Percentile(p)
		if rel := math.Abs(float64(got-want)) / float64(want); rel > MaxError {
			t.Errorf("p%g = %v, want %v within %.2f%%", p, got, want, MaxError*100)
		}
	}
	if h.Percentile(0) != 10*time.Millisecond || h.Percentile(100) != 50*time.Millisecond {
		t.Errorf("the ends are clamped to the exact min and max: p0 %v p100 %v", h.Percentile(0), h.Percentile(100))
	}
	if New().Percentile(50) != 0 {
		t.Error("an empty histogram has no percentile")
	}
}

func TestSubHalfMillisecondValuesStayOrdered(t *testing.T) {
	h := New()
	for i := 1; i <= 100; i++ {
		h.Add(time.Duration(i) * 4 * time.Microsecond) // 4µs to 400µs
	}
	p50, p95, p99 := h.Percentile(50), h.Percentile(95), h.Percentile(99)
	if !(h.Min() <= p50 && p50 < p95 && p95 < p99 && p99 <= h.Max()) {
		t.Fatalf("p50 %v p95 %v p99 %v outside or out of order in [%v, %v]", p50, p95, p99, h.Min(), h.Max())
	}
}

func fill(seed int64, n int) (*Histogram, []time.Duration) {
	rng := rand.New(rand.NewSource(seed))
	h := New()
	vals := make([]time.Duration, n)
	for i := range vals {
		vals[i] = time.Duration(rng.ExpFloat64() * float64(30*time.Millisecond))
		h.Add(vals[i])
	}
	return h, vals
}

func TestMergeIsTheSameAsAddingEverything(t *testing.T) {
	a, va := fill(1, 50_000)
	b, vb := fill(2, 30_000)
	c, vc := fill(3, 70_000)

	all := New()
	for _, vs := range [][]time.Duration{va, vb, vc} {
		for _, v := range vs {
			all.Add(v)
		}
	}
	left := a.Clone()
	left.Merge(b)
	left.Merge(c)
	bc := b.Clone()
	bc.Merge(c)
	right := a.Clone()
	right.Merge(bc)
	reverse := c.Clone()
	reverse.Merge(b)
	reverse.Merge(a)

	for name, got := range map[string]*Histogram{"(a+b)+c": left, "a+(b+c)": right, "c+b+a": reverse} {
		if !reflect.DeepEqual(got.counts, all.counts) || got.n != all.n || got.sum != all.sum || got.min != all.min || got.max != all.max {
			t.Fatalf("%s differs from one histogram of every value", name)
		}
		for _, p := range []float64{50, 90, 95, 99} {
			if got.Percentile(p) != all.Percentile(p) {
				t.Fatalf("%s p%g = %v, want %v", name, p, got.Percentile(p), all.Percentile(p))
			}
		}
	}
	if a.Count() != 50_000 {
		t.Fatal("Merge changed the histogram it read from")
	}
	empty := New()
	empty.Merge(a)
	empty.Merge(nil)
	empty.Merge(New())
	if !reflect.DeepEqual(empty.counts, a.counts) || empty.min != a.min || empty.max != a.max {
		t.Fatal("merging into an empty histogram is not a copy")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	h, _ := fill(9, 10_000)
	h.Add(0)
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var back Histogram
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.counts, h.counts) || back.n != h.n || back.sum != h.sum || back.min != h.min || back.max != h.max {
		t.Fatalf("round trip changed the histogram: %s", raw)
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"base_ms", "growth", "count", "sum_ns", "min_ns", "max_ns", "counts"} {
		if _, ok := shape[k]; !ok {
			t.Errorf("JSON has no %q: %s", k, raw)
		}
	}
	if string(shape["base_ms"]) != "0.5" || string(shape["growth"]) != "1.05" {
		t.Errorf("ladder = %s, %s", shape["base_ms"], shape["growth"])
	}

	var empty Histogram
	raw, _ = json.Marshal(&empty)
	if err := json.Unmarshal(raw, &back); err != nil || back.Count() != 0 {
		t.Fatalf("empty round trip: %s %v", raw, err)
	}
}

func TestJSONRefusesWhatCannotBeMerged(t *testing.T) {
	for name, raw := range map[string]string{
		"another ladder":   `{"base_ms":1,"growth":1.1,"count":1,"sum_ns":1,"min_ns":1,"max_ns":1,"counts":{"0":1}}`,
		"count mismatch":   `{"base_ms":0.5,"growth":1.05,"count":3,"sum_ns":1,"min_ns":1,"max_ns":1,"counts":{"0":1}}`,
		"bad index":        `{"base_ms":0.5,"growth":1.05,"count":1,"sum_ns":1,"min_ns":1,"max_ns":1,"counts":{"x":1}}`,
		"padded index":     `{"base_ms":0.5,"growth":1.05,"count":1,"sum_ns":1,"min_ns":1,"max_ns":1,"counts":{"01":1}}`,
		"negative count":   `{"base_ms":0.5,"growth":1.05,"count":-1,"sum_ns":1,"min_ns":1,"max_ns":1,"counts":{"3":-1}}`,
		"min above max":    `{"base_ms":0.5,"growth":1.05,"count":1,"sum_ns":1,"min_ns":9,"max_ns":1,"counts":{"0":1}}`,
		"index past range": `{"base_ms":0.5,"growth":1.05,"count":1,"sum_ns":1,"min_ns":1,"max_ns":1,"counts":{"99999":1}}`,
	} {
		var h Histogram
		if err := json.Unmarshal([]byte(raw), &h); err == nil {
			t.Errorf("%s: accepted %s", name, raw)
		}
	}
}

// The counts are the sidecar's {"index": count} map, bucketed by its
// formula, so a histogram from either side can be read by the other.
func TestSidecarCountsAreCompatible(t *testing.T) {
	ms := []float64{0.1, 0.5, 0.7, 1, 1, 2.5, 12.345, 12.345, 99, 1000, 4321.5}
	want := map[string]int64{}
	h := New()
	for _, v := range ms {
		want[strconv.Itoa(sidecarIndex(v))]++
		h.Add(time.Duration(v * 1e6))
	}
	if got := h.IndexCounts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("IndexCounts = %v, the sidecar would write %v", got, want)
	}

	back, err := FromIndexCounts(want)
	if err != nil {
		t.Fatal(err)
	}
	if back.Count() != int64(len(ms)) || !reflect.DeepEqual(back.IndexCounts(), want) {
		t.Fatalf("FromIndexCounts: count %d counts %v", back.Count(), back.IndexCounts())
	}
	if back.Min() > h.Min() || back.Max() < h.Max() {
		t.Errorf("estimated min %v and max %v should enclose the real %v and %v", back.Min(), back.Max(), h.Min(), h.Max())
	}
	for _, p := range []float64{50, 90, 99} {
		got, exact := back.Percentile(p), h.Percentile(p)
		if got != exact && math.Abs(float64(got-exact))/float64(exact) > 0.05 {
			t.Errorf("p%g from sidecar counts %v, from the values %v", p, got, exact)
		}
	}
	if _, err := FromIndexCounts(map[string]int64{"-1": 2}); err == nil {
		t.Error("a negative index was accepted")
	}
}

// The histogram's memory is its buckets: it does not grow with the number
// of values.
func TestMemoryDoesNotGrowWithValues(t *testing.T) {
	h := New()
	for i := 0; i < 1_000_000; i++ {
		h.Add(time.Duration(i%5000) * time.Millisecond)
	}
	if len(h.counts) > Index(5*time.Second)+1 || cap(h.counts) > len(h.counts)+8 {
		t.Fatalf("%d buckets (cap %d) for values up to 5s", len(h.counts), cap(h.counts))
	}
}

func BenchmarkAdd(b *testing.B) {
	h := New()
	for i := 0; i < b.N; i++ {
		h.Add(time.Duration(i%400+1) * time.Millisecond)
	}
}
