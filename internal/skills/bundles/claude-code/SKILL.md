---
name: vegaload
description: Use VegaLoad for a real load test of an HTTP/gRPC/WebSocket API — ramped or stepped VU shapes, soak runs, stress and capacity tests, pass/fail thresholds, comparing a run against a baseline, diagnosing failures, or generating a scenario from an OpenAPI spec. For a quick "does it work" check, use the vegaload-smoke skill instead.
---

# VegaLoad load testing

This skill is for real load: many users, longer runs, shapes, thresholds and
baselines. For a quick check that an API works and answers fast under a few
users, use the `vegaload-smoke` skill first. A clean smoke run is a good
start before any load run.

VegaLoad is an open-source load testing tool. `vegaload init` registered its MCP
server for this project, which exposes nine tools — every one of them a thin
wrapper around the same `vegaload` CLI commands you could run yourself from a
terminal, so nothing here does anything a human running `vegaload` by hand
couldn't also do.

## Tools

- **create_scenario** — scaffold a new scenario file (`vegaload new`). The
  scenario's `http`/`ws` globals can make real calls, carrying a value from
  one into the next (create something over HTTP, then watch it over
  WebSocket) — the same per-call allowlist `run_test`'s safety gate below
  describes applies to every one of those calls, not just a `target`.
  Scripts also have `tcp`, `udp`, `mqtt`, `kafka`, `grpc`, `postgres`, `mysql`, `redis`, `rabbitmq`, `ftp` and `sftp` globals, so one
  flow can mix protocols; `grpc.call(url, {method, body})` takes and returns
  JSON, with no `.proto` file, when the server offers reflection.
  Scripts can also call `check(value, {name: test})` to count named
  assertions without failing the iteration; `run_test`'s `thresholds` can
  gate on them with the `check_rate` metric. They can also wrap stages in
  `step(name, fn)` (Python: `with step(name):`) so results show latency and
  error rate per stage, and a threshold can target one, e.g.
  `p95{step="login"} < 300ms`. A threshold can also target one protocol or
  one operation: `p95{protocol="ftp"} < 500ms`,
  `error_rate{protocol="ftp",op="roundtrip"} < 1%`. The result includes
  `protocols` when the run recorded calls. A script HTTP 4xx or 5xx counts
  as a success in that row; use `check()` on the status to judge it.
- **run_test** — run a load test, either a scenario file or a protocol-direct
  target (`target` + `protocol`, e.g. `http1`). Pick an `executor`: `fixed-vus`
  (steady concurrency), `ramp` (stages that climb then fall), `step` (stages
  that jump), or `constant-arrival-rate` (fixed throughput via `rate`).
  Returns a `report.Result` (total/failed/error_rate/latency percentiles/a
  per-second time series) and, unless `no_report` is set, a path to a
  self-contained HTML report you can open or point the user at. For a long
  run with `thresholds`, set `abort_on_breach: true` to stop it early once a
  threshold is broken beyond recovery; the result then has `aborted`.
  **Safety gate**: a target that isn't localhost and isn't in `allow_targets`
  is refused unless `yes: true` is set — there's no terminal here to prompt a
  human for confirmation, so a deliberate `yes` (or an explicit allowlist) is
  required before VegaLoad sends real load at someone else's server. Default
  to a short run (`duration: "10s"`, modest `vus`) unless the user asks for
  more, and always confirm with the user before pointing load at anything
  that isn't localhost or a target they explicitly named.
- **get_results** — read back a report JSON file you already have a path for
  (from `run_test`'s `report_path`, or a file the user points you at). The
  report includes `protocols` when the run recorded calls.
- **suggest_thresholds** — given a baseline run's report, get a suggested p95
  latency ceiling and error-rate ceiling for gating future runs.
- **diagnose_failure** — given a report, get rule-based findings (failure
  rate, latency long tail, whether failures were transient or spread across
  the run) and, if the user has configured `VEGALOAD_LLM_PROVIDER` in this
  MCP server's environment, a plain-English narrative.
- **compare_reports** — diff a candidate JSON report against a baseline JSON
  report (`vegaload compare`). Returns metric deltas, each named check's pass
  rate in both runs, and whether error rate, p95 latency or a check's pass
  rate regressed. It also lists each protocol's p95 and error rate. Those
  lines do not change the verdict. Use after a second run to check for regressions.
- **generate_from_spec** — given a JSON OpenAPI document, write a runnable
  scenario (JavaScript, or Python with `python: true`) that calls every
  operation as a named step, and a runbook with one `vegaload run` command per
  endpoint. The order is a guess (creates, reads, updates, deletes; a created
  id is used by the calls under it), so validate it and edit it to fit the
  real flow. Only JSON specs are supported — ask the user to export YAML specs
  to JSON first.
- **list_protocols** — list every supported protocol, with options and examples
  (`vegaload protocols`). Call it before choosing a `-protocol` value.
- **validate_scenario** — run a scenario once, with one user and one iteration,
  and report whether it works (`vegaload validate`). It makes real calls, under
  the same host rules as **run_test**. Returns `valid`, and when not valid, the
  `stage` (`load` or `iteration`) and `error`. Call it after writing or editing
  a scenario and before **run_test**. A failed `check()` does not make a
  scenario invalid.

## Typical flow

1. If the user names an OpenAPI spec, call **generate_from_spec** first. It
   writes a scenario to start from (check it with **validate_scenario**) and a
   runbook with a command for each endpoint.
2. For a scenario you just wrote or edited, call **validate_scenario** first to
   check that it works with one user. Fix it if `valid` is false.
3. Call **run_test** with the target/protocol (or scenario) and shape the user
   asked for. If the user has a baseline report, pass `baseline_path` (and
   `max_regression`, in percent) to gate the run on it in one call. Pass
   `junit_path` to also write JUnit XML for a CI system. A scenario can read
   rows from `data_files` (CSV or JSON) as `data.NAME.next()` and variables
   from `env` as `env.NAME`. With a target and protocol, the same data files fill `{data.NAME.COLUMN}` on each call for postgres, mysql, redis, ftp, sftp, kafka and rabbitmq; env stays for scenario files. Put tokens and passwords in `secret_env`, which
   removes them from every output. **validate_scenario** takes the same.
4. Read the `report_path` it returns, or call **get_results** later to re-read
   it.
5. If the run had failures or looks degraded, call **diagnose_failure** on
   the report for an explanation before guessing.
6. Once a baseline run looks healthy, call **suggest_thresholds** to propose
   gates for future runs. Keep that baseline JSON path.
7. After a later run, call **compare_reports** with the baseline and candidate
   report paths to see whether error rate or p95 got worse.

Report numbers back to the user plainly (total requests, failure rate, p95
latency) rather than dumping the raw JSON, and mention the HTML report path
so they can open it themselves.

## The live view

A `run_test` is also shown live in the user's browser by default (the live view):
the page opens by itself, and its address comes back as `live view: URL`, the
second text item of the result (the first is always the result JSON). After you
start a run, tell the user that URL. Pass `no-live` only if they do not want it,
and `no-open` to keep it from opening a browser tab. It is off in CI.
