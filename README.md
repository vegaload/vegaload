# VegaLoad

VegaLoad is a thin, open-source load testing tool: a single static binary
with a scriptable core engine, protocol drivers (HTTP/1.1, HTTP/2, gRPC,
WebSocket, MQTT, Kafka, and raw TCP and UDP), and a self-contained HTML report — no server, no account,
no telemetry.

It's also agent-native: `vegaload init` registers VegaLoad as an MCP server
for Claude Code, Cursor, or any other MCP-capable agent host, so an agent
can run tests, read results, and explain failures through the same CLI
commands a human would type. It is never agent-*mandatory* — every feature
works from a plain terminal with nothing else installed. See `AGENTS.md` for
the design principle behind that split.

## Status

Phase 0 (core engine, CLI, protocols, scripting) and Phase 1 (the HTML/JSON
report) are done. Phase 2 (the MCP server, skill bundles, and a versioned
eval suite for the MCP tools) is also done — this README's walkthrough
covers all three. Since then, v0.3.0 and v0.4.0 added thresholds, checks,
`vegaload validate`, a baseline gate (`run -baseline`), JUnit output and a CI
job summary, and data files and env vars (`-data`, `-env`, `-secret-env`).
The main branch, which becomes v0.5.0, adds MCP over HTTP and SSE
(`mcp serve -http`), a second eval suite (`mcp eval -suite v2`), Windows
support, a Helm chart that runs a test as a Kubernetes Job with no CRD, and a
container image published to `ghcr.io/vegaload/vegaload`.
A Harness RT bridge (`--move-to-harness`) is intentionally out of scope for now.

## Install

With Homebrew (macOS and Linux), after the first tagged GitHub release:

```
brew install vegaload/tap/vegaload
vegaload version
```

`brew tap vegaload/tap` followed by `brew install vegaload` does the same
thing. Upgrade with `brew upgrade vegaload`. Until a tag exists, build from
source below.

Or build from source (Go 1.22+):

```
git clone https://github.com/vegaload/vegaload
cd vegaload
go build -o vegaload ./cmd/vegaload
./vegaload version
```

`go install github.com/vegaload/vegaload/cmd/vegaload@latest` also works
once the repository is public. Release archives for Linux, macOS and
Windows are attached to each [GitHub release](https://github.com/vegaload/vegaload/releases).

On Windows 10 or later, download the zip for your CPU from the release page,
unpack it, and run `vegaload.exe`. Put its folder on your `PATH` to run
`vegaload` from anywhere. `run`, `validate`, `doctor`, `init` and `mcp serve`
all work. Python scenarios need Python 3 from python.org (VegaLoad finds
`python3`, `python` or the `py` launcher). Scoop and winget packages are set
up in [RELEASING.md](./RELEASING.md) and come after the first Windows release.
A scratch-based Docker image is published to `ghcr.io/vegaload/vegaload` with
each release from v0.5.0 on (`linux/amd64` and `linux/arm64`):

```
docker run --rm ghcr.io/vegaload/vegaload:0.5.0 version
```

You can also build it yourself from the included `Dockerfile`
(`docker build .`). The image carries nothing but the binary
and CA certificates, so Python-scripted scenarios (which shell out to a
local `python3`) need a different base image — see the `Dockerfile`'s
comment.

## Getting started

Everything below uses [`examples/sample-app`](./examples/sample-app), a
small widgets service with injected latency and a ~3% failure rate on
creates, served over HTTP/1.1, HTTP/2, WebSocket, and gRPC, built
specifically so this walkthrough has something real to point `vegaload` at.
Start it in its own terminal first:

```
cd examples/sample-app
go run .
```

Leave it running, and do the rest from a second terminal at the repository
root.

### 1. Run a load test, no scenario file needed

Protocol-direct mode drives a target straight from CLI flags — useful for a
quick check, or for an agent that just got a URL and doesn't need to write a
script first:

```
./vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 -vus 10 -duration 30s
```

This runs 10 virtual users against the sample app for 30 seconds, prints a
summary, and writes (and opens) a self-contained HTML report — one file,
with the full latency distribution and a requests/errors-over-time chart,
nothing else to host.

### Test a raw TCP or UDP service

For a service that speaks its own protocol, use the `tcp` or `udp` driver.
Each iteration opens a connection, sends the body, optionally checks the
reply, and closes. Driver settings go in repeatable `-opt key=value` flags.

```
# Does the service answer PING with PONG? Read until the reply contains PONG.
./vegaload run -target tcp://127.0.0.1:6379 -protocol tcp \
  -body 'PING\r\n' -opt expect=PONG -vus 10 -duration 20s

# Read exactly 8 bytes back, or read until a delimiter, or only connect.
./vegaload run -target tcp://127.0.0.1:7000 -protocol tcp -body 'hello\n' -opt read=8
./vegaload run -target tcp://127.0.0.1:25 -protocol tcp -opt until='\r\n' -opt expect=220
./vegaload run -target tcp://127.0.0.1:7000 -protocol tcp

# Send one UDP datagram. Add -opt reply=true or -opt expect=... to wait for an answer.
./vegaload run -target udp://127.0.0.1:514 -protocol udp -body '<13>test message\n'
```

The `tcp` options are `read`, `until`, `expect`, `max`, `tls` and `escape`.
The `udp` options are `reply`, `expect` and `escape`. In `-body`, `expect`
and `until`, the text may use `\n`, `\r`, `\t`, `\0`, `\\` and `\xNN`. Pass
`-opt escape=false` to send backslashes as they are. Use `-opt tls=true` for
a TLS service, with `-insecure` if its certificate is not trusted. A
misspelled option is an error, not ignored. The same host allowlist and
caps apply as for HTTP targets. Each iteration opens a new connection, so a
very high rate on one machine can run out of local ports. Keep the rate
moderate, or spread the load over more machines.

### Test an MQTT broker

The `mqtt` driver does one job per iteration: connect, do the job, close. The
job is set with `-opt mode=...`. The body is the message payload.

```
# Publish a message and wait for the broker's ack (the default mode).
./vegaload run -target mqtt://127.0.0.1:1883 -protocol mqtt -body 'hello' \
  -opt topic=load/test -opt qos=1 -vus 20 -duration 30s

# Publish, and wait until the same message comes back. This measures
# delivery through the broker. {id} is a unique client id for each iteration.
./vegaload run -target mqtt://127.0.0.1:1883 -protocol mqtt -body 'ping {id}' \
  -opt mode=roundtrip -opt topic='load/{id}' -opt qos=1

# Subscribe and wait for 5 messages that contain "ok".
./vegaload run -target mqtt://127.0.0.1:1883 -protocol mqtt \
  -opt mode=subscribe -opt topic='sensors/#' -opt count=5 -opt expect=ok
```

The options are `mode` (`publish`, `subscribe`, `roundtrip`), `topic`,
`qos`, `retain`, `username`, `password_env`, `client_id`, `count`, `expect`
and `keepalive`. Put the password in an environment variable and pass its
name with `-opt password_env=NAME` (it needs `username`), so it is not on
the command line. In `roundtrip` mode the body must contain `{id}`, so each
user can tell its own message from the messages of other users. Use `mqtts://` for TLS, with `-insecure` if the certificate is not
trusted. Every iteration uses a new client id, because a broker closes an
older connection that has the same id. The same host allowlist and caps
apply as for HTTP targets.

### Test a Kafka cluster

The `kafka` driver uses a pure Go client, so there is nothing to install.
The target is `kafka://host:9092`, or `kafkas://host:9093` for TLS. The job
of each iteration is set with `-opt mode=...`:

```
# Produce: send records and wait for the broker's ack (the default mode).
./vegaload run -target kafka://127.0.0.1:9092 -protocol kafka -body '{"order":1}' \
  -opt topic=orders -opt key='user-{id}' -opt acks=all -vus 20 -duration 30s

# Roundtrip: produce, then read exactly those records back.
./vegaload run -target kafka://127.0.0.1:9092 -protocol kafka -body 'ping {id}' \
  -opt mode=roundtrip -opt topic=orders -vus 10 -duration 30s

# Consume: read 5 records from the start of the topic and check their text.
./vegaload run -target kafka://127.0.0.1:9092 -protocol kafka \
  -opt mode=consume -opt topic=orders -opt count=5 -opt expect=order

# Admin: create and delete a topic, over and over. {id} makes each name new.
./vegaload run -target kafka://127.0.0.1:9092 -protocol kafka \
  -opt mode=admin -opt action=topic_lifecycle -opt topic='load-{id}' -opt partitions=3
```

The options are `mode` (`produce`, `consume`, `roundtrip`, `admin`), `topic`,
`key`, `acks` (`all`, `leader`, `none`), `compression` (`none`, `gzip`,
`snappy`, `lz4`, `zstd`), `count`, `expect`, `from` (`start`, `end`), `action`
(`list_topics`, `create_topic`, `delete_topic`, `topic_lifecycle`,
`list_groups`, `describe_cluster`), `partitions`, `replication`, `sasl`
(`plain`, `scram-sha-256`, `scram-sha-512`), `username`, `password_env` and
`client_id`. Put the SASL password in an environment variable and pass its
name with `-opt password_env=NAME`, so it is not on the command line. An
option that the chosen mode does not use is an error.

Some points to know:

- Produce, roundtrip and admin share one client between all users, like a
  real producer. Consume, and the reading half of roundtrip, open a new
  connection for each iteration, so that time is part of the result.
- Roundtrip reads back the exact partition and offset it wrote. It does not
  use consumer groups, so it does not measure group rebalancing. Consumer
  groups are not part of this driver yet.
- The first broker is the target you give. The client then connects to the
  broker addresses that the cluster announces. The host allowlist checks only
  the first address, so make sure the announced brokers are ones you may test.
- The topic must exist, unless the broker creates topics on its own. Use the
  admin `create_topic` action first.

### 2. Or write a scenario file

```
./vegaload new my-scenario
```

scaffolds `my-scenario.vl.js` (JavaScript by default; `-python` for a
`.py` scenario shelling out to a local `python3`). A scenario's `http` and
`ws` globals make real HTTP and WebSocket calls, carrying a value from one
into the next — a script can create something over HTTP, then open a
WebSocket and read frames until it's done, the way
[`examples/scenarios/http-ws-chain.vl.js`](./examples/scenarios/http-ws-chain.vl.js)
does against the sample app (its Python twin,
[`http_ws_chain.py`](./examples/scenarios/http_ws_chain.py), does the same
thing):

```
./vegaload run -vus 5 -duration 10s examples/scenarios/http-ws-chain.vl.js
```

(flags before the scenario file — `vegaload`'s flag parser stops at the
first non-flag argument; run `vegaload run -h` for the full flag list.) A
host outside localhost needs `-allow-target` or `-yes` — the same FR-CLI-06
allowlist protocol-direct mode's `-target` uses, just enforced per call
since a script's own targets aren't known until it runs. A scenario with no
network calls at all is still useful for pure per-iteration logic against
VegaLoad's VU pool and executor shapes (fixed-VU, ramp, step,
constant-arrival-rate) —
[`examples/scenarios/smoke.vl.js`](./examples/scenarios/smoke.vl.js) is a
minimal one:

```
./vegaload run -vus 5 -duration 5s examples/scenarios/smoke.vl.js
```

To load test an actual HTTP target without writing a script at all, use
protocol-direct mode (step 1) instead — or generate a runbook of
protocol-direct commands from an OpenAPI spec:

```
./vegaload new -from-openapi examples/sample-app/openapi.json sample-app
```

writes `sample-app.vegaload-plan.md`: one ready-to-run `vegaload run`
command per endpoint the spec declares.

### Start from a browser recording

Record a real session in your browser (the network tab can save it as a
HAR file), then turn it into a scenario:

```
./vegaload import har recording.har -o checkout.vl.js
./vegaload validate -secret-env VL_COOKIE checkout.vl.js
```

The importer makes a first draft, and you edit it. It keeps the real calls,
in the order they were recorded, one `http` call each, with a check on the
status the browser got. It leaves out images, fonts, style sheets and
scripts, requests to other sites (analytics, ads, CDNs), CORS preflight
requests, and requests that failed. "Other sites" means sites other than the
one of the first page you opened in the recording. Use `-include-static`,
`-include-third-party` or `-host` to change that, and `-max N` to stop after
N requests.

The importer keeps the secrets it can recognise out of the file. A `Cookie`
or `Authorization` header, any header, query parameter or body field whose
name looks secret (password, token, api key, session, csrf and similar), and
any value that is a JWT, is read from the environment as `env.VL_NAME`. The
file lists the variables, and you pass each one by name with `-secret-env`.
It works on names and on the shape of a JWT, so a secret with an ordinary
name stays as it was recorded: a token in a path such as `/reset/<token>`, a
query parameter named `code`, or a field named `key`. Read the file before
you share it, and do not commit the HAR file. The importer also drops headers a client
sets by itself (`User-Agent`, `Content-Length`, `Referer`, `Origin`, `Sec-*`
and similar).

Some things are left for you, and the file marks them with `TODO` lines:

- A value that looks like it changes on every run (a UUID, a long number, a
  token). When the same value was in the answer to an earlier request, the
  note says which request, so you can carry it forward with `r2.json()`.
- A multipart body. It is left out.
- Waits between requests, and cookies that an answer sets. A scenario has no
  sleep and no cookie jar, so pass the `Cookie` header as a secret.

The hosts in the file are real. A host that is not localhost needs
`-allow-target` on the run, and the file's first lines say which.

### Check a scenario before a real run

```
./vegaload validate examples/scenarios/crud-flow.vl.js
```

`validate` runs the scenario once, with one user and one iteration, and says
whether it works. It catches a script that will not load (a syntax error, a
missing file, no `iteration()` in Python) and an iteration that fails (a
wrong URL, a thrown error, a host that is not allowed). It makes real
network calls, under the same host rules as `run`, so a host that is not
localhost needs `-allow-target` or `-yes`. Any `check()` results are listed,
and a failed check does not make the scenario invalid. Add `-output json`
for a machine-readable result.

| Exit code | Meaning                          |
|-----------|----------------------------------|
| 0         | The scenario loaded and its iteration ran |
| 1         | The scenario is not valid        |
| 2         | Bad usage                        |

From an agent, the `validate_scenario` tool does the same and returns
`valid`, and for an invalid scenario the `stage` (`load` or `iteration`) and
`error`.

### Feed a scenario data and settings

A scenario can read rows from a file and settings from the environment, so
each virtual user can act on different data.

```
API_KEY=demo-key ./vegaload run -vus 5 -duration 30s \
  -data users.csv -env REGION -secret-env API_KEY scenario.vl.js
```

- `-data FILE` is a CSV file (the first line names the columns) or a JSON
  file (an array of objects). The script reads it as `data.NAME`, where
  NAME is the file name without its extension: `users.csv` is `data.users`.
  Give another name with `-data name=path`. The flag can be repeated.
  `data.users.next()` takes the next row in file order, shared by all
  users, and starts again after the last row. `data.users.random()` takes
  any row. `data.users.length` is the number of rows. CSV values are
  strings. JSON values keep their types. In Python, use `len(data.users)`.
- `-env NAME` lets the script read the environment variable NAME as
  `env.NAME`. A script can only read the variables you name. It cannot see
  the rest of your environment. A variable that is not set is a usage error
  (exit 2), before any load is sent.
- `-secret-env NAME` is the same, and the value is a secret. It is taken out
  of the summary, the JSON output, the HTML report, the audit log and
  `console.log` output. Use it for tokens and passwords.

```js
const row = data.users.next();
http.post(url, { body: JSON.stringify({ user: row.name }),
                 headers: { Authorization: "Bearer " + env.API_TOKEN } });
```

```python
row = data.users.next()
http.post(url, body=json.dumps({"user": row["name"]}),
          headers={"Authorization": "Bearer " + env.API_TOKEN})
```

Secrets are removed by matching the value in text VegaLoad writes: check
names, error messages, the audit log and console output. A script that
changes a secret first, for example by encoding it, can still show the
changed text. Python scenarios are ordinary Python programs, so they can
also read the whole process environment through `os.environ`; only the
`env` object is limited to the names you give. `validate` takes the same
flags as `run`. From an agent, `run_test` and `validate_scenario` take
`data_files`, `env` and `secret_env`. See `examples/scenarios/data-env.vl.js`.

### 3. Explain a run's results

Keep a run's JSON summary alongside its HTML report with `-out`:

```
./vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -vus 10 -duration 30s -out baseline.json
./vegaload diagnose baseline.json
```

`diagnose` reports the failure rate, flags a latency long tail (p99 more
than 5x p50), and says whether failures were concentrated in one period or
spread out — plus suggested pass/fail thresholds (a p95 ceiling, an error
rate ceiling) derived from that same run, for writing into a CI gate. Add a
bring-your-own-LLM connector for a plain-English narrative on top of the
rule-based findings:

```
export VEGALOAD_LLM_PROVIDER=openai       # or anthropic, ollama
export VEGALOAD_LLM_API_KEY=sk-...
./vegaload diagnose baseline.json
```

Nothing about a run or its results leaves your machine unless you configure
this yourself (`-no-llm` skips narration even if it's configured).

### 4. Compare a later run against the baseline

After another run with `-out candidate.json`:

```
./vegaload compare baseline.json candidate.json
```

Prints deltas (error rate, latency percentiles, totals, overall RPS) and
exits non-zero if error rate or p95 latency got worse. Optional slack:

```
./vegaload compare -error-rate-delta 0.01 -p95-ratio 1.2 \
  baseline.json candidate.json
```

Checked-in fixtures under `examples/scenarios/compare/` show both an ok and
a regressed pair without needing a live target.

Or do it in one command, with the baseline as a file you keep:

```
./vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -vus 10 -duration 30s -baseline baseline.json -max-regression 10
```

The run exits 3 if p95 latency, or the error rate, is worse than the
baseline by more than 10 percent. It uses the same comparison as
`vegaload compare`. The error rate may rise by that percent of the
baseline's error rate, so a baseline with no errors allows none. Use a
`-threshold "error_rate < 1%"` for an absolute limit. Without
`-max-regression`, any increase fails. VegaLoad keeps no baseline store.
The baseline is a file you supply, from `-out`. The summary, JSON, HTML
report and audit log show the verdict. From an agent, `run_test` takes
`baseline_path` and `max_regression`.

### Gate a run on pass/fail thresholds

Add `-threshold` to make a run pass or fail on its own numbers, for example
in CI:

```
./vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -vus 10 -duration 30s \
  -threshold "p95 < 300ms" \
  -threshold "api stays up: error_rate < 1%"
```

Each threshold is an optional `name:`, a metric, an operator (`<`, `<=`,
`>`, `>=`) and a value. The metrics are `p50`, `p90`, `p95`, `p99`, `mean`,
`min`, `max` (durations such as `300ms`), `error_rate` (a fraction like
`0.01` or a percentage like `1%`), `rps`, `failed`, `total` and `check_rate` (the share of `check()` calls
that passed; see below). Every
threshold is judged once, on the finished run. A run that completed no
requests fails all of them.

The text summary, the JSON output, the HTML report and the audit log all
show each threshold's verdict. The exit code tells a script what happened:

| Exit code | Meaning                                                  |
|-----------|----------------------------------------------------------|
| 0         | The run finished and every threshold passed (or none set) |
| 1         | The run itself failed                                    |
| 2         | Bad usage, such as a threshold that cannot be parsed     |
| 3         | The run finished but broke at least one threshold        |

#### Checks

In a scenario script, `check(value, {name: test})` counts named assertions
without failing the iteration. Each test is a function or a boolean; a test
that throws counts as failed. `check` returns true only if all tests passed.

```js
check(res, { "status is 200": (r) => r.status === 200 });
```

In Python the tests are callables or bools, and a test that raises counts as
failed. The summary, JSON, and HTML report list each check's passes and
fails. Gate on them with `-threshold "check_rate >= 99%"`. A run with no
checks fails that threshold. Use fixed check names. A run keeps at most 100 distinct
names; any more are counted together as `(other checks)`. See
`examples/scenarios/checks.vl.js`.

#### JUnit XML for CI

`-junit results.xml` also writes the verdicts as JUnit XML, so a CI system
can show them as test results. Each threshold, each check and the baseline
gate is one test case. A failed threshold, a check that failed even once,
or a baseline the run is worse than is a failed test case. A failed check
does not change the exit code of `vegaload run`, but it does show as a
failed test case, because the report says what happened and the exit code
is the gate. A run with none of these writes an empty suite.

Thresholds can also come from a file, with `-thresholds gate.json`. The file
is a JSON list of `{"name", "metric", "operator", "value"}`, or the output of
`vegaload diagnose -output json`, so a baseline run can set the bar for the
next ones:

```
./vegaload diagnose -no-llm -output json baseline.json > gate.json
./vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -vus 10 -duration 30s -thresholds gate.json
```

`diagnose` also prints the same suggestion as ready-to-paste `-threshold`
flags. From an agent, `run_test` takes a `thresholds` list; a breach comes
back as a normal result with `thresholds_passed: false`.

### 5. Hand all of this to an agent

```
./vegaload init
```

writes four things into the current project, each skippable if already
present: a Claude Code skill bundle (`.claude/skills/vegaload`), a Cursor
rules file (`.cursor/rules/vegaload.mdc`), and an MCP server entry merged
into both `.mcp.json` and `.cursor/mcp.json`, pointing at this same compiled
binary running `vegaload mcp serve`. Open the project in Claude Code or
Cursor afterward and the agent has eight tools — `create_scenario`,
`run_test`, `get_results`, `suggest_thresholds`, `diagnose_failure`,
`compare_reports`, `generate_from_spec`, `validate_scenario` — each one calling the exact CLI
command shown above and parsing its `-output json` result; there is no
agent-only path that skips the CLI.

By default the MCP server talks over stdio. To use it over the network (for
example from a container), run it over HTTP:

```
vegaload mcp serve -http 127.0.0.1:8765
```

`POST /mcp` takes one JSON-RPC message and returns the answer. `GET /sse` opens
a Server-Sent Events stream (MCP protocol 2024-11-05) and `POST /message` sends
requests to it. On a loopback address no token is needed. On any other address
a token is required: put it in an environment variable and pass its name, for
example `-token-env VEGALOAD_MCP_TOKEN`. Clients then send
`Authorization: Bearer <token>`. Browser requests are refused unless their
Origin is a loopback host or the one given with `-allow-origin`. HTTP is off by
default, and stays off in the Docker image unless you pass `-http`.

`vegaload mcp eval` runs a versioned, non-LLM suite of {tool call, expected
outcome} cases against those same tools directly — the thing to run in
CI after upgrading, to check the tool layer itself still behaves, independent
of any model's tool-picking behavior:

```
./vegaload mcp eval
```

There are two suites. `v1` is the default and never changes. `v2` keeps every
v1 case and adds cases for `validate_scenario`, the baseline gate, JUnit
output and checks. Run it with `./vegaload mcp eval -suite v2`.

### 5. Check that everything is wired up

```
./vegaload doctor
```

checks that VegaLoad works from here and says how to fix anything that does
not. It looks at the CLI itself (version, `PATH`, writable folders), at each
agent host it finds (Cursor, Claude Code, and Claude Desktop on macOS), and
optionally at a target. For a host that is installed but has no VegaLoad
MCP entry, that is a warning, not a failure — using the CLI alone is
healthy. When VegaLoad *is* registered, doctor starts the configured
server, performs a real MCP handshake, and expects all eight tools. An
installed editor with no VegaLoad setup does not fail the command.

```
./vegaload doctor -target http://localhost:8080   # also check a target
./vegaload doctor -fix                            # repair what can be repaired safely
./vegaload doctor -fix -dry-run                   # show what -fix would change
./vegaload doctor -output json                    # for CI; exits 1 if any check fails
```

`-fix` only adds missing MCP entries and rules files in the project, and
rewrites a stale server command. It never overwrites a rules file you edited,
and it changes files in your home directory only when you name the host, as in
`-host cursor`. `-smoke` adds a one-user, one-second test against `-target`
(it sends real traffic, so it is off by default). `-harness` is a placeholder
until `--move-to-harness` ships: it makes no network call and cannot verify
credentials.

See [`examples/scenarios/README.md`](./examples/scenarios/README.md) for
this same walkthrough as a standalone, copy-pasteable script.

## Command reference

| Command                 | What it does                                                          |
|--------------------------|------------------------------------------------------------------------|
| `vegaload run`           | Run a load test: a scenario file, or a protocol-direct target         |
| `vegaload validate`      | Run a scenario once, with one user, to check that it works            |
| `vegaload new`           | Scaffold a starter scenario file, or a runbook from an OpenAPI spec    |
| `vegaload import har`    | Make a scenario file from a browser recording (a HAR file)             |
| `vegaload diagnose`      | Print environment info, or explain a report's results                |
| `vegaload mcp serve`     | Run an MCP server over stdio (or `-http addr`) for agent-native use   |
| `vegaload mcp eval`      | Run the versioned MCP tool-calling eval suite against this binary     |
| `vegaload init`          | Register the MCP server and skill bundles for the current project     |
| `vegaload doctor`        | Check the CLI, agent hosts, and a target; `-fix` repairs what it can   |

Every command supports `-output text` (default), `json`, or (`run` only)
`jsonl`. Run `vegaload <command> -h` for its full flag list, or `vegaload
help` for the top-level summary.

## Use in Kubernetes

A Helm chart in [`charts/vegaload`](./charts/vegaload) runs a load test as a
Kubernetes Job, close to the service you are testing. It needs no CRD and no
cluster-wide permissions, only access to one namespace. It uses the published
image `ghcr.io/vegaload/vegaload` (from v0.5.0):

```
helm install smoke ./charts/vegaload --namespace perf \
  --set run.target=http://orders.shop.svc:8080/health --set run.protocol=http1 \
  --set run.vus=5 --set run.duration=30s --set 'run.thresholds={p95 < 300ms}' \
  --wait --wait-for-jobs
kubectl logs -n perf job/smoke-vegaload-1
```

A scenario file works too: `--set-file scenario=./checkout.vl.js`. A broken
threshold fails the Job. See the chart's README. For plain `kubectl`, use
[`examples/k8s/job.yaml`](./examples/k8s/job.yaml).

## Use in GitHub Actions

This repository is also a GitHub Action. It installs a released VegaLoad
(Linux and macOS runners) and runs it, and the step fails when `vegaload`
exits with a non-zero code. With `-threshold`, that means a broken
threshold fails the build:

```yaml
- uses: vegaload/vegaload@v0.3.0
  with:
    args: >-
      run -target https://staging.example.com/health -protocol http1
      -vus 20 -duration 1m -allow-target staging.example.com
      -threshold "p95 < 300ms" -threshold "error_rate < 1%"
```

Inside GitHub Actions, `vegaload run` also appends a summary to the job's
page: the key numbers, and the verdict of each threshold, check and
baseline gate. It does this on its own, because GitHub sets
`GITHUB_STEP_SUMMARY`. Pass `-no-step-summary` to turn it off. Add `-junit`
to write JUnit XML as well, which many CI systems show as test results:

```yaml
- uses: vegaload/vegaload@v0.3.0
  with:
    args: >-
      run -target https://staging.example.com/health -protocol http1
      -vus 20 -duration 1m -allow-target staging.example.com
      -threshold "p95 < 300ms" -junit vegaload-junit.xml
- uses: actions/upload-artifact@v4
  if: always()
  with:
    name: vegaload-junit
    path: vegaload-junit.xml
```

- `args` is what you would type after `vegaload`, with the same quoting.
  Leave it out to only install, then call `vegaload` in later steps.
- The action installs the version you pin after the `@`. Set `version:` to
  install a different one. A branch name such as `@main` installs the
  latest release.
- The archive is checked against the release's `checksums.txt` before it
  is used. Nothing is sent anywhere except the downloads from the GitHub
  release.
- The step's `version` output is the installed version.

## License

Apache-2.0. See `LICENSE`.
