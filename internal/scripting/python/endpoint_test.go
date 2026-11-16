package python

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/report"
)

// runEndpoints runs src once against a server that answers every path, and
// returns the http rows, keyed by name, and the steps it recorded.
func runEndpoints(t *testing.T, src string, opts ...Option) (map[string]report.OperationResult, []string, error) {
	t.Helper()
	skipIfNoPython(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	script, err := Load(writeScript(t, strings.ReplaceAll(src, "BASE", srv.URL)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	vu, err := script.NewVU(nil, 5*time.Second, opts...)
	if err != nil {
		t.Fatalf("NewVU: %v", err)
	}
	defer vu.Close()
	col := report.NewCollector()
	steps := &fakeSteps{}
	vu.SetCallRecorder(col)
	vu.SetStepRecorder(steps)
	iterErr := vu.Iteration(context.Background())
	rows := map[string]report.OperationResult{}
	if p, ok := col.Finish("fixed-vus", time.Second).Protocol("http"); ok {
		for _, o := range p.Operations {
			rows[o.Name] = o
		}
	}
	return rows, steps.got, iterErr
}

func rowNames(rows map[string]report.OperationResult) string {
	var names []string
	for n, o := range rows {
		names = append(names, n+"="+strconv.FormatInt(o.Total, 10))
	}
	return strings.Join(names, ", ")
}

func wantRows(t *testing.T, rows map[string]report.OperationResult, want map[string]int64) {
	t.Helper()
	if len(rows) != len(want) {
		t.Fatalf("rows = %s", rowNames(rows))
	}
	for n, c := range want {
		if rows[n].Total != c {
			t.Errorf("row %q = %d calls, want %d (rows %s)", n, rows[n].Total, c, rowNames(rows))
		}
	}
}

func TestEndpoints_IDsShareOneRow(t *testing.T) {
	rows, _, err := runEndpoints(t, `
def iteration():
    for i in range(1, 51):
        http.get("BASE/users/%d?v=%d" % (i, i))
    http.get("BASE/")
`)
	if err != nil {
		t.Fatal(err)
	}
	wantRows(t, rows, map[string]int64{"GET /users/{id}": 50, "GET /": 1})
}

func TestEndpoints_NoURLGrouping(t *testing.T) {
	rows, _, err := runEndpoints(t, `
def iteration():
    http.get("BASE/users/1")
    http.get("BASE/users/2")
`, WithNoURLGrouping())
	if err != nil {
		t.Fatal(err)
	}
	wantRows(t, rows, map[string]int64{"GET /users/1": 1, "GET /users/2": 1})
}

func TestEndpoints_NameOptionAndURLTemplate(t *testing.T) {
	rows, _, err := runEndpoints(t, `
def iteration():
    for id in ["a", "b", "c"]:
        http.get("BASE/items/" + id, name="/items/{name}")
        u = http.url("BASE/things/{}/parts?full={}", id, id)
        assert str(u) == "BASE/things/%s/parts?full=%s" % (id, id), str(u)
        assert u.url == str(u)
        http.post(u, body="x")
        http.request("PUT", u, name="explicit wins")
    assert str(http.url("BASE/n/{:03d}/{{literal}}", 7)) == "BASE/n/007/{literal}"
`)
	if err != nil {
		t.Fatal(err)
	}
	wantRows(t, rows, map[string]int64{"GET /items/{name}": 3, "POST /things/${}/parts": 3, "PUT explicit wins": 3})
}

func TestEndpoints_GroupNamesCallsAndIsAStep(t *testing.T) {
	rows, steps, err := runEndpoints(t, `
def checkout():
    http.get("BASE/cart/1")
    with group("pay"):
        http.post("BASE/pay/1")
    http.post("BASE/order/1", name="/order")
    http.get(http.url("BASE/cart/{}", 7))
    return 42

def iteration():
    v = group("checkout", checkout)
    assert v == 42, "group should return the function's value"
    http.get("BASE/after/1")
`)
	if err != nil {
		t.Fatal(err)
	}
	wantRows(t, rows, map[string]int64{"GET checkout": 1, "POST pay": 1, "POST /order": 1, "GET /cart/${}": 1, "GET /after/{id}": 1})
	if got := strings.Join(steps, ","); got != "pay=ok,checkout=ok" {
		t.Errorf("steps = %q", got)
	}
}

func TestEndpoints_RaisingGroupFailsAndEnds(t *testing.T) {
	rows, steps, err := runEndpoints(t, `
def iteration():
    try:
        with group("bad"):
            http.get("BASE/x/1")
            raise ValueError("boom")
    except ValueError:
        pass
    http.get("BASE/x/2")
    with group("worse"):
        raise ValueError("card declined")
`)
	if err == nil || !strings.Contains(err.Error(), "card declined") {
		t.Fatalf("err = %v, want the group's error", err)
	}
	wantRows(t, rows, map[string]int64{"GET bad": 1, "GET /x/{id}": 1})
	if got := strings.Join(steps, ","); got != "bad=fail,worse=fail" {
		t.Errorf("steps = %q", got)
	}
}

func TestEndpoints_BadArguments(t *testing.T) {
	for name, src := range map[string]string{
		"too few values":    "def iteration():\n    http.url(\"BASE/{}/{}\", 1)\n",
		"too many values":   "def iteration():\n    http.url(\"BASE/{}\", 1, 2)\n",
		"named placeholder": "def iteration():\n    http.url(\"BASE/{id}\", 1)\n",
		"name not a string": "def iteration():\n    http.get(\"BASE/x\", name=5)\n",
		"group not a name":  "def iteration():\n    group(5)\n",
	} {
		if _, _, err := runEndpoints(t, src); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
