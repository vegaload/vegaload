package report

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/vegaload/vegaload/internal/hist"
)

// Result is a run's structured outcome: everything the HTML report
// renders, and the same shape FR-RPT-04 says the MCP get_results tool
// (Phase 2) must return, so an agent host can render its own view from
// exactly the same data a human reads in the HTML report.
//
// Total, Failed, ErrorRate, Latency and TimeSeries are the run's requests.
// In a -protocol run a request is one iteration. In a scenario run it is
// one protocol call the scenario made (an HTTP request, a query, a
// publish), and the iterations are counted on their own in Iterations.
type Result struct {
	// Schema names the shape of this JSON. It is Schema for a report
	// whose latencies carry histograms, and empty in reports written
	// before they did.
	Schema string `json:"schema,omitempty"`

	// MergedFrom is how many reports `vegaload merge` combined into this
	// one. It is omitted for a single run.
	MergedFrom int `json:"merged_from,omitempty"`

	Executor  string        `json:"executor"`
	StartedAt time.Time     `json:"started_at"`
	Elapsed   time.Duration `json:"elapsed_ns"`
	Total     int64         `json:"total"`
	Failed    int64         `json:"failed"`
	ErrorRate float64       `json:"error_rate"`
	Latency   Latency       `json:"latency"`

	// FailureReasons counts the failed requests by why they failed, such
	// as "timeout" or "status 503" (see internal/failure), most first. It
	// is omitted when no request failed.
	FailureReasons []FailureReason `json:"failure_reasons,omitempty"`

	// Iterations is how many times a scenario ran its iteration function,
	// and how long a whole iteration took. It is set only for a scenario
	// run; in a -protocol run an iteration is one request.
	Iterations *IterationStats `json:"iterations,omitempty"`

	// DroppedIterations counts the arrivals an arrival-rate executor could
	// not start because every VU was busy. A dropped iteration never ran:
	// it is not a request, so it is in no latency, count or error rate.
	// It is omitted when nothing was dropped.
	DroppedIterations int64 `json:"dropped_iterations,omitempty"`

	// Warnings says what makes the numbers less trustworthy than they
	// look, such as many dropped iterations. It is omitted when there is
	// nothing to say.
	Warnings []string `json:"warnings,omitempty"`

	// Generator is how hard the machine running VegaLoad worked during
	// the run. It is omitted when it was not measured, so a report from
	// before it existed reads the same.
	Generator *GeneratorStats `json:"generator,omitempty"`

	// TimeSeries is RPS and error rate bucketed by one-second intervals
	// from the run's start (FR-RPT-01's "RPS over time" and "error rate
	// over time"). It stays small regardless of request volume — one
	// point per second of run duration, not one per request — which is
	// what lets the HTML report embed it directly even for a
	// 1,000,000-request run (NFR-03).
	TimeSeries []Point `json:"time_series"`

	// Thresholds is the verdict of each pass/fail threshold the run was
	// given (FR-CLI-11), in the order they were given. It is omitted when
	// the run had none, so reports from before thresholds existed, and
	// runs that do not use them, look exactly as before. ThresholdsPassed
	// is true only when every threshold passed.
	Thresholds       []ThresholdResult `json:"thresholds,omitempty"`
	ThresholdsPassed *bool             `json:"thresholds_passed,omitempty"`

	// Aborted is set when the run was stopped early because a threshold
	// was broken (-abort-on-breach, FR-CLI-13). It is omitted for a run
	// that ran to its end.
	Aborted *AbortInfo `json:"aborted,omitempty"`

	// Baseline is the verdict of the baseline gate (FR-CLI-14): this run
	// compared with a baseline report the user supplied. It is omitted
	// when the run had no baseline.
	Baseline *BaselineResult `json:"baseline,omitempty"`

	// Steps is the latency and error rate of each named step the scenario
	// ran with step() (FR-CLI-18), in the order each name was first seen.
	// It is omitted when the scenario used no steps, so reports from
	// before steps existed look exactly as before. A step that throws is a
	// failed step, and it also fails its iteration.
	Steps []StepResult `json:"steps,omitempty"`

	// Protocols is the latency, error rate, and byte totals of each
	// protocol the run called, in the order each name was first seen.
	// It is omitted when the run recorded no call, so a report written
	// before this field existed, and a run that makes no recorded call,
	// look exactly as before.
	Protocols []ProtocolResult `json:"protocols,omitempty"`

	// Checks is the pass and fail count of each named check the scenario
	// made with check() (FR-CLI-12), in the order each name was first
	// seen. It is omitted when the scenario made no checks. A failed
	// check does not fail an iteration or count as a failed request: it
	// is its own measure, which a threshold can judge through the
	// check_rate metric.
	Checks []CheckResult `json:"checks,omitempty"`

	// DataFiles is the name and row count of each data file a protocol
	// run read. It is omitted when the run had no -data, so that JSON
	// stays the same. A cell is never recorded.
	DataFiles []DataFile `json:"data_files,omitempty"`
}

// DataFile is one data file a protocol run read. Name is the file's base
// name. Rows is how many rows it has. A cell is not stored.
type DataFile struct {
	Name string `json:"name"`
	Rows int    `json:"rows"`
}

// GeneratorStats is the load generator's own load during a run: the CPU
// the VegaLoad process used, as a percentage of all the CPUs it can use,
// and how long its goroutines waited to be scheduled. Overloaded is true
// when the run also has a warning starting "generator overloaded:".
type GeneratorStats struct {
	CPUs           int     `json:"cpus"`
	CPUAvgPercent  float64 `json:"cpu_avg_percent"`
	CPUPeakPercent float64 `json:"cpu_peak_percent"`
	// SchedLatencyP99 is the 99th percentile of how long a goroutine
	// waited to run, over the whole run. SchedLatencyPeakP99 is the
	// highest of the one-second p99 values.
	SchedLatencyP99     time.Duration `json:"sched_latency_p99_ns"`
	SchedLatencyPeakP99 time.Duration `json:"sched_latency_peak_p99_ns"`
	Overloaded          bool          `json:"overloaded"`
}

// AbortInfo says why and when a run was stopped early (FR-CLI-13).
type AbortInfo struct {
	// Threshold is the name of the threshold that stopped the run.
	Threshold string `json:"threshold"`
	// Observed is what the run measured when it was stopped.
	Observed string `json:"observed"`
	// At is how far into the run it was stopped.
	At time.Duration `json:"at_ns"`
}

// FailureReason is how many requests failed for one reason.
type FailureReason struct {
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

// IterationStats is a scenario run's iterations: how many ran, how many
// ended in an error, and how long a whole iteration took, every call in it
// included.
type IterationStats struct {
	Total     int64   `json:"total"`
	Failed    int64   `json:"failed"`
	ErrorRate float64 `json:"error_rate"`
	Duration  Latency `json:"duration"`
}

// StepResult is one named step's totals across the whole run: how many
// times it ran, how many of those failed, and its latency.
type StepResult struct {
	Name      string  `json:"name"`
	Total     int64   `json:"total"`
	Failed    int64   `json:"failed"`
	ErrorRate float64 `json:"error_rate"`
	Latency   Latency `json:"latency"`
}

// ProtocolResult is one protocol's totals across the whole run. Latency
// is computed from every call, not from the operation percentiles.
// BytesPerSec is (sent + received) divided by the run's elapsed time.
type ProtocolResult struct {
	Name          string            `json:"name"`
	Total         int64             `json:"total"`
	Failed        int64             `json:"failed"`
	ErrorRate     float64           `json:"error_rate"`
	Latency       Latency           `json:"latency"`
	BytesSent     int64             `json:"bytes_sent"`
	BytesReceived int64             `json:"bytes_received"`
	BytesPerSec   float64           `json:"bytes_per_second"`
	Operations    []OperationResult `json:"operations"`
	Measures      []MeasureResult   `json:"measures,omitempty"`
}

// OperationResult is one operation inside a protocol, such as ftp roundtrip.
// An HTTP protocol (see IsHTTP) has one per endpoint, named "METHOD name",
// such as "GET /users/{id}", with Method and Endpoint set to the two parts.
// Its calls past MaxEndpoints rows share the row OtherEndpoints, which has
// neither.
type OperationResult struct {
	Name          string  `json:"name"`
	Method        string  `json:"method,omitempty"`
	Endpoint      string  `json:"endpoint,omitempty"`
	Total         int64   `json:"total"`
	Failed        int64   `json:"failed"`
	ErrorRate     float64 `json:"error_rate"`
	Latency       Latency `json:"latency"`
	BytesSent     int64   `json:"bytes_sent"`
	BytesReceived int64   `json:"bytes_received"`
}

// MeasureResult is a phase time inside a protocol, such as ftp upload_time.
// A measure is not a call: it does not change the protocol's totals.
type MeasureResult struct {
	Name    string  `json:"name"`
	Count   int64   `json:"count"`
	Latency Latency `json:"latency"`
}

// Operation returns the operation with the given name.
func (p ProtocolResult) Operation(name string) (OperationResult, bool) {
	for _, o := range p.Operations {
		if o.Name == name {
			return o, true
		}
	}
	return OperationResult{}, false
}

// Protocol returns the protocol with the given name, if the run recorded one.
func (r *Result) Protocol(name string) (ProtocolResult, bool) {
	for _, p := range r.Protocols {
		if p.Name == name {
			return p, true
		}
	}
	return ProtocolResult{}, false
}

// ShowProtocols reports whether the text, Markdown, and HTML views should
// print the protocol breakdown. JSON always keeps the data. A run that
// only called http, http1, or http2 keeps the layout it had before this
// breakdown existed: its rows are the endpoints table (see Endpoints).
func ShowProtocols(res *Result) bool {
	if res == nil || len(res.Protocols) == 0 {
		return false
	}
	if len(res.Protocols) > 1 {
		return true
	}
	switch res.Protocols[0].Name {
	case "http", "http1", "http2":
		return false
	default:
		return true
	}
}

// Endpoints is the rows of every HTTP protocol the run called, in order:
// one per "METHOD name", then the Other row if the cap was reached. The
// text, Markdown and HTML views show them as the endpoints table, and leave
// them out of the protocol breakdown.
func Endpoints(res *Result) []OperationResult {
	if res == nil {
		return nil
	}
	var out []OperationResult
	for _, p := range res.Protocols {
		if IsHTTP(p.Name) {
			out = append(out, p.Operations...)
		}
	}
	return out
}

// FormatByteCount renders a byte count as B, KiB, MiB, GiB, TiB, or PiB.
func FormatByteCount(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < len(byteUnits)-1 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), byteUnits[exp])
}

var byteUnits = []string{"KiB", "MiB", "GiB", "TiB", "PiB"}

// Step returns the step with the given name, if the run had one.
func (r *Result) Step(name string) (StepResult, bool) {
	for _, s := range r.Steps {
		if s.Name == name {
			return s, true
		}
	}
	return StepResult{}, false
}

// CheckResult is one named check's totals across the whole run.
type CheckResult struct {
	Name   string `json:"name"`
	Passes int64  `json:"passes"`
	Fails  int64  `json:"fails"`
}

// CheckRate is the share of all checks that passed, from 0 to 1. The
// second result is false when the run made no checks, since a rate of
// nothing is not a rate of 100%.
func (r *Result) CheckRate() (float64, bool) {
	var pass, total int64
	for _, c := range r.Checks {
		pass += c.Passes
		total += c.Passes + c.Fails
	}
	if total == 0 {
		return 0, false
	}
	return float64(pass) / float64(total), true
}

// ThresholdResult is one threshold's verdict: what was asked (a name,
// a metric, an operator and a value) and what the run observed. The
// shape is deliberately plain — a name, a metric, an operator, a value —
// so anything that reads a saved report can read it without knowing how
// VegaLoad evaluated it. Value and Observed are strings so durations
// ("300ms") and plain numbers share one field.
type ThresholdResult struct {
	Name   string `json:"name"`
	Metric string `json:"metric"`
	// Step is the step the threshold judges (FR-CLI-18). It is empty for a
	// threshold on the whole run.
	Step string `json:"step,omitempty"`
	// Protocol and Op are the protocol, and optionally one operation, the
	// threshold judges. They are empty for a threshold on the whole run.
	Protocol string `json:"protocol,omitempty"`
	Op       string `json:"op,omitempty"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
	Observed string `json:"observed"`
	Passed   bool   `json:"passed"`
}

// BaselineResult is the verdict of a run's baseline gate (FR-CLI-14). It
// only covers the two gated metrics, p95 latency and error rate; the full
// comparison is what `vegaload compare` prints.
type BaselineResult struct {
	Path string `json:"path"`
	// MaxRegressionPercent is how much worse than the baseline each gated
	// metric may be before the gate fails.
	MaxRegressionPercent float64          `json:"max_regression_percent"`
	Passed               bool             `json:"passed"`
	Metrics              []BaselineMetric `json:"metrics"`
	Notes                []string         `json:"notes,omitempty"`
}

// BaselineMetric is one gated metric: its baseline and this run's value,
// in the unit given ("ns" for p95, "ratio" for the error rate).
type BaselineMetric struct {
	Name      string  `json:"name"`
	Baseline  float64 `json:"baseline"`
	Candidate float64 `json:"candidate"`
	Unit      string  `json:"unit"`
	Regressed bool    `json:"regressed"`
}

// Schema is the value of Result.Schema in a report this version writes.
const Schema = "vegaload.io/report/v2"

// Latency holds the percentiles and summary stats FR-RPT-01 asks for.
// Min, Max and Mean are exact. The percentiles are estimated from
// Histogram (within hist.MaxError above 0.5 ms), or exact in a run with
// Options.Exact. Histogram is what `vegaload merge` adds up; it is nil in
// a report written before histograms existed, and for no values.
type Latency struct {
	Min       time.Duration   `json:"min_ns"`
	Mean      time.Duration   `json:"mean_ns"`
	Max       time.Duration   `json:"max_ns"`
	P50       time.Duration   `json:"p50_ns"`
	P90       time.Duration   `json:"p90_ns"`
	P95       time.Duration   `json:"p95_ns"`
	P99       time.Duration   `json:"p99_ns"`
	Histogram *hist.Histogram `json:"histogram,omitempty"`
}

// Point is one bucket of Result.TimeSeries.
type Point struct {
	Offset    time.Duration `json:"offset_ns"`
	Requests  int64         `json:"requests"`
	Failed    int64         `json:"failed"`
	RPS       float64       `json:"rps"`
	ErrorRate float64       `json:"error_rate"`
}

// WriteJSON implements FR-CLI-05's structured JSON output mode for a
// finished run's result: res marshalled as indented JSON to path.
func WriteJSON(path string, res *Result) error {
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
