package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/vegaload/vegaload/internal/genload"
	"github.com/vegaload/vegaload/internal/report"
)

// How a run measures the load generator. They are variables only so a
// test can feed a fake Source and not wait a second per sample.
var (
	generatorSource   genload.Source = genload.ProcessSource
	generatorInterval                = genload.DefaultInterval
	generatorCPUs                    = genload.CPUs
)

// overloadPrefix starts the one warning of a run whose generator could not
// keep up, so a reader (or a script) can find it among the others.
const overloadPrefix = "generator overloaded: "

func startGenerator() *genload.Monitor {
	return genload.Start(generatorSource, generatorCPUs(), generatorInterval)
}

// applyGenerator records what the generator measured on the result, and
// adds the overload warning when there is one. droppedPercent is the
// share of planned iterations that were dropped.
func applyGenerator(result *report.Result, s genload.Stats, droppedPercent float64) {
	w := overloadWarning(s, droppedPercent)
	result.Generator = &report.GeneratorStats{
		CPUs:                s.CPUs,
		CPUAvgPercent:       s.CPUAvgPercent,
		CPUPeakPercent:      s.CPUPeakPercent,
		SchedLatencyP99:     s.SchedP99,
		SchedLatencyPeakP99: s.SchedPeakP99,
		Overloaded:          w != "",
	}
	if w != "" {
		result.Warnings = append(result.Warnings, w)
	}
}

// overloadWarning says why the generator could not send the planned load
// on time, or is "" when it could: its CPU stayed busy, its goroutines
// waited too long to run, or more than report.DropWarnPercent of the
// planned iterations were dropped. The drop has its own warning with the
// numbers and the fix; this one names it among the causes.
func overloadWarning(s genload.Stats, droppedPercent float64) string {
	var why []string
	short := s.CPUHigh() || s.SchedBehind()
	if s.CPUHigh() {
		cpus := fmt.Sprintf("%d CPUs", s.CPUs)
		if s.CPUs == 1 {
			cpus = "1 CPU"
		}
		why = append(why, fmt.Sprintf("CPU use was at or above %.0f%% of %s for %d of %d seconds (peak %.0f%%)",
			genload.HighCPUPercent, cpus, s.BusySamples, s.Samples, s.CPUPeakPercent))
	}
	if s.SchedBehind() {
		why = append(why, fmt.Sprintf("goroutines waited %s or more to run for %d of %d seconds (peak p99 %s)",
			genload.SlowSchedLatency, s.SlowSamples, s.Samples, s.SchedPeakP99.Round(time.Millisecond)))
	}
	if droppedPercent > report.DropWarnPercent {
		why = append(why, fmt.Sprintf("%.2f%% of the planned iterations were dropped because every VU was busy", droppedPercent))
	}
	if len(why) == 0 {
		return ""
	}
	effect := "so the target got less load than planned."
	if short {
		effect = "so latency and throughput may show this machine's limits, not the target's. Run fewer VUs on this machine, or spread the load over more machines."
	}
	return overloadPrefix + strings.Join(why, "; ") + ". VegaLoad could not send the planned load on time, " + effect
}
