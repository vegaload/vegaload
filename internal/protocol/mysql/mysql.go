// Package mysql implements the "mysql" protocol.Protocol driver, for load
// testing a MySQL or MariaDB server (or a database that speaks the same
// wire protocol, such as TiDB). It uses a pure Go client.
//
// Each call to Do runs one SQL text, read from -body, and reads every row
// it returns. The connections are kept in a pool that all users share, as an
// application's connection pool is. Waiting for a free connection counts as
// part of the measured time, so give -opt pool at least as many connections
// as users when you want to measure the server and not the queue.
//
// Options (set with -opt key=value):
//
//	username      The database user. It can also be given in the URL.
//	password_env  The name of an environment variable that holds the
//	              password. It needs a user. The password is never put on
//	              the command line, and it is refused in the URL.
//	database      The database. It can also be given as the path of the URL.
//	tls           preferred (default), false, true, or skip-verify.
//	              true checks the certificate and the host name. With
//	              -insecure, true does not check the certificate.
//	              preferred uses TLS when the server offers it, and plain
//	              text when it does not.
//	pool          The most connections the run opens (default 10).
//	allow_writes  true to let the SQL change data. The default is read-only:
//	              every new connection runs SET SESSION TRANSACTION READ ONLY,
//	              so a load test cannot change data by mistake. A statement
//	              that writes then fails, and the error says how to allow it.
//	              This is a safety net, not a lock: the SQL can still ask for
//	              a read-write transaction itself (START TRANSACTION READ WRITE).
//	              "set session transaction read write; insert ..." in one text
//	              works. The next call is blocked, because SET discards the
//	              connection.
//	query_mode    simple (default) or prepared. simple sends the SQL as one
//	              text, so it can hold several statements such as
//	              "begin; update ... where id = ?; select ...; commit",
//	              with or without args. VegaLoad puts the args into the text
//	              as quoted values. prepared prepares the statement once and
//	              reuses it, as most applications do. It runs one statement
//	              and does not keep a connection for the call, so it cannot
//	              run set, use, lock, xa, unlock, begin, or start.
//	args          A JSON array of values for the ? placeholders, such as
//	              '[42, "abc"]'.
//	min_rows      The result must have at least this many rows.
//	expect        Some value in the result must contain this text. Only the
//	              first max_rows rows are checked.
//	max_rows      How many rows a scenario script gets back (default 1000).
//	              Every row is read and counted either way.
//
// The target is mysql://host[:port][/database] (or mariadb://), with port
// 3306 by default. The URL may end with ?tls=.... Any other setting after
// the "?" is refused.
//
// A text of several statements reports rowsAffected as 0. The client library
// skips a result that has no columns, so the affected-row count of an update
// that shares the text with a select is not available. A single insert or
// update does report rowsAffected and lastInsertId.
package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/protocol/sqlcommon"
	"github.com/vegaload/vegaload/internal/urlerr"
)

// Options are the -opt keys this driver accepts. The command's protocol
// table uses the same list, so the two cannot differ.
var Options = []string{
	"username", "password_env", "database", "tls",
	"pool", "allow_writes", "query_mode", "args", "min_rows", "expect", "max_rows",
}

// connOptions are the keys that make up the connection. The rest describe
// the job of one call.
var connOptions = []string{
	"username", "password_env", "database", "tls",
	"pool", "allow_writes", "query_mode",
}

const (
	defaultPool    = 10
	maxPool        = 1000
	defaultMaxRows = 1000
	maxMaxRows     = 1000000

	readOnlySQL = "SET SESSION TRANSACTION READ ONLY"
	sqlModeSQL  = "SELECT @@SESSION.sql_mode"
)

// Driver is a MySQL protocol.Protocol.
type Driver struct {
	target  protocol.Target
	timeout time.Duration

	simple bool // query_mode=simple

	// allowWrites is the allow_writes option. script is true for a
	// connection made for a scenario script (NewConn). Both only decide
	// the words of an error hint.
	allowWrites bool
	script      bool

	sql     string
	args    []any
	hasArgs bool
	minRows int
	expect  string
	maxRows int

	// password is kept only so an error can be checked for it. It is never
	// written into an error, a log, or a DSN string.
	password string
	host     string
	port     string

	db     *sql.DB
	ownsDB bool
	modes  *atomic.Pointer[sqlMode]
	owner  *Driver

	stmtMu sync.Mutex
	stmts  map[string]*sql.Stmt

	scanMu  sync.Mutex
	scanned map[sqlMode]scanCache
}

type scanCache struct {
	out  string
	info scanInfo
}

// New returns a ready-to-use Driver for target. target.URL is
// mysql://host[:port][/database]. The SQL is target.Body.
//
// New returns an error only for a configuration problem, never for the
// server being unreachable, which Do reports per call. Creating the pool
// does not connect.
func New(target protocol.Target, timeout time.Duration) (*Driver, error) {
	return build(target, timeout, nil, false)
}

// NewConn is New for a scenario script. It makes only the connection pool:
// the server, the login and the connection options. It needs no SQL. The
// job of each call is then set with Call. password is the password, which
// the script already holds, so password_env is not used with it.
func NewConn(target protocol.Target, timeout time.Duration, password *string) (*Driver, error) {
	return build(target, timeout, password, true)
}

// Call returns a Driver for one call of a script: the same pool as d, with
// the job that opts and body describe. opts must hold the same connection
// options d was made with. The result shares d's pool, so closing it is d's
// job, not the caller's.
func (d *Driver) Call(opts map[string]string, body []byte, timeout time.Duration) (*Driver, error) {
	owner := d.owner
	if owner == nil {
		owner = d
	}
	nd := &Driver{
		target:      d.target,
		timeout:     timeout,
		simple:      d.simple,
		allowWrites: d.allowWrites,
		script:      d.script,
		password:    d.password,
		host:        d.host,
		port:        d.port,
		db:          d.db,
		modes:       d.modes,
		owner:       owner,
	}
	nd.target.Body = body
	nd.target.Options = opts
	if err := nd.target.RejectUnknownOptions(Options...); err != nil {
		return nil, fmt.Errorf("mysql: %w", err)
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
		modes:   new(atomic.Pointer[sqlMode]),
	}

	u, err := url.Parse(target.URL)
	if err != nil {
		return nil, fmt.Errorf("mysql: parsing target URL: %w", urlerr.Inner(err))
	}
	if u.Scheme != "mysql" && u.Scheme != "mariadb" {
		return nil, fmt.Errorf("mysql: unsupported scheme %q, want mysql://", u.Scheme)
	}
	if u.Hostname() == "" {
		// The URL is not echoed: it may hold a password in the query string.
		return nil, errors.New("mysql: the target URL has no host")
	}
	urlTLS := ""
	for k, vs := range u.Query() {
		switch strings.ToLower(k) {
		case "tls":
			if len(vs) != 1 {
				return nil, errors.New("mysql: the URL gives tls more than once")
			}
			urlTLS = vs[0]
		case "password", "passwd", "pwd", "passfile":
			return nil, fmt.Errorf("mysql: do not put %s in the URL, use -opt password_env=NAME", k)
		default:
			return nil, fmt.Errorf("mysql: the URL setting %q is not supported (use -opt for tls)", k)
		}
	}
	if o, has := target.Options["tls"]; has && urlTLS != "" && o != urlTLS {
		return nil, errors.New("mysql: give tls in the URL or in -opt, not both")
	}

	port := u.Port()
	if port == "" {
		port = "3306"
	}
	d.host = u.Hostname()
	d.port = port

	allowed := Options
	if connOnly {
		allowed = connOptions
	}
	if err := target.RejectUnknownOptions(allowed...); err != nil {
		return nil, fmt.Errorf("mysql: %w", err)
	}

	user := target.Option("username", "")
	if u.User != nil {
		if _, has := u.User.Password(); has {
			return nil, errors.New("mysql: do not put a password in the URL, use -opt password_env=NAME")
		}
		if name := u.User.Username(); name != "" {
			if user != "" && user != name {
				return nil, errors.New("mysql: give the user in the URL or in username, not both")
			}
			user = name
		}
	}

	dbName := strings.TrimPrefix(u.Path, "/")
	if v := target.Option("database", ""); v != "" {
		if dbName != "" && dbName != v {
			return nil, errors.New("mysql: give the database in the URL or in database, not both")
		}
		dbName = v
	}

	pw := ""
	if password != nil {
		if target.Option("password_env", "") != "" {
			return nil, errors.New("mysql: give a password or password_env, not both")
		}
		if user == "" {
			return nil, errors.New("mysql: a password needs a user (username): a password is only sent together with a user name")
		}
		pw = *password
	} else if env := target.Option("password_env", ""); env != "" {
		if user == "" {
			return nil, errors.New("mysql: password_env needs a user (username): a password is only sent together with a user name")
		}
		v, ok := os.LookupEnv(env)
		if !ok || v == "" {
			return nil, fmt.Errorf("mysql: password_env=%s, but that environment variable is not set", env)
		}
		pw = v
	}
	d.password = pw

	tlsName := target.Option("tls", urlTLS)
	if tlsName == "" {
		tlsName = "preferred"
	}
	switch tlsName {
	case "preferred", "false", "true", "skip-verify":
	default:
		return nil, fmt.Errorf("mysql: tls=%q, want preferred, false, true, or skip-verify", tlsName)
	}
	if target.InsecureSkipVerify && tlsName == "true" {
		tlsName = "skip-verify"
	}

	switch qm := target.Option("query_mode", "simple"); qm {
	case "simple":
		d.simple = true
	case "prepared":
	default:
		return nil, fmt.Errorf("mysql: query_mode=%q, want simple or prepared", qm)
	}

	poolSize, err := target.OptionInt("pool", defaultPool)
	if err != nil {
		return nil, fmt.Errorf("mysql: %w", err)
	}
	if poolSize < 1 || poolSize > maxPool {
		return nil, fmt.Errorf("mysql: pool=%d, want 1 to %d", poolSize, maxPool)
	}
	allowWrites, err := target.OptionBool("allow_writes", false)
	if err != nil {
		return nil, fmt.Errorf("mysql: %w", err)
	}
	d.allowWrites = allowWrites

	if !connOnly {
		if err := d.readJob(); err != nil {
			return nil, err
		}
	}

	cfg := gomysql.NewConfig()
	cfg.User = user
	cfg.Passwd = pw
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(d.host, d.port)
	cfg.DBName = dbName
	cfg.Collation = "utf8mb4_general_ci"
	cfg.MultiStatements = d.simple
	cfg.ParseTime = false
	cfg.TLSConfig = tlsName
	// AllowAllFiles stays off, so LOAD DATA LOCAL INFILE cannot read a file.
	// AllowNativePasswords stays at the library default.
	// v1.9 asks the server for its public key when the login is not inside
	// TLS, which is what MySQL 8 needs for caching_sha2_password.

	base, err := gomysql.NewConnector(cfg)
	if err != nil {
		return nil, errors.New("mysql: the connection settings are not valid (check the user, database and tls)")
	}
	db := sql.OpenDB(&sessionConnector{base: base, allowWrites: allowWrites, modes: d.modes})
	db.SetMaxOpenConns(poolSize)
	db.SetMaxIdleConns(poolSize)
	d.db = db
	d.ownsDB = true
	return d, nil
}

// sessionConnector runs the read-only setting and reads sql_mode on every
// new connection, before the pool hands it out.
type sessionConnector struct {
	base        driver.Connector
	allowWrites bool
	modes       *atomic.Pointer[sqlMode]
}

func (c *sessionConnector) Driver() driver.Driver { return c.base.Driver() }

func (c *sessionConnector) Connect(ctx context.Context) (driver.Conn, error) {
	raw, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	execer, ok1 := raw.(driver.ExecerContext)
	queryer, ok2 := raw.(driver.QueryerContext)
	if !ok1 || !ok2 {
		_ = raw.Close()
		return nil, errors.New("mysql: the driver connection cannot run the session setup")
	}
	if !c.allowWrites {
		if _, err := execer.ExecContext(ctx, readOnlySQL, nil); err != nil {
			_ = raw.Close()
			return nil, err
		}
	}
	rows, err := queryer.QueryContext(ctx, sqlModeSQL, nil)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	mode, err := readSQLMode(rows)
	_ = rows.Close()
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	c.modes.CompareAndSwap(nil, &mode)
	return raw, nil
}

func readSQLMode(rows driver.Rows) (sqlMode, error) {
	dest := make([]driver.Value, len(rows.Columns()))
	if err := rows.Next(dest); err != nil {
		return sqlMode{}, err
	}
	text := ""
	switch v := dest[0].(type) {
	case nil:
	case []byte:
		text = string(v)
	case string:
		text = v
	default:
		text = fmt.Sprint(v)
	}
	var mode sqlMode
	for _, part := range strings.Split(text, ",") {
		switch strings.ToUpper(strings.TrimSpace(part)) {
		case "NO_BACKSLASH_ESCAPES":
			mode.noBackslash = true
		case "ANSI_QUOTES":
			mode.ansiQuotes = true
		}
	}
	return mode, nil
}

// readJob reads the options that describe one call: the SQL, its arguments
// and the checks on the result. The server's sql_mode is not known yet, so
// the scan uses the default mode and only checks that the args line up.
func (d *Driver) readJob() error {
	t := d.target
	d.sql = string(bytes.TrimSpace(t.Body))
	if d.sql == "" {
		return sqlcommon.ErrNoSQL("mysql")
	}
	var err error
	if d.minRows, err = t.OptionInt("min_rows", 0); err != nil {
		return fmt.Errorf("mysql: %w", err)
	}
	if d.minRows < 0 {
		return fmt.Errorf("mysql: min_rows=%d, want 0 or more", d.minRows)
	}
	if d.maxRows, err = t.OptionInt("max_rows", defaultMaxRows); err != nil {
		return fmt.Errorf("mysql: %w", err)
	}
	if d.maxRows < 1 || d.maxRows > maxMaxRows {
		return fmt.Errorf("mysql: max_rows=%d, want 1 to %d", d.maxRows, maxMaxRows)
	}
	d.expect = t.Option("expect", "")
	d.args = nil
	d.hasArgs = false
	if raw, ok := t.Options["args"]; ok {
		d.hasArgs = true
		if d.args, err = sqlcommon.ParseArgs("mysql", raw); err != nil {
			return err
		}
	}
	var args []any
	if d.hasArgs {
		args = d.args
	}
	_, info, err := scan(d.sql, sqlMode{}, args)
	if err != nil {
		return err
	}
	if !d.simple {
		if info.statements != 1 {
			return errors.New("mysql: query_mode=prepared runs one statement")
		}
		kw := ""
		if len(info.keywords) > 0 {
			kw = info.keywords[0]
		}
		if preparedRefused(kw) {
			return fmt.Errorf("mysql: query_mode=prepared cannot run %q; use query_mode=simple for set, use, lock, xa, unlock, begin, and start", kw)
		}
	}
	return nil
}

// Name implements protocol.Protocol.
func (d *Driver) Name() string { return "mysql" }

// Close implements protocol.Protocol. It closes the pool and any prepared
// statements. A Driver made with Call shares the pool, so Close on it does
// nothing.
func (d *Driver) Close() error {
	if !d.ownsDB {
		return nil
	}
	d.stmtMu.Lock()
	for _, s := range d.stmts {
		_ = s.Close()
	}
	d.stmts = nil
	d.stmtMu.Unlock()
	if d.db != nil {
		return d.db.Close()
	}
	return nil
}

// Reply is what a call read, besides the result.
type Reply struct {
	// Columns are the column names of the result.
	Columns []string
	// Rows are the first max_rows rows, each as plain values in column order.
	Rows [][]any
	// RowCount is how many rows the result had, including rows past max_rows.
	RowCount int
	// RowsAffected is how many rows a single insert, update or delete
	// changed. It is 0 when the text holds several statements: that count
	// is not available then.
	RowsAffected int64
	// LastInsertID is the id a single insert produced. It is 0 when the
	// text holds several statements.
	LastInsertID int64
}

// Do implements protocol.Protocol.
func (d *Driver) Do(parent context.Context) (protocol.Result, error) {
	res, _ := d.Run(parent)
	return res, nil
}

// Run is Do, and it also returns the rows of the call. A load test does not
// need them. A scenario script does.
func (d *Driver) Run(parent context.Context) (protocol.Result, Reply) {
	ctx, cancel := context.WithTimeout(parent, d.timeout)
	defer cancel()

	var rep Reply
	sent := int64(len(d.sql))
	var (
		got     int64
		err     error
		connect bool
	)
	if d.simple {
		got, connect, err = d.runSimple(ctx, &rep)
	} else {
		// A prepared statement takes a connection from the pool when it
		// runs. Holding one here would make it wait for a connection this
		// call already has, and finish would clean that other connection.
		got, connect, err = d.runPrepared(ctx, &rep)
	}
	if err != nil {
		return protocol.Result{BytesSent: sent, BytesReceived: got, Err: d.explain(parent, ctx, err, connect)}, rep
	}
	if err = sqlcommon.Check("mysql", d.minRows, rep.RowCount, d.expect, rep.Rows); err != nil {
		return protocol.Result{BytesSent: sent, BytesReceived: got, Err: d.explain(parent, ctx, err, false)}, rep
	}
	return protocol.Result{Success: true, BytesSent: sent, BytesReceived: got}, rep
}

func (d *Driver) cachedScan(mode sqlMode) (string, scanInfo, error) {
	d.scanMu.Lock()
	defer d.scanMu.Unlock()
	if c, ok := d.scanned[mode]; ok {
		return c.out, c.info, nil
	}
	var args []any
	if d.hasArgs {
		args = d.args
	}
	out, info, err := scan(d.sql, mode, args)
	if err != nil {
		return "", scanInfo{}, err
	}
	if d.scanned == nil {
		d.scanned = map[sqlMode]scanCache{}
	}
	d.scanned[mode] = scanCache{out: out, info: info}
	return out, info, nil
}

// runSimple holds one pool connection for the whole call, then puts the
// session back or discards the connection.
func (d *Driver) runSimple(ctx context.Context, rep *Reply) (int64, bool, error) {
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return 0, true, err
	}
	defer conn.Close()

	text, info, err := d.cachedScan(d.sessionMode())
	if err != nil {
		return 0, false, err
	}
	var got int64
	if info.statements == 1 && !returnsRows(info.keywords[0]) {
		got, err = execOn(ctx, conn, text, rep)
	} else {
		got, err = queryOn(ctx, conn, text, rep, d.maxRows)
	}
	d.finish(ctx, conn, info, err != nil)
	return got, false, err
}

// runPrepared uses the shared prepared statement. The statement borrows a
// pool connection for the query and returns it, so this call must not hold
// one of its own. Session statements are refused in readJob, and finish is
// not used here.
func (d *Driver) runPrepared(ctx context.Context, rep *Reply) (int64, bool, error) {
	stmt, err := d.prepare(ctx)
	if err != nil {
		// The first connection learns sql_mode. No mode means it never connected.
		return 0, d.modes.Load() == nil, err
	}
	_, info, err := d.cachedScan(d.sessionMode())
	if err != nil {
		return 0, false, err
	}
	if info.statements != 1 {
		return 0, false, errors.New("mysql: query_mode=prepared runs one statement")
	}
	if !returnsRows(info.keywords[0]) {
		n, err := execPrepared(ctx, stmt, d.args, rep)
		return n, false, err
	}
	n, err := queryPrepared(ctx, stmt, d.args, rep, d.maxRows)
	return n, false, err
}

func (d *Driver) sessionMode() sqlMode {
	if p := d.modes.Load(); p != nil {
		return *p
	}
	return sqlMode{}
}

func execOn(ctx context.Context, conn *sql.Conn, text string, rep *Reply) (int64, error) {
	res, err := conn.ExecContext(ctx, text)
	if err != nil {
		return 0, err
	}
	return readResult(res, rep)
}

func execPrepared(ctx context.Context, stmt *sql.Stmt, args []any, rep *Reply) (int64, error) {
	res, err := stmt.ExecContext(ctx, args...)
	if err != nil {
		return 0, err
	}
	return readResult(res, rep)
}

func readResult(res sql.Result, rep *Reply) (int64, error) {
	if n, e := res.RowsAffected(); e == nil {
		rep.RowsAffected = n
	}
	if n, e := res.LastInsertId(); e == nil {
		rep.LastInsertID = n
	}
	return 0, nil
}

func queryOn(ctx context.Context, conn *sql.Conn, text string, rep *Reply, maxRows int) (int64, error) {
	rows, err := conn.QueryContext(ctx, text)
	if err != nil {
		return 0, err
	}
	return readRows(rows, rep, maxRows)
}

func queryPrepared(ctx context.Context, stmt *sql.Stmt, args []any, rep *Reply, maxRows int) (int64, error) {
	rows, err := stmt.QueryContext(ctx, args...)
	if err != nil {
		return 0, err
	}
	return readRows(rows, rep, maxRows)
}

func readRows(rows *sql.Rows, rep *Reply, maxRows int) (int64, error) {
	defer rows.Close()
	got, err := readSet(rows, rep, maxRows)
	if err != nil {
		return got, err
	}
	for rows.NextResultSet() {
		n, err := readSet(rows, rep, maxRows)
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, rows.Err()
}

// readSet reads the current result. A set with no columns leaves rep as it
// was, so an earlier select is kept. got is the sum of the cell lengths.
func readSet(rows *sql.Rows, rep *Reply, maxRows int) (int64, error) {
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	if len(cols) == 0 {
		return 0, nil
	}
	types, _ := rows.ColumnTypes()
	rep.Columns = cols
	rep.Rows = nil
	rep.RowCount = 0
	var got int64
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return got, err
		}
		rep.RowCount++
		for _, v := range vals {
			got += cellLen(v)
		}
		if len(rep.Rows) < maxRows {
			row := make([]any, len(vals))
			for i, v := range vals {
				name := ""
				if i < len(types) && types[i] != nil {
					name = types[i].DatabaseTypeName()
				}
				row[i] = normalizeCell(v, name)
			}
			rep.Rows = append(rep.Rows, row)
		}
	}
	return got, rows.Err()
}

func (d *Driver) prepare(ctx context.Context) (*sql.Stmt, error) {
	owner := d.owner
	if owner == nil {
		owner = d
	}
	owner.stmtMu.Lock()
	defer owner.stmtMu.Unlock()
	if s, ok := owner.stmts[d.sql]; ok {
		return s, nil
	}
	s, err := owner.db.PrepareContext(ctx, d.sql)
	if err != nil {
		return nil, err
	}
	if owner.stmts == nil {
		owner.stmts = map[string]*sql.Stmt{}
	}
	owner.stmts[d.sql] = s
	return s, nil
}

// finish drops a connection that failed or that changed session state, and
// rolls back a transaction the text started, so the next call does not
// inherit it.
func (d *Driver) finish(ctx context.Context, conn *sql.Conn, info scanInfo, failed bool) {
	if failed || ctx.Err() != nil {
		discard(conn)
		return
	}
	for _, kw := range info.keywords {
		if changesSession(kw) {
			discard(conn)
			return
		}
	}
	for _, kw := range info.keywords {
		if startsTx(kw) {
			if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
				discard(conn)
			}
			return
		}
	}
}

func discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}

func (d *Driver) explain(parent, ctx context.Context, err error, connect bool) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("mysql: timed out after %s", d.timeout)
	}
	var me *gomysql.MySQLError
	if errors.As(err, &me) {
		state := string(me.SQLState[:])
		if me.SQLState == [5]byte{} {
			state = "HY000"
		}
		msg := fmt.Sprintf("mysql: %s (error %d, SQLSTATE %s)", me.Message, me.Number, state)
		if !d.allowWrites && (me.Number == 1792 || state == "25006") {
			way := "use -opt allow_writes=true"
			if d.script {
				way = "pass allow_writes: true in the connection options"
			}
			msg += ". VegaLoad is read-only by default: " + way + " to allow writes"
		}
		return errors.New(d.redact(msg))
	}
	if strings.HasPrefix(err.Error(), "mysql:") {
		return errors.New(d.redact(err.Error()))
	}
	if connect {
		return fmt.Errorf("mysql: could not connect to %s: %s", net.JoinHostPort(d.host, d.port), d.redact(err.Error()))
	}
	return fmt.Errorf("mysql: %s", d.redact(err.Error()))
}

func (d *Driver) redact(s string) string {
	if d.password != "" {
		s = strings.ReplaceAll(s, d.password, "redacted")
	}
	return s
}

func cellLen(v any) int64 {
	switch x := v.(type) {
	case nil:
		return 0
	case []byte:
		return int64(len(x))
	case string:
		return int64(len(x))
	default:
		return int64(len(fmt.Sprint(x)))
	}
}

// normalizeCell turns one cell into the value a script sees. The text
// protocol already turns integers and floats into numbers; other types
// arrive as bytes. Prepared statements use the same rules, so both modes
// agree.
func normalizeCell(v any, typeName string) any {
	if v == nil {
		return nil
	}
	switch x := v.(type) {
	case int64:
		return x
	case uint64:
		if x > math.MaxInt64 {
			return strconv.FormatUint(x, 10)
		}
		return int64(x)
	case float64:
		return x
	case float32:
		return float64(x)
	case bool:
		return x
	case []byte:
		return normalizeText(string(x), typeName)
	case string:
		return normalizeText(x, typeName)
	default:
		return fmt.Sprint(v)
	}
}

func normalizeText(s, typeName string) any {
	name := strings.ToUpper(typeName)
	unsigned := strings.Contains(name, "UNSIGNED")
	name = strings.TrimPrefix(name, "UNSIGNED ")
	switch name {
	case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER", "BIGINT", "YEAR":
		if unsigned {
			u, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return s
			}
			if u > math.MaxInt64 {
				return s
			}
			return int64(u)
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return s
		}
		return n
	case "FLOAT", "DOUBLE":
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return s
		}
		return f
	case "DECIMAL", "NEWDECIMAL":
		return s
	case "JSON":
		var parsed any
		if json.Unmarshal([]byte(s), &parsed) == nil {
			return parsed
		}
		return s
	default:
		return s
	}
}
