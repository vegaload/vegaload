// run.go implements `vegaload run`, the command that actually drives a
// load test. It wires together three things built in earlier Phase 0
// deliverables without any of them knowing about each other:
//
//   - internal/engine picks the load shape (fixed-vus, ramp, step,
//     constant-arrival-rate) and schedules VUs against it.
//   - internal/protocol/* send requests, when run is given a
//     protocol-direct target (-target and -protocol, no scenario file).
//   - internal/scripting/* run a scenario file's exported function,
//     when run is given one instead.
//
// A run is exactly one of those two modes, never both: either
//
//	vegaload run -target https://example.com -protocol http1 -vus 10 -duration 30s
//
// or
//
//	vegaload run scenario.vl.js -vus 10 -duration 30s
//
// The two modes stay separate rather than "a script that also hits
// -target": protocol-direct mode repeats one fixed target as fast and
// simply as possible, while a scenario file (per FR-CLI-08) can make
// any number of its own real HTTP/WebSocket calls via the `http`/`ws`
// globals internal/scripting/js and /python inject into every VU,
// carrying a value from one call into the next — see those packages'
// doc comments for a worked example (LaunchPad's create-then-ignite
// flow). Every one of those calls still goes through the same
// allowlist FR-CLI-06 built for -target, just enforced per call instead
// of once up front — see buildSafetyCheck below for why that has to
// differ from enforceSafety's own one-time, promptable check.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vegaload/vegaload/internal/audit"
	"github.com/vegaload/vegaload/internal/engine"
	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/protocol/grpc"
	"github.com/vegaload/vegaload/internal/protocol/http1"
	"github.com/vegaload/vegaload/internal/protocol/http2"
	"github.com/vegaload/vegaload/internal/protocol/websocket"
	"github.com/vegaload/vegaload/internal/report"
	"github.com/vegaload/vegaload/internal/safety"
	"github.com/vegaload/vegaload/internal/scripting/js"
	"github.com/vegaload/vegaload/internal/scripting/netapi"
	"github.com/vegaload/vegaload/internal/scripting/python"
)

// headerFlags accumulates repeated -header "Key: Value" flags into a map.
type headerFlags map[string]string

func (h headerFlags) String() string { return "" }

func (h headerFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, ":")
	if !ok {
		return fmt.Errorf("-header %q must be in \"Key: Value\" form", v)
	}
	h[strings.TrimSpace(k)] = strings.TrimSpace(val)
	return nil
}

// repeatedFlags accumulates repeated occurrences of a flag (e.g.
// -allow-target) into a slice.
type repeatedFlags []string

func (r *repeatedFlags) String() string { return "" }

func (r *repeatedFlags) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// runConfig is everything `vegaload run` needs, independent of how it
// was parsed — kept separate from flag.FlagSet so the logic below it is
// testable without going through command-line parsing.
type runConfig struct {
	ScenarioPath string

	Executor string // fixed-vus, ramp, step, constant-arrival-rate
	VUs      int
	Duration time.Duration
	Stages   []engine.Stage
	Rate     float64
	MaxVUs   int

	Target   protocol.Target
	Protocol string
	Timeout  time.Duration

	OutPath string

	// FR-CLI-06: target allowlist and hard caps.
	AllowTargets []string
	Yes          bool // skip the confirmation prompt for a non-allowlisted target
	Limits       safety.Limits

	// FR-CLI-07: audit log. Trigger identifies what started this run
	// ("cli" by default; the MCP server's run_test tool sets
	// "mcp:<session-id>" — see internal/mcp in Phase 2).
	Trigger      string
	AuditLogPath string

	// FR-RPT-01/02 and FR-CLI-05: the HTML report and the output mode.
	OutputMode string // "text" (default), "json", or "jsonl"
	ReportPath string
	NoReport   bool
	NoOpen     bool
}

// parseRunArgs parses run's flags into a runConfig. It returns a usage
// error (rather than exiting the process) so it can be exercised by
// tests and so cmdRun controls its own exit code.
func parseRunArgs(args []string) (*runConfig, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfg := &runConfig{}
	headers := headerFlags{}

	var stagesRaw string
	var bodyRaw string
	var method string
	var insecure bool
	var allowTargets repeatedFlags

	fs.StringVar(&cfg.Executor, "executor", "fixed-vus", "load shape: fixed-vus, ramp, step, constant-arrival-rate")
	fs.IntVar(&cfg.VUs, "vus", 1, "number of virtual users (fixed-vus; also the scripting VU pool size for ramp/step)")
	fs.DurationVar(&cfg.Duration, "duration", 10*time.Second, "run duration (fixed-vus, constant-arrival-rate)")
	fs.StringVar(&stagesRaw, "stages", "", "comma-separated target:duration stages for ramp/step, e.g. 10:30s,0:10s")
	fs.Float64Var(&cfg.Rate, "rate", 0, "iterations per second (constant-arrival-rate)")
	fs.IntVar(&cfg.MaxVUs, "max-vus", 0, "max concurrent VUs (constant-arrival-rate)")
	fs.StringVar(&cfg.Target.URL, "target", "", "target URL or host:port (protocol-direct mode; omit if a scenario file is given)")
	fs.StringVar(&cfg.Protocol, "protocol", "", "http1, http2, grpc, or websocket (protocol-direct mode)")
	fs.StringVar(&method, "method", "", "HTTP verb, or the full gRPC method (e.g. /package.Service/Method)")
	fs.StringVar(&bodyRaw, "body", "", "request body")
	fs.BoolVar(&insecure, "insecure", false, "skip TLS certificate verification")
	fs.DurationVar(&cfg.Timeout, "timeout", 30*time.Second, "per-iteration timeout")
	fs.StringVar(&cfg.OutPath, "out", "", "also write a JSON summary to this path")
	fs.Var(headers, "header", "request header \"Key: Value\" (repeatable)")

	// FR-CLI-06: target allowlist and hard caps, on by default.
	fs.Var(&allowTargets, "allow-target", "additional host (or host:port) allowed without confirmation, besides localhost (repeatable)")
	fs.BoolVar(&cfg.Yes, "yes", false, "skip the confirmation prompt for a non-localhost, non-allowlisted target (required for non-interactive/MCP-driven runs)")
	cfg.Limits = safety.DefaultLimits
	fs.IntVar(&cfg.Limits.MaxVUs, "cap-vus", safety.DefaultLimits.MaxVUs, "hard cap on virtual users for this run")
	fs.DurationVar(&cfg.Limits.MaxDuration, "cap-duration", safety.DefaultLimits.MaxDuration, "hard cap on run duration")
	fs.Float64Var(&cfg.Limits.MaxRate, "cap-rate", safety.DefaultLimits.MaxRate, "hard cap on iterations/sec (constant-arrival-rate)")

	// FR-CLI-07: audit log.
	fs.StringVar(&cfg.Trigger, "trigger", "cli", "what started this run, recorded in the audit log (e.g. \"cli\", or \"mcp:<session-id>\")")
	fs.StringVar(&cfg.AuditLogPath, "audit-log", "", "path to the audit log (default: ~/.vegaload/audit.log)")

	// FR-RPT-01/02 and FR-CLI-05: the HTML report and output mode.
	fs.StringVar(&cfg.OutputMode, "output", "text", "output mode: text, json, or jsonl (FR-CLI-05's structured and streaming modes)")
	fs.StringVar(&cfg.ReportPath, "report", "", "path for the self-contained HTML report (default: vegaload-report-<timestamp>.html)")
	fs.BoolVar(&cfg.NoReport, "no-report", false, "skip writing the HTML report")
	fs.BoolVar(&cfg.NoOpen, "no-open", false, "don't auto-open the HTML report in the default browser")

	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: vegaload run [flags] [scenario-file]")
		fmt.Fprintln(fs.Output(), "Flags must come before the scenario file, e.g. \"vegaload run -vus 10 scenario.vl.js\" —")
		fmt.Fprintln(fs.Output(), "Go's flag parser stops at the first non-flag argument.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	switch cfg.OutputMode {
	case "text", "json", "jsonl":
	default:
		return nil, fmt.Errorf("-output %q: want text, json, or jsonl", cfg.OutputMode)
	}

	if fs.NArg() > 0 {
		cfg.ScenarioPath = fs.Arg(0)
	}
	cfg.AllowTargets = []string(allowTargets)
	cfg.Target.Method = method
	cfg.Target.Body = []byte(bodyRaw)
	cfg.Target.Headers = headers
	cfg.Target.InsecureSkipVerify = insecure

	if stagesRaw != "" {
		stages, err := parseStages(stagesRaw)
		if err != nil {
			return nil, fmt.Errorf("-stages: %w", err)
		}
		cfg.Stages = stages
	}

	if cfg.ScenarioPath == "" && (cfg.Target.URL == "" || cfg.Protocol == "") {
		return nil, fmt.Errorf("provide a scenario file, or both -target and -protocol")
	}
	if cfg.ScenarioPath != "" && (cfg.Target.URL != "" || cfg.Protocol != "") {
		return nil, fmt.Errorf("a scenario file and -target/-protocol are two different modes — see run.go's doc comment; use one, not both")
	}

	return cfg, nil
}

// parseStages parses "target:duration,target:duration,..." into
// engine.Stage values, e.g. "10:30s,20:1m,0:10s".
func parseStages(s string) ([]engine.Stage, error) {
	parts := strings.Split(s, ",")
	stages := make([]engine.Stage, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		targetStr, durStr, ok := strings.Cut(p, ":")
		if !ok {
			return nil, fmt.Errorf("stage %q must be target:duration (e.g. 10:30s)", p)
		}
		target, err := strconv.Atoi(strings.TrimSpace(targetStr))
		if err != nil {
			return nil, fmt.Errorf("stage %q: invalid target VU count: %w", p, err)
		}
		dur, err := time.ParseDuration(strings.TrimSpace(durStr))
		if err != nil {
			return nil, fmt.Errorf("stage %q: invalid duration: %w", p, err)
		}
		stages = append(stages, engine.Stage{Target: target, Duration: dur})
	}
	return stages, nil
}

// buildExecutor turns cfg's load-shape flags into an engine.Executor.
func buildExecutor(cfg *runConfig) (engine.Executor, error) {
	switch cfg.Executor {
	case "fixed-vus":
		return engine.FixedVUs{VUs: cfg.VUs, Dur: cfg.Duration}, nil
	case "ramp":
		if len(cfg.Stages) == 0 {
			return nil, fmt.Errorf("-executor ramp requires -stages")
		}
		return engine.Ramp{Stages: cfg.Stages}, nil
	case "step":
		if len(cfg.Stages) == 0 {
			return nil, fmt.Errorf("-executor step requires -stages")
		}
		return engine.Step{Stages: cfg.Stages}, nil
	case "constant-arrival-rate":
		if cfg.Rate <= 0 {
			return nil, fmt.Errorf("-executor constant-arrival-rate requires -rate > 0")
		}
		return engine.ConstantArrivalRate{Rate: cfg.Rate, Dur: cfg.Duration, MaxVUs: cfg.MaxVUs}, nil
	default:
		return nil, fmt.Errorf("unknown -executor %q (want fixed-vus, ramp, step, or constant-arrival-rate)", cfg.Executor)
	}
}

// concurrencyHint estimates how many VUs can be running cfg's executor
// at once, used to size the scripting VU pool (see vupool.go) — it does
// not need to be exact, only a reasonable upper bound, since the pool
// grows past it (closing the overflow on return) rather than blocking.
func concurrencyHint(cfg *runConfig) int {
	switch cfg.Executor {
	case "constant-arrival-rate":
		if cfg.MaxVUs > 0 {
			return cfg.MaxVUs
		}
		return 1
	case "ramp", "step":
		max := cfg.VUs
		for _, s := range cfg.Stages {
			if s.Target > max {
				max = s.Target
			}
		}
		return max
	default:
		return cfg.VUs
	}
}

// buildIteration returns the engine.IterationFunc for cfg (either
// protocol-direct or scripted) and a cleanup function the caller must
// run once the executor has finished.
func buildIteration(cfg *runConfig) (engine.IterationFunc, func() error, error) {
	if cfg.ScenarioPath != "" {
		return scriptedIteration(cfg, concurrencyHint(cfg))
	}
	return protocolIteration(cfg.Protocol, cfg.Target, cfg.Timeout)
}

// buildSafetyCheck returns the netapi.SafetyCheck every scripted VU's
// http/ws globals run before connecting — FR-CLI-08's per-call
// extension of FR-CLI-06's allowlist, built from the same
// cfg.AllowTargets/-yes a protocol-direct target is checked against in
// enforceSafety.
//
// It deliberately never prompts the way enforceSafety's interactive
// y/N confirmation does: a script can make an unbounded number of calls
// to hosts only it discovers at run time, so there's no single moment
// to pause the whole run for a human the way there is for one
// protocol-direct target chosen before the run starts. The approved
// design is a hard fail instead — a disallowed host always refuses the
// call unless -yes was passed for the whole run — so a scenario author
// who wants to hit something outside localhost/-allow-target either
// allowlists it up front or opts in to the whole run with -yes, the
// same two ways out enforceSafety already offers, minus the prompt.
func buildSafetyCheck(cfg *runConfig) netapi.SafetyCheck {
	return func(host string) error {
		if safety.IsAllowed(host, cfg.AllowTargets) {
			return nil
		}
		if cfg.Yes {
			return nil
		}
		return fmt.Errorf("host %q is not localhost or allowlisted (pass -yes, or add it with -allow-target)", host)
	}
}

// protocolIteration adapts one of the four protocol.Protocol drivers
// into an engine.IterationFunc: a failed Result (and a non-nil
// protocol.Result.Err) becomes a returned error, which is all the
// minimal Summary recorder (see internal/engine) distinguishes today —
// the richer per-call detail in protocol.Result (status code, byte
// counts) wires into a real report in Phase 1, not here.
func protocolIteration(name string, target protocol.Target, timeout time.Duration) (engine.IterationFunc, func() error, error) {
	var (
		driver protocol.Protocol
		err    error
	)
	switch name {
	case "http1":
		driver = http1.New(target, timeout)
	case "http2":
		driver, err = http2.New(target, timeout)
	case "grpc":
		driver, err = grpc.New(target, timeout)
	case "websocket":
		driver, err = websocket.New(target, timeout)
	default:
		return nil, nil, fmt.Errorf("unknown -protocol %q (want http1, http2, grpc, or websocket)", name)
	}
	if err != nil {
		return nil, nil, err
	}

	iter := func(ctx context.Context) error {
		res, err := driver.Do(ctx)
		if err != nil {
			return err
		}
		if !res.Success {
			if res.Err != nil {
				return res.Err
			}
			return fmt.Errorf("%s: request failed (status %d)", driver.Name(), res.StatusCode)
		}
		return nil
	}
	return iter, driver.Close, nil
}

// scriptedIteration loads cfg.ScenarioPath (as JavaScript/TypeScript or
// Python, decided by extension) and returns an engine.IterationFunc
// backed by a vuPool sized by concurrency — see vupool.go for why a
// pool, rather than one shared runtime, is needed here.
//
// Every VU it creates gets the same netapi.SafetyCheck (built once, via
// buildSafetyCheck) and cfg.Timeout, so FR-CLI-08's http/ws globals
// enforce FR-CLI-06's allowlist and respect the run's per-request
// timeout exactly as -target/-protocol mode's own driver does.
func scriptedIteration(cfg *runConfig, concurrency int) (engine.IterationFunc, func() error, error) {
	path := cfg.ScenarioPath
	check := buildSafetyCheck(cfg)

	if strings.ToLower(filepath.Ext(path)) == ".py" {
		script, err := python.Load(path)
		if err != nil {
			return nil, nil, err
		}
		pool := newVUPool(concurrency, func() (iterCloser, error) {
			return script.NewVU(check, cfg.Timeout)
		})
		// Validate the script once, up front, the same way
		// protocol-direct mode's New() fails fast on a bad target,
		// instead of only discovering a broken script on the first
		// iteration deep inside the executor.
		if _, err := pool.borrowAndRelease(); err != nil {
			return nil, nil, err
		}
		return pool.iteration, pool.Close, nil
	}

	script, err := js.Load(path)
	if err != nil {
		return nil, nil, err
	}
	pool := newVUPool(concurrency, func() (iterCloser, error) {
		return script.NewVU(check, cfg.Timeout)
	})
	if _, err := pool.borrowAndRelease(); err != nil {
		return nil, nil, err
	}
	return pool.iteration, pool.Close, nil
}

// runScenario runs cfg end to end: builds the executor and iteration
// function, runs them, and returns the result. It has no dependency on
// flag or os, so a test can call it directly. It records into a fresh
// report.Collector; use runScenarioWithCollector directly when the
// caller needs to observe progress while the run is still in flight
// (cmdRun's -output jsonl mode does this — see streamProgress below).
func runScenario(cfg *runConfig) (*report.Result, error) {
	return runScenarioWithCollector(cfg, report.NewCollector())
}

func runScenarioWithCollector(cfg *runConfig, collector *report.Collector) (*report.Result, error) {
	ex, err := buildExecutor(cfg)
	if err != nil {
		return nil, err
	}

	iter, closeFn, err := buildIteration(cfg)
	if err != nil {
		return nil, err
	}
	defer closeFn() //nolint:errcheck // best-effort cleanup; a close failure shouldn't mask the run's own result

	start := time.Now()
	if err := ex.Run(context.Background(), iter, collector); err != nil {
		return nil, err
	}
	elapsed := time.Since(start)

	return collector.Finish(ex.Name(), elapsed), nil
}

// enforceSafety implements FR-CLI-06 for cfg: it rejects a run whose
// shape exceeds the hard caps outright (no prompt — those are mistakes,
// not judgment calls), and for protocol-direct mode, requires either an
// allowlisted target or explicit confirmation (-yes, or an interactive
// y/N prompt on confirm) before continuing. confirm is a seam for
// testing; cmdRun passes promptConfirm, which reads a real y/N from in.
//
// A scripted run's own network calls are a separate, per-call check —
// buildSafetyCheck, run against every host a script's http/ws globals
// connect to as FR-CLI-08 makes those calls — since those hosts aren't
// known until the script actually runs, unlike -target here.
func enforceSafety(cfg *runConfig, in io.Reader, out io.Writer, confirm func(io.Reader, io.Writer, string) bool) error {
	rate := cfg.Rate
	if err := safety.CheckLimits(cfg.VUs, cfg.Duration, rate, cfg.Limits); err != nil {
		return err
	}
	// ramp/step can reach a higher VU count than -vus via -stages; check
	// that too, using the same concurrency estimate the scripting pool
	// is sized with.
	if peak := concurrencyHint(cfg); peak > cfg.VUs {
		if err := safety.CheckLimits(peak, cfg.Duration, rate, cfg.Limits); err != nil {
			return err
		}
	}

	if cfg.Target.URL == "" {
		return nil // scripted run: no protocol-direct target to check here -- see buildSafetyCheck for its own per-call gate
	}
	host, err := safety.TargetHost(cfg.Target.URL)
	if err != nil {
		return err
	}
	if safety.IsAllowed(host, cfg.AllowTargets) {
		return nil
	}
	if cfg.Yes {
		return nil
	}
	prompt := fmt.Sprintf("Target %q is not localhost or allowlisted. Continue? [y/N] ", cfg.Target.URL)
	if confirm(in, out, prompt) {
		return nil
	}
	return fmt.Errorf("refusing to run against %q without confirmation (pass -yes, or add it with -allow-target)", cfg.Target.URL)
}

// promptConfirm writes prompt to out and reads one line from in,
// treating "y" or "yes" (case-insensitive) as confirmation.
func promptConfirm(in io.Reader, out io.Writer, prompt string) bool {
	fmt.Fprint(out, prompt)
	line, _ := bufio.NewReader(in).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

func cmdRun(args []string) int {
	cfg, err := parseRunArgs(args)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		fmt.Fprintf(os.Stderr, "vegaload run: %v\n", err)
		return 2
	}

	if err := enforceSafety(cfg, os.Stdin, os.Stdout, promptConfirm); err != nil {
		fmt.Fprintf(os.Stderr, "vegaload run: %v\n", err)
		return 2
	}

	var result *report.Result
	switch cfg.OutputMode {
	case "jsonl":
		result, err = runJSONL(cfg, os.Stdout)
	default:
		if cfg.OutputMode == "text" {
			fmt.Printf("running %s (vus=%d, duration=%s)...\n", cfg.Executor, cfg.VUs, cfg.Duration)
		}
		result, err = runScenario(cfg)
	}
	recordAudit(cfg, result, err)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload run: %v\n", err)
		return 1
	}

	switch cfg.OutputMode {
	case "json":
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload run: encoding -output json: %v\n", err)
			return 1
		}
	case "text":
		printResult(os.Stdout, result)
	}

	if cfg.OutPath != "" {
		if err := report.WriteJSON(cfg.OutPath, result); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload run: writing -out: %v\n", err)
			return 1
		}
	}

	if !cfg.NoReport {
		if err := writeAndMaybeOpenReport(cfg, result); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload run: warning: %v\n", err)
			// A report-writing failure doesn't fail the run itself —
			// the load test's own result already happened.
		}
	}
	return 0
}

// runJSONL implements FR-CLI-05's streaming JSONL event mode: a
// "run_started" line, one "progress" line per second while the run is
// in flight (polling Collector.Counts — see its doc comment for why
// that's safe and cheap mid-run), and a final "run_finished" line
// carrying the full report.Result, the same shape -output json prints
// in one shot.
func runJSONL(cfg *runConfig, w io.Writer) (*report.Result, error) {
	enc := json.NewEncoder(w)
	emit := func(typ string, data any) {
		_ = enc.Encode(map[string]any{"type": typ, "data": data})
	}

	collector := report.NewCollector()
	emit("run_started", map[string]any{
		"executor": cfg.Executor, "vus": cfg.VUs, "duration_ns": cfg.Duration,
		"scenario_path": cfg.ScenarioPath, "target": cfg.Target.URL,
	})

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				total, failed := collector.Counts()
				emit("progress", map[string]any{"total": total, "failed": failed})
			case <-done:
				return
			}
		}
	}()

	result, err := runScenarioWithCollector(cfg, collector)
	close(done)
	if err != nil {
		emit("run_failed", map[string]any{"error": err.Error()})
		return nil, err
	}
	emit("run_finished", result)
	return result, nil
}

// writeAndMaybeOpenReport implements FR-RPT-01/02: write the
// self-contained HTML report, then (unless -no-open) best-effort open
// it in the default browser. Both steps are reported to the caller as
// one combined error so cmdRun can warn without failing the run.
func writeAndMaybeOpenReport(cfg *runConfig, result *report.Result) error {
	path := cfg.ReportPath
	if path == "" {
		path = fmt.Sprintf("vegaload-report-%s.html", time.Now().Format("20060102-150405"))
	}
	if err := report.WriteHTML(path, result); err != nil {
		return fmt.Errorf("writing HTML report: %w", err)
	}
	if cfg.OutputMode == "text" {
		fmt.Printf("report: %s\n", path)
	}
	if !cfg.NoOpen {
		if err := openBrowser(path); err != nil {
			return fmt.Errorf("opening report in browser (the file itself was written to %s): %w", path, err)
		}
	}
	return nil
}

// recordAudit implements FR-CLI-07. A failure to write the audit log is
// reported as a warning, not a run failure — the same best-effort
// treatment internal/audit's doc comment describes — since the run
// itself already happened and its own outcome is what matters most to
// the caller.
func recordAudit(cfg *runConfig, result *report.Result, runErr error) {
	entry := audit.Entry{
		Time:         time.Now(),
		Trigger:      cfg.Trigger,
		Target:       cfg.Target.URL,
		Protocol:     cfg.Protocol,
		ScenarioPath: cfg.ScenarioPath,
		Executor:     cfg.Executor,
		VUs:          cfg.VUs,
		Duration:     cfg.Duration,
		Rate:         cfg.Rate,
	}
	if runErr != nil {
		entry.Outcome = "error"
		entry.Error = runErr.Error()
	} else {
		entry.Outcome = "success"
		entry.Total = result.Total
		entry.Failed = result.Failed
	}
	if err := audit.Append(cfg.AuditLogPath, entry); err != nil {
		fmt.Fprintf(os.Stderr, "vegaload run: warning: could not write audit log: %v\n", err)
	}
}

func printResult(w io.Writer, r *report.Result) {
	fmt.Fprintf(w, "\n%s summary\n", r.Executor)
	fmt.Fprintf(w, "  elapsed:  %s\n", r.Elapsed)
	fmt.Fprintf(w, "  total:    %d\n", r.Total)
	fmt.Fprintf(w, "  failed:   %d\n", r.Failed)
	fmt.Fprintf(w, "  mean:     %s\n", r.Latency.Mean)
}
