---
title: "Serve the live numbers at /stats"
description: "`-stats-addr` serves a run's numbers as JSON while it runs, in the shape of the Harness k6 stats sidecar."
sidebar_position: 20
---

# Serve the live numbers at /stats

`-stats-addr` starts a small HTTP server for the length of the run. Anything that can read JSON over HTTP can watch the run from it: a dashboard, a script, or a Harness load test runner.

```
vegaload run -vus 5 -duration 10m -stats-addr 127.0.0.1:9093 checkout.vl.js
```

The run prints the address on stderr:

```
stats: http://127.0.0.1:9093/stats
```

Read it as often as you like:

```
curl -s http://127.0.0.1:9093/stats
```

```json
{
  "totalRequests": 1100,
  "totalFailures": 563,
  "avgResponseMs": 20.157,
  "p50Ms": 19.896,
  "p95Ms": 55.429,
  "p99Ms": 60.769,
  "errorRate": 51.182,
  "avgRps": 109.987,
  "iterations": 545,
  "latencyHist": {"0": 546, "76": 3, "77": 12},
  "latencyHistParams": {"baseMs": 0.5, "growth": 1.05},
  "requestStats": {
    "POST:/widgets": {
      "name": "/widgets",
      "method": "POST",
      "numRequests": 550,
      "numFailures": 13,
      "avgResponseMs": 40.155,
      "minResponseMs": 20.288,
      "maxResponseMs": 60.769,
      "p50Ms": 41.362,
      "p95Ms": 58.2,
      "p99Ms": 60.769,
      "requestsPerSec": 54.994,
      "avgSizeBytes": 23.909,
      "latencyHist": {"76": 3, "77": 11}
    }
  },
  "checkStats": {
    "status is 201": {"name": "status is 201", "passes": 537, "fails": 13}
  },
  "currentUsers": 0,
  "peakUsers": 5,
  "runDurationSec": 10.001,
  "dataSentBytes": 7700,
  "dataReceivedBytes": 23600,
  "avgLatencyMs": 20.135,
  "p50LatencyMs": 19.896,
  "p95LatencyMs": 55.429,
  "p99LatencyMs": 60.754,
  "state": "finished",
  "maxUsers": 5,
  "iterationsStarted": 550,
  "iterationsCompleted": 545,
  "droppedIterations": 0,
  "errors": {
    "status 404": {"kind": "status 404", "count": 550, "sample": "status 404"},
    "check failed": {"kind": "check failed", "count": 13, "sample": "status is 201"}
  },
  "steps": {
    "create": {"name": "create", "numRequests": 550, "numFailures": 0, "avgResponseMs": 40.23, "p50Ms": 41.362, "p95Ms": 58.2, "p99Ms": 60.871}
  }
}
```

The example is cut short. A real answer has every row, bucket and kind.

## What the numbers mean

Every number counts from the start of the run, so two reads never go down. Times are in milliseconds. `errorRate` is a percentage, so `51.182` is 51.182%. The JSON report's `error_rate` is a fraction instead.

| Field | Meaning |
| --- | --- |
| `totalRequests`, `totalFailures` | Requests so far, and how many failed. In a scenario run a request is one call the scenario made. |
| `avgResponseMs`, `p50Ms`, `p95Ms`, `p99Ms` | Latency of the requests. |
| `avgRps` | Requests per second since the start. |
| `iterations` | Iterations that completed. |
| `latencyHist`, `latencyHistParams` | The latency histogram, keyed by bucket number. The buckets are the ones in the [JSON report](../reference/report-json.md#histogram). |
| `requestStats` | One row per kind of call. An HTTP row is keyed `METHOD:endpoint`, such as `GET:/users/{id}`. A row for another protocol is keyed by the protocol and the operation, such as `ftp roundtrip`. `avgSizeBytes` is the bytes received per call. |
| `checkStats` | Passes and fails of each `check()`. |
| `currentUsers`, `peakUsers`, `maxUsers` | VUs running now, the most that ran at once, and the most the load shape allows. |
| `runDurationSec` | How long the run has gone on. |
| `dataSentBytes`, `dataReceivedBytes` | Bytes moved by every protocol. |
| `avgLatencyMs`, `p50LatencyMs`, `p95LatencyMs`, `p99LatencyMs` | Time to first byte of the HTTP calls. |
| `state` | `starting` until the load begins, then `running`, then `finished`. `failed` means the run stopped with an error. |
| `iterationsStarted`, `iterationsCompleted`, `droppedIterations` | Iterations that started, ended, and could not start because every VU was busy. |
| `errors` | The failures by kind, each with one sample message. A password in a URL is masked and every `-secret-env` value is removed. |
| `steps` | One entry per `step()`. |

The fields up to `checkStats` are the ones the Harness k6 stats sidecar serves. The `currentUsers` to `p99LatencyMs` fields are the ones its JMeter sidecar adds. A reader that does not know a field ignores it.

`/healthz` answers `{"ok":true}`. Any other path is a 404. A method other than `GET` or `HEAD` is a 405.

## Keep the final numbers up

The server stops when the run ends. A reader that polls every few seconds can miss the last numbers. `-stats-linger` keeps them up a little longer:

```
vegaload run -vus 5 -duration 10m -stats-addr 127.0.0.1:9093 -stats-linger 30s checkout.vl.js
```

```
stats: serving the final numbers at http://127.0.0.1:9093/stats for 30s (Ctrl-C to stop)
```

`state` is then `finished`, and `currentUsers` is 0. Ctrl-C stops the wait early. The exit code is the run's own.

## Who can read it

`127.0.0.1:9093` listens on this machine only. `:9093` listens on every interface, so other machines on the network can read the numbers. Use it in a container or a pod, where the reader is next to the run. The endpoint has no password, so do not open it to a network you do not trust.

The server only answers. VegaLoad sends nothing anywhere because of it. The answer holds the run's numbers, step, check and endpoint names, and error samples. It never holds headers, bodies, options or environment values.

If the address is taken, the run does not start. It exits 2:

```
vegaload run: -stats-addr: listen tcp 127.0.0.1:9093: bind: address already in use
```

`-stats-addr` works with the live view and with `-stream-file` and `-stream-push`. They are separate: turning one off does not turn off the others.
