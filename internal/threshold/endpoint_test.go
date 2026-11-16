package threshold

import (
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/report"
)

func TestParse_EndpointOp(t *testing.T) {
	for expr, op := range map[string]string{
		`p95{protocol="http",op="GET /users/{id}"} < 300ms`:           "GET /users/{id}",
		`p95{op='GET /users/{id}',protocol="http"} < 300ms`:           "GET /users/{id}",
		`users: p95{protocol="http",op="GET /a/{id}/b/{id}"} < 300ms`: "GET /a/{id}/b/{id}",
		`error_rate{protocol="http",op="POST checkout"} < 1%`:         "POST checkout",
		`p95{protocol="http",op="GET /users/${}"} < 1s`:               "GET /users/${}",
	} {
		th, err := Parse(expr)
		if err != nil {
			t.Errorf("%s: %v", expr, err)
			continue
		}
		if th.Protocol != "http" || th.Op != op {
			t.Errorf("%s: protocol %q op %q", expr, th.Protocol, th.Op)
		}
		again, err := Parse(th.Name)
		if th.Name == th.core() && (err != nil || again.Op != op) {
			t.Errorf("%s: its own name %q does not parse back: %v", expr, th.Name, err)
		}
	}
	for _, bad := range []string{
		`p95{protocol="http",op="GET /users/{id}} < 300ms`,
		`p95{protocol="http",op="GET /users/{id}"`,
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

func TestEvaluate_EndpointRow(t *testing.T) {
	res := &report.Result{
		Elapsed: time.Second,
		Protocols: []report.ProtocolResult{{
			Name: "http", Total: 3, Failed: 1,
			Latency: report.Latency{P95: 900 * time.Millisecond},
			Operations: []report.OperationResult{
				{Name: "GET /users/{id}", Total: 2, Latency: report.Latency{P95: 100 * time.Millisecond}},
				{Name: "POST checkout", Total: 1, Failed: 1, ErrorRate: 1, Latency: report.Latency{P95: 900 * time.Millisecond}},
			},
		}},
	}
	ts, err := ParseAll([]string{
		`p95{protocol="http",op="GET /users/{id}"} < 300ms`,
		`p95{protocol="http",op="POST checkout"} < 300ms`,
		`p95{protocol="http",op="GET /missing/{id}"} < 300ms`,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := Evaluate(ts, res)
	if !got[0].Passed || got[0].Op != "GET /users/{id}" || got[0].Observed != "100ms" {
		t.Errorf("users row = %+v", got[0])
	}
	if got[1].Passed {
		t.Errorf("checkout row passed: %+v", got[1])
	}
	if got[2].Passed {
		t.Errorf("a row with no calls passed: %+v", got[2])
	}
}
