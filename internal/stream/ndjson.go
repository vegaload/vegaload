package stream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Recording is one run's events as read from an NDJSON file.
type Recording struct {
	Path   string
	Events []Envelope
	// Raw holds each event's original line, so that serving a recording
	// forwards fields this version does not know.
	Raw [][]byte
}

// RunID returns the run id shared by every event in the recording.
func (r *Recording) RunID() string {
	if len(r.Events) == 0 {
		return ""
	}
	return r.Events[0].RunID
}

// Terminal returns the last event when it ends a run.
func (r *Recording) Terminal() (Envelope, bool) {
	if len(r.Events) == 0 {
		return Envelope{}, false
	}
	last := r.Events[len(r.Events)-1]
	return last, last.Terminal()
}

// maxLine bounds one NDJSON line. run.finished carries the whole result, so
// it can be large.
const maxLine = 64 << 20

// Read parses an NDJSON event stream and checks its structure: every line is
// a JSON object with the v1 schema, one run id, a type, data, and a seq that
// increases by exactly one starting at 1. A recording with no events, or with
// events after a terminal event, is rejected. Unknown fields are kept.
func Read(r io.Reader) (*Recording, error) {
	rec := &Recording{}
	br := bufio.NewReaderSize(r, 1<<20)
	for lineNo := 1; ; lineNo++ {
		line, err := readLine(br)
		if len(bytes.TrimSpace(line)) > 0 {
			var ev Envelope
			if jerr := json.Unmarshal(line, &ev); jerr != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, jerr)
			}
			if verr := checkEvent(ev, rec); verr != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, verr)
			}
			rec.Events = append(rec.Events, ev)
			rec.Raw = append(rec.Raw, bytes.TrimSpace(line))
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if len(rec.Events) == 0 {
		return nil, fmt.Errorf("no events")
	}
	return rec, nil
}

func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if err != nil {
			return buf, err
		}
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			return nil, fmt.Errorf("line longer than %d bytes", maxLine)
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

func checkEvent(ev Envelope, rec *Recording) error {
	if ev.Schema != Schema {
		return fmt.Errorf("schema %q, want %q", ev.Schema, Schema)
	}
	if ev.RunID == "" {
		return fmt.Errorf("missing runId")
	}
	if ev.Type == "" {
		return fmt.Errorf("missing type")
	}
	if len(ev.Data) == 0 || !json.Valid(ev.Data) {
		return fmt.Errorf("missing or invalid data")
	}
	if ev.TS.IsZero() {
		return fmt.Errorf("missing ts")
	}
	n := int64(len(rec.Events))
	if ev.Seq != n+1 {
		return fmt.Errorf("seq %d, want %d", ev.Seq, n+1)
	}
	if n > 0 {
		if ev.RunID != rec.Events[0].RunID {
			return fmt.Errorf("runId %q differs from %q", ev.RunID, rec.Events[0].RunID)
		}
		if rec.Events[n-1].Terminal() {
			return fmt.Errorf("event after %s", rec.Events[n-1].Type)
		}
	} else if ev.Type != TypeRunStarted {
		return fmt.Errorf("first event is %s, want %s", ev.Type, TypeRunStarted)
	}
	return nil
}

// ReadFile reads and checks a recording from a file.
func ReadFile(path string) (*Recording, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rec, err := Read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	rec.Path = path
	return rec, nil
}

// Write writes events as NDJSON, one per line.
func Write(w io.Writer, events []Envelope) error {
	bw := bufio.NewWriter(w)
	for _, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := bw.Write(b); err != nil {
			return err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}
	return bw.Flush()
}
