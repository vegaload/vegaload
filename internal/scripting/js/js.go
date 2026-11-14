// Package js runs a scenario written as a JavaScript or TypeScript file,
// using the embedded Goja ECMAScript runtime — no Node.js, no network
// fetch of a runtime, and no separate build step for the author. A
// scenario file exports a default function:
//
//	export default function () {
//	  // one iteration's worth of work
//	}
//
// A .ts file is accepted too: it is transpiled to JavaScript (types
// stripped, ESM import/export rewritten to a form this package can read
// back out) with esbuild before Goja ever sees it. esbuild is a
// committed dependency of this binary, not an external tool — nothing
// about this package requires Node, npm, or network access to work.
//
// AGENTS.md's first rule is that a test is always a real file; this
// package's whole job is turning that file into something the engine can
// call for a VU. Per FR-CLI-08, a VU also gets real HTTP and WebSocket
// access via the global `http` and `ws` objects described below, backed
// by internal/scripting/netapi — this package still knows nothing about
// gRPC, and nothing about FR-CLI-06's allowlist policy itself, only that
// NewVU is given a netapi.SafetyCheck to run before every call.
//
// A scenario can carry a value from one call into the next the way
// FR-CLI-08's LaunchPad example needs to:
//
//	export default function () {
//	  const created = http.post("http://localhost:8080/launches", { body: JSON.stringify({name: "demo"}) });
//	  const launch = created.json();
//	  const conn = ws.connect("ws://localhost:8080/launches/" + launch.id + "/ignite");
//	  conn.send(JSON.stringify({action: "ignite"}));
//	  for (;;) {
//	    const frame = conn.receive(5000);
//	    if (frame === null) break; // connection closed
//	    const msg = JSON.parse(frame);
//	    if (msg.status === "complete" || msg.status === "aborted") break;
//	  }
//	  conn.close();
//	}
package js

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/evanw/esbuild/pkg/api"

	"github.com/vegaload/vegaload/internal/scripting/netapi"
	"github.com/vegaload/vegaload/internal/secrets"
)

// Script is a loaded, compiled scenario file, ready to be instantiated
// into any number of VUs.
type Script struct {
	program *goja.Program
}

// Load reads and compiles the scenario file at path. A .ts, .tsx, or .jsx
// file is transpiled first; anything else is treated as plain
// JavaScript. Load returns an error for anything that stops every VU
// identically — a missing file, a syntax error, a transpile failure — so
// the caller can fail the whole run immediately instead of discovering
// it only when the first VU starts.
func Load(path string) (*Script, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("js: reading %s: %w", path, err)
	}

	code, err := transpile(path, src)
	if err != nil {
		return nil, fmt.Errorf("js: transpiling %s: %w", path, err)
	}

	program, err := goja.Compile(path, code, false)
	if err != nil {
		return nil, fmt.Errorf("js: compiling %s: %w", path, err)
	}

	return &Script{program: program}, nil
}

// transpile converts src to CommonJS JavaScript that Goja can run and
// this package can pull a default export back out of. It always runs
// src through esbuild, even plain .js: esbuild's CommonJS output format
// is what turns `export default function(){}` into an `exports.default`
// assignment this package's VU setup can read, so a .js file using ESM
// export syntax (the only form this package documents) needs the same
// pass a .ts file does.
func transpile(path string, src []byte) (string, error) {
	result := api.Transform(string(src), api.TransformOptions{
		Loader:     loaderFor(path),
		Format:     api.FormatCommonJS,
		Target:     api.ES2020,
		Sourcefile: filepath.Base(path),
	})
	if len(result.Errors) > 0 {
		msgs := make([]string, len(result.Errors))
		for i, e := range result.Errors {
			msgs[i] = e.Text
		}
		return "", errors.New(strings.Join(msgs, "; "))
	}
	return string(result.Code), nil
}

func loaderFor(path string) api.Loader {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts":
		return api.LoaderTS
	case ".tsx":
		return api.LoaderTSX
	case ".jsx":
		return api.LoaderJSX
	default:
		return api.LoaderJS
	}
}

// VU is one virtual user's instance of a Script: its own Goja runtime
// running its own copy of the script's top-level state, the default
// export function pulled out of it, and (per FR-CLI-08) its own HTTP
// client and set of open WebSocket connections, scoped to this VU's
// lifetime the same way the VU's own runtime is. A goja.Runtime is not
// safe for concurrent use, so the engine must create one VU per virtual
// user via NewVU — never share a single VU across goroutines.
type VU struct {
	vm *goja.Runtime
	fn goja.Callable

	http        *netapi.HTTPClient
	proto       *netapi.ProtoClient
	safetyCheck netapi.SafetyCheck
	timeout     time.Duration
	ctx         context.Context //nolint:containedctx // set per-Iteration; native functions called synchronously from within that same iteration read it to build call/dial contexts.

	openConns []*netapi.WSConn

	// checks receives every check() outcome (FR-CLI-12). It may be nil.
	checks netapi.CheckRecorder
	// steps receives every step() outcome (FR-CLI-18). It may be nil.
	steps netapi.StepRecorder
	// calls receives every protocol call this VU makes. It may be nil.
	calls netapi.CallRecorder

	// inputs is what the `env` and `data` globals expose (FR-CLI-17). It
	// may be nil, which exposes nothing.
	inputs netapi.Inputs

	// httpOpts are how NewVU builds http, set by Options.
	httpOpts []netapi.HTTPOption
}

// SetCheckRecorder sets where this VU reports the outcome of every
// check() its script makes. Call it before the first Iteration. With no
// recorder (or nil), check() still runs the tests and returns the
// result, but nothing is counted.
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
// the top level runs inside NewVU.
type Option func(*VU)

// WithInputs exposes the `env` and `data` globals (FR-CLI-17). Without it,
// `env` is an empty object and `data` has no files.
func WithInputs(in netapi.Inputs) Option { return func(v *VU) { v.inputs = in } }

// WithNoConnectionReuse makes every http call open a new connection
// (-no-connection-reuse).
func WithNoConnectionReuse() Option {
	return func(v *VU) { v.httpOpts = append(v.httpOpts, netapi.NoConnectionReuse()) }
}

// NewVU creates a fresh VU from the script: a new runtime, the script's
// top-level code run once (so top-level state like a counter declared
// with let/const starts fresh for this VU), its default export resolved
// and validated, and the `http`/`ws` globals wired up.
//
// check is run (if non-nil) against the host of every call a script
// makes through http or ws, before connecting — see
// internal/scripting/netapi.SafetyCheck's doc comment for why this has
// to happen per call rather than once up front. timeout bounds each
// individual HTTP request and WebSocket handshake; it does not bound
// ws.receive, which takes its own optional timeout argument.
func (s *Script) NewVU(check netapi.SafetyCheck, timeout time.Duration, opts ...Option) (*VU, error) {
	vm := goja.New()
	// requestOptions (the http.*/ws.connect options argument) is defined
	// with `json` tags so its Go field names don't have to match a
	// script's lowerCamelCase property names (`headers`, not `Headers`);
	// this mapper is what makes goja's ExportTo honor those tags.
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))

	v := &VU{
		vm:          vm,
		proto:       netapi.NewProtoClient(check, timeout),
		safetyCheck: check,
		timeout:     timeout,
	}
	for _, opt := range opts {
		opt(v)
	}
	v.http = netapi.NewHTTPClient(check, timeout, v.httpOpts...)

	exportsObj := vm.NewObject()
	moduleObj := vm.NewObject()
	if err := moduleObj.Set("exports", exportsObj); err != nil {
		return nil, fmt.Errorf("js: %w", err)
	}
	if err := vm.Set("exports", exportsObj); err != nil {
		return nil, fmt.Errorf("js: %w", err)
	}
	if err := vm.Set("module", moduleObj); err != nil {
		return nil, fmt.Errorf("js: %w", err)
	}
	if err := vm.Set("console", newConsole(vm)); err != nil {
		return nil, fmt.Errorf("js: %w", err)
	}
	if err := vm.Set("http", v.newHTTPGlobal(vm)); err != nil {
		return nil, fmt.Errorf("js: %w", err)
	}
	if err := vm.Set("ws", v.newWSGlobal(vm)); err != nil {
		return nil, fmt.Errorf("js: %w", err)
	}
	for name, obj := range v.newProtoGlobals(vm) {
		if err := vm.Set(name, obj); err != nil {
			return nil, fmt.Errorf("js: %w", err)
		}
	}
	if err := vm.Set("check", v.newCheckFunc(vm)); err != nil {
		return nil, fmt.Errorf("js: %w", err)
	}
	if err := vm.Set("step", v.newStepFunc(vm)); err != nil {
		return nil, fmt.Errorf("js: %w", err)
	}
	if err := vm.Set("env", v.newEnvGlobal(vm)); err != nil {
		return nil, fmt.Errorf("js: %w", err)
	}
	if err := vm.Set("data", v.newDataGlobal(vm)); err != nil {
		return nil, fmt.Errorf("js: %w", err)
	}

	if _, err := vm.RunProgram(s.program); err != nil {
		return nil, fmt.Errorf("js: running script: %w", err)
	}

	// esbuild's CommonJS output does not assign properties onto the
	// `exports` object this package provided — it builds its own
	// exports object internally and reassigns `module.exports` to it
	// wholesale (`module.exports = __toCommonJS(stdin_exports)`), which
	// is why the default export has to be read back off moduleObj
	// rather than off the original exportsObj.
	finalExports := moduleObj.Get("exports")
	exportsAsObject, ok := finalExports.(*goja.Object)
	if !ok {
		return nil, fmt.Errorf("js: script's module.exports is not an object")
	}
	def := exportsAsObject.Get("default")
	if def == nil || goja.IsUndefined(def) {
		return nil, fmt.Errorf("js: script has no default export function (expected `export default function() { ... }`)")
	}
	fn, ok := goja.AssertFunction(def)
	if !ok {
		return nil, fmt.Errorf("js: default export is not a function")
	}
	v.fn = fn

	return v, nil
}

// newConsole builds a minimal console object (just log, matching what a
// scenario author reaches for first) backed by the Go standard log
// output, so a script's console.log calls show up without needing a
// real Node-style console implementation.
func newConsole(vm *goja.Runtime) *goja.Object {
	c := vm.NewObject()
	logFn := func(call goja.FunctionCall) goja.Value {
		parts := make([]string, len(call.Arguments))
		for i, a := range call.Arguments {
			parts[i] = a.String()
		}
		fmt.Println(secrets.Redact(strings.Join(parts, " ")))
		return goja.Undefined()
	}
	_ = c.Set("log", logFn)
	return c
}

// newEnvGlobal builds the `env` global (FR-CLI-17): one string property
// per environment variable the run exposed with -env or -secret-env.
// A name that was not exposed is undefined.
func (v *VU) newEnvGlobal(vm *goja.Runtime) *goja.Object {
	obj := vm.NewObject()
	if v.inputs == nil {
		return obj
	}
	for _, name := range v.inputs.EnvNames() {
		if val, ok := v.inputs.Env(name); ok {
			_ = obj.Set(name, val)
		}
	}
	return obj
}

// newDataGlobal builds the `data` global (FR-CLI-17): one property per
// data file, each with next(), random() and length.
//
//	const row = data.users.next();   // rows in file order, shared by all VUs
//	const any = data.users.random(); // any row
//	data.users.length                // the number of rows
func (v *VU) newDataGlobal(vm *goja.Runtime) *goja.Object {
	obj := vm.NewObject()
	if v.inputs == nil {
		return obj
	}
	for _, name := range v.inputs.DatasetNames() {
		ds, ok := v.inputs.Dataset(name)
		if !ok {
			continue
		}
		set := vm.NewObject()
		_ = set.Set("next", func(goja.FunctionCall) goja.Value { return vm.ToValue(ds.Next()) })
		_ = set.Set("random", func(goja.FunctionCall) goja.Value { return vm.ToValue(ds.Random()) })
		_ = set.Set("length", ds.Len())
		_ = obj.Set(name, set)
	}
	return obj
}

// newCheckFunc builds the global check(value, tests) function
// (FR-CLI-12):
//
//	check(res, {
//	  "status is 200": (r) => r.status === 200,
//	  "has an id":     (r) => r.json().id !== undefined,
//	});
//
// Each entry of tests is a name and either a function, called with
// value, or a plain boolean. A test passes when it returns a truthy
// value; a test that throws counts as failed. A failed check is counted
// and the script carries on, so one bad response does not end the
// iteration. check returns true only if every test passed.
func (v *VU) newCheckFunc(vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		val := call.Argument(0)
		testsArg := call.Argument(1)
		if goja.IsUndefined(testsArg) || goja.IsNull(testsArg) {
			throw(vm, fmt.Errorf("check: want check(value, {name: test, ...})"))
		}
		tests := testsArg.ToObject(vm)
		allPassed := true
		for _, name := range tests.Keys() {
			tv := tests.Get(name)
			passed := false
			if fn, isFn := goja.AssertFunction(tv); isFn {
				res, err := fn(goja.Undefined(), val)
				passed = err == nil && res.ToBoolean()
			} else if b, isBool := tv.Export().(bool); isBool {
				passed = b
			} else {
				throw(vm, fmt.Errorf("check: %q must be a function or a boolean", name))
			}
			if !passed {
				allPassed = false
				if netapi.RunEnded(v.ctx) {
					// The run ended while this test ran, or just before.
					// A test that fails now was cut off. It did not fail.
					// Counting it would add false failed checks, one for
					// each VU that was busy at the end. Stop here, as
					// before: the tests after it were cut off too. Tests
					// before it that passed are already counted.
					return vm.ToValue(false)
				}
			}
			if v.checks != nil {
				v.checks.RecordCheck(name, passed)
			}
		}
		return vm.ToValue(allPassed)
	}
}

// newStepFunc builds the step(name, fn) global (FR-CLI-18). It runs fn,
// times it, and reports the time and outcome under name. A step that
// throws is a failed step, and the exception carries on, so the iteration
// fails too. The value fn returns is the value step returns. Steps may
// nest; each is recorded on its own.
func (v *VU) newStepFunc(vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		nameV := call.Argument(0)
		fn, isFn := goja.AssertFunction(call.Argument(1))
		if goja.IsUndefined(nameV) || goja.IsNull(nameV) || nameV.String() == "" || !isFn {
			throw(vm, fmt.Errorf("step: want step(name, fn)"))
		}
		name := nameV.String()
		start := time.Now()
		res, err := fn(goja.Undefined())
		d := time.Since(start)
		if v.steps != nil && !(err != nil && netapi.RunEnded(v.ctx)) {
			v.steps.RecordStep(name, d, err != nil)
		}
		if err != nil {
			// Re-raise the original exception unchanged.
			if ex, ok := err.(*goja.Exception); ok {
				panic(ex.Value())
			}
			panic(vm.NewGoError(err))
		}
		return res
	}
}

// throw raises err as a catchable JS exception (a Go Error instance
// wrapping err, via goja's own NewGoError), rather than a Go panic
// escaping the runtime. Every native function below uses this instead of
// returning a Go error, since goja.FunctionCall-shaped functions have no
// error return of their own — panicking with a *goja.Object is goja's
// documented way for a native function to signal a JS-visible error.
func throw(vm *goja.Runtime, err error) {
	panic(vm.NewGoError(err))
}

// requestOptions is the shape of the optional second argument to every
// http.* method and ws.connect: `{ headers, body, insecure,
// expectedStatuses, timeout, redirects, params }`. All fields are
// optional; a caller that passes no options object at all gets the zero
// value. expectedStatuses is read by parseExpectedStatuses, not by the
// field mapper, so that a number that is not whole is refused instead of
// rounded. timeout (a number of milliseconds, or a duration string such as
// "5s"), redirects and params are read by netapi's parsers, the same ones
// Python uses; ws.connect ignores them.
type requestOptions struct {
	Headers          map[string]string `json:"headers"`
	Body             string            `json:"body"`
	Insecure         bool              `json:"insecure"`
	ExpectedStatuses []int             `json:"-"`
	Timeout          time.Duration     `json:"-"`
	Redirects        *int              `json:"-"`
	Params           url.Values        `json:"-"`
}

func parseOptions(vm *goja.Runtime, call goja.FunctionCall, argIndex int) requestOptions {
	var opts requestOptions
	if len(call.Arguments) <= argIndex {
		return opts
	}
	arg := call.Arguments[argIndex]
	if goja.IsUndefined(arg) || goja.IsNull(arg) {
		return opts
	}
	if err := vm.ExportTo(arg, &opts); err != nil {
		throw(vm, fmt.Errorf("js: parsing options: %w", err))
	}
	obj := arg.ToObject(vm)
	statuses, err := parseExpectedStatuses(vm, obj.Get("expectedStatuses"))
	if err != nil {
		throw(vm, fmt.Errorf("js: expectedStatuses: %w", err))
	}
	opts.ExpectedStatuses = statuses
	if opts.Timeout, err = netapi.ParseTimeout(exportOption(obj.Get("timeout")), time.Millisecond); err != nil {
		throw(vm, fmt.Errorf("js: timeout: %w", err))
	}
	if opts.Redirects, err = netapi.ParseRedirects(exportOption(obj.Get("redirects"))); err != nil {
		throw(vm, fmt.Errorf("js: redirects: %w", err))
	}
	if opts.Params, err = netapi.ParseParams(exportOption(obj.Get("params"))); err != nil {
		throw(vm, fmt.Errorf("js: params: %w", err))
	}
	return opts
}

// exportOption is an option's plain Go value, or nil when the script left
// it out or gave null.
func exportOption(v goja.Value) any {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	return v.Export()
}

// parseExpectedStatuses reads the expectedStatuses option: nil when it is
// not given, else an array of whole numbers that are HTTP statuses.
func parseExpectedStatuses(vm *goja.Runtime, v goja.Value) ([]int, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}
	items, ok := v.Export().([]interface{})
	if !ok {
		return nil, fmt.Errorf("want an array of statuses, like [200, 404]")
	}
	statuses := make([]int, 0, len(items))
	for _, item := range items {
		var n float64
		switch x := item.(type) {
		case int64:
			n = float64(x)
		case float64:
			n = x
		default:
			return nil, fmt.Errorf("want an array of whole numbers, like [200, 404], not %v", vm.ToValue(item))
		}
		if n != math.Trunc(n) {
			return nil, fmt.Errorf("want whole numbers, not %v", n)
		}
		statuses = append(statuses, int(n))
	}
	if err := netapi.CheckExpectedStatuses(statuses); err != nil {
		return nil, err
	}
	return statuses, nil
}

// newHTTPGlobal builds the `http` object every VU's scripts see:
// shorthand methods for the common verbs, plus `request` for anything
// else, each returning a response object a script can inspect and parse
// -- the value-chaining FR-CLI-08 exists for.
func (v *VU) newHTTPGlobal(vm *goja.Runtime) *goja.Object {
	h := vm.NewObject()

	doHTTP := func(method string) func(call goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 1 {
				throw(vm, errors.New("js: http."+strings.ToLower(method)+"(url[, options]) requires a url argument"))
			}
			url := call.Arguments[0].String()
			opts := parseOptions(vm, call, 1)
			return v.doHTTPRequest(vm, method, url, opts)
		}
	}

	for _, name := range ScriptHTTPMethods() {
		if name == "request" {
			continue
		}
		_ = h.Set(name, doHTTP(strings.ToUpper(name)))
	}
	_ = h.Set("request", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 2 {
			throw(vm, errors.New("js: http.request(method, url[, options]) requires method and url arguments"))
		}
		method := call.Arguments[0].String()
		url := call.Arguments[1].String()
		opts := parseOptions(vm, call, 2)
		return v.doHTTPRequest(vm, method, url, opts)
	})

	return h
}

// ScriptHTTPMethods are the functions on the http global, in both
// JavaScript and Python. request takes a method name; the others do not.
func ScriptHTTPMethods() []string {
	return []string{"get", "post", "put", "patch", "delete", "request"}
}

// ScriptWSMethods are the WebSocket calls a script can make. connect is on
// the ws global. send, receive and close are methods of the connection.
func ScriptWSMethods() []string {
	return []string{"connect", "send", "receive", "close"}
}

func (v *VU) doHTTPRequest(vm *goja.Runtime, method, url string, opts requestOptions) goja.Value {
	if v.ctx == nil {
		throw(vm, errors.New("js: http call made outside of an iteration"))
	}
	var body []byte
	if opts.Body != "" {
		body = []byte(opts.Body)
	}
	resp, err := v.http.Do(v.ctx, method, url, body, netapi.Options{
		Headers:            opts.Headers,
		InsecureSkipVerify: opts.Insecure,
		ExpectedStatuses:   opts.ExpectedStatuses,
		Timeout:            opts.Timeout,
		Redirects:          opts.Redirects,
		Params:             opts.Params,
	})
	if err != nil {
		throw(vm, err)
	}
	return v.wrapHTTPResponse(vm, resp)
}

// wrapHTTPResponse turns a netapi.HTTPResponse into the JS object a
// script works with: `{status, ok, body, headers, json()}`. body is
// exposed as a string (scripts deal in JSON and text, not byte arrays);
// json() parses it and throws a catchable error if it isn't valid JSON,
// so a script can call response.json() the same way it would in a
// browser fetch API.
func (v *VU) wrapHTTPResponse(vm *goja.Runtime, resp *netapi.HTTPResponse) *goja.Object {
	o := vm.NewObject()
	_ = o.Set("status", resp.StatusCode)
	_ = o.Set("ok", resp.StatusCode >= 200 && resp.StatusCode < 300)
	bodyStr := string(resp.Body)
	_ = o.Set("body", bodyStr)

	headers := vm.NewObject()
	for k, v := range netapi.ScriptHeaders(resp.Headers) {
		_ = headers.Set(k, v)
	}
	_ = o.Set("headers", headers)

	_ = o.Set("json", func(call goja.FunctionCall) goja.Value {
		var parsed interface{}
		if err := json.Unmarshal(resp.Body, &parsed); err != nil {
			throw(vm, fmt.Errorf("js: response.json(): %w", err))
		}
		return vm.ToValue(parsed)
	})

	return o
}

// newWSGlobal builds the `ws` object: `connect(url[, options])` opens a
// connection a script can send to and receive from repeatedly, unlike
// internal/protocol/websocket.Driver's one-shot-per-Do behavior.
func (v *VU) newWSGlobal(vm *goja.Runtime) *goja.Object {
	w := vm.NewObject()
	_ = w.Set("connect", func(call goja.FunctionCall) goja.Value {
		if v.ctx == nil {
			throw(vm, errors.New("js: ws.connect call made outside of an iteration"))
		}
		if len(call.Arguments) < 1 {
			throw(vm, errors.New("js: ws.connect(url[, options]) requires a url argument"))
		}
		url := call.Arguments[0].String()
		opts := parseOptions(vm, call, 1)

		conn, err := netapi.Dial(v.ctx, v.safetyCheck, url, v.timeout, netapi.Options{
			Headers:            opts.Headers,
			InsecureSkipVerify: opts.Insecure,
			Recorder:           v.calls,
		})
		if err != nil {
			throw(vm, err)
		}
		v.openConns = append(v.openConns, conn)
		return v.wrapWSConn(vm, conn)
	})
	return w
}

// wrapWSConn turns a netapi.WSConn into the JS object a script works
// with: `{send(data), receive(timeoutMs), close()}`. receive returns
// null on a clean close, so a "read frames until done" loop can check
// for that directly, matching FR-CLI-08's own LaunchPad example.
func (v *VU) wrapWSConn(vm *goja.Runtime, conn *netapi.WSConn) *goja.Object {
	o := vm.NewObject()

	_ = o.Set("send", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 1 {
			throw(vm, errors.New("js: send(data) requires a data argument"))
		}
		data := call.Arguments[0].String()
		if err := conn.Send(v.ctx, []byte(data), true); err != nil {
			throw(vm, err)
		}
		return goja.Undefined()
	})

	_ = o.Set("receive", func(call goja.FunctionCall) goja.Value {
		ctx := v.ctx
		if len(call.Arguments) >= 1 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
			ms := call.Arguments[0].ToInteger()
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(v.ctx, time.Duration(ms)*time.Millisecond)
			defer cancel()
		}
		data, closed, err := conn.Receive(ctx)
		if err != nil {
			throw(vm, err)
		}
		if closed {
			return goja.Null()
		}
		return vm.ToValue(string(data))
	})

	_ = o.Set("close", func(call goja.FunctionCall) goja.Value {
		if err := conn.Close(); err != nil {
			throw(vm, err)
		}
		return goja.Undefined()
	})

	return o
}

// Iteration runs the script's default export once. It implements
// engine.IterationFunc's signature (func(context.Context) error)
// directly, so a *VU can be used anywhere the engine wants one, without
// an adapter: `executor.Run(ctx, vu.Iteration, recorder)`.
//
// ctx is also what every http/ws call this iteration makes uses to bound
// and cancel itself — it's stashed on v for the native functions above
// to read, since goja.FunctionCall gives them no way to receive it
// directly.
//
// If ctx is cancelled while the function is running, Iteration
// interrupts the runtime and returns promptly instead of waiting for a
// runaway script to finish on its own.
func (v *VU) Iteration(ctx context.Context) error {
	v.vm.ClearInterrupt()
	v.ctx = ctx

	done := make(chan struct{})
	defer close(done)
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				v.vm.Interrupt(ctx.Err())
			case <-done:
			}
		}()
	}

	_, err := v.fn(goja.Undefined())
	if err == nil {
		return nil
	}

	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		return fmt.Errorf("js: %w", ctx.Err())
	}

	var jsErr *goja.Exception
	if errors.As(err, &jsErr) {
		return fmt.Errorf("js: %s", jsErr.Value().String())
	}
	return fmt.Errorf("js: %w", err)
}

// Close releases v's resources: the VU's HTTP client's idle connections
// and every WebSocket connection it ever opened via ws.connect, even one
// a script never explicitly closed itself.
func (v *VU) Close() error {
	v.http.Close()
	v.proto.Close()
	for _, conn := range v.openConns {
		_ = conn.Close()
	}
	return nil
}
