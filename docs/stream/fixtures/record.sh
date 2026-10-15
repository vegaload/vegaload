#!/usr/bin/env bash
# Records the example streams in this folder from real runs of the vegaload
# binary against examples/sample-app (see docs/streaming.md). Run from anywhere:
#   bash docs/stream/fixtures/record.sh
# Needs Go. Writes only here and to a temporary folder. The timestamps and
# numbers change on each recording; the shapes do not. Every run uses
# -no-live and -no-open, so nothing is sent or opened.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/vl-fixtures.XXXXXX")"
trap 'kill "${APP_PID:-0}" 2>/dev/null || true; wait "${APP_PID:-0}" 2>/dev/null || true; rm -rf "$TMP"' EXIT

(cd "$ROOT" && go build -o "$TMP/vegaload" ./cmd/vegaload)
(cd "$ROOT/examples/sample-app" && go build -o "$TMP/sample-app" .)

HTTP=127.0.0.1:18080
"$TMP/sample-app" -addr "$HTTP" -grpc-addr 127.0.0.1:19090 >"$TMP/app.log" 2>&1 &
APP_PID=$!
for _ in $(seq 1 50); do
  curl -sf "http://$HTTP/health" >/dev/null && break
  sleep 0.2
done

cat >"$TMP/checkout.vl.js" <<JS
const base = "http://$HTTP";
export default function () {
  step("browse", () => http.get(base + "/widgets"));
  step("health", () => http.get(base + "/health"));
}
JS
printf 'export default function () {\n' >"$TMP/broken.vl.js"

export VEGALOAD_LIVE=off VEGALOAD_OPEN=off VEGALOAD_CONFIG_DIR="$TMP/config"
for v in VEGALOAD_STREAM_FILE VEGALOAD_STREAM_PUSH VEGALOAD_STREAM_TOKEN VEGALOAD_RUN_ID VEGALOAD_LIVE_URL; do unset "$v"; done

record() { # name run-id extra-flags... ; the exit codes 3 and 1 are expected
  local name="$1" id="$2"; shift 2
  (cd "$TMP" && ./vegaload run -no-live -no-open -no-report -output json \
    -audit-log "$TMP/audit.log" -run-id "$id" -label service=checkout -label env=staging \
    -stream-file "$HERE/$name.ndjson" "$@" >/dev/null 2>"$TMP/$name.err") || true
}

STAGES=(-executor ramp -vus 20 -stages 20:4s,20:8s)
record checkout-load        a1b2c3d4e5f6 "${STAGES[@]}" \
  -threshold "p95 < 1s" -threshold "error_rate < 1%" checkout.vl.js
record checkout-load-failed b2c3d4e5f6a1 "${STAGES[@]}" \
  -threshold "p95 < 1s" -threshold 'p95{step="browse"} < 1ms' checkout.vl.js
record checkout-load-aborted c3d4e5f6a1b2 "${STAGES[@]}" \
  -threshold 'p95{step="browse"} < 1ms' -abort-on-breach -abort-grace 2s checkout.vl.js
record run-failed           d4e5f6a1b2c3 -vus 10 -duration 10s broken.vl.js

echo "recorded; check them with: go test ./internal/stream -run Fixtures"
