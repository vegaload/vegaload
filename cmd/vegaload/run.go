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
	"github.com/vegaload/vegaload/internal/compare"
	"github.com/vegaload/vegaload/internal/engine"
	"github.com/vegaload/vegaload/internal/inputs"
	"github.com/vegaload/vegaload/internal/live"
	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/protocol/xfercommon"
	"github.com/vegaload/vegaload/internal/report"
	"github.com/vegaload/vegaload/internal/safety"
	"github.com/vegaload/vegaload/internal/scripting/js"
	"github.com/vegaload/vegaload/internal/scripting/netapi"
	"github.com/vegaload/vegaload/internal/scripting/python"
	"github.com/vegaload/vegaload/internal/secrets"
	"github.com/vegaload/vegaload/internal/threshold"
)

// exitThresholdsBreached is the exit code of a run that completed but
// broke one of its pass/fail thresholds (FR-CLI-11). It is distinct from
// 1 (the run itself failed) and 2 (bad usage) so CI and agents can tell
// "the test did not meet its bar" apart from "the tool broke".
const exitThresholdsBreached = 3

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

// optionFlags accumulates repeated -opt key=value flags into a map. The
// keys are driver specific; see protocol.Target.Options.
type optionFlags map[string]string

func (o optionFlags) String() string { return "" }

func (o optionFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	k = strings.TrimSpace(k)
	if !ok || k == "" {
		return fmt.Errorf("-opt %q must be in key=value form", v)
	}
	o[k] = val
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

	// checkRecorder is where scripted VUs report check() outcomes
	// (FR-CLI-12). runScenarioWithCollector sets it to the run's
	// collector; it is not a flag.
	checkRecorder netapi.CheckRecorder
	// stepRecorder is where scripted VUs report step() timings (FR-CLI-18).
	stepRecorder netapi.StepRecorder
	// callRecorder is where scripted VUs and -protocol mode report each call.
	callRecorder netapi.CallRecorder

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

	// FR-CLI-11: pass/fail thresholds judged on the finished run.
	Thresholds []threshold.Threshold

	// FR-CLI-13: stop the run as soon as a threshold is broken beyond
	// recovery. AbortGrace is the warm-up in which statistics are not
	// judged. It counts only when AbortGraceSet is true (-abort-grace was
	// given), and then zero means no warm-up. Otherwise the warm-up is
	// automatic (see autoGrace).
	AbortOnBreach bool
	AbortGrace    time.Duration
	AbortGraceSet bool

	// FR-CLI-14: judge the finished run against a baseline report the
	// user supplies. Baseline is loaded when the flags are parsed, so a
	// bad path fails before any load is sent.
	BaselinePath  string
	MaxRegression float64 // percent
	Baseline      *report.Result

	// FR-RPT-05: a JUnit XML file, and the GitHub Actions job summary.
	JUnitPath     string
	NoStepSummary bool
	// StepSummaryPath is where the Markdown summary is appended. It
	// comes from GITHUB_STEP_SUMMARY, so it is empty outside Actions.
	StepSummaryPath string

	// FR-CLI-17: data files and environment variables scripts can read.
	// Inputs is nil when none were given.
	Inputs netapi.Inputs

	// DataSets, DataPick and DataPlan are -data for -target and -protocol.
	// DataPlan is nil when the run has no data file, and one call then does
	// no extra work. A scenario keeps using Inputs instead.
	DataSets map[string]*inputs.Set
	DataPick string
	DataPlan *dataPlan

	// Stream holds the -stream-*, -run-id, -label and live view flags
	// (docs/streaming.md). stream is what they produced for this run; it is
	// nil when the run is not streamed, and then nothing streams.
	Stream runStream
	stream *streaming
}

// parseRunArgs parses run's flags into a runConfig. It returns a usage
// error (rather than exiting the process) so it can be exercised by
// tests and so cmdRun controls its own exit code.
func parseRunArgs(args []string) (*runConfig, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfg := &runConfig{}
	headers := headerFlags{}
	options := optionFlags{}

	var stagesRaw string
	var bodyRaw string
	var method string
	var insecure bool
	var allowTargets repeatedFlags
	var thresholdExprs repeatedFlags
	var thresholdsFile string

	fs.StringVar(&cfg.Executor, "executor", "fixed-vus", "load shape: fixed-vus, ramp, step, constant-arrival-rate")
	fs.IntVar(&cfg.VUs, "vus", 1, "number of virtual users (fixed-vus; also the scripting VU pool size for ramp/step)")
	fs.DurationVar(&cfg.Duration, "duration", 10*time.Second, "run duration (fixed-vus, constant-arrival-rate)")
	fs.StringVar(&stagesRaw, "stages", "", "comma-separated target:duration stages for ramp/step, e.g. 10:30s,0:10s")
	fs.Float64Var(&cfg.Rate, "rate", 0, "iterations per second (constant-arrival-rate)")
	fs.IntVar(&cfg.MaxVUs, "max-vus", 0, "max concurrent VUs (constant-arrival-rate)")
	fs.StringVar(&cfg.Target.URL, "target", "", "target URL or host:port (protocol-direct mode; omit if a scenario file is given)")
	fs.StringVar(&cfg.Protocol, "protocol", "", protocolList()+" (protocol-direct mode)")
	fs.StringVar(&method, "method", "", "HTTP verb, or the full gRPC method (e.g. /package.Service/Method)")
	fs.StringVar(&bodyRaw, "body", "", "request body")
	fs.BoolVar(&insecure, "insecure", false, "skip TLS certificate verification")
	fs.DurationVar(&cfg.Timeout, "timeout", 30*time.Second, "per-iteration timeout")
	fs.StringVar(&cfg.OutPath, "out", "", "also write a JSON summary to this path")
	fs.Var(headers, "header", "request header \"Key: Value\" (repeatable)")
	fs.Var(options, "opt", "driver option key=value, e.g. -opt read=64 (repeatable; each protocol lists its own keys)")

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
	fs.BoolVar(&cfg.NoOpen, "no-open", false, "don't open a browser tab: neither the HTML report nor the live view page (also VEGALOAD_OPEN=off). The live view still streams. When the live view page opens, the HTML report is written but not opened")

	// docs/streaming.md: stream the run, and the live view.
	cfg.Stream.register(fs)

	// FR-CLI-11: pass/fail thresholds.
	fs.Var(&thresholdExprs, "threshold", "pass/fail threshold on the run, e.g. \"p95 < 300ms\", \"p95{step=\\\"login\\\"} < 300ms\" or \"p95{protocol=\\\"ftp\\\"} < 500ms\" (repeatable; metrics: "+strings.Join(threshold.MetricNames(), ", ")+"). A breach exits 3")
	fs.BoolVar(&cfg.AbortOnBreach, "abort-on-breach", false, "stop the run as soon as a threshold is broken beyond recovery, and exit 3. Needs -threshold or -thresholds. Thresholds like \"failed < 5\" stop it at once; statistics like p95 or error_rate must stay broken for three seconds, after a warm-up. Thresholds that need the whole run (rps, total >=) wait for the end")
	fs.DurationVar(&cfg.AbortGrace, "abort-grace", 0, "with -abort-on-breach: the warm-up in which p95, error_rate and other statistics are not judged (default: 5s, or a quarter of the run if shorter; 0 means no warm-up)")
	fs.StringVar(&thresholdsFile, "thresholds", "", "JSON file of thresholds (a list of {name, metric, operator, value}, with optional step, protocol and op, or the output of `vegaload diagnose -output json`)")

	// FR-CLI-14: baseline gate.
	fs.StringVar(&cfg.BaselinePath, "baseline", "", "JSON report of an earlier run (from -out) to compare this run with. If p95 or the error rate is worse by more than -max-regression, the run exits 3")
	fs.Float64Var(&cfg.MaxRegression, "max-regression", 0, "with -baseline: how many percent worse than the baseline p95 and error rate may be (default 0: any increase fails)")

	// FR-RPT-05: JUnit XML and the GitHub Actions job summary.
	fs.StringVar(&cfg.JUnitPath, "junit", "", "also write a JUnit XML file to this path: each threshold, check and the baseline gate is one test case, so CI systems show the results natively")
	fs.BoolVar(&cfg.NoStepSummary, "no-step-summary", false, "inside GitHub Actions, do not append the run's summary to the job summary")

	// FR-CLI-17: data files and environment variables for scripts.
	var scriptInputs scriptInputFlags
	scriptInputs.register(fs)

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
	if len(options) > 0 {
		cfg.Target.Options = options
	}
	cfg.Target.InsecureSkipVerify = insecure

	if stagesRaw != "" {
		stages, err := parseStages(stagesRaw)
		if err != nil {
			return nil, fmt.Errorf("-stages: %w", err)
		}
		cfg.Stages = stages
	}

	if thresholdsFile != "" {
		ts, err := threshold.LoadFile(thresholdsFile)
		if err != nil {
			return nil, fmt.Errorf("-thresholds: %w", err)
		}
		cfg.Thresholds = append(cfg.Thresholds, ts...)
	}
	if len(thresholdExprs) > 0 {
		ts, err := threshold.ParseAll([]string(thresholdExprs))
		if err != nil {
			return nil, fmt.Errorf("-threshold: %w", err)
		}
		cfg.Thresholds = append(cfg.Thresholds, ts...)
	}

	if cfg.AbortOnBreach && len(cfg.Thresholds) == 0 {
		return nil, fmt.Errorf("-abort-on-breach needs at least one -threshold or -thresholds")
	}
	if cfg.AbortGrace < 0 {
		return nil, fmt.Errorf("-abort-grace %s: want zero or more", cfg.AbortGrace)
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "abort-grace" {
			cfg.AbortGraceSet = true
		}
	})
	if cfg.AbortGraceSet && !cfg.AbortOnBreach {
		return nil, fmt.Errorf("-abort-grace needs -abort-on-breach")
	}

	if !cfg.NoStepSummary {
		cfg.StepSummaryPath = os.Getenv("GITHUB_STEP_SUMMARY")
	}

	maxRegressionSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "max-regression" {
			maxRegressionSet = true
		}
	})
	if maxRegressionSet && cfg.BaselinePath == "" {
		return nil, fmt.Errorf("-max-regression needs -baseline")
	}
	if cfg.MaxRegression < 0 {
		return nil, fmt.Errorf("-max-regression %g: want zero or more percent", cfg.MaxRegression)
	}
	if cfg.BaselinePath != "" {
		base, err := loadReportJSON(cfg.BaselinePath)
		if err != nil {
			return nil, fmt.Errorf("-baseline: %w", err)
		}
		if base.Total == 0 {
			return nil, fmt.Errorf("-baseline: %s has no requests, so there is nothing to compare with", cfg.BaselinePath)
		}
		cfg.Baseline = base
	}

	if err := scriptInputs.resolve(cfg); err != nil {
		return nil, err
	}

	if cfg.ScenarioPath == "" && (cfg.Target.URL == "" || cfg.Protocol == "") {
		return nil, fmt.Errorf("provide a scenario file, or both -target and -protocol")
	}
	if cfg.ScenarioPath != "" && (cfg.Target.URL != "" || cfg.Protocol != "") {
		return nil, fmt.Errorf("a scenario file and -target/-protocol are two different modes — see run.go's doc comment; use one, not both")
	}
	if cfg.ScenarioPath != "" && len(cfg.Target.Options) > 0 {
		return nil, fmt.Errorf("-opt sets options for a protocol driver, so it needs -target and -protocol, not a scenario file")
	}
	if cfg.Protocol != "" {
		if _, ok := findDriver(cfg.Protocol); !ok {
			return nil, fmt.Errorf("%s", unknownProtocolMessage(cfg.Protocol))
		}
	}
	if len(cfg.DataSets) > 0 {
		plan, err := prepareProtocolData(cfg)
		if err != nil {
			return nil, err
		}
		cfg.DataPlan = plan
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
	return protocolIteration(cfg.Protocol, cfg.Target, cfg.Timeout, cfg.callRecorder, cfg.DataPlan)
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
		return safety.HostRefusal(host)
	}
}

// protocolIteration adapts one of the four protocol.Protocol drivers
// into an engine.IterationFunc: a failed Result (and a non-nil
// protocol.Result.Err) becomes a returned error, which is all the
// minimal Summary recorder (see internal/engine) distinguishes today —
// the richer per-call detail in protocol.Result (status code, byte
// counts) wires into a real report in Phase 1, not here.
func protocolIteration(name string, target protocol.Target, timeout time.Duration, calls netapi.CallRecorder, plan *dataPlan) (engine.IterationFunc, func() error, error) {
	spec, ok := findDriver(name)
	if !ok {
		return nil, nil, fmt.Errorf("%s", unknownProtocolMessage(name))
	}
	// A driver says which -opt keys it knows. Any other key is a typo or
	// meant for another protocol, so refuse it now instead of ignoring it.
	if err := target.RejectUnknownOptions(spec.options...); err != nil {
		return nil, nil, fmt.Errorf("-opt for -protocol %s: %w", name, err)
	}
	buildTarget := target
	if plan != nil {
		// The first row is filled only so New accepts the config. It is not
		// run, and the row counter stays where it is, so the first real call
		// still starts at that row.
		var err error
		buildTarget, err = plan.filledTarget(target, plan.sample())
		if err != nil {
			return nil, nil, err
		}
	}
	driver, err := spec.build(buildTarget, timeout)
	if err != nil {
		return nil, nil, err
	}

	op := protocolOp(driver.Name(), target.Options)
	if plan == nil {
		iter := func(ctx context.Context) error {
			start := time.Now()
			res, timing, timed, err := doProtocol(driver, ctx)
			if recordDriverCall(calls, ctx, driver.Name(), op, time.Since(start), res, err) && timed {
				netapi.RecordXferMeasures(calls, driver.Name(), op, res.Success, netapi.XferTimingFrom(timing))
			}
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

	iter := func(ctx context.Context) error {
		start := time.Now()
		res, timing, timed, err := dataCall(ctx, spec, driver, plan, timeout)
		if recordDriverCall(calls, ctx, driver.Name(), op, time.Since(start), res, err) && timed {
			netapi.RecordXferMeasures(calls, driver.Name(), op, res.Success, netapi.XferTimingFrom(timing))
		}
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

// dataCall draws one row, fills the placeholders, and runs that one call.
// The driver Call returns shares the connection, so it is not closed here.
func dataCall(ctx context.Context, spec driverSpec, base protocol.Protocol, plan *dataPlan, timeout time.Duration) (protocol.Result, xfercommon.Timing, bool, error) {
	opts, body, err := plan.fill(plan.draw())
	if err != nil {
		return protocol.Result{}, xfercommon.Timing{}, false, err
	}
	next, err := spec.perCall(base, opts, body, timeout)
	if err != nil {
		return protocol.Result{}, xfercommon.Timing{}, false, err
	}
	return doProtocol(next, ctx)
}

// protocolOp is the operation name for one -protocol call. It is the mode
// option only when that mode is in the driver's own list. Anything else
// uses the driver's own name, so a raw option string is never stored.
func protocolOp(protocolName string, opts map[string]string) string {
	mode := ""
	if opts != nil {
		mode = opts["mode"]
	}
	spec, ok := findDriver(protocolName)
	if mode != "" && ok && spec.hasMode(mode) {
		return mode
	}
	return protocolName
}

// timedDriver is a driver that can report the phase times of the call it
// just made. FTP and SFTP do. The times are not a field of protocol.Result,
// because every other driver would then carry a type it does not use.
type timedDriver interface {
	DoTimed(ctx context.Context) (protocol.Result, xfercommon.Timing, error)
}

func doProtocol(driver protocol.Protocol, ctx context.Context) (protocol.Result, xfercommon.Timing, bool, error) {
	if td, ok := driver.(timedDriver); ok {
		res, timing, err := td.DoTimed(ctx)
		return res, timing, true, err
	}
	res, err := driver.Do(ctx)
	return res, xfercommon.Timing{}, false, err
}

func recordDriverCall(rec netapi.CallRecorder, ctx context.Context, protocolName, op string, d time.Duration, res protocol.Result, err error) bool {
	if rec == nil || driverRefused(res) {
		return false
	}
	if err != nil {
		if netapi.RunEnded(ctx) {
			return false
		}
		rec.RecordCall(protocolName, op, d, true, res.BytesSent, res.BytesReceived)
		return true
	}
	if !res.Success && netapi.RunEnded(ctx) {
		return false
	}
	rec.RecordCall(protocolName, op, d, !res.Success, res.BytesSent, res.BytesReceived)
	return true
}

// driverRefused is a call the safety rules or a missing write flag stopped
// before any bytes moved. It is a script or option error, not a latency.
func driverRefused(res protocol.Result) bool {
	return res.Refused
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
			vu, err := script.NewVU(check, cfg.Timeout, python.WithInputs(cfg.Inputs))
			if err != nil {
				return nil, err
			}
			vu.SetCheckRecorder(cfg.checkRecorder)
			vu.SetStepRecorder(cfg.stepRecorder)
			vu.SetCallRecorder(cfg.callRecorder)
			return vu, nil
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
		vu, err := script.NewVU(check, cfg.Timeout, js.WithInputs(cfg.Inputs))
		if err != nil {
			return nil, err
		}
		vu.SetCheckRecorder(cfg.checkRecorder)
		vu.SetStepRecorder(cfg.stepRecorder)
		vu.SetCallRecorder(cfg.callRecorder)
		return vu, nil
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

	// Scripts report their check() outcomes straight to the collector
	// (FR-CLI-12).
	cfg.checkRecorder = collector
	cfg.stepRecorder = collector
	cfg.callRecorder = collector

	// A streamed run says it has started before the scenario is loaded, so a
	// scenario that cannot be loaded is a run.failed after a run.started.
	var feed *pointFeed
	if cfg.stream != nil {
		feed = cfg.stream.begin(cfg, ex, collector)
		// Every path out of here, an error included, ends the points.
		defer feed.finish()
	}

	iter, closeFn, err := buildIteration(cfg)
	if err != nil {
		return nil, err
	}
	defer closeFn() //nolint:errcheck // best-effort cleanup; a close failure shouldn't mask the run's own result

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	// FR-CLI-13: a watcher looks at the run as it goes, and cancels ctx
	// when a threshold is broken beyond recovery.
	var aborted *report.AbortInfo
	var watcherDone chan struct{}
	stopWatcher := func() {}
	if cfg.AbortOnBreach {
		grace := cfg.AbortGrace
		if !cfg.AbortGraceSet {
			grace = autoGrace(ex.Duration())
		}
		w := newBreachWatcher(cfg.Thresholds, collector, ex.Name(), start, grace)
		stop := make(chan struct{})
		watcherDone = make(chan struct{})
		go func() {
			defer close(watcherDone)
			aborted = w.watch(stop, cancel)
		}()
		stopWatcher = func() { close(stop); <-watcherDone }
	}
	runErr := ex.Run(ctx, iter, collector)
	stopWatcher()
	if runErr != nil {
		return nil, runErr
	}
	elapsed := time.Since(start)

	result := collector.Finish(ex.Name(), elapsed)
	applyThresholds(cfg, result)
	applyAbort(result, aborted)
	applyBaseline(cfg, result)
	applyDataFiles(cfg, result)
	return result, nil
}

// applyDataFiles records the data file name and the row count. A run
// without -data is left untouched, so its JSON stays the same.
func applyDataFiles(cfg *runConfig, result *report.Result) {
	if cfg.DataPlan == nil || len(cfg.DataPlan.files) == 0 {
		return
	}
	result.DataFiles = cfg.DataPlan.files
}

// applyThresholds judges cfg's thresholds against a finished run and
// records the verdicts on the result, so the text summary, the JSON and
// JSONL output, the HTML report and the MCP run_test tool all carry the
// same verdicts. A run without thresholds is left untouched.
func applyThresholds(cfg *runConfig, result *report.Result) {
	if len(cfg.Thresholds) == 0 {
		return
	}
	result.Thresholds = threshold.Evaluate(cfg.Thresholds, result)
	passed := threshold.AllPassed(result.Thresholds)
	result.ThresholdsPassed = &passed
}

// applyBaseline judges a finished run against cfg's baseline (FR-CLI-14)
// and records the verdict on the result, so the text summary, JSON, HTML
// report and the MCP run_test tool all carry it. A run without a baseline
// is left untouched.
func applyBaseline(cfg *runConfig, result *report.Result) {
	if cfg.Baseline == nil {
		return
	}
	result.Baseline = compare.Gate(cfg.Baseline, result, cfg.BaselinePath, cfg.MaxRegression)
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

	if cfg.AbortOnBreach {
		abortNote(os.Stderr, cfg.Thresholds)
	}

	// The stream and the live view, when there is one. A destination that
	// breaks a rule stops the run before it starts.
	st, err := cfg.Stream.setup(cfg.NoOpen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload run: %v\n", err)
		return 2
	}
	var out runOutcome
	if st != nil {
		cfg.stream = st
		for _, w := range st.warnings {
			fmt.Fprintln(os.Stderr, w)
		}
		st.watchSignals()
	}
	code := executeRun(cfg, &out)
	st.end(cfg, &out, code)
	return code
}

// executeRun is the run itself: the load test, its outputs and its gates. It
// returns the process exit code. out gets the result, or why there is none, so
// that the stream can end with the right event.
func executeRun(cfg *runConfig, out *runOutcome) int {
	var err error
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
	recordAudit("run", cfg, result, err)
	if err != nil {
		out.fail(err)
		fmt.Fprintf(os.Stderr, "vegaload run: %s\n", secrets.Redact(err.Error()))
		return 1
	}
	out.result = result

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

	// FR-RPT-05. A failure here is a warning, like the HTML report: the
	// load test itself already happened, and its exit code is the gate.
	if cfg.JUnitPath != "" {
		if err := report.WriteJUnit(cfg.JUnitPath, result); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload run: warning: writing -junit: %v\n", err)
		}
	}
	if cfg.StepSummaryPath != "" {
		if err := report.AppendStepSummary(cfg.StepSummaryPath, result); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload run: warning: writing the GitHub job summary: %v\n", err)
		}
	}

	if !cfg.NoReport {
		if err := writeAndMaybeOpenReport(cfg, result); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload run: warning: %v\n", err)
			// A report-writing failure doesn't fail the run itself —
			// the load test's own result already happened.
		}
	}

	// FR-CLI-11: a run that completed but broke a threshold exits 3. The
	// reason goes to stderr in every output mode, so stdout stays clean
	// for -output json and jsonl.
	// FR-CLI-14: a run that completed but is worse than its baseline by
	// more than -max-regression also exits 3. Both reasons are printed
	// when both gates fail.
	if failures := gateFailures(result); len(failures) > 0 {
		for _, f := range failures {
			fmt.Fprintf(os.Stderr, "vegaload run: %s\n", f)
		}
		return exitThresholdsBreached
	}
	return 0
}

// gateFailures lists why a finished run fails its gates: a breached
// threshold (FR-CLI-11) and a baseline it is worse than (FR-CLI-14), one
// line each. It is empty when the run passed every gate.
func gateFailures(result *report.Result) []string {
	var out []string
	if result.ThresholdsPassed != nil && !*result.ThresholdsPassed {
		out = append(out, "thresholds breached: "+strings.Join(breachedNames(result), "; "))
	}
	if b := result.Baseline; b != nil && !b.Passed {
		out = append(out, fmt.Sprintf("worse than the baseline %s: %s", b.Path, strings.Join(b.Notes, "; ")))
	}
	return out
}

// breachedNames lists the thresholds a run broke, with what was observed.
func breachedNames(result *report.Result) []string {
	var out []string
	for _, t := range result.Thresholds {
		if !t.Passed {
			out = append(out, fmt.Sprintf("%s (observed %s)", t.Name, t.Observed))
		}
	}
	return out
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

// opensReport says whether the HTML report is opened in the browser when it
// is written. It is not with -no-open or VEGALOAD_OPEN=off (which stop every
// browser tab), when the live view is showing the run (its page is the tab),
// or for a run an agent started (-trigger mcp...), which has no one at the
// screen to want a second tab.
func (c *runConfig) opensReport() bool {
	if c.NoOpen || (c.stream != nil && c.stream.liveActive) || strings.HasPrefix(c.Trigger, "mcp") {
		return false
	}
	return live.OpenEnvAllows()
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
	if cfg.opensReport() {
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
func recordAudit(command string, cfg *runConfig, result *report.Result, runErr error) {
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
		entry.Error = secrets.Redact(runErr.Error()) // FR-CLI-17: a script's error text can carry a secret it read
	} else {
		entry.Outcome = "success"
		entry.Total = result.Total
		entry.Failed = result.Failed
		entry.ThresholdsPassed = result.ThresholdsPassed
		if result.Baseline != nil {
			entry.Baseline = result.Baseline.Path
			entry.BaselinePassed = &result.Baseline.Passed
		}
	}
	for _, t := range cfg.Thresholds {
		entry.Thresholds = append(entry.Thresholds, t.Expression())
	}
	if cfg.stream != nil {
		entry.Stream = cfg.stream.audit // kinds and hosts, never a path, query, id or token
	}
	if cfg.DataPlan != nil {
		for _, f := range cfg.DataPlan.files {
			entry.DataFiles = append(entry.DataFiles, audit.DataFile{Name: f.Name, Rows: f.Rows})
		}
	}
	if err := audit.Append(cfg.AuditLogPath, entry); err != nil {
		fmt.Fprintf(os.Stderr, "vegaload %s: warning: could not write audit log: %v\n", command, err)
	}
}

func printResult(w io.Writer, r *report.Result) {
	fmt.Fprintf(w, "\n%s summary\n", r.Executor)
	fmt.Fprintf(w, "  elapsed:  %s\n", r.Elapsed)
	fmt.Fprintf(w, "  total:    %d\n", r.Total)
	fmt.Fprintf(w, "  failed:   %d\n", r.Failed)
	fmt.Fprintf(w, "  mean:     %s\n", r.Latency.Mean)
	if a := r.Aborted; a != nil {
		fmt.Fprintf(w, "  ABORTED:  stopped at %s because %s was broken (observed %s)\n", a.At.Round(time.Millisecond), a.Threshold, a.Observed)
	}
	printChecks(w, r)
	printSteps(w, r)
	printDataFiles(w, r)
	printProtocols(w, r)
	printThresholds(w, r)
	printBaseline(w, r)
}

// printBaseline writes the baseline block of the text summary, if the run
// had a baseline (FR-CLI-14).
func printBaseline(w io.Writer, r *report.Result) {
	b := r.Baseline
	if b == nil {
		return
	}
	verdict := "PASS"
	if !b.Passed {
		verdict = "FAIL"
	}
	fmt.Fprintf(w, "\nbaseline %s (max regression %g%%)  %s\n", b.Path, b.MaxRegressionPercent, verdict)
	for _, m := range b.Metrics {
		status := "PASS"
		if m.Regressed {
			status = "FAIL"
		}
		fmt.Fprintf(w, "  %s  %s  baseline %s  now %s\n", status, m.Name, formatMetric(m.Baseline, m.Unit), formatMetric(m.Candidate, m.Unit))
	}
	for _, n := range b.Notes {
		fmt.Fprintf(w, "  - %s\n", n)
	}
}

// printDataFiles writes the data files a protocol run read, if it had any.
func printDataFiles(w io.Writer, r *report.Result) {
	if len(r.DataFiles) == 0 {
		return
	}
	fmt.Fprintln(w, "\ndata files")
	for _, f := range r.DataFiles {
		fmt.Fprintf(w, "  %s  %d rows\n", f.Name, f.Rows)
	}
}

// printChecks writes the checks block of the text summary, if the
// scenario made any checks (FR-CLI-12).
func printChecks(w io.Writer, r *report.Result) {
	if len(r.Checks) == 0 {
		return
	}
	rate, _ := r.CheckRate()
	fmt.Fprintf(w, "\nchecks (%.2f%% passed)\n", rate*100)
	for _, c := range r.Checks {
		status := "PASS"
		if c.Fails > 0 {
			status = "FAIL"
		}
		pct := 100 * float64(c.Passes) / float64(c.Passes+c.Fails)
		fmt.Fprintf(w, "  %s  %s  %.2f%%  (%d passed, %d failed)\n", status, c.Name, pct, c.Passes, c.Fails)
	}
}

// printSteps writes the per-step block of the text summary, if the
// scenario named any steps (FR-CLI-18).
func printSteps(w io.Writer, r *report.Result) {
	if len(r.Steps) == 0 {
		return
	}
	fmt.Fprintln(w, "\nsteps")
	for _, st := range r.Steps {
		status := "PASS"
		if st.Failed > 0 {
			status = "FAIL"
		}
		fmt.Fprintf(w, "  %s  %s  %d runs, %d failed (%.2f%%)  p50 %s  p95 %s  max %s\n",
			status, st.Name, st.Total, st.Failed, st.ErrorRate*100,
			st.Latency.P50.Round(time.Microsecond), st.Latency.P95.Round(time.Microsecond), st.Latency.Max.Round(time.Microsecond))
	}
}

// printProtocols writes the per-protocol block. A plain HTTP run does not
// show it, so that summary stays the same as before.
func printProtocols(w io.Writer, r *report.Result) {
	if !report.ShowProtocols(r) {
		return
	}
	fmt.Fprintln(w, "\nprotocols")
	for _, p := range r.Protocols {
		printCallLine(w, "", p.Name, p.Total, p.Failed, p.ErrorRate, p.Latency, p.BytesSent, p.BytesReceived)
		for _, op := range p.Operations {
			printCallLine(w, "  ", op.Name, op.Total, op.Failed, op.ErrorRate, op.Latency, op.BytesSent, op.BytesReceived)
		}
		for _, m := range p.Measures {
			fmt.Fprintf(w, "    %s  %d  p50 %s  p95 %s  max %s\n",
				m.Name, m.Count, m.Latency.P50.Round(time.Microsecond), m.Latency.P95.Round(time.Microsecond), m.Latency.Max.Round(time.Microsecond))
		}
	}
}

func printCallLine(w io.Writer, indent, name string, total, failed int64, rate float64, lat report.Latency, sent, received int64) {
	status := "PASS"
	if failed > 0 {
		status = "FAIL"
	}
	fmt.Fprintf(w, "  %s%s  %s  %d calls, %d failed (%.2f%%)  p50 %s  p95 %s  max %s  sent %s  received %s\n",
		indent, status, name, total, failed, rate*100,
		lat.P50.Round(time.Microsecond), lat.P95.Round(time.Microsecond), lat.Max.Round(time.Microsecond),
		report.FormatByteCount(sent), report.FormatByteCount(received))
}

// printThresholds writes the thresholds block of the text summary, if
// the run had any thresholds.
func printThresholds(w io.Writer, r *report.Result) {
	if len(r.Thresholds) == 0 {
		return
	}
	fmt.Fprintln(w, "\nthresholds")
	for _, t := range r.Thresholds {
		status := "PASS"
		if !t.Passed {
			status = "FAIL"
		}
		fmt.Fprintf(w, "  %s  %s  (observed %s)\n", status, t.Name, t.Observed)
	}
	if r.ThresholdsPassed != nil && *r.ThresholdsPassed {
		fmt.Fprintln(w, "  all thresholds passed")
	} else {
		fmt.Fprintln(w, "  thresholds breached")
	}
}
