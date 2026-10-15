package stream

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// memSink keeps every line it is given. Block, when set, holds Write until it
// is closed, so a test can fill the queue behind it.
type memSink struct {
	mu    sync.Mutex
	lines [][]byte
	Block chan struct{}
}

func (m *memSink) Name() string { return "mem" }
func (m *memSink) Write(line []byte) error {
	if m.Block != nil {
		<-m.Block
	}
	m.mu.Lock()
	m.lines = append(m.lines, append([]byte(nil), line...))
	m.mu.Unlock()
	return nil
}
func (m *memSink) Close() error { return nil }
func (m *memSink) events(t *testing.T) []Envelope {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Envelope
	for _, l := range m.lines {
		var e Envelope
		if err := json.Unmarshal(l, &e); err != nil {
			t.Fatalf("a line is not JSON: %v: %s", err, l)
		}
		out = append(out, e)
	}
	return out
}

func startedAt() RunStarted { return RunStarted{Name: "x", Executor: "fixed-vus"} }

func TestRecorderNumbersEventsAndSharesThemAcrossSinks(t *testing.T) {
	a, b := &memSink{}, &memSink{}
	r := NewRecorder(RecorderOptions{RunID: "abc123", Labels: map[string]string{"k": "v"}}, a, b)
	at := time.Now()
	r.Started(at, startedAt())
	r.Point(Point{OffsetS: 0, Requests: 3})
	r.Point(Point{OffsetS: 1, Requests: 4})
	r.Finished(json.RawMessage(`{"total":7}`), true, 0)
	if stuck := r.Close(2 * time.Second); len(stuck) != 0 {
		t.Fatalf("sinks did not finish: %v", stuck)
	}
	ea, eb := a.events(t), b.events(t)
	if len(ea) != 4 || len(eb) != 4 {
		t.Fatalf("want 4 events in both sinks, got %d and %d", len(ea), len(eb))
	}
	for i := range ea {
		if ea[i].Seq != int64(i+1) || eb[i].Seq != ea[i].Seq || !ea[i].TS.Equal(eb[i].TS) {
			t.Errorf("event %d differs between sinks or is not numbered 1..n: %+v vs %+v", i, ea[i], eb[i])
		}
		if ea[i].Schema != Schema || ea[i].RunID != "abc123" || ea[i].Labels["k"] != "v" {
			t.Errorf("event %d envelope wrong: %+v", i, ea[i])
		}
	}
	if ea[0].Type != TypeRunStarted || ea[3].Type != TypeRunFinished {
		t.Errorf("first/last types: %s, %s", ea[0].Type, ea[3].Type)
	}
}

func TestRecorderEndsOnceAndIgnoresEventsOutsideTheRun(t *testing.T) {
	s := &memSink{}
	r := NewRecorder(RecorderOptions{RunID: "r1"}, s)
	r.Point(Point{})                      // before run.started
	r.Finished(map[string]int{}, true, 0) // before run.started
	r.Started(time.Now(), startedAt())
	r.Started(time.Now(), startedAt()) // only the first counts
	r.Failed("interrupted", nil)
	r.Point(Point{})                      // after the end
	r.Finished(map[string]int{}, true, 0) // a second ending
	r.Failed("again", nil)
	r.Close(2 * time.Second)
	ev := s.events(t)
	var types []string
	for _, e := range ev {
		types = append(types, e.Type)
	}
	if got := strings.Join(types, ","); got != "run.started,run.failed" {
		t.Fatalf("events = %s, want run.started,run.failed", got)
	}
	var f struct {
		Reason string          `json:"reason"`
		Result json.RawMessage `json:"result"`
	}
	_ = json.Unmarshal(ev[1].Data, &f)
	if f.Reason != "interrupted" || f.Result != nil {
		t.Errorf("run.failed data = %s", ev[1].Data)
	}
}

// A sink that cannot keep up loses points and keeps the lifecycle events, and
// the engine's callers are never held up by it.
func TestSlowSinkDropsPointsFirstAndNeverLifecycle(t *testing.T) {
	s := &memSink{Block: make(chan struct{})}
	r := NewRecorder(RecorderOptions{RunID: "slow", QueueDepth: 3}, s)
	r.Started(time.Now(), startedAt())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			r.Point(Point{OffsetS: float64(i), Requests: 1})
		}
		r.Finished(map[string]int{"total": 200}, true, 0)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emitting blocked on a sink that is not reading")
	}
	close(s.Block)
	if stuck := r.Close(3 * time.Second); len(stuck) != 0 {
		t.Fatalf("stuck sinks: %v", stuck)
	}
	var points, started, finished int
	var last int64
	for _, e := range s.events(t) {
		if e.Seq <= last {
			t.Errorf("seq went %d -> %d", last, e.Seq)
		}
		last = e.Seq
		switch e.Type {
		case TypePoint:
			points++
		case TypeRunStarted:
			started++
		case TypeRunFinished:
			finished++
		}
	}
	if started != 1 || finished != 1 {
		t.Errorf("lifecycle events lost: started=%d finished=%d", started, finished)
	}
	if points >= 200 || points == 0 {
		t.Errorf("points = %d, want some but not all of 200", points)
	}
	warned := false
	for _, w := range r.Warnings() {
		warned = warned || strings.Contains(w, "dropped")
	}
	if !warned {
		t.Errorf("no warning about dropped events: %v", r.Warnings())
	}
}

func TestQueueClassesAndOrder(t *testing.T) {
	q := newQueue(2)
	for i := 0; i < 5; i++ {
		q.push(classPoint, []byte{byte('a' + i)})
	}
	q.push(classLifecycle, []byte("L"))
	if n := q.takeDropped(); n != 3 {
		t.Errorf("dropped = %d, want 3", n)
	}
	q.close()
	var got string
	for {
		it, state := q.pop(-1)
		if state != popItem {
			break
		}
		got += string(it.line)
	}
	if got != "abL" {
		t.Errorf("order = %q, want abL", got)
	}
}

func TestSpliceVerdictKeepsTheResultAsItIs(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"total":7,"error_rate":0.10,"x":{"a":[1,2]}}`, `{"total":7,"error_rate":0.10,"x":{"a":[1,2]},"passed":true,"exitCode":0}`},
		{`{}`, `{"passed":true,"exitCode":0}`},
		{` { "a": 1 } `, `{"a": 1,"passed":true,"exitCode":0}`},
		{`null`, `{"passed":true,"exitCode":0}`},
		{``, `{"passed":true,"exitCode":0}`},
	}
	for _, c := range cases {
		got, err := spliceVerdict(json.RawMessage(c.in), true, 0)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("splice(%q) = %s, want %s", c.in, got, c.want)
		}
		if !json.Valid(got) {
			t.Errorf("splice(%q) is not valid JSON: %s", c.in, got)
		}
	}
	got, _ := spliceVerdict(json.RawMessage(`{"a":1}`), false, 3)
	if !bytes.Contains(got, []byte(`"passed":false,"exitCode":3`)) {
		t.Errorf("verdict missing: %s", got)
	}
}

func TestValidRunID(t *testing.T) {
	for _, ok := range []string{"a", "nightly-42", "A_b.c", strings.Repeat("x", 64)} {
		if err := ValidRunID(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a b", "../x", "a/b", "é", strings.Repeat("x", 129), "a\nb"} {
		if err := ValidRunID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if id := NewRunID(); len(id) != 12 || ValidRunID(id) != nil {
		t.Errorf("NewRunID = %q", id)
	}
}
