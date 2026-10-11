// Package redis implements the "redis" protocol.Protocol driver, for load
// testing a Redis server or a store that speaks the same wire protocol
// (Valkey, and often KeyDB, Dragonfly and Garnet). It uses a pure Go client.
//
// Each call runs one command, or several commands as a pipeline (one per
// line of -body). With transaction=true the commands run inside MULTI/EXEC.
// The client is shared by every user, as an application's client is. Waiting
// for a free connection counts as part of the measured time, so give -opt
// pool at least as many connections as users when you want to measure the
// server and not the queue.
//
// Redis has no session read-only switch. This driver checks every command
// before it sends anything. A refused pipeline sends nothing. A read-only
// server user is still the real lock; allow_writes is a safety net.
//
// A timed-out call is dropped by the client library, which closes that
// connection. Refused commands are never sent, so they do not change a
// connection and need no cleanup. This driver does not hold a connection
// while it does other work: it uses Do, Pipeline or TxPipeline, and it never
// takes a connection of its own.
//
// Options (set with -opt key=value):
//
//	username      An ACL user. It can also be given in the URL. The default
//	              user needs no name. A password does not need a user.
//	password_env  The name of an environment variable that holds the
//	              password. The password is never put on the command line,
//	              and it is refused in the URL.
//	database      A number from 0 to 255 (default 0). It can also be the
//	              path of the URL, as redis://host:6379/2.
//	tls           false (default for redis://), true, or skip-verify.
//	              rediss:// means true. true checks the certificate and the
//	              host name. With -insecure, true does not check the
//	              certificate. tls=false with rediss:// is an error.
//	pool          The most connections the run opens (default 10).
//	protocol      2 (default) or 3. 2 is RESP2. 3 is RESP3.
//	allow_writes  true to let a command change data. The default is
//	              read-only. A command that is not on the read list fails
//	              before it is sent, and the error says how to allow it.
//	allow_admin   true to allow admin commands such as FLUSHALL. It needs
//	              allow_writes as well. allow_admin without allow_writes is
//	              an error.
//	transaction   true to run the commands inside MULTI/EXEC, even when
//	              there is only one. The default sends one command on its
//	              own, and two or more as a pipeline, which is not atomic.
//	args          A JSON array of values for the ? placeholders, such as
//	              '["user:42", "ann"]'. A ? is one whole argument.
//	min_rows      The last reply must have at least this many elements.
//	expect        Some kept element of the last reply must contain this text.
//	max_rows      How many elements of an array reply a scenario script gets
//	              back (default 1000). Every element is read and counted
//	              either way.
//
// The target is redis://host[:port][/database] or rediss:// for TLS, with
// port 6379 by default. The URL takes no query string. A password in the URL
// is refused.
package redis

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/protocol/sqlcommon"
	"github.com/vegaload/vegaload/internal/urlerr"
)

// Options are the -opt keys this driver accepts. The command's protocol
// table uses the same list, so the two cannot differ.
var Options = []string{
	"username", "password_env", "database", "tls", "pool", "protocol",
	"allow_writes", "allow_admin", "transaction", "args", "min_rows", "expect", "max_rows",
}

// connOptions are the keys that make up one client. The rest describe one call.
var connOptions = []string{
	"username", "password_env", "database", "tls", "pool", "protocol",
	"allow_writes", "allow_admin",
}

const (
	defaultPool    = 10
	maxPool        = 1000
	defaultMaxRows = 1000
	maxMaxRows     = 1000000
	defaultPort    = "6379"
)

// Reply is what one call read back. Values has one entry per command, in
// order. A command that returned an error has a nil entry. Value is the last
// entry. RowCount is the number of top-level elements of the last reply
// (the whole reply, including elements past max_rows), 1 for any other
// non-nil reply, and 0 for nil.
type Reply struct {
	Values   []any
	Value    any
	RowCount int
}

// Driver is a Redis protocol.Protocol.
type Driver struct {
	target  protocol.Target
	timeout time.Duration

	allowWrites bool
	allowAdmin  bool
	script      bool
	transaction bool

	password string
	host     string
	port     string

	cmds    []command
	minRows int
	expect  string
	maxRows int

	// owns is true for the driver that created the client. A Call driver
	// shares it and must not close it.
	owns   bool
	client *goredis.Client

	closeOnce sync.Once
	closeErr  error
}

// New builds a driver for one target. It does not connect: the first call does.
func New(target protocol.Target, timeout time.Duration) (*Driver, error) {
	return build(target, timeout, nil, false)
}

// NewConn is New for a scenario script. It makes only the client: the
// server, the login and the connection options. It needs no command. The
// job of each call is then set with Call. password is the password, which
// the script already holds, so password_env is not used with it.
func NewConn(target protocol.Target, timeout time.Duration, password *string) (*Driver, error) {
	return build(target, timeout, password, true)
}

// Call returns a Driver for one call of a script: the same client as d, with
// the job that opts and body describe. Closing it does nothing. Closing d
// closes the client.
func (d *Driver) Call(opts map[string]string, body []byte, timeout time.Duration) (*Driver, error) {
	nd := &Driver{
		target:      d.target,
		timeout:     timeout,
		allowWrites: d.allowWrites,
		allowAdmin:  d.allowAdmin,
		script:      d.script,
		password:    d.password,
		host:        d.host,
		port:        d.port,
		client:      d.client,
		owns:        false,
	}
	nd.target.Body = body
	nd.target.Options = opts
	if err := nd.target.RejectUnknownOptions(Options...); err != nil {
		return nil, fmt.Errorf("redis: %w", err)
	}
	if err := nd.readJob(); err != nil {
		return nil, err
	}
	return nd, nil
}

func build(target protocol.Target, timeout time.Duration, password *string, connOnly bool) (*Driver, error) {
	d := &Driver{
		target:  target,
		timeout: timeout,
		script:  connOnly,
		owns:    true,
		maxRows: defaultMaxRows,
	}

	u, err := url.Parse(target.URL)
	if err != nil {
		return nil, fmt.Errorf("redis: parsing target URL: %w", urlerr.Inner(err))
	}
	if u.Scheme != "redis" && u.Scheme != "rediss" {
		return nil, fmt.Errorf("redis: unsupported scheme %q, want redis://", u.Scheme)
	}
	if u.Hostname() == "" {
		// The URL is not echoed: it may hold a password.
		return nil, errors.New("redis: the target URL has no host")
	}
	if u.RawQuery != "" {
		return nil, queryRefused(u.RawQuery)
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			return nil, errors.New("redis: do not put a password in the URL, use -opt password_env=NAME")
		}
	}

	allowed := Options
	if connOnly {
		allowed = connOptions
	}
	if err := target.RejectUnknownOptions(allowed...); err != nil {
		return nil, fmt.Errorf("redis: %w", err)
	}

	user := target.Option("username", "")
	if u.User != nil {
		if name := u.User.Username(); name != "" {
			if user != "" && user != name {
				return nil, errors.New("redis: give the user in the URL or in username, not both")
			}
			user = name
		}
	}

	db, err := databaseOf(u.Path, target.Option("database", ""))
	if err != nil {
		return nil, err
	}

	pw := ""
	if password != nil {
		if target.Option("password_env", "") != "" {
			return nil, errors.New("redis: give a password or password_env, not both")
		}
		pw = *password
	} else if name := target.Option("password_env", ""); name != "" {
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			return nil, fmt.Errorf("redis: password_env %s is not set", name)
		}
		pw = v
	}
	d.password = pw

	tlsMode, err := tlsOf(u.Scheme, target)
	if err != nil {
		return nil, err
	}

	pool, err := target.OptionInt("pool", defaultPool)
	if err != nil {
		return nil, fmt.Errorf("redis: %w", err)
	}
	if pool < 1 || pool > maxPool {
		return nil, errors.New("redis: pool must be a number from 1 to 1000")
	}

	proto := 2
	if v := target.Option("protocol", ""); v != "" {
		switch v {
		case "2":
			proto = 2
		case "3":
			proto = 3
		default:
			return nil, errors.New("redis: protocol must be 2 or 3")
		}
	}

	d.allowWrites, err = target.OptionBool("allow_writes", false)
	if err != nil {
		return nil, fmt.Errorf("redis: %w", err)
	}
	d.allowAdmin, err = target.OptionBool("allow_admin", false)
	if err != nil {
		return nil, fmt.Errorf("redis: %w", err)
	}
	if d.allowAdmin && !d.allowWrites {
		return nil, errors.New("redis: allow_admin=true needs allow_writes=true")
	}

	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	d.host = u.Hostname()
	d.port = port

	// MaxRetries -1 is one attempt. The library turns 0 into three retries,
	// so 0 would hide a failure. ReadTimeout and WriteTimeout -1 mean no
	// deadline of their own (the library turns -1 into 0). The call context
	// is what ends the wait, which needs ContextTimeoutEnabled. DisableIdentity
	// stops the client sending CLIENT SETINFO after HELLO. Protocol 0 would
	// mean RESP3, so the default here is 2.
	opt := &goredis.Options{
		Addr:                  net.JoinHostPort(d.host, d.port),
		Username:              user,
		Password:              pw,
		DB:                    db,
		Protocol:              proto,
		PoolSize:              pool,
		MinIdleConns:          0,
		MaxActiveConns:        pool,
		MaxRetries:            -1,
		DisableIdentity:       true,
		ContextTimeoutEnabled: true,
		ReadTimeout:           -1,
		WriteTimeout:          -1,
		DialTimeout:           10 * time.Second,
		PoolTimeout:           30 * time.Second,
	}
	if tlsMode != tlsOff {
		opt.TLSConfig = &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ServerName:         d.host,
			InsecureSkipVerify: tlsMode == tlsSkip,
		}
	}
	d.client = goredis.NewClient(opt)

	if !connOnly {
		if err := d.readJob(); err != nil {
			_ = d.client.Close()
			return nil, err
		}
	}
	return d, nil
}

type tlsMode int

const (
	tlsOff tlsMode = iota
	tlsOn
	tlsSkip
)

func tlsOf(scheme string, target protocol.Target) (tlsMode, error) {
	mode := tlsOff
	if scheme == "rediss" {
		mode = tlsOn
	}
	v, ok := target.Options["tls"]
	if !ok {
		if target.InsecureSkipVerify && mode == tlsOn {
			return tlsSkip, nil
		}
		return mode, nil
	}
	switch v {
	case "false":
		if scheme == "rediss" {
			return 0, errors.New("redis: tls=false cannot be used with rediss://")
		}
		mode = tlsOff
	case "true":
		mode = tlsOn
	case "skip-verify":
		mode = tlsSkip
	default:
		return 0, errors.New("redis: tls must be false, true, or skip-verify")
	}
	if target.InsecureSkipVerify && mode == tlsOn {
		mode = tlsSkip
	}
	return mode, nil
}

func databaseOf(path, opt string) (int, error) {
	pathDB := strings.TrimPrefix(path, "/")
	if strings.Contains(pathDB, "/") {
		return 0, errors.New("redis: database must be a number from 0 to 255")
	}
	if pathDB != "" && opt != "" && pathDB != opt {
		return 0, errors.New("redis: give the database in the URL or in database, not both")
	}
	raw := pathDB
	if opt != "" {
		raw = opt
	}
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > 255 {
		return 0, errors.New("redis: database must be a number from 0 to 255")
	}
	return n, nil
}

// queryRefused names the query key and never the value. A value can be a secret.
func queryRefused(raw string) error {
	q, err := url.ParseQuery(raw)
	if err != nil || len(q) == 0 {
		return errors.New("redis: a URL query string is not supported (use -opt)")
	}
	var key string
	for k := range q {
		if key == "" || k < key {
			key = k
		}
	}
	return fmt.Errorf("redis: the URL setting %q is not supported (use -opt)", key)
}

func (d *Driver) readJob() error {
	tx, err := d.target.OptionBool("transaction", false)
	if err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	d.transaction = tx

	d.maxRows = defaultMaxRows
	if _, ok := d.target.Options["max_rows"]; ok {
		n, err := d.target.OptionInt("max_rows", defaultMaxRows)
		if err != nil {
			return fmt.Errorf("redis: %w", err)
		}
		if n < 1 || n > maxMaxRows {
			return errors.New("redis: max_rows must be a number from 1 to 1000000")
		}
		d.maxRows = n
	}
	d.minRows = 0
	if _, ok := d.target.Options["min_rows"]; ok {
		n, err := d.target.OptionInt("min_rows", 0)
		if err != nil {
			return fmt.Errorf("redis: %w", err)
		}
		if n < 0 {
			return errors.New("redis: min_rows must be a number from 0 up")
		}
		d.minRows = n
	}
	d.expect = d.target.Option("expect", "")

	rawArgs, hasArgs := d.target.Options["args"]
	cmds, err := parseCommands(string(d.target.Body), rawArgs, hasArgs)
	if err != nil {
		return err
	}
	d.cmds = cmds
	return nil
}

// Name returns the protocol name.
func (d *Driver) Name() string { return "redis" }

// Do runs one call. A command the server rejects, a timeout, and a refused
// command are a failed iteration (Result.Err set), not a reason to stop the
// run. Do returns a nil error so the engine keeps going.
func (d *Driver) Do(parent context.Context) (protocol.Result, error) {
	res, _ := d.Run(parent)
	return res, nil
}

// Run runs one call and returns the reply a scenario script reads.
func (d *Driver) Run(parent context.Context) (protocol.Result, Reply) {
	// Check every command first. If one is refused, nothing is sent.
	for _, cmd := range d.cmds {
		if err := refuse(cmd, d.allowWrites, d.allowAdmin, d.script); err != nil {
			return protocol.Result{Err: err}, Reply{}
		}
	}

	sent := bytesSent(d.cmds)
	ctx, cancel := context.WithTimeout(parent, d.timeout)
	defer cancel()

	values, lastCount, got, err := d.exec(ctx)
	rep := Reply{Values: values, RowCount: lastCount}
	if len(values) > 0 {
		rep.Value = values[len(values)-1]
	}
	if err != nil {
		return protocol.Result{BytesSent: sent, BytesReceived: got, Err: d.explain(parent, ctx, err)}, rep
	}
	rows := checkRows(rep.Value)
	if err := sqlcommon.Check("redis", d.minRows, lastCount, d.expect, rows); err != nil {
		return protocol.Result{BytesSent: sent, BytesReceived: got, Err: err}, rep
	}
	return protocol.Result{Success: true, BytesSent: sent, BytesReceived: got}, rep
}

func bytesSent(cmds []command) int64 {
	var n int64
	for _, c := range cmds {
		for _, a := range c.argv {
			n += int64(len(a))
		}
	}
	return n
}

func checkRows(value any) [][]any {
	switch x := value.(type) {
	case nil:
		return nil
	case []any:
		rows := make([][]any, len(x))
		for i, v := range x {
			rows[i] = []any{v}
		}
		return rows
	default:
		return [][]any{{value}}
	}
}

func (d *Driver) exec(ctx context.Context) ([]any, int, int64, error) {
	if d.transaction || len(d.cmds) > 1 {
		return d.execPipe(ctx)
	}
	val, err := d.client.Do(ctx, argsOf(d.cmds[0])...).Result()
	if err != nil && errors.Is(err, goredis.Nil) {
		return []any{nil}, 0, 0, nil
	}
	if err != nil {
		return []any{nil}, 0, 0, err
	}
	norm, n, bytes := normalize(val, d.maxRows)
	return []any{norm}, n, bytes, nil
}

func (d *Driver) execPipe(ctx context.Context) ([]any, int, int64, error) {
	var pipe goredis.Pipeliner
	if d.transaction {
		pipe = d.client.TxPipeline()
	} else {
		pipe = d.client.Pipeline()
	}
	queued := make([]*goredis.Cmd, len(d.cmds))
	for i, cmd := range d.cmds {
		queued[i] = pipe.Do(ctx, argsOf(cmd)...)
	}
	_, _ = pipe.Exec(ctx)

	values := make([]any, len(d.cmds))
	var got int64
	var lastCount int
	var firstServer error
	var fatal error
	for i, cmd := range queued {
		val, err := cmd.Result()
		if err != nil && errors.Is(err, goredis.Nil) {
			values[i] = nil
			if i == len(d.cmds)-1 {
				lastCount = 0
			}
			continue
		}
		if err != nil {
			values[i] = nil
			if msg, ok := serverMessage(err); ok {
				if firstServer == nil {
					firstServer = fmt.Errorf("redis: command %d (%s): %s", i+1, strings.ToUpper(d.cmds[i].name), msg)
				}
				if i == len(d.cmds)-1 {
					lastCount = 0
				}
				continue
			}
			if fatal == nil {
				fatal = err
			}
			continue
		}
		norm, n, bytes := normalize(val, d.maxRows)
		got += bytes
		values[i] = norm
		if i == len(d.cmds)-1 {
			lastCount = n
		}
	}
	if fatal != nil {
		return values, lastCount, got, fatal
	}
	if firstServer != nil {
		return values, lastCount, got, firstServer
	}
	return values, lastCount, got, nil
}

func argsOf(cmd command) []any {
	out := make([]any, len(cmd.argv))
	for i, a := range cmd.argv {
		out[i] = a
	}
	return out
}

func (d *Driver) explain(parent, ctx context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	// The socket is closed on the call deadline. That error can arrive a
	// moment before the context itself reports DeadlineExceeded, so a network
	// timeout counts once the deadline is due.
	if callTimedOut(ctx, err) {
		return fmt.Errorf("redis: timed out after %s", d.timeout)
	}
	if msg, ok := serverMessage(err); ok {
		return errors.New(d.redact("redis: " + msg))
	}
	if isDial(err) {
		return fmt.Errorf("redis: could not connect to %s: %s", net.JoinHostPort(d.host, d.port), d.redact(err.Error()))
	}
	if strings.HasPrefix(err.Error(), "redis:") {
		return errors.New(d.redact(err.Error()))
	}
	return fmt.Errorf("redis: %s", d.redact(err.Error()))
}

func callTimedOut(ctx context.Context, err error) bool {
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	if !isNetTimeout(err) {
		return false
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return false
	}
	// The net poller and the context timer do not fire on the same tick.
	return time.Until(deadline) <= time.Millisecond
}

func isNetTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func serverMessage(err error) (string, bool) {
	var re goredis.Error
	if errors.As(err, &re) {
		return re.Error(), true
	}
	return "", false
}

func isDial(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") || strings.Contains(s, "no such host") || strings.HasPrefix(s, "dial ")
}

func (d *Driver) redact(s string) string {
	if d.password != "" {
		s = strings.ReplaceAll(s, d.password, "redacted")
	}
	return s
}

// Close closes the client. A driver returned by Call shares the client, so
// Close on it does nothing.
func (d *Driver) Close() error {
	if !d.owns {
		return nil
	}
	d.closeOnce.Do(func() {
		if d.client != nil {
			d.closeErr = d.client.Close()
		}
	})
	return d.closeErr
}

// normalize turns a go-redis reply into a value a script can hold.
// trim is how many top-level array elements to keep. Nested arrays are kept
// whole. The returned count is the full top-level length, before the trim.
// bytes is an estimate: the client library does not report how many bytes
// were read, so this sums the text it decoded and counts 8 bytes per integer.
// The full reply is counted, including elements that trim drops.
func normalize(v any, trim int) (any, int, int64) {
	switch x := v.(type) {
	case nil:
		return nil, 0, 0
	case string:
		s := strings.ToValidUTF8(x, "\uFFFD")
		return s, 1, int64(len(s))
	case []byte:
		s := strings.ToValidUTF8(string(x), "\uFFFD")
		return s, 1, int64(len(s))
	case int64:
		return x, 1, 8
	case int:
		return int64(x), 1, 8
	case float64:
		return x, 1, 0
	case bool:
		return x, 1, 0
	case *big.Int:
		if x == nil {
			return nil, 0, 0
		}
		s := x.String()
		return s, 1, int64(len(s))
	case []any:
		return normSlice(x, trim)
	case map[any]any:
		return normMap(x, trim)
	case map[string]any:
		m := make(map[any]any, len(x))
		for k, v := range x {
			m[k] = v
		}
		return normMap(m, trim)
	default:
		s := fmt.Sprint(x)
		return s, 1, int64(len(s))
	}
}

func normSlice(xs []any, trim int) (any, int, int64) {
	full := len(xs)
	keptN := full
	if trim >= 0 && trim < keptN {
		keptN = trim
	}
	kept := make([]any, 0, keptN)
	var n int64
	for i, v := range xs {
		// Nested arrays are not trimmed. Only the top-level reply is.
		child, _, b := normalize(v, -1)
		n += b
		if trim < 0 || i < trim {
			kept = append(kept, child)
		}
	}
	return kept, full, n
}

func normMap(m map[any]any, trim int) (any, int, int64) {
	obj := make(map[string]any, len(m))
	var n int64
	for k, v := range m {
		key := fmt.Sprint(k)
		child, _, b := normalize(v, -1)
		obj[key] = child
		n += int64(len(key)) + b
	}
	return obj, 1, n
}
