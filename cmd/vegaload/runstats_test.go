package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/engine"
	"github.com/vegaload/vegaload/internal/failure"
	"github.com/vegaload/vegaload/internal/report"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

type statsBody struct {
	State         string                    `json:"state"`
	TotalRequests int64                     `json:"totalRequests"`
	CurrentUsers  int64                     `json:"currentUsers"`
	PeakUsers     int64                     `json:"peakUsers"`
	RequestStats  map[string]map[string]any `json:"requestStats"`
	LatencyHist   map[string]int64          `json:"latencyHist"`
	DataReceived  int64                     `json:"dataReceivedBytes"`
	P50LatencyMs  float64                   `json:"p50LatencyMs"`
}

func readStats(url string) (statsBody, error) {
	var st statsBody
	resp, err := http.Get(url)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// -stats-addr serves the run while it goes on, and its final numbers for
// -stats-linger after it.
func TestStatsAddrServesTheRunAndItsFinalNumbers(t *testing.T) {
	clearStreamEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Millisecond)
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(srv.Close)
	addr := freeAddr(t)
	out := filepath.Join(t.TempDir(), "out.json")
	args := []string{"-target", srv.URL + "/items", "-protocol", "http1", "-vus", "2", "-duration", "2s",
		"-no-report", "-audit-log", filepath.Join(t.TempDir(), "audit.log"), "-out", out,
		"-stats-addr", addr, "-stats-linger", "3s"}
	type ran struct {
		code   int
		stderr string
	}
	done := make(chan ran, 1)
	go func() {
		code, _, stderr := captureCmd(t, func() int { return cmdRun(args) })
		done <- ran{code, stderr}
	}()

	url := "http://" + addr + "/stats"
	var reads []statsBody
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := readStats(url)
		if err == nil && (st.State == "running" && st.TotalRequests > 0 || st.State == "finished") {
			reads = append(reads, st)
			if st.State == "finished" {
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	if len(reads) < 3 {
		t.Fatalf("%d reads: %+v", len(reads), reads)
	}
	final := reads[len(reads)-1]
	if final.State != "finished" || final.CurrentUsers != 0 || final.PeakUsers != 2 {
		t.Fatalf("final read %+v", final)
	}
	if reads[0].State != "running" || reads[0].CurrentUsers != 2 {
		t.Fatalf("first read %+v", reads[0])
	}
	for i := 1; i < len(reads); i++ {
		if reads[i].TotalRequests < reads[i-1].TotalRequests {
			t.Fatalf("totalRequests went down: %d then %d", reads[i-1].TotalRequests, reads[i].TotalRequests)
		}
	}
	row, ok := final.RequestStats["GET:/items"]
	if !ok || row["numRequests"] != float64(final.TotalRequests) || final.DataReceived != 5*final.TotalRequests || final.P50LatencyMs <= 0 {
		t.Fatalf("final row %v (ok %v), bytes %d, ttfb p50 %v", row, ok, final.DataReceived, final.P50LatencyMs)
	}
	var n int64
	for _, c := range final.LatencyHist {
		n += c
	}
	if n != final.TotalRequests {
		t.Fatalf("latencyHist adds up to %d, want %d", n, final.TotalRequests)
	}

	select {
	case r := <-done:
		t.Fatalf("the run exited (%d) before -stats-linger ended: %s", r.code, r.stderr)
	default:
	}
	r := <-done
	if r.code != 0 || !strings.Contains(r.stderr, "stats: http://"+addr+"/stats") || !strings.Contains(r.stderr, "serving the final numbers") {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if _, err := readStats(url); err == nil {
		t.Fatal("/stats still answers after the process would have exited")
	}
}

func TestStatsFlagsAreChecked(t *testing.T) {
	if _, err := parseRunArgs([]string{"-target", "http://127.0.0.1:1", "-protocol", "http1", "-stats-linger", "5s"}); err == nil || !strings.Contains(err.Error(), "-stats-linger needs -stats-addr") {
		t.Fatalf("linger without addr: %v", err)
	}
	if _, err := parseRunArgs([]string{"-target", "http://127.0.0.1:1", "-protocol", "http1", "-stats-addr", ":0", "-stats-linger", "-1s"}); err == nil {
		t.Fatal("a negative linger was accepted")
	}

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	code, _, stderr := captureCmd(t, func() int {
		return cmdRun([]string{"-target", "http://127.0.0.1:1", "-protocol", "http1", "-duration", "1s", "-no-report", "-audit-log", filepath.Join(t.TempDir(), "a.log"), "-stats-addr", taken.Addr().String()})
	})
	if code != 2 || !strings.Contains(stderr, "-stats-addr") {
		t.Fatalf("a taken address: exit %d: %s", code, stderr)
	}
}

func TestPrintResultShowsLoadBytesTTFBAndErrorKinds(t *testing.T) {
	c := report.NewCollector()
	c.RequestsFromCalls()
	c.SetPlannedVUs(4)
	c.VUStarted()
	c.Begin(c.Start())
	c.RecordCall("http", "GET /a", 10*time.Millisecond, fmt.Errorf("http: %w", failure.Status(503)), 10, 2048)
	c.RecordMeasure("http", report.TTFBMeasure, 3*time.Millisecond)
	c.Record(engine.IterationResult{Start: c.Start(), Duration: 20 * time.Millisecond})
	c.End(c.Start())
	var b strings.Builder
	printResult(&b, c.Finish("fixed-vus", time.Second))
	for _, want := range []string{"vus:      peak 1 of 4", "iters:    1 started, 1 completed", "bytes:    10 B sent, 2.0 KiB received", "ttfb:     p50 3ms", "errors by kind", "status 503", "http: status 503"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, b.String())
		}
	}
}
