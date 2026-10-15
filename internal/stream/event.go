// Package stream defines the VegaLoad run event stream (vegaload.io/stream/v1):
// the event format, the per-sink queues, and the file and push sinks.
//
// The format is documented in docs/streaming.md. This package does not import
// the engine, the report package or any driver: the runner hands it plain
// values through the Emitter interface, so a run with no stream pays nothing
// and the core engine knows nothing about streaming (AGENTS.md, module
// boundaries).
package stream

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// Schema is the value of the schema field on every event.
const Schema = "vegaload.io/stream/v1"

// Event types.
const (
	TypeRunStarted  = "run.started"
	TypePoint       = "point"
	TypeWarning     = "warning"
	TypeRunFinished = "run.finished"
	TypeRunFailed   = "run.failed"
)

// Envelope wraps every event. Data stays raw so that readers keep fields and
// event types they do not know.
type Envelope struct {
	Schema    string            `json:"schema"`
	RunID     string            `json:"runId"`
	Seq       int64             `json:"seq"`
	TS        time.Time         `json:"ts"`
	OffsetS   float64           `json:"offsetS"`
	Labels    map[string]string `json:"labels,omitempty"`
	Synthetic bool              `json:"synthetic,omitempty"`
	Type      string            `json:"type"`
	Data      json.RawMessage   `json:"data"`
}

// Terminal reports whether the event ends a run.
func (e Envelope) Terminal() bool {
	return e.Type == TypeRunFinished || e.Type == TypeRunFailed
}

// RunStarted is the data of run.started. It carries the shape of the run and
// nothing a user supplied as a credential: no header, body, option, secret,
// environment value or data cell (docs/streaming.md, "run.started").
type RunStarted struct {
	Name          string          `json:"name"`
	Executor      string          `json:"executor"`
	VUs           int             `json:"vus,omitempty"`
	Rate          float64         `json:"rate,omitempty"`
	Duration      string          `json:"duration,omitempty"`
	Stages        []Stage         `json:"stages,omitempty"`
	Target        *Target         `json:"target,omitempty"`
	Thresholds    []ThresholdSpec `json:"thresholds,omitempty"`
	AbortOnBreach bool            `json:"abortOnBreach,omitempty"`
	Version       string          `json:"version,omitempty"`
}

// Stage is one ramp or step stage.
type Stage struct {
	Target   int    `json:"target"`
	Duration string `json:"duration"`
}

// Target is the protocol-direct target of a run, without credentials.
type Target struct {
	Protocol string `json:"protocol"`
	BaseURL  string `json:"baseUrl"`
}

// ThresholdSpec is one pass/fail threshold as the run was given it.
type ThresholdSpec struct {
	Name     string `json:"name"`
	Metric   string `json:"metric"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
	Step     string `json:"step,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Op       string `json:"op,omitempty"`
}

// Point is the data of point: one closed one-second bucket of the run.
type Point struct {
	OffsetS     float64 `json:"offsetS"`
	RPS         float64 `json:"rps"`
	Requests    int64   `json:"requests"`
	Failed      int64   `json:"failed"`
	ErrorRate   float64 `json:"errorRate"`
	P50Ms       float64 `json:"p50Ms"`
	P95Ms       float64 `json:"p95Ms"`
	P99Ms       float64 `json:"p99Ms"`
	Total       int64   `json:"total"`
	TotalFailed int64   `json:"totalFailed"`
}

// Warning is the data of warning.
type Warning struct {
	Message string `json:"message"`
}

// finished is the data of run.finished: the run's result exactly as `-out`
// writes it, plus the verdict. It is built by splicing two fields into the
// result's own JSON, so no field of the result is renamed, reordered or
// converted (a number stays the number the report holds).
type finished struct {
	result   json.RawMessage
	passed   bool
	exitCode int
}

func (f finished) MarshalJSON() ([]byte, error) {
	return spliceVerdict(f.result, f.passed, f.exitCode)
}

// spliceVerdict adds "passed" and "exitCode" to the JSON object result.
func spliceVerdict(result json.RawMessage, passed bool, exitCode int) ([]byte, error) {
	extra, err := json.Marshal(struct {
		Passed   bool `json:"passed"`
		ExitCode int  `json:"exitCode"`
	}{passed, exitCode})
	if err != nil {
		return nil, err
	}
	body := bytes.TrimSpace(result)
	if len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
		return extra, nil
	}
	inner := bytes.TrimSpace(body[1 : len(body)-1])
	var out bytes.Buffer
	out.WriteByte('{')
	if len(inner) > 0 {
		out.Write(inner)
		out.WriteByte(',')
	}
	out.Write(extra[1:]) // drops the opening brace of extra
	return out.Bytes(), nil
}

// failed is the data of run.failed.
type failed struct {
	Reason string          `json:"reason"`
	Result json.RawMessage `json:"result,omitempty"`
}

// SanitizeTarget returns target without anything that can carry a credential:
// the userinfo, the query and the fragment of a URL, and a userinfo before an
// "@" in a plain host:port. Anything it cannot read as a URL it returns with
// every "@..." prefix and "?..." suffix cut off, so a secret is never passed
// on by accident.
func SanitizeTarget(target string) string {
	target = strings.TrimSpace(target)
	if strings.Contains(target, "://") {
		if u, err := url.Parse(target); err == nil && u.Host != "" {
			u.User = nil
			u.RawQuery = ""
			u.ForceQuery = false
			u.Fragment = ""
			u.RawFragment = ""
			return u.String()
		}
		// Not parseable as a URL: keep the scheme and drop the rest of any
		// authority up to the last "@".
		scheme, rest, _ := strings.Cut(target, "://")
		rest = cutAfterAny(rest, "?#")
		if i := strings.LastIndex(rest, "@"); i >= 0 {
			rest = rest[i+1:]
		}
		return scheme + "://" + rest
	}
	target = cutAfterAny(target, "?#")
	if i := strings.LastIndex(target, "@"); i >= 0 {
		target = target[i+1:]
	}
	return target
}

func cutAfterAny(s, chars string) string {
	if i := strings.IndexAny(s, chars); i >= 0 {
		return s[:i]
	}
	return s
}
