// validate.go implements `vegaload validate` (FR-CLI-15): run a scenario
// once, with one user and one iteration, and say whether it works. It is
// the cheap step between writing a script and starting a real load test,
// so a typo or a wrong URL shows up in a second, not after a long run.
//
// validate makes real network calls, exactly as one iteration of `run`
// would. It uses the same target safety rules as `run` (FR-CLI-06) and
// writes an audit log entry (FR-CLI-07).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/vegaload/vegaload/internal/report"
	"github.com/vegaload/vegaload/internal/safety"
	"github.com/vegaload/vegaload/internal/secrets"
)

// validateResult is what `vegaload validate -output json` prints, and
// what the validate_scenario MCP tool returns.
type validateResult struct {
	Scenario string `json:"scenario"`
	// Valid is true when the script loaded and its one iteration ran
	// without an error. A failed check() does not make a scenario
	// invalid: the checks are listed so the author can see them.
	Valid bool `json:"valid"`
	// Stage says where an invalid scenario stopped: "load" (the script
	// could not be read, parsed or started, or its options are wrong),
	// "setup", "iteration" (it started, but the iteration returned an
	// error) or "teardown".
	Stage   string               `json:"stage,omitempty"`
	Error   string               `json:"error,omitempty"`
	Elapsed string               `json:"elapsed"`
	Checks  []report.CheckResult `json:"checks,omitempty"`

	elapsed time.Duration // for the audit log
}

func cmdValidate(args []string) int {
	return runValidate(args, os.Stdout, os.Stderr)
}

func runValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := &runConfig{Executor: "fixed-vus", VUs: 1, Limits: safety.DefaultLimits}
	var allowTargets repeatedFlags
	output := fs.String("output", "text", "output mode: text or json")
	fs.DurationVar(&cfg.Timeout, "timeout", 30*time.Second, "time limit for the one iteration")
	fs.Var(&allowTargets, "allow-target", "additional host (or host:port) allowed without confirmation, besides localhost (repeatable)")
	fs.BoolVar(&cfg.Yes, "yes", false, "allow hosts that are not localhost or allowlisted (needed for non-interactive and MCP-driven runs)")
	fs.StringVar(&cfg.Trigger, "trigger", "cli", "what started this run, recorded in the audit log")
	fs.StringVar(&cfg.AuditLogPath, "audit-log", "", "path to the audit log (default: ~/.vegaload/audit.log)")
	var scriptInputs scriptInputFlags
	scriptInputs.register(fs)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: vegaload validate [flags] <scenario-file>")
		fmt.Fprintln(fs.Output(), "Runs the scenario once, with one user and one iteration, and reports whether it works.")
		fmt.Fprintln(fs.Output(), "This makes real network calls, under the same host rules as \"vegaload run\".")
		fmt.Fprintln(fs.Output(), "Exit codes: 0 valid, 1 not valid, 2 bad usage. Flags go before the scenario file.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *output != "text" && *output != "json" {
		fmt.Fprintf(stderr, "vegaload validate: -output %q: want text or json\n", *output)
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "vegaload validate: provide exactly one scenario file")
		return 2
	}
	cfg.ScenarioPath = fs.Arg(0)
	cfg.AllowTargets = []string(allowTargets)
	if err := scriptInputs.resolve(cfg); err != nil {
		fmt.Fprintf(stderr, "vegaload validate: %v\n", err)
		return 2
	}

	res := validateScenario(cfg)
	recordValidateAudit(cfg, res)

	if *output == "json" {
		if err := json.NewEncoder(stdout).Encode(res); err != nil {
			fmt.Fprintf(stderr, "vegaload validate: encoding -output json: %v\n", err)
			return 1
		}
	} else {
		printValidate(stdout, res)
	}
	if !res.Valid {
		return 1
	}
	return 0
}

// validateScenario loads cfg's scenario and runs its setup, one iteration
// with what setup returned, and its teardown.
func validateScenario(cfg *runConfig) *validateResult {
	res := &validateResult{Scenario: cfg.ScenarioPath}
	collector := report.NewCollector()
	cfg.checkRecorder = collector
	cfg.stepRecorder = collector
	cfg.callRecorder = collector

	start := time.Now()
	finish := func(stage string, err error) *validateResult {
		res.elapsed = time.Since(start)
		res.Elapsed = res.elapsed.Round(time.Millisecond).String()
		res.Checks = collector.Finish("validate", time.Since(start)).Checks
		if err != nil {
			// FR-CLI-17: the error text can carry a secret the script read.
			res.Stage, res.Error = stage, secrets.Redact(err.Error())
			return res
		}
		res.Valid = true
		return res
	}

	iter, closeFn, sc, err := buildIteration(cfg)
	if err != nil {
		return finish("load", err)
	}
	defer closeFn() //nolint:errcheck // best-effort cleanup
	// The options must be ones run would accept. Flags are not checked here.
	if err := setScenarioOptions(&runConfig{}, sc.options, nil); err != nil {
		return finish("load", err)
	}
	if err := sc.setup(context.Background(), lifecycleTimeout(cfg)); err != nil {
		return finish("setup", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	iterErr := iter(ctx)
	// teardown runs after a failed iteration too, as it does after a run.
	tdErr := sc.teardown(lifecycleTimeout(cfg))
	if iterErr != nil {
		return finish("iteration", iterErr)
	}
	if tdErr != nil {
		return finish("teardown", tdErr)
	}
	return finish("", nil)
}

// recordValidateAudit writes validate's audit entry (FR-CLI-07). An
// invalid scenario is an "error" outcome with its message and no totals,
// the same as a failed run. A valid one ran exactly one iteration.
func recordValidateAudit(cfg *runConfig, res *validateResult) {
	cfg.Executor = "validate"
	cfg.Duration = res.elapsed
	audited := &report.Result{Executor: "validate", Total: 1}
	var runErr error
	if !res.Valid {
		runErr = fmt.Errorf("%s: %s", res.Stage, res.Error)
	}
	recordAudit("validate", cfg, audited, runErr)
}

func printValidate(w io.Writer, r *validateResult) {
	if r.Valid {
		fmt.Fprintf(w, "valid: %s ran one iteration in %s\n", r.Scenario, r.Elapsed)
	} else {
		fmt.Fprintf(w, "not valid: %s (%s)\n", r.Scenario, r.Stage)
		fmt.Fprintf(w, "  %s\n", r.Error)
	}
	if len(r.Checks) == 0 {
		return
	}
	fmt.Fprintln(w, "\nchecks")
	for _, c := range r.Checks {
		status := "PASS"
		if c.Fails > 0 {
			status = "FAIL"
		}
		fmt.Fprintf(w, "  %s  %s\n", status, c.Name)
	}
}
