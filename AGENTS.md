# AGENTS.md — VegaLoad

This file tells any AI coding agent (Claude Code, Cursor, or similar) how to work in this repository. Read this before making any change.

## What this project is

VegaLoad is a thin, open-source load testing tool. It plays the same role for Harness RT's load testing that LitmusChaos plays for chaos engineering: a simple tool people adopt on their own, with a smooth path into the enterprise product later.

Full requirements live in the PRD, not here. This file covers how to work in the code, not what the product does.

Licence: Apache-2.0.

## The one design principle that overrides everything else

VegaLoad is agent-native. It is not agent-mandatory.

This means three things. Follow all three on every change:

1. **Every test is a real file, never chat state.** A test an agent creates must be written to disk as an actual scenario file in the user's repo. Never hold a test only in conversation memory.
2. **The CLI always works alone.** Every feature must work from a plain terminal, with no agent present. If a feature only works through an agent, that is a bug.
3. **MCP tools mirror CLI commands.** Every MCP tool must call the same underlying CLI command a human can type by hand. Do not build a richer, agent-only path. That means building two products instead of one.

If a proposed change breaks any of these three rules, stop and flag it instead of proceeding.

## Language and runtime

- Core engine: Go. Single static binary. Cross-compiles to Linux, macOS, Windows, and a scratch Docker image.
- Scripting: JavaScript/TypeScript, run through an embedded Go JS interpreter (Goja). No Node.js dependency for this path.
- Python scripting is a second-class, optional path. It shells out to a local `python3` process. It requires Python installed on the user's machine. Never claim this path is dependency-free.
- The MCP server is also Go, built as a subcommand (`vegaload mcp serve`), not a separate Node.js process. This keeps the single-binary story intact.

## Module boundaries

Keep these modules separate. Do not let one reach into another's internals.

- **Core engine**: the VU scheduler and load-shape executors (fixed-VU, ramp, step, constant-arrival-rate). Knows nothing about MCP, reporting, or Harness RT.
- **Protocol drivers**: HTTP/1.1, HTTP/2, gRPC, WebSocket, MQTT, Kafka, PostgreSQL, MySQL, Redis, raw TCP and UDP. Each implements a shared `Protocol` interface. Adding a new protocol should never require editing the core engine.
- **Importers**: `internal/har` turns a HAR recording into a scenario file. It only reads a recording and writes text. It must not import the engine, the drivers, or the scripting runtimes. `vegaload import` is the only command that uses it.
- **Output/reporting**: the self-contained HTML report, plus optional JSON/Parquet export. Implements a shared `Output` interface.
- **MCP layer**: calls the same CLI commands a human would run, then parses their output. It must not call core-engine functions directly. If the MCP layer needs new data, add a CLI flag or output mode first, then have MCP use it.
- **Doctor and host adapters**: `internal/doctor` holds the checks and `internal/hosts` holds where each agent host keeps its files. `vegaload init` and `vegaload doctor` both read from `internal/hosts`, so they cannot disagree. To support a new host, add one adapter there. Do not add host paths anywhere else.
- **Streaming and the live view**: `internal/stream` (events, queues, file and push sinks) and `internal/live` (the live view's identity, probe and browser opener). The contract is `docs/streaming.md`; the code follows it, and a change to one is a change to the other. The core engine and `internal/report` know nothing about streaming or any transport. The runner (`cmd/vegaload/runstream.go`) hands plain values to a `stream.Emitter`, and `report.Collector` only offers `LiveBuckets()`, per-second buckets computed with the same arithmetic as the final time series. A run with no stream has no `Emitter`, and the engine never waits on a sink.
- **Harness bridge** (`--move-to-harness`): talks to the Harness RT API. Lives in its own package. The core engine must have zero awareness that Harness RT exists.

When in doubt about where new code belongs, put it in the smallest module that needs it, not in core.

## Conventions

- Format Go code with `gofmt` before committing. No exceptions.
- Write tests alongside the code they test (`_test.go`), not in a separate top-level test tree.
- Tests must not open a socket per call in a free-running loop. Drivers like tcp, ftp, sftp and rabbitmq open a new socket for each call. On Windows, this can use up all ephemeral ports and make CI flaky. Pace such tests with `-executor constant-arrival-rate -rate N -max-vus M` instead of `-vus N -duration D`. For a missing-file check, prefer `mode=stat` for ftp. It reuses the control connection.
- Every new CLI flag needs a corresponding line in `vegaload --help` output and in the docs. Don't ship an undocumented flag.
- Never name the underlying scripting engine (Goja) in user-facing CLI output, error messages, or docs. Describe VegaLoad by the languages it supports: JavaScript, TypeScript, Python. This matches the existing Harness messaging rule.
- Pass/fail limits on a run are called **thresholds** everywhere users read them: flags, output, errors, docs. Do not use Harness RT terms (probe, hypothesis, resilience score) for them. A threshold stays a plain name, metric, operator and value judged on the run's own numbers. Anything richer belongs to Harness RT, not here.
- Commit messages: one line, present tense, describing what changed and why, not a log of commands run.

## Commands an agent should know

- `vegaload run <file>` — run a scenario.
- `vegaload new -from-openapi <spec> [name]` — write a runnable scenario from a JSON OpenAPI spec (`-python` for Python), with every operation as a named step, plus a runbook of one `vegaload run` command per endpoint. Flags go before the name. The generator is `internal/openapi/scenario.go`; the order (creates, reads, updates, deletes) and the id wiring are rules, not something the spec says, so the file tells the user to edit it.
- `vegaload run -threshold "p95 < 300ms" ...` — make a run pass or fail on its own numbers. A breach exits 3; `-thresholds <file>` loads them from JSON. A selector can name a protocol or one operation: `p95{protocol="ftp"} < 500ms`, `error_rate{protocol="ftp",op="roundtrip"} < 1%`. `check_rate` cannot. A protocol that recorded no calls fails its threshold.
- In scenarios, `check(value, {name: test})` counts named assertions without failing the iteration; gate with `-threshold "check_rate >= 99%"`.
- In scenarios, `step(name, fn)` (Python: `with step(name):`) names a stage; the summary, JSON and HTML show latency and error rate per step, and `-threshold 'p95{step="login"} < 300ms'` gates one step.
- `vegaload validate <file>` — run a scenario once, with one user, to check that it works before a real run. Exit 0 valid, 1 not valid.
- `vegaload run -baseline <report.json> -max-regression <percent>` — compare the run with a baseline file you supply. Exit 3 if p95 or the error rate is worse by more than that percent. There is no baseline store.
- `vegaload run -abort-on-breach -threshold ...` — stop the run early once a threshold is broken beyond recovery, and exit 3. `failed < N` stops at once; p95 and error_rate must stay broken for 3s after a warm-up (`-abort-grace`). rps and `total >=` wait for the end.
- `vegaload run -junit <file>` — also write JUnit XML: each threshold, check and the baseline gate is one test case. Inside GitHub Actions, `run` also appends a Markdown summary to the job page (`-no-step-summary` turns that off).
- JavaScript and Python scenarios have `http`, `ws`, `tcp`, `udp`, `mqtt`, `kafka`, `grpc`, `postgres`, `mysql`, `redis`, `rabbitmq`, `ftp` and `sftp` globals. The `tcp`/`udp`/`mqtt`/`kafka`/`grpc`/`postgres`/`mysql`/`redis`/`rabbitmq`/`ftp`/`sftp` ones are one call per action, return `{ok, error, ...data}` and never throw for a network failure. They live in `internal/scripting/netapi/proto.go` and call the same drivers as the load-test mode (`Run` returns the data a script needs). Kafka clients are kept per VU and closed with it. `internal/protocol/kafka/kafkatest` is a fake broker that tests in any package may import. The list of functions is `netapi.ProtoFunctions()` and the option parsing is `netapi.ProtoCallFromArgs`: both languages use them, so a new protocol function is added once in `netapi` and appears in both. `internal/protocol/mqtt/mqtttest` is a small fake MQTT broker for the same purpose. `postgres.query` runs one SQL text and returns `rows` (objects keyed by column name), `columns`, `rowCount`, `rowsAffected` and `commandTag`; the pool is kept per VU like the Kafka client, and `internal/protocol/postgres/postgrestest` is a fake server (simple query protocol only) for tests in any package. `internal/protocol/postgres/postgres_live_test.go` runs against a real server when `VEGALOAD_TEST_POSTGRES` is set. `mysql.query` runs one SQL text the same way and returns `rows`, `columns`, `rowCount`, `rowsAffected` and `lastInsertId`. Placeholders are `?`. The pool is kept per VU. `internal/protocol/mysql/mysqltest` is a fake server (text protocol only) for tests in any package. `internal/protocol/mysql/mysql_live_test.go` runs against a real MySQL or MariaDB server when `VEGALOAD_TEST_MYSQL` is set; the password comes from the environment variable named by `VEGALOAD_TEST_MYSQL_PASSWORD_ENV`. `-protocol` stays `mysql`; `mariadb://` is only a URL scheme. `redis.command` runs one command, or several as a pipeline, and returns `value`, `values` and `rowCount`. Placeholders are `?`. The client is kept per VU. `internal/protocol/redis/redistest` is a fake server (RESP2 text protocol) for tests in any package. `internal/protocol/redis/redis_live_test.go` runs against a real Redis or Valkey server when `VEGALOAD_TEST_REDIS` is set; the password comes from the environment variable named by `VEGALOAD_TEST_REDIS_PASSWORD_ENV`. `-protocol` stays `redis`; `rediss://` is only a URL scheme for TLS. `rabbitmq.publish`, `rabbitmq.consume`, `rabbitmq.roundtrip` and `rabbitmq.admin` publish and consume AMQP 0-9-1 messages and return `messages` and `text`. The client is kept per VU. `internal/protocol/rabbitmq/rabbitmqtest` is a fake broker (AMQP 0-9-1) for tests in any package. `internal/protocol/rabbitmq/rabbitmq_live_test.go` runs against a real RabbitMQ server when `VEGALOAD_TEST_RABBITMQ` is set; the user comes from `VEGALOAD_TEST_RABBITMQ_USER` and the password from the environment variable named by `VEGALOAD_TEST_RABBITMQ_PASSWORD_ENV`. `-protocol` stays `rabbitmq`; `amqps://` is only a URL scheme for TLS. `ftp.connect`, `ftp.download`, `ftp.upload`, `ftp.list`, `ftp.stat`, `ftp.delete` and `ftp.roundtrip` speak FTP and FTPS and return `entries`, `size`, `total`, `timing` and `text`. A session pool is kept per VU and closed with it. `internal/protocol/ftp/ftptest` is an in-memory FTP server for tests in any package. `internal/protocol/ftp/ftp_live_test.go` runs against a real server when `VEGALOAD_TEST_FTP` is set; the user comes from `VEGALOAD_TEST_FTP_USER` and the password from the environment variable named by `VEGALOAD_TEST_FTP_PASSWORD_ENV`. Live tests for FTP, Redis, RabbitMQ, MySQL and PostgreSQL also read `VEGALOAD_TEST_<NAME>_CA_FILE`, `_CERT_FILE`, `_KEY_FILE` and `_TLS_SERVER_NAME`, where `<NAME>` is `FTP`, `REDIS`, `RABBITMQ`, `MYSQL` or `POSTGRES`. The tests copy these into the options `ca_file`, `cert_file`, `key_file` and `tls_server_name`. `-protocol` stays `ftp`; `ftps://` is implicit TLS on port 990. `sftp.connect`, `sftp.download`, `sftp.upload`, `sftp.list`, `sftp.stat`, `sftp.delete` and `sftp.roundtrip` speak SFTP and return the same fields as FTP. One set of SSH connections is kept per VU and closed with it. `internal/protocol/sftp/sftptest` is an SSH server with an SFTP subsystem for tests in any package. `internal/protocol/sftp/sftp_live_test.go` runs against a real server when `VEGALOAD_TEST_SFTP` is set; the password comes from the environment variable named by `VEGALOAD_TEST_SFTP_PASSWORD_ENV`. `-protocol` stays `sftp`. `ssh://` is refused. `grpc.call` is JSON in and out: `internal/protocol/grpc/script.go` encodes it with message types fetched by server reflection (`reflect.go`), and `internal/protocol/grpc/grpctest` is a test server (health service, with or without reflection) for any package.
- `vegaload run -data <file> -env <NAME> -secret-env <NAME>` — scenarios read rows from a CSV/JSON file as `data.NAME.next()` / `.random()`, and exposed environment variables as `env.NAME`. A `-secret-env` value is removed from every output. `validate` takes the same flags. With `-target` and `-protocol`, `-data` and `-data-pick next|random` fill `{data.NAME.COLUMN}` on each call for postgres, mysql, redis, ftp, sftp, kafka and rabbitmq. SQL values go in `-opt args`. `-env` and `-secret-env` stay for scenario files.
- `vegaload run` is shown live in the browser by default (the live view): it pushes per-second points to a live view service (default `http://127.0.0.1:7080`, `-live-url`), prints `live view: URL` on stderr and opens the page. It is off with `-no-live`, `VEGALOAD_LIVE=off`, `CI`, or any `-stream-*` destination (`-live` forces it on). It never fails a run, and the push token and project id are never in a report, event, log, audit entry or error text (the audit log notes `live:HOST` only). `-no-open` and `VEGALOAD_OPEN=off` stop every browser tab, the HTML report's too. When the live view is on, or the trigger starts with `mcp`, the HTML report is written but not opened. `vegaload live [-open]` prints the page, `live reset` starts a new project. `-stream-file PATH|auto` and `-stream-push URL` write or send the run as `vegaload.io/stream/v1` events (see `docs/streaming.md`); `-run-id`, `-label key=value` and `VEGALOAD_STREAM_TOKEN` go with them. `VEGALOAD_CONFIG_DIR` moves where `live.json` is kept.
- Tests that run `vegaload run` in-process must not reach a live view service or write a real config folder. `cmd/vegaload/main_test.go` (`TestMain`) sets `VEGALOAD_CONFIG_DIR` to a temporary folder and `VEGALOAD_LIVE=off`, `VEGALOAD_OPEN=off`, and clears the other streaming variables. A package that runs a scenario through `run` does the same, or sets `-no-live`. `vegaload mcp eval` sets `VEGALOAD_LIVE=off` for the runs it makes. A test that checks the live view uses a fake bridge on a loopback port and replaces `live.Opener`.
- `vegaload diagnose <report>` — explain a failed run.
- `vegaload compare <baseline.json> <candidate.json>` — diff a candidate report against a baseline; exits non-zero on regression. It also prints each protocol's p95 and error rate. Those lines do not change the exit code.
- `vegaload mcp serve` — start the MCP server (stdio by default).
- `vegaload doctor` — check the setup (CLI, agent hosts, target). Run it first when something is not working; `-fix` repairs what it safely can.
- `vegaload protocols` — list every supported protocol, with options and examples. `vegaload protocols NAME` prints one. `-output json` prints the same data. An unknown `-protocol` exits 2 and prints this list.
- `vegaload --move-to-harness <test-name|all>` — push a test to Harness RT.
- `go test ./...` — run the test suite. Run this before proposing any change as finished.

## What not to do

- Do not introduce YAML as a test-authoring format. Tests are JavaScript/TypeScript or Python code, not config.
- Do not add a feature to the MCP layer that has no CLI equivalent.
- Do not add Harness RT–specific logic to the core engine, the protocol drivers, or the output layer.
- Do not add telemetry or any network call that was not explicitly requested. See NFR-05 in the PRD: nothing leaves the local machine unless the user runs `--move-to-harness` or configures a bring-your-own-LLM connector themselves. The one deliberate exception is the live view (see `docs/streaming.md`, "Live view"): a run's numbers go to the live view service at `-live-url`, which is loopback by default, and `-no-live` or `VEGALOAD_LIVE=off` turns it off. Do not widen it: no other data, no other default destination, no remote `http://`.

## Docs

User-facing changes need the docs page changed in the same PR once the page exists. Do not describe a feature that is not released. Use short sentences. Every command in a page must run on the released binary.

Until launch, the text of a guide page also lives in README.md. A PR that changes one changes the other, in the same PR. After launch the README keeps a summary and links, and the pages are the only copy.

## Where the real requirements live

The PRD is the source of truth for what to build and why. This file is the source of truth for how to build it. If the two ever conflict, the PRD wins, and this file should be updated to match.
