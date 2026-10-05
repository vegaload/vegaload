---
name: vegaload
description: Use VegaLoad to load-test an HTTP/gRPC/WebSocket API — create a scenario, run a load test with a fixed, ramped, or stepped VU shape, read back results, get suggested pass/fail thresholds, diagnose a run's failures, compare a run against a baseline, or generate a runbook from an OpenAPI spec. Use this whenever the user asks to load test, stress test, soak test, or benchmark an API's performance.
---

# VegaLoad load testing

VegaLoad is an open-source load testing tool. `vegaload init` registered its MCP
server for this project, which exposes seven tools — every one of them a thin
wrapper around the same `vegaload` CLI commands you could run yourself from a
terminal, so nothing here does anything a human running `vegaload` by hand
couldn't also do.

## Tools

- **create_scenario** — scaffold a new scenario file (`vegaload new`). Only
  useful for scripted scenarios with non-network per-iteration logic; VegaLoad's
  scripting runtimes can't make network calls yet (see generate_from_spec below
  for the network case).
- **run_test** — run a load test, either a scenario file or a protocol-direct
  target (`target` + `protocol`, e.g. `http1`). Pick an `executor`: `fixed-vus`
  (steady concurrency), `ramp` (stages that climb then fall), `step` (stages
  that jump), or `constant-arrival-rate` (fixed throughput via `rate`).
  Returns a `report.Result` (total/failed/error_rate/latency percentiles/a
  per-second time series) and, unless `no_report` is set, a path to a
  self-contained HTML report you can open or point the user at.
  **Safety gate**: a target that isn't localhost and isn't in `allow_targets`
  is refused unless `yes: true` is set — there's no terminal here to prompt a
  human for confirmation, so a deliberate `yes` (or an explicit allowlist) is
  required before VegaLoad sends real load at someone else's server. Default
  to a short run (`duration: "10s"`, modest `vus`) unless the user asks for
  more, and always confirm with the user before pointing load at anything
  that isn't localhost or a target they explicitly named.
- **get_results** — read back a report JSON file you already have a path for
  (from `run_test`'s `report_path`, or a file the user points you at).
- **suggest_thresholds** — given a baseline run's report, get a suggested p95
  latency ceiling and error-rate ceiling for gating future runs.
- **diagnose_failure** — given a report, get rule-based findings (failure
  rate, latency long tail, whether failures were transient or spread across
  the run) and, if the user has configured `VEGALOAD_LLM_PROVIDER` in this
  MCP server's environment, a plain-English narrative.
- **compare_reports** — diff a candidate JSON report against a baseline JSON
  report (`vegaload compare`). Returns metric deltas and whether error rate
  or p95 latency regressed. Use after a second run to check for regressions.
- **generate_from_spec** — given a JSON OpenAPI document, generate a runbook
  (one ready-to-run `vegaload run` command per endpoint). Only JSON specs are
  supported — ask the user to export YAML specs to JSON first.

## Typical flow

1. If the user names an OpenAPI spec, call **generate_from_spec** first to see
   what endpoints exist and get a starting command for each.
2. Call **run_test** with the target/protocol (or scenario) and shape the user
   asked for.
3. Read the `report_path` it returns, or call **get_results** later to re-read
   it.
4. If the run had failures or looks degraded, call **diagnose_failure** on
   the report for an explanation before guessing.
5. Once a baseline run looks healthy, call **suggest_thresholds** to propose
   gates for future runs. Keep that baseline JSON path.
6. After a later run, call **compare_reports** with the baseline and candidate
   report paths to see whether error rate or p95 got worse.

Report numbers back to the user plainly (total requests, failure rate, p95
latency) rather than dumping the raw JSON, and mention the HTML report path
so they can open it themselves.
