package netapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/protocol/ftp"
	"github.com/vegaload/vegaload/internal/protocol/grpc"
	"github.com/vegaload/vegaload/internal/protocol/kafka"
	"github.com/vegaload/vegaload/internal/protocol/mqtt"
	"github.com/vegaload/vegaload/internal/protocol/mysql"
	"github.com/vegaload/vegaload/internal/protocol/postgres"
	"github.com/vegaload/vegaload/internal/protocol/rabbitmq"
	"github.com/vegaload/vegaload/internal/protocol/redis"
	"github.com/vegaload/vegaload/internal/protocol/sftp"
	"github.com/vegaload/vegaload/internal/protocol/socket"
	"github.com/vegaload/vegaload/internal/protocol/xfercommon"
	"github.com/vegaload/vegaload/internal/safety"
	"github.com/vegaload/vegaload/internal/tlsconf"
	"github.com/vegaload/vegaload/internal/urlerr"
)

// defaultProtoTimeout is used when the caller gives no timeout at all.
const defaultProtoTimeout = 10 * time.Second

// ProtoCall is one call a script makes to a tcp, udp, mqtt, kafka, grpc, postgres, mysql, redis, rabbitmq or ftp target.
type ProtoCall struct {
	// URL is the target. tcp and udp accept a bare host:port too.
	URL string
	// Body is the data to send.
	Body []byte
	// Options are the driver's own options, the same keys as -opt.
	Options map[string]string
	// Insecure turns off TLS certificate checks.
	Insecure bool
	// Timeout bounds the whole call. Zero means the client's default.
	Timeout time.Duration
	// Password is the MQTT, Kafka, PostgreSQL, MySQL, Redis, RabbitMQ, FTP or SFTP password. nil means none. A script passes it
	// here, from `env`, so it never has to be put in Options.
	Password *string
	// KeyPassphrase is the passphrase of an SFTP private key, from key_passphrase. nil means none.
	KeyPassphrase *string
	// Headers are the gRPC metadata of a grpc.call.
	Headers map[string]string
}

// ProtoFunctions lists the calls a script can make, by namespace: tcp.send,
// mqtt.publish and so on. Each scripting language builds its globals from
// this list, so they cannot differ. It stays out of line so the compiler
// does not copy this body into every caller. That copy is what makes the
// binary larger.
//
//go:noinline
func ProtoFunctions() map[string][]string {
	return map[string][]string{
		"tcp":      {"send"},
		"udp":      {"send"},
		"mqtt":     {"publish", "subscribe", "roundtrip"},
		"kafka":    {"produce", "consume", "roundtrip", "admin"},
		"grpc":     {"call"},
		"postgres": {"query"},
		"mysql":    {"query"},
		"redis":    {"command"},
		"rabbitmq": {"publish", "consume", "roundtrip", "admin"},
		"ftp":      {"connect", "download", "upload", "list", "stat", "delete", "roundtrip"},
		"sftp":     {"connect", "download", "upload", "list", "stat", "delete", "roundtrip"},
	}
}

// Call makes the call that name says, such as "tcp.send" or "kafka.admin".
// A finished call is recorded once here. The typed methods do not record,
// so a test that calls them directly is not counted.
func (c *ProtoClient) Call(ctx context.Context, name string, call ProtoCall) (*ProtoReply, error) {
	start := time.Now()
	reply, err := c.dispatch(ctx, name, call)
	if reply == nil && safety.IsHostRefusal(err) {
		reply = &ProtoReply{Refused: true, Error: err.Error()}
	}
	c.observeCall(ctx, name, time.Since(start), reply, err)
	return reply, err
}

func (c *ProtoClient) dispatch(ctx context.Context, name string, call ProtoCall) (*ProtoReply, error) {
	ns, fn, _ := strings.Cut(name, ".")
	switch ns {
	case "tcp":
		return c.TCP(ctx, call)
	case "udp":
		return c.UDP(ctx, call)
	case "mqtt":
		return c.MQTT(ctx, fn, call)
	case "kafka":
		return c.Kafka(ctx, fn, call)
	case "grpc":
		return c.GRPC(ctx, call)
	case "postgres":
		return c.Postgres(ctx, call)
	case "mysql":
		return c.MySQL(ctx, call)
	case "redis":
		return c.Redis(ctx, call)
	case "rabbitmq":
		return c.RabbitMQ(ctx, fn, call)
	case "ftp":
		return c.FTP(ctx, fn, call)
	case "sftp":
		return c.SFTP(ctx, fn, call)
	}
	return nil, fmt.Errorf("unknown call %q", name)
}

// ProtoCallFromArgs builds a call from a target and the options object a
// script passed. body, insecure, timeout and password are the script's own
// keys. For kafka, value is the same as body. Every other key becomes a
// driver option, and the driver rejects the ones it does not know. A
// trailing underscore is dropped from a key, so a language where a word such
// as "from" is reserved can write from_.
func ProtoCallFromArgs(name, rawURL string, args map[string]any) (ProtoCall, error) {
	pc := ProtoCall{URL: rawURL}
	valueIsBody := strings.HasPrefix(name, "kafka.")
	isGRPC := strings.HasPrefix(name, "grpc.")
	isSQL := strings.HasPrefix(name, "postgres.") || strings.HasPrefix(name, "mysql.") || strings.HasPrefix(name, "redis.")
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		val := args[k]
		if val == nil {
			continue
		}
		key := strings.TrimSuffix(k, "_")
		if key == "args" && isSQL {
			// The values for $1, $2 or ?: a list, or JSON text of one.
			s, err := sqlArgs(val)
			if err != nil {
				return pc, err
			}
			if pc.Options == nil {
				pc.Options = map[string]string{}
			}
			pc.Options["args"] = s
			continue
		}
		switch key {
		case "body", "value":
			if key == "value" && !valueIsBody {
				return pc, errors.New("unknown option value (use body)")
			}
			if pc.Body != nil {
				return pc, errors.New("give body or value, not both")
			}
			var s string
			switch val.(type) {
			case map[string]any, []any:
				// A gRPC body may be a JSON object. It is sent as its JSON text.
				if !isGRPC {
					return pc, fmt.Errorf("option %s must be a string, number or boolean", key)
				}
				b, err := json.Marshal(val)
				if err != nil {
					return pc, fmt.Errorf("option body: %w", err)
				}
				s = string(b)
			default:
				var err error
				if s, err = optionText(key, val); err != nil {
					return pc, err
				}
			}
			pc.Body = []byte(s)
		case "headers":
			if !isGRPC {
				return pc, errors.New("unknown option headers (only grpc.call takes headers)")
			}
			m, ok := val.(map[string]any)
			if !ok {
				return pc, errors.New("option headers must be an object of names and values")
			}
			pc.Headers = make(map[string]string, len(m))
			for hk, hv := range m {
				hs, err := optionText("headers."+hk, hv)
				if err != nil {
					return pc, err
				}
				pc.Headers[hk] = hs
			}
		case "insecure":
			b, ok := val.(bool)
			if !ok {
				return pc, errors.New("option insecure must be true or false")
			}
			pc.Insecure = b
		case "timeout":
			d, err := optionTimeout(val)
			if err != nil {
				return pc, err
			}
			pc.Timeout = d
		case "password":
			s, err := optionText(key, val)
			if err != nil {
				return pc, err
			}
			pc.Password = &s
		case "key_passphrase":
			if !strings.HasPrefix(name, "sftp.") {
				return pc, errors.New("unknown option key_passphrase")
			}
			s, err := optionText(key, val)
			if err != nil {
				return pc, err
			}
			pc.KeyPassphrase = &s
		default:
			s, err := optionText(key, val)
			if err != nil {
				return pc, err
			}
			if pc.Options == nil {
				pc.Options = map[string]string{}
			}
			pc.Options[key] = s
		}
	}
	return pc, nil
}

// sqlArgs turns the args of a postgres.query or mysql.query into the JSON
// text the driver reads.
func sqlArgs(val any) (string, error) {
	switch x := val.(type) {
	case string:
		return x, nil
	case []any:
		b, err := json.Marshal(x)
		if err != nil {
			return "", fmt.Errorf("option args: %w", err)
		}
		return string(b), nil
	}
	return "", errors.New("option args must be a list of values, such as [42, \"abc\"]")
}

// optionText turns a script value into the text a driver option holds.
func optionText(key string, val any) (string, error) {
	switch x := val.(type) {
	case string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10), nil
		}
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	}
	return "", fmt.Errorf("option %s must be a string, number or boolean", key)
}

// optionTimeout reads timeout as milliseconds (a number) or as a duration
// text such as "2s".
func optionTimeout(val any) (time.Duration, error) {
	var d time.Duration
	switch x := val.(type) {
	case int:
		d = time.Duration(x) * time.Millisecond
	case int64:
		d = time.Duration(x) * time.Millisecond
	case float64:
		d = time.Duration(x * float64(time.Millisecond))
	case string:
		var err error
		if d, err = time.ParseDuration(strings.TrimSpace(x)); err != nil {
			return 0, fmt.Errorf("option timeout=%q: want milliseconds or a duration such as 2s", x)
		}
	default:
		return 0, errors.New("option timeout must be milliseconds or a duration such as \"2s\"")
	}
	if d <= 0 {
		return 0, errors.New("option timeout must be more than zero")
	}
	return d, nil
}

// Fields is the reply as plain data: the shape a script sees. body is the
// reply of tcp and udp (and of grpc, as JSON text, with json holding it parsed,
// and status and statusName for the gRPC code). messages is for mqtt, each {topic, body}. kafka adds
// records (each {topic, partition, offset, key, value}) and text. A RabbitMQ
// reply uses messages too, each {exchange, routingKey, body, messageId,
// redelivered}, and text for an admin answer. error is
// "" when the call worked.
func (r *ProtoReply) Fields() map[string]any {
	msgs := make([]any, 0, len(r.Messages))
	for _, m := range r.Messages {
		msgs = append(msgs, map[string]any{"topic": m.Topic, "body": string(m.Body)})
	}
	f := map[string]any{
		"ok":            r.OK,
		"error":         r.Error,
		"bytesSent":     r.BytesSent,
		"bytesReceived": r.BytesReceived,
		"body":          string(r.Body),
		"messages":      msgs,
	}
	if r.IsGRPC {
		f["status"] = r.GRPCStatus
		f["statusName"] = r.GRPCStatusName
		f["json"] = r.JSON
	}
	if r.IsSQL {
		rows := make([]any, 0, len(r.SQLRows))
		for _, row := range r.SQLRows {
			m := make(map[string]any, len(r.SQLColumns))
			for i, col := range r.SQLColumns {
				if i < len(row) {
					m[col] = row[i]
				}
			}
			rows = append(rows, m)
		}
		cols := make([]any, len(r.SQLColumns))
		for i, c := range r.SQLColumns {
			cols[i] = c
		}
		f["rows"] = rows
		f["columns"] = cols
		f["rowCount"] = r.SQLRowCount
		f["rowsAffected"] = r.SQLRowsAffected
		if r.SQLHasCommandTag {
			f["commandTag"] = r.SQLCommandTag
		}
		if r.SQLHasLastInsert {
			f["lastInsertId"] = r.SQLLastInsertID
		}
	}
	if r.IsRedis {
		vals := r.RedisValues
		if vals == nil {
			vals = []any{}
		}
		f["value"] = r.RedisValue
		f["values"] = vals
		f["rowCount"] = r.RedisRowCount
	}
	if r.IsKafka {
		recs := make([]any, 0, len(r.Records))
		for _, rec := range r.Records {
			recs = append(recs, map[string]any{
				"topic": rec.Topic, "partition": rec.Partition, "offset": rec.Offset,
				"key": string(rec.Key), "value": string(rec.Value),
			})
		}
		f["records"] = recs
		f["text"] = r.Text
	}
	if r.IsRabbit {
		rm := make([]any, 0, len(r.RabbitMessages))
		for _, m := range r.RabbitMessages {
			rm = append(rm, map[string]any{
				"exchange": m.Exchange, "routingKey": m.RoutingKey, "body": m.Body,
				"messageId": m.MessageID, "redelivered": m.Redelivered,
			})
		}
		f["messages"] = rm
		f["text"] = r.RabbitText
	}
	if r.IsXfer {
		ents := make([]any, 0, len(r.XferEntries))
		for _, e := range r.XferEntries {
			when := ""
			if !e.Time.IsZero() {
				when = e.Time.UTC().Format(time.RFC3339)
			}
			ents = append(ents, map[string]any{
				"name": e.Name, "type": e.Type, "size": e.Size, "time": when,
			})
		}
		f["entries"] = ents
		f["size"] = r.XferSize
		f["total"] = r.XferTotal
		f["timing"] = map[string]any{
			"connectMs":  r.XferTiming.ConnectMs,
			"loginMs":    r.XferTiming.LoginMs,
			"transferMs": r.XferTiming.TransferMs,
			"uploadMs":   r.XferTiming.UploadMs,
			"downloadMs": r.XferTiming.DownloadMs,
			"deleteMs":   r.XferTiming.DeleteMs,
		}
		f["text"] = r.XferText
	}
	return f
}

// ProtoMessage is one MQTT message a call received.
type ProtoMessage struct {
	Topic string
	Body  []byte
}

// KafkaRecord is one Kafka record a call produced or read.
type KafkaRecord struct {
	Topic     string
	Partition int32
	Offset    int64
	Key       []byte
	Value     []byte
}

// ProtoReply is what a call hands back. A call that fails on the network
// (refused, timed out, no reply) is a reply with OK false and Error set,
// not a Go error: a script is expected to look at it.
type ProtoReply struct {
	OK            bool
	Error         string
	BytesSent     int64
	BytesReceived int64
	// Body is the reply read from a tcp or udp target. It is set even when
	// the call failed after some bytes came in.
	Body []byte
	// Messages are the messages an mqtt subscribe or roundtrip received.
	Messages []ProtoMessage
	// IsKafka is true for a Kafka reply, the only one with Records and Text.
	IsKafka bool
	// Records are the records a kafka produce, consume or roundtrip
	// wrote or read. For produce, Partition and Offset say where the broker
	// stored each one.
	Records []KafkaRecord
	// Text is the answer of a kafka admin action.
	Text string
	// IsGRPC is true for a gRPC reply. Body is then the reply as JSON text
	// (or base64), JSON is that text parsed, and GRPCStatus is the status
	// code, 0 when the call worked.
	IsGRPC         bool
	GRPCStatus     int
	GRPCStatusName string
	JSON           any
	// IsSQL is true for a PostgreSQL or MySQL reply. SQLRows hold the first
	// max_rows rows in the order of SQLColumns. SQLRowCount counts them all.
	// commandTag is set only for PostgreSQL. lastInsertId is set only for MySQL.
	IsSQL            bool
	SQLColumns       []string
	SQLRows          [][]any
	SQLRowCount      int
	SQLRowsAffected  int64
	SQLCommandTag    string
	SQLHasCommandTag bool
	SQLLastInsertID  int64
	SQLHasLastInsert bool
	// IsRedis is true for a Redis reply. Value is the last command's reply.
	// Values has one entry per command. RowCount counts the last reply.
	IsRedis       bool
	RedisValue    any
	RedisValues   []any
	RedisRowCount int
	// IsRabbit is true for a RabbitMQ reply. RabbitMessages are the messages
	// a consume or roundtrip read. RabbitText is an admin answer.
	IsRabbit       bool
	RabbitMessages []RabbitMessage
	RabbitText     string
	// Refused is true when the call was stopped before any bytes moved.
	Refused bool
	// IsXfer is true for an FTP or SFTP reply. XferEntries are the names a
	// list or stat returned. XferText is the short answer. Times are UTC.
	IsXfer      bool
	XferEntries []XferEntry
	XferSize    int64
	XferTotal   int
	XferTiming  XferTiming
	XferText    string
}

// XferEntry is one name from an FTP or SFTP listing.
type XferEntry struct {
	Name string
	Type string
	Size int64
	Time time.Time
}

// XferTiming is how long the parts of an FTP or SFTP call took, in milliseconds.
type XferTiming struct {
	ConnectMs  int64
	LoginMs    int64
	TransferMs int64
	UploadMs   int64
	DownloadMs int64
	DeleteMs   int64
}

// RabbitMessage is one AMQP message a consume or roundtrip read.
type RabbitMessage struct {
	Exchange    string
	RoutingKey  string
	Body        string
	MessageID   string
	Redelivered bool
}

// ProtoClient makes tcp, udp and mqtt calls for one VU. Each call opens
// its own connection, the same as the load-test drivers do, so the client
// holds no connection, except the Kafka clients that Close releases.
type ProtoClient struct {
	check   SafetyCheck
	timeout time.Duration

	// kafkaConns holds one Kafka client per connection setup, made on the
	// first call that needs it and kept until Close, as a real producer
	// keeps its client. The other protocols hold nothing.
	mu         sync.Mutex
	calls      CallRecorder
	kafkaConns map[string]*kafka.Driver
	grpcConns  map[string]*grpc.Conn
	pgConns    map[string]*postgres.Driver
	myConns    map[string]*mysql.Driver
	redisConns map[string]*redis.Driver
	rmqConns   map[string]*rabbitmq.Driver
	ftpConns   map[string]*ftp.Driver
	sftpConns  map[string]*sftp.Driver
}

// NewProtoClient returns a ProtoClient. check, when not nil, is run on the
// host of every call before it connects. timeout is the default for a call.
func NewProtoClient(check SafetyCheck, timeout time.Duration) *ProtoClient {
	if timeout <= 0 {
		timeout = defaultProtoTimeout
	}
	return &ProtoClient{check: check, timeout: timeout}
}

// SetCallRecorder counts calls made through Call. Nil means not counted.
func (c *ProtoClient) SetCallRecorder(r CallRecorder) {
	if c != nil {
		c.calls = r
	}
}

func (c *ProtoClient) observeCall(ctx context.Context, name string, d time.Duration, reply *ProtoReply, err error) {
	if c == nil || c.calls == nil || err != nil || reply == nil {
		return
	}
	if !reply.OK && RunEnded(ctx) {
		return
	}
	if refusedBeforeSend(reply) {
		return
	}
	ns, fn, ok := strings.Cut(name, ".")
	if !ok || !knownProtoFn(ns, fn) {
		return
	}
	c.calls.RecordCall(ns, fn, d, reply.failure(), reply.BytesSent, reply.BytesReceived)
	if reply.IsXfer && (ns == "ftp" || ns == "sftp") {
		RecordXferMeasures(c.calls, ns, fn, reply.OK, reply.XferTiming)
	}
}

// failure is the reply's error as a Go error, or nil when the call worked.
// A reply carries its error as text only.
func (r *ProtoReply) failure() error {
	if r.OK {
		return nil
	}
	return errors.New(r.Error)
}

// protoFnSet is the script call list, built once from ProtoFunctions so
// the check on every call does not allocate.
var protoFnSet = func() map[string]map[string]struct{} {
	out := map[string]map[string]struct{}{}
	for ns, fns := range ProtoFunctions() {
		set := map[string]struct{}{}
		for _, fn := range fns {
			set[fn] = struct{}{}
		}
		out[ns] = set
	}
	return out
}()

// knownProtoFn reports whether a script can call namespace.fn.
func knownProtoFn(ns, fn string) bool {
	_, ok := protoFnSet[ns][fn]
	return ok
}

// refusedBeforeSend is a call the safety rules or a missing write flag
// stopped before any bytes moved. That is a script error, not a latency.
func refusedBeforeSend(reply *ProtoReply) bool {
	return reply.Refused
}

// XferTimingFrom copies phase times from a file-transfer driver.
func XferTimingFrom(t xfercommon.Timing) XferTiming {
	return XferTiming{
		ConnectMs: t.ConnectMs, LoginMs: t.LoginMs, TransferMs: t.TransferMs,
		UploadMs: t.UploadMs, DownloadMs: t.DownloadMs, DeleteMs: t.DeleteMs,
	}
}

// RecordXferMeasures records the phases an FTP or SFTP call ran.
// The times are whole milliseconds, so a 0 can mean "did not run" or "ran in
// under 1 ms". A call that worked ran every phase its mode has, so those are
// recorded even at 0. A call that failed is recorded only for a phase that
// reports a time above 0, because a phase it never reached also reports 0.
// connect_time and login_time are recorded only above 0: a reused connection
// reports 0, and a 0 would pull the percentiles down. A connect or login
// under 1 ms is therefore not counted.
func RecordXferMeasures(rec CallRecorder, protocol, op string, ok bool, t XferTiming) {
	phase := func(name string, ms int64) {
		if ok || ms > 0 {
			rec.RecordMeasure(protocol, name, time.Duration(ms)*time.Millisecond)
		}
	}
	if t.ConnectMs > 0 {
		rec.RecordMeasure(protocol, "connect_time", time.Duration(t.ConnectMs)*time.Millisecond)
	}
	if t.LoginMs > 0 {
		rec.RecordMeasure(protocol, "login_time", time.Duration(t.LoginMs)*time.Millisecond)
	}
	switch op {
	case "upload":
		phase("upload_time", t.UploadMs)
	case "download":
		phase("download_time", t.DownloadMs)
	case "delete":
		phase("delete_time", t.DeleteMs)
	case "roundtrip":
		phase("upload_time", t.UploadMs)
		phase("download_time", t.DownloadMs)
		phase("delete_time", t.DeleteMs)
	}
}

// TCP sends over one TCP connection and reads the reply the options ask for.
func (c *ProtoClient) TCP(ctx context.Context, call ProtoCall) (*ProtoReply, error) {
	return c.socket(ctx, "tcp", call)
}

// UDP sends one datagram, and waits for a reply if the options ask for it.
func (c *ProtoClient) UDP(ctx context.Context, call ProtoCall) (*ProtoReply, error) {
	return c.socket(ctx, "udp", call)
}

// MQTT does one mqtt job. mode is publish, subscribe or roundtrip.
func (c *ProtoClient) MQTT(ctx context.Context, mode string, call ProtoCall) (*ProtoReply, error) {
	if err := c.checkTarget(call.URL, "mqtt"); err != nil {
		return nil, err
	}
	opts := cloneOptions(call.Options)
	if _, ok := opts["mode"]; ok {
		return nil, errors.New("mqtt: option mode is not allowed here, the function you called sets it")
	}
	opts["mode"] = mode
	target := protocol.Target{URL: call.URL, Body: call.Body, Options: opts, InsecureSkipVerify: call.Insecure}

	var (
		d   *mqtt.Driver
		err error
	)
	if call.Password != nil {
		d, err = mqtt.NewWithPassword(target, c.timeoutFor(call), *call.Password)
	} else {
		d, err = mqtt.New(target, c.timeoutFor(call))
	}
	if err != nil {
		return nil, err
	}
	res, msgs := d.Run(ctx)
	r := fromResult(res)
	for _, m := range msgs {
		r.Messages = append(r.Messages, ProtoMessage{Topic: m.Topic, Body: m.Payload})
	}
	return r, nil
}

// kafkaConnOptions are the options that make up a Kafka client. They are
// the same for every call of one client. The rest of a call's options
// describe the job.
var kafkaConnOptions = append([]string{"client_id", "sasl", "username", "acks", "compression"}, tlsconf.Keys...)

// Kafka does one Kafka job. mode is produce, consume, roundtrip or admin.
// The client is kept for the next call with the same connection options.
func (c *ProtoClient) Kafka(ctx context.Context, mode string, call ProtoCall) (*ProtoReply, error) {
	if err := c.checkTarget(call.URL, "kafka"); err != nil {
		return nil, err
	}
	opts := cloneOptions(call.Options)
	if _, ok := opts["mode"]; ok {
		return nil, errors.New("kafka: option mode is not allowed here, the function you called sets it")
	}
	opts["mode"] = mode
	if _, ok := opts["password_env"]; ok {
		return nil, errors.New("kafka: password_env is for the command line, pass password: env.NAME instead")
	}

	conn, err := c.kafkaConn(call, opts)
	if err != nil {
		return nil, err
	}
	d, err := conn.Call(opts, call.Body, c.timeoutFor(call))
	if err != nil {
		return nil, err
	}
	res, rep := d.Run(ctx)
	r := fromResult(res)
	r.IsKafka = true
	r.Text = rep.Text
	for _, rec := range rep.Records {
		r.Records = append(r.Records, KafkaRecord(rec))
	}
	return r, nil
}

// kafkaConn returns the client for call's connection options, making it
// the first time.
func (c *ProtoClient) kafkaConn(call ProtoCall, opts map[string]string) (*kafka.Driver, error) {
	connOpts := map[string]string{}
	for _, k := range kafkaConnOptions {
		if v, ok := opts[k]; ok {
			connOpts[k] = v
		}
	}
	keys := make([]string, 0, len(connOpts))
	for k := range connOpts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var kb strings.Builder
	fmt.Fprintf(&kb, "%s|insecure=%t|", call.URL, call.Insecure)
	if call.Password != nil {
		fmt.Fprintf(&kb, "pw=%q|", *call.Password)
	}
	for _, k := range keys {
		fmt.Fprintf(&kb, "%s=%q|", k, connOpts[k])
	}
	key := kb.String()

	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.kafkaConns[key]; ok {
		return d, nil
	}
	d, err := kafka.NewConn(protocol.Target{URL: call.URL, Options: connOpts, InsecureSkipVerify: call.Insecure}, c.timeoutFor(call), call.Password)
	if err != nil {
		return nil, err
	}
	if c.kafkaConns == nil {
		c.kafkaConns = map[string]*kafka.Driver{}
	}
	c.kafkaConns[key] = d
	return d, nil
}

// postgresConnOptions are the options that make up a PostgreSQL pool. They
// are the same for every call of one client. The rest of a call's options
// describe the job.
var postgresConnOptions = append([]string{"username", "database", "sslmode", "application_name", "pool", "allow_writes", "query_mode"}, tlsconf.Keys...)

// Postgres runs one SQL text. The pool is kept for the next call with the
// same connection options, as an application keeps its pool. It has one
// connection, because a script is one user, unless pool says more.
func (c *ProtoClient) Postgres(ctx context.Context, call ProtoCall) (*ProtoReply, error) {
	if err := c.checkTarget(call.URL, "postgres"); err != nil {
		return nil, err
	}
	opts := cloneOptions(call.Options)
	if _, ok := opts["password_env"]; ok {
		return nil, errors.New("postgres: password_env is for the command line, pass password: env.NAME instead")
	}
	conn, err := c.postgresConn(call, opts)
	if err != nil {
		return nil, err
	}
	d, err := conn.Call(opts, call.Body, c.timeoutFor(call))
	if err != nil {
		return nil, err
	}
	res, rep := d.Run(ctx)
	r := fromResult(res)
	r.IsSQL = true
	r.SQLHasCommandTag = true
	r.SQLColumns = rep.Columns
	r.SQLRows = rep.Rows
	r.SQLRowCount = rep.RowCount
	r.SQLRowsAffected = rep.RowsAffected
	r.SQLCommandTag = rep.CommandTag
	return r, nil
}

// postgresConn returns the pool for call's connection options, making it
// the first time.
func (c *ProtoClient) postgresConn(call ProtoCall, opts map[string]string) (*postgres.Driver, error) {
	connOpts := map[string]string{}
	for _, k := range postgresConnOptions {
		if v, ok := opts[k]; ok {
			connOpts[k] = v
		}
	}
	if _, ok := connOpts["pool"]; !ok {
		connOpts["pool"] = "1"
	}
	keys := make([]string, 0, len(connOpts))
	for k := range connOpts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var kb strings.Builder
	fmt.Fprintf(&kb, "%s|insecure=%t|", call.URL, call.Insecure)
	if call.Password != nil {
		fmt.Fprintf(&kb, "pw=%q|", *call.Password)
	}
	for _, k := range keys {
		fmt.Fprintf(&kb, "%s=%q|", k, connOpts[k])
	}
	key := kb.String()

	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.pgConns[key]; ok {
		return d, nil
	}
	d, err := postgres.NewConn(protocol.Target{URL: call.URL, Options: connOpts, InsecureSkipVerify: call.Insecure}, c.timeoutFor(call), call.Password)
	if err != nil {
		return nil, err
	}
	if c.pgConns == nil {
		c.pgConns = map[string]*postgres.Driver{}
	}
	c.pgConns[key] = d
	return d, nil
}

// GRPC makes one unary gRPC call. Its connection is kept for the next call
// to the same target, as a real client keeps its channel.
func (c *ProtoClient) GRPC(ctx context.Context, call ProtoCall) (*ProtoReply, error) {
	if err := c.checkTarget(call.URL, "grpc"); err != nil {
		return nil, err
	}
	gc := grpc.Call{Body: call.Body, Headers: call.Headers, Timeout: c.timeoutFor(call)}
	for k, v := range call.Options {
		switch k {
		case "method":
			gc.Method = v
		case "encoding":
			gc.Encoding = v
		case "ca_file", "cert_file", "key_file", "tls_server_name":
		default:
			return nil, fmt.Errorf("grpc: unknown option %s (want method, body, headers, encoding, timeout, insecure)", k)
		}
	}
	if gc.Method == "" {
		return nil, errors.New("grpc: option method is required, such as method: \"/package.Service/Method\"")
	}
	conn, err := c.grpcConn(call)
	if err != nil {
		return nil, err
	}
	rep, err := conn.Do(ctx, gc)
	if err != nil {
		return nil, err
	}
	r := fromResult(grpc.ResultOf(rep))
	r.IsGRPC = true
	r.GRPCStatus = int(rep.Code)
	r.GRPCStatusName = rep.Code.String()
	r.Body = rep.Body
	if len(rep.Body) > 0 && (gc.Encoding == "" || gc.Encoding == grpc.EncodingJSON) {
		var parsed any
		if json.Unmarshal(rep.Body, &parsed) == nil {
			r.JSON = parsed
		}
	}
	return r, nil
}

func (c *ProtoClient) grpcConn(call ProtoCall) (*grpc.Conn, error) {
	files := tlsconf.FromOptions(call.Options)
	key := fmt.Sprintf("%s|insecure=%t|ca=%s|cert=%s|key=%s|name=%s",
		call.URL, call.Insecure, files.CAFile, files.CertFile, files.KeyFile, files.ServerName)
	c.mu.Lock()
	defer c.mu.Unlock()
	if g, ok := c.grpcConns[key]; ok {
		return g, nil
	}
	g, err := grpc.NewConnWithFiles(call.URL, call.Insecure, files)
	if err != nil {
		return nil, err
	}
	if c.grpcConns == nil {
		c.grpcConns = map[string]*grpc.Conn{}
	}
	c.grpcConns[key] = g
	return g, nil
}

// mysqlConnOptions are the options that make up a MySQL pool. They are
// the same for every call of one client. The rest of a call's options
// describe the job.
var mysqlConnOptions = append([]string{"username", "database", "tls", "pool", "allow_writes", "query_mode"}, tlsconf.Keys...)

// MySQL runs one SQL text. The pool is kept for the next call with the
// same connection options, as an application keeps its pool. It has one
// connection, because a script is one user, unless pool says more.
func (c *ProtoClient) MySQL(ctx context.Context, call ProtoCall) (*ProtoReply, error) {
	if err := c.checkTarget(call.URL, "mysql"); err != nil {
		return nil, err
	}
	opts := cloneOptions(call.Options)
	if _, ok := opts["password_env"]; ok {
		return nil, errors.New("mysql: password_env is for the command line, pass password: env.NAME instead")
	}
	conn, err := c.mysqlConn(call, opts)
	if err != nil {
		return nil, err
	}
	d, err := conn.Call(opts, call.Body, c.timeoutFor(call))
	if err != nil {
		return nil, err
	}
	res, rep := d.Run(ctx)
	r := fromResult(res)
	r.IsSQL = true
	r.SQLHasLastInsert = true
	r.SQLColumns = rep.Columns
	r.SQLRows = rep.Rows
	r.SQLRowCount = rep.RowCount
	r.SQLRowsAffected = rep.RowsAffected
	r.SQLLastInsertID = rep.LastInsertID
	return r, nil
}

// mysqlConn returns the pool for call's connection options, making it the
// first time.
func (c *ProtoClient) mysqlConn(call ProtoCall, opts map[string]string) (*mysql.Driver, error) {
	connOpts := map[string]string{}
	for _, k := range mysqlConnOptions {
		if v, ok := opts[k]; ok {
			connOpts[k] = v
		}
	}
	if _, ok := connOpts["pool"]; !ok {
		connOpts["pool"] = "1"
	}
	keys := make([]string, 0, len(connOpts))
	for k := range connOpts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var kb strings.Builder
	fmt.Fprintf(&kb, "%s|insecure=%t|", call.URL, call.Insecure)
	if call.Password != nil {
		fmt.Fprintf(&kb, "pw=%q|", *call.Password)
	}
	for _, k := range keys {
		fmt.Fprintf(&kb, "%s=%q|", k, connOpts[k])
	}
	key := kb.String()

	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.myConns[key]; ok {
		return d, nil
	}
	d, err := mysql.NewConn(protocol.Target{URL: call.URL, Options: connOpts, InsecureSkipVerify: call.Insecure}, c.timeoutFor(call), call.Password)
	if err != nil {
		return nil, err
	}
	if c.myConns == nil {
		c.myConns = map[string]*mysql.Driver{}
	}
	c.myConns[key] = d
	return d, nil
}

// redisConnOptions are the options that make up a Redis client. They are
// the same for every call of one client. The rest of a call's options
// describe the job.
var redisConnOptions = append([]string{"username", "database", "tls", "pool", "protocol", "allow_writes", "allow_admin"}, tlsconf.Keys...)

// Redis runs one command, or several as a pipeline. The client is kept for
// the next call with the same connection options. It has one connection,
// because a script is one user, unless pool says more.
func (c *ProtoClient) Redis(ctx context.Context, call ProtoCall) (*ProtoReply, error) {
	if err := c.checkTarget(call.URL, "redis"); err != nil {
		return nil, err
	}
	opts := cloneOptions(call.Options)
	if _, ok := opts["password_env"]; ok {
		return nil, errors.New("redis: password_env is for the command line, pass password: env.NAME instead")
	}
	conn, err := c.redisConn(call, opts)
	if err != nil {
		return nil, err
	}
	d, err := conn.Call(opts, call.Body, c.timeoutFor(call))
	if err != nil {
		return nil, err
	}
	res, rep := d.Run(ctx)
	r := fromResult(res)
	r.IsRedis = true
	r.RedisValue = rep.Value
	r.RedisValues = rep.Values
	r.RedisRowCount = rep.RowCount
	return r, nil
}

// redisConn returns the client for call's connection options, making it the
// first time.
func (c *ProtoClient) redisConn(call ProtoCall, opts map[string]string) (*redis.Driver, error) {
	connOpts := map[string]string{}
	for _, k := range redisConnOptions {
		if v, ok := opts[k]; ok {
			connOpts[k] = v
		}
	}
	if _, ok := connOpts["pool"]; !ok {
		connOpts["pool"] = "1"
	}
	keys := make([]string, 0, len(connOpts))
	for k := range connOpts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var kb strings.Builder
	fmt.Fprintf(&kb, "%s|insecure=%t|", call.URL, call.Insecure)
	if call.Password != nil {
		fmt.Fprintf(&kb, "pw=%q|", *call.Password)
	}
	for _, k := range keys {
		fmt.Fprintf(&kb, "%s=%q|", k, connOpts[k])
	}
	key := kb.String()

	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.redisConns[key]; ok {
		return d, nil
	}
	d, err := redis.NewConn(protocol.Target{URL: call.URL, Options: connOpts, InsecureSkipVerify: call.Insecure}, c.timeoutFor(call), call.Password)
	if err != nil {
		return nil, err
	}
	if c.redisConns == nil {
		c.redisConns = map[string]*redis.Driver{}
	}
	c.redisConns[key] = d
	return d, nil
}

// Close closes the Kafka and gRPC clients, the SQL pools and the Redis clients. Safe to call more than once.
func (c *ProtoClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, g := range c.grpcConns {
		_ = g.Close()
		delete(c.grpcConns, k)
	}
	for k, d := range c.kafkaConns {
		_ = d.Close()
		delete(c.kafkaConns, k)
	}
	for k, d := range c.pgConns {
		_ = d.Close()
		delete(c.pgConns, k)
	}
	for k, d := range c.myConns {
		_ = d.Close()
		delete(c.myConns, k)
	}
	for k, d := range c.redisConns {
		_ = d.Close()
		delete(c.redisConns, k)
	}
	for k, d := range c.rmqConns {
		_ = d.Close()
		delete(c.rmqConns, k)
	}
	for k, d := range c.ftpConns {
		_ = d.Close()
		delete(c.ftpConns, k)
	}
	for k, d := range c.sftpConns {
		_ = d.Close()
		delete(c.sftpConns, k)
	}
}

// rabbitConnOptions are the options that make up a RabbitMQ connection.
// They are the same for every call of one client. The rest of a call's
// options describe the job.
var rabbitConnOptions = append([]string{
	"username", "vhost", "tls", "connection", "channels", "heartbeat",
	"connection_name", "allow_writes", "allow_admin",
}, tlsconf.Keys...)

// RabbitMQ does one RabbitMQ job. mode is publish, consume, roundtrip or
// admin. The connection is kept for the next call with the same connection
// options.
func (c *ProtoClient) RabbitMQ(ctx context.Context, mode string, call ProtoCall) (*ProtoReply, error) {
	if err := c.checkTarget(call.URL, "rabbitmq"); err != nil {
		return nil, err
	}
	opts := cloneOptions(call.Options)
	if _, ok := opts["mode"]; ok {
		return nil, errors.New("rabbitmq: option mode is not allowed here, the function you called sets it")
	}
	opts["mode"] = mode
	if _, ok := opts["password_env"]; ok {
		return nil, errors.New("rabbitmq: password_env is for the command line, pass password: env.NAME instead")
	}
	conn, err := c.rabbitConn(call, opts)
	if err != nil {
		return nil, err
	}
	d, err := conn.Call(opts, call.Body, c.timeoutFor(call))
	if err != nil {
		return nil, err
	}
	res, rep := d.Run(ctx)
	r := fromResult(res)
	r.IsRabbit = true
	r.RabbitText = rep.Text
	for _, m := range rep.Messages {
		r.RabbitMessages = append(r.RabbitMessages, RabbitMessage{
			Exchange: m.Exchange, RoutingKey: m.RoutingKey, Body: m.Body,
			MessageID: m.MessageID, Redelivered: m.Redelivered,
		})
	}
	return r, nil
}

func (c *ProtoClient) rabbitConn(call ProtoCall, opts map[string]string) (*rabbitmq.Driver, error) {
	connOpts := map[string]string{}
	for _, k := range rabbitConnOptions {
		if v, ok := opts[k]; ok {
			connOpts[k] = v
		}
	}
	keys := make([]string, 0, len(connOpts))
	for k := range connOpts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var kb strings.Builder
	fmt.Fprintf(&kb, "%s|insecure=%t|", call.URL, call.Insecure)
	if call.Password != nil {
		fmt.Fprintf(&kb, "pw=%q|", *call.Password)
	}
	for _, k := range keys {
		fmt.Fprintf(&kb, "%s=%q|", k, connOpts[k])
	}
	key := kb.String()

	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.rmqConns[key]; ok {
		return d, nil
	}
	d, err := rabbitmq.NewConn(protocol.Target{URL: call.URL, Options: connOpts, InsecureSkipVerify: call.Insecure}, c.timeoutFor(call), call.Password)
	if err != nil {
		return nil, err
	}
	if c.rmqConns == nil {
		c.rmqConns = map[string]*rabbitmq.Driver{}
	}
	c.rmqConns[key] = d
	return d, nil
}

// ftpConnOptions are the options that make up an FTP session pool. They
// are the same for every call of one client. The rest of a call's options
// describe the job.
var ftpConnOptions = append([]string{
	"username", "tls", "tls_verify", "sessions", "connection",
	"data_host", "epsv", "allow_writes", "allow_admin",
}, tlsconf.Keys...)

// FTP does one FTP job. mode is connect, download, upload, list, stat,
// delete or roundtrip. The session pool is kept for the next call with
// the same connection options.
func (c *ProtoClient) FTP(ctx context.Context, mode string, call ProtoCall) (*ProtoReply, error) {
	if err := c.checkTarget(call.URL, "ftp"); err != nil {
		return nil, err
	}
	opts := cloneOptions(call.Options)
	if _, ok := opts["mode"]; ok {
		return nil, errors.New("ftp: option mode is not allowed here, the function you called sets it")
	}
	opts["mode"] = mode
	if _, ok := opts["password_env"]; ok {
		return nil, errors.New("ftp: password_env is for the command line, pass password: env.NAME instead")
	}
	conn, err := c.ftpConn(call, opts)
	if err != nil {
		return nil, err
	}
	d, err := conn.Call(opts, call.Body, c.timeoutFor(call))
	if err != nil {
		return nil, err
	}
	res, rep := d.Run(ctx)
	r := fromResult(res)
	fillXfer(r, rep)
	return r, nil
}

func (c *ProtoClient) ftpConn(call ProtoCall, opts map[string]string) (*ftp.Driver, error) {
	connOpts := map[string]string{}
	for _, k := range ftpConnOptions {
		if v, ok := opts[k]; ok {
			connOpts[k] = v
		}
	}
	keys := make([]string, 0, len(connOpts))
	for k := range connOpts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var kb strings.Builder
	fmt.Fprintf(&kb, "%s|insecure=%t|", call.URL, call.Insecure)
	if call.Password != nil {
		fmt.Fprintf(&kb, "pw=%q|", *call.Password)
	}
	for _, k := range keys {
		fmt.Fprintf(&kb, "%s=%q|", k, connOpts[k])
	}
	key := kb.String()

	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.ftpConns[key]; ok {
		return d, nil
	}
	d, err := ftp.NewConn(protocol.Target{URL: call.URL, Options: connOpts, InsecureSkipVerify: call.Insecure}, c.timeoutFor(call), call.Password)
	if err != nil {
		return nil, err
	}
	if c.ftpConns == nil {
		c.ftpConns = map[string]*ftp.Driver{}
	}
	c.ftpConns[key] = d
	return d, nil
}

// sftpConnOptions are the options that make up one set of SFTP connections.
// They are the same for every call of one client. The rest of a call's
// options describe the job. TLS options are not among them.
var sftpConnOptions = []string{
	"username", "key_file", "host_key", "connections", "connection",
	"allow_writes", "allow_admin", "concurrent_writes",
}

// SFTP does one SFTP job. mode is connect, download, upload, list, stat,
// delete or roundtrip. The connections are kept for the next call with
// the same connection options.
func (c *ProtoClient) SFTP(ctx context.Context, mode string, call ProtoCall) (*ProtoReply, error) {
	if err := c.checkTarget(call.URL, "sftp"); err != nil {
		return nil, err
	}
	opts := cloneOptions(call.Options)
	if _, ok := opts["mode"]; ok {
		return nil, errors.New("sftp: option mode is not allowed here, the function you called sets it")
	}
	opts["mode"] = mode
	if _, ok := opts["password_env"]; ok {
		return nil, errors.New("sftp: password_env is for the command line, pass password: env.NAME instead")
	}
	if _, ok := opts["key_passphrase_env"]; ok {
		return nil, errors.New("sftp: key_passphrase_env is for the command line, pass key_passphrase: env.NAME instead")
	}
	conn, err := c.sftpConn(call, opts)
	if err != nil {
		return nil, err
	}
	d, err := conn.Call(opts, call.Body, c.timeoutFor(call))
	if err != nil {
		return nil, err
	}
	res, rep := d.Run(ctx)
	r := fromResult(res)
	fillXfer(r, rep)
	return r, nil
}

func (c *ProtoClient) sftpConn(call ProtoCall, opts map[string]string) (*sftp.Driver, error) {
	connOpts := map[string]string{}
	for _, k := range sftpConnOptions {
		if v, ok := opts[k]; ok {
			connOpts[k] = v
		}
	}
	keys := make([]string, 0, len(connOpts))
	for k := range connOpts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var kb strings.Builder
	fmt.Fprintf(&kb, "%s|insecure=%t|", call.URL, call.Insecure)
	if call.Password != nil {
		fmt.Fprintf(&kb, "pw=%s|", secretHash(*call.Password))
	}
	if call.KeyPassphrase != nil {
		fmt.Fprintf(&kb, "pp=%s|", secretHash(*call.KeyPassphrase))
	}
	for _, k := range keys {
		fmt.Fprintf(&kb, "%s=%q|", k, connOpts[k])
	}
	key := kb.String()

	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.sftpConns[key]; ok {
		return d, nil
	}
	d, err := sftp.NewConn(protocol.Target{URL: call.URL, Options: connOpts, InsecureSkipVerify: call.Insecure}, c.timeoutFor(call), call.Password, call.KeyPassphrase)
	if err != nil {
		return nil, err
	}
	if c.sftpConns == nil {
		c.sftpConns = map[string]*sftp.Driver{}
	}
	c.sftpConns[key] = d
	return d, nil
}

func fillXfer(r *ProtoReply, rep xfercommon.Reply) {
	r.IsXfer = true
	r.XferSize = rep.Size
	r.XferTotal = rep.Total
	r.XferText = rep.Text
	r.XferTiming = XferTiming{
		ConnectMs: rep.Timing.ConnectMs, LoginMs: rep.Timing.LoginMs,
		TransferMs: rep.Timing.TransferMs, UploadMs: rep.Timing.UploadMs,
		DownloadMs: rep.Timing.DownloadMs, DeleteMs: rep.Timing.DeleteMs,
	}
	for _, e := range rep.Entries {
		r.XferEntries = append(r.XferEntries, XferEntry{Name: e.Name, Type: e.Type, Size: e.Size, Time: e.Time})
	}
}

func secretHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (c *ProtoClient) socket(ctx context.Context, network string, call ProtoCall) (*ProtoReply, error) {
	if err := c.checkTarget(call.URL, network); err != nil {
		return nil, err
	}
	opts := cloneOptions(call.Options)
	// A script writes real newlines and \x00 itself. Turning on the
	// command line's backslash escapes by default would change what it sent.
	if _, ok := opts["escape"]; !ok {
		opts["escape"] = "false"
	}
	target := protocol.Target{URL: call.URL, Body: call.Body, Options: opts, InsecureSkipVerify: call.Insecure}
	newDriver := socket.NewTCP
	if network == "udp" {
		newDriver = socket.NewUDP
	}
	d, err := newDriver(target, c.timeoutFor(call))
	if err != nil {
		return nil, err
	}
	res, body := d.Run(ctx)
	r := fromResult(res)
	r.Body = body
	return r, nil
}

func (c *ProtoClient) timeoutFor(call ProtoCall) time.Duration {
	if call.Timeout > 0 {
		return call.Timeout
	}
	return c.timeout
}

func fromResult(res protocol.Result) *ProtoReply {
	r := &ProtoReply{OK: res.Success, BytesSent: res.BytesSent, BytesReceived: res.BytesReceived, Refused: res.Refused}
	if res.Err != nil {
		r.Error = res.Err.Error()
	}
	return r
}

func cloneOptions(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// checkTarget runs the safety check on the call's host. A target that has
// no host is refused here, because the check cannot judge it.
func (c *ProtoClient) checkTarget(rawURL, scheme string) error {
	if c.check == nil {
		return nil
	}
	if rawURL == "" {
		return fmt.Errorf("%s: a url is required", scheme)
	}
	if !strings.Contains(rawURL, "://") {
		rawURL = scheme + "://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%s: parsing url: %w", scheme, urlerr.Inner(err))
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%s: url %q has no host", scheme, urlerr.Mask(rawURL))
	}
	return c.check(u.Hostname())
}
