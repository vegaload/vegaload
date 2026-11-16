package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vegaload/vegaload/internal/report"
)

func endpointServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/pay") {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCmdRun_ScenarioEndpoints(t *testing.T) {
	srv := endpointServer(t)
	dir := t.TempDir()
	scenario := filepath.Join(dir, "endpoints.vl.js")
	src := `let n = 0;
export default function () {
  for (let i = 0; i < 20; i++) { n++; http.get("` + srv.URL + `/users/" + n); }
  group("checkout", () => {
    http.get("` + srv.URL + `/cart/" + n);
    http.post("` + srv.URL + `/pay/" + n, { name: "/pay" });
  });
}`
	if err := os.WriteFile(scenario, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "r.json")
	code, stdout, stderr := captureCmd(t, func() int {
		return cmdRun([]string{
			"-vus", "1", "-duration", "300ms", "-no-report", "-no-live",
			"-audit-log", filepath.Join(dir, "audit.log"), "-out", out,
			"-threshold", `p95{protocol="http",op="GET /users/{id}"} < 10s`,
			"-threshold", `error_rate{protocol="http",op="POST /pay"} < 1%`,
			scenario,
		})
	})
	if code != 3 {
		t.Fatalf("exit %d, want 3 (POST /pay always fails)\n%s\n%s", code, stdout, stderr)
	}
	res := readResult(t, out)
	p, _ := res.Protocol("http")
	if len(p.Operations) != 3 {
		t.Fatalf("rows = %+v", p.Operations)
	}
	users, _ := p.Operation("GET /users/{id}")
	cart, _ := p.Operation("GET checkout")
	pay, _ := p.Operation("POST /pay")
	// The run can end inside an iteration, so the counts are checked to
	// within one iteration.
	if cart.Total == 0 || users.Total < 20*(cart.Total) || users.Total > 20*(cart.Total+1) || pay.Total < cart.Total-1 || pay.Total > cart.Total || pay.Failed != pay.Total {
		t.Fatalf("users %+v cart %+v pay %+v", users, cart, pay)
	}
	if users.Total+cart.Total+pay.Total != res.Total || users.Method != "GET" || users.Endpoint != "/users/{id}" {
		t.Fatalf("rows do not add up to the run: %d %+v", res.Total, users)
	}
	if co, ok := res.Step("checkout"); !ok || co.Total < pay.Total-1 || co.Total > pay.Total {
		t.Errorf("group step = %+v %v", co, ok)
	}
	if !res.Thresholds[0].Passed || res.Thresholds[1].Passed || res.Thresholds[0].Op != "GET /users/{id}" {
		t.Errorf("thresholds = %+v", res.Thresholds)
	}
	for _, want := range []string{"\nendpoints\n", "PASS  GET /users/{id}  ", "PASS  GET checkout  ", "FAIL  POST /pay  "} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text summary lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "\nprotocols\n") {
		t.Errorf("a plain HTTP scenario shows the protocols block:\n%s", stdout)
	}
}

func TestCmdRun_ProtocolHTTPEndpointRow(t *testing.T) {
	srv := endpointServer(t)
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		args []string
		row  string
	}{
		"folded":      {nil, "POST /users/{id}/orders"},
		"no grouping": {[]string{"-no-url-grouping"}, "POST /users/42/orders"},
	} {
		out := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
		args := append([]string{
			"-target", srv.URL + "/users/42/orders?x=1", "-protocol", "http1", "-method", "post",
			"-vus", "1", "-duration", "200ms", "-no-report", "-no-live",
			"-audit-log", filepath.Join(dir, "audit.log"), "-out", out,
			"-threshold", `p95{protocol="http1"} < 10s`,
			"-threshold", `p95{protocol="http1",op="` + tc.row + `"} < 10s`,
		}, tc.args...)
		code, stdout, stderr := captureCmd(t, func() int { return cmdRun(args) })
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s\n%s", name, code, stdout, stderr)
		}
		res := readResult(t, out)
		p, _ := res.Protocol("http1")
		if len(p.Operations) != 1 || p.Operations[0].Name != tc.row || p.Operations[0].Total != res.Total || p.Operations[0].Method != "POST" {
			t.Fatalf("%s: rows = %+v", name, p.Operations)
		}
		if !strings.Contains(stdout, "\nendpoints\n  PASS  "+tc.row+"  ") {
			t.Errorf("%s: text summary:\n%s", name, stdout)
		}
	}
}

func TestParseRunArgs_NoURLGroupingIsForHTTP(t *testing.T) {
	if _, err := parseRunArgs([]string{"-no-url-grouping", "-target", "127.0.0.1:1", "-protocol", "tcp"}); err == nil || !strings.Contains(err.Error(), "-no-url-grouping is for HTTP") {
		t.Fatalf("tcp: %v", err)
	}
	cfg, err := parseRunArgs([]string{"-no-url-grouping", "-target", "http://127.0.0.1:1", "-protocol", "http2"})
	if err != nil || !cfg.NoURLGrouping {
		t.Fatalf("http2: %v %+v", err, cfg)
	}
}

func TestPrintEndpoints_OnlyHTTPRowsOnce(t *testing.T) {
	var buf bytes.Buffer
	printResult(&buf, &report.Result{
		Executor: "fixed-vus",
		Protocols: []report.ProtocolResult{
			{Name: "http", Total: 3, Operations: []report.OperationResult{{Name: "GET /a/{id}", Total: 2}, {Name: report.OtherEndpoints, Total: 1}}},
			{Name: "ftp", Total: 1, Operations: []report.OperationResult{{Name: "download", Total: 1}}},
		},
	})
	text := buf.String()
	if strings.Count(text, "GET /a/{id}") != 1 || !strings.Contains(text, "PASS  Other  1 calls") || !strings.Contains(text, "\nprotocols\n") || !strings.Contains(text, "download") {
		t.Fatalf("summary:\n%s", text)
	}
}
