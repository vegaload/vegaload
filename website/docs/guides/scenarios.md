---
title: "Write a scenario file"
description: "`vegaload new my-scenario` scaffolds `my-scenario.vl.js` (JavaScript by default; `-python` for a `.py` scenario shelling out to a local `python3`)."
sidebar_position: 12
---

# Write a scenario file

```
vegaload new my-scenario
```

scaffolds `my-scenario.vl.js` (JavaScript by default; `-python` for a
`.py` scenario shelling out to a local `python3`). A scenario's `http` and
`ws` globals make real HTTP and WebSocket calls, carrying a value from one
into the next — a script can create something over HTTP, then open a
WebSocket and read frames until it's done, the way
[`examples/scenarios/http-ws-chain.vl.js`](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/http-ws-chain.vl.js)
does against the sample app (its Python twin,
[`http_ws_chain.py`](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/http_ws_chain.py), does the same
thing):

```
vegaload run -vus 5 -duration 10s examples/scenarios/http-ws-chain.vl.js
```

(flags before the scenario file — `vegaload`'s flag parser stops at the
first non-flag argument; run `vegaload run -h` for the full flag list.) A
host outside localhost needs `-allow-target` or `-yes` — the same FR-CLI-06
allowlist protocol-direct mode's `-target` uses, just enforced per call
since a script's own targets aren't known until it runs. A scenario with no
network calls at all is still useful for pure per-iteration logic against
VegaLoad's VU pool and executor shapes (fixed-VU, ramp, step,
constant-arrival-rate) —
[`examples/scenarios/smoke.vl.js`](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/smoke.vl.js) is a
minimal one:

```
vegaload run -vus 5 -duration 5s examples/scenarios/smoke.vl.js
```

To load test an actual HTTP target without writing a script at all, use
[protocol-direct mode](/get-started/first-test) instead — or generate a runbook of
protocol-direct commands from an OpenAPI spec:

```
vegaload new -from-openapi examples/sample-app/openapi.json sample-app
```

writes two files. `sample-app.vl.js` is a runnable scenario that calls every
operation of the spec once per iteration, each as a named step (add `-python`
for a Python scenario; flags go before the name). It sends the example bodies
and required query parameters from the spec, reads a credential from `env`
(give it with `-secret-env`), and uses the id that a create returns in the calls
under it. A spec describes each endpoint on its own, so the order (creates,
reads, updates, deletes) is a guess: the file says so, and marks the ids and
other values it could not fill. Check it with `vegaload validate` and edit it to
fit your real flow. `sample-app.vegaload-plan.md` is a runbook with one
ready-to-run `vegaload run` command per endpoint, for loading one endpoint at a
time.

## What a scenario run counts

Each call a scenario makes is one request: an HTTP request, a query, a
publish. `total`, `failed`, the error rate, the latency and the time series
are those calls. An iteration is one run of the default function (or of
`iteration()` in Python). Iterations are counted on their own, under
`iterations` in the summary and the JSON: how many ran, how many failed, and
how long a whole iteration took. A scenario that makes no calls, such as
`smoke.vl.js`, has iterations and no requests.

An HTTP call fails on a status of 400 or higher, the same rule as
`-protocol http1`. The script still gets the answer, and the iteration goes
on. To accept another status for one call, list the statuses it expects:

```js
const r = http.get(base + "/widgets/999", { expectedStatuses: [404] });
```

```python
r = http.get(base + "/widgets/999", expected_statuses=[404])
```

The list replaces the default rule for that call, so `[200]` makes a 302 a
failure too. `-timeout` is the time limit for each call. An HTTP call can
set its own with `timeout`, and also takes `redirects` and `params`. See
[Request options](/guides/http#request-options).

Each failed request has a reason: `timeout`, `status 503` (the status it
got), `connection refused`, `connection reset`, `dns`, `tls` or `error`. The
summary prints them as `failures:`, and the JSON has them as `failure_reasons`.

## Groups

A group is a step that also names the HTTP calls inside it. `group(name, fn)` runs `fn` and returns its value:

```js
const order = group("checkout", () => {
  http.post(base + "/cart", { body: cart });    // row: POST checkout
  return http.post(base + "/orders").json();    // row: POST checkout
});
```

In Python, `with group("checkout"):` names a block, and `group("checkout", fn)` runs a function and returns its value:

```python
with group("checkout"):
    http.post(base + "/cart", body=cart)
    order = http.post(base + "/orders").json()
```

Each HTTP call inside is reported as `METHOD checkout`, unless the call has its own `name` or uses `http.url`. When groups nest, the innermost one names the calls. Calls over other protocols keep their own names.

A group is timed like a step: it is in the steps table, and `-threshold 'p95{step="checkout"} < 500ms'` gates it. A group that throws (or raises) fails like a step. See [Endpoint rows](/guides/http#endpoint-rows) and [Steps](/guides/thresholds#steps).

## Call other protocols from a scenario

The full list, with options and examples, is in [Supported protocols](/guides).

A JavaScript scenario can also use the `tcp`, `udp`, `mqtt`, `kafka`, `grpc`, `postgres`, `mysql`, `redis`, `rabbitmq`, `ftp` and `sftp` globals. Each
function is one call that does one job and returns what it read:

```js
export default function () {
  const r = tcp.send("tcp://localhost:6379", { body: "PING\r\n", until: "\r\n" });
  check(r, { "got PONG": (x) => x.ok && x.body === "+PONG\r\n" });

  mqtt.publish("mqtt://localhost", { topic: "sensors/1", body: "21.5", qos: 1 });
  const m = mqtt.subscribe("mqtt://localhost", { topic: "sensors/#", count: 2, timeout: "5s" });
  console.log(m.messages.map((x) => x.topic + "=" + x.body).join(", "));
}
```

- `tcp.send(url, options)` and `udp.send(url, options)`: the options are the
  same as the `-opt` keys in the [TCP and UDP guide](/guides/tcp-udp) (`until`, `read`,
  `expect`, `tls`, `reply`, and so on), plus `body`. The reply is `r.body`.
  A script's text is sent as written: backslash escapes are off unless you
  pass `escape: true`.
- `mqtt.publish`, `mqtt.subscribe` and `mqtt.roundtrip` take `topic`, `body`
  and the other MQTT `-opt` keys (not `mode`: the function sets it). The
  messages that were read are in `r.messages`, each with `topic` and `body`.
  Give a password as `password: env.MQTT_PASSWORD` and run with
  `-secret-env MQTT_PASSWORD`, so it is removed from every output.
- `kafka.produce`, `kafka.consume`, `kafka.roundtrip` and `kafka.admin` take
  the Kafka `-opt` keys (`topic`, `key`, `count`, `expect`, `from`, `action`,
  `sasl`, `username`, `acks`, and so on; not `mode`) plus `value` (the record
  value, `body` means the same). The records are in `r.records`, each with
  `topic`, `partition`, `offset`, `key` and `value`. For `produce`, the
  partition and offset say where the broker stored the record. An admin
  answer is in `r.text`. A Kafka client is made on the first call and kept
  for the rest of that virtual user's run, so `produce` does not connect again
  for every call. `consume` and `roundtrip` read with their own connection,
  as in the load-test mode. A SASL password is `password: env.NAME`.
  The safety allowlist checks the first broker you name, not the other
  brokers the cluster announces. The kept client stops at the longest timeout
  it was built with (the first call's `timeout`, and at least one minute), so a
  single Kafka call cannot run longer than that.
- `grpc.call(url, options)` makes one unary gRPC call. The options are
  `method` (required, such as `"/package.Service/Method"`), `body` (an object,
  or JSON text; leave it out for an empty message) and `headers` (an object,
  sent as gRPC metadata). The reply has `status` (the gRPC code, 0 when it
  worked), `statusName` (such as `"NotFound"`), `body` (the reply as JSON
  text) and `json` (the same, already parsed). The script needs no `.proto`
  file: the first call to a method asks the server for its message types
  (server reflection), and the answer is kept. A JSON body needs the server to
  offer reflection. Sent to a server without it, the call throws. Call such a
  server with `encoding: "base64"`: `body` and the reply are then the encoded
  message in base64. A server's error status is a reply with `ok` false, not
  an exception. Only unary methods work for now. `url` is `host:port`,
  `grpc://host:port` or `grpcs://host:port` for TLS. `grpcs://` takes the four options under "TLS with your own CA". One connection is made on
  the first call and kept for the rest of that virtual user's run.
- `postgres.query(url, options)` runs one SQL text, given as `body`, and takes
  the other PostgreSQL `-opt` keys (`username`, `database`, `sslmode`, `args`,
  `min_rows`, `expect`, `max_rows`, `query_mode`, `allow_writes`, `pool`). `args`
  is a list: `args: [42, "paid"]`. The reply has `rows` (a list of objects, one
  per row, keyed by column name, up to `max_rows`), `columns` (the names, in
  order), `rowCount` (all the rows, also those past `max_rows`),
  `rowsAffected` and `commandTag` (such as `"SELECT 2"`). Numbers, text,
  booleans and NULL (`null`) keep their type, and JSON columns are parsed.
  Times are text in UTC (RFC 3339), and `numeric`, `uuid` and byte columns are
  text, so no precision is lost. If two columns have the same name, the last
  one wins: give them different names with `as`. Give the password as
  `password: env.DB_PASSWORD` and run with `-secret-env DB_PASSWORD`. A pool
  with one connection is made on the first call and kept for the rest of that
  virtual user's run, so each call does not connect again. A server error
  (a bad SQL, a failed constraint) is a reply with `ok` false and the
  SQLSTATE code in `error`, not an exception. Scripts are read-only like the
  command line: a script that writes passes `allow_writes: true`.
- `mysql.query(url, options)` runs one SQL text, given as `body`, and takes
  the other MySQL `-opt` keys (`username`, `database`, `tls`, `args`,
  `min_rows`, `expect`, `max_rows`, `query_mode`, `allow_writes`, `pool`).
  Placeholders are `?`, and `args` is a list: `args: [42, "paid"]`. The reply
  has `rows`, `columns`, `rowCount` and `rowsAffected`, the same as
  PostgreSQL, plus `lastInsertId` for a single insert. It has no
  `commandTag`. A text of several statements reports `rowsAffected` as 0.
  Give the password as `password: env.DB_PASSWORD` and run with
  `-secret-env DB_PASSWORD`. A pool with one connection is made on the first
  call and kept for the rest of that virtual user's run. A server error is a
  reply with `ok` false, and the error number and SQLSTATE are in `error`.
  Scripts are read-only like the command line: a script that writes passes
  `allow_writes: true`. `url` may be `mysql://` or `mariadb://`.
- `redis.command(url, options)` runs one Redis command, given as `body`, or
  several, one per line. It takes the other Redis `-opt` keys (`username`,
  `database`, `tls`, `pool`, `protocol`, `allow_writes`, `allow_admin`,
  `transaction`, `args`, `min_rows`, `expect`, `max_rows`). `args` is a
  list: `args: ["user:42", "ann"]`. A `?` is one whole argument. The reply
  has `value` (the last command), `values` (one entry per command) and
  `rowCount`. In Python, `r.values` is that list (and `r["values"]` is the
  same). It has no `rows`, `columns`, `rowsAffected`, `commandTag` or
  `lastInsertId`. Give the password as `password: env.REDIS_PASSWORD` and
  run with `-secret-env REDIS_PASSWORD`. A client with one connection is
  made on the first call and kept for the rest of that virtual user's run.
  A server error, and a command the read-only check refuses, is a reply
  with `ok` false, not an exception. A script that writes passes
  `allow_writes: true` in the connection options. Admin commands also pass
  `allow_admin: true`. `url` may be `redis://` or `rediss://`.
- `rabbitmq.publish`, `rabbitmq.consume`, `rabbitmq.roundtrip` and
  `rabbitmq.admin` take the RabbitMQ `-opt` keys (not `mode`: the function
  sets it) plus `body`. The messages that were read are in `r.messages`,
  each with `exchange`, `routingKey`, `body`, `messageId` and
  `redelivered`. An admin answer is in `r.text`. Give the password as
  `password: env.RABBITMQ_PASSWORD` and run with
  `-secret-env RABBITMQ_PASSWORD`. A connection is made on the first call
  and kept for the rest of that virtual user's run. A server error, and a
  call the safety check refuses, is a reply with `ok` false, not an
  exception. A script that removes messages passes `allow_writes: true`.
  Admin actions that delete or purge also pass `allow_admin: true`. `url`
  may be `amqp://` or `amqps://`.
- `ftp.connect`, `ftp.download`, `ftp.upload`, `ftp.list`, `ftp.stat`,
  `ftp.delete` and `ftp.roundtrip` take the FTP `-opt` keys (not `mode`:
  the function sets it) plus `body`. The names that were read are in
  `r.entries`, each with `name`, `type`, `size` and `time`. `r.size` is
  the file size, `r.total` is how many names the listing had, `r.timing`
  is the parts of the call in milliseconds, and `r.text` is the short
  answer. Give the password as `password: env.FTP_PASSWORD` and run with
  `-secret-env FTP_PASSWORD`. A session pool is made on the first call and
  kept for the rest of that virtual user's run. A server error, and a call
  the safety check refuses, is a reply with `ok` false, not an exception.
  A script that writes passes `allow_writes: true` in the connection
  options. A delete, or an upload to a fixed path, also passes
  `allow_admin: true`. `url` may be `ftp://` or `ftps://`.
- `sftp.connect`, `sftp.download`, `sftp.upload`, `sftp.list`, `sftp.stat`,
  `sftp.delete` and `sftp.roundtrip` take the SFTP `-opt` keys (not `mode`:
  the function sets it) plus `body`. The reply fields are the same as
  `ftp`. Give the password as `password: env.SFTP_PASSWORD` and a key
  passphrase as `key_passphrase: env.SFTP_PP`. `password_env` and
  `key_passphrase_env` are refused. `key_file` is a path. One set of SSH
  connections is made on the first call and kept for the rest of that
  virtual user's run. A server error, and a call the safety check
  refuses, is a reply with `ok` false, not an exception. A script that
  writes passes `allow_writes: true` in the connection options. A delete,
  or an upload to a fixed path, also passes `allow_admin: true`. `url`
  is `sftp://`. `insecure: true` skips the host key check. TLS options
  are refused.
- Every function also accepts `insecure` (skip TLS checks; for SFTP this skips the host key check) and `timeout`
  (milliseconds, or text such as `"2s"`).
- Every call returns `{ok, error, bytesSent, bytesReceived, body, messages}`. Kafka calls also return `records` and `text`, gRPC calls return `status`, `statusName` and `json`, PostgreSQL calls return `rows`, `columns`, `rowCount`, `rowsAffected` and `commandTag`, MySQL calls return `rows`, `columns`, `rowCount`, `rowsAffected` and `lastInsertId`, Redis calls return `value`, `values` and `rowCount`, and RabbitMQ calls return `messages` (each `{exchange, routingKey, body, messageId, redelivered}`) and `text`, and FTP and SFTP calls return `entries` (each `{name, type, size, time}`), `size`, `total`, `timing` and `text`.
  A network failure does not throw: `ok` is false and `error` says why. A
  call that is set up wrongly (an unknown option, a missing topic) does throw.
- The safety allowlist is checked for the host of every call, like `http`.
- Each call opens its own connection and closes it, like the load-test mode.
  For `mqtt.subscribe`, the message must arrive during the call, so run it
  when something else publishes.

Python scenarios have the same globals, with the same options and the
same reply. The options are keyword arguments, the reply works as
`r.ok` and as `r["ok"]`, and a word that Python reserves gets a trailing
underscore (`from_="end"`):

```python
def iteration():
    r = tcp.send("tcp://localhost:6379", body="PING\r\n", until="\r\n")
    check(r, {"got PONG": lambda x: x.ok and x.body == "+PONG\r\n"})
    k = kafka.produce("kafka://localhost", topic="orders", key="k1", value="v")
    print(k.records[0].partition, k.records[0].offset)
    g = grpc.call("localhost:50051", method="/pkg.Greeter/SayHello", body={"name": "x"})
    check(g, {"greeted": lambda x: x.ok and x.json.message == "Hello x"})
```

`grpc.call` sends and reads JSON, which needs server reflection on the
server. Without it, the call throws. For such a server use
`encoding="base64"`, and send the encoded message in base64.

A call set up wrongly raises `VegaloadError`.

[`examples/scenarios/mqtt-tcp.vl.js`](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/mqtt-tcp.vl.js)
(and its Python twin, [`mqtt_tcp.py`](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/mqtt_tcp.py))
is a small example, and
[`examples/scenarios/kafka-orders.vl.js`](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/kafka-orders.vl.js)
shows Kafka, and
[`examples/scenarios/mixed-protocols.vl.js`](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/mixed-protocols.vl.js)
(Python: [`mixed_protocols.py`](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/mixed_protocols.py)) mixes
HTTP, gRPC and WebSocket in one flow, and
[`examples/scenarios/postgres-orders.vl.js`](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/postgres-orders.vl.js)
(Python: [`postgres_orders.py`](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/postgres_orders.py))
writes and reads rows in PostgreSQL, and
[`examples/scenarios/mysql-orders.vl.js`](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/mysql-orders.vl.js)
(Python: [`mysql_orders.py`](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/mysql_orders.py))
reads and writes a MySQL row.
[`examples/scenarios/redis-cache.vl.js`](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/redis-cache.vl.js)
(Python: [`redis_cache.py`](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/redis_cache.py))
reads a cache key, fills it on a miss, and updates a counter.
