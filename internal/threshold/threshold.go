// Package threshold implements FR-CLI-11: pass/fail thresholds on a
// run's own results, so a run can gate a CI pipeline.
//
// A threshold is a name, a metric, an operator and a value, for example
//
//	fast-api: p95 < 300ms
//
// It is judged once, against the finished run's numbers — the latency
// percentiles, error rate, request rate and counts VegaLoad itself
// measured. It never looks outside the run, and it produces a verdict
// (pass or fail) and nothing else.
//
// A threshold can judge one named step of a scenario (FR-CLI-18), one
// protocol, or one operation of a protocol, instead of the whole run, by
// adding a selector after the metric:
//
//	login-fast: p95{step="login"} < 300ms
//	ftp-fast: p95{protocol="ftp"} < 500ms
//	rt-ok: error_rate{protocol="ftp",op="roundtrip"} < 1%
package threshold

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vegaload/vegaload/internal/report"
)

// Operators a threshold can use, longest first so "<=" is matched before "<".
var operators = []string{"<=", ">=", "<", ">"}

// kind says how a metric's value is written and compared.
type kind int

const (
	kindDuration kind = iota // 300ms, 1.5s
	kindRatio                // 0.01 or 1%
	kindNumber               // 120.5
	kindCount                // 100
)

var metrics = map[string]kind{
	"p50":        kindDuration,
	"p90":        kindDuration,
	"p95":        kindDuration,
	"p99":        kindDuration,
	"mean":       kindDuration,
	"min":        kindDuration,
	"max":        kindDuration,
	"error_rate": kindRatio,
	"rps":        kindNumber,
	"failed":     kindCount,
	"total":      kindCount,
	// check_rate is the share of check() calls that passed (FR-CLI-12).
	"check_rate": kindRatio,
}

// MetricNames lists the metrics a threshold can use, for help text and
// error messages.
func MetricNames() []string {
	return []string{"p50", "p90", "p95", "p99", "mean", "min", "max", "error_rate", "rps", "failed", "total", "check_rate"}
}

// Threshold is one parsed pass/fail threshold.
type Threshold struct {
	Name   string
	Metric string
	// Step is the named step this threshold judges (FR-CLI-18). It is empty
	// for a threshold on the whole run.
	Step string
	// Protocol and Op judge one protocol, or one operation of it. They are
	// empty for a threshold on the whole run. A threshold names a step or a
	// protocol, never both. Op is set only together with Protocol.
	Protocol string
	Op       string
	Operator string
	// Value is the canonical text of the limit: a duration such as
	// "300ms", or a plain number such as "0.01".
	Value string

	limit float64 // nanoseconds for durations, else the number itself
}

// Parse parses one threshold expression: an optional "name:" prefix,
// then metric, operator and value, e.g. "p95 < 300ms" or
// "fast-api: error_rate <= 1%".
func Parse(expr string) (Threshold, error) {
	orig := expr
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return Threshold{}, fmt.Errorf("empty threshold")
	}

	name := ""
	// The name ends at the first ':' that comes before a selector, so a
	// name such as "a:b" inside {step="a:b"} is not read as a name.
	colon, brace := strings.Index(expr, ":"), strings.Index(expr, "{")
	if colon >= 0 && (brace < 0 || colon < brace) {
		name = strings.TrimSpace(expr[:colon])
		expr = strings.TrimSpace(expr[colon+1:])
		if name == "" {
			return Threshold{}, fmt.Errorf("threshold %q: the name before ':' is empty", orig)
		}
	}

	step, protocol, operation := "", "", ""
	var metric, rest string
	if lb := strings.Index(expr, "{"); lb >= 0 {
		var err error
		if step, protocol, operation, rest, err = parseSelector(expr[lb:]); err != nil {
			return Threshold{}, fmt.Errorf("threshold %q: %w", orig, err)
		}
		metric = strings.ToLower(strings.TrimSpace(expr[:lb]))
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "<") && !strings.HasPrefix(rest, ">") {
			return Threshold{}, fmt.Errorf("threshold %q: want an operator (<, <=, >, >=) and a value after the selector", orig)
		}
	} else {
		idx := strings.IndexAny(expr, "<>")
		if idx < 0 {
			return Threshold{}, fmt.Errorf("threshold %q: want metric, operator and value, e.g. \"p95 < 300ms\" (operators: <, <=, >, >=)", orig)
		}
		metric = strings.ToLower(strings.TrimSpace(expr[:idx]))
		rest = expr[idx:]
	}
	op := ""
	for _, o := range operators {
		if strings.HasPrefix(rest, o) {
			op = o
			break
		}
	}
	valueText := strings.TrimSpace(rest[len(op):])

	k, ok := metrics[metric]
	if !ok {
		return Threshold{}, fmt.Errorf("threshold %q: unknown metric %q (want one of: %s)", orig, metric, strings.Join(MetricNames(), ", "))
	}
	if valueText == "" {
		return Threshold{}, fmt.Errorf("threshold %q: missing value after %q", orig, op)
	}
	if (step != "" || protocol != "" || operation != "") && metric == "check_rate" {
		return Threshold{}, fmt.Errorf("threshold %q: check_rate is for the whole run and cannot target a step or a protocol", orig)
	}

	limit, canonical, err := parseValue(k, metric, valueText)
	if err != nil {
		return Threshold{}, fmt.Errorf("threshold %q: %w", orig, err)
	}

	t := Threshold{Name: name, Metric: metric, Step: step, Protocol: protocol, Op: operation, Operator: op, Value: canonical, limit: limit}
	if t.Name == "" {
		t.Name = t.core()
	}
	return t, nil
}

func selectorFormError() error {
	return fmt.Errorf("the selector must be {step=NAME}, {protocol=NAME}, or {protocol=NAME,op=NAME}, for example p95{step=\"login\"} < 300ms, p95{protocol=\"ftp\"} < 500ms, or error_rate{protocol=\"ftp\",op=\"roundtrip\"} < 1%%")
}

// parseSelector reads a selector from the start of s and returns the step,
// protocol and operation, plus the text after the closing brace. A name may
// be in double or single quotes, or bare when it has no spaces or braces.
// A quoted name may hold spaces, slashes and braces, such as the HTTP row
// op="GET /users/{id}". The keys are case sensitive.
func parseSelector(s string) (step, protocol, operation, rest string, err error) {
	end := -1
	var quote byte
	for i := 1; i < len(s) && end < 0; i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '}':
			end = i
		}
	}
	if quote != 0 {
		return "", "", "", "", fmt.Errorf("the selector is missing its closing quote")
	}
	if end < 0 {
		return "", "", "", "", fmt.Errorf("the selector is missing its closing '}'")
	}
	pairs, err := splitSelector(s[1:end])
	if err != nil {
		return "", "", "", "", err
	}
	if len(pairs) == 0 {
		return "", "", "", "", selectorFormError()
	}
	for _, pair := range pairs {
		key, val, ok := strings.Cut(pair, "=")
		if !ok {
			return "", "", "", "", selectorFormError()
		}
		key = strings.TrimSpace(key)
		val, err = selectorName(key, strings.TrimSpace(val))
		if err != nil {
			return "", "", "", "", err
		}
		switch key {
		case "step":
			if step != "" {
				return "", "", "", "", fmt.Errorf("the selector names %q more than once", key)
			}
			step = val
		case "protocol":
			if protocol != "" {
				return "", "", "", "", fmt.Errorf("the selector names %q more than once", key)
			}
			protocol = val
		case "op":
			if operation != "" {
				return "", "", "", "", fmt.Errorf("the selector names %q more than once", key)
			}
			operation = val
		default:
			return "", "", "", "", selectorFormError()
		}
	}
	if step != "" && (protocol != "" || operation != "") {
		return "", "", "", "", fmt.Errorf("a selector cannot name a step and a protocol")
	}
	if operation != "" && protocol == "" {
		return "", "", "", "", fmt.Errorf("op needs a protocol, for example error_rate{protocol=\"ftp\",op=\"roundtrip\"} < 1%%")
	}
	if step == "" && protocol == "" {
		return "", "", "", "", selectorFormError()
	}
	return step, protocol, operation, s[end+1:], nil
}

func splitSelector(inner string) ([]string, error) {
	var parts []string
	var b strings.Builder
	var quote byte
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if quote != 0 {
			b.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			b.WriteByte(c)
			continue
		}
		if c == ',' {
			parts = append(parts, strings.TrimSpace(b.String()))
			b.Reset()
			continue
		}
		b.WriteByte(c)
	}
	if quote != 0 {
		return nil, fmt.Errorf("the selector is missing its closing quote")
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		parts = append(parts, s)
	}
	return parts, nil
}

func selectorName(key, val string) (string, error) {
	if key != "step" && key != "protocol" && key != "op" {
		return "", selectorFormError()
	}
	if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
		val = val[1 : len(val)-1]
	}
	if val == "" {
		return "", fmt.Errorf("the %s name in the selector is empty", key)
	}
	if strings.ContainsAny(val, "\"'") {
		return "", fmt.Errorf("a %s name with a quote mark cannot be used in a threshold", key)
	}
	return val, nil
}

// selector is the text after the metric that names the step, protocol or
// operation, or "".
func (t Threshold) selector() string {
	switch {
	case t.Step != "":
		return fmt.Sprintf("{step=%q}", t.Step)
	case t.Protocol != "" && t.Op != "":
		return fmt.Sprintf("{protocol=%q,op=%q}", t.Protocol, t.Op)
	case t.Protocol != "":
		return fmt.Sprintf("{protocol=%q}", t.Protocol)
	default:
		return ""
	}
}

// core is the metric, selector, operator and value as text.
func (t Threshold) core() string {
	return fmt.Sprintf("%s%s %s %s", t.Metric, t.selector(), t.Operator, t.Value)
}

func parseValue(k kind, metric, text string) (limit float64, canonical string, err error) {
	switch k {
	case kindDuration:
		d, derr := time.ParseDuration(text)
		if derr != nil {
			return 0, "", fmt.Errorf("%s needs a duration with a unit, like 300ms or 1.5s, not %q", metric, text)
		}
		if d < 0 {
			return 0, "", fmt.Errorf("%s cannot be negative", metric)
		}
		return float64(d.Nanoseconds()), d.String(), nil
	case kindRatio:
		if p, ok := strings.CutSuffix(text, "%"); ok {
			f, perr := strconv.ParseFloat(strings.TrimSpace(p), 64)
			if perr != nil || f < 0 || f > 100 {
				return 0, "", fmt.Errorf("%s %q: want a percentage from 0%% to 100%%", metric, text)
			}
			f /= 100
			return f, formatNumber(f), nil
		}
		f, perr := strconv.ParseFloat(text, 64)
		if perr != nil || f < 0 || f > 1 {
			return 0, "", fmt.Errorf("%s %q: want a fraction from 0 to 1 (like 0.01), or a percentage (like 1%%)", metric, text)
		}
		return f, formatNumber(f), nil
	case kindCount:
		f, perr := strconv.ParseFloat(text, 64)
		if perr != nil || f < 0 || f != float64(int64(f)) {
			return 0, "", fmt.Errorf("%s %q: want a whole number", metric, text)
		}
		return f, formatNumber(f), nil
	default:
		f, perr := strconv.ParseFloat(text, 64)
		if perr != nil || f < 0 {
			return 0, "", fmt.Errorf("%s %q: want a number", metric, text)
		}
		return f, formatNumber(f), nil
	}
}

func formatNumber(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// ParseAll parses several expressions, failing on the first bad one.
func ParseAll(exprs []string) ([]Threshold, error) {
	out := make([]Threshold, 0, len(exprs))
	for _, e := range exprs {
		t, err := Parse(e)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// Expression returns the threshold as text that Parse accepts again,
// including its name, e.g. "fast-api: p95 < 300ms". It is what other
// commands print as a ready-to-paste -threshold flag.
func (t Threshold) Expression() string {
	core := t.core()
	if t.Name == core {
		return core
	}
	return t.Name + ": " + core
}

// Evaluate judges every threshold against a finished run's result, in
// order. A run that completed no requests fails every threshold: with
// nothing measured, "p95 < 300ms" would otherwise pass trivially.
func Evaluate(ts []Threshold, res *report.Result) []report.ThresholdResult {
	out := make([]report.ThresholdResult, 0, len(ts))
	for _, t := range ts {
		r := report.ThresholdResult{Name: t.Name, Metric: t.Metric, Step: t.Step, Protocol: t.Protocol, Op: t.Op, Operator: t.Operator, Value: t.Value}
		if t.Step != "" {
			s, ok := res.Step(t.Step)
			if !ok {
				r.Observed = fmt.Sprintf("no step named %q ran", t.Step)
				out = append(out, r)
				continue
			}
			view := &report.Result{Total: s.Total, Failed: s.Failed, ErrorRate: s.ErrorRate, Latency: s.Latency, Elapsed: res.Elapsed}
			observed := observe(t.Metric, view)
			r.Observed = formatObserved(t.Metric, observed)
			r.Passed = compare(observed, t.Operator, t.limit)
			out = append(out, r)
			continue
		}
		if t.Protocol != "" {
			view, ok := protocolView(res, t.Protocol, t.Op)
			if !ok {
				r.Observed = noProtocolCalls(t)
				out = append(out, r)
				continue
			}
			observed := observe(t.Metric, view)
			r.Observed = formatObserved(t.Metric, observed)
			r.Passed = compare(observed, t.Operator, t.limit)
			out = append(out, r)
			continue
		}
		if t.Metric == "check_rate" {
			rate, any := res.CheckRate()
			if !any {
				r.Observed = "no checks were made"
				out = append(out, r)
				continue
			}
			r.Observed = formatObserved(t.Metric, rate)
			r.Passed = compare(rate, t.Operator, t.limit)
			out = append(out, r)
			continue
		}
		if res.Total == 0 {
			r.Observed = "no requests completed"
			out = append(out, r)
			continue
		}
		observed := observe(t.Metric, res)
		r.Observed = formatObserved(t.Metric, observed)
		r.Passed = compare(observed, t.Operator, t.limit)
		out = append(out, r)
	}
	return out
}

// AllPassed reports whether every result passed. It is true for an
// empty list, since a run with no thresholds has nothing to breach.
func AllPassed(rs []report.ThresholdResult) bool {
	for _, r := range rs {
		if !r.Passed {
			return false
		}
	}
	return true
}

func noProtocolCalls(t Threshold) string {
	if t.Op != "" {
		return fmt.Sprintf("no calls to protocol %q op %q ran", t.Protocol, t.Op)
	}
	return fmt.Sprintf("no calls to protocol %q ran", t.Protocol)
}

// protocolView is the protocol's, or one operation's, totals and latency,
// with the run's elapsed time so rps is calls of that protocol per second
// of the run. ok is false when that protocol or operation recorded nothing.
func protocolView(res *report.Result, protocol, op string) (*report.Result, bool) {
	p, ok := res.Protocol(protocol)
	if !ok || p.Total == 0 {
		return nil, false
	}
	view := &report.Result{Total: p.Total, Failed: p.Failed, ErrorRate: p.ErrorRate, Latency: p.Latency, Elapsed: res.Elapsed}
	if op == "" {
		return view, true
	}
	o, ok := p.Operation(op)
	if !ok || o.Total == 0 {
		return nil, false
	}
	view.Total = o.Total
	view.Failed = o.Failed
	view.ErrorRate = o.ErrorRate
	view.Latency = o.Latency
	return view, true
}

func observe(metric string, res *report.Result) float64 {
	switch metric {
	case "p50":
		return float64(res.Latency.P50.Nanoseconds())
	case "p90":
		return float64(res.Latency.P90.Nanoseconds())
	case "p95":
		return float64(res.Latency.P95.Nanoseconds())
	case "p99":
		return float64(res.Latency.P99.Nanoseconds())
	case "mean":
		return float64(res.Latency.Mean.Nanoseconds())
	case "min":
		return float64(res.Latency.Min.Nanoseconds())
	case "max":
		return float64(res.Latency.Max.Nanoseconds())
	case "error_rate":
		return res.ErrorRate
	case "rps":
		if res.Elapsed <= 0 {
			return 0
		}
		return float64(res.Total) / res.Elapsed.Seconds()
	case "failed":
		return float64(res.Failed)
	case "total":
		return float64(res.Total)
	}
	return 0
}

func formatObserved(metric string, v float64) string {
	switch metrics[metric] {
	case kindDuration:
		return time.Duration(int64(v)).Round(10 * time.Microsecond).String()
	case kindRatio:
		return strconv.FormatFloat(v, 'g', 4, 64)
	case kindNumber:
		return strconv.FormatFloat(v, 'f', 2, 64)
	default:
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
}

func compare(observed float64, op string, limit float64) bool {
	switch op {
	case "<":
		return observed < limit
	case "<=":
		return observed <= limit
	case ">":
		return observed > limit
	case ">=":
		return observed >= limit
	}
	return false
}
