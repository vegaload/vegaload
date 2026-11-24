package report

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// recordEndpoints records one call to each of n distinct rows, the even
// ones failed, plus a second call to the first row.
func recordEndpoints(c *Collector, n int) {
	for i := 0; i < n; i++ {
		var err error
		if i%2 == 0 {
			err = errors.New("status 500")
		}
		c.RecordCall("http", fmt.Sprintf("GET /r%d", i), time.Duration(i+1)*time.Millisecond, err, 1, 2)
	}
	c.RecordCall("http", "GET /r0", time.Millisecond, nil, 1, 2)
}

func TestEndpoints_CapFoldsIntoOther(t *testing.T) {
	c := NewCollector()
	recordEndpoints(c, 600)
	c.RecordCall("ftp", "download", time.Millisecond, nil, 0, 0)
	res := c.Finish("fixed-vus", time.Second)
	p, _ := res.Protocol("http")

	if len(p.Operations) != MaxEndpoints+1 {
		t.Fatalf("%d rows, want %d named plus Other", len(p.Operations), MaxEndpoints)
	}
	other := p.Operations[len(p.Operations)-1]
	if other.Name != OtherEndpoints || other.Total != 100 || other.Failed != 50 || other.Method != "" || other.Endpoint != "" {
		t.Fatalf("Other row = %+v", other)
	}
	var total, failed, sent int64
	for _, o := range p.Operations {
		total += o.Total
		failed += o.Failed
		sent += o.BytesSent
		if o.Latency.Histogram.Count() != o.Total {
			t.Errorf("row %s histogram holds %d of %d", o.Name, o.Latency.Histogram.Count(), o.Total)
		}
	}
	if total != 601 || p.Total != total || failed != 300 || p.Failed != failed || sent != p.BytesSent {
		t.Fatalf("sum of rows %d/%d/%d, protocol %d/%d/%d", total, failed, sent, p.Total, p.Failed, p.BytesSent)
	}
	if first := p.Operations[0]; first.Name != "GET /r0" || first.Total != 2 || first.Method != "GET" || first.Endpoint != "/r0" {
		t.Fatalf("first row = %+v", first)
	}

	if got := len(Endpoints(res)); got != MaxEndpoints+1 {
		t.Errorf("Endpoints = %d rows", got)
	}
	ftp, _ := res.Protocol("ftp")
	if ftp.Operations[0].Method != "" || ftp.Operations[0].Endpoint != "" {
		t.Errorf("a non-HTTP op got endpoint fields: %+v", ftp.Operations[0])
	}
}

func TestEndpoints_MergeKeepsTheCap(t *testing.T) {
	a, b := NewCollector(), NewCollector()
	recordEndpoints(a, 400)
	for i := 300; i < 700; i++ {
		b.RecordCall("http", fmt.Sprintf("GET /r%d", i), time.Millisecond, nil, 0, 0)
	}
	ra, rb := a.Finish("fixed-vus", time.Second), b.Finish("fixed-vus", time.Second)
	m, err := Merge([]*Result{ra, rb})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := m.Protocol("http")
	if len(p.Operations) != MaxEndpoints+1 || p.Operations[len(p.Operations)-1].Name != OtherEndpoints {
		t.Fatalf("%d merged rows", len(p.Operations))
	}
	var total int64
	for _, o := range p.Operations {
		total += o.Total
	}
	pa, _ := ra.Protocol("http")
	pb, _ := rb.Protocol("http")
	if total != pa.Total+pb.Total || total != p.Total || total != 801 {
		t.Fatalf("merged rows hold %d calls, protocol %d, inputs %d", total, p.Total, pa.Total+pb.Total)
	}
	if o, _ := p.Operation("GET /r350"); o.Total != 2 || o.Method != "GET" || o.Endpoint != "/r350" {
		t.Errorf("shared row = %+v", o)
	}
	// /r0../r399 from a, then /r400../r499 from b fill the 500 rows.
	if o, _ := p.Operation(OtherEndpoints); o.Total != 200 {
		t.Errorf("Other = %d calls, want 200", o.Total)
	}
}

func TestEndpoints_Views(t *testing.T) {
	c := NewCollector()
	c.RecordCall("http", "GET /users/{id}", 2*time.Millisecond, nil, 0, 10)
	c.RecordCall("http", "POST checkout", 5*time.Millisecond, errors.New("status 503"), 3, 0)
	res := c.Finish("fixed-vus", time.Second)

	page := renderHTML(res)
	if !strings.Contains(page, "<h2>Endpoints</h2>") || !strings.Contains(page, "GET /users/{id}") || !strings.Contains(page, "POST checkout") {
		t.Fatalf("HTML has no endpoints table:\n%s", page)
	}
	if strings.Contains(page, "<h2>Protocols</h2>") {
		t.Error("a plain HTTP run shows the protocols table")
	}
	md := MarkdownSummary(res)
	if !strings.Contains(md, "### Endpoints") || !strings.Contains(md, "| PASS | GET /users/{id} | 1 | 0 |") || !strings.Contains(md, "| FAIL | POST checkout | 1 | 1 |") {
		t.Fatalf("Markdown has no endpoints table:\n%s", md)
	}

	// Next to another protocol, http keeps its protocol line, but its rows
	// are only in the endpoints table.
	c.RecordCall("ftp", "download", time.Millisecond, nil, 0, 0)
	mixed := c.Finish("fixed-vus", time.Second)
	md = MarkdownSummary(mixed)
	if !strings.Contains(md, "### Protocols") || strings.Count(md, "GET /users/{id}") != 1 || !strings.Contains(md, "↳ download") {
		t.Fatalf("mixed Markdown:\n%s", md)
	}
	if page := renderHTML(mixed); strings.Count(page, "GET /users/{id}") != 1 {
		t.Fatalf("mixed HTML repeats the endpoint rows:\n%s", page)
	}

	if strings.Contains(renderHTML(&Result{Executor: "fixed-vus"}), "Endpoints") || strings.Contains(MarkdownSummary(&Result{}), "Endpoints") {
		t.Error("a run without HTTP calls shows an endpoints table")
	}
}
