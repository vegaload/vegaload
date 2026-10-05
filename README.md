# VegaLoad

VegaLoad is a thin, open-source load testing tool: a single static binary
with a scriptable core engine, four protocol drivers (HTTP/1.1, HTTP/2,
gRPC, WebSocket), and a self-contained HTML report — no server, no account,
no telemetry.

It's also agent-native: `vegaload init` registers VegaLoad as an MCP server
for Claude Code, Cursor, or any other MCP-capable agent host, so an agent
can run tests, read results, and explain failures through the same CLI
commands a human would type. It is never agent-*mandatory* — every feature
works from a plain terminal with nothing else installed. See `AGENTS.md` for
the design principle behind that split.

## Status

Phase 0 (core engine, CLI, protocols, scripting) and Phase 1 (the HTML/JSON
report) are done. Phase 2 (the MCP server, skill bundles, and a versioned
eval suite for the MCP tools) is also done — this README's walkthrough
covers all three. A Harness RT bridge (`--move-to-harness`) is intentionally
out of scope for now.

## Install

Not yet published to a package registry. For now, build from source (Go
1.22+):

```
git clone https://github.com/vegaload/vegaload
cd vegaload
go build -o vegaload ./cmd/vegaload
./vegaload version
```

`go install github.com/vegaload/vegaload/cmd/vegaload@latest` also works
once the repository is public. A scratch-based Docker image builds from the
included `Dockerfile` (`docker build .`); it carries nothing but the binary
and CA certificates, so Python-scripted scenarios (which shell out to a
local `python3`) need a different base image — see the `Dockerfile`'s
comment.

## Getting started

Everything below uses [`examples/sample-app`](./examples/sample-app), a
small widgets service with injected latency and a ~3% failure rate on
creates, served over HTTP/1.1, HTTP/2, WebSocket, and gRPC, built
specifically so this walkthrough has something real to point `vegaload` at.
Start it in its own terminal first:

```
cd examples/sample-app
go run .
```

Leave it running, and do the rest from a second terminal at the repository
root.

### 1. Run a load test, no scenario file needed

Protocol-direct mode drives a target straight from CLI flags — useful for a
quick check, or for an agent that just got a URL and doesn't need to write a
script first:

```
./vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 -vus 10 -duration 30s
```

This runs 10 virtual users against the sample app for 30 seconds, prints a
summary, and writes (and opens) a self-contained HTML report — one file,
with the full latency distribution and a requests/errors-over-time chart,
nothing else to host.

### 2. Or write a scenario file

```
./vegaload new my-scenario
```

scaffolds `my-scenario.vl.js` (JavaScript by default; `-python` for a
`.py` scenario shelling out to a local `python3`). VegaLoad's scripting
runtimes can't make network calls yet (see `AGENTS.md`), so today a scenario
file is for your own per-iteration logic against VegaLoad's VU pool and
executor shapes (fixed-VU, ramp, step, constant-arrival-rate) —
[`examples/scenarios/smoke.vl.js`](./examples/scenarios/smoke.vl.js) is a
minimal one:

```
./vegaload run -vus 5 -duration 5s examples/scenarios/smoke.vl.js
```

(flags before the scenario file — `vegaload`'s flag parser stops at the
first non-flag argument; run `vegaload run -h` for the full flag list.)

To load test an actual HTTP target today, use protocol-direct mode (step 1)
instead — or generate a runbook of protocol-direct commands from an OpenAPI
spec:

```
./vegaload new -from-openapi examples/sample-app/openapi.json sample-app
```

writes `sample-app.vegaload-plan.md`: one ready-to-run `vegaload run`
command per endpoint the spec declares.

### 3. Explain a run's results

Keep a run's JSON summary alongside its HTML report with `-out`:

```
./vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -vus 10 -duration 30s -out baseline.json
./vegaload diagnose baseline.json
```

`diagnose` reports the failure rate, flags a latency long tail (p99 more
than 5x p50), and says whether failures were concentrated in one period or
spread out — plus suggested pass/fail thresholds (a p95 ceiling, an error
rate ceiling) derived from that same run, for writing into a CI gate. Add a
bring-your-own-LLM connector for a plain-English narrative on top of the
rule-based findings:

```
export VEGALOAD_LLM_PROVIDER=openai       # or anthropic, ollama
export VEGALOAD_LLM_API_KEY=sk-...
./vegaload diagnose baseline.json
```

Nothing about a run or its results leaves your machine unless you configure
this yourself (`-no-llm` skips narration even if it's configured).

### 4. Compare a later run against the baseline

After another run with `-out candidate.json`:

```
./vegaload compare baseline.json candidate.json
```

Prints deltas (error rate, latency percentiles, totals, overall RPS) and
exits non-zero if error rate or p95 latency got worse. Optional slack:

```
./vegaload compare -error-rate-delta 0.01 -p95-ratio 1.2 \
  baseline.json candidate.json
```

Checked-in fixtures under `examples/scenarios/compare/` show both an ok and
a regressed pair without needing a live target.

### 5. Hand all of this to an agent

```
./vegaload init
```

writes four things into the current project, each skippable if already
present: a Claude Code skill bundle (`.claude/skills/vegaload`), a Cursor
rules file (`.cursor/rules/vegaload.mdc`), and an MCP server entry merged
into both `.mcp.json` and `.cursor/mcp.json`, pointing at this same compiled
binary running `vegaload mcp serve`. Open the project in Claude Code or
Cursor afterward and the agent has seven tools — `create_scenario`,
`run_test`, `get_results`, `suggest_thresholds`, `diagnose_failure`,
`compare_reports`, `generate_from_spec` — each one calling the exact CLI
command shown above and parsing its `-output json` result; there is no
agent-only path that skips the CLI.

`vegaload mcp eval` runs a versioned, non-LLM suite of {tool call, expected
outcome} cases against those same tools directly — the thing to run in
CI after upgrading, to check the tool layer itself still behaves, independent
of any model's tool-picking behavior:

```
./vegaload mcp eval
```

See [`examples/scenarios/README.md`](./examples/scenarios/README.md) for
this same walkthrough as a standalone, copy-pasteable script.

## Command reference

| Command                 | What it does                                                          |
|--------------------------|------------------------------------------------------------------------|
| `vegaload run`           | Run a load test: a scenario file, or a protocol-direct target         |
| `vegaload new`           | Scaffold a starter scenario file, or a runbook from an OpenAPI spec    |
| `vegaload diagnose`      | Print environment info, or explain a report's results                |
| `vegaload mcp serve`     | Run an MCP server over stdio for agent-native use                     |
| `vegaload mcp eval`      | Run the versioned MCP tool-calling eval suite against this binary     |
| `vegaload init`          | Register the MCP server and skill bundles for the current project     |

Every command supports `-output text` (default), `json`, or (`run` only)
`jsonl`. Run `vegaload <command> -h` for its full flag list, or `vegaload
help` for the top-level summary.

## License

Apache-2.0. See `LICENSE`.
