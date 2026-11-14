// Package genload measures how hard the load generator itself works
// during a run: the CPU the VegaLoad process uses, and how long its
// goroutines wait to be scheduled. A generator that is short of CPU
// starts calls late and adds its own delay to every latency it records,
// so its numbers describe the machine running VegaLoad, not the target.
//
// It knows nothing about the engine, the report or any protocol. The cmd
// layer starts a Monitor around a run and hands the Stats to the report.
//
// The CPU time comes from the operating system (getrusage on Unix,
// GetProcessTimes on Windows), because the runtime's own CPU estimates
// (runtime/metrics /cpu/classes) only move when the garbage collector
// runs. It is this process only: the python3 processes of a Python
// scenario are not in it. The scheduling latency comes from
// runtime/metrics, which every platform has.
package genload

import (
	"math"
	"runtime"
	"runtime/metrics"
	"slices"
	"sync"
	"time"
)

const (
	// HighCPUPercent is the share of the available CPUs at or above which
	// one sample counts as busy.
	HighCPUPercent = 90.0
	// SlowSchedLatency is the goroutine scheduling latency (the sample's
	// p99) at or above which one sample counts as behind. A goroutine that
	// waits this long to run adds that much to whatever it is timing.
	SlowSchedLatency = 20 * time.Millisecond
	// SustainedSamples is how many busy, or behind, samples make a run
	// overloaded. With one-second samples a short burst, such as starting
	// every VU, does not count, and a run shorter than this never does.
	SustainedSamples = 3
	// DefaultInterval is how often a Monitor samples.
	DefaultInterval = time.Second

	// minSchedEvents is the fewest scheduling events a sample needs for
	// its p99 to mean anything; with fewer it is just the slowest one.
	minSchedEvents = 100
)

// Histogram is a cumulative histogram in the shape of
// metrics.Float64Histogram: Counts[i] events fell between Buckets[i] and
// Buckets[i+1], in seconds.
type Histogram struct {
	Counts  []uint64
	Buckets []float64
}

// Snapshot is the generator's cumulative counters at one moment.
type Snapshot struct {
	At time.Time
	// CPU is the CPU time (user and system) the process has used so far.
	// CPUOK is false on a platform where it cannot be read.
	CPU   time.Duration
	CPUOK bool
	// Sched is the goroutine scheduling latency of the process so far.
	Sched Histogram
}

// Source reads a Snapshot. ProcessSource reads the real process; a test
// passes its own.
type Source func() Snapshot

// ProcessSource reads this process's counters.
func ProcessSource() Snapshot {
	s := Snapshot{At: time.Now()}
	s.CPU, s.CPUOK = processCPU()
	sample := []metrics.Sample{{Name: "/sched/latencies:seconds"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() == metrics.KindFloat64Histogram {
		h := sample[0].Value.Float64Histogram()
		s.Sched = Histogram{Counts: slices.Clone(h.Counts), Buckets: h.Buckets}
	}
	return s
}

// CPUs is how many CPUs the generator can use: GOMAXPROCS, which is the
// machine's CPU count unless it was set.
func CPUs() int { return runtime.GOMAXPROCS(0) }

// Stats is what a Monitor measured over a run.
type Stats struct {
	// CPUs is the CPU count the percentages are of.
	CPUs int
	// Samples is how many full samples were taken.
	Samples int
	// CPUMeasured is false when the platform gave no CPU time; the CPU
	// fields are then zero.
	CPUMeasured bool
	// CPUAvgPercent is the CPU used over the whole run, and CPUPeakPercent
	// the most in one sample, as a percentage of all CPUs.
	CPUAvgPercent  float64
	CPUPeakPercent float64
	// BusySamples is how many samples were at or above HighCPUPercent.
	BusySamples int
	// SchedP99 is the goroutine scheduling latency p99 over the whole
	// run, and SchedPeakP99 the highest p99 of one sample.
	SchedP99     time.Duration
	SchedPeakP99 time.Duration
	// SlowSamples is how many samples had a p99 at or above
	// SlowSchedLatency.
	SlowSamples int
}

// CPUHigh says the generator's CPU was busy for SustainedSamples samples
// or more.
func (s Stats) CPUHigh() bool { return s.BusySamples >= SustainedSamples }

// SchedBehind says goroutines waited SlowSchedLatency or more to run in
// SustainedSamples samples or more.
func (s Stats) SchedBehind() bool { return s.SlowSamples >= SustainedSamples }

// Monitor samples a Source while a run goes on.
type Monitor struct {
	src      Source
	cpus     int
	interval time.Duration

	mu          sync.Mutex
	first, last Snapshot
	stats       Stats

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func newMonitor(src Source, cpus int, interval time.Duration) *Monitor {
	if cpus < 1 {
		cpus = 1
	}
	first := src()
	return &Monitor{
		src:      src,
		cpus:     cpus,
		interval: interval,
		first:    first,
		last:     first,
		stats:    Stats{CPUs: cpus, CPUMeasured: first.CPUOK},
	}
}

// Start takes a first Snapshot from src and then one every interval, until
// Stop. cpus is the CPU count the percentages are of.
func Start(src Source, cpus int, interval time.Duration) *Monitor {
	m := newMonitor(src, cpus, interval)
	m.stop = make(chan struct{})
	m.done = make(chan struct{})
	go func() {
		defer close(m.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				m.observe(m.src())
			case <-m.stop:
				return
			}
		}
	}()
	return m
}

// Stop ends the sampling, takes a last Snapshot, and returns the Stats.
// Calling it again returns the same Stats.
func (m *Monitor) Stop() Stats {
	m.stopOnce.Do(func() {
		if m.stop != nil {
			close(m.stop)
			<-m.done
		}
		m.observe(m.src())
	})
	return m.Stats()
}

// Stats returns what has been measured so far.
func (m *Monitor) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}

// observe adds the interval from the last Snapshot to cur. An interval
// shorter than half the sampling interval (the tail of the run, cut by
// Stop) counts in the averages over the whole run, but is not a sample:
// a few milliseconds are too short to say the generator was busy.
func (m *Monitor) observe(cur Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.last
	m.last = cur
	wall := cur.At.Sub(prev.At)
	s := &m.stats

	if s.CPUMeasured && cur.CPUOK {
		if total := cur.At.Sub(m.first.At); total > 0 {
			s.CPUAvgPercent = cpuPercent(cur.CPU-m.first.CPU, total, m.cpus)
		}
	}
	if p, n := p99(m.first.Sched, cur.Sched); n > 0 {
		s.SchedP99 = p
	}
	if wall < m.interval/2 || wall <= 0 {
		return
	}

	s.Samples++
	if s.CPUMeasured && cur.CPUOK {
		pct := cpuPercent(cur.CPU-prev.CPU, wall, m.cpus)
		s.CPUPeakPercent = math.Max(s.CPUPeakPercent, pct)
		if pct >= HighCPUPercent {
			s.BusySamples++
		}
	}
	if p, n := p99(prev.Sched, cur.Sched); n >= minSchedEvents {
		s.SchedPeakP99 = max(s.SchedPeakP99, p)
		if p >= SlowSchedLatency {
			s.SlowSamples++
		}
	}
}

// cpuPercent is cpu as a share of all cpus over wall, at most 100: a
// process can briefly use more than GOMAXPROCS (threads in system calls),
// but no more than the machine has.
func cpuPercent(cpu, wall time.Duration, cpus int) float64 {
	if wall <= 0 || cpu < 0 {
		return 0
	}
	pct := 100 * float64(cpu) / (float64(wall) * float64(cpus))
	return math.Round(math.Min(pct, 100)*10) / 10
}

// p99 is the 99th percentile of the events between two cumulative
// histograms, and how many events there were. It is the lower edge of the
// bucket the percentile falls in, so it never says more than was measured.
func p99(prev, cur Histogram) (time.Duration, uint64) {
	if len(cur.Counts) == 0 || len(cur.Buckets) != len(cur.Counts)+1 {
		return 0, 0
	}
	same := len(prev.Counts) == len(cur.Counts)
	deltas := make([]uint64, len(cur.Counts))
	var total uint64
	for i, c := range cur.Counts {
		if same && prev.Counts[i] <= c {
			c -= prev.Counts[i]
		}
		deltas[i] = c
		total += c
	}
	if total == 0 {
		return 0, 0
	}
	need := uint64(math.Ceil(float64(total) * 0.99))
	var seen uint64
	for i, d := range deltas {
		seen += d
		if seen >= need {
			lower := cur.Buckets[i]
			if math.IsInf(lower, 0) || lower < 0 {
				lower = 0
			}
			return time.Duration(lower * float64(time.Second)), total
		}
	}
	return 0, total
}
