package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vegaload/vegaload/internal/report"
)

func runResultJSON(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(report.Result{Executor: "fixed-vus", Total: 10})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// call sends one tools/call through the server, as a client would, and
// returns the text items of the answer.
func callOverTheWire(t *testing.T, bin, tool string, args map[string]any) (items []string, isError bool) {
	t.Helper()
	srv := NewServer()
	for _, tl := range NewTools(bin) {
		srv.Register(tl)
	}
	params, _ := json.Marshal(map[string]any{"name": tool, "arguments": args})
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": json.RawMessage(params)})
	resp := srv.handleLine(context.Background(), line)
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Result.Content {
		if c.Type != "text" {
			t.Fatalf("content type %q", c.Type)
		}
		items = append(items, c.Text)
	}
	return items, v.Result.IsError
}

func TestRunTestForwardsTheLiveViewAndStreamArguments(t *testing.T) {
	bin, argsFile := argRecordingBinary(t, runResultJSON(t), 0)
	_, err := callTool(t, NewTools(bin), "run_test", map[string]any{
		"scenario_path": "s.vl.js", "no_report": true,
		"live": true, "no-live": true, "no-open": true,
		"stream_file": "auto", "stream_push": "http://127.0.0.1:7080/ingest,https://b.example.com/ingest",
		"run_id": "r-1", "labels": []string{"service=checkout", "env=ci"},
	})
	if err != nil {
		t.Fatal(err)
	}
	args := readArgs(t, argsFile)
	got := strings.Join(args, " ")
	for _, want := range []string{
		"-live", "-no-live", "-no-open", "-stream-file auto",
		"-stream-push http://127.0.0.1:7080/ingest,https://b.example.com/ingest",
		"-run-id r-1", "-label service=checkout", "-label env=ci",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if args[len(args)-1] != "s.vl.js" {
		t.Errorf("the scenario must stay last: %v", args)
	}
	// There is no argument that carries a token or relaxes https.
	for _, banned := range []string{"token", "insecure"} {
		if strings.Contains(strings.ToLower(got), banned) {
			t.Errorf("%s reachable from MCP: %s", banned, got)
		}
	}
}

func TestRunTestAddsNoLiveFlagsWithoutArguments(t *testing.T) {
	bin, argsFile := argRecordingBinary(t, runResultJSON(t), 0)
	if _, err := callTool(t, NewTools(bin), "run_test", map[string]any{"scenario_path": "s.vl.js", "no_report": true}); err != nil {
		t.Fatal(err)
	}
	for _, a := range readArgs(t, argsFile) {
		switch a {
		case "-live", "-no-live", "-no-open", "-stream-file", "-stream-push", "-run-id", "-label":
			t.Errorf("%s without an argument", a)
		}
	}
}

func TestTheSchemaDocumentsTheNewArgumentsAndNoSecretOnes(t *testing.T) {
	for _, tl := range NewTools("x") {
		if tl.Name != "run_test" {
			continue
		}
		props := tl.InputSchema["properties"].(map[string]any)
		for _, k := range []string{"live", "no-live", "no-open", "stream_file", "stream_push", "run_id", "labels"} {
			if _, ok := props[k]; !ok {
				t.Errorf("run_test has no %q argument", k)
			}
		}
		for k := range props {
			if strings.Contains(k, "token") || strings.Contains(k, "insecure") {
				t.Errorf("run_test has an argument %q", k)
			}
		}
		if !strings.Contains(tl.Description, "live view: URL") {
			t.Error("the description does not tell the agent about the live view line")
		}
	}
}

// What the CLI says on stderr (the live view's address) is a second text item.
// The first is the run's result and stays valid JSON, whatever is on stderr.
func TestStdoutStaysJSONAndStderrNotesComeSeparately(t *testing.T) {
	bin := fakeBinary(t, runResultJSON(t), "live view: http://127.0.0.1:7080/p/p-abc   (not wanted? -no-live)\n", 0)
	items, isErr := callOverTheWire(t, bin, "run_test", map[string]any{"scenario_path": "s.vl.js", "no_report": true})
	if isErr || len(items) != 2 {
		t.Fatalf("isError %v, %d items: %v", isErr, len(items), items)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(items[0]), &first); err != nil || first["result"] == nil {
		t.Fatalf("item 1 is not the result JSON: %v\n%s", err, items[0])
	}
	if strings.Contains(items[0], "live view") {
		t.Fatalf("a note leaked into the JSON: %s", items[0])
	}
	if !strings.HasPrefix(items[1], "live view: http://127.0.0.1:7080/p/p-abc") {
		t.Fatalf("item 2: %q", items[1])
	}
}

func TestNoNotesMeansOneItem(t *testing.T) {
	bin := fakeBinary(t, runResultJSON(t), "", 0)
	items, _ := callOverTheWire(t, bin, "run_test", map[string]any{"scenario_path": "s.vl.js", "no_report": true})
	if len(items) != 1 {
		t.Fatalf("%d items: %v", len(items), items)
	}
	// Tools that do not use notes keep one item even when the CLI is chatty.
	bin = fakeBinary(t, `{"schema_version":1,"protocols":[]}`, "noise on stderr", 0)
	items, _ = callOverTheWire(t, bin, "list_protocols", map[string]any{})
	if len(items) != 1 {
		t.Fatalf("%d items: %v", len(items), items)
	}
}

func TestABreachedRunKeepsItsResultAndItsNotes(t *testing.T) {
	bin := fakeBinary(t, runResultJSON(t), "live view: http://127.0.0.1:7080/p/p-abc\nvegaload run: thresholds breached: p95 (observed 2s)\n", 3)
	items, isErr := callOverTheWire(t, bin, "run_test", map[string]any{"scenario_path": "s.vl.js", "no_report": true})
	if isErr || len(items) != 2 {
		t.Fatalf("isError %v: %v", isErr, items)
	}
	if !strings.Contains(items[1], "live view:") || !strings.Contains(items[1], "thresholds breached") {
		t.Fatalf("notes %q", items[1])
	}
}

func TestAFailedRunReportsBothStreams(t *testing.T) {
	bin := fakeBinary(t, "partial output", "bad scenario", 2)
	items, isErr := callOverTheWire(t, bin, "run_test", map[string]any{"scenario_path": "s.vl.js", "no_report": true})
	if !isErr || len(items) != 1 {
		t.Fatalf("isError %v: %v", isErr, items)
	}
	if !strings.Contains(items[0], "bad scenario") || !strings.Contains(items[0], "partial output") {
		t.Fatalf("%q", items[0])
	}
}

func TestWithNotesEncodesAsItsValue(t *testing.T) {
	b, err := json.Marshal(WithNotes{Value: map[string]any{"a": 1}, Notes: "n"})
	if err != nil || string(b) != `{"a":1}` {
		t.Fatalf("%s %v", b, err)
	}
}
