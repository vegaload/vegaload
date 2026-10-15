---
name: vegaload-smoke
description: Use VegaLoad for a quick smoke test of an HTTP/gRPC/WebSocket API — check that it works and answers fast under a few users, in about ten seconds. Use this when the user asks "does it work", "is it up", for a smoke test or a sanity check, or before a bigger load test. For real load, ramps, soak, thresholds or baselines, use the vegaload skill.
---

# VegaLoad smoke test

A smoke test is small and fast. It answers one question: does this API work,
and does it answer in a sensible time with a few users? It is not a load test.

`vegaload init` registered VegaLoad's MCP server for this project. These
tools are all you need here:

- **generate_from_spec** — if the user has a JSON OpenAPI spec, write a
  scenario from it. The order of calls is a guess, so validate it. Only JSON
  specs work; ask the user to export YAML to JSON.
- **create_scenario** — scaffold a scenario file when there is no spec.
- **validate_scenario** — run the scenario once with one user. Call it first.
  If `valid` is false, read `stage` and `error`, fix the scenario, and try
  again. It makes real calls.
- **run_test** — run a short test. Use the limits below.
- **get_results** and **diagnose_failure** — read a report back, and explain
  failures before you guess.

## Limits

- Use at most `vus: 5` and `duration: "10s"`, with the `fixed-vus` executor.
  If the user asks for more, do not use this skill. Use `vegaload`.
- A target that is not localhost needs `allow_targets` or `yes: true`. There
  is no terminal here to ask a person, so always confirm with the user before
  you send even a small test to a server that is not localhost or that they
  did not name.
- Put tokens and passwords in `secret_env`. VegaLoad removes them from every
  output.

## Flow

1. With a spec, call **generate_from_spec**. Otherwise use **create_scenario**,
   or a protocol-direct `target` + `protocol` (for example `http1`).
2. Call **validate_scenario** for any scenario you wrote or edited.
3. Call **run_test** with the limits above.
4. Tell the user the answer in plain words: did it work (total requests,
   failures), and what was the p95 latency. Give the HTML report path.
5. If there were failures, call **diagnose_failure** and report what it says.
6. If the user wants more than a smoke test, such as more users, a longer
   run, or pass/fail gates, switch to the `vegaload` skill.

## The live view

A `run_test` is also shown live in the user's browser by default (the live view):
the page opens by itself, and its address comes back as `live view: URL`, the
second text item of the result (the first is always the result JSON). After you
start a run, tell the user that URL. Pass `no-live` only if they do not want it,
and `no-open` to keep it from opening a browser tab. It is off in CI.
