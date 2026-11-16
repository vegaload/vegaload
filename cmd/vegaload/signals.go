package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// runSignals stops a run on Ctrl-C or SIGTERM. The first signal ends ctx:
// the load stops, a scenario's teardown runs, and the run then exits with
// the status a shell shows for a process killed by that signal (130 or
// 143). A streamed run ends its stream with run.failed, reason
// "interrupted". A second signal exits at once.
type runSignals struct {
	ctx    context.Context //nolint:containedctx // the run's own lifetime
	cancel context.CancelFunc

	mu   sync.Mutex
	sig  os.Signal
	quit chan struct{}
	stop func()
}

// watchRunSignals starts watching for Ctrl-C and SIGTERM. Call stop when
// the run is over.
func watchRunSignals(st *streaming) *runSignals {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	rs := newRunSignals(ch, st, os.Stderr, os.Exit)
	inner := rs.stop
	rs.stop = func() {
		signal.Stop(ch)
		inner()
	}
	return rs
}

// newRunSignals watches ch. exit is called with the exit code on a second
// signal.
func newRunSignals(ch <-chan os.Signal, st *streaming, stderr io.Writer, exit func(int)) *runSignals {
	ctx, cancel := context.WithCancel(context.Background())
	rs := &runSignals{ctx: ctx, cancel: cancel, quit: make(chan struct{})}
	var once sync.Once
	rs.stop = func() { once.Do(func() { close(rs.quit) }) }
	go func() {
		select {
		case sig := <-ch:
			rs.mu.Lock()
			rs.sig = sig
			rs.mu.Unlock()
			fmt.Fprintln(stderr, "vegaload run: stopping (press Ctrl-C again to quit at once)")
			cancel()
		case <-rs.quit:
			return
		}
		select {
		case sig := <-ch:
			if st != nil {
				exit(st.interrupt(sig))
				return
			}
			exit(signalExitCode(sig))
		case <-rs.quit:
		}
	}()
	return rs
}

// received is the first signal, or nil when there was none.
func (rs *runSignals) received() os.Signal {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.sig
}

// signalExitCode is the status a shell shows for a process killed by sig:
// 130 for Ctrl-C, 143 for SIGTERM.
func signalExitCode(sig os.Signal) int {
	if sig == syscall.SIGTERM {
		return 143
	}
	return 130
}
