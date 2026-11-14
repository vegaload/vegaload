---
title: JSON report
description: Every field in the JSON summary that vegaload run -out writes.
sidebar_position: 4
---

# JSON report

`vegaload run -out report.json` writes a JSON summary of the run. `vegaload compare`, `-baseline` and `vegaload diagnose` read this file.

All times are whole numbers of nanoseconds. Names end in `_ns`. A rate such as `error_rate` is a fraction: `0.02` is 2%.

Some sections appear only when they apply. This page was checked against real 0.9.0 files.

## Top level

| Field | Meaning |
| --- | --- |
| `executor` | The load shape, such as `fixed-vus`. |
| `started_at` | When the run started, as a UTC timestamp. |
| `elapsed_ns` | How long the run took. |
| `total` | Number of requests. In a `-protocol` run a request is one iteration. In a scenario run it is one call the scenario made. |
| `failed` | How many of them failed. |
| `error_rate` | `failed` divided by `total`. |
| `latency` | The latency summary of the requests. See below. |
| `failure_reasons` | How many requests failed for each reason, most first. Present when a request failed. See below. |
| `iterations` | A scenario run's iterations. Present only for a scenario run. See below. |
| `dropped_iterations` | Arrivals that `constant-arrival-rate` or `ramping-arrival-rate` could not start because every VU was busy. They never ran, so they are in no other number. Present when there were any. |
| `warnings` | Plain sentences about what makes the numbers less trustworthy, such as more than 1% of the planned iterations dropped, or a warning that starts with `generator overloaded:`. Present when there is one. |
| `generator` | How hard the machine running VegaLoad worked during the run. See below. |
| `time_series` | The run, cut into slices over time. |
| `thresholds` | Each threshold and its result. Present when you set thresholds. |
| `thresholds_passed` | `true` if every threshold passed. |
| `baseline` | The result of the baseline gate. Present with `-baseline`. |
| `steps` | Results for each named `step()`. Present when the scenario uses steps. |
| `protocols` | Results for each protocol the run used. |
| `checks` | Pass and fail counts for each check. Present when the scenario uses checks. |

## `latency`

The same fields appear in `steps` and `protocols`.

| Field | Meaning |
| --- | --- |
| `min_ns`, `mean_ns`, `max_ns` | Fastest, average and slowest call. |
| `p50_ns`, `p90_ns`, `p95_ns`, `p99_ns` | Percentiles. `p95_ns` means 95% of calls were this fast or faster. |

## `generator`

The load generator's own load. When it cannot keep up, it starts calls late and adds its own delay to the latency, so the numbers show its limits, not the target's.

| Field | Meaning |
| --- | --- |
| `cpus` | The CPUs VegaLoad can use (`GOMAXPROCS`, which is the machine's CPU count unless you set it). |
| `cpu_avg_percent` | The CPU the VegaLoad process used over the run, as a percentage of `cpus`. The `python3` processes of a Python scenario are not in it. |
| `cpu_peak_percent` | The most it used in one second. |
| `sched_latency_p99_ns` | How long a goroutine waited to run, 99th percentile, over the run. |
| `sched_latency_peak_p99_ns` | The highest one-second value of the same. |
| `overloaded` | `true` when the run has the `generator overloaded:` warning: the CPU was at 90% or more for 3 seconds or more, goroutines waited 20 ms or more to run (p99) for 3 seconds or more, or more than 1% of the planned iterations were dropped. |

## `failure_reasons[]`

| Field | Meaning |
| --- | --- |
| `reason` | `timeout`, `status N` (the status the answer had, such as `status 503`), `connection refused`, `connection reset`, `dns`, `tls` or `error`. |
| `count` | How many requests failed for that reason. The counts add up to `failed`. |

## `iterations`

An iteration is one run of the scenario's default function (or `iteration()` in Python), every call in it included.

| Field | Meaning |
| --- | --- |
| `total` | How many iterations ran. |
| `failed` | How many ended in an error the scenario threw. A failed status is not one: it fails the call, not the iteration. |
| `error_rate` | `failed` divided by `total`. |
| `duration` | How long a whole iteration took, with the same fields as `latency`. |

## `time_series[]`

| Field | Meaning |
| --- | --- |
| `offset_ns` | Where this slice starts, counted from the start of the run. |
| `requests` | Calls in this slice. |
| `failed` | Failed calls in this slice. |
| `rps` | Calls per second in this slice. |
| `error_rate` | Failure fraction in this slice. |

## `thresholds[]`

| Field | Meaning |
| --- | --- |
| `name` | The threshold as you wrote it. |
| `metric` | The metric, such as `p95`. |
| `operator` | The comparison, such as `<`. |
| `value` | The limit, as text, such as `500ms`. |
| `observed` | What the run measured, as text. |
| `passed` | `true` or `false`. |
| `step` | The step the threshold is limited to. Present only for a step threshold. |
| `protocol` | The protocol the threshold is limited to. Present only for a protocol threshold. |

## `baseline`

| Field | Meaning |
| --- | --- |
| `path` | The baseline file you gave. |
| `max_regression_percent` | The slack you allowed. |
| `passed` | `true` if no metric regressed beyond the slack. |
| `metrics[]` | One entry for `error_rate` and one for `latency.p95`. Each has `name`, `baseline`, `candidate`, `unit` and `regressed`. |
| `notes[]` | A plain sentence about the result. |

## `steps[]`

| Field | Meaning |
| --- | --- |
| `name` | The step name. |
| `total` | How many times the step ran. |
| `failed` | How many runs failed. |
| `error_rate` | The failure fraction. |
| `latency` | The latency summary. |

## `protocols[]`

| Field | Meaning |
| --- | --- |
| `name` | The protocol, such as `http`. |
| `total`, `failed`, `error_rate` | Counts and failure fraction for this protocol. |
| `latency` | The latency summary. |
| `bytes_sent`, `bytes_received` | Bytes moved. |
| `bytes_per_second` | Throughput. |
| `operations[]` | One entry per kind of call, such as `GET`. Each has `name`, `total`, `failed`, `error_rate`, `latency`, `bytes_sent` and `bytes_received`. |

## `checks[]`

| Field | Meaning |
| --- | --- |
| `name` | The check name. |
| `passes` | How many times it passed. |
| `fails` | How many times it failed. |

## Example

A short run with one step and one check, trimmed:

```json
{
  "executor": "fixed-vus",
  "total": 224,
  "failed": 0,
  "error_rate": 0,
  "latency": { "p50_ns": 26642194, "p95_ns": 40212388 },
  "thresholds": [
    { "name": "p95 < 500ms", "metric": "p95", "operator": "<",
      "value": "500ms", "observed": "40.22ms", "passed": true }
  ],
  "thresholds_passed": true,
  "steps": [{ "name": "list", "total": 224, "failed": 0 }],
  "checks": [{ "name": "status is 200", "passes": 224, "fails": 0 }]
}
```

To compare two reports, see [Compare two runs](../guides/compare-runs.md). To explain one, see [Explain a run](../guides/explain-a-run.md).
