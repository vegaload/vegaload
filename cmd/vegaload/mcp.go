// mcp.go implements `vegaload mcp serve`, FR-MCP-02's stdio-transport
// MCP server. It is intentionally thin: internal/mcp holds the actual
// JSON-RPC engine and tool implementations, every one of which shells
// out back to this same compiled binary (see internal/mcp.RunCLI) —
// this file's only real job is resolving that binary's own path once
// and handing it to internal/mcp.NewTools, then running the server on
// stdin/stdout until the client disconnects.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/vegaload/vegaload/internal/mcp"
)

// cmdMCP dispatches vegaload's "mcp" command group: "serve" runs
// FR-MCP-02's stdio server, "eval" runs FR-MCP-04's versioned tool-
// calling eval suite against this same binary.
func cmdMCP(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: vegaload mcp <serve|eval>")
		return 2
	}
	switch args[0] {
	case "serve":
		return cmdMCPServe(args[1:])
	case "eval":
		return cmdMCPEval(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "vegaload mcp: unknown subcommand %q\n", args[0])
		fmt.Fprintln(os.Stderr, "Usage: vegaload mcp <serve|eval>")
		return 2
	}
}

func cmdMCPServe(args []string) int {
	fs := flag.NewFlagSet("mcp serve", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: vegaload mcp serve")
		fmt.Fprintln(fs.Output(), "Runs an MCP server over stdio, exposing VegaLoad's CLI commands as tools")
		fmt.Fprintln(fs.Output(), "(create_scenario, run_test, get_results, suggest_thresholds, diagnose_failure, compare_reports, generate_from_spec)")
		fmt.Fprintln(fs.Output(), "for an MCP-aware agent (Claude Code, Cursor, etc.) to call directly.")
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload mcp serve: locating the vegaload binary: %v\n", err)
		return 1
	}

	server := mcp.NewServer()
	for _, t := range mcp.NewTools(exePath) {
		server.Register(t)
	}

	if err := server.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "vegaload mcp serve: %v\n", err)
		return 1
	}
	return 0
}
