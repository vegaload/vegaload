---
title: Test an HTTP API
description: Load test an HTTP/1.1 or HTTP/2 endpoint with one command, or write a scenario for a flow of calls.
sidebar_position: 2
---

# Test an HTTP API

VegaLoad speaks HTTP/1.1 (`-protocol http1`) and HTTP/2 (`-protocol http2`). Both take `http://` and `https://` targets.

The examples on this page run against the [sample app](https://github.com/vegaload/vegaload/tree/main/examples/sample-app) in the repository. Start it first, in its own terminal:

```bash
cd examples/sample-app
go run .
```

It serves `http://127.0.0.1:8080` with `GET /health`, `GET /widgets`, `GET /widgets/{id}` and `POST /widgets`. Localhost is always allowed. For any other host, see [Safety](/ai-agents/safety).

## One command, no file

Read an endpoint with 5 users for 10 seconds:

```bash
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 -vus 5 -duration 10s
```

On a laptop this makes about 600 requests in 3 seconds, with p50 near 25 ms. The sample app adds 10 to 40 ms of delay on purpose.

Send a write with a method, a header and a body:

```bash
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -method POST -header 'Content-Type: application/json' -body '{"name":"bolt"}' \
  -vus 5 -duration 3s -out post.json
vegaload diagnose post.json
```

The sample app fails about 3% of creates on purpose. The run shows a failure rate near 3%, and `diagnose` says so.

| Flag | What it does |
| --- | --- |
| `-method` | The HTTP verb. The default is GET. |
| `-header "Key: Value"` | A request header. Repeat it for more. |
| `-body` | The request body. |
| `-timeout` | The limit for one request. The default is 30 seconds. A request that runs out of time fails with the reason `timeout`. |
| `-insecure` | Skip the TLS certificate check. |
| `-no-connection-reuse` | Open a new connection for every request. See [Connections](#connections). |
| `-data` | Fill `{data.NAME.COLUMN}` in `-body` from a CSV or JSON file. See [Data files](/guides/data-files). |

## Connections

VegaLoad keeps connections open between requests, as a browser or an API client does. Each user keeps its own connection, so 50 users hold about 50 connections for the whole run. This is true for `-protocol http1` and for the `http` calls of a scenario, with or without `insecure`. HTTP/2 sends every user's requests over one connection per host.

To test connection setup on purpose, add `-no-connection-reuse`. Every request then opens a new connection and closes it. Pace such a run with `-executor constant-arrival-rate`, or the machine can run out of local ports:

```bash
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 -no-connection-reuse \
  -executor constant-arrival-rate -rate 50 -max-vus 10 -duration 5s
```

## HTTP/2

Use `-protocol http2`:

```bash
vegaload run -target http://127.0.0.1:8080/widgets -protocol http2 -vus 5 -duration 3s
```

The sample app serves HTTP/1.1 and HTTP/2 on one port, with the same handlers. A run with `http1` and a run with `http2` compare like for like. Any gap is protocol overhead. `-method POST` works here too.

## TLS

For `https://` targets, VegaLoad checks the certificate. To trust your own CA, or to send a client certificate, pass `ca_file`, `cert_file`, `key_file` and `tls_server_name` with `-opt`. See [TLS with your own CA](/guides/tls).

## Pass or fail limits

Add a threshold to stop a pull request when the API is too slow:

```bash
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -vus 5 -duration 3s \
  -threshold 'p95{protocol="http1"} < 300ms' -threshold 'error_rate < 1%'
```

A breach exits with code 3. See [Thresholds](/guides/thresholds).

## A flow of calls

One URL is not a user session. For a flow, such as create, read back and list, write a scenario. The script has `http.get`, `http.post`, `http.put`, `http.patch`, `http.delete` and `http.request`. A value from one call can go into the next.

Each call is one request in the report. A status of 400 or higher fails the call, the same rule as `-protocol http1`, and the script still gets the answer. To accept another status for one call, pass `expectedStatuses: [404]` in its options (`expected_statuses=[404]` in Python). See [What a scenario run counts](/guides/scenarios#what-a-scenario-run-counts).

[`crud-flow.vl.js`](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/crud-flow.vl.js) (and its Python twin, [`crud_flow.py`](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/crud_flow.py)) creates a widget, reads it back by id, and lists all widgets. Each stage is a named `step()`, so the report shows latency and errors for each one. Run it:

```bash
vegaload run -vus 5 -duration 10s crud-flow.vl.js
```

Flags go before the scenario file. See [Scenarios](/guides/scenarios) for the full script reference.

### Request options

Each `http` call takes options. These three change how one request is sent:

| Option | What it does |
| --- | --- |
| `timeout` | The limit for this request. It replaces `-timeout` for this call. In JavaScript, a number is milliseconds. In Python, a number is seconds. Both take a duration string such as `"500ms"` or `"5s"`. A request that runs out of time fails with the reason `timeout`. |
| `redirects` | How many redirects to follow. `0` does not follow: the script gets the 3xx answer. The default follows up to 10. More than the limit fails the call. |
| `params` | Query parameters to add to the URL, encoded for you. A list repeats the name. A query already in the URL is kept. |

```js
const res = http.get("http://127.0.0.1:8080/widgets", {
  params: { limit: 10, tag: ["a", "b"] }, // ?limit=10&tag=a&tag=b
  timeout: "2s",
  redirects: 0,
});
```

```python
res = http.get("http://127.0.0.1:8080/widgets",
               params={"limit": 10, "tag": ["a", "b"]}, timeout=2, redirects=0)
```

A value of the wrong type throws an error the script can catch, and no request is sent.

## Start from what you have

- From an OpenAPI file: `vegaload new -from-openapi openapi.json`. See the [cookbook recipe](/cookbook/start-from-an-openapi-spec).
- From a browser recording: `vegaload import har`. See [Start from a browser recording](/guides/import-har).

## Options

`vegaload protocols http1` lists the options for the protocol, and the scenario calls it has. HTTP/1.1 and HTTP/2 take `ca_file`, `cert_file`, `key_file` and `tls_server_name`. They write nothing to the server beyond what your requests send.
