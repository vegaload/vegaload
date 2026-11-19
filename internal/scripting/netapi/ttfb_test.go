package netapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/report"
)

func TestTTFBMeasureNamesMatch(t *testing.T) {
	if protocol.TTFBMeasure != report.TTFBMeasure {
		t.Fatalf("protocol.TTFBMeasure %q != report.TTFBMeasure %q", protocol.TTFBMeasure, report.TTFBMeasure)
	}
}

// A scenario's HTTP call records its time to first byte: after the server's
// delay before the headers, and before the body has finished.
func TestHTTPClientRecordsTTFB(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write([]byte("done"))
	}))
	defer srv.Close()

	col := report.NewCollector()
	c := NewHTTPClient(nil, 5*time.Second)
	c.SetCallRecorder(col)
	if _, err := c.Do(context.Background(), "GET", srv.URL+"/slow", nil, Options{}); err != nil {
		t.Fatal(err)
	}
	res := col.Finish("fixed-vus", time.Second)
	if res.TTFB == nil || res.TTFB.Histogram.Count() != 1 {
		t.Fatalf("ttfb = %+v", res.TTFB)
	}
	p, _ := res.Protocol("http")
	if ttfb := res.TTFB.Max; ttfb < 30*time.Millisecond || ttfb >= p.Latency.Max {
		t.Fatalf("ttfb %v, call %v: want at least 30ms and less than the call", ttfb, p.Latency.Max)
	}
	if len(p.Measures) != 1 || p.Measures[0].Name != report.TTFBMeasure {
		t.Fatalf("measures = %+v", p.Measures)
	}
}
