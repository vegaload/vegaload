// http-ws-chain.vl.js -- FR-CLI-08's own shape of scenario: an HTTP call
// whose response is carried into a WebSocket call, against
// ../sample-app (see ../sample-app/README.md for its endpoints). It
// creates a widget over POST /widgets, then opens /ws/echo and sends a
// message naming that widget's freshly-created id, reading the echo
// back to confirm the round trip -- standing in for the kind of
// create-then-watch flow FR-CLI-08's LaunchPad example describes
// (create a launch, open its WebSocket, read frames until done).
//
// http and ws are globals every VU gets; see internal/scripting/js's
// doc comment for the full API.
//
// Run it against a running sample-app (flags before the scenario file):
//   cd ../sample-app && go run .          # in one terminal
//   vegaload run -vus 5 -duration 10s http-ws-chain.vl.js   # in another
//
// localhost needs no -allow-target/-yes; a host elsewhere would (see
// "vegaload run -h" and AGENTS.md's FR-CLI-06 allowlist).
export default function () {
  const created = http.post("http://127.0.0.1:8080/widgets", {
    body: JSON.stringify({ name: "load-test-widget" }),
  });
  if (created.status !== 201) {
    // sample-app injects a ~3% failure on create -- treat it as a
    // failed iteration rather than trying to continue without an id.
    throw new Error("POST /widgets: status " + created.status);
  }
  const widget = created.json();

  const conn = ws.connect("ws://127.0.0.1:8080/ws/echo");
  const sent = JSON.stringify({ widgetId: widget.id, name: widget.name });
  conn.send(sent);
  const echoed = conn.receive(2000);
  conn.close();

  if (echoed !== sent) {
    throw new Error("expected the echo to match what was sent, got: " + echoed);
  }
}
