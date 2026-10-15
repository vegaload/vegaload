---
title: Features at a glance
description: Every VegaLoad feature in one table, with its status. Use it to check what the released binary does today and what is planned.
sidebar_position: 2
---

# Features at a glance

**This page describes VegaLoad 0.9.0.** A feature marked Planned is not released.

If you are choosing between tools, read [Compare VegaLoad](./compare/index.md) too.

## Running a test

| Feature | What it does | Status |
| --- | --- | --- |
| Direct mode | `vegaload run -target URL -protocol NAME` runs a test with no script file | Available |
| Scenario files | Write a test as a file in JavaScript, TypeScript or Python. Steps can carry values from one call to the next, also across protocols | Available |
| Load shapes | Fixed users, ramp, step, and constant arrival rate | Available |
| Data files | Feed a test from a CSV or JSON file with `-data`, also in direct mode | Available |
| Environment values and secrets | `-env` and `-secret-env`. A secret is removed from the summary, the JSON, the audit log and the console | Available |
| Check a scenario first | `vegaload validate` runs a scenario once with one user | Available |

## Protocols

14 protocols are built in. No extension and no custom build.

| Group | Protocols | Status |
| --- | --- | --- |
| Web | HTTP/1.1, HTTP/2, gRPC, WebSocket | Available |
| Messaging | MQTT, Kafka, RabbitMQ | Available |
| Databases and cache | PostgreSQL, MySQL, Redis | Available |
| Files | FTP (with TLS), SFTP | Available |
| Raw sockets | TCP, UDP | Available |

Run `vegaload protocols` to see the list, with the options and an example for each. Add `-output json` for a machine-readable list.

## Pass or fail

| Feature | What it does | Status |
| --- | --- | --- |
| Thresholds | `-threshold 'p95 < 500ms'`. A breach exits 3. You can limit one protocol, for example `p95{protocol="http1"} < 5s` | Available |
| Stop early | `-abort-on-breach` stops the run when a limit is broken beyond recovery | Available |
| Checks | Assertions inside a scenario, counted in the result | Available |
| Baseline gate | `-baseline` compares a run with an earlier JSON report. If p95 or the error rate is worse than the allowed amount, the run exits 3 | Available |
| Compare two runs | `vegaload compare` shows the difference between two reports, with a section for each protocol | Available |

## Results

| Feature | What it does | Status |
| --- | --- | --- |
| HTML report | One self-contained file. It opens offline. No server, no account, no telemetry | Available |
| Live view | A run is shown live in the browser by default, through a live view service. Off with `-no-live`, in CI, and with `-stream-file` or `-stream-push`. It is the one thing that sends a run anywhere | Available |
| Event stream | `-stream-file` and `-stream-push` write or send the run as NDJSON events, one point per second | Available |
| JSON report | Machine-readable. Includes a section for each protocol | Available |
| JUnit XML | Each threshold, check and baseline gate is one test case, so CI shows it natively | Available |
| CI job summary | A summary on the GitHub Actions page | Available |
| Explain a run | `vegaload diagnose` explains a report's results in plain words | Available |

## Start from what you have

| Feature | What it does | Status |
| --- | --- | --- |
| OpenAPI | `vegaload new -from-openapi` makes a scenario and a runbook from a spec | Available |
| Browser recording | `vegaload import har` makes a scenario from a HAR file | Available |
| JMeter import | Converts a JMeter plan into a scenario and a report of what needs manual work | Planned |
| k6 import | Converts a k6 script | Planned |

## AI agents

| Feature | What it does | Status |
| --- | --- | --- |
| MCP server | Your agent can run tests, read results and explain failures. 9 tools, in the same binary. Over stdio, and over HTTP and SSE | Available |
| One-command setup | `vegaload init` registers the MCP server and skills for Cursor and Claude Code. Other MCP hosts can be configured by hand | Available |
| Host allow-list | Localhost is always allowed. Any other host needs your approval | Available |
| Audit log | Every run is written to `~/.vegaload/audit.log` on your machine | Available |
| Tool evals | `vegaload mcp eval` runs a versioned suite that checks the MCP tools | Available |
| Health check | `vegaload doctor` checks the CLI, the agent hosts and a target | Available |

## Run it anywhere

| Feature | What it does | Status |
| --- | --- | --- |
| One static binary | Linux, macOS and Windows. No runtime to install. Python scenarios need a local `python3` | Available |
| Docker image | `ghcr.io/vegaload/vegaload` for `linux/amd64` and `linux/arm64` | Available |
| Kubernetes | A Helm chart runs one test as a Kubernetes Job, with no custom resource | Available |
| GitHub Action | Run a test in a workflow | Available |
| Homebrew | `brew install vegaload/tap/vegaload` | Available |

## Scale

| Feature | What it does | Status |
| --- | --- | --- |
| Distributed load generation | Run one test from many machines | Not built in. Planned through Harness Resilience Testing |
| Move a test to Harness | One command moves a test to Harness Resilience Testing. Your local test and report stay as they are | Planned |

## Open source

VegaLoad is open source under the Apache 2.0 license. It is sponsored by Harness.
