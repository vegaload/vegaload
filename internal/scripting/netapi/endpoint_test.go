package netapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/report"
)

func TestRowName_Order(t *testing.T) {
	c := NewHTTPClient(nil, time.Second)
	u := "http://api.test/users/42?x=1"
	cases := []struct {
		opts Options
		want string
	}{
		{Options{}, "/users/{id}"},
		{Options{Group: "checkout"}, "checkout"},
		{Options{Name: "/users/:id", Group: "checkout"}, "/users/:id"},
	}
	for _, tc := range cases {
		if got := c.RowName(u, tc.opts); got != tc.want {
			t.Errorf("RowName(%+v) = %q, want %q", tc.opts, got, tc.want)
		}
	}
	raw := NewHTTPClient(nil, time.Second, NoURLGrouping())
	if got := raw.RowName(u, Options{}); got != "/users/42" {
		t.Errorf("NoURLGrouping RowName = %q, want /users/42", got)
	}
}

func TestHTTP_RecordsEndpointRows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	col := report.NewCollector()
	c := NewHTTPClient(nil, time.Second)
	c.SetCallRecorder(col)
	ctx := context.Background()
	for _, path := range []string{"/users/1", "/users/2?page=3", "/users/3"} {
		if _, err := c.Do(ctx, "GET", srv.URL+path, nil, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Do(ctx, "POST", srv.URL+"/users/9", nil, Options{Group: "signup"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(ctx, "POST", srv.URL+"/users/9", nil, Options{Name: "/users/new", Group: "signup"}); err != nil {
		t.Fatal(err)
	}
	p, _ := col.Finish("fixed-vus", time.Second).Protocol("http")
	want := map[string]int64{"GET /users/{id}": 3, "POST signup": 1, "POST /users/new": 1}
	if len(p.Operations) != len(want) {
		t.Fatalf("rows = %+v", p.Operations)
	}
	for _, o := range p.Operations {
		if want[o.Name] != o.Total {
			t.Errorf("row %q has %d calls, want %d", o.Name, o.Total, want[o.Name])
		}
	}
	if o, _ := p.Operation("GET /users/{id}"); o.Method != "GET" || o.Endpoint != "/users/{id}" {
		t.Errorf("method/endpoint = %q %q", o.Method, o.Endpoint)
	}
}
