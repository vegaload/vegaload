package stream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileSinkWritesAReadableRecording(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "dir", "run.ndjson")
	fs, err := NewFileSink(path)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRecorder(RecorderOptions{RunID: "f1"}, fs)
	r.Started(time.Now(), startedAt())
	r.Point(Point{Requests: 2, Total: 2})
	r.Finished(map[string]int{"total": 2}, true, 0)
	r.Close(2 * time.Second)

	rec, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if end, ok := rec.Terminal(); !ok || end.Type != TypeRunFinished || len(rec.Events) != 3 {
		t.Errorf("recording: %d events, terminal=%v", len(rec.Events), ok)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm()&0o022 != 0 {
		t.Errorf("mode %v is writable by others", fi.Mode())
	}
}

func TestFileSinkNeedsAWritableDestination(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewFileSink(dir); err == nil {
		t.Error("a directory is not a file destination")
	}
}

// The example streams in docs/stream/fixtures are real recordings (record.sh).
// They have to stay valid by the contract.
func TestFixturesFollowTheContract(t *testing.T) {
	want := map[string]string{
		"checkout-load.ndjson":         TypeRunFinished,
		"checkout-load-failed.ndjson":  TypeRunFinished,
		"checkout-load-aborted.ndjson": TypeRunFinished,
		"run-failed.ndjson":            TypeRunFailed,
	}
	for name, terminal := range want {
		rec, err := ReadFile(filepath.Join("..", "..", "docs", "stream", "fixtures", name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		end, ok := rec.Terminal()
		if !ok || end.Type != terminal {
			t.Errorf("%s: ends with %q, want %q", name, end.Type, terminal)
		}
		var lastTotal float64 = -1
		var offset float64 = -1
		for _, e := range rec.Events {
			if e.Type != TypePoint {
				continue
			}
			p := mustPoint(t, e)
			if p.OffsetS != offset+1 {
				t.Errorf("%s: point offset %v after %v", name, p.OffsetS, offset)
			}
			offset = p.OffsetS
			lastTotal = float64(p.Total)
		}
		if terminal == TypeRunFinished {
			if got := resultTotal(t, end); got != lastTotal {
				t.Errorf("%s: last point total %v != run.finished total %v", name, lastTotal, got)
			}
		}
	}
}

func mustPoint(t *testing.T, e Envelope) Point {
	t.Helper()
	var p Point
	if err := json.Unmarshal(e.Data, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func resultTotal(t *testing.T, e Envelope) float64 {
	t.Helper()
	var r struct {
		Total float64 `json:"total"`
	}
	if err := json.Unmarshal(e.Data, &r); err != nil {
		t.Fatal(err)
	}
	return r.Total
}
