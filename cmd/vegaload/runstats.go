package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/vegaload/vegaload/internal/report"
	"github.com/vegaload/vegaload/internal/statsapi"
)

// statsExporter is the /stats server of one run (-stats-addr). It answers
// with the run's cumulative numbers while it goes on, and with the final
// ones after it, until the process exits. It sends nothing anywhere. A nil
// statsExporter is a run without -stats-addr, and does nothing.
type statsExporter struct {
	srv *statsapi.Server

	mu        sync.Mutex
	collector *report.Collector
	executor  string
}

// startStats listens on addr before the run's clock starts, so a scraper can
// connect first.
func startStats(addr string) (*statsExporter, error) {
	srv, err := statsapi.Listen(addr)
	if err != nil {
		return nil, err
	}
	return &statsExporter{srv: srv}, nil
}

func (s *statsExporter) url() string { return "http://" + s.srv.Addr() + "/stats" }

// attach serves the live numbers of c, whose clock has just started.
func (s *statsExporter) attach(c *report.Collector, executor string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.collector, s.executor = c, executor
	s.mu.Unlock()
	s.srv.SetSource(func() statsapi.Stats {
		return statsapi.FromResult(c.Finish(executor, time.Since(c.Start())), c.ActiveVUs(), statsapi.StateRunning)
	})
}

// finish serves the finished run's numbers from now on. With no result the
// run failed, and the numbers are what it recorded before it stopped.
func (s *statsExporter) finish(res *report.Result) {
	if s == nil {
		return
	}
	if res != nil {
		s.srv.Freeze(statsapi.FromResult(res, 0, statsapi.StateFinished))
		return
	}
	s.mu.Lock()
	c, ex := s.collector, s.executor
	s.mu.Unlock()
	var partial *report.Result
	if c != nil {
		partial = c.Finish(ex, time.Since(c.Start()))
	}
	s.srv.Freeze(statsapi.FromResult(partial, 0, statsapi.StateFailed))
}

// linger keeps the process, and so the server, up for d after the run, so a
// scraper can read the final numbers. Ctrl-C or SIGTERM ends it early.
func (s *statsExporter) linger(d time.Duration, stderr io.Writer) {
	if s == nil || d <= 0 {
		return
	}
	fmt.Fprintf(stderr, "stats: serving the final numbers at %s for %s (Ctrl-C to stop)\n", s.url(), d)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(ch)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ch:
	}
}

func (s *statsExporter) close() {
	if s != nil {
		_ = s.srv.Close()
	}
}
