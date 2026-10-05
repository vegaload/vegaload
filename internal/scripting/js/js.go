// Package js runs a scenario written as a JavaScript or TypeScript file,
// using the embedded Goja ECMAScript runtime — no Node.js, no network
// fetch of a runtime, and no separate build step for the author. A
// scenario file exports a default function the same way a k6 script
// does:
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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/evanw/esbuild/pkg/api"

	"github.com/vegaload/vegaload/internal/scripting/netapi"
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
	safetyCheck netapi.SafetyCheck
	timeout     time.Duration
	ctx         context.Context //nolint:containedctx // set per-Iteration; native functions called synchronously from within that same iteration read it to build call/dial contexts.

	openConns []*netapi.WSConn
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
func (s *Script) NewVU(check netapi.SafetyCheck, timeout time.Duration) (*VU, error) {
	vm := goja.New()
	// requestOptions (the http.*/ws.connect options argument) is defined
	// with `json` tags so its Go field names don't have to match a
	// script's lowerCamelCase property names (`headers`, not `Headers`);
	// this mapper is what makes goja's ExportTo honor those tags.
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))

	v := &VU{
		vm:          vm,
		http:        netapi.NewHTTPClient(check, timeout),
		safetyCheck: check,
		timeout:     timeout,
	}

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
		fmt.Println(strings.Join(parts, " "))
		return goja.Undefined()
	}
	_ = c.Set("log", logFn)
	return c
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
// http.* method and ws.connect: `{ headers, body, insecure }`. All
// fields are optional; a caller that passes no options object at all
// gets the zero value.
type requestOptions struct {
	Headers  map[string]string `json:"headers"`
	Body     string            `json:"body"`
	Insecure bool              `json:"insecure"`
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
	return opts
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

	_ = h.Set("get", doHTTP("GET"))
	_ = h.Set("post", doHTTP("POST"))
	_ = h.Set("put", doHTTP("PUT"))
	_ = h.Set("patch", doHTTP("PATCH"))
	_ = h.Set("delete", doHTTP("DELETE"))
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
	for k := range resp.Headers {
		_ = headers.Set(k, resp.Headers.Get(k))
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
	for _, conn := range v.openConns {
		_ = conn.Close()
	}
	return nil
}
