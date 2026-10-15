---
title: Commands and flags
description: Every VegaLoad command, the flags of run, and the exit codes.
sidebar_position: 2
---

# Commands and flags

```
vegaload <command> [arguments]
```

Flags go before the scenario file. Go stops reading flags at the first argument that is not a flag. So write `vegaload run -vus 10 scenario.vl.js`, not `vegaload run scenario.vl.js -vus 10`.

Every command takes `-output text` (the default) or `-output json`. `run` and `doctor` also take `-output jsonl`.

## Commands

| Command | What it does |
| --- | --- |
| `run` | Run a load test from a scenario file, or from a protocol-direct target. |
| `new` | Write a starter scenario file. |
| `validate` | Run a scenario once, with one user, to check that it works. |
| `import har` | Make a scenario from a browser recording. |
| `diagnose` | Print environment info, or explain a JSON report. |
| `compare` | Compare a candidate report with a baseline. |
| `protocols` | List the supported protocols, with their options. |
| `init` | Set up the MCP server and agent rules for this project. |
| `doctor` | Check that VegaLoad is installed and wired up. |
| `live` | Print or open this machine's live view page. `live reset` starts a new project. |
| `mcp serve` | Run the MCP server for an agent. |
| `mcp eval` | Run the MCP tool-calling eval suite against this binary. |
| `version` | Print the version. |

## Exit codes

| Code | `run` | Other commands |
| --- | --- | --- |
| 0 | The run finished. Thresholds, if any, passed. | Success. |
| 1 | | `validate`: the scenario is not valid. `compare`: a regression. `doctor`: a check failed. `init -status`: something is missing. |
| 2 | VegaLoad refused to start, for example a host that is not allowed. | `validate`: bad usage. |
| 3 | A threshold or the baseline gate was breached. | |

A run with no threshold exits 0 even if every call failed. See [Troubleshooting](../help/troubleshooting.md).

## `run`

```
vegaload run [flags] [scenario-file]
```

Give a scenario file, or give `-target` and `-protocol` for a protocol-direct run.

### Target and request

| Flag | Meaning |
| --- | --- |
| `-target` | Target URL or `host:port`. Leave it out if you give a scenario file. |
| `-protocol` | `http1`, `http2`, `grpc`, `websocket`, `mqtt`, `kafka`, `postgres`, `mysql`, `redis`, `rabbitmq`, `ftp`, `sftp`, `tcp` or `udp`. |
| `-method` | The HTTP verb, or the full gRPC method such as `/package.Service/Method`. |
| `-header` | A request header, `"Key: Value"`. Repeat the flag for more. |
| `-body` | The request body. |
| `-opt` | A driver option, `key=value`. Repeat the flag. Each protocol has its own keys: see [Protocol options](./protocol-options.md). |
| `-insecure` | Skip TLS certificate checks. |
| `-timeout` | Time limit for one iteration. Default `30s`. |

### Load shape

| Flag | Meaning |
| --- | --- |
| `-executor` | `fixed-vus` (default), `ramp`, `step` or `constant-arrival-rate`. |
| `-vus` | Number of virtual users. Default `1`. For `ramp` and `step`, this is the size of the scripting pool. |
| `-duration` | Run length for `fixed-vus` and `constant-arrival-rate`. Default `10s`. |
| `-stages` | Stages for `ramp` and `step`, as `target:duration` pairs, for example `10:30s,0:10s`. |
| `-rate` | Iterations per second, for `constant-arrival-rate`. |
| `-max-vus` | Most users that may run at once, for `constant-arrival-rate`. |

### Pass and fail

| Flag | Meaning |
| --- | --- |
| `-threshold` | A limit on the run, for example `"p95 < 300ms"`. Repeat the flag. A breach exits 3. |
| `-thresholds` | A JSON file of thresholds. It can be the output of `vegaload diagnose -output json`. |
| `-abort-on-breach` | Stop as soon as a threshold is broken beyond recovery, and exit 3. Needs a threshold. |
| `-abort-grace` | With `-abort-on-breach`: the warm-up in which statistics such as p95 are not judged. |
| `-baseline` | A JSON report of an earlier run. If p95 or the error rate is worse, the run exits 3. |
| `-max-regression` | With `-baseline`: how many percent worse is allowed. Default `0`. |

Threshold metrics are `p50`, `p90`, `p95`, `p99`, `mean`, `min`, `max`, `error_rate`, `rps`, `failed`, `total` and `check_rate`. A threshold can be limited to a step or a protocol, as in `p95{step="login"} < 300ms` and `p95{protocol="ftp"} < 500ms`. See [Gate a run on thresholds](../guides/thresholds.md).

### Data and environment

| Flag | Meaning |
| --- | --- |
| `-data` | A CSV (with a header row) or JSON (array of objects) file. Repeat the flag. Give `name=path` to choose the name. See [Data files](../guides/data-files.md). |
| `-data-pick` | With `-target` and `-protocol`: how a call picks a row, `next` (default) or `random`. |
| `-env` | An environment variable that scripts can read as `env.NAME`. |
| `-secret-env` | Like `-env`, but the value is removed from the summary, JSON, audit log and console. |

### Output

| Flag | Meaning |
| --- | --- |
| `-output` | `text` (default), `json` or `jsonl`. |
| `-out` | Also write the JSON summary to this path. See [JSON report](./report-json.md). |
| `-report` | Path for the HTML report. Default `vegaload-report-<timestamp>.html`. |
| `-no-report` | Do not write the HTML report. |
| `-no-open` | Open no browser tab: neither the HTML report nor the live view page. The live view still streams. Also `VEGALOAD_OPEN=off`. |
| `-junit` | Also write a JUnit XML file. Each threshold, check and the baseline gate is one test case. |
| `-no-step-summary` | Inside GitHub Actions, do not add the summary to the job summary. |

### Live view and streaming

By default a run is shown live in the browser. See [`docs/streaming.md`](https://github.com/vegaload/vegaload/blob/main/docs/streaming.md) for the event format.

| Flag | Variable | Meaning |
| --- | --- | --- |
| `-live` | | Show the run in the live view even when `CI` is set or a `-stream-*` destination is used. |
| `-no-live` | `VEGALOAD_LIVE=off` | Do not stream to the live view or open it. It is also off when `CI` is set and when you give `-stream-file` or `-stream-push`. |
| `-live-url` | `VEGALOAD_LIVE_URL` | Address of the live view service. Default `http://127.0.0.1:7080`. |
| `-stream-file` | `VEGALOAD_STREAM_FILE` | Write the run as NDJSON events to this path, or `auto` for `.vegaload/runs/<run id>.ndjson`. |
| `-stream-push` | `VEGALOAD_STREAM_PUSH` | POST the events to this ingest URL, with retry. Repeat the flag, or separate URLs with commas. `http://` is only allowed for a loopback host. |
| `-stream-insecure` | | Allow `-stream-push` over `http://` to a host that is not loopback. |
| | `VEGALOAD_STREAM_TOKEN` | The Bearer token sent with `-stream-push`. |
| `-run-id` | `VEGALOAD_RUN_ID` | The run id on every event. Default: 12 random hex characters. |
| `-label` | | A `key=value` label on every event. Repeat the flag. |
| | `VEGALOAD_CONFIG_DIR` | The folder where the live view keeps this machine's project (`live.json`). |

A flag wins over its variable. The live view sends the run's numbers, thresholds, step and protocol names, labels and the target (without credentials) to the service. It never sends headers, bodies, options, environment values, secrets or data. Turn it off with `-no-live` or `VEGALOAD_LIVE=off`.

### Safety

| Flag | Meaning |
| --- | --- |
| `-allow-target` | A host (or `host:port`) that is allowed without a question, besides localhost. Repeat the flag. |
| `-yes` | Skip the question for a host that is not localhost and not allowed. Needed for runs with no one at the keyboard and for agent runs. |
| `-cap-vus` | Hard cap on virtual users. Default `10000`. |
| `-cap-duration` | Hard cap on run length. Default `24h`. |
| `-cap-rate` | Hard cap on iterations per second. Default `100000`. |
| `-audit-log` | Path of the audit log. Default `~/.vegaload/audit.log`. |
| `-trigger` | What started the run, as recorded in the audit log. Default `cli`. |

See [Safety](../ai-agents/safety.md).

## `validate`

```
vegaload validate [flags] <scenario-file>
```

Runs the scenario once, with one user and one iteration. It makes real network calls and follows the same host rules as `run`. Flags: `-allow-target`, `-yes`, `-audit-log`, `-data`, `-data-pick`, `-env`, `-secret-env`, `-timeout` (default `30s`), `-trigger`, `-output`.

## `new`

```
vegaload new [name] [flags]
```

| Flag | Meaning |
| --- | --- |
| `-python` | Write a Python scenario instead of JavaScript. |
| `-from-openapi` | A JSON OpenAPI spec. Writes a scenario that calls every operation as a named step, and a runbook with one run command per endpoint. |
| `-force` | Overwrite the file if it exists. |
| `-output` | `text` or `json`. |

## `import har`

```
vegaload import har <recording.har> [flags]
```

| Flag | Meaning |
| --- | --- |
| `-o` | Scenario file to write. Default: the HAR file name with `.vl.js`. Use `-` for the screen. |
| `-host` | Keep only requests to this host or `host:port`. Repeat the flag. Default: the main host of the recording. |
| `-include-static` | Keep images, fonts, style sheets and scripts. |
| `-include-third-party` | Keep requests to other sites. |
| `-max` | Stop after this many requests. |
| `-force` | Overwrite the scenario file if it exists. |
| `-output` | `text` or `json`. |

See [Import a HAR file](../guides/import-har.md).

## `compare`

```
vegaload compare [flags] <baseline.json> <candidate.json>
```

Prints the change in error rate, latency percentiles, totals and requests per second. It also compares checks. Each protocol's p95 and error rate are printed, but they do not change the exit code. Exit code 1 means a regression.

| Flag | Meaning |
| --- | --- |
| `-p95-ratio` | Largest allowed candidate-to-baseline p95 ratio. `1.2` allows 20% more. Default `1`. |
| `-error-rate-delta` | Slack on the error rate. `0.01` is one percentage point. |
| `-check-rate-delta` | Slack on each check's pass rate. |
| `-output` | `text` or `json`. |

See [Compare two runs](../guides/compare-runs.md).

## `diagnose`

```
vegaload diagnose [flags]               # check the environment
vegaload diagnose [flags] <report.json> # explain a run
```

| Flag | Meaning |
| --- | --- |
| `-no-llm` | Skip the plain-English summary, even if `VEGALOAD_LLM_PROVIDER` is set. |
| `-output` | `text` or `json`. |

## `protocols`

```
vegaload protocols [flags] [NAME]
```

Lists every protocol, or one. `NAME` is a `-protocol` value. `-output` is `text` or `json`.

## `init`

```
vegaload init [flags]
```

| Flag | Meaning |
| --- | --- |
| `-editor` | `all` (default), or a comma list of `claude-code` and `cursor`. |
| `-dir` | Project folder. Default `.`. |
| `-status` | Change nothing. Report what is set up. Exit 1 if anything is missing. |
| `-force` | Overwrite files that exist. |
| `-output` | `text` or `json`. |

## `doctor`

```
vegaload doctor [flags]
```

Checks the CLI, each agent host (with a live MCP handshake), and optionally a target. It changes nothing unless you pass `-fix`. It exits 1 if any check fails.

| Flag | Meaning |
| --- | --- |
| `-host` | Hosts to check: `auto` (default), `all`, `none`, or a comma list of `cursor`, `claude-code` and `claude-desktop`. |
| `-fix` | Repair what can be repaired safely: missing MCP entries and rules files, stale commands. |
| `-dry-run` | With `-fix`, show what would change and change nothing. |
| `-target` | Check this URL or `host:port`. |
| `-smoke` | Also run a one-user, one-second test against `-target`. This sends real traffic. |
| `-allow-target` | An extra allowed host, as for `run`. |
| `-strict` | Exit 1 on warnings too. |
| `-only`, `-skip` | Run only, or skip, these checks (a comma list of check IDs or groups). |
| `-mcp-config` | Also check this MCP config file. |
| `-dir` | Project folder to check. Default: the current folder. |
| `-timeout` | Time limit for each check. Default `10s`. |
| `-v` | Show detail lines for passing checks. |
| `-output` | `text`, `json` or `jsonl`. |

## `mcp serve`

```
vegaload mcp serve [-http addr [-token-env NAME] [-allow-origin URL]]
```

Runs the MCP server on stdio. See [MCP tools](../ai-agents/mcp-tools.md).

| Flag | Meaning |
| --- | --- |
| `-http` | Listen for MCP over HTTP and SSE on this address, for example `127.0.0.1:8765`. |
| `-token-env` | Name of the environment variable that holds the bearer token. A non-loopback address needs one. |
| `-allow-origin` | An extra browser origin that may call the server. Loopback origins are always allowed. |
