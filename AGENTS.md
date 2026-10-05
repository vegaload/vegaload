# AGENTS.md — VegaLoad

This file tells any AI coding agent (Claude Code, Cursor, or similar) how to work in this repository. Read this before making any change.

## What this project is

VegaLoad is a thin, open-source load testing tool. It plays the same role for Harness RT's load testing that LitmusChaos plays for chaos engineering: a simple tool people adopt on their own, with a smooth path into the enterprise product later.

Full requirements live in the PRD, not here. This file covers how to work in the code, not what the product does.

Licence: Apache-2.0.

## The one design principle that overrides everything else

VegaLoad is agent-native. It is not agent-mandatory.

This means three things. Follow all three on every change:

1. **Every test is a real file, never chat state.** A test an agent creates must be written to disk as an actual scenario file in the user's repo. Never hold a test only in conversation memory.
2. **The CLI always works alone.** Every feature must work from a plain terminal, with no agent present. If a feature only works through an agent, that is a bug.
3. **MCP tools mirror CLI commands.** Every MCP tool must call the same underlying CLI command a human can type by hand. Do not build a richer, agent-only path. That means building two products instead of one.

If a proposed change breaks any of these three rules, stop and flag it instead of proceeding.

## Language and runtime

- Core engine: Go. Single static binary. Cross-compiles to Linux, macOS, Windows, and a scratch Docker image.
- Scripting: JavaScript/TypeScript, run through an embedded Go JS interpreter (Goja), the same approach k6 uses. No Node.js dependency for this path.
- Python scripting is a second-class, optional path. It shells out to a local `python3` process. It requires Python installed on the user's machine. Never claim this path is dependency-free.
- The MCP server is also Go, built as a subcommand (`vegaload mcp serve`), not a separate Node.js process. This keeps the single-binary story intact.

## Module boundaries

Keep these modules separate. Do not let one reach into another's internals.

- **Core engine**: the VU scheduler and load-shape executors (fixed-VU, ramp, step, constant-arrival-rate). Knows nothing about MCP, reporting, or Harness RT.
- **Protocol drivers**: HTTP/1.1, HTTP/2, gRPC, WebSocket. Each implements a shared `Protocol` interface. Adding a new protocol should never require editing the core engine.
- **Output/reporting**: the self-contained HTML report, plus optional JSON/Parquet export. Implements a shared `Output` interface.
- **MCP layer**: calls the same CLI commands a human would run, then parses their output. It must not call core-engine functions directly. If the MCP layer needs new data, add a CLI flag or output mode first, then have MCP use it.
- **Harness bridge** (`--move-to-harness`): talks to the Harness RT API. Lives in its own package. The core engine must have zero awareness that Harness RT exists.

When in doubt about where new code belongs, put it in the smallest module that needs it, not in core.

## Conventions

- Format Go code with `gofmt` before committing. No exceptions.
- Write tests alongside the code they test (`_test.go`), not in a separate top-level test tree.
- Every new CLI flag needs a corresponding line in `vegaload --help` output and in the docs. Don't ship an undocumented flag.
- Never name the underlying scripting engine (Goja) in user-facing CLI output, error messages, or docs. Describe VegaLoad by the languages it supports: JavaScript, TypeScript, Python. This matches the existing Harness messaging rule.
- Commit messages: one line, present tense, describing what changed and why, not a log of commands run.

## Commands an agent should know

- `vegaload run <file>` — run a scenario.
- `vegaload new --from-openapi <spec>` — generate a starter scenario from an OpenAPI spec.
- `vegaload diagnose <report>` — explain a failed run.
- `vegaload compare <baseline.json> <candidate.json>` — diff a candidate report against a baseline; exits non-zero on regression.
- `vegaload mcp serve` — start the MCP server (stdio by default).
- `vegaload --move-to-harness <test-name|all>` — push a test to Harness RT.
- `go test ./...` — run the test suite. Run this before proposing any change as finished.

## What not to do

- Do not introduce YAML as a test-authoring format. Tests are JavaScript/TypeScript or Python code, not config.
- Do not add a feature to the MCP layer that has no CLI equivalent.
- Do not add Harness RT–specific logic to the core engine, the protocol drivers, or the output layer.
- Do not add telemetry or any network call that was not explicitly requested. See NFR-05 in the PRD: nothing leaves the local machine unless the user runs `--move-to-harness` or configures a bring-your-own-LLM connector themselves.

## Where the real requirements live

The PRD is the source of truth for what to build and why. This file is the source of truth for how to build it. If the two ever conflict, the PRD wins, and this file should be updated to match.
