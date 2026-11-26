package main

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/vegaload/vegaload/internal/engine"
	"github.com/vegaload/vegaload/internal/threshold"
)

// scenarioOption is one key a scenario's options may set, and the run flag
// it stands for. A flag given on the command line wins over the option.
type scenarioOption struct {
	key   string
	flags []string
	apply func(cfg *runConfig, v any) error
}

var scenarioOptions = []scenarioOption{
	{"executor", []string{"executor"}, func(cfg *runConfig, v any) error {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("want a string, like \"ramp\", not %v", v)
		}
		cfg.Executor = s
		return nil
	}},
	{"vus", []string{"vus"}, func(cfg *runConfig, v any) error {
		n, err := wholeNumber(v)
		cfg.VUs = n
		return err
	}},
	{"duration", []string{"duration"}, func(cfg *runConfig, v any) error {
		d, err := optionDuration(v)
		cfg.Duration = d
		return err
	}},
	{"stages", []string{"stages"}, func(cfg *runConfig, v any) error {
		st, err := optionStages(v)
		cfg.Stages = st
		return err
	}},
	{"rate", []string{"rate"}, func(cfg *runConfig, v any) error {
		n, ok := v.(float64)
		if !ok || n < 0 {
			return fmt.Errorf("want iterations per second, like 20, not %v", v)
		}
		cfg.Rate = n
		return nil
	}},
	{"maxVUs", []string{"max-vus"}, func(cfg *runConfig, v any) error {
		n, err := wholeNumber(v)
		cfg.MaxVUs = n
		return err
	}},
	{"timeout", []string{"timeout"}, func(cfg *runConfig, v any) error {
		d, err := optionDuration(v)
		cfg.Timeout = d
		return err
	}},
	{"thresholds", []string{"threshold", "thresholds"}, func(cfg *runConfig, v any) error {
		items, ok := v.([]any)
		if !ok {
			return fmt.Errorf("want a list of thresholds, like [\"p95 < 300ms\"], not %v", v)
		}
		exprs := make([]string, len(items))
		for i, item := range items {
			s, ok := item.(string)
			if !ok {
				return fmt.Errorf("want a list of thresholds, like [\"p95 < 300ms\"], not %v", item)
			}
			exprs[i] = s
		}
		ts, err := threshold.ParseAll(exprs)
		if err != nil {
			return err
		}
		cfg.Thresholds = ts
		return nil
	}},
}

// scenarioOptionKeys lists the keys options may have, for an error.
func scenarioOptionKeys() string {
	keys := make([]string, len(scenarioOptions))
	for i, o := range scenarioOptions {
		keys[i] = o.key
	}
	return strings.Join(keys, ", ")
}

// optionKey matches a key the way both languages may write it: maxVUs in
// JavaScript, max_vus in Python.
func optionKey(k string) string {
	return strings.ToLower(strings.ReplaceAll(k, "_", ""))
}

// applyScenarioOptions reads the options cfg's scenario declares (export
// const options in JavaScript and TypeScript, a module-level options dict
// in Python) and sets each one whose flag is not in given. A script that
// cannot be loaded is left to the run, which reports why.
func applyScenarioOptions(cfg *runConfig, given map[string]bool) error {
	opts, err := readScenarioOptions(cfg)
	if err != nil {
		return err
	}
	return setScenarioOptions(cfg, opts, given)
}

// readScenarioOptions runs the scenario's top-level code once, on a VU
// that may make no calls, and returns its options.
func readScenarioOptions(cfg *runConfig) (map[string]any, error) {
	newVU, err := scenarioVUs(cfg, func(string) error {
		return errors.New("no calls while the options are read")
	})
	if err != nil {
		return nil, nil
	}
	vu, err := newVU()
	if err != nil {
		return nil, nil
	}
	defer vu.Close() //nolint:errcheck // nothing ran on it
	return vu.Options()
}

// setScenarioOptions sets cfg from opts, skipping a key whose flag is in
// given. An unknown key, or a value of the wrong kind, is an error.
func setScenarioOptions(cfg *runConfig, opts map[string]any, given map[string]bool) error {
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var opt *scenarioOption
		for i := range scenarioOptions {
			if optionKey(scenarioOptions[i].key) == optionKey(k) {
				opt = &scenarioOptions[i]
			}
		}
		if opt == nil {
			return fmt.Errorf("options: unknown key %q (allowed: %s)", k, scenarioOptionKeys())
		}
		flagGiven := false
		for _, f := range opt.flags {
			flagGiven = flagGiven || given[f]
		}
		if flagGiven || opts[k] == nil {
			continue
		}
		if err := opt.apply(cfg, opts[k]); err != nil {
			return fmt.Errorf("options.%s: %w", k, err)
		}
	}
	return nil
}

func wholeNumber(v any) (int, error) {
	n, ok := v.(float64)
	if !ok || n != math.Trunc(n) || n < 0 || n > math.MaxInt32 {
		return 0, fmt.Errorf("want a whole number, like 10, not %v", v)
	}
	return int(n), nil
}

func optionDuration(v any) (time.Duration, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("want a duration, like \"30s\" or \"2m\", not %v", v)
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("want a duration, like \"30s\" or \"2m\", not %q", s)
	}
	return d, nil
}

// optionStages reads stages as a list, [{target: 10, duration: "30s"}], or
// as the -stages text, "10:30s,0:10s".
func optionStages(v any) ([]engine.Stage, error) {
	if s, ok := v.(string); ok {
		return parseStages(s)
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("want a list like [{target: 10, duration: \"30s\"}], not %v", v)
	}
	stages := make([]engine.Stage, 0, len(items))
	for i, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("stage %d: want {target: 10, duration: \"30s\"}, not %v", i+1, item)
		}
		for k := range m {
			if k != "target" && k != "duration" {
				return nil, fmt.Errorf("stage %d: unknown key %q (allowed: target, duration)", i+1, k)
			}
		}
		target, err := wholeNumber(m["target"])
		if err != nil {
			return nil, fmt.Errorf("stage %d: target: %w", i+1, err)
		}
		d, err := optionDuration(m["duration"])
		if err != nil {
			return nil, fmt.Errorf("stage %d: duration: %w", i+1, err)
		}
		stages = append(stages, engine.Stage{Target: target, Duration: d})
	}
	return stages, nil
}
