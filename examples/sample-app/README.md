# sample-app

A tiny service to load test, so VegaLoad's getting-started walkthrough
(see [`../README.md`](../README.md)) has something real to point at instead
of a placeholder URL. It serves the same in-memory "widgets" data over all
four protocols VegaLoad speaks, so every driver has a target.

It is not part of VegaLoad itself -- it has its own `go.mod` -- it's just a
target worth running `vegaload` against.

## Running it

```
cd examples/sample-app
go run .                                   # HTTP on 127.0.0.1:8080, gRPC on 127.0.0.1:9090
go run . -addr :8081 -grpc-addr :9091      # or pick your own addresses
```

Leave it running in one terminal, then drive load at it with `vegaload` from
another. [`TESTING.md`](./TESTING.md) walks through each protocol: a quick
check by hand, the `vegaload` command, and what a healthy run looks like.
[`../scenarios/README.md`](../scenarios/README.md) has the full walkthrough,
including `diagnose` and the OpenAPI runbook.

## What it serves

Latency and failures are injected on purpose, so a run has something for
`vegaload diagnose` to say. Every endpoint is backed by one shared store: a
widget created over gRPC shows up over REST, and the other way round.

### HTTP/1.1 and HTTP/2 -- `http://127.0.0.1:8080`

The same port speaks HTTP/1.1 and HTTP/2 over plain TCP (h2c), so
`-protocol http1` and `-protocol http2` hit identical handlers.

| Method | Path            | Behavior                                                   |
|--------|-----------------|------------------------------------------------------------|
| GET    | `/health`       | always `200`, near-zero latency                            |
| GET    | `/widgets`      | lists widgets, 10-40ms simulated latency                   |
| GET    | `/widgets/{id}` | one widget, or `404` if the id doesn't exist               |
| POST   | `/widgets`      | creates a widget, 20-60ms latency, and a ~3% random `500`  |

[`openapi.json`](./openapi.json) describes these four operations, for the
`generate_from_spec` / `vegaload new -from-openapi` walkthrough.

### WebSocket -- `ws://127.0.0.1:8080/ws/echo`

Echoes every message back, with 5-15ms simulated latency per message. That
matches VegaLoad's WebSocket driver (connect, send `-body`, wait for one
reply); a client that keeps the connection open and sends many messages
works too. Connections from a browser page on another origin are refused.

### gRPC -- `127.0.0.1:9090`

| Method                                       | Behavior                                                          |
|----------------------------------------------|-------------------------------------------------------------------|
| `/grpc.health.v1.Health/Check`               | the standard health service; always `SERVING`                     |
| `/widgets.v1.WidgetService/ListWidgets`      | all widgets, 10-40ms latency                                      |
| `/widgets.v1.WidgetService/GetWidget`        | one widget, or `NOT_FOUND`; 10-40ms latency                       |
| `/widgets.v1.WidgetService/CreateWidget`     | 20-60ms latency, ~3% `UNAVAILABLE`; `INVALID_ARGUMENT` if no name |

[`widgets.proto`](./widgets.proto) is the contract. The server has no
generated code (no `protoc` needed to work on it); it reads and writes
plain protobuf bytes, so any gRPC client works against it -- for example
`grpcurl -plaintext -proto widgets.proto 127.0.0.1:9090 widgets.v1.WidgetService/ListWidgets`.

VegaLoad's gRPC driver sends `-body` as already-encoded protobuf bytes.
For this service those are short enough to write by hand in bash or zsh:

| Request                            | `-body`                        |
|------------------------------------|--------------------------------|
| `Health/Check`, `ListWidgets`      | omit it (an empty message)     |
| `GetWidget { id: 2 }`              | `$'\x08\x02'`                  |
| `CreateWidget { name: "flange" }`  | `$'\x0a\x06flange'`            |

For `CreateWidget`, the byte after `\x0a` is the name's length in bytes
(`\x06` for the six letters of "flange"); a wrong length is rejected as
`INVALID_ARGUMENT`.

## Tests

```
go test ./...
```

covers each protocol: h2c and HTTP/1.1 on the shared port, the WebSocket
echo, and every gRPC method, checked with real protobuf messages built
from `widgets.proto` so the hand-written encoding can't drift from the
contract unnoticed.
