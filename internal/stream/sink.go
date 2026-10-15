package stream

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Sink receives the serialized events of a run, one call per event, in order.
// Write is called from a single goroutine per sink and may block: the Recorder
// puts a bounded queue in front of every sink, so a slow sink cannot delay the
// run. A Write error marks the sink dead; the rest of the run's events for
// that sink are discarded.
type Sink interface {
	// Name is used in warnings, for example "file run.ndjson".
	Name() string
	Write(line []byte) error
	Close() error
}

// Flusher is implemented by sinks that batch. The runner calls Flush when the
// deadline passes with nothing new to write, and once more when the run is over.
type Flusher interface {
	// Deadline reports when the pending batch must be sent; ok is false when
	// nothing is pending.
	Deadline() (t time.Time, ok bool)
	Flush() error
}

// Aborter is implemented by sinks that can be stuck in a retry loop. Abort
// makes them give up, so Close can return.
type Aborter interface{ Abort() }

// Drainer is implemented by sinks that retry. Drain is called, from any
// goroutine, when the run is over and the recorder is about to wait for the
// sink for a bounded time. The sink should stop backing off and try for the
// rest of that time, because what is left includes the terminal event.
type Drainer interface{ Drain() }

// Warner is implemented by sinks that report problems without failing, for
// example a batch the receiver rejected.
type Warner interface{ Warnings() []string }

// Drop classes. Lifecycle events are never dropped.
const (
	classLifecycle = iota
	classPoint
)

func classOf(typ string) int {
	if typ == TypePoint {
		return classPoint
	}
	return classLifecycle
}

type queued struct {
	class int
	line  []byte
}

// queue is a sink's bounded buffer. Lifecycle events (run.started, warning,
// run.finished, run.failed) are always accepted: there are few of them. Points
// share maxPoints slots; when they are full an incoming point is dropped.
type queue struct {
	mu           sync.Mutex
	items        []queued
	points       int
	maxPoints    int
	closed       bool
	dropped      int // dropped since last takeDropped
	totalDropped int
	notify       chan struct{}
}

func newQueue(maxPoints int) *queue {
	return &queue{maxPoints: maxPoints, notify: make(chan struct{}, 1)}
}

func (q *queue) wake() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// push never blocks. It reports whether the event was dropped.
func (q *queue) push(class int, line []byte) (dropped bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	if class == classPoint {
		if q.points >= q.maxPoints {
			q.dropped++
			q.totalDropped++
			return true
		}
		q.points++
	}
	q.items = append(q.items, queued{class: class, line: line})
	q.wake()
	return false
}

// Results of pop.
const (
	popItem = iota
	popTimeout
	popClosed
)

// pop waits for an event. wait < 0 waits without a limit; otherwise popTimeout
// is returned after wait. popClosed means the queue is closed and empty.
func (q *queue) pop(wait time.Duration) (queued, int) {
	var timer <-chan time.Time
	if wait >= 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		timer = t.C
	}
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			it := q.items[0]
			q.items[0] = queued{}
			q.items = q.items[1:]
			if it.class == classPoint {
				q.points--
			}
			q.mu.Unlock()
			return it, popItem
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return queued{}, popClosed
		}
		select {
		case <-q.notify:
		case <-timer:
			return queued{}, popTimeout
		}
	}
}

func (q *queue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.wake()
}

// takeDropped returns and clears the number of events dropped since the last call.
func (q *queue) takeDropped() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := q.dropped
	q.dropped = 0
	return n
}

// runner drains one sink's queue on its own goroutine.
type runner struct {
	sink Sink
	q    *queue
	done chan struct{}

	mu   sync.Mutex
	err  error // first write error; the sink is dead afterwards
	last time.Time
}

func newRunner(s Sink, maxPoints int, onDrop func(sink string, n int)) *runner {
	r := &runner{sink: s, q: newQueue(maxPoints), done: make(chan struct{})}
	go r.loop(onDrop)
	return r
}

func (r *runner) loop(onDrop func(string, int)) {
	defer close(r.done)
	fl, batches := r.sink.(Flusher)
	fail := func(err error) {
		r.mu.Lock()
		if r.err == nil {
			r.err = err
		}
		r.mu.Unlock()
	}
	for {
		wait := time.Duration(-1)
		if batches {
			if dl, ok := fl.Deadline(); ok {
				if wait = time.Until(dl); wait < 0 {
					wait = 0
				}
			}
		}
		it, state := r.q.pop(wait)
		switch state {
		case popTimeout:
			if r.failed() == nil {
				if err := fl.Flush(); err != nil {
					fail(err)
				}
			}
		case popClosed:
			// The run is over: send what is batched.
			if batches && r.failed() == nil {
				if err := fl.Flush(); err != nil {
					fail(err)
				}
			}
			return
		default:
			if r.failed() == nil {
				if err := r.sink.Write(it.line); err != nil {
					fail(err)
				}
			}
		}
		// Report drops at most once a second, so a stuck sink does not flood
		// the stream with warnings about itself.
		if onDrop != nil && time.Since(r.last) >= time.Second {
			if n := r.q.takeDropped(); n > 0 {
				r.last = time.Now()
				onDrop(r.sink.Name(), n)
			}
		}
	}
}

func (r *runner) failed() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// FileSink writes events as NDJSON, one line per event, unbuffered, so that
// `tail -f` works and another process can follow the run.
type FileSink struct {
	path string
	f    *os.File
}

// NewFileSink creates (or truncates) the file at path, and the directories
// above it.
func NewFileSink(path string) (*FileSink, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	return &FileSink{path: path, f: f}, nil
}

func (s *FileSink) Name() string { return "file " + s.path }

func (s *FileSink) Write(line []byte) error {
	buf := make([]byte, 0, len(line)+1)
	buf = append(append(buf, line...), '\n')
	if _, err := s.f.Write(buf); err != nil {
		return fmt.Errorf("%s: %w", s.Name(), err)
	}
	return nil
}

func (s *FileSink) Close() error { return s.f.Close() }
