---
title: MCP tools
description: The nine tools the VegaLoad MCP server gives an agent, and what each one does.
sidebar_position: 2
---

# MCP tools

The VegaLoad MCP server has nine tools. Each one is a thin wrapper around a command line command. `vegaload doctor` checks that your agent host sees all nine.

| Tool | What it does | Same as |
| --- | --- | --- |
| `list_protocols` | Lists the 14 protocols, with options and examples. The agent calls it before it picks a protocol. | `vegaload protocols` |
| `create_scenario` | Writes a new scenario file. JavaScript by default, or Python. | `vegaload new` |
| `generate_from_spec` | Writes a scenario and a runbook from a JSON OpenAPI file. Each operation is a named step. YAML is not supported. | `vegaload new -from-openapi` |
| `validate_scenario` | Runs a scenario once with one user. Returns `valid`, and the `stage` and `error` if not. It makes real calls. | `vegaload validate` |
| `run_test` | Runs a load test, from a scenario file or from a target and a protocol. Returns the result and the path to an HTML report. | `vegaload run` |
| `get_results` | Reads back a JSON report from an earlier run. | |
| `suggest_thresholds` | Suggests p95 and error rate limits from a baseline report. | `vegaload diagnose -no-llm` |
| `diagnose_failure` | Explains a run with rule-based findings. Adds a plain-English summary only if you set `VEGALOAD_LLM_PROVIDER`. | `vegaload diagnose` |
| `compare_reports` | Compares a new report with a baseline. Returns the changes and whether p95, error rate or a check got worse. | `vegaload compare` |

## About `run_test`

`run_test` takes either `scenario_path`, or `target` plus `protocol`.

It also takes the load shape (`vus`, `duration`, `executor`, `stages`, `rate`), `thresholds`, `baseline_path`, data files and environment values, and where to write the HTML, JSON and JUnit reports. Example thresholds: `"p95 < 300ms"`, `"error_rate < 1%"`, `"p95{protocol=\"ftp\"} < 500ms"`.

It also takes `live`, `no-live` and `no-open`, and `stream_file`, `stream_push`, `run_id` and `labels`. By default the run is shown live in the user's browser. The first text item of the answer is always the result JSON. The live view address comes as a second text item, `live view: URL`, and the agent should tell the user. There is no argument for a push token. See [Live view and streaming](../reference/cli.md#live-view-and-streaming).

A threshold breach is a normal result with `thresholds_passed` set to false. It is not a tool error. From a terminal, the same run exits 3.

A target that is not localhost is refused unless it is in `allow_targets`, or `yes` is true. See [Safety](/ai-agents/safety).

## About `validate_scenario`

Call it after you write or edit a scenario, and before `run_test`. A scenario that is not valid is a normal result. Read `valid` first. A failed `check()` does not make a scenario invalid.

## Where the list comes from

The tool names and descriptions on this page are from VegaLoad 0.9.0. You can see the live list on your own machine:

```bash
vegaload doctor
```

Doctor prints the nine tool names from a real handshake with the server.
