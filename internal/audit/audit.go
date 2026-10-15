// Package audit implements FR-CLI-07: an audit record of every run —
// trigger source, target, parameters, and timestamp — appended to a
// local, append-only JSONL file. It never leaves the local machine (see
// NFR-05); nothing in this package makes a network call.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Entry is one audit record. Trigger identifies what started the run:
// "cli" for a human running vegaload directly, or "mcp:<session-id>"
// when the MCP server's run_test tool drove it (see internal/mcp,
// Phase 2) — either way it is the caller's job to set Trigger correctly,
// this package just records whatever it is given.
type Entry struct {
	Time         time.Time     `json:"time"`
	Trigger      string        `json:"trigger"`
	Target       string        `json:"target,omitempty"`
	Protocol     string        `json:"protocol,omitempty"`
	ScenarioPath string        `json:"scenario_path,omitempty"`
	Executor     string        `json:"executor"`
	VUs          int           `json:"vus"`
	Duration     time.Duration `json:"duration_ns"`
	Rate         float64       `json:"rate,omitempty"`
	Outcome      string        `json:"outcome"` // "success" or "error"
	Error        string        `json:"error,omitempty"`
	Total        int64         `json:"total,omitempty"`
	Failed       int64         `json:"failed,omitempty"`

	// Thresholds are the pass/fail thresholds the run was given
	// (FR-CLI-11), as text, and whether they all passed.
	Thresholds       []string `json:"thresholds,omitempty"`
	ThresholdsPassed *bool    `json:"thresholds_passed,omitempty"`

	// Baseline is the report a run was compared with (FR-CLI-14), and
	// whether the run stayed within the allowed regression.
	Baseline       string `json:"baseline,omitempty"`
	BaselinePassed *bool  `json:"baseline_passed,omitempty"`

	// DataFiles is the name and row count of each data file a protocol
	// run read. A cell is never recorded.
	DataFiles []DataFile `json:"data_files,omitempty"`

	// Stream names where a streamed run was sent, as kinds and hosts:
	// "file", "push:HOST" and "live:HOST", comma-separated. It never holds a
	// path, a URL path or query, a project id or a token.
	Stream string `json:"stream,omitempty"`
}

// DataFile is one data file. Name is the file's base name.
type DataFile struct {
	Name string `json:"name"`
	Rows int    `json:"rows"`
}

// DefaultPath is where a run's audit record is appended when the caller
// does not override it (cmd/vegaload's -audit-log flag). It lives under
// the user's home directory so it accumulates across projects/repos,
// the way a shell history file does.
func DefaultPath() string {
	if dir, err := os.UserHomeDir(); err == nil {
		return filepath.Join(dir, ".vegaload", "audit.log")
	}
	return filepath.Join(".", ".vegaload-audit.log")
}

// Append writes e as one JSON line to path, creating the file and its
// parent directory if needed. A failure here is deliberately non-fatal
// to the caller's run — see cmd/vegaload's run.go, which logs a warning
// rather than failing the run itself when Append errors, the same way
// memory/telemetry failures are treated as best-effort elsewhere.
func Append(path string, e Entry) error {
	if path == "" {
		path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("audit: creating %s: %w", filepath.Dir(path), err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("audit: opening %s: %w", path, err)
	}
	defer f.Close()

	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("audit: encoding entry: %w", err)
	}
	data = append(data, '\n')
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("audit: writing %s: %w", path, err)
	}
	return nil
}
