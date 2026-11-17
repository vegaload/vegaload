---
title: "Gate a run on pass/fail thresholds"
description: "Add `-threshold` to make a run pass or fail on its own numbers, for example in CI:"
sidebar_position: 16
---

# Gate a run on pass/fail thresholds

Add `-threshold` to make a run pass or fail on its own numbers, for example
in CI:

```
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -vus 10 -duration 30s \
  -threshold "p95 < 300ms" \
  -threshold "api stays up: error_rate < 1%"
```

Each threshold is an optional `name:`, a metric, an operator (`<`, `<=`,
`>`, `>=`) and a value. The metrics are `p50`, `p90`, `p95`, `p99`, `mean`,
`min`, `max` (durations such as `300ms`), `error_rate` (a fraction like
`0.01` or a percentage like `1%`), `rps`, `failed`, `total` and `check_rate` (the share of `check()` calls
that passed; see below). A threshold can
target one named step (`p95{step="login"} < 300ms`), one protocol
(`p95{protocol="ftp"} < 500ms`), or one operation
(`error_rate{protocol="ftp",op="roundtrip"} < 1%`). `check_rate` cannot
target a step or a protocol. See Steps and Per-protocol results below. Every
threshold is judged once, on the finished run. A run that completed no
requests fails a whole-run threshold. A step or protocol that never ran fails
its own.

The text summary, the JSON output, the HTML report and the audit log all
show each threshold's verdict. The exit code tells a script what happened:

| Exit code | Meaning                                                  |
|-----------|----------------------------------------------------------|
| 0         | The run finished and every threshold passed (or none set) |
| 1         | The run itself failed                                    |
| 2         | Bad usage, such as a threshold that cannot be parsed     |
| 3         | The run finished but broke at least one threshold        |

## Stop early on a breach

Add `-abort-on-breach` and the run stops as soon as a threshold is broken
beyond recovery. It exits 3, like any breach, and saves the time and load
of a run that has already failed. The summary, JSON, HTML report and GitHub
job summary say that the run was aborted, at what time, and by which
threshold.

```
vegaload run -vus 50 -duration 10m -abort-on-breach \
  -threshold "failed < 20" -threshold "p95 < 300ms" -threshold "error_rate < 1%" scenario.vl.js
```

Two kinds of threshold are judged while the run goes on:

- A threshold that cannot recover, such as `failed < 20`, `max < 2s` or
  `total < 5000`, stops the run at once.
- A statistic that moves up and down, such as `p95`, `error_rate`, `mean`
  or `check_rate` (also for one step), stops the run only if it stays
  broken for three seconds in a row. It is not judged during a warm-up (5
  seconds, or a quarter of the run if that is shorter; change it with
  `-abort-grace`; `-abort-grace 0` turns it off), and not until it has at least 20 samples.

A threshold that needs the whole run, such as `rps >= 100` or
`total >= 1000`, is judged at the end, on what ran before the stop. The run
tells you which ones before it starts. `-abort-on-breach` needs at least
one threshold. From an agent, `run_test` takes `abort_on_breach` and
`abort_grace`.

## Checks

In a scenario script, `check(value, {name: test})` counts named assertions
without failing the iteration. Each test is a function or a boolean; a test
that throws counts as failed. `check` returns true only if all tests passed.

```js
check(res, { "status is 200": (r) => r.status === 200 });
```

In Python the tests are callables or bools, and a test that raises counts as
failed. The summary, JSON, and HTML report list each check's passes and
fails. Gate on them with `-threshold "check_rate >= 99%"`. A run with no
checks fails that threshold. Use fixed check names. A run keeps at most 100 distinct
names; any more are counted together as `(other checks)`. See
`examples/scenarios/checks.vl.js`.

## Steps

Name the stages of a flow with `step(name, fn)` and the summary, the JSON
output, the HTML report and the GitHub step summary show the latency and
error rate of each one, so you can see which stage is slow.

```js
const token = step("login", () => http.post(base + "/login", { body }).json().token);
step("checkout", () => { /* ... */ });
```

In Python, `with step("login"):` times a block, and `step("login", fn)` runs
a function and returns its value. A step that throws (or raises) is a failed
step, and the error carries on, so the iteration fails too. Steps can nest;
each is counted on its own. Gate on one step with a selector:
`-threshold 'p95{step="login"} < 300ms'`. A step that never ran fails its
threshold. Step names with a quote mark in them cannot be targeted. A run
keeps at most 100 distinct step names; any more are counted together as
`(other steps)`. See `examples/scenarios/crud-flow.vl.js`.

## Per-protocol results

A scenario can mix HTTP, gRPC, WebSocket, SQL, MQTT, FTP and more. The report
also shows one row per protocol, and under it one row per operation. In a
scenario run, the whole-run numbers are every call, so they add up to the
protocol rows.

| Protocol | Calls | Failed | Error rate | p50 | p95 | Max | Sent | Received |
|----------|------:|-------:|-----------:|----:|----:|----:|-----:|---------:|
| http | 4,200 | 3 | 0.07% | 12ms | 40ms | 210ms | 1.2 MiB | 9.8 MiB |
| postgres | 4,200 | 0 | 0% | 3ms | 9ms | 31ms | 0.4 MiB | 12 MiB |
| ftp | 600 | 12 | 2% | 85ms | 190ms | 1.1s | 1.2 GiB | 1.2 GiB |

Under ftp, the operations are `roundtrip`, `upload`, and so on.

The JSON report includes `protocols` when at least one call was recorded. The
text summary, the HTML report and the GitHub step summary show the table when
the run used more than one protocol, or one protocol that is not `http`,
`http1` or `http2`. A plain HTTP run looks as it did before.

The names are the protocol and the operation. A script's HTTP calls are
`http`, and the operation is the method (`GET`, `POST`, and so on; any other
verb is `OTHER`). A `-protocol` run uses the driver's name, such as `http1`,
`http2`, `grpc` or `websocket`. The operation is the mode, such as
`roundtrip`. More measures will come later.

FTP and SFTP also record phase times: `connect_time`, `login_time`,
`upload_time`, `download_time` and `delete_time`. A connect or login under
1 ms is not counted. The times are whole milliseconds, and 0 means a reused
connection.

A script HTTP call fails on a transport error (the connection failed, it timed
out, or the body could not be read) and on a status of 400 or higher, the same
rule as `-protocol http1`. A 2xx or 3xx answer is a success. To accept another
status for one call, list the statuses it expects:
`http.get(url, { expectedStatuses: [200, 404] })`, or in Python
`http.get(url, expected_statuses=[200, 404])`.

Gate on a protocol, or on one of its operations. The names are matched
exactly (`ftp`, not `FTP`). `check_rate` cannot target a protocol. A protocol
or operation that recorded no calls fails its threshold.

```
vegaload run -threshold 'p95{protocol="ftp"} < 500ms' \
  -threshold 'error_rate{protocol="ftp",op="roundtrip"} < 1%' \
  -target ftp://127.0.0.1:2121/ -protocol ftp -opt mode=roundtrip ...
```

A thresholds file names them next to `step`:

```json
[
  {"name": "ftp-fast", "metric": "p95", "protocol": "ftp", "operator": "<", "value": "500ms"},
  {"metric": "error_rate", "protocol": "ftp", "op": "roundtrip", "operator": "<", "value": "1%"}
]
```

`vegaload compare` prints each protocol's p95 and error rate, and lists a
protocol that is only in one of the two reports. Those lines do not change
the exit code. The gate stays the whole run's p95 and error rate.

JUnit XML adds properties `protocol.<name>.calls`, `.failed`, `.p95_ms`,
`.bytes_sent` and `.bytes_received`. It does not add a test case per protocol.

## JUnit XML for CI

`-junit results.xml` also writes the verdicts as JUnit XML, so a CI system
can show them as test results. Each threshold, each check and the baseline
gate is one test case. A failed threshold, a check that failed even once,
or a baseline the run is worse than is a failed test case. A failed check
does not change the exit code of `vegaload run`, but it does show as a
failed test case, because the report says what happened and the exit code
is the gate. A run with none of these writes an empty suite.

Thresholds can also come from a file, with `-thresholds gate.json`. The file
is a JSON list of `{"name", "metric", "operator", "value"}`, with optional
`step`, `protocol` and `op`, or the output of
`vegaload diagnose -output json`, so a baseline run can set the bar for the
next ones:

```
vegaload diagnose -no-llm -output json baseline.json > gate.json
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -vus 10 -duration 30s -thresholds gate.json
```

`diagnose` also prints the same suggestion as ready-to-paste `-threshold`
flags. From an agent, `run_test` takes a `thresholds` list; a breach comes
back as a normal result with `thresholds_passed: false`.
