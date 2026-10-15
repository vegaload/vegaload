# Streaming a run (`vegaload.io/stream/v1`)

Status: design contract, written 2026-10-15. It is the agreement between VegaLoad (the sender) and rt-bridge (a consumer). The code follows this file; where they differ, fix one of them and say so here.

VegaLoad can send a run, as it happens, to an application: a file, an HTTP endpoint, or the live view in a browser. The stream is a sequence of JSON events, one per line when stored (NDJSON).

The format mirrors litmus-lite's `litmus-lite.io/stream/v1` on purpose: the same envelope, the same delivery rules, the same names where a field means the same thing. It is a separate schema because the two tools live in different projects and change on their own. A consumer that reads one can read the other by switching on `schema`.

## Envelope

```json
{
  "schema": "vegaload.io/stream/v1",
  "runId": "9f2c4e1a7b3d",
  "seq": 42,
  "ts": "2026-10-15T08:21:07.412Z",
  "offsetS": 7.0,
  "labels": { "service": "checkout" },
  "type": "point",
  "data": {}
}
```

| Field | Meaning |
| --- | --- |
| `schema` | Always `vegaload.io/stream/v1`. |
| `runId` | One id per run. Random 12 hex characters unless the user gives `-run-id` (or `VEGALOAD_RUN_ID`). |
| `seq` | Starts at 1, grows by one per event within a run, and is the same in every sink. |
| `ts` | Wall-clock time of the event (RFC 3339, UTC). |
| `offsetS` | Seconds since the run started. |
| `labels` | Optional `key: value` strings from `-label key=value` (repeatable). |
| `synthetic` | Optional, true when the events were built from a finished report instead of observed live. |
| `type`, `data` | Described below. |

Delivery is at least once. A consumer dedupes on `(runId, seq)`. Unknown fields are ignored and unknown `type` values are skipped. `run.finished` (or `run.failed`) is authoritative: a consumer that missed events can rebuild everything from it.

## Events

| `type` | When |
| --- | --- |
| `run.started` | First event of every stream. |
| `point` | Once per second, after each one-second bucket closes. |
| `warning` | Something the user should know, for example dropped events. |
| `run.finished` | Last event of a run that ran to its end, passed or not. |
| `run.failed` | Last event of a run that could not run or was interrupted. |

Every stream ends with exactly one of `run.finished` or `run.failed`, including on Ctrl-C (the process then exits 130 for SIGINT, or 143 for SIGTERM, as a process killed by that signal would). A run that VegaLoad refuses before it starts (a host that is not allowed, a flag that is not valid, a bad `-run-id`) sends no stream at all. A run whose scenario cannot be loaded or started sends `run.started` and then `run.failed`.

### `run.started`

| Field | Type | Notes |
| --- | --- | --- |
| `name` | string | The scenario file's base name, or `PROTOCOL TARGET` in protocol-direct mode. |
| `executor` | string | `fixed-vus`, `ramp`, `step` or `constant-arrival-rate`. |
| `vus` | integer | Fixed-vus: the user count. Ramp, step and arrival-rate: the peak (or `max-vus`). Optional. |
| `rate` | number | Iterations per second, for `constant-arrival-rate`. Optional. |
| `duration` | string | Planned length, a Go duration such as `30s`. |
| `stages` | array | `{ "target": 10, "duration": "30s" }` for ramp and step. Optional. |
| `target` | object | `{ "protocol": "http", "baseUrl": "https://host:port/path" }`. Protocol-direct mode only. Credentials (userinfo) and the query string are removed. |
| `thresholds` | array | The pass/fail limits: `{ "name", "metric", "operator", "value", "step"?, "protocol"?, "op"? }`, as the run was given them. |
| `abortOnBreach` | boolean | True with `-abort-on-breach`. |
| `version` | string | VegaLoad version. |

Nothing that came from `-header`, `-body`, `-opt`, `-secret-env`, `-env` or a data file is ever sent.

### `point`

One per second of the run, in order, covering the calls that started in that second.

| Field | Type | Notes |
| --- | --- | --- |
| `offsetS` | number | Start of the one-second bucket, in seconds since the run started. |
| `rps` | number | Calls in the bucket per second. |
| `requests` | integer | Calls in the bucket. |
| `failed` | integer | Failed calls in the bucket. |
| `errorRate` | number | 0 to 1; `0` when `requests` is 0. |
| `p50Ms`, `p95Ms`, `p99Ms` | number | Latency of the bucket's calls in ms; `0` when `requests` is 0. |
| `total` | integer | Calls since the start, up to and including this bucket. |
| `totalFailed` | integer | Failed calls since the start. |

A call is recorded when it ends but belongs to the second it started in, so a point is sent only when it is complete. VegaLoad sends the point of second N when all of these hold:

- the second has closed and 500 ms have passed (a call just starting or just ending is counted in it), and
- no call that started in second N or an earlier one is still running (the hold), and
- a call has been recorded that started in second N or a later one. This keeps the points inside the final `time_series`, which ends at the last second a call started in: a quiet second at the end of a run is never sent early, and a quiet second in the middle is sent when a later second has a call (it is a point with `requests: 0`).

With fast calls the hold never matters, and a live consumer sees a second about 1.5 s after it began. With slow calls (a chaos run with 800 ms of added latency) it sees a second when the last call that started in it has ended. Points are always in order, one per second, each exactly once.

**The cap.** A hold lasts at most 10 s past the end of the second (`report.LiveMaxHold`), so a hung call cannot freeze the live view. After that the point is sent with the calls it has, and a `warning` is sent once per run: `live points were emitted before slow calls finished; the final result has them`. The warning carries no target or other data. A point sent at the cap goes out even if nothing has been recorded yet. A point sent at the cap can have fewer calls than the matching `time_series` entry, and the totals of the points after it can be short by the same calls; `run.finished` is authoritative.

When the executor ends, every remaining second up to the second of the last recorded call is sent as a `point` before `run.finished`, so the points and the final series cover the same seconds. A call that is cut off by the end of the run is not recorded, by the report or by the points.

The bucket width is the same one-second width as the final `time_series` in the report, and a point is computed from the same samples with the same arithmetic. The `rps` and `errorRate` of every point equal the matching entry of `time_series` in `run.finished`. This holds for every point, except one sent after the cap (above).

### `warning`

`{ "message": "..." }`.

### `run.finished`

`data` is the run's structured result, exactly what `-out` writes (the `report.Result` JSON: `executor`, `started_at`, `elapsed_ns`, `total`, `failed`, `error_rate`, `latency`, `time_series`, and when present `thresholds`, `thresholds_passed`, `aborted`, `baseline`, `steps`, `protocols`, `checks`, `data_files`), plus:

| Field | Type | Notes |
| --- | --- | --- |
| `passed` | boolean | True when every threshold and the baseline gate (if any) passed and the run was not aborted. The same verdict as exit code 0. |
| `exitCode` | integer | The process exit code of the run (0, or 3 when a threshold or the baseline gate failed). |

A run stopped early by `-abort-on-breach` is a `run.finished` with `aborted` set and `passed: false`.

### `run.failed`

`{ "reason": "...", "result": { ... } }`. `reason` is for example `interrupted`, or why the scenario could not start. `result` is the partial result in the same shape as `run.finished.data` (without `passed` and `exitCode`), when the run got far enough to build one (it can show zero calls, for a scenario that failed to load); it is left out when there was nothing to report.

## Sinks (what VegaLoad does with the events)

| Option | Variable | Meaning |
| --- | --- | --- |
| `-stream-file PATH` or `auto` | `VEGALOAD_STREAM_FILE` | Write NDJSON. `auto` writes `.vegaload/runs/<runId>.ndjson`. |
| `-stream-push URL[,URL]` | `VEGALOAD_STREAM_PUSH` | POST batches of NDJSON, with retry. Bearer token from `VEGALOAD_STREAM_TOKEN`. `http://` to a remote host is refused unless `-stream-insecure`. |
| `-run-id ID` | `VEGALOAD_RUN_ID` | Use this run id. |
| `-label key=value` | | Add a label (repeatable). |
| `-live`, `-no-live` | `VEGALOAD_LIVE=off` | The live view, below. |
| `-live-url URL` | `VEGALOAD_LIVE_URL` | Base address of the live-view service. Default `http://127.0.0.1:7080`. |

The engine never waits on a sink. Each sink has a bounded queue; when it is full, `point` events are dropped before lifecycle events, and a `warning` says so. A sink problem never changes the run's result, report or exit code. A sink that cannot be set up (a file that cannot be created, say) is left out with a note on stderr and the run goes on.

The audit log records which destinations a run used and where they are, never a token: `stream` is `file`, `push:HOST` or `live:HOST`, comma-separated.

Push batches follow the same rules as litmus-lite's: `POST` with `Content-Type: application/x-ndjson`, one event per line, at least once, a `2xx` answer means the whole batch was taken.

## Live view

`vegaload run` with no `-stream-*` destination does what litmus-lite does: it sends the run to a project on the live-view service (rt-bridge) and opens the page for it in the browser. All of litmus-lite's rules apply, with VegaLoad's names:

- Identity: `live.json` (mode 0600) in the user config directory under `vegaload/` (override with `VEGALOAD_CONFIG_DIR`): `projectId` (`p-` plus 26 base32 characters, 128 bits), `pushToken` (32 random bytes, base64url), optional `base`, `createdAt`. Made on first use. The project id is the read secret (it is in the page URL). The push token is the write secret: bearer only, never in a URL, never written to a report, event, log line or audit entry. The audit log notes `live:HOST` only.
- Probe `GET <base>/v1/health` (1.5 s, no redirects) must answer `{"status":"ok","projects":true}`, otherwise the run goes on and says why on stderr. An unusable setup never fails or delays a run by more than the probe.
- Push to `<base>/v1/projects/<projectId>/ingest?source=vegaload` with `Authorization: Bearer <pushToken>`.
- Print `live view: <base>/p/<projectId>` on stderr, then open the browser.
- Off with `-no-live`, `VEGALOAD_LIVE=off`, `CI` set (not empty, `0` or `false`), or when the user names any `-stream-*` destination. `-live` forces it on (except `VEGALOAD_LIVE=off`).
- The browser does not open with `-no-open`, `VEGALOAD_OPEN=off`, in CI, or on Linux with no display. `-no-open` already stops the HTML report from opening; it now stops every browser tab VegaLoad would open. When the live view is on, the HTML report (still written) is not opened as well, whether or not the page itself opens. A run started by an agent (`-trigger` starting with `mcp`) never opens the HTML report.
- Remote `http://` is refused as for any push.
- `vegaload live [-open]` prints (or opens) the project page. `vegaload live reset` makes a new project.
- This is a deliberate exception to "nothing leaves the machine by default": the run (its numbers, thresholds, step and protocol names, labels, target without credentials) goes to the live-view service. `-no-live` and `VEGALOAD_LIVE=off` turn it off.

## From an agent (MCP)

`run_test` follows the same default and takes `live`, `no-live` and `no-open` arguments (also written `no_live`, `no_open`), plus `stream_file`, `stream_push`, `run_id` and `labels` (an array of `key=value` strings). VegaLoad no longer forces `-no-open` for an agent, because the live view is the point; the agent's HTML report is still never opened. There is no argument for a token or for `-stream-insecure`. The tool result's first text item stays the run's JSON, taken from the CLI's stdout alone; the CLI's stderr (including `live view: URL`) comes as a second text item, present only when there is something on stderr. The agent's rule text tells it to relay `live view: URL` to the user.

## Not in v1 of this contract

`-stream-pull` (a server the consumer reads from), `stream validate|replay|receive`, `serve`, `doctor -stream`, live threshold status events, per-step live points, and joining with a litmus-lite run (the combined demo). See `docs/PENDING.md` (kept locally, not in git).
