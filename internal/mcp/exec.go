package mcp

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// RunCLI runs the vegaload binary at exePath with args and returns its
// stdout. This is the only way any tool in this package touches
// vegaload's actual behavior: it shells out to the same binary a human
// would run from their own shell, which is what structurally guarantees
// AGENTS.md's module-boundary rule ("the MCP layer must not call
// core-engine functions directly") rather than relying on code-review
// discipline to keep it true. exePath is resolved once at server
// startup (cmd/vegaload/mcp.go calls os.Executable()) and threaded
// through every tool's closure — this function takes it as a parameter,
// rather than calling os.Executable() itself, purely so tests can point
// it at a stand-in script instead of the real binary.
//
// A non-zero exit is reported as an error that includes stderr, since
// every vegaload subcommand writes its own error messages there.
func RunCLI(ctx context.Context, exePath string, args ...string) ([]byte, error) {
	stdout, _, err := RunCLISplit(ctx, exePath, args...)
	return stdout, err
}

// RunCLISplit is RunCLI that also returns stderr, kept apart from stdout. A
// command's stdout is the data a tool parses (its -output json); its stderr
// is notes for a person, such as `run`'s "live view: URL" line. Mixing the two
// would let a note break the parsing, so a tool that wants the notes asks for
// them here and returns them beside its result (see WithNotes).
func RunCLISplit(ctx context.Context, exePath string, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, exePath, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), errb.Bytes(), fmt.Errorf("vegaload %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), errb.Bytes(), nil
}
