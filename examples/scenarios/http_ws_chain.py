# http_ws_chain.py -- the same FR-CLI-08 flow as http-ws-chain.vl.js,
# in Python: an HTTP call whose response is carried into a WebSocket
# call, against ../sample-app (see ../sample-app/README.md). It creates
# a widget over POST /widgets, then opens /ws/echo and sends a message
# naming that widget's freshly-created id, reading the echo back to
# confirm the round trip.
#
# http and ws are globals every VU gets; see
# internal/scripting/python's doc comment for the full API.
#
# Run it against a running sample-app (flags before the scenario file):
#   cd ../sample-app && go run .                       # in one terminal
#   vegaload run -vus 5 -duration 10s http_ws_chain.py  # in another
#
# localhost needs no -allow-target/-yes; a host elsewhere would (see
# "vegaload run -h" and AGENTS.md's FR-CLI-06 allowlist).
import json


def iteration():
    created = http.post("http://127.0.0.1:8080/widgets", body=json.dumps({"name": "load-test-widget"}))
    if created.status != 201:
        # sample-app injects a ~3% failure on create -- treat it as a
        # failed iteration rather than trying to continue without an id.
        raise ValueError("POST /widgets: status %d" % created.status)
    widget = created.json()

    conn = ws.connect("ws://127.0.0.1:8080/ws/echo")
    sent = json.dumps({"widgetId": widget["id"], "name": widget["name"]})
    conn.send(sent)
    echoed = conn.receive(timeout_ms=2000)
    conn.close()

    if echoed != sent:
        raise ValueError("expected the echo to match what was sent, got: %r" % echoed)
