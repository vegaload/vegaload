package report

import (
	"sort"
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
	ops          map[string]*callOp
	opOrder      []string
	measures     map[string][]time.Duration
	measureOrder []string
}

// callOp is one operation. samples is packed like a step: duration in
// nanoseconds shifted left by one, with the low bit set when the call failed.
type callOp struct {
	samples  []uint64
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
	packed := packRun(d, err != nil)
	c.callMu.Lock()
	p := c.protoLocked(protocol)
	o := p.opLocked(op)
	o.samples = append(o.samples, packed)
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
		p.measures = map[string][]time.Duration{}
	}
	samples, known := p.measures[name]
	if !known && len(p.measures) >= maxCallNames {
		name = otherCalls
		samples, known = p.measures[name]
	}
	if !known {
		p.measureOrder = append(p.measureOrder, name)
	}
	p.measures[name] = append(samples, d)
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
		p = &callProto{}
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
		o = &callOp{}
		p.ops[name] = o
		p.opOrder = append(p.opOrder, name)
	}
	return o
}

// snapshotProtocols builds the protocol breakdown. An empty recording is
// nil, so omitempty drops the JSON key.
func (c *Collector) snapshotProtocols(elapsed time.Duration) []ProtocolResult {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	if len(c.callOrder) == 0 {
		return nil
	}
	secs := elapsed.Seconds()
	out := make([]ProtocolResult, 0, len(c.callOrder))
	for _, name := range c.callOrder {
		p := c.calls[name]
		merged := make([]uint64, 0)
		var sent, received int64
		ops := make([]OperationResult, 0, len(p.opOrder))
		for _, on := range p.opOrder {
			o := p.ops[on]
			merged = append(merged, o.samples...)
			sent += o.sent
			received += o.received
			total, failed, lat := unpackCalls(o.samples)
			ops = append(ops, OperationResult{
				Name:          on,
				Total:         total,
				Failed:        failed,
				ErrorRate:     rateOf(failed, total),
				Latency:       lat,
				BytesSent:     o.sent,
				BytesReceived: o.received,
			})
		}
		total, failed, lat := unpackCalls(merged)
		bps := 0.0
		if secs > 0 {
			bps = float64(sent+received) / secs
		}
		var measures []MeasureResult
		for _, mn := range p.measureOrder {
			samples := append([]time.Duration(nil), p.measures[mn]...)
			sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
			measures = append(measures, MeasureResult{
				Name:    mn,
				Count:   int64(len(samples)),
				Latency: latencyOf(samples),
			})
		}
		out = append(out, ProtocolResult{
			Name:          name,
			Total:         total,
			Failed:        failed,
			ErrorRate:     rateOf(failed, total),
			Latency:       lat,
			BytesSent:     sent,
			BytesReceived: received,
			BytesPerSec:   bps,
			Operations:    ops,
			Measures:      measures,
		})
	}
	return out
}

func rateOf(failed, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(failed) / float64(total)
}

func unpackCalls(packed []uint64) (total, failed int64, lat Latency) {
	durs := make([]time.Duration, len(packed))
	for i, v := range packed {
		durs[i] = time.Duration(v >> 1)
		if v&1 == 1 {
			failed++
		}
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	return int64(len(durs)), failed, latencyOf(durs)
}

func latencyOf(sorted []time.Duration) Latency {
	if len(sorted) == 0 {
		return Latency{}
	}
	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}
	return Latency{
		Min:  sorted[0],
		Max:  sorted[len(sorted)-1],
		Mean: sum / time.Duration(len(sorted)),
		P50:  percentile(sorted, 50),
		P90:  percentile(sorted, 90),
		P95:  percentile(sorted, 95),
		P99:  percentile(sorted, 99),
	}
}

// callSampleBytes is the packed call samples currently retained, in bytes.
// It counts length, not slice capacity.
func (c *Collector) callSampleBytes() int {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	n := 0
	for _, p := range c.calls {
		for _, o := range p.ops {
			n += len(o.samples) * 8
		}
		for _, m := range p.measures {
			n += len(m) * 8
		}
	}
	return n
}
