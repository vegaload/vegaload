package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/vegaload/vegaload/internal/scripting/js"
	"github.com/vegaload/vegaload/internal/scripting/netapi"
	"github.com/vegaload/vegaload/internal/scripting/python"
)

// scenarioVU is what js.VU and python.VU both offer a run.
type scenarioVU interface {
	iterCloser
	SetCheckRecorder(netapi.CheckRecorder)
	SetStepRecorder(netapi.StepRecorder)
	SetCallRecorder(netapi.CallRecorder)
	HasSetup() bool
	HasTeardown() bool
	Setup(ctx context.Context) (json.RawMessage, error)
	Teardown(ctx context.Context, data json.RawMessage) error
	SetData(data json.RawMessage) error
	Options() (map[string]any, error)
}

// scenarioVUs loads cfg's scenario file and returns a function that makes
// a fresh VU of it, with no recorders. check is run before every call the
// VU makes.
func scenarioVUs(cfg *runConfig, check netapi.SafetyCheck) (func() (scenarioVU, error), error) {
	path := cfg.ScenarioPath
	if strings.ToLower(filepath.Ext(path)) == ".py" {
		script, err := python.Load(path)
		if err != nil {
			return nil, err
		}
		opts := []python.Option{python.WithInputs(cfg.Inputs)}
		if cfg.NoConnectionReuse {
			opts = append(opts, python.WithNoConnectionReuse())
		}
		if cfg.NoURLGrouping {
			opts = append(opts, python.WithNoURLGrouping())
		}
		if cfg.NoCookies {
			opts = append(opts, python.WithNoCookies())
		}
		return func() (scenarioVU, error) {
			vu, err := script.NewVU(check, cfg.Timeout, opts...)
			if err != nil {
				return nil, err
			}
			return vu, nil
		}, nil
	}

	script, err := js.Load(path)
	if err != nil {
		return nil, err
	}
	opts := []js.Option{js.WithInputs(cfg.Inputs)}
	if cfg.NoConnectionReuse {
		opts = append(opts, js.WithNoConnectionReuse())
	}
	if cfg.NoURLGrouping {
		opts = append(opts, js.WithNoURLGrouping())
	}
	if cfg.NoCookies {
		opts = append(opts, js.WithNoCookies())
	}
	return func() (scenarioVU, error) {
		vu, err := script.NewVU(check, cfg.Timeout, opts...)
		if err != nil {
			return nil, err
		}
		return vu, nil
	}, nil
}

// scenario is a scenario file loaded for one run: the pool of VUs that run
// its iterations, and its setup and teardown.
//
// setup runs once, before the load, on a VU of its own that records
// nothing, so its calls are not part of the run's numbers. Every VU gets
// its own copy of what setup returned. teardown runs once after the load,
// on the same VU as setup, and only when setup worked.
type scenario struct {
	pool  *vuPool
	newVU func() (scenarioVU, error)

	hasSetup, hasTeardown bool
	// options is what the script's options declare, read from the first VU.
	options map[string]any

	// data is what setup returned. It is set before the load starts, and
	// only read after that.
	data json.RawMessage
	// life is the VU setup ran on, kept for teardown.
	life scenarioVU
}

// loadScenario loads cfg's scenario and makes its first VU, so that a
// broken script fails here, before the load starts, the same way a bad
// -target fails when its driver is made.
func loadScenario(cfg *runConfig, concurrency int) (*scenario, error) {
	newVU, err := scenarioVUs(cfg, buildSafetyCheck(cfg))
	if err != nil {
		return nil, err
	}
	sc := &scenario{newVU: newVU}
	sc.pool = newVUPool(concurrency, func() (iterCloser, error) {
		vu, err := newVU()
		if err != nil {
			return nil, err
		}
		vu.SetCheckRecorder(cfg.checkRecorder)
		vu.SetStepRecorder(cfg.stepRecorder)
		vu.SetCallRecorder(cfg.callRecorder)
		if sc.data != nil {
			if err := vu.SetData(sc.data); err != nil {
				_ = vu.Close()
				return nil, err
			}
		}
		return vu, nil
	})
	first, err := sc.pool.borrowAndRelease()
	if err != nil {
		return nil, err
	}
	vu := first.(scenarioVU)
	sc.hasSetup, sc.hasTeardown = vu.HasSetup(), vu.HasTeardown()
	sc.options, err = vu.Options()
	if err != nil {
		_ = sc.Close()
		return nil, err
	}
	return sc, nil
}

// lifecycleTimeout is how long setup and teardown each may take: a minute,
// or -timeout when that is longer.
func lifecycleTimeout(cfg *runConfig) time.Duration {
	return max(time.Minute, cfg.Timeout)
}

// setup runs the script's setup, if it has one, and gives what it returned
// to every VU. ctx ends it early (Ctrl-C). A failed setup is an error, and
// teardown will not run.
func (sc *scenario) setup(ctx context.Context, timeout time.Duration) error {
	if !sc.hasSetup && !sc.hasTeardown {
		return nil
	}
	life, err := sc.newVU()
	if err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	if !sc.hasSetup {
		sc.life = life
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, err := life.Setup(ctx)
	if err != nil {
		_ = life.Close()
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("setup did not finish in %s", timeout)
		}
		return fmt.Errorf("setup: %w", err)
	}
	sc.life, sc.data = life, data
	sc.pool.mu.Lock()
	defer sc.pool.mu.Unlock()
	for _, vu := range sc.pool.all {
		if err := vu.(scenarioVU).SetData(data); err != nil {
			return fmt.Errorf("setup: %w", err)
		}
	}
	return nil
}

// teardown runs the script's teardown once, if it has one and setup
// worked. It has its own time limit, so it runs after a run that was
// stopped early too.
func (sc *scenario) teardown(timeout time.Duration) error {
	if sc.life == nil || !sc.hasTeardown {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := sc.life.Teardown(ctx, sc.data); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("teardown did not finish in %s", timeout)
		}
		return fmt.Errorf("teardown: %w", err)
	}
	return nil
}

// Close releases every VU of the scenario.
func (sc *scenario) Close() error {
	err := sc.pool.Close()
	if sc.life != nil {
		if lerr := sc.life.Close(); err == nil {
			err = lerr
		}
		sc.life = nil
	}
	return err
}
