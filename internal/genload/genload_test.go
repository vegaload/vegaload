package genload

import (
	"math"
	"testing"
	"time"
)

// schedBuckets are histogram edges in seconds: 0-1ms, 1-10ms, 10-20ms,
// 20-50ms, 50ms and up.
var schedBuckets = []float64{0, 0.001, 0.01, 0.02, 0.05, math.Inf(1)}

// fake builds the cumulative snapshots a Source would return, one second
// (or any step) at a time.
type fake struct {
	t0  time.Time
	cur Snapshot
}

func newFake() *fake {
	t0 := time.Unix(1000, 0)
	return &fake{t0: t0, cur: Snapshot{At: t0, CPUOK: true, Sched: Histogram{Counts: make([]uint64, 5), Buckets: schedBuckets}}}
}

// step moves the clock by wall, adds cpu of CPU time, and adds counts to
// the scheduling histogram.
func (f *fake) step(wall, cpu time.Duration, counts ...uint64) Snapshot {
	f.cur.At = f.cur.At.Add(wall)
	f.cur.CPU += cpu
	c := append([]uint64(nil), f.cur.Sched.Counts...)
	for i, n := range counts {
		c[i] += n
	}
	f.cur.Sched = Histogram{Counts: c, Buckets: schedBuckets}
	return f.cur
}

func monitorOn(f *fake, cpus int) *Monitor {
	return newMonitor(func() Snapshot { return f.cur }, cpus, time.Second)
}

func TestMonitor_SustainedHighCPU(t *testing.T) {
	f := newFake()
	m := monitorOn(f, 4)
	for i := 0; i < 3; i++ {
		m.observe(f.step(time.Second, 3800*time.Millisecond)) // 95% of 4 CPUs
	}
	s := m.Stats()
	if s.Samples != 3 || s.BusySamples != 3 || !s.CPUHigh() {
		t.Fatalf("stats %+v, want three busy samples and CPUHigh", s)
	}
	if s.CPUPeakPercent != 95 || s.CPUAvgPercent != 95 || s.CPUs != 4 {
		t.Errorf("avg %v peak %v cpus %d, want 95, 95, 4", s.CPUAvgPercent, s.CPUPeakPercent, s.CPUs)
	}
}

// A burst shorter than SustainedSamples, such as starting every VU, is not
// an overload.
func TestMonitor_ShortBurstIsNotHigh(t *testing.T) {
	f := newFake()
	m := monitorOn(f, 2)
	m.observe(f.step(time.Second, 2*time.Second))
	m.observe(f.step(time.Second, 2*time.Second))
	for i := 0; i < 5; i++ {
		m.observe(f.step(time.Second, 200*time.Millisecond))
	}
	s := m.Stats()
	if s.CPUHigh() || s.BusySamples != 2 || s.CPUPeakPercent != 100 {
		t.Errorf("stats %+v, want two busy samples, peak 100, and not CPUHigh", s)
	}
	if want := 100 * 5.0 / 14; math.Abs(s.CPUAvgPercent-want) > 0.1 {
		t.Errorf("avg %v, want about %.1f", s.CPUAvgPercent, want)
	}
}

// The tail cut by Stop counts in the average but is not a sample.
func TestMonitor_ShortTailIsNotASample(t *testing.T) {
	f := newFake()
	m := monitorOn(f, 1)
	m.observe(f.step(time.Second, 100*time.Millisecond))
	m.observe(f.step(100*time.Millisecond, 100*time.Millisecond))
	s := m.Stats()
	if s.Samples != 1 || s.CPUPeakPercent != 10 {
		t.Errorf("stats %+v, want one sample with peak 10", s)
	}
	if want := 100 * 0.2 / 1.1; math.Abs(s.CPUAvgPercent-want) > 0.1 {
		t.Errorf("avg %v, want about %.1f", s.CPUAvgPercent, want)
	}
}

func TestMonitor_SchedulingBehind(t *testing.T) {
	f := newFake()
	m := monitorOn(f, 8)
	// 98% under 1ms, 2% at 20-50ms: the p99 is at least 20ms.
	for i := 0; i < 3; i++ {
		m.observe(f.step(time.Second, 0, 980, 0, 0, 20))
	}
	s := m.Stats()
	if !s.SchedBehind() || s.SlowSamples != 3 || s.SchedPeakP99 != 20*time.Millisecond || s.SchedP99 != 20*time.Millisecond {
		t.Errorf("stats %+v, want three slow samples at 20ms", s)
	}
	if s.CPUHigh() {
		t.Error("an idle CPU counted as high")
	}
}

func TestMonitor_SchedulingNeedsEnoughEvents(t *testing.T) {
	f := newFake()
	m := monitorOn(f, 8)
	for i := 0; i < 5; i++ {
		m.observe(f.step(time.Second, 0, 0, 0, 0, 0, 10)) // ten slow events a second
	}
	if s := m.Stats(); s.SlowSamples != 0 || s.SchedBehind() {
		t.Errorf("stats %+v, want no slow sample from ten events", s)
	}
}

func TestMonitor_NoCPUReading(t *testing.T) {
	f := newFake()
	f.cur.CPUOK = false
	m := monitorOn(f, 2)
	for i := 0; i < 4; i++ {
		m.observe(f.step(time.Second, 2*time.Second))
	}
	if s := m.Stats(); s.CPUMeasured || s.CPUHigh() || s.CPUPeakPercent != 0 {
		t.Errorf("stats %+v, want no CPU numbers where CPU cannot be read", s)
	}
}

func TestP99(t *testing.T) {
	prev := Histogram{Counts: []uint64{10, 0, 0, 0, 0}, Buckets: schedBuckets}
	cur := Histogram{Counts: []uint64{1000, 0, 100, 0, 0}, Buckets: schedBuckets}
	// 990 under 1ms and 100 at 10-20ms: the p99 is in the 10-20ms bucket.
	if p, n := p99(prev, cur); n != 1090 || p != 10*time.Millisecond {
		t.Errorf("p99 = %s over %d, want 10ms over 1090", p, n)
	}
	if p, n := p99(cur, cur); p != 0 || n != 0 {
		t.Errorf("no new events: %s over %d", p, n)
	}
	if _, n := p99(Histogram{}, Histogram{}); n != 0 {
		t.Error("an empty histogram has events")
	}
}

// The real process can be read and sampled.
func TestStart_ProcessSource(t *testing.T) {
	m := Start(ProcessSource, CPUs(), 20*time.Millisecond)
	deadline := time.Now().Add(120 * time.Millisecond)
	for time.Now().Before(deadline) {
	}
	s := m.Stop()
	if s.Samples < 2 || s.CPUs < 1 {
		t.Errorf("stats %+v, want a few samples", s)
	}
	if s.CPUMeasured && s.CPUAvgPercent <= 0 {
		t.Errorf("a busy loop measured %v%% CPU", s.CPUAvgPercent)
	}
	if again := m.Stop(); again != s {
		t.Errorf("a second Stop changed the stats: %+v", again)
	}
}
