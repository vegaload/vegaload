package js

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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	script, err := Load(writeScript(t, "scenario.js", strings.ReplaceAll(src, "BASE", srv.URL)))
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

func TestEndpoints_IDsShareOneRow(t *testing.T) {
	rows, _, err := runEndpoints(t, `
		export default function () {
			for (let i = 1; i <= 50; i++) http.get("BASE/users/" + i + "?v=" + i);
			http.get("BASE/");
		}
	`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows["GET /users/{id}"].Total != 50 || rows["GET /"].Total != 1 {
		t.Fatalf("rows = %s", rowNames(rows))
	}
}

func TestEndpoints_NoURLGrouping(t *testing.T) {
	rows, _, err := runEndpoints(t, `
		export default function () { http.get("BASE/users/1"); http.get("BASE/users/2"); }
	`, WithNoURLGrouping())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows["GET /users/1"].Total != 1 || rows["GET /users/2"].Total != 1 {
		t.Fatalf("rows = %s", rowNames(rows))
	}
}

func TestEndpoints_NameOptionAndURLTemplate(t *testing.T) {
	rows, _, err := runEndpoints(t, `
		export default function () {
			for (const id of ["a", "b", "c"]) {
				http.get("BASE/items/" + id, { name: "/items/{name}" });
				const u = http.url`+"`BASE/things/${id}/parts?full=${id}`"+`;
				if (String(u) !== "BASE/things/" + id + "/parts?full=" + id) throw new Error("String(url) = " + String(u));
				if (u.url !== String(u)) throw new Error("url property = " + u.url);
				http.post(u, { body: "x" });
				http.request("PUT", u, { name: "explicit wins" });
			}
		}
	`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"GET /items/{name}": 3, "POST /things/${}/parts": 3, "PUT explicit wins": 3}
	if len(rows) != len(want) {
		t.Fatalf("rows = %s", rowNames(rows))
	}
	for n, c := range want {
		if rows[n].Total != c {
			t.Errorf("row %q = %d calls, want %d (rows %s)", n, rows[n].Total, c, rowNames(rows))
		}
	}
}

func TestEndpoints_GroupNamesCallsAndIsAStep(t *testing.T) {
	rows, steps, err := runEndpoints(t, `
		export default function () {
			const v = group("checkout", () => {
				http.get("BASE/cart/1");
				group("pay", () => { http.post("BASE/pay/1"); });
				http.post("BASE/order/1", { name: "/order" });
				http.get(http.url`+"`BASE/cart/${7}`"+`);
				return 42;
			});
			if (v !== 42) throw new Error("group should return the function's value");
			http.get("BASE/after/1");
		}
	`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"GET checkout": 1, "POST pay": 1, "POST /order": 1, "GET /cart/${}": 1, "GET /after/{id}": 1}
	if len(rows) != len(want) {
		t.Fatalf("rows = %s", rowNames(rows))
	}
	for n, c := range want {
		if rows[n].Total != c {
			t.Errorf("row %q = %d calls, want %d (rows %s)", n, rows[n].Total, c, rowNames(rows))
		}
	}
	if got := strings.Join(steps, ","); got != "pay=ok,checkout=ok" {
		t.Errorf("steps = %q", got)
	}
}

func TestEndpoints_ThrowingGroupFailsAndEnds(t *testing.T) {
	rows, steps, err := runEndpoints(t, `
		export default function () {
			try { group("bad", () => { http.get("BASE/x/1"); throw new Error("boom"); }); } catch (e) {}
			http.get("BASE/x/2");
			group("worse", () => { throw new Error("card declined"); });
		}
	`)
	if err == nil || !strings.Contains(err.Error(), "card declined") {
		t.Fatalf("err = %v, want the group's error", err)
	}
	if rows["GET bad"].Total != 1 || rows["GET /x/{id}"].Total != 1 {
		t.Errorf("rows = %s: a call after a thrown group is not in it", rowNames(rows))
	}
	if got := strings.Join(steps, ","); got != "bad=fail,worse=fail" {
		t.Errorf("steps = %q", got)
	}
}

func TestEndpoints_BadArguments(t *testing.T) {
	for name, src := range map[string]string{
		"url not a template": `export default function () { http.url("BASE/x"); }`,
		"name not a string":  `export default function () { http.get("BASE/x", { name: 5 }); }`,
		"group without fn":   `export default function () { group("x"); }`,
	} {
		if _, _, err := runEndpoints(t, src); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
