package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/engine"
	"github.com/vegaload/vegaload/internal/secrets"
	"github.com/vegaload/vegaload/internal/stream"
)

// clearStreamEnv removes every streaming variable but the live view's, which
// TestMain keeps off.
func clearStreamEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"VEGALOAD_STREAM_FILE", "VEGALOAD_STREAM_PUSH", "VEGALOAD_STREAM_TOKEN", "VEGALOAD_RUN_ID", "VEGALOAD_LIVE_URL"} {
		t.Setenv(k, "")
	}
}

type streamed struct {
	code           int
	stdout, stderr string
	audit          string
	outPath        string
	out            map[string]any // the -out JSON
	outRaw         []byte
}

// runAgainst runs a short protocol-direct test against target and returns what
// it printed and wrote. extra flags come last.
func runAgainst(t *testing.T, target string, extra ...string) streamed {
	t.Helper()
	dir := t.TempDir()
	s := streamed{outPath: filepath.Join(dir, "out.json")}
	auditPath := filepath.Join(dir, "audit.log")
	args := []string{"-target", target, "-protocol", "http1", "-vus", "2", "-duration", "2300ms",
		"-no-report", "-audit-log", auditPath, "-out", s.outPath}
	args = append(args, extra...)
	s.code, s.stdout, s.stderr = captureCmd(t, func() int { return cmdRun(args) })
	if b, err := os.ReadFile(s.outPath); err == nil {
		s.outRaw = b
		_ = json.Unmarshal(b, &s.out)
	}
	if b, err := os.ReadFile(auditPath); err == nil {
		s.audit = string(b)
	}
	return s
}

func quietServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	return srv
}

func dataOf(t *testing.T, e stream.Envelope) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Data, &m); err != nil {
		t.Fatalf("%s data: %v", e.Type, err)
	}
	return m
}

func TestStreamFileIsAValidStreamThatMatchesTheResult(t *testing.T) {
	clearStreamEnv(t)
	srv := quietServer(t)
	file := filepath.Join(t.TempDir(), "run.ndjson")
	r := runAgainst(t, srv.URL, "-stream-file", file, "-run-id", "run-abc", "-label", "service=checkout", "-label", "env=ci")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}

	// Read checks the structure: one schema, one run id, seq 1..n without gaps,
	// run.started first, one terminal event last.
	rec, err := stream.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	first, last := rec.Events[0], rec.Events[len(rec.Events)-1]
	if first.Type != stream.TypeRunStarted || last.Type != stream.TypeRunFinished {
		t.Fatalf("first %s, last %s", first.Type, last.Type)
	}
	for _, e := range rec.Events {
		if e.RunID != "run-abc" || e.Labels["service"] != "checkout" || e.Labels["env"] != "ci" || e.Schema != stream.Schema {
			t.Fatalf("envelope %+v", e)
		}
	}

	started := dataOf(t, first)
	if started["executor"] != "fixed-vus" || started["vus"] != float64(2) || started["duration"] != "2.3s" || started["version"] != version {
		t.Fatalf("run.started %v", started)
	}
	if started["name"] != "http1 "+srv.URL {
		t.Fatalf("name %v", started["name"])
	}
	if tg := started["target"].(map[string]any); tg["protocol"] != "http1" || tg["baseUrl"] != srv.URL {
		t.Fatalf("target %v", tg)
	}

	// run.finished is the -out JSON plus passed and exitCode.
	fin := dataOf(t, last)
	if fin["passed"] != true || fin["exitCode"] != float64(0) {
		t.Fatalf("verdict %v %v", fin["passed"], fin["exitCode"])
	}
	delete(fin, "passed")
	delete(fin, "exitCode")
	if !reflect.DeepEqual(fin, r.out) {
		t.Fatalf("run.finished differs from -out:\n%v\n%v", fin, r.out)
	}

	// One point per second, in order, equal to the final time series.
	var points []stream.Envelope
	for _, e := range rec.Events[1 : len(rec.Events)-1] {
		if e.Type != stream.TypePoint {
			t.Fatalf("unexpected %s in the middle of the stream", e.Type)
		}
		points = append(points, e)
	}
	series := r.out["time_series"].([]any)
	if len(points) < len(series) || len(series) < 2 {
		t.Fatalf("%d points for %d series entries", len(points), len(series))
	}
	var total, failed float64
	for i, e := range points {
		p := dataOf(t, e)
		if p["offsetS"] != float64(i) {
			t.Fatalf("point %d offsetS %v: a second was missed or repeated", i, p["offsetS"])
		}
		total += p["requests"].(float64)
		failed += p["failed"].(float64)
		if p["total"] != total || p["totalFailed"] != failed {
			t.Fatalf("point %d totals %v/%v, want %v/%v", i, p["total"], p["totalFailed"], total, failed)
		}
		if i >= len(series) {
			if p["requests"] != float64(0) {
				t.Fatalf("point %d, past the series, has calls: %v", i, p)
			}
			continue
		}
		s := series[i].(map[string]any)
		if p["rps"] != s["rps"] || p["errorRate"] != s["error_rate"] || p["requests"] != s["requests"] || p["failed"] != s["failed"] {
			t.Fatalf("point %d %v differs from the series %v", i, p, s)
		}
	}
	if total != r.out["total"] || failed != r.out["failed"] {
		t.Fatalf("points add up to %v/%v, the result has %v/%v", total, failed, r.out["total"], r.out["failed"])
	}
	if !strings.Contains(r.audit, `"stream":"file"`) {
		t.Fatalf("audit does not name the destination kind: %s", r.audit)
	}
	if strings.Contains(r.audit, "run.ndjson") || strings.Contains(r.audit, "run-abc") {
		t.Fatalf("audit holds a path or id: %s", r.audit)
	}
}

func TestStreamFileAutoWritesUnderTheRunsDirectory(t *testing.T) {
	clearStreamEnv(t)
	srv := quietServer(t)
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	dir := t.TempDir()
	_ = os.Chdir(dir)
	if r := runAgainst(t, srv.URL, "-stream-file", "auto", "-run-id", "r1", "-duration", "300ms"); r.code != 0 {
		t.Fatalf("exit %d", r.code)
	}
	if _, err := stream.ReadFile(filepath.Join(dir, ".vegaload", "runs", "r1.ndjson")); err != nil {
		t.Fatal(err)
	}
}

func TestFlagsBeatEnvironmentAndRunIDIsRandomByDefault(t *testing.T) {
	clearStreamEnv(t)
	srv := quietServer(t)
	dir := t.TempDir()
	envFile, flagFile := filepath.Join(dir, "env.ndjson"), filepath.Join(dir, "flag.ndjson")
	t.Setenv("VEGALOAD_STREAM_FILE", envFile)
	t.Setenv("VEGALOAD_RUN_ID", "from-env")

	// The variable alone is enough.
	if r := runAgainst(t, srv.URL, "-duration", "300ms"); r.code != 0 {
		t.Fatal(r.stderr)
	}
	rec, err := stream.ReadFile(envFile)
	if err != nil || rec.RunID() != "from-env" {
		t.Fatalf("%v %v", err, rec)
	}
	// A flag wins.
	if r := runAgainst(t, srv.URL, "-stream-file", flagFile, "-run-id", "from-flag", "-duration", "300ms"); r.code != 0 {
		t.Fatal(r.stderr)
	}
	rec, err = stream.ReadFile(flagFile)
	if err != nil || rec.RunID() != "from-flag" {
		t.Fatalf("%v", err)
	}
	// No id given: 12 random hex digits.
	t.Setenv("VEGALOAD_RUN_ID", "")
	if r := runAgainst(t, srv.URL, "-stream-file", flagFile, "-duration", "300ms"); r.code != 0 {
		t.Fatal(r.stderr)
	}
	rec, _ = stream.ReadFile(flagFile)
	if id := rec.RunID(); len(id) != 12 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Fatalf("run id %q", id)
	}
}

func TestStreamFlagsAreRefusedWhenTheyBreakARule(t *testing.T) {
	clearStreamEnv(t)
	srv := quietServer(t)
	cases := map[string][]string{
		"bad run id":              {"-stream-file", filepath.Join(t.TempDir(), "x"), "-run-id", "has space"},
		"remote http push":        {"-stream-push", "http://ingest.example.com/v1/x"},
		"credentials in the push": {"-stream-push", "https://user:pw@ingest.example.com/v1/x"},
		"live and no-live":        {"-live", "-no-live"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			r := runAgainst(t, srv.URL, extra...)
			if r.code != 2 {
				t.Fatalf("exit %d, want 2: %s", r.code, r.stderr)
			}
			if r.out != nil {
				t.Fatal("the run started although the destination was refused")
			}
			if strings.Contains(r.stderr, "pw") && !strings.Contains(r.stderr, "password") {
				t.Fatalf("the error repeats the credential: %s", r.stderr)
			}
		})
	}
	// -label without a key.
	if code, _, _ := captureCmd(t, func() int { return cmdRun([]string{"-target", srv.URL, "-protocol", "http1", "-label", "=x"}) }); code != 2 {
		t.Fatalf("bad label: exit %d", code)
	}
}

// receiver is a fake ingest endpoint.
type receiver struct {
	srv  *httptest.Server
	mu   sync.Mutex
	auth []string
	body bytes.Buffer
	hdr  http.Header
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.auth = append(r.auth, req.Header.Get("Authorization"))
		r.hdr = req.Header.Clone()
		r.body.Write(b)
		r.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

func TestStreamPushSendsEventsWithTheTokenAndKeepsItOutOfEverything(t *testing.T) {
	clearStreamEnv(t)
	const token = "push-token-SECRET-123"
	t.Setenv("VEGALOAD_STREAM_TOKEN", token)
	srv := quietServer(t)
	rcv := newReceiver(t)

	r := runAgainst(t, srv.URL, "-stream-push", rcv.srv.URL+"/v1/ingest?key=QUERYSECRET", "-run-id", "push-1")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	body := rcv.text()
	for _, want := range []string{`"type":"run.started"`, `"type":"run.finished"`, `"type":"point"`, `"runId":"push-1"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("no %s in what was pushed", want)
		}
	}
	for _, a := range rcv.auth {
		if a != "Bearer "+token {
			t.Fatalf("authorization %q", a)
		}
	}
	if ct := rcv.hdr.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content type %q", ct)
	}
	// The token and the destination's query are in no output of VegaLoad.
	for name, content := range map[string]string{"stdout": r.stdout, "stderr": r.stderr, "audit": r.audit, "-out": string(r.outRaw), "events": body} {
		if strings.Contains(content, token) {
			t.Fatalf("the token is in the %s", name)
		}
		if name != "events" && strings.Contains(content, "QUERYSECRET") {
			t.Fatalf("the push URL's query is in the %s", name)
		}
	}
	if !strings.Contains(r.audit, `"stream":"push:127.0.0.1:`) || strings.Contains(r.audit, "/v1/ingest") {
		t.Fatalf("audit %s", r.audit)
	}
}

// Nothing a user supplies as a credential is in any event: not the URL's
// password or query, a header, a body, nor an option.
func TestEventsNeverCarryCredentials(t *testing.T) {
	clearStreamEnv(t)
	srv := quietServer(t)
	file := filepath.Join(t.TempDir(), "run.ndjson")
	host := strings.TrimPrefix(srv.URL, "http://")
	r := runAgainst(t, "http://alice:PASSWORDSECRET@"+host+"/api?token=QUERYSECRET#FRAGSECRET",
		"-stream-file", file,
		"-header", "Authorization: Bearer HEADERSECRET", "-header", "X-Api-Key: KEYSECRET",
		"-body", "BODYSECRET",
		"-threshold", "p95 < 10s", "-duration", "300ms")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"PASSWORDSECRET", "alice", "QUERYSECRET", "FRAGSECRET", "HEADERSECRET", "KEYSECRET", "BODYSECRET", "Authorization", "X-Api-Key"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("%q is in the stream:\n%s", secret, b)
		}
	}
	rec, err := stream.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	started := dataOf(t, rec.Events[0])
	if tg := started["target"].(map[string]any); tg["baseUrl"] != "http://"+host+"/api" {
		t.Fatalf("target %v", tg)
	}
	th := started["thresholds"].([]any)[0].(map[string]any)
	if th["metric"] != "p95" || th["operator"] != "<" || th["value"] != "10s" {
		t.Fatalf("thresholds %v", started["thresholds"])
	}
}

// Options, environment values and data cells are never read into run.started:
// it is built from the plan of the run, field by field.
func TestRunStartedIsBuiltOnlyFromThePlan(t *testing.T) {
	cfg := &runConfig{
		Executor: "ramp", VUs: 3, Duration: 40 * time.Second, Protocol: "redis",
		Stages: []engine.Stage{{Target: 10, Duration: 30 * time.Second}, {Target: 0, Duration: 10 * time.Second}},
	}
	cfg.Target.URL = "redis://:hunter2@cache.internal:6379/0?x=y"
	cfg.Target.Options = map[string]string{"password": "OPTSECRET", "args": "SELECT SECRETCELL"}
	cfg.Target.Headers = map[string]string{"Authorization": "HDRSECRET"}
	cfg.Target.Body = []byte("BODYSECRET")
	rs := buildRunStarted(cfg, "ramp", 40*time.Second)
	b, _ := json.Marshal(rs)
	for _, secret := range []string{"hunter2", "OPTSECRET", "SECRETCELL", "HDRSECRET", "BODYSECRET", "x=y"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("%q in %s", secret, b)
		}
	}
	if rs.VUs != 10 || len(rs.Stages) != 2 || rs.Stages[0].Duration != "30s" || rs.Target.BaseURL != "redis://cache.internal:6379/0" {
		t.Fatalf("%+v", rs)
	}
}

func TestSanitizeTarget(t *testing.T) {
	for in, want := range map[string]string{
		"https://u:p@h.example:8443/a/b?q=1#f": "https://h.example:8443/a/b",
		"localhost:6379":                       "localhost:6379",
		"user:pw@host:3306":                    "host:3306",
		"host:9092?auth=x":                     "host:9092",
		"redis://:pw@h:6379":                   "redis://h:6379",
		"postgres://u:p@w@h/db?sslmode=x":      "postgres://h/db",
	} {
		if got := stream.SanitizeTarget(in); got != want {
			t.Errorf("SanitizeTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAbortedRunFinishesWithAbortedAndExit3(t *testing.T) {
	clearStreamEnv(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	t.Cleanup(bad.Close)
	file := filepath.Join(t.TempDir(), "run.ndjson")
	r := runAgainst(t, bad.URL, "-stream-file", file, "-threshold", "failed < 3", "-abort-on-breach")
	if r.code != exitThresholdsBreached {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	rec, err := stream.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	last := rec.Events[len(rec.Events)-1]
	fin := dataOf(t, last)
	if last.Type != stream.TypeRunFinished || fin["passed"] != false || fin["exitCode"] != float64(3) || fin["aborted"] == nil {
		t.Fatalf("%s %v", last.Type, fin)
	}
	if started := dataOf(t, rec.Events[0]); started["abortOnBreach"] != true {
		t.Fatalf("%v", started)
	}
}

func TestBreachedThresholdIsNotPassed(t *testing.T) {
	clearStreamEnv(t)
	srv := quietServer(t)
	file := filepath.Join(t.TempDir(), "run.ndjson")
	r := runAgainst(t, srv.URL, "-stream-file", file, "-threshold", "total < 1", "-duration", "300ms")
	if r.code != exitThresholdsBreached {
		t.Fatalf("exit %d", r.code)
	}
	rec, _ := stream.ReadFile(file)
	fin := dataOf(t, rec.Events[len(rec.Events)-1])
	if fin["passed"] != false || fin["exitCode"] != float64(3) || fin["thresholds_passed"] != false {
		t.Fatalf("%v", fin)
	}
}

func TestAScenarioThatCannotLoadIsARunFailed(t *testing.T) {
	clearStreamEnv(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "run.ndjson")
	bad := filepath.Join(dir, "broken.vl.js")
	if err := os.WriteFile(bad, []byte("this is not ( valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := captureCmd(t, func() int {
		return cmdRun([]string{"-stream-file", file, "-no-report", "-audit-log", filepath.Join(dir, "a.log"), bad})
	})
	if code != 1 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	rec, err := stream.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Events) != 2 || rec.Events[0].Type != stream.TypeRunStarted || rec.Events[1].Type != stream.TypeRunFailed {
		t.Fatalf("events %d, %s", len(rec.Events), rec.Events[len(rec.Events)-1].Type)
	}
	if started := dataOf(t, rec.Events[0]); started["name"] != "broken.vl.js" {
		t.Fatalf("%v", started)
	}
	if f := dataOf(t, rec.Events[1]); f["reason"] == "" || f["reason"] == nil {
		t.Fatalf("%v", f)
	}
}

// A run that fails before the executor can start still ends the stream, and
// starts it first.
func TestARunThatCannotStartStillGetsRunStartedThenRunFailed(t *testing.T) {
	clearStreamEnv(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "run.ndjson")
	code, _, _ := captureCmd(t, func() int {
		return cmdRun([]string{"-stream-file", file, "-no-report", "-audit-log", filepath.Join(dir, "a.log"),
			"-executor", "ramp", "-target", "http://127.0.0.1:1", "-protocol", "http1"})
	})
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	rec, err := stream.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if last := rec.Events[len(rec.Events)-1]; last.Type != stream.TypeRunFailed || rec.Events[0].Type != stream.TypeRunStarted {
		t.Fatalf("%+v", rec.Events)
	}
}

func TestInterruptEndsTheStreamWithRunFailedAndAShellExitCode(t *testing.T) {
	clearStreamEnv(t)
	file := filepath.Join(t.TempDir(), "run.ndjson")
	fs, err := stream.NewFileSink(file)
	if err != nil {
		t.Fatal(err)
	}
	rec := stream.NewRecorder(stream.RecorderOptions{RunID: "int-1"}, fs)
	st := &streaming{rec: rec}
	rec.Started(time.Now(), stream.RunStarted{Name: "x", Executor: "fixed-vus"})
	if code := st.interrupt(os.Interrupt); code != 130 {
		t.Fatalf("exit %d", code)
	}
	got, err := stream.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	last := got.Events[len(got.Events)-1]
	if last.Type != stream.TypeRunFailed || dataOf(t, last)["reason"] != "interrupted" {
		t.Fatalf("%s %s", last.Type, last.Data)
	}
}

// A scenario's secret environment value, its data cells and the headers it
// sends are in no event, though a check name that held the secret ends up in
// run.finished (redacted, as it is in -out).
func TestScenarioSecretsAndDataNeverReachTheStream(t *testing.T) {
	secrets.Reset()
	t.Cleanup(secrets.Reset)
	clearStreamEnv(t)
	t.Setenv("REGION", "south")
	t.Setenv("API_KEY", apiKey)
	rs := newRecordingServer(t)
	dir := t.TempDir()
	scenario := filepath.Join(dir, "checkout.vl.js")
	if err := os.WriteFile(scenario, []byte(inputsScenario(rs.URL)), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "run.ndjson")
	code, _, stderr := captureCmd(t, func() int {
		return cmdRun([]string{"-vus", "2", "-duration", "700ms", "-no-report", "-audit-log", filepath.Join(dir, "a.log"),
			"-stream-file", file, "-data", writeData(t), "-env", "REGION", "-secret-env", "API_KEY", scenario})
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{apiKey, "south", `"ann"`, `"bob"`, "X-Api-Key"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("%q is in the stream", secret)
		}
	}
	rec, err := stream.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	started := dataOf(t, rec.Events[0])
	if started["name"] != "checkout.vl.js" || started["target"] != nil {
		t.Fatalf("a scenario run has its file name and no target: %v", started)
	}
}
