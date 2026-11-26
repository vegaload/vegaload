//go:build !windows

package main

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A real Ctrl-C stops a run gracefully: the load ends, teardown runs once
// with the setup data, and the exit code is 130.
func TestRun_CtrlCRunsTeardown(t *testing.T) {
	for _, lang := range []string{"js", "py"} {
		t.Run(lang, func(t *testing.T) {
			ls := newLifeServer(t)
			var path string
			if lang == "py" {
				skipWithoutPython(t)
				path = writeTemp(t, "life.py", lifeScenarioPy(ls.URL, "/it"))
			} else {
				path = writeTemp(t, "life.vl.js", lifeScenarioJS(ls.URL, "/it"))
			}
			// The handler is installed before setup runs, so the signal is
			// sent only once the load is under way.
			go func() {
				deadline := time.Now().Add(10 * time.Second)
				for ls.count("/it") == 0 && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				_ = syscall.Kill(syscall.Getpid(), syscall.SIGINT)
			}()
			dir := t.TempDir()
			start := time.Now()
			code, _, stderr := captureCmd(t, func() int {
				return cmdRun([]string{"-no-live", "-no-report", "-audit-log", filepath.Join(dir, "a.log"),
					"-executor", "constant-arrival-rate", "-rate", "20", "-max-vus", "2", "-duration", "30s", path})
			})
			if code != 130 {
				t.Fatalf("exit %d, want 130: %s", code, stderr)
			}
			if took := time.Since(start); took > 15*time.Second {
				t.Fatalf("the stopped run took %v", took)
			}
			checkLifecycle(t, ls, nil)
		})
	}
}
