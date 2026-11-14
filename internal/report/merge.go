package report

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vegaload/vegaload/internal/hist"
)

// Mergeable says why r cannot be merged with other results, or nil when it
// can. A report from before histograms existed cannot: its percentiles
// cannot be combined correctly with anything.
func Mergeable(r *Result) error {
	if r.Schema == "" {
		return errors.New("it was written by an older vegaload, without latency histograms, so its percentiles cannot be merged; run it again with this version")
	}
	if r.Schema != Schema {
		return fmt.Errorf("it has the schema %q; this vegaload merges %q", r.Schema, Schema)
	}
	check := func(what string, n int64, l Latency) error {
		var got int64
		if l.Histogram != nil {
			got = l.Histogram.Count()
		}
		if got != n {
			return fmt.Errorf("%s has %d values but its latency histogram holds %d", what, n, got)
		}
		return nil
	}
	errs := []error{check("the run", r.Total, r.Latency)}
	if it := r.Iterations; it != nil {
		errs = append(errs, check("iterations", it.Total, it.Duration))
	}
	for _, s := range r.Steps {
		errs = append(errs, check("step "+s.Name, s.Total, s.Latency))
	}
	for _, p := range r.Protocols {
		errs = append(errs, check("protocol "+p.Name, p.Total, p.Latency))
		for _, o := range p.Operations {
			errs = append(errs, check("operation "+p.Name+" "+o.Name, o.Total, o.Latency))
		}
		for _, m := range p.Measures {
			errs = append(errs, check("measure "+p.Name+" "+m.Name, m.Count, m.Latency))
		}
	}
	return errors.Join(errs...)
}

// Merge combines the results of runs that sent load side by side, from
// different machines, into one result, the same as one run of all of it
// would give. It adds the counts and histograms, takes the smallest
// minimum and the largest maximum, works the mean out from the summed sums,
// and works every percentile and rate out again from the merged data.
// Steps, protocols, operations, measures and checks are matched by name,
// in the order each name was first seen. The time series are lined up by
// each run's start, to the nearest second. Each input's thresholds,
// baseline and abort are left out: they judged a part of the load.
func Merge(rs []*Result) (*Result, error) {
	if len(rs) == 0 {
		return nil, errors.New("no results to merge")
	}
	for i, r := range rs {
		if err := Mergeable(r); err != nil {
			return nil, fmt.Errorf("result %d: %w", i+1, err)
		}
	}

	start, end := rs[0].StartedAt, rs[0].StartedAt.Add(rs[0].Elapsed)
	for _, r := range rs[1:] {
		if r.StartedAt.Before(start) {
			start = r.StartedAt
		}
		if e := r.StartedAt.Add(r.Elapsed); e.After(end) {
			end = e
		}
	}
	out := &Result{Schema: Schema, StartedAt: start, Elapsed: end.Sub(start)}

	reqs := hist.New()
	reasons := map[string]int64{}
	var series []bucketAccum
	var iterations *mergedRuns
	steps := newOrdered[*mergedRuns]()
	checks := newOrdered[*CheckResult]()
	protocols := newOrdered[*mergedProto]()
	var executors, warnings []string
	var aborted int
	seenData := map[DataFile]bool{}

	for _, r := range rs {
		out.MergedFrom += max(r.MergedFrom, 1)
		executors = appendNew(executors, r.Executor)
		out.Total += r.Total
		out.Failed += r.Failed
		reqs.Merge(r.Latency.Histogram)
		for _, fr := range r.FailureReasons {
			reasons[fr.Reason] += fr.Count
		}
		out.DroppedIterations += r.DroppedIterations
		for _, w := range r.Warnings {
			warnings = appendNew(warnings, w)
		}
		if r.Aborted != nil {
			aborted++
		}
		out.Generator = busiestGenerator(out.Generator, r.Generator)
		for _, f := range r.DataFiles {
			if !seenData[f] {
				seenData[f] = true
				out.DataFiles = append(out.DataFiles, f)
			}
		}

		shift := int(r.StartedAt.Sub(start).Round(time.Second) / time.Second)
		for i, p := range r.TimeSeries {
			idx := shift + i
			if idx >= len(series) {
				series = append(series, make([]bucketAccum, idx+1-len(series))...)
			}
			series[idx].count += p.Requests
			series[idx].failed += p.Failed
		}

		if it := r.Iterations; it != nil {
			if iterations == nil {
				iterations = &mergedRuns{h: hist.New()}
			}
			iterations.add(it.Total, it.Failed, it.Duration)
		}
		for _, s := range r.Steps {
			steps.get(s.Name, newMergedRuns).add(s.Total, s.Failed, s.Latency)
		}
		for _, c := range r.Checks {
			m := checks.get(c.Name, func() *CheckResult { return &CheckResult{Name: c.Name} })
			m.Passes += c.Passes
			m.Fails += c.Fails
		}
		for _, p := range r.Protocols {
			mp := protocols.get(p.Name, newMergedProto)
			mp.add(p.Total, p.Failed, p.Latency)
			mp.sent += p.BytesSent
			mp.received += p.BytesReceived
			for _, o := range p.Operations {
				mo := mp.ops.get(o.Name, newMergedRuns)
				mo.add(o.Total, o.Failed, o.Latency)
				mo.sent += o.BytesSent
				mo.received += o.BytesReceived
			}
			for _, m := range p.Measures {
				mp.measures.get(m.Name, newMergedRuns).add(m.Count, 0, m.Latency)
			}
		}
	}

	out.Executor = strings.Join(executors, ",")
	out.ErrorRate = rateOf(out.Failed, out.Total)
	out.Latency = latencyOf(reqs)
	out.FailureReasons = sortedReasons(reasons)
	out.TimeSeries = buildSeries(series)
	if aborted > 0 {
		warnings = append(warnings, fmt.Sprintf("%d of the %d merged reports were stopped early by -abort-on-breach, so they cover less load than planned.", aborted, len(rs)))
	}
	out.Warnings = warnings
	if iterations != nil {
		out.Iterations = &IterationStats{Total: iterations.total, Failed: iterations.failed, ErrorRate: rateOf(iterations.failed, iterations.total), Duration: latencyOf(iterations.h)}
	}
	for _, name := range steps.order {
		s := steps.items[name]
		out.Steps = append(out.Steps, StepResult{Name: name, Total: s.total, Failed: s.failed, ErrorRate: rateOf(s.failed, s.total), Latency: latencyOf(s.h)})
	}
	for _, name := range checks.order {
		out.Checks = append(out.Checks, *checks.items[name])
	}
	secs := out.Elapsed.Seconds()
	for _, name := range protocols.order {
		mp := protocols.items[name]
		pr := ProtocolResult{
			Name: name, Total: mp.total, Failed: mp.failed, ErrorRate: rateOf(mp.failed, mp.total),
			Latency: latencyOf(mp.h), BytesSent: mp.sent, BytesReceived: mp.received,
			BytesPerSec: bytesPerSec(mp.sent+mp.received, secs),
			Operations:  []OperationResult{},
		}
		for _, on := range mp.ops.order {
			o := mp.ops.items[on]
			pr.Operations = append(pr.Operations, OperationResult{
				Name: on, Total: o.total, Failed: o.failed, ErrorRate: rateOf(o.failed, o.total),
				Latency: latencyOf(o.h), BytesSent: o.sent, BytesReceived: o.received,
			})
		}
		for _, mn := range mp.measures.order {
			m := mp.measures.items[mn]
			pr.Measures = append(pr.Measures, MeasureResult{Name: mn, Count: m.total, Latency: latencyOf(m.h)})
		}
		out.Protocols = append(out.Protocols, pr)
	}
	return out, nil
}

// mergedRuns is the merged totals of one latency block.
type mergedRuns struct {
	total, failed  int64
	sent, received int64
	h              *hist.Histogram
}

func newMergedRuns() *mergedRuns { return &mergedRuns{h: hist.New()} }

func (m *mergedRuns) add(total, failed int64, l Latency) {
	m.total += total
	m.failed += failed
	m.h.Merge(l.Histogram)
}

type mergedProto struct {
	mergedRuns
	ops      *ordered[*mergedRuns]
	measures *ordered[*mergedRuns]
}

func newMergedProto() *mergedProto {
	return &mergedProto{mergedRuns: *newMergedRuns(), ops: newOrdered[*mergedRuns](), measures: newOrdered[*mergedRuns]()}
}

// ordered is a map that remembers the order its keys were first seen in.
type ordered[V any] struct {
	order []string
	items map[string]V
}

func newOrdered[V any]() *ordered[V] { return &ordered[V]{items: map[string]V{}} }

func (o *ordered[V]) get(name string, mk func() V) V {
	v, ok := o.items[name]
	if !ok {
		v = mk()
		o.items[name] = v
		o.order = append(o.order, name)
	}
	return v
}

// busiestGenerator folds one generator's load into the merged one. Each
// generator is its own machine, so a CPU share or a scheduling delay cannot
// be added up: the merged report keeps the highest value of each, and is
// overloaded when any generator was.
func busiestGenerator(acc, g *GeneratorStats) *GeneratorStats {
	if g == nil {
		return acc
	}
	if acc == nil {
		c := *g
		return &c
	}
	acc.CPUs = max(acc.CPUs, g.CPUs)
	acc.CPUAvgPercent = max(acc.CPUAvgPercent, g.CPUAvgPercent)
	acc.CPUPeakPercent = max(acc.CPUPeakPercent, g.CPUPeakPercent)
	acc.SchedLatencyP99 = max(acc.SchedLatencyP99, g.SchedLatencyP99)
	acc.SchedLatencyPeakP99 = max(acc.SchedLatencyPeakP99, g.SchedLatencyPeakP99)
	acc.Overloaded = acc.Overloaded || g.Overloaded
	return acc
}

func appendNew(list []string, s string) []string {
	for _, have := range list {
		if have == s {
			return list
		}
	}
	return append(list, s)
}
