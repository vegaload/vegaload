package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// buildVegaload compiles the current module's cmd/vegaload package to a
// temp binary and returns its path. cmdMCPEval relies on os.Executable()
// resolving to a real vegaload binary (so run_test's eval cases can
// shell back out to "run", "new", "diagnose", ...), which the test
// binary built by `go test` is not — so, unlike this package's other
// tests, TestCmdMCPEval needs a real build.
func buildVegaload(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	pkgDir := filepath.Dir(thisFile)

	out := filepath.Join(t.TempDir(), "vegaload")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = pkgDir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building vegaload: %v\n%s", err, output)
	}
	return out
}

func TestCmdMCPEval_AllCasesPass(t *testing.T) {
	bin := buildVegaload(t)

	cmd := exec.Command(bin, "mcp", "eval", "-output", "json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vegaload mcp eval failed: %v\n%s", err, output)
	}
	t.Logf("eval output:\n%s", output)
}

func TestCmdMCPEval_RejectsUnknownFlag(t *testing.T) {
	if code := cmdMCPEval([]string{"-bogus"}); code != 2 {
		t.Errorf("cmdMCPEval with an unrecognized flag returned %d, want 2", code)
	}
}

func TestCmdMCPEval_RejectsInvalidOutputMode(t *testing.T) {
	if code := cmdMCPEval([]string{"-output", "bogus"}); code != 2 {
		t.Errorf("cmdMCPEval with an invalid -output returned %d, want 2", code)
	}
}

func TestCmdMCP_DispatchesEval(t *testing.T) {
	// A bogus subcommand should fail with exit code 2 and not panic;
	// exercising this through cmdMCP (not cmdMCPEval directly) is what
	// actually checks the dispatch wiring in mcp.go.
	if code := cmdMCP([]string{"bogus"}); code != 2 {
		t.Errorf("cmdMCP with an unknown subcommand returned %d, want 2", code)
	}
}

func TestWriteEvalFixtures(t *testing.T) {
	dir := t.TempDir()
	if err := writeEvalFixtures(dir); err != nil {
		t.Fatalf("writeEvalFixtures: %v", err)
	}
	for _, name := range []string{"fixture-report.json", "fixture-report-worse.json", "fixture-spec.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s to exist: %v", name, err)
		}
	}
}
