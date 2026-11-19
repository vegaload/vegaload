// Package statsapi serves a running test's cumulative numbers over HTTP, at
// /stats, in the JSON shape of the Harness k6 stats sidecar, so a scraper
// that reads that sidecar reads VegaLoad too. It also serves /healthz.
//
// It reads a report.Result and plain values the runner hands it. The report
// package knows nothing about it, and it sends nothing anywhere: it only
// answers the requests it is sent.
package statsapi

import (
	"math"
	"time"

	"github.com/vegaload/vegaload/internal/hist"
	"github.com/vegaload/vegaload/internal/report"
)

// The states of a run.
const (
	StateStarting = "starting"
	StateRunning  = "running"
	StateFinished = "finished"
	StateFailed   = "failed"
)

// Stats is the body of /stats. Every counter is cumulative from the start of
// the run, so a missed read loses nothing. The fields up to CheckStats are
// the k6 sidecar's; the rest are the JMeter sidecar's users, bytes and
// latency (time to first byte), and VegaLoad's own.
type Stats struct {
	TotalRequests     int64                  `json:"totalRequests"`
	TotalFailures     int64                  `json:"totalFailures"`
	AvgResponseMs     float64                `json:"avgResponseMs"`
	P50Ms             float64                `json:"p50Ms"`
	P95Ms             float64                `json:"p95Ms"`
	P99Ms             float64                `json:"p99Ms"`
	ErrorRate         float64                `json:"errorRate"` // percent, 0 to 100
	AvgRps            float64                `json:"avgRps"`
	Iterations        int64                  `json:"iterations"`
	LatencyHist       map[string]int64       `json:"latencyHist"`
	LatencyHistParams HistParams             `json:"latencyHistParams"`
	RequestStats      map[string]RequestStat `json:"requestStats"`
	CheckStats        map[string]CheckStat   `json:"checkStats"`

	CurrentUsers      int64   `json:"currentUsers"`
	PeakUsers         int64   `json:"peakUsers"`
	RunDurationSec    float64 `json:"runDurationSec"`
	DataSentBytes     int64   `json:"dataSentBytes"`
	DataReceivedBytes int64   `json:"dataReceivedBytes"`
	AvgLatencyMs      float64 `json:"avgLatencyMs"`
	P50LatencyMs      float64 `json:"p50LatencyMs"`
	P95LatencyMs      float64 `json:"p95LatencyMs"`
	P99LatencyMs      float64 `json:"p99LatencyMs"`

	State               string               `json:"state"`
	MaxUsers            int64                `json:"maxUsers"`
	IterationsStarted   int64                `json:"iterationsStarted"`
	IterationsCompleted int64                `json:"iterationsCompleted"`
	DroppedIterations   int64                `json:"droppedIterations"`
	Errors              map[string]ErrorStat `json:"errors"`
	Steps               map[string]StepStat  `json:"steps"`
}

// HistParams names the bucket ladder of every latencyHist: bucket i holds
// the latencies up to BaseMs * Growth^i milliseconds.
type HistParams struct {
	BaseMs float64 `json:"baseMs"`
	Growth float64 `json:"growth"`
}

// RequestStat is one row: an HTTP endpoint, keyed "METHOD:name", or another
// protocol's operation, keyed by its name, "redis command" for example.
type RequestStat struct {
	Name           string           `json:"name"`
	Method         string           `json:"method"`
	NumRequests    int64            `json:"numRequests"`
	NumFailures    int64            `json:"numFailures"`
	AvgResponseMs  float64          `json:"avgResponseMs"`
	MinResponseMs  float64          `json:"minResponseMs"`
	MaxResponseMs  float64          `json:"maxResponseMs"`
	P50Ms          float64          `json:"p50Ms"`
	P95Ms          float64          `json:"p95Ms"`
	P99Ms          float64          `json:"p99Ms"`
	RequestsPerSec float64          `json:"requestsPerSec"`
	AvgSizeBytes   float64          `json:"avgSizeBytes"`
	LatencyHist    map[string]int64 `json:"latencyHist"`
}

// CheckStat is one named check.
type CheckStat struct {
	Name   string `json:"name"`
	Passes int64  `json:"passes"`
	Fails  int64  `json:"fails"`
}

// ErrorStat is one kind of failure (see report.ErrorKind).
type ErrorStat struct {
	Kind   string `json:"kind"`
	Count  int64  `json:"count"`
	Sample string `json:"sample"`
}

// StepStat is one named step.
type StepStat struct {
	Name          string  `json:"name"`
	NumRequests   int64   `json:"numRequests"`
	NumFailures   int64   `json:"numFailures"`
	AvgResponseMs float64 `json:"avgResponseMs"`
	P50Ms         float64 `json:"p50Ms"`
	P95Ms         float64 `json:"p95Ms"`
	P99Ms         float64 `json:"p99Ms"`
}

// Empty is the body before the run has started: every count zero and every
// map empty, not null.
func Empty(state string) Stats {
	return Stats{
		LatencyHist:       map[string]int64{},
		LatencyHistParams: HistParams{BaseMs: hist.BaseMs, Growth: hist.Growth},
		RequestStats:      map[string]RequestStat{},
		CheckStats:        map[string]CheckStat{},
		Errors:            map[string]ErrorStat{},
		Steps:             map[string]StepStat{},
		State:             state,
	}
}

// FromResult is the body for res, the run so far or the finished run.
// currentUsers is the VUs running now.
func FromResult(res *report.Result, currentUsers int64, state string) Stats {
	s := Empty(state)
	if res == nil {
		return s
	}
	secs := res.Elapsed.Seconds()
	s.TotalRequests = res.Total
	s.TotalFailures = res.Failed
	s.AvgResponseMs = ms(res.Latency.Mean)
	s.P50Ms, s.P95Ms, s.P99Ms = ms(res.Latency.P50), ms(res.Latency.P95), ms(res.Latency.P99)
	s.ErrorRate = round(100 * res.ErrorRate)
	s.AvgRps = perSec(res.Total, secs)
	s.Iterations = res.IterationsCompleted
	s.LatencyHist = indexCounts(res.Latency.Histogram)

	for _, p := range res.Protocols {
		for _, o := range p.Operations {
			key, rs := row(p.Name, o)
			rs.RequestsPerSec = perSec(o.Total, secs)
			s.RequestStats[key] = rs
		}
	}
	for _, c := range res.Checks {
		s.CheckStats[c.Name] = CheckStat{Name: c.Name, Passes: c.Passes, Fails: c.Fails}
	}

	s.CurrentUsers = currentUsers
	s.PeakUsers = res.VUsPeak
	s.MaxUsers = res.VUsMax
	s.RunDurationSec = round(secs)
	s.DataSentBytes, s.DataReceivedBytes = res.BytesSent, res.BytesReceived
	if t := res.TTFB; t != nil {
		s.AvgLatencyMs = ms(t.Mean)
		s.P50LatencyMs, s.P95LatencyMs, s.P99LatencyMs = ms(t.P50), ms(t.P95), ms(t.P99)
	}

	s.IterationsStarted = res.IterationsStarted
	s.IterationsCompleted = res.IterationsCompleted
	s.DroppedIterations = res.DroppedIterations
	for _, e := range res.Errors {
		s.Errors[e.Kind] = ErrorStat{Kind: e.Kind, Count: e.Count, Sample: e.Sample}
	}
	for _, st := range res.Steps {
		s.Steps[st.Name] = StepStat{
			Name: st.Name, NumRequests: st.Total, NumFailures: st.Failed,
			AvgResponseMs: ms(st.Latency.Mean),
			P50Ms:         ms(st.Latency.P50), P95Ms: ms(st.Latency.P95), P99Ms: ms(st.Latency.P99),
		}
	}
	return s
}

// row is the requestStats key and entry of one operation of protocol.
func row(protocol string, o report.OperationResult) (string, RequestStat) {
	rs := RequestStat{
		Name:          o.Name,
		NumRequests:   o.Total,
		NumFailures:   o.Failed,
		AvgResponseMs: ms(o.Latency.Mean),
		MinResponseMs: ms(o.Latency.Min),
		MaxResponseMs: ms(o.Latency.Max),
		P50Ms:         ms(o.Latency.P50),
		P95Ms:         ms(o.Latency.P95),
		P99Ms:         ms(o.Latency.P99),
		LatencyHist:   indexCounts(o.Latency.Histogram),
	}
	if o.Total > 0 {
		rs.AvgSizeBytes = round(float64(o.BytesReceived) / float64(o.Total))
	}
	switch {
	case o.Method != "":
		rs.Name, rs.Method = o.Endpoint, o.Method
		return o.Method + ":" + o.Endpoint, rs
	case report.IsHTTP(protocol):
		return o.Name, rs // the Other row
	default:
		rs.Name = protocol + " " + o.Name
		return rs.Name, rs
	}
}

func indexCounts(h *hist.Histogram) map[string]int64 {
	if h == nil {
		return map[string]int64{}
	}
	return h.IndexCounts()
}

func perSec(n int64, secs float64) float64 {
	if secs <= 0 {
		return 0
	}
	return round(float64(n) / secs)
}

func ms(d time.Duration) float64 { return round(float64(d) / float64(time.Millisecond)) }

func round(v float64) float64 { return math.Round(v*1000) / 1000 }
