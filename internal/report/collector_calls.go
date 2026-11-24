package report

import (
	"time"

	"github.com/vegaload/vegaload/internal/secrets"
)

// maxCallNames is the cap on protocol names, and on operation and measure
// names inside one protocol. It is a guard against a bug that would
// otherwise store a name from outside the fixed lists. Names past the cap
// share one "(other)" bucket, the same way extra step names do.
const maxCallNames = 32

const otherCalls = "(other)"

// callProto is one protocol's operations and phase measures.
type callProto struct {
	exact        bool
	ops          map[string]*callOp
	opOrder      []string
	measures     map[string]*dist
	measureOrder []string
}

// callOp is one operation: its calls, and the bytes they moved.
type callOp struct {
	runStats
	sent     int64
	received int64
}

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
	c.callMu.Lock()
	p := c.protoLocked(protocol)
	o := p.opLocked(op)
	o.add(d, err != nil)
	o.sent += sent
	o.received += received
	c.callMu.Unlock()
	if c.callsAreRequests.Load() {
		c.addRequest(time.Now().Add(-d), d, err)
	}
}

// RecordMeasure counts one phase time inside a protocol. A measure is not
// a call and does not change that protocol's totals.
func (c *Collector) RecordMeasure(protocol, name string, d time.Duration) {
	if c == nil {
		return
	}
	protocol = callName(protocol)
	name = callName(name)
	c.callMu.Lock()
	defer c.callMu.Unlock()
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
		p = &callProto{exact: c.exact}
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
	if !known && len(p.ops) >= maxCallNames {
		name = otherCalls
		o, known = p.ops[name]
	}
	if !known {
		o = &callOp{runStats: runStats{d: newDist(p.exact)}}
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
			ps.ops = append(ps.ops, opSnap{name: on, d: o.d.snapshot(), failed: o.failed, sent: o.sent, received: o.received})
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
			ops = append(ops, OperationResult{
				Name:          o.name,
				Total:         total,
				Failed:        o.failed,
				ErrorRate:     rateOf(o.failed, total),
				Latency:       o.d.latency(),
				BytesSent:     o.sent,
				BytesReceived: o.received,
			})
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
