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
// The tcp, udp, mqtt and kafka globals work the same way: a call such as
// tcp.send(url, body="PING\r\n") or kafka.produce(url, topic="t", value="v")
// is one "proto" request, performed on the Go side by netapi.ProtoClient, and
// the reply comes back as plain data (a dict that also allows r.ok).
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
	"runtime"
	"strings"
	"time"

	"github.com/vegaload/vegaload/internal/pyfind"
	"github.com/vegaload/vegaload/internal/scripting/netapi"
)

// pythonBin is the interpreter this package shells out to. It is a
// plain var, not a const, so a test (or a future CLI flag) can point it
// at a specific interpreter instead of whatever "python3" resolves to on
// PATH.
var pythonBin = "python3"

// defaultPythonBin is pythonBin's default. While pythonBin is unchanged,
// the interpreter is searched with internal/pyfind (on Windows that tries
// python3, python and the py launcher). Once something sets pythonBin to
// another value, that exact command is used.
const defaultPythonBin = "python3"

// harnessSource is the harness with the list of tcp, udp, mqtt and kafka
// functions filled in from netapi, so Python and JavaScript offer the same.
var harnessSource = func() string {
	list, _ := json.Marshal(netapi.ProtoFunctions())
	return strings.Replace(harnessTemplate, "__PROTO_FUNCS__", string(list), 1)
}()

// findInterpreter returns the interpreter command to run.
func findInterpreter() (pyfind.Interpreter, error) {
	if pythonBin != defaultPythonBin {
		p, err := exec.LookPath(pythonBin)
		if err != nil {
			return pyfind.Interpreter{}, fmt.Errorf("python: %s not found on PATH: %w", pythonBin, err)
		}
		return pyfind.Interpreter{Path: p}, nil
	}
	in, ok := pyfind.FindOnThisMachine()
	if !ok {
		return pyfind.Interpreter{}, fmt.Errorf("python: no Python 3 interpreter found on PATH (tried %s). The Python scripting driver needs Python 3 installed separately, see AGENTS.md", strings.Join(pyfind.Names(runtime.GOOS), ", "))
	}
	return in, nil
}

// harnessTemplate is run, once its protocol list is filled in, as `python3 -c harnessSource <script path>`. It
// loads the scenario module once (with `http`/`ws` already present as
// globals), validates it, then answers a "ready" message and loops,
// running iteration() once per "iteration" command and answering a
// "result" message — proxying any http/ws call the script makes, mid
// iteration, back over the same pipe as a "call"/"call_result" exchange.
// See the package doc comment for a worked example and VU.Iteration for
// the Go side of this same protocol.
const harnessTemplate = `
import sys, json, runpy, itertools, time

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

def _expected_statuses(statuses):
    if statuses is None:
        return {}
    try:
        statuses = list(statuses)
    except TypeError:
        raise VegaloadError("expected_statuses: want a list of statuses, like [200, 404]")
    for s in statuses:
        if isinstance(s, bool) or not isinstance(s, int):
            raise VegaloadError("expected_statuses: want a list of whole numbers, like [200, 404], not %r" % (s,))
    return {"expected_statuses": statuses}

def _request_options(timeout, redirects, params):
    """timeout (seconds, or a duration string such as "500ms"), redirects
    and params, as given. VegaLoad checks them, the same way it does for
    JavaScript."""
    out = {}
    if timeout is not None:
        out["timeout"] = timeout
    if redirects is not None:
        out["redirects"] = redirects
    if params is not None:
        if not isinstance(params, dict):
            raise VegaloadError("params: want a dict of name: value pairs, like {\"page\": 2}")
        params = {str(k): list(v) if isinstance(v, (list, tuple)) else v for k, v in params.items()}
        try:
            json.dumps(params)
        except (TypeError, ValueError):
            raise VegaloadError("params: want strings, numbers, True or False, or lists of them")
        out["params"] = params
    return out

class HTTP:
    def request(self, method, url, headers=None, body=None, insecure=False, expected_statuses=None,
                timeout=None, redirects=None, params=None):
        result = _call("http", method=method, url=url,
                        headers=headers or {}, body=body or "", insecure=bool(insecure),
                        **_expected_statuses(expected_statuses),
                        **_request_options(timeout, redirects, params))
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

class _Reply(dict):
    """The answer of a tcp, udp, mqtt, kafka, grpc, postgres, mysql, redis, rabbitmq, ftp or sftp call. r.ok and r["ok"] mean
    the same. Nested messages and records are _Reply too, so
    r.messages[0].body works. A field named values is also an attribute:
    dict already has a values method, and the field would be hidden without this."""
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        if "values" in self:
            object.__setattr__(self, "values", self["values"])

    def __getattr__(self, name):
        try:
            return self[name]
        except KeyError:
            raise AttributeError(name)

def _wrap(value):
    if isinstance(value, dict):
        return _Reply({k: _wrap(v) for k, v in value.items()})
    if isinstance(value, list):
        return [_wrap(v) for v in value]
    return value

class _Proto:
    """tcp, udp, mqtt, kafka, grpc, postgres, mysql, redis, rabbitmq, ftp and sftp: one call per action, such as
    tcp.send(url, body="PING\\r\\n", until="\\r\\n"),
    kafka.produce(url, topic="t", value="v") or
    grpc.call(url, method="/pkg.Svc/Method", body={"name": "x"}). Options are keyword arguments.
    A word Python reserves gets a trailing underscore: from_="end". A call
    that fails on the network does not raise. It returns a reply with
    ok False and error set. A call that is set up wrongly raises
    VegaloadError."""
    def __init__(self, ns, funcs):
        for fn in funcs:
            setattr(self, fn, self._make(ns + "." + fn))

    @staticmethod
    def _make(name):
        def call(url, **options):
            return _wrap(_call("proto", fn=name, url=url, args=options))
        call.__name__ = name
        return call

_PROTO_FUNCS = __PROTO_FUNCS__
_PROTOS = {ns: _Proto(ns, funcs) for ns, funcs in _PROTO_FUNCS.items()}
tcp, udp, mqtt, kafka, grpc, postgres, mysql, redis, rabbitmq, ftp, sftp = _PROTOS["tcp"], _PROTOS["udp"], _PROTOS["mqtt"], _PROTOS["kafka"], _PROTOS["grpc"], _PROTOS["postgres"], _PROTOS["mysql"], _PROTOS["redis"], _PROTOS["rabbitmq"], _PROTOS["ftp"], _PROTOS["sftp"]

class _Env:
    """env.NAME, env["NAME"] or env.get("NAME", default): an environment
    variable the run exposed with -env or -secret-env. A name that was not
    exposed is missing, so env.NAME raises AttributeError."""
    def __init__(self):
        self._cache = {}

    def _lookup(self, name):
        if name not in self._cache:
            res = _call("env", name=name)
            self._cache[name] = res["value"] if res.get("found") else None
        return self._cache[name]

    def get(self, name, default=None):
        value = self._lookup(name)
        return default if value is None else value

    def __getitem__(self, name):
        value = self._lookup(name)
        if value is None:
            raise KeyError(name)
        return value

    def __getattr__(self, name):
        if name.startswith("_"):
            raise AttributeError(name)
        value = self._lookup(name)
        if value is None:
            raise AttributeError("env has no variable %r: pass it to vegaload with -env or -secret-env" % name)
        return value

class _DataFile:
    def __init__(self, name):
        self._name = name

    def next(self):
        return _call("data", name=self._name, action="next")["row"]

    def random(self):
        return _call("data", name=self._name, action="random")["row"]

    def __len__(self):
        return _call("data", name=self._name, action="length")["length"]

    @property
    def length(self):
        return len(self)

class _Data:
    """data.NAME is a data file given to vegaload with -data: next() returns
    the rows in file order, shared by all users, random() returns any row,
    and len(data.NAME) is the number of rows."""
    def __getattr__(self, name):
        if name.startswith("_"):
            raise AttributeError(name)
        return _DataFile(name)

    def __getitem__(self, name):
        return _DataFile(name)

env = _Env()
data = _Data()

def check(value, tests):
    """check(value, {"name": test, ...}): run each test on value and count
    it as passed or failed. A test is a function, called with value, or a
    plain bool. A test that raises counts as failed. Returns True only if
    every test passed."""
    if not isinstance(tests, dict):
        raise TypeError("check: want check(value, {name: test, ...})")
    all_passed = True
    outcomes = []
    for name, test in tests.items():
        if callable(test):
            try:
                passed = bool(test(value))
            except Exception:
                passed = False
        elif isinstance(test, bool):
            passed = test
        else:
            raise TypeError("check: %r must be a function or a bool" % (name,))
        outcomes.append({"name": str(name), "ok": passed})
        if not passed:
            all_passed = False
    # One round trip for the whole call, not one per test, so checks add
    # as little as possible to the iteration being timed.
    if outcomes:
        _call("check", results=outcomes)
    return all_passed

class _Step(object):
    """A named step: a context manager and a decorator-free callable form."""
    def __init__(self, name):
        if not isinstance(name, str) or name == "":
            raise TypeError("step: want step(name) or step(name, fn)")
        self._name = name
        self._t0 = None

    def __enter__(self):
        self._t0 = time.monotonic()
        return self

    def __exit__(self, exc_type, exc, tb):
        us = int((time.monotonic() - self._t0) * 1000000)
        # A step that raises is a failed step. The error carries on, so the
        # iteration fails too.
        _call("step", name=self._name, duration_us=us, ok=exc_type is None)
        return False

def step(name, fn=None):
    """step(name) is a context manager: "with step('login'):". step(name, fn)
    runs fn and returns its value. Latency and errors are reported per name."""
    s = _Step(name)
    if fn is None:
        return s
    if not callable(fn):
        raise TypeError("step: the second argument must be a function")
    with s:
        return fn()

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
            init_globals={"http": http, "ws": ws, "tcp": tcp, "udp": udp, "mqtt": mqtt, "kafka": kafka, "grpc": grpc, "postgres": postgres, "mysql": mysql, "redis": redis, "rabbitmq": rabbitmq, "ftp": ftp, "sftp": sftp, "check": check, "step": step, "env": env, "data": data},
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
	path   string
	interp pyfind.Interpreter
}

// Load returns a Script for the scenario file at path, after confirming
// the file exists and that pythonBin is on PATH. It does not start an
// interpreter or touch the script's contents — see NewVU.
func Load(path string) (*Script, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("python: %w", err)
	}
	interp, err := findInterpreter()
	if err != nil {
		return nil, err
	}
	return &Script{path: path, interp: interp}, nil
}

// message is one line of the harness's stdout protocol: a "ready"
// handshake at startup, a "result" ending one iteration, or a "call"
// asking the Go side to perform one http/ws operation mid-iteration.
// checkOutcome is one test's result inside a "check" call.
type checkOutcome struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
}

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
	// ExpectedStatuses is nil when the script did not give
	// expected_statuses, and empty when it gave an empty list.
	ExpectedStatuses []int `json:"expected_statuses"`
	// HTTPTimeout, Redirects and Params are the http call's timeout,
	// redirects and params, as the script gave them; netapi's parsers
	// check them. They are nil when not given.
	HTTPTimeout any    `json:"timeout"`
	Redirects   any    `json:"redirects"`
	Params      any    `json:"params"`
	Name        string `json:"name"`
	Handle      int    `json:"handle"`
	Data        string `json:"data"`
	Text        bool   `json:"text"`
	// Fn and Args are a "proto" call: the function (such as "tcp.send")
	// and its keyword arguments.
	Fn   string         `json:"fn"`
	Args map[string]any `json:"args"`
	// Results is the batch of outcomes on a "check" call.
	Results []checkOutcome `json:"results"`
	// DurationUs is how long a "step" call took, in microseconds. OK says
	// whether the step finished without an error.
	DurationUs int64 `json:"duration_us"`
	// Action is the operation on a "data" call: next, random or length.
	Action string `json:"action"`
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
	proto       *netapi.ProtoClient
	safetyCheck netapi.SafetyCheck
	timeout     time.Duration
	ctx         context.Context //nolint:containedctx // set per-Iteration; dispatchCall reads it synchronously within that same iteration to build call/dial contexts.

	conns      map[int]*netapi.WSConn
	nextHandle int

	// checks receives every check() outcome (FR-CLI-12). It may be nil.
	checks netapi.CheckRecorder
	// steps receives every step() outcome (FR-CLI-18). It may be nil.
	steps netapi.StepRecorder
	// calls receives every protocol call this VU makes. It may be nil.
	calls netapi.CallRecorder

	// inputs is what the `env` and `data` globals read (FR-CLI-17). It
	// may be nil, which exposes nothing.
	inputs netapi.Inputs

	// httpOpts are how NewVU builds http, set by Options.
	httpOpts []netapi.HTTPOption
}

// SetCheckRecorder sets where this VU reports the outcome of every
// check() its script makes. Call it before the first Iteration.
func (v *VU) SetCheckRecorder(rec netapi.CheckRecorder) { v.checks = rec }

// SetStepRecorder sets where this VU reports the timing and outcome of
// every step() its script runs. Call it before the first Iteration.
func (v *VU) SetStepRecorder(rec netapi.StepRecorder) { v.steps = rec }

// SetCallRecorder counts the HTTP, WebSocket, and protocol calls this VU
// makes. Call it before the first Iteration. Nil means not counted.
func (v *VU) SetCallRecorder(rec netapi.CallRecorder) {
	v.calls = rec
	v.http.SetCallRecorder(rec)
	v.proto.SetCallRecorder(rec)
}

// Option changes how NewVU builds a VU. An option is needed, rather than a
// setter called afterwards, when the script's top-level code has to see it:
// the top level runs while NewVU waits for the interpreter to be ready.
type Option func(*VU)

// WithInputs exposes the `env` and `data` globals (FR-CLI-17). Without it,
// every env name is missing and every data file is unknown.
func WithInputs(in netapi.Inputs) Option { return func(v *VU) { v.inputs = in } }

// WithNoConnectionReuse makes every http call open a new connection
// (-no-connection-reuse).
func WithNoConnectionReuse() Option {
	return func(v *VU) { v.httpOpts = append(v.httpOpts, netapi.NoConnectionReuse()) }
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
func (s *Script) NewVU(check netapi.SafetyCheck, timeout time.Duration, opts ...Option) (*VU, error) {
	cmdArgs := append(append([]string{}, s.interp.Args...), "-u", "-c", harnessSource, s.path)
	cmd := exec.Command(s.interp.Path, cmdArgs...) //nolint:gosec // path and interpreter are operator-controlled, not request input

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
		proto:       netapi.NewProtoClient(check, timeout),
		safetyCheck: check,
		timeout:     timeout,
		conns:       make(map[int]*netapi.WSConn),
	}
	for _, opt := range opts {
		opt(vu)
	}
	vu.http = netapi.NewHTTPClient(check, timeout, vu.httpOpts...)

	msg, err := vu.readMessage()
	// While the script's top-level code runs, it may read env and data. Any
	// other call is refused: there is no iteration yet to run it in.
	for err == nil && msg.Type == "call" {
		if msg.Op != "env" && msg.Op != "data" {
			_ = vu.writeCmd(cmdMsg{Cmd: "call_result", ID: msg.ID, OK: false, Error: fmt.Sprintf("%s calls are not available while the script is loading, only inside iteration()", msg.Op)})
		} else if herr := vu.handleCall(msg); herr != nil {
			err = herr
			break
		}
		msg, err = vu.readMessage()
	}
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
	case "check":
		if v.checks != nil {
			for _, r := range msg.Results {
				if !r.OK && netapi.RunEnded(v.ctx) {
					// Cut off by the end of the run, not a real failure.
					// Python runs every test of a check() call first and
					// sends one list, so this decision is made once, when
					// the list arrives. If the run ended meanwhile, every
					// failed test in the list is dropped, even one that
					// failed earlier. Passed tests are still counted.
					continue
				}
				v.checks.RecordCheck(r.Name, r.OK)
			}
		}
		return map[string]interface{}{}, nil
	case "step":
		if v.steps != nil && !(!msg.OK && netapi.RunEnded(v.ctx)) {
			// A step cut off by the end of the run did not fail.
			v.steps.RecordStep(msg.Name, time.Duration(msg.DurationUs)*time.Microsecond, !msg.OK)
		}
		return map[string]interface{}{}, nil
	case "proto":
		return v.callProto(msg)
	case "env":
		return v.callEnv(msg), nil
	case "data":
		return v.callData(msg)
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

// callProto makes one tcp, udp, mqtt or kafka call and returns its reply as
// plain data. A network failure is a reply with ok false. A call set up
// wrongly, or refused by the safety check, is an error the script sees as
// VegaloadError.
func (v *VU) callProto(msg message) (interface{}, error) {
	pc, err := netapi.ProtoCallFromArgs(msg.Fn, msg.URL, msg.Args)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", msg.Fn, err)
	}
	reply, err := v.proto.Call(v.ctx, msg.Fn, pc)
	if err != nil {
		return nil, err
	}
	return reply.Fields(), nil
}

// callEnv answers env.NAME: the variable's value if the run exposed it.
func (v *VU) callEnv(msg message) interface{} {
	if v.inputs != nil {
		if val, ok := v.inputs.Env(msg.Name); ok {
			return map[string]interface{}{"found": true, "value": val}
		}
	}
	return map[string]interface{}{"found": false}
}

// callData answers data.NAME.next(), .random() and len(data.NAME).
func (v *VU) callData(msg message) (interface{}, error) {
	var ds netapi.Dataset
	if v.inputs != nil {
		ds, _ = v.inputs.Dataset(msg.Name)
	}
	if ds == nil {
		return nil, fmt.Errorf("no data file named %q: give it to vegaload with -data", msg.Name)
	}
	switch msg.Action {
	case "next":
		return map[string]interface{}{"row": ds.Next()}, nil
	case "random":
		return map[string]interface{}{"row": ds.Random()}, nil
	case "length":
		return map[string]interface{}{"length": ds.Len()}, nil
	default:
		return nil, fmt.Errorf("unknown data action %q", msg.Action)
	}
}

func (v *VU) callHTTP(msg message) (interface{}, error) {
	if err := netapi.CheckExpectedStatuses(msg.ExpectedStatuses); err != nil {
		return nil, fmt.Errorf("expected_statuses: %w", err)
	}
	timeout, err := netapi.ParseTimeout(msg.HTTPTimeout, time.Second)
	if err != nil {
		return nil, fmt.Errorf("timeout: %w", err)
	}
	redirects, err := netapi.ParseRedirects(msg.Redirects)
	if err != nil {
		return nil, fmt.Errorf("redirects: %w", err)
	}
	params, err := netapi.ParseParams(msg.Params)
	if err != nil {
		return nil, fmt.Errorf("params: %w", err)
	}
	var body []byte
	if msg.Body != "" {
		body = []byte(msg.Body)
	}
	resp, err := v.http.Do(v.ctx, msg.Method, msg.URL, body, netapi.Options{
		Headers:            msg.Headers,
		InsecureSkipVerify: msg.Insecure,
		ExpectedStatuses:   msg.ExpectedStatuses,
		Timeout:            timeout,
		Redirects:          redirects,
		Params:             params,
	})
	if err != nil {
		return nil, err
	}
	headers := netapi.ScriptHeaders(resp.Headers)
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
		Recorder:           v.calls,
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
	v.proto.Close()
	return v.cmd.Wait()
}
