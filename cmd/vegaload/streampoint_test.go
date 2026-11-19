package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/vegaload/vegaload/internal/stream"
)

// The fields a point had before the full live metrics. A reader written for
// them must keep working: each is still there, with the same type.
var oldPointFields = []string{"offsetS", "rps", "requests", "failed", "errorRate", "p50Ms", "p95Ms", "p99Ms", "total", "totalFailed"}

// The fields every point now carries as well.
var newPointFields = []string{
	"minMs", "avgMs", "maxMs", "p90Ms", "activeVUs", "maxVUs",
	"iterationsStarted", "iterationsCompleted", "iterationsDropped",
	"iterationsStartedTotal", "iterationsCompletedTotal", "iterationsDroppedTotal",
	"bytesSent", "bytesReceived", "bytesSentTotal", "bytesReceivedTotal",
	"checksPassed", "checksFailed", "checksPassedTotal", "checksFailedTotal",
}

func TestStreamPointsKeepTheOldFieldsAndCarryTheNewOnes(t *testing.T) {
	clearStreamEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(srv.Close)
	file := filepath.Join(t.TempDir(), "run.ndjson")
	r := runAgainst(t, srv.URL, "-stream-file", file)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	rec, err := stream.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var points []map[string]any
	for _, e := range rec.Events {
		if e.Type == stream.TypePoint {
			points = append(points, dataOf(t, e))
		}
	}
	if len(points) < 2 {
		t.Fatalf("%d points", len(points))
	}
	for i, p := range points {
		for _, k := range append(oldPointFields, newPointFields...) {
			if _, ok := p[k].(float64); !ok {
				t.Fatalf("point %d: %s is %T (%v), want a number", i, k, p[k], p[k])
			}
		}
	}

	// The first point is a full second of two looping VUs calling one URL.
	p := points[0]
	if p["activeVUs"] != float64(2) || p["maxVUs"] != float64(2) {
		t.Errorf("vus: %v/%v", p["activeVUs"], p["maxVUs"])
	}
	if p["requests"].(float64) == 0 || p["iterationsCompleted"] != p["requests"] || p["bytesReceived"] != 5*p["requests"].(float64) {
		t.Errorf("iterations %v, bytes %v, for %v requests", p["iterationsCompleted"], p["bytesReceived"], p["requests"])
	}
	if p["minMs"].(float64) > p["p50Ms"].(float64) || p["p90Ms"].(float64) > p["maxMs"].(float64) {
		t.Errorf("latency figures out of order: %v", p)
	}
	if _, ok := p["ttfbP50Ms"].(float64); !ok {
		t.Errorf("no ttfbP50Ms: %v", p)
	}
	rows, _ := p["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows: %v", p["rows"])
	}
	row := rows[0].(map[string]any)
	if row["protocol"] != "http1" || row["name"] != "GET /" || row["method"] != "GET" || row["endpoint"] != "/" || row["count"] != p["requests"] {
		t.Errorf("row: %v", row)
	}

	// The running totals end where the result does.
	last := points[len(points)-1]
	if last["bytesReceivedTotal"] != r.out["bytes_received"] || last["iterationsCompletedTotal"] != r.out["total"] {
		t.Errorf("last point totals %v/%v, result %v/%v", last["bytesReceivedTotal"], last["iterationsCompletedTotal"], r.out["bytes_received"], r.out["total"])
	}
	if r.out["vus_peak"] != float64(2) || r.out["ttfb"] == nil {
		t.Errorf("result: vus_peak %v ttfb %v", r.out["vus_peak"], r.out["ttfb"])
	}
}
