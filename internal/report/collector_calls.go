package report

import (
	"time"

	"github.com/vegaload/vegaload/internal/failure"
	"github.com/vegaload/vegaload/internal/secrets"
	"github.com/vegaload/vegaload/internal/urlname"
)

// maxCallNames is the cap on protocol names, and on operation and measure
// names inside one protocol. It is a guard against a bug that would
// otherwise store a name from outside the fixed lists. Names past the cap
// share one "(other)" bucket, the same way extra step names do.
const maxCallNames = 32

const otherCalls = "(other)"

// MaxEndpoints is the cap on the rows of an HTTP protocol, one per
// "METHOD name". The rows past it share one row named OtherEndpoints, so
// the totals stay exact and memory stays bounded however many URLs a
// script calls. The cap and the name match the Harness k6 stats sidecar.
const MaxEndpoints = 500

// OtherEndpoints is the row of the HTTP calls past MaxEndpoints.
const OtherEndpoints = urlname.Other

// IsHTTP reports whether protocol records one row per endpoint: http in a
// scenario, and http1 or http2 in -protocol mode.
func IsHTTP(protocol string) bool {
	switch protocol {
	case "http", "http1", "http2":
		return true
	}
	return false
}

// opCap is how many operations protocol keeps, and the name of the row the
// rest share.
func opCap(protocol string) (int, string) {
	if IsHTTP(protocol) {
		return MaxEndpoints, OtherEndpoints
	}
	return maxCallNames, otherCalls
}

// callProto is one protocol's operations and phase measures.
type callProto struct {
	name         string
	exact        bool
	maxOps       int
	otherOp      string
	ops          map[string]*callOp
	opOrder      []string
	measures     map[string]*dist
	measureOrder []string
}

// callOp is one operation: its calls, the bytes they moved, and its
// failures by kind. name is the row it is stored under.
type callOp struct {
	runStats
	name     string
	sent     int64
	received int64
	errs     errorKinds
}

// TTFBMeasure is the measure name of the time to the first byte of an
// answer. Besides the protocol's measure, every one goes into Result.TTFB
// and the live points.
const TTFBMeasure = "ttfb"

// RecordCall counts one finished protocol call that took d and failed with
// err, or worked when err is nil. A nil collector counts nothing. protocol
// and op are redacted before they are stored. After RequestsFromCalls, the
// call is also one request of the run.
func (c *Collector) RecordCall(protocol, op string, d time.Duration, err error, sent, received int64) {
	if c == nil {
		return
	}
	protocol = callName(protocol)
	op = callName(op)
	failed := err != nil
	var reason string
	if failed {
		reason = failure.Reason(err)
	}
	c.callMu.Lock()
	p := c.protoLocked(protocol)
	o := p.opLocked(op)
	o.add(d, failed)
	o.sent += sent
	o.received += received
	if failed {
		o.errs.addErr(reason, err)
	}
	lc := &liveCall{protocol: p.name, op: o.name, sent: sent, received: received}
	c.callMu.Unlock()
	start := time.Now().Add(-d)
	if c.callsAreRequests.Load() {
		c.addRequest(request{start: start, d: d, err: err, reason: reason, call: lc})
		return
	}
	c.withLive(start, func(s *liveSec) { s.addCall(lc, d, failed) })
}

// RecordMeasure counts one phase time inside a protocol. A measure is not
// a call and does not change that protocol's totals.
func (c *Collector) RecordMeasure(protocol, name string, d time.Duration) {
	if c == nil {
		return
	}
	protocol = callName(protocol)
	name = callName(name)
	ttfb := name == TTFBMeasure
	c.addMeasure(protocol, name, d, ttfb)
	if ttfb {
		c.withLive(time.Now().Add(-d), func(s *liveSec) { s.ttfb.Add(d) })
	}
}

func (c *Collector) addMeasure(protocol, name string, d time.Duration, ttfb bool) {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	if ttfb {
		c.ttfb.add(d)
	}
	p := c.protoLocked(protocol)
	if p.measures == nil {
		p.measures = map[string]*dist{}
	}
	m, known := p.measures[name]
	if !known && len(p.measures) >= maxCallNames {
		name = otherCalls
		m, known = p.measures[name]
	}
	if !known {
		m = newDist(p.exact)
		p.measures[name] = m
		p.measureOrder = append(p.measureOrder, name)
	}
	m.add(d)
}

// callName redacts a stored name. It stays out of line so the compiler does
// not copy this body into every caller. That copy is what makes the binary
// larger.
//
//go:noinline
func callName(name string) string {
	name = secrets.Redact(name)
	if name == "" {
		return otherCalls
	}
	return name
}

// protoLocked returns the protocol bucket, creating it or folding a new
// name into "(other)" once the cap is reached. It stays out of line so the
// compiler does not copy this body into every caller. That copy is what
// makes the binary larger.
//
//go:noinline
func (c *Collector) protoLocked(name string) *callProto {
	if c.calls == nil {
		c.calls = map[string]*callProto{}
	}
	p, known := c.calls[name]
	if !known && len(c.calls) >= maxCallNames {
		name = otherCalls
		p, known = c.calls[name]
	}
	if !known {
		p = &callProto{exact: c.exact, name: name}
		p.maxOps, p.otherOp = opCap(name)
		c.calls[name] = p
		c.callOrder = append(c.callOrder, name)
	}
	return p
}

// opLocked returns the operation bucket. It stays out of line so the
// compiler does not copy this body into every caller. That copy is what
// makes the binary larger.
//
//go:noinline
func (p *callProto) opLocked(name string) *callOp {
	if p.ops == nil {
		p.ops = map[string]*callOp{}
	}
	o, known := p.ops[name]
	if !known && len(p.ops) >= p.maxOps {
		name = p.otherOp
		o, known = p.ops[name]
	}
	if !known {
		o = &callOp{runStats: runStats{d: newDist(p.exact)}, name: name}
		p.ops[name] = o
		p.opOrder = append(p.opOrder, name)
	}
	return o
}

// opSnap is an operation or a measure copied under callMu, so its latency
// can be worked out after the lock is released.
type opSnap struct {
	name           string
	d              *dist
	failed         int64
	sent, received int64
	errs           []ErrorKind
}

// snapshotProtocols builds the protocol breakdown. An empty recording is
// nil, so omitempty drops the JSON key.
func (c *Collector) snapshotProtocols(elapsed time.Duration) []ProtocolResult {
	type protoSnap struct {
		name     string
		ops      []opSnap
		measures []opSnap
	}
	c.callMu.Lock()
	snaps := make([]protoSnap, 0, len(c.callOrder))
	for _, name := range c.callOrder {
		p := c.calls[name]
		ps := protoSnap{name: name}
		for _, on := range p.opOrder {
			o := p.ops[on]
			ps.ops = append(ps.ops, opSnap{name: on, d: o.d.snapshot(), failed: o.failed, sent: o.sent, received: o.received, errs: o.errs.list()})
		}
		for _, mn := range p.measureOrder {
			ps.measures = append(ps.measures, opSnap{name: mn, d: p.measures[mn].snapshot()})
		}
		snaps = append(snaps, ps)
	}
	c.callMu.Unlock()
	if len(snaps) == 0 {
		return nil
	}

	secs := elapsed.Seconds()
	out := make([]ProtocolResult, 0, len(snaps))
	for _, ps := range snaps {
		all := newDist(c.exact)
		var failed, sent, received int64
		ops := make([]OperationResult, 0, len(ps.ops))
		for _, o := range ps.ops {
			all.merge(o.d)
			failed += o.failed
			sent += o.sent
			received += o.received
			total := o.d.h.Count()
			ops = append(ops, withEndpoint(ps.name, OperationResult{
				Name:          o.name,
				Total:         total,
				Failed:        o.failed,
				ErrorRate:     rateOf(o.failed, total),
				Latency:       o.d.latency(),
				BytesSent:     o.sent,
				BytesReceived: o.received,
				Errors:        o.errs,
			}))
		}
		var measures []MeasureResult
		for _, m := range ps.measures {
			measures = append(measures, MeasureResult{
				Name:    m.name,
				Count:   m.d.h.Count(),
				Latency: m.d.latency(),
			})
		}
		total := all.h.Count()
		out = append(out, ProtocolResult{
			Name:          ps.name,
			Total:         total,
			Failed:        failed,
			ErrorRate:     rateOf(failed, total),
			Latency:       all.latency(),
			BytesSent:     sent,
			BytesReceived: received,
			BytesPerSec:   bytesPerSec(sent+received, secs),
			Operations:    ops,
			Measures:      measures,
		})
	}
	return out
}

// withEndpoint sets the Method and Endpoint of an HTTP protocol's row from
// its name. Other rows, and the Other row, keep them empty.
func withEndpoint(protocol string, o OperationResult) OperationResult {
	if IsHTTP(protocol) {
		if m, n, ok := urlname.Split(o.Name); ok {
			o.Method, o.Endpoint = m, n
		}
	}
	return o
}

func bytesPerSec(bytes int64, secs float64) float64 {
	if secs <= 0 {
		return 0
	}
	return float64(bytes) / secs
}

func rateOf(failed, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(failed) / float64(total)
}
