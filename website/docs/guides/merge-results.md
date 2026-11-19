---
title: "Merge results from several machines"
description: "`vegaload merge` combines the JSON reports of runs that sent load at the same time into one report."
sidebar_position: 19
---

# Merge results from several machines

One machine may not be able to send all the load you need. Run VegaLoad on several machines at the same time, each with `-out`:

```
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -executor constant-arrival-rate -rate 500 -max-vus 100 -duration 10m \
  -out gen-1.json
```

Copy the JSON files to one place, then merge them:

```
vegaload merge -out merged.json -report merged.html gen-1.json gen-2.json gen-3.json
```

The merged report is the same as one run of all the load would give. Percentiles from several machines cannot be averaged. So each report carries a latency histogram, and `merge` adds the histograms up and works every percentile out again from the sum. It also adds:

- the totals, failed requests, failure reasons, errors by kind and dropped iterations,
- the iterations started and completed, and the peak and most VUs, since each machine ran its own VUs,
- the time to first byte, from its histograms like the latency,
- the iterations, steps, checks, protocols, operations and measures, matched by name,
- the bytes sent and received. Bytes per second is worked out from the merged run's length.

Min and max are the smallest and largest of any report. The mean is exact. The merged run starts at the earliest start and ends at the latest end. The time series are lined up by each run's start, to the nearest second, so keep the machines' clocks in sync.

The text summary is the same as `run` prints. `-output json` prints the merged report instead. `merged_from` in the JSON says how many runs it combines. A merged report can be merged again.

## Thresholds

Each report's own thresholds, baseline and abort judged only its part of the load, so they are not carried over. Judge the merged result instead:

```
vegaload merge -threshold "p95 < 300ms" -threshold "error_rate < 1%" \
  gen-1.json gen-2.json gen-3.json
```

A breach exits 3, as with `run`. `-thresholds` reads them from a JSON file. Exit code 1 means a report could not be read or merged. Exit code 2 means bad usage.

## What can be merged

Every report this version writes can be merged, including one from `-exact-percentiles`. The merged percentiles then come from the histograms. A report from an older version has no histograms (no `schema` field), so its percentiles cannot be combined correctly. `merge` refuses it with exit 1. Run it again with this version.

See [JSON report](../reference/report-json.md) for the `histogram` fields.
