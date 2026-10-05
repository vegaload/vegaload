// Package python runs a scenario written as a Python file, by shelling
// out to a python3 interpreter the user has installed separately.
//
// This is VegaLoad's second-class scripting path, as AGENTS.md describes
// it: JavaScript/TypeScript (internal/scripting/js) is embedded and needs
// nothing beyond the vegaload binary itself, while Python here depends on
// a python3 executable already being on PATH. A scenario file defines a
// module-level function named iteration:
//
//	def iteration():
//	    # one iteration's worth of work
//	    pass
//
// Each VU gets its own python3 subprocess (started once, in NewVU, and
// reused for every iteration that VU runs) talking a line-delimited JSON
// RPC protocol over stdin/stdout — not a fresh process per iteration,
// which would make the per-request overhead dwarf whatever the scenario
// is actually measuring.
//
// Per FR-CLI-08, every VU's subprocess also gets real HTTP and WebSocket
// access: the harness injects module-level `http` and `ws` objects
// (mirroring internal/scripting/js's globals of the same names) that a
// script calls directly:
//
//	def iteration():
//	    created = http.post("http://localhost:8080/launches", body='{"name":"demo"}')
//	    launch = created.json()
//	    conn = ws.connect("ws://localhost:8080/launches/" + launch["id"] + "/ignite")
//	    conn.send('{"action":"ignite"}')
//	    while True:
//	        frame = conn.receive(timeout_ms=5000)
//	        if frame is None:
//	            break
//	        msg = json.loads(frame)
//	        if msg["status"] in ("complete", "aborted"):
//	            break
//	    conn.close()
//
// A Python subprocess can't hold a Go value (an *http.Client, a websocket
// connection), so http and ws don't talk to the network themselves —
// every call is proxied back over the same stdin/stdout pipe as a small
// request/response exchange, handled on the Go side by VU.Iteration and
// actually performed by internal/scripting/netapi, the same package
// backing the JS globals. A WebSocket connection is identified to Python
// by an opaque integer handle rather than any kind of reference, since
// that's the only thing that survives the process boundary.
package python

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/vegaload/vegaload/internal/scripting/netapi"
)

// pythonBin is the interpreter this package shells out to. It is a
// plain var, not a const, so a test (or a future CLI flag) can point it
// at a specific interpreter instead of whatever "python3" resolves to on
// PATH.
var pythonBin = "python3"

// harnessSource is run as `python3 -c harnessSource <script path>`. It
// loads the scenario module once (with `http`/`ws` already present as
// globals), validates it, then answers a "ready" message and loops,
// running iteration() once per "iteration" command and answering a
// "result" message — proxying any http/ws call the script makes, mid
// iteration, back over the same pipe as a "call"/"call_result" exchange.
// See the package doc comment for a worked example and VU.Iteration for
// the Go side of this same protocol.
const harnessSource = `
import sys, json, runpy, itertools

_next_id = itertools.count(1)

def _send(msg):
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()

def _recv():
    line = sys.stdin.readline()
    if line == "":
        raise EOFError("vegaload: parent process closed the connection")
    return json.loads(line)

class VegaloadError(Exception):
    pass

def _call(op, **fields):
    call_id = next(_next_id)
    _send({"type": "call", "id": call_id, "op": op, **fields})
    while True:
        msg = _recv()
        if msg.get("cmd") == "call_result" and msg.get("id") == call_id:
            if not msg.get("ok"):
                raise VegaloadError(msg.get("error") or "call failed")
            return msg.get("result") or {}
        # Anything else on the wire while waiting for this call's own
        # reply would be a protocol bug on the Go side, not something a
        # script can do anything about -- ignore and keep waiting.

class HTTPResponse:
    def __init__(self, data):
        self.status = data.get("status", 0)
        self.ok = 200 <= self.status < 300
        self.body = data.get("body", "")
        self.headers = data.get("headers", {})

    def json(self):
        return json.loads(self.body)

class HTTP:
    def request(self, method, url, headers=None, body=None, insecure=False):
        result = _call("http", method=method, url=url,
                        headers=headers or {}, body=body or "", insecure=bool(insecure))
        return HTTPResponse(result)

    def get(self, url, **kw):
        return self.request("GET", url, **kw)

    def post(self, url, **kw):
        return self.request("POST", url, **kw)

    def put(self, url, **kw):
        return self.request("PUT", url, **kw)

    def patch(self, url, **kw):
        return self.request("PATCH", url, **kw)

    def delete(self, url, **kw):
        return self.request("DELETE", url, **kw)

class WSConn:
    def __init__(self, handle):
        self._handle = handle

    def send(self, data, text=True):
        _call("ws_send", handle=self._handle, data=data, text=bool(text))

    def receive(self, timeout_ms=None):
        result = _call("ws_receive", handle=self._handle, timeout_ms=timeout_ms)
        if result.get("closed"):
            return None
        return result.get("data", "")

    def close(self):
        _call("ws_close", handle=self._handle)

class WS:
    def connect(self, url, headers=None, insecure=False):
        result = _call("ws_connect", url=url, headers=headers or {}, insecure=bool(insecure))
        return WSConn(result["handle"])

http = HTTP()
ws = WS()

def _respond_ready(ok, error=None):
    msg = {"type": "ready", "ok": ok}
    if error is not None:
        msg["error"] = error
    _send(msg)

def _respond_result(ok, error=None):
    msg = {"type": "result", "ok": ok}
    if error is not None:
        msg["error"] = error
    _send(msg)

def main():
    script_path = sys.argv[1]
    try:
        module_globals = runpy.run_path(
            script_path,
            init_globals={"http": http, "ws": ws},
            run_name="__vegaload_scenario__",
        )
    except BaseException as e:
        _respond_ready(False, "loading script: %s: %s" % (type(e).__name__, e))
        sys.exit(1)

    iteration = module_globals.get("iteration")
    if not callable(iteration):
        _respond_ready(False, "script has no callable iteration() function")
        sys.exit(1)

    _respond_ready(True)

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            cmd = json.loads(line)
        except Exception as e:
            _respond_result(False, "parsing command: %s" % e)
            continue
        if cmd.get("cmd") != "iteration":
            _respond_result(False, "unexpected command %r" % cmd.get("cmd"))
            continue
        try:
            iteration()
            _respond_result(True)
        except BaseException as e:
            _respond_result(False, "%s: %s" % (type(e).__name__, e))

main()
`

// Script is a loaded, validated-at-load-time-only scenario file. Load
// itself only checks that the file exists and that an interpreter is
// available — the script's own correctness (does it define iteration?,
// does it even parse?) is checked once per VU, in NewVU, since that is
// where the interpreter that would tell us is actually started.
type Script struct {
	path string
}

// Load returns a Script for the scenario file at path, after confirming
// the file exists and that pythonBin is on PATH. It does not start an
// interpreter or touch the script's contents — see NewVU.
func Load(path string) (*Script, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("python: %w", err)
	}
	if _, err := exec.LookPath(pythonBin); err != nil {
		return nil, fmt.Errorf("python: %s not found on PATH (the Python scripting driver requires a python3 interpreter to be installed separately — see AGENTS.md): %w", pythonBin, err)
	}
	return &Script{path: path}, nil
}

// message is one line of the harness's stdout protocol: a "ready"
// handshake at startup, a "result" ending one iteration, or a "call"
// asking the Go side to perform one http/ws operation mid-iteration.
type message struct {
	Type  string `json:"type"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`

	// Present on a "call" message.
	ID       int               `json:"id"`
	Op       string            `json:"op"`
	Method   string            `json:"method"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers"`
	Body     string            `json:"body"`
	Insecure bool              `json:"insecure"`
	Handle   int               `json:"handle"`
	Data     string            `json:"data"`
	Text     bool              `json:"text"`
	// TimeoutMs is a pointer because Python's None (no timeout given)
	// must stay distinguishable from an explicit 0.
	TimeoutMs *int `json:"timeout_ms"`
}

// cmdMsg is one line of the Go-to-harness stdin protocol: either
// {"cmd":"iteration"} to run one iteration, or a {"cmd":"call_result",
// ...} reply to a "call" message the harness sent.
type cmdMsg struct {
	Cmd    string      `json:"cmd"`
	ID     int         `json:"id,omitempty"`
	OK     bool        `json:"ok"`
	Error  string      `json:"error,omitempty"`
	Result interface{} `json:"result,omitempty"`
}

// VU is one virtual user's python3 subprocess, plus the HTTP client and
// set of open WebSocket connections (keyed by the handle the harness
// uses to refer to them) that back its `http`/`ws` globals.
type VU struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	stderr  *bytes.Buffer

	http        *netapi.HTTPClient
	safetyCheck netapi.SafetyCheck
	timeout     time.Duration
	ctx         context.Context //nolint:containedctx // set per-Iteration; dispatchCall reads it synchronously within that same iteration to build call/dial contexts.

	conns      map[int]*netapi.WSConn
	nextHandle int
}

// NewVU starts a fresh python3 subprocess for one virtual user and waits
// for its ready signal, which is also where a script that fails to
// import, or has no iteration() function, is caught — before any
// iteration is attempted, the same way js.Script.NewVU validates the
// default export up front.
//
// check is run (if non-nil) against the host of every call the script
// makes through http or ws, before connecting; timeout bounds each
// individual HTTP request and WebSocket handshake, same as
// js.Script.NewVU's parameters of the same name.
func (s *Script) NewVU(check netapi.SafetyCheck, timeout time.Duration) (*VU, error) {
	cmd := exec.Command(pythonBin, "-u", "-c", harnessSource, s.path) //nolint:gosec // path and interpreter are operator-controlled, not request input

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("python: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("python: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("python: starting interpreter: %w", err)
	}

	vu := &VU{
		cmd:         cmd,
		stdin:       stdin,
		scanner:     bufio.NewScanner(stdout),
		stderr:      &stderr,
		http:        netapi.NewHTTPClient(check, timeout),
		safetyCheck: check,
		timeout:     timeout,
		conns:       make(map[int]*netapi.WSConn),
	}

	msg, err := vu.readMessage()
	if err != nil {
		_ = vu.Close()
		return nil, fmt.Errorf("python: %w", err)
	}
	if msg.Type != "ready" {
		_ = vu.Close()
		return nil, fmt.Errorf("python: expected a ready message from the interpreter, got %q", msg.Type)
	}
	if !msg.OK {
		_ = vu.Close()
		return nil, fmt.Errorf("python: %s", msg.Error)
	}

	return vu, nil
}

// readMessage reads and parses one protocol line from the interpreter.
func (v *VU) readMessage() (message, error) {
	if !v.scanner.Scan() {
		if err := v.scanner.Err(); err != nil {
			return message{}, fmt.Errorf("reading from interpreter: %w", err)
		}
		if msg := v.stderr.String(); msg != "" {
			return message{}, fmt.Errorf("interpreter exited unexpectedly: %s", msg)
		}
		return message{}, fmt.Errorf("interpreter exited before responding")
	}
	var msg message
	if err := json.Unmarshal(v.scanner.Bytes(), &msg); err != nil {
		return message{}, fmt.Errorf("parsing interpreter response %q: %w", v.scanner.Text(), err)
	}
	return msg, nil
}

// readMessageCtx is readMessage, but gives up and returns ctx.Err() if
// ctx is cancelled before a line arrives. The read itself keeps running
// in the background afterward -- there's no way to abandon a blocked
// bufio.Scanner.Scan() call -- which is the same limitation
// VU.Iteration's doc comment already describes for the subprocess as a
// whole.
func (v *VU) readMessageCtx(ctx context.Context) (message, error) {
	type result struct {
		msg message
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		msg, err := v.readMessage()
		resCh <- result{msg, err}
	}()

	select {
	case r := <-resCh:
		return r.msg, r.err
	case <-ctx.Done():
		return message{}, ctx.Err()
	}
}

// writeCmd writes one line of the Go-to-harness protocol.
func (v *VU) writeCmd(m cmdMsg) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding command: %w", err)
	}
	data = append(data, '\n')
	if _, err := v.stdin.Write(data); err != nil {
		return fmt.Errorf("writing to interpreter: %w", err)
	}
	return nil
}

// Iteration runs the script's iteration() function once, in this VU's
// subprocess, handling every http/ws "call" message the script makes
// along the way until the subprocess reports the iteration's own
// result. It implements engine.IterationFunc's signature directly, so a
// *VU can be used anywhere the engine wants one.
//
// Unlike js.VU.Iteration, a cancelled ctx here does not interrupt
// iteration() mid-execution inside the subprocess — there is no
// in-process equivalent of Goja's Interrupt across a process boundary.
// It does stop Iteration from waiting on a response past ctx's
// deadline, returning promptly, and it bounds every http/ws call the
// script makes (via the same ctx, same as js.VU); the subprocess itself
// is only reclaimed when Close is called, which the executor is
// expected to do once the run ends.
func (v *VU) Iteration(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	v.ctx = ctx
	if err := v.writeCmd(cmdMsg{Cmd: "iteration"}); err != nil {
		return fmt.Errorf("python: %w", err)
	}

	for {
		msg, err := v.readMessageCtx(ctx)
		if err != nil {
			return fmt.Errorf("python: %w", err)
		}

		switch msg.Type {
		case "result":
			if !msg.OK {
				return fmt.Errorf("python: %s", msg.Error)
			}
			return nil
		case "call":
			if err := v.handleCall(msg); err != nil {
				return fmt.Errorf("python: %w", err)
			}
		default:
			return fmt.Errorf("python: unexpected message type %q from interpreter", msg.Type)
		}
	}
}

// handleCall performs the one http/ws operation msg describes and
// writes its outcome back to the interpreter as a call_result — as a
// successful result, or (far more often, for an ordinary refused or
// failed call the script's own try/except is meant to see) an error
// string the harness's _call() raises as a catchable VegaloadError.
// handleCall's own return error is reserved for something that stops
// the whole iteration dead: failing to write the reply at all.
func (v *VU) handleCall(msg message) error {
	result, err := v.dispatchCall(msg)
	if err != nil {
		return v.writeCmd(cmdMsg{Cmd: "call_result", ID: msg.ID, OK: false, Error: err.Error()})
	}
	return v.writeCmd(cmdMsg{Cmd: "call_result", ID: msg.ID, OK: true, Result: result})
}

func (v *VU) dispatchCall(msg message) (interface{}, error) {
	switch msg.Op {
	case "http":
		return v.callHTTP(msg)
	case "ws_connect":
		return v.callWSConnect(msg)
	case "ws_send":
		return v.callWSSend(msg)
	case "ws_receive":
		return v.callWSReceive(msg)
	case "ws_close":
		return v.callWSClose(msg)
	default:
		return nil, fmt.Errorf("unknown call op %q", msg.Op)
	}
}

func (v *VU) callHTTP(msg message) (interface{}, error) {
	var body []byte
	if msg.Body != "" {
		body = []byte(msg.Body)
	}
	resp, err := v.http.Do(v.ctx, msg.Method, msg.URL, body, netapi.Options{
		Headers:            msg.Headers,
		InsecureSkipVerify: msg.Insecure,
	})
	if err != nil {
		return nil, err
	}
	headers := make(map[string]string, len(resp.Headers))
	for k := range resp.Headers {
		headers[k] = resp.Headers.Get(k)
	}
	return map[string]interface{}{
		"status":  resp.StatusCode,
		"body":    string(resp.Body),
		"headers": headers,
	}, nil
}

func (v *VU) callWSConnect(msg message) (interface{}, error) {
	conn, err := netapi.Dial(v.ctx, v.safetyCheck, msg.URL, v.timeout, netapi.Options{
		Headers:            msg.Headers,
		InsecureSkipVerify: msg.Insecure,
	})
	if err != nil {
		return nil, err
	}
	v.nextHandle++
	handle := v.nextHandle
	v.conns[handle] = conn
	return map[string]interface{}{"handle": handle}, nil
}

func (v *VU) callWSSend(msg message) (interface{}, error) {
	conn, ok := v.conns[msg.Handle]
	if !ok {
		return nil, fmt.Errorf("unknown websocket handle %d", msg.Handle)
	}
	if err := conn.Send(v.ctx, []byte(msg.Data), msg.Text); err != nil {
		return nil, err
	}
	return map[string]interface{}{}, nil
}

func (v *VU) callWSReceive(msg message) (interface{}, error) {
	conn, ok := v.conns[msg.Handle]
	if !ok {
		return nil, fmt.Errorf("unknown websocket handle %d", msg.Handle)
	}
	ctx := v.ctx
	if msg.TimeoutMs != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(v.ctx, time.Duration(*msg.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	data, closed, err := conn.Receive(ctx)
	if err != nil {
		return nil, err
	}
	if closed {
		return map[string]interface{}{"closed": true}, nil
	}
	return map[string]interface{}{"closed": false, "data": string(data)}, nil
}

func (v *VU) callWSClose(msg message) (interface{}, error) {
	conn, ok := v.conns[msg.Handle]
	if !ok {
		return nil, fmt.Errorf("unknown websocket handle %d", msg.Handle)
	}
	delete(v.conns, msg.Handle)
	if err := conn.Close(); err != nil {
		return nil, err
	}
	return map[string]interface{}{}, nil
}

// Close signals the interpreter to exit (by closing its stdin, which
// ends the harness's read loop), closes every WebSocket connection this
// VU's script ever opened via ws.connect (even one it never explicitly
// closed itself), and waits for the subprocess to exit.
func (v *VU) Close() error {
	_ = v.stdin.Close()
	for _, conn := range v.conns {
		_ = conn.Close()
	}
	v.http.Close()
	return v.cmd.Wait()
}
