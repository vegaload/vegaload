package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vegaload/vegaload/internal/engine"
	"github.com/vegaload/vegaload/internal/live"
	"github.com/vegaload/vegaload/internal/report"
	"github.com/vegaload/vegaload/internal/secrets"
	"github.com/vegaload/vegaload/internal/stream"
	"github.com/vegaload/vegaload/internal/urlerr"
)

// This file wires `vegaload run` to internal/stream and internal/live. The
// contract is docs/streaming.md. With no stream and no live view, runConfig.stream
// is nil and nothing in here runs.

// pointGrace is how long after the end of a second its point waits, so that a
// call that is just starting or just ending in that second is counted in it.
// A point is also held back while a call that started in that second, or an
// earlier one, is still running, for at most report.LiveMaxHold past the end
// of its second (docs/streaming.md, "point").
//
// It is a variable only so that a test need not wait half a second.
var pointGrace = 500 * time.Millisecond

// liveMaxHold is report.LiveMaxHold; a variable so that a test need not wait
// ten seconds.
var liveMaxHold = report.LiveMaxHold

// slowCallsWarning is the one warning of a run whose hold hit the limit. It
// names no target and nothing a user supplied.
const slowCallsWarning = "live points were emitted before slow calls finished; the final result has them"

// streamCloseTimeout bounds how long the end of a run waits for the sinks.
const streamCloseTimeout = 5 * time.Second

// interruptCloseTimeout bounds the same wait after Ctrl-C or SIGTERM.
const interruptCloseTimeout = 3 * time.Second

// pushFlag collects repeated -stream-push URLs.
type pushFlag []string

func (p *pushFlag) String() string { return strings.Join(*p, ",") }
func (p *pushFlag) Set(v string) error {
	*p = append(*p, v)
	return nil
}

// labelFlag collects repeated -label key=value flags.
type labelFlag map[string]string

func (l labelFlag) String() string { return fmt.Sprint(map[string]string(l)) }

func (l labelFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(k) == "" {
		return fmt.Errorf("label %q: want key=value", v)
	}
	l[strings.TrimSpace(k)] = val
	return nil
}

// runStream holds the streaming flags of `run`. All of them are optional:
// with none set, and the live view off, `run` behaves as it always has.
// -no-open is the run's own flag (it also stops the HTML report opening).
type runStream struct {
	file     string
	push     pushFlag
	insecure bool
	runID    string
	labels   labelFlag

	// The live view, on by default (see internal/live).
	liveOn  bool
	noLive  bool
	liveURL string
}

func (s *runStream) register(fs *flag.FlagSet) {
	s.labels = labelFlag{}
	fs.StringVar(&s.file, "stream-file", "", "write the run as NDJSON events to PATH, or `auto` for .vegaload/runs/<run id>.ndjson (also VEGALOAD_STREAM_FILE). Turns the live view off")
	fs.Var(&s.push, "stream-push", "POST the run's events to this ingest URL, with retry; a comma-separated list or repeated; https unless the host is loopback; the Bearer token comes from VEGALOAD_STREAM_TOKEN (also VEGALOAD_STREAM_PUSH). Turns the live view off")
	fs.BoolVar(&s.insecure, "stream-insecure", false, "allow -stream-push over http:// to a host that is not loopback")
	fs.StringVar(&s.runID, "run-id", "", "run id used on stream events (A-Z a-z 0-9 . _ -, up to 64; default: 12 random hex digits; also VEGALOAD_RUN_ID)")
	fs.Var(s.labels, "label", "label key=value on every stream event (repeatable)")
	fs.BoolVar(&s.liveOn, "live", false, "show the run in the live view even when CI is set or a -stream-* destination is used")
	fs.BoolVar(&s.noLive, "no-live", false, "do not stream to the live view or open it in a browser (also VEGALOAD_LIVE=off)")
	fs.StringVar(&s.liveURL, "live-url", "", "base address of the live view service (default: VEGALOAD_LIVE_URL, or "+live.DefaultBase+")")
}

// streaming is what setup produced for one run.
type streaming struct {
	rec        *stream.Recorder
	warnings   []string // problems known before the run starts
	audit      string   // what the audit log may say: kinds and hosts, nothing else
	liveActive bool     // the live view is showing this run

	mu         sync.Mutex
	collector  *report.Collector // set once the run's clock starts
	executor   string
	closeOnce  sync.Once
	sigStop    func()
	closeStuck []string
}

// setup returns the streaming state for this run, or nil when there is
// neither a -stream-* destination nor a live view. A flag wins over its
// environment variable. A destination that breaks a rule (http:// to a remote
// host, a bad run id, a bad label) is refused, so the run does not start. A
// sink that cannot be set up for another reason, such as a file that cannot
// be written, is a warning and the run goes on without it. noOpen is -no-open.
func (s *runStream) setup(noOpen bool) (*streaming, error) {
	if s.liveOn && s.noLive {
		return nil, fmt.Errorf("-live and -no-live together: pick one")
	}
	file := s.file
	if file == "" {
		file = os.Getenv("VEGALOAD_STREAM_FILE")
	}
	var pushes []string
	for _, v := range s.push {
		pushes = append(pushes, splitCSV(v)...)
	}
	if len(pushes) == 0 {
		pushes = splitCSV(os.Getenv("VEGALOAD_STREAM_PUSH"))
	}
	runID := s.runID
	if runID == "" {
		runID = os.Getenv("VEGALOAD_RUN_ID")
	}
	if runID != "" {
		if err := stream.ValidRunID(runID); err != nil {
			return nil, err
		}
	} else {
		runID = stream.NewRunID()
	}
	explicit := file != "" || len(pushes) > 0

	// Rules first, so a refused destination never starts a run.
	var urls []*url.URL
	for _, raw := range pushes {
		u, err := stream.ValidatePushURL(raw, s.insecure)
		if err != nil {
			return nil, err
		}
		urls = append(urls, u)
	}

	// The live view is the default when the user asked for no destination of
	// their own. -live adds it to the ones they asked for.
	var lv *liveView
	if live.Enabled(s.liveOn, s.noLive) && (!explicit || s.liveOn) {
		lv = s.resolveLive()
	}
	if !explicit && lv == nil {
		return nil, nil
	}
	token := os.Getenv("VEGALOAD_STREAM_TOKEN")

	st := &streaming{}
	var sinks []stream.Sink
	var kinds []string
	var where []string
	if file != "" {
		path := file
		if file == "auto" {
			path = filepath.Join(".vegaload", "runs", runID+".ndjson")
		}
		fsink, err := stream.NewFileSink(path)
		if err != nil {
			st.warnings = append(st.warnings, "stream: "+err.Error()+"; the run goes on without it")
		} else {
			sinks = append(sinks, fsink)
			kinds = append(kinds, "file")
			where = append(where, "file "+path)
		}
	}
	for _, u := range urls {
		sinks = append(sinks, stream.NewPushSink(stream.PushOptions{URL: u, Token: token, Version: version}))
		kinds = append(kinds, "push:"+u.Host)
		where = append(where, "push "+u.Scheme+"://"+u.Host)
	}
	if lv != nil {
		// Its own token: the project's push token, not VEGALOAD_STREAM_TOKEN.
		sinks = append(sinks, stream.NewPushSink(stream.PushOptions{URL: lv.push, Token: lv.token, Version: version}))
		kinds = append(kinds, "live:"+lv.push.Host)
		st.liveActive = true
	}

	var labels map[string]string
	if len(s.labels) > 0 {
		labels = map[string]string(s.labels)
	}
	st.rec = stream.NewRecorder(stream.RecorderOptions{RunID: runID, Labels: labels}, sinks...)
	st.audit = strings.Join(kinds, ",")
	if len(where) > 0 {
		fmt.Fprintf(os.Stderr, "streaming run %s: %s\n", runID, strings.Join(where, ", "))
	}
	if lv != nil {
		fmt.Fprintf(os.Stderr, "live view: %s   (not wanted? -no-live, or VEGALOAD_LIVE=off)\n", lv.page)
		if live.OpenEnabled(noOpen) {
			if err := live.Opener(lv.page); err != nil {
				fmt.Fprintf(os.Stderr, "live view: could not open the browser (%v); open the URL above\n", err)
			}
		}
	}
	return st, nil
}

func splitCSV(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// liveView is the resolved live-view destination of one run.
type liveView struct {
	push  *url.URL
	token string
	page  string
}

// resolveLive prepares the live view, or says on stderr why there is none. It
// never fails the run: a machine with no service, or one that cannot keep its
// identity file, just runs without the view.
func (s *runStream) resolveLive() *liveView {
	id, err := live.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "live view: off (%v)\n", err)
		return nil
	}
	base, err := id.BaseURL(s.liveURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "live view: off (%v)\n", err)
		return nil
	}
	push, err := stream.ValidatePushURL(id.PushURL(base), false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "live view: off (%v)\n", err)
		return nil
	}
	if err := live.Probe(context.Background(), base); err != nil {
		fmt.Fprintf(os.Stderr, "live view: off, %s://%s %v. The run goes on without it (-no-live to stop this message)\n", base.Scheme, base.Host, err)
		return nil
	}
	if id.Created {
		// The project id is the secret that protects the page, so it is not
		// repeated here; the URL printed next is the one place it appears.
		fmt.Fprintf(os.Stderr, "live view: made a project for this machine, kept in %s\n", id.Path)
	}
	return &liveView{push: push, token: id.PushToken, page: id.PageURL(base)}
}

// attach is called by the runner once the run's clock has started.
func (st *streaming) attach(c *report.Collector, executor string) {
	st.mu.Lock()
	st.collector, st.executor = c, executor
	st.mu.Unlock()
}

// partial returns the result of the run so far, or nil before it started.
func (st *streaming) partial() *report.Result {
	st.mu.Lock()
	c, ex := st.collector, st.executor
	st.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.Finish(ex, time.Since(c.Start()))
}

// begin starts the stream for a run whose collector has just been made: it
// sends run.started and returns the feed of one-second points. The runner
// calls finish on the feed when the executor has returned.
func (st *streaming) begin(cfg *runConfig, ex engine.Executor, c *report.Collector) *pointFeed {
	st.attach(c, ex.Name())
	st.rec.Started(c.Start(), buildRunStarted(cfg, ex.Name(), ex.Duration()))
	return startPointFeed(st.rec, c)
}

// pointFeed sends one point per second while the run goes on.
type pointFeed struct {
	em   stream.Emitter
	lb   *report.LiveBuckets
	c    *report.Collector
	stop chan struct{}
	done chan struct{}
	once sync.Once
	// warned is only used by the feed goroutine and, after it has stopped, by finish.
	warned bool
}

// send hands one point to the stream, and says once if it left out a call
// that was still running.
func (f *pointFeed) send(b report.BucketStat) {
	if b.Capped && !f.warned {
		f.warned = true
		f.em.Warning(slowCallsWarning)
	}
	f.em.Point(toPoint(b))
}

func startPointFeed(em stream.Emitter, c *report.Collector) *pointFeed {
	f := &pointFeed{em: em, lb: c.LiveBuckets(), c: c, stop: make(chan struct{}), done: make(chan struct{})}
	f.lb.SetMaxHold(liveMaxHold)
	go func() {
		defer close(f.done)
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				now := time.Since(c.Start())
				for {
					b, ok := f.lb.Next(now, pointGrace)
					if !ok {
						break
					}
					f.send(b)
				}
			case <-f.stop:
				return
			}
		}
	}()
	return f
}

// finish stops the feed and sends the points not sent yet, so that the points
// and the final time series cover the same seconds: none missed, none twice.
func (f *pointFeed) finish() {
	f.once.Do(func() {
		close(f.stop)
		<-f.done
		for _, b := range f.lb.Rest() {
			f.send(b)
		}
	})
}

func toPoint(b report.BucketStat) stream.Point {
	return stream.Point{
		OffsetS:     b.Offset.Seconds(),
		RPS:         b.RPS,
		Requests:    b.Requests,
		Failed:      b.Failed,
		ErrorRate:   b.ErrorRate,
		P50Ms:       ms(b.P50),
		P95Ms:       ms(b.P95),
		P99Ms:       ms(b.P99),
		Total:       b.Total,
		TotalFailed: b.TotalFailed,
	}
}

func ms(d time.Duration) float64 { return math.Round(float64(d)/float64(time.Millisecond)*1000) / 1000 }

// buildRunStarted describes the run for run.started. It reads the planned
// shape and the thresholds, and nothing a user supplied as a credential: not
// the headers, body, options, environment, secrets or data (docs/streaming.md).
func buildRunStarted(cfg *runConfig, executor string, duration time.Duration) stream.RunStarted {
	rs := stream.RunStarted{
		Executor:      executor,
		Duration:      duration.String(),
		AbortOnBreach: cfg.AbortOnBreach,
		Version:       version,
	}
	if cfg.ScenarioPath != "" {
		rs.Name = filepath.Base(cfg.ScenarioPath)
	} else {
		target := stream.SanitizeTarget(cfg.Target.URL)
		rs.Name = cfg.Protocol + " " + target
		rs.Target = &stream.Target{Protocol: cfg.Protocol, BaseURL: target}
	}
	switch cfg.Executor {
	case "constant-arrival-rate":
		rs.Rate = cfg.Rate
		rs.VUs = cfg.MaxVUs
	case "ramp", "step":
		rs.VUs = concurrencyHint(cfg)
		for _, s := range cfg.Stages {
			rs.Stages = append(rs.Stages, stream.Stage{Target: s.Target, Duration: s.Duration.String()})
		}
	default:
		rs.VUs = cfg.VUs
	}
	for _, t := range cfg.Thresholds {
		rs.Thresholds = append(rs.Thresholds, stream.ThresholdSpec{
			Name: t.Name, Metric: t.Metric, Operator: t.Operator, Value: t.Value,
			Step: t.Step, Protocol: t.Protocol, Op: t.Op,
		})
	}
	return rs
}

// runOutcome is what the run produced, for the end of the stream.
type runOutcome struct {
	result *report.Result
	reason string // why there is no result
}

// fail records why the run has no result. The text is cleaned the same way
// the error on stderr is, and a URL's password is masked.
func (o *runOutcome) fail(err error) {
	o.reason = secrets.Redact(urlerr.Mask(err.Error()))
}

// watchSignals ends the stream with run.failed (reason "interrupted") on
// Ctrl-C or SIGTERM, then exits with the status a process killed by that
// signal shows in a shell (130, 143). Without a stream, `run` installs no
// handler and a signal kills it exactly as before.
func (st *streaming) watchSignals() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	quit := make(chan struct{})
	st.sigStop = func() {
		signal.Stop(ch)
		close(quit)
	}
	go func() {
		select {
		case sig := <-ch:
			// A second signal must not be swallowed while the sinks flush.
			signal.Stop(ch)
			os.Exit(st.interrupt(sig))
		case <-quit:
		}
	}()
}

// interrupt ends the stream for a run stopped by sig and returns the exit
// code a shell shows for a process killed by it: 130 for Ctrl-C, 143 for SIGTERM.
func (st *streaming) interrupt(sig os.Signal) int {
	st.rec.Failed("interrupted", st.partialOrNil())
	st.closeSinks(interruptCloseTimeout)
	if sig == syscall.SIGTERM {
		return 143
	}
	return 130
}

// partialOrNil returns the partial result as an any that is nil when there is none.
func (st *streaming) partialOrNil() any {
	if p := st.partial(); p != nil {
		return p
	}
	return nil
}

// end finishes the stream: run.finished with the result and the verdict, or
// run.failed when there is no result. It runs after the reports are written,
// so a stuck sink cannot delay them or change the exit code. code is the
// process exit code of the run.
func (st *streaming) end(cfg *runConfig, out *runOutcome, code int) {
	if st == nil {
		return
	}
	if st.sigStop != nil {
		st.sigStop()
	}
	if out.result != nil {
		st.rec.Finished(out.result, code == 0, code)
	} else {
		reason := out.reason
		if reason == "" {
			reason = "the run did not complete"
		}
		// The run may have failed before its clock started.
		if !st.rec.HasStarted() {
			st.rec.Started(time.Now(), buildRunStarted(cfg, cfg.Executor, cfg.Duration))
		}
		st.rec.Failed(reason, st.partialOrNil())
	}
	st.closeSinks(streamCloseTimeout)
}

// closeSinks flushes the sinks, bounded, and reports what went wrong with them.
func (st *streaming) closeSinks(timeout time.Duration) {
	st.closeOnce.Do(func() {
		st.closeStuck = st.rec.Close(timeout)
		for _, w := range st.rec.Warnings() {
			fmt.Fprintln(os.Stderr, w)
		}
		for _, name := range st.closeStuck {
			fmt.Fprintf(os.Stderr, "stream: %s did not finish; some events may be missing\n", name)
		}
	})
}
