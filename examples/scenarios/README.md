# scenarios

Ready-to-run examples against [`../sample-app`](../sample-app), plus one
scripted scenario file. Start the sample app first:

```
cd ../sample-app && go run .
```

Then, from the repository root, with `vegaload` built (`go build ./cmd/vegaload`
or `go install ./cmd/vegaload`):

## 1. A protocol-direct baseline run

No scenario file needed -- `-target`/`-protocol` drive the sample app's
`/widgets` endpoint directly:

```
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 -vus 10 -duration 30s
```

This writes a self-contained HTML report (and opens it) by default. Keep the
JSON summary too, for the next two steps:

```
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -vus 10 -duration 30s -out baseline.json
```

### The same widgets over the other protocols

The sample app serves the same data over every protocol VegaLoad speaks
(see [`../sample-app/README.md`](../sample-app/README.md) for each
endpoint's behavior):

```
# HTTP/2, over plain TCP (h2c) on the same port
vegaload run -target http://127.0.0.1:8080/widgets -protocol http2 -vus 10 -duration 30s

# WebSocket: connect, send one message, wait for its echo
vegaload run -target ws://127.0.0.1:8080/ws/echo -protocol websocket -body hello -vus 10 -duration 30s

# gRPC: the standard health check, then the widget service
vegaload run -target 127.0.0.1:9090 -protocol grpc \
  -method /grpc.health.v1.Health/Check -vus 10 -duration 30s
vegaload run -target 127.0.0.1:9090 -protocol grpc \
  -method /widgets.v1.WidgetService/GetWidget -body $'\x08\x02' -vus 10 -duration 30s

# gRPC create, with the same ~3% injected failure as POST /widgets
vegaload run -target 127.0.0.1:9090 -protocol grpc \
  -method /widgets.v1.WidgetService/CreateWidget -body $'\x0a\x06flange' -vus 10 -duration 30s
```

A gRPC `-body` is already-encoded protobuf (`$'...'` is bash/zsh syntax for
those bytes); the sample app's README explains how the two above are built.

## 2. Explain the results

```
vegaload diagnose baseline.json
```

With the sample app's injected latency and ~3% POST failure rate, a run
against `/widgets` (GET-only, so no failures) is a clean baseline; try it
against `-method POST` to see `diagnose` actually have something to flag.

## 3. Generate a runbook from an OpenAPI spec

`generate_from_spec` (the MCP tool) and `vegaload new -from-openapi` (the CLI
command behind it) are the same thing -- a Markdown runbook of one
ready-to-run command per endpoint, not a scripted scenario (see
`smoke.vl.js`'s comment for why):

```
vegaload new -from-openapi ../sample-app/openapi.json sample-app
```

This writes `sample-app.vegaload-plan.md` with a `vegaload run` command for
each of `/health`, `/widgets` (GET and POST), and `/widgets/{id}`.

## 4. The scripted scenario

`smoke.vl.js` is here as the "a test is a real file" example (see
`AGENTS.md`), not a load test of the sample app -- it exercises VegaLoad's VU
pool and executor shapes against plain JS, since Phase 0's scripting
runtimes can't reach the network yet:

```
vegaload run -vus 5 -duration 5s smoke.vl.js
```

(flags before the scenario file -- see `vegaload run -h`)

## 5. Do all of this from an agent instead

```
vegaload init
```

registers this same CLI as an MCP server (and a Claude Code / Cursor skill
bundle) for the project you run it in, so an agent can call `run_test`,
`diagnose_failure`, `generate_from_spec`, and the rest of
[FR-MCP-03's tool set](../../AGENTS.md) directly -- each one shelling out to
the exact commands above. See the top-level `README.md` for the full
walkthrough.
