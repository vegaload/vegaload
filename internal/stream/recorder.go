package stream

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sync"
	"time"
)

// DefaultQueueDepth bounds how many points a sink may have waiting.
const DefaultQueueDepth = 4096

var validRunID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidRunID reports whether id can be used as a run id. The id ends up in
// file names and URLs, so the set is small.
func ValidRunID(id string) error {
	if !validRunID.MatchString(id) {
		return fmt.Errorf("run id %q: use 1 to 64 characters from A-Z a-z 0-9 . _ -", id)
	}
	return nil
}

// NewRunID returns a random 12-hex-digit run id.
func NewRunID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; fall back to the clock.
		return fmt.Sprintf("%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	return hex.EncodeToString(b[:])
}

// Emitter is what a runner needs from a stream. The runner holds one as a
// nil-able interface value: with no stream there is no Emitter and no cost.
// Every method is safe to call from any goroutine and none of them blocks on a
// sink or returns an error.
type Emitter interface {
	// Started begins the stream. at is the instant the run's clock started:
	// offsetS on every event counts from it.
	Started(at time.Time, rs RunStarted)
	// Point reports one closed one-second bucket.
	Point(p Point)
	// Warning reports something the user should know.
	Warning(msg string)
	// Finished ends the stream with the run's result. result is marshalled
	// as it is (the same value `-out` writes); passed and exitCode are added.
	Finished(result any, passed bool, exitCode int)
	// Failed ends the stream for a run that could not run or was interrupted.
	// partial is the partial result, or nil.
	Failed(reason string, partial any)
}

// RecorderOptions configures a Recorder.
type RecorderOptions struct {
	RunID  string            // default: NewRunID()
	Labels map[string]string // attached to every event
	// QueueDepth overrides DefaultQueueDepth (tests).
	QueueDepth int
}

// Recorder turns what the runner reports into events and hands each event to
// every sink. It implements Emitter. Sequence numbers and timestamps are
// assigned once, so every sink sees the same events.
//
// A Recorder never blocks the caller on a sink and never returns an error to
// it: a failing sink only adds a warning.
type Recorder struct {
	opt   RecorderOptions
	sinks []*runner

	mu       sync.Mutex
	seq      int64
	start    time.Time
	started  bool
	finished bool
	warnings []string
	now      func() time.Time
}

var _ Emitter = (*Recorder)(nil)

// NewRecorder creates a Recorder that writes to sinks. With no sinks it still
// numbers events, which is useful in tests.
func NewRecorder(opt RecorderOptions, sinks ...Sink) *Recorder {
	if opt.RunID == "" {
		opt.RunID = NewRunID()
	}
	if opt.QueueDepth <= 0 {
		opt.QueueDepth = DefaultQueueDepth
	}
	r := &Recorder{opt: opt, now: time.Now}
	for _, s := range sinks {
		r.sinks = append(r.sinks, newRunner(s, opt.QueueDepth, r.dropped))
	}
	return r
}

// RunID returns the id used on every event.
func (r *Recorder) RunID() string { return r.opt.RunID }

// HasStarted reports whether run.started has been emitted.
func (r *Recorder) HasStarted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started
}

// Warnings returns what has gone wrong with the sinks so far: a sink that
// stopped accepting events, and events dropped under pressure.
func (r *Recorder) Warnings() []string {
	r.mu.Lock()
	out := append([]string(nil), r.warnings...)
	r.mu.Unlock()
	out = append(out, runnerWarnings(r.sinks)...)
	return out
}

// dropped is called by a sink's goroutine when events were dropped. It tells
// every sink, and is also kept for Warnings.
func (r *Recorder) dropped(sink string, n int) {
	msg := fmt.Sprintf("stream: %d events dropped for %s", n, sink)
	r.mu.Lock()
	r.warnings = append(r.warnings, msg)
	r.mu.Unlock()
	r.emit(TypeWarning, Warning{Message: msg})
}

// Close stops accepting events, waits up to timeout for the sinks to write
// (and, for batching sinks, send) what is queued, and closes them. A sink that
// is still busy at the deadline is aborted. It returns the names of the sinks
// that did not finish.
func (r *Recorder) Close(timeout time.Duration) []string {
	return closeRunners(r.sinks, timeout)
}

// runnerWarnings returns what has gone wrong with the sinks: a sink that
// stopped accepting events, and what a sink reports about itself.
func runnerWarnings(rs []*runner) []string {
	var out []string
	for _, s := range rs {
		if err := s.failed(); err != nil {
			out = append(out, "stream: "+err.Error())
		}
		if w, ok := s.sink.(Warner); ok {
			out = append(out, w.Warnings()...)
		}
	}
	return out
}

// closeRunners tells the sinks the run is over, waits up to timeout for them
// to finish what is queued, and closes them. A sink that is still busy at the
// deadline is aborted. It returns the names of the sinks that did not finish.
func closeRunners(rs []*runner, timeout time.Duration) []string {
	var stuck []string
	deadline := time.After(timeout)
	for _, s := range rs {
		if d, ok := s.sink.(Drainer); ok {
			d.Drain()
		}
		s.q.close()
	}
	expired := false
	for _, s := range rs {
		if !expired {
			select {
			case <-s.done:
				_ = s.sink.Close()
				continue
			case <-deadline:
				expired = true
			}
		} else {
			select {
			case <-s.done:
				_ = s.sink.Close()
				continue
			default:
			}
		}
		// Out of time: give up on this sink.
		if a, ok := s.sink.(Aborter); ok {
			a.Abort()
		}
		select {
		case <-s.done:
			_ = s.sink.Close()
		case <-time.After(300 * time.Millisecond):
		}
		stuck = append(stuck, s.sink.Name())
	}
	return stuck
}

func (r *Recorder) emit(typ string, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished || (!r.started && typ != TypeRunStarted) {
		return
	}
	b, err := json.Marshal(data)
	if err != nil {
		// Event payloads are plain data; a failure here is a bug, and it must
		// not take the run down.
		return
	}
	ts := r.now().UTC()
	r.seq++
	env := Envelope{
		Schema:  Schema,
		RunID:   r.opt.RunID,
		Seq:     r.seq,
		TS:      ts,
		OffsetS: round3(ts.Sub(r.start).Seconds()),
		Labels:  r.opt.Labels,
		Type:    typ,
		Data:    b,
	}
	line, err := json.Marshal(env)
	if err != nil {
		r.seq--
		return
	}
	if typ == TypeRunFinished || typ == TypeRunFailed {
		r.finished = true
	}
	class := classOf(typ)
	for _, s := range r.sinks {
		s.q.push(class, line)
	}
}

// Started implements Emitter. Only the first call counts.
func (r *Recorder) Started(at time.Time, rs RunStarted) {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return
	}
	r.started = true
	r.start = at
	r.mu.Unlock()
	r.emit(TypeRunStarted, rs)
}

// Point implements Emitter.
func (r *Recorder) Point(p Point) { r.emit(TypePoint, p) }

// Warning implements Emitter.
func (r *Recorder) Warning(msg string) { r.emit(TypeWarning, Warning{Message: msg}) }

// Finished implements Emitter.
func (r *Recorder) Finished(result any, passed bool, exitCode int) {
	raw, err := json.Marshal(result)
	if err != nil {
		raw = nil
	}
	r.emit(TypeRunFinished, finished{result: raw, passed: passed, exitCode: exitCode})
}

// Failed implements Emitter.
func (r *Recorder) Failed(reason string, partial any) {
	f := failed{Reason: reason}
	if partial != nil {
		if raw, err := json.Marshal(partial); err == nil {
			f.Result = raw
		}
	}
	r.emit(TypeRunFailed, f)
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
