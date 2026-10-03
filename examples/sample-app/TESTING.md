# Testing the sample app, protocol by protocol

How to check each protocol the sample app serves: first by hand, with
a standard tool, to confirm the endpoint is up, then under load with
`vegaload`, with what a healthy run looks like. See [`README.md`](./README.md)
for what each endpoint does.

The "Expected" numbers below come from 5 VUs for 3 seconds on a laptop;
yours will differ in volume but should be close in shape.

## 0. Setup

Terminal 1, start the app:

```
cd examples/sample-app
go run .
```

It prints the three addresses it's serving:

```
sample-app listening:
  http://127.0.0.1:8080    HTTP/1.1 + h2c: GET /health, GET /widgets, GET /widgets/{id}, POST /widgets
  ws://127.0.0.1:8080/ws/echo    WebSocket echo
  127.0.0.1:9090    gRPC: grpc.health.v1.Health, widgets.v1.WidgetService
```

Terminal 2, from the repository root, build VegaLoad:

```
go build -o vegaload ./cmd/vegaload
```

Every `vegaload run` below writes and opens an HTML report. Add
`-no-open` to only write it, `-no-report` to skip it, or `-out run.json`
to keep a JSON summary for `vegaload diagnose run.json`.

The gRPC `-body` values use `$'...'`, bash/zsh syntax for writing raw
bytes. In fish or PowerShell, write those bytes another way.

---

## 1. HTTP/1.1, `http://127.0.0.1:8080`

**By hand:**

```
curl -s http://127.0.0.1:8080/widgets
curl -s -X POST http://127.0.0.1:8080/widgets -H 'Content-Type: application/json' -d '{"name":"bolt"}'
```

The first lists the three seeded widgets (sprocket, gear, cam). The second
returns the new widget, or `simulated transient failure` about 3% of the
time.

**Under load, reads:**

```
./vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 -vus 5 -duration 3s
```

Expected: about 600 requests, p50 about 25ms, p99 about 40ms (the endpoint's
10-40ms injected latency). Errors should be zero.

**Under load, writes (where the failures are):**

```
./vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -method POST -header 'Content-Type: application/json' -body '{"name":"bolt"}' \
  -vus 5 -duration 3s -out post.json
./vegaload diagnose post.json
```

Expected: about 380 requests, p50 about 40ms (20-60ms latency), and an error
rate around 3%, which is the injected 500s. `diagnose` should report it.

## 2. HTTP/2 (h2c), `http://127.0.0.1:8080`

The same port and handlers as HTTP/1.1, spoken as HTTP/2 over plain TCP.

**By hand:**

```
curl -s --http2-prior-knowledge -o /dev/null -w '%{http_version}\n' http://127.0.0.1:8080/widgets
```

Expected: `2`. Without `--http2-prior-knowledge`, curl uses HTTP/1.1 and
prints `1.1`, which also confirms the port still serves both.

**Under load:**

```
./vegaload run -target http://127.0.0.1:8080/widgets -protocol http2 -vus 5 -duration 3s
```

Expected: the same shape as the HTTP/1.1 read run (about 600 requests, p50
about 25ms). The handler is identical, so the two reports compare like for
like; any gap is protocol overhead. `-method POST` works here too.

## 3. WebSocket, `ws://127.0.0.1:8080/ws/echo`

**By hand** (with [websocat](https://github.com/vi/websocat), or any
WebSocket client):

```
websocat ws://127.0.0.1:8080/ws/echo
```

Type a line and it comes straight back. Ctrl-C to quit.

**Under load:**

```
./vegaload run -target ws://127.0.0.1:8080/ws/echo -protocol websocket -body hello -vus 5 -duration 3s
```

Each iteration opens a connection, sends `hello`, waits for the echo, and
closes. Expected: about 1,400 iterations, p50 about 10ms (the 5-15ms echo
latency plus the handshake). Errors should be zero.

Omit `-body` to measure the handshake alone (connect and close, no
message). It runs much faster, because the echo latency only applies to
messages: tens of thousands of connections in a few seconds.

That speed has a side effect worth knowing. Every iteration uses a new
TCP connection, and the operating system keeps each closed connection's
local port reserved for a while afterwards (TIME_WAIT). A handshake-only
run can use up most of the available local ports. Any run started right
after it, of any protocol that opens new connections, then fails many
iterations instantly, with near-zero latency. If you see that, wait a
minute and rerun. It's a limit of the machine running VegaLoad, not of
the sample app.

## 4. gRPC, `127.0.0.1:9090`

**By hand** (with [grpcurl](https://github.com/fullstorydev/grpcurl)). The
server has no reflection, so give grpcurl the contract with `-proto`, run
from `examples/sample-app`:

```
grpcurl -plaintext -proto widgets.proto 127.0.0.1:9090 widgets.v1.WidgetService/ListWidgets
grpcurl -plaintext -proto widgets.proto -d '{"id": 2}' 127.0.0.1:9090 widgets.v1.WidgetService/GetWidget
grpcurl -plaintext -proto widgets.proto -d '{"name": "flange"}' 127.0.0.1:9090 widgets.v1.WidgetService/CreateWidget
```

For the health service, grpcurl would need the health `.proto` too.
[grpc-health-probe](https://github.com/grpc-ecosystem/grpc-health-probe)
is simpler: `grpc-health-probe -addr=127.0.0.1:9090` should print
`status: SERVING`.

**Under load.** VegaLoad sends `-body` as already-encoded protobuf. These
are the bytes for each call. [`grpc.go`](./grpc.go)'s header comment
explains how they're built.

| Call | `-body` | Expected (5 VUs, 3s) |
|---|---|---|
| `/grpc.health.v1.Health/Check` | omit | tens of thousands of calls, sub-millisecond: no injected latency |
| `/widgets.v1.WidgetService/ListWidgets` | omit | about 600 calls, p50 about 25ms |
| `/widgets.v1.WidgetService/GetWidget` | `$'\x08\x02'` (id 2) | about 600 calls, p50 about 25ms |
| `/widgets.v1.WidgetService/CreateWidget` | `$'\x0a\x06flange'` | about 380 calls, p50 about 40ms, about 3% `UNAVAILABLE` |

For example:

```
./vegaload run -target 127.0.0.1:9090 -protocol grpc \
  -method /grpc.health.v1.Health/Check -vus 5 -duration 3s

./vegaload run -target 127.0.0.1:9090 -protocol grpc \
  -method /widgets.v1.WidgetService/CreateWidget -body $'\x0a\x06flange' \
  -vus 5 -duration 3s -out grpc-create.json
./vegaload diagnose grpc-create.json
```

To create with another name, the byte after `\x0a` must be the name's
length in bytes, written in hex: `$'\x0a\x04gear'` (4 letters),
`$'\x0a\x08sprocket'` (8 letters). A wrong length doesn't hang. It
fails every call instantly with `INVALID_ARGUMENT`, so a run with a 100%
error rate and sub-millisecond latency almost always means a bad `-body`.

---

## Cross-protocol check

All protocols share one store. After a gRPC `CreateWidget` run, the
widgets it created are visible over REST:

```
curl -s http://127.0.0.1:8080/widgets
```

Every create, from any protocol, adds to the in-memory list, so after a
long write run, `GET /widgets` and `ListWidgets` return much more data
and get slower. Restart the app to reset to the three seeded widgets
before comparing read runs.

## Unit tests

```
cd examples/sample-app
go test ./...
```

These run in-process (no running app needed) and cover each protocol.
CI runs them as a separate step, because this directory is its own Go
module and the repository's root `go test ./...` doesn't reach it.
