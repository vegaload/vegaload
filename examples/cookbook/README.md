# VegaLoad cookbook

Short recipes for common jobs. Each one has a goal, a command, and what to
look for. They run against [`../sample-app`](../sample-app), so you can try
every one:

```
cd ../sample-app && go run .
```

Then, in another terminal, run the commands from the `examples/scenarios`
folder (so that the file names below are found), with `vegaload` built
(`go build ./cmd/vegaload` from the repository root).

Flags always go before the scenario file: `vegaload run -vus 5 file.vl.js`.

1. [Check that an API works](#1-check-that-an-api-works)
2. [Find out how many users it can take](#2-find-out-how-many-users-it-can-take)
3. [Send a fixed number of requests per second](#3-send-a-fixed-number-of-requests-per-second)
4. [Fail the build when the API is too slow](#4-fail-the-build-when-the-api-is-too-slow)
5. [Stop a long run early when it is going badly](#5-stop-a-long-run-early-when-it-is-going-badly)
6. [Check that a change made nothing worse](#6-check-that-a-change-made-nothing-worse)
7. [Time each stage of a flow](#7-time-each-stage-of-a-flow)
8. [Give every user different data](#8-give-every-user-different-data)
9. [Keep a token out of the reports](#9-keep-a-token-out-of-the-reports)
10. [Start from a browser recording](#10-start-from-a-browser-recording)
11. [Start from an OpenAPI spec](#11-start-from-an-openapi-spec)
12. [Understand a bad run](#12-understand-a-bad-run)
13. [Let an AI agent run the tests](#13-let-an-ai-agent-run-the-tests)
14. [Check your setup](#14-check-your-setup)
15. [Block a slow pull request in GitHub Actions](#15-block-a-slow-pull-request-in-github-actions)

## 1. Check that an API works

A smoke test: a few users, a few seconds. It tells you the API answers and
does not fail.

```
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 -vus 5 -duration 10s
```

Look at `failed` (it should be 0) and `p95` (the time that 95 out of 100
requests beat). Run this before any bigger test.

## 2. Find out how many users it can take

Raise the number of users in stages and watch where latency or errors start
to climb. `-stages` takes `users:time` pairs: the first number is how many
users to reach, the second is how long the stage lasts. The users go up
smoothly during each stage.

```
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -executor ramp -stages 10:20s,25:20s,50:20s,0:10s
```

This run lasts as long as its stages, 70 seconds here. `-duration` is not
used with `-stages`, even though the start line of the run prints the default
10 seconds. Open the HTML report and look at the time series: the
point where p95 bends up is your limit. Use `-executor step` for jumps
instead of smooth climbs.

## 3. Send a fixed number of requests per second

Real traffic does not slow down when your server does. The
`constant-arrival-rate` executor keeps the rate fixed, so a slow server shows
up as a growing delay, not as fewer requests.

```
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -executor constant-arrival-rate -rate 50 -max-vus 100 -duration 30s
```

`-rate` is iterations per second. `-max-vus` is the most users VegaLoad may
use to keep that rate.

## 4. Fail the build when the API is too slow

A threshold is a pass or fail rule. When one breaks, the run exits with
code 3, so a CI step fails.

```
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -vus 10 -duration 30s \
  -threshold "p95 < 300ms" -threshold "error_rate < 1%" \
  -junit results.xml
echo $?
```

`-junit` writes a file that CI systems show natively: each threshold is one
test case. In GitHub Actions the summary is also added to the job page.

## 5. Stop a long run early when it is going badly

For a ten-minute run there is no reason to wait if the API fell over in the
first minute.

```
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -vus 10 -duration 10m \
  -threshold "error_rate < 5%" -abort-on-breach
```

Statistics such as `p95` and `error_rate` must stay broken for three
seconds, after a short warm-up, so one slow second does not stop the run.
The run exits with code 3 and the result says it was aborted.

## 6. Check that a change made nothing worse

Keep a good run as the baseline, then compare after your change.

```
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -vus 10 -duration 30s -out baseline.json

# ... change your code, restart the service ...

vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -vus 10 -duration 30s -baseline baseline.json -max-regression 10
```

The run exits with code 3 if p95 or the error rate is more than 10 percent
worse. To compare two files you already have:
`vegaload compare baseline.json candidate.json`.

## 7. Time each stage of a flow

A real user does several things in order. Wrap each one in `step(name, fn)`
and the summary shows latency and errors for each stage on its own.
[`crud-flow.vl.js`](../scenarios/crud-flow.vl.js) creates a widget, reads it
back, and lists all widgets:

```
vegaload run -vus 5 -duration 10s -threshold 'p95{step="list"} < 300ms' crud-flow.vl.js
```

The sample app fails about 3 percent of creates on purpose, so the `create`
stage shows a few failures. The threshold looks at the `list` stage only. If you do not know which stage
is slow, run without a threshold and read the per-step table first.

## 8. Give every user different data

[`data-env.vl.js`](../scenarios/data-env.vl.js) reads one row of
`data/widgets.csv` for each iteration, so users do not all send the same
request.

```
API_KEY=demo-key vegaload run -vus 5 -duration 10s -data data/widgets.csv -secret-env API_KEY data-env.vl.js
```

The sample app fails about 3 percent of creates on purpose, so a few checks
fail. That is expected. In the script, `data.widgets.next()` is the next row, and
`data.widgets.random()` is any row.

## 9. Keep a token out of the reports

Scripts can only read variables you name. Use `-secret-env` for tokens and
passwords. The value is removed from the summary, the JSON file, the audit
log and the console. Use `-env` for settings that are not secret.

```
API_KEY=demo-key REGION=eu vegaload run -vus 2 -duration 5s -secret-env API_KEY -env REGION data-env.vl.js
```

In the script you read them as `env.API_KEY` and `env.REGION`.
A variable you name must be set, or the run stops with an error.

## 10. Start from a browser recording

Save a HAR file from the network tab of your browser, then turn it into a
scenario.

```
vegaload import har -o recorded.vl.js recording.har
vegaload validate recorded.vl.js
```

By default it keeps the main site and drops images, fonts, style sheets and
analytics. Add `-include-static` or `-include-third-party` to keep them.
Always run `validate` first. It runs the scenario once with one user.

## 11. Start from an OpenAPI spec

```
vegaload new -from-openapi ../sample-app/openapi.json sample-app
vegaload validate sample-app.vl.js
```

This writes a scenario that calls every operation as a named step, and a
runbook with one command per endpoint. The order of calls is a guess:
creates, then reads, updates and deletes. Edit it to match the real flow. Add
`-python` for a Python scenario. Only JSON specs work; export YAML to JSON
first.

## 12. Understand a bad run

```
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -vus 10 -duration 20s -method POST -body '{"name":"x"}' -out bad.json
vegaload diagnose bad.json
```

`diagnose` lists findings in plain words: how many requests failed, whether
latency has a long tail, and whether the failures were one burst or spread
over the whole run. To get ready-made thresholds from a healthy run, use
`vegaload diagnose -output json baseline.json > gate.json`, then
`-thresholds gate.json` on later runs.

## 13. Let an AI agent run the tests

```
vegaload init
vegaload init -status
```

`init` registers VegaLoad as an MCP server for Claude Code and Cursor in this
project, and adds two skills: `vegaload-smoke` for quick checks and `vegaload`
for real load. Use `-editor cursor` to set up only one editor. `-status`
shows what is in place and changes nothing.

## 14. Check your setup

```
vegaload doctor
```

It checks the CLI, the agent hosts and, with `-target`, that the target
answers. Run `vegaload doctor -fix` to repair what it can.

## 15. Block a slow pull request in GitHub Actions

Run a short load test on every pull request. When a limit breaks, the check
fails, and the pull request cannot be merged until it is fixed.

Add `.github/workflows/load-gate.yml` to your repository. Replace the start
command and the URL with your own:

```yaml
name: Load gate

on: pull_request

jobs:
  load:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Start the service
        run: |
          nohup ./start-service.sh > service.log 2>&1 &
          for i in $(seq 1 30); do
            curl -sf http://127.0.0.1:8080/health && exit 0
            sleep 1
          done
          echo "The service did not start"; cat service.log; exit 1

      - uses: vegaload/vegaload@v0.9.0
        with:
          args: >-
            run -target http://127.0.0.1:8080/widgets -protocol http1
            -vus 10 -duration 30s
            -threshold "p95 < 300ms" -threshold "error_rate < 1%"
            -junit vegaload-junit.xml

      - uses: actions/upload-artifact@v4
        if: always()
        with:
          name: vegaload-results
          path: |
            vegaload-junit.xml
            vegaload-report-*.html
```

What happens:

1. The job starts your service on the runner and waits until it answers.
2. VegaLoad runs for 30 seconds. If p95 is above 300 ms, or more than 1 in
   100 requests fail, it exits with code 3 and the step turns red.
3. The job page shows a table with the verdict of each limit. VegaLoad adds it
   on its own inside GitHub Actions. The HTML report is saved as an artifact.

To make it a real gate, tell GitHub that this check is required. In the
repository, open Settings, then Branches (or Rules), and add a rule for your
main branch that requires the status check `load`. Without this rule, a red
check can still be merged.

Tips:

- The service runs on the runner itself, so the target is localhost and needs
  no approval. If you test a shared staging URL instead, add
  `-allow-target` with its host name to `args`, because nobody can answer the
  question in a job.
- Shared runners are not all the same speed. Start with a loose limit, for
  example twice your usual p95. Run the job a few times, look at the spread,
  and tighten the limit later. A limit that fails by chance makes people stop
  trusting the check.
- To also catch a change that makes things slower than before, add `-baseline`
  and `-max-regression` as in [recipe 6](#6-check-that-a-change-made-nothing-worse).
  VegaLoad keeps no baseline for you, so the job must have the baseline file,
  for example one you keep in the repository.
- The action runs on Linux and macOS runners.
