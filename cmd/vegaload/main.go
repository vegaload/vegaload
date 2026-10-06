// Command vegaload is the VegaLoad CLI.
//
// Phase 0 wires together the engine, the four protocol drivers, and the
// two scripting runtimes behind commands including run, new, diagnose,
// and compare.
// See AGENTS.md for the module boundaries this command is built on top
// of, and run.go's doc comment for how a scenario file and a
// protocol-direct target relate to each other.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

// run is main's body, factored out so a test can drive it with an
// explicit argument list instead of the real os.Args.
func run(args []string) int {
	if len(args) < 1 {
		printUsage()
		return 1
	}

	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "new":
		return cmdNew(args[1:])
	case "diagnose":
		return cmdDiagnose(args[1:])
	case "compare":
		return cmdCompare(args[1:])
	case "mcp":
		return cmdMCP(args[1:])
	case "init":
		return cmdInit(args[1:])
	case "version":
		fmt.Printf("vegaload %s\n", version)
		return 0
	case "help", "-h", "--help":
		printUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "vegaload: unknown command %q\n\n", args[0])
		printUsage()
		return 1
	}
}

func printUsage() {
	fmt.Println(`vegaload — a thin, open-source load testing tool

Usage:
  vegaload <command> [arguments]

Commands:
  run        Run a load test: a scenario file, or a protocol-direct target
  new        Scaffold a starter scenario file
  diagnose   Print environment info, or explain a report's results
  compare    Diff a candidate JSON report against a baseline
  mcp serve  Run an MCP server over stdio for agent-native use (Claude Code, Cursor, ...)
  mcp eval   Run the versioned MCP tool-calling eval suite against this binary
  init       Register the MCP server and skill bundles for this project
  version    Print the vegaload version
  help       Show this help text

Every command supports -output text (default), json, or (run only) jsonl.
A "run" also writes a self-contained HTML report by default — see
"vegaload run -h" for -report/-no-report/-no-open.

Run "vegaload run -h" for the run command's flags.
Run "vegaload compare -h" for baseline-vs-candidate flags.
Run "vegaload mcp serve -h" for the MCP server's tools.
Run "vegaload mcp eval -output json" for machine-readable eval results.
Run "vegaload init -h" for what init writes.`)
}
