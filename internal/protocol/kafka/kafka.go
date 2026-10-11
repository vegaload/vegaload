// Package kafka implements the "kafka" protocol.Protocol driver, for load
// testing an Apache Kafka cluster (or any broker that speaks the Kafka
// protocol). It uses the franz-go client, which is pure Go.
//
// The job of each call to Do is chosen with -opt mode=...:
//
//	produce    send count records and wait for the broker's ack. This is
//	           the default.
//	consume    read count records from the topic and check them.
//	roundtrip  produce count records, then read exactly those records
//	           back. This measures the way through the broker.
//	admin      do one admin action, chosen with -opt action=...
//
// Options (set with -opt key=value):
//
//	mode           produce (default), consume, roundtrip, or admin.
//	topic          The topic. Required, except for the admin actions that
//	               list things. {id} is replaced by this call's unique id.
//	key            produce and roundtrip: the record key. {id} is replaced.
//	acks           produce: all (default), leader, or none. roundtrip
//	               needs the offset, so it does not accept none.
//	compression    produce: none (default), gzip, snappy, lz4, or zstd.
//	count          produce, consume, roundtrip: records per call (default 1).
//	expect         consume: every record value must contain this text.
//	               admin list actions: the listing must contain it.
//	from           consume: start (default) or end.
//	action         admin: list_topics, create_topic, delete_topic,
//	               topic_lifecycle (create and then delete), list_groups,
//	               or describe_cluster.
//	partitions     admin create: partitions of the new topic (default 1).
//	replication    admin create: replication factor (default 1).
//	sasl           plain, scram-sha-256, or scram-sha-512.
//	username       The SASL user name. Needs sasl.
//	password_env   The name of an environment variable that holds the SASL
//	               password. Needs username. The password is never put on
//	               the command line.
//	client_id      The client id sent to the brokers (default "vegaload").
//
// The target is kafka://host:port (default port 9092) or kafkas://host:port
// (TLS, default port 9093). It is the first broker to ask. The client
// then connects to the broker addresses the cluster announces, so those
// must be reachable too.
//
// produce, roundtrip and admin share one client between all users, as a
// real producer does. consume and the reading half of roundtrip open a
// new connection for each call, because each call reads its own part of
// the topic. That connect time is part of the measured time.
package kafka

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/urlerr"
)

// Options are the -opt keys this driver accepts. The command's protocol
// table uses the same list, so the two cannot differ.
var Options = []string{
	"mode", "topic", "key", "acks", "compression", "count", "expect", "from",
	"action", "partitions", "replication", "sasl", "username", "password_env",
	"client_id",
}

const (
	modeProduce   = "produce"
	modeConsume   = "consume"
	modeRoundtrip = "roundtrip"
	modeAdmin     = "admin"

	actListTopics     = "list_topics"
	actCreateTopic    = "create_topic"
	actDeleteTopic    = "delete_topic"
	actTopicLifecycle = "topic_lifecycle"
	actListGroups     = "list_groups"
	actDescribe       = "describe_cluster"

	maxCount = 10000
)

// Driver is a Kafka protocol.Protocol.
type Driver struct {
	target  protocol.Target
	timeout time.Duration
	mode    string
	topic   string
	key     string
	count   int
	expect  []byte
	fromEnd bool
	action  string
	parts   int32
	repl    int16

	base []kgo.Opt // options every client uses

	// shared is the client for produce, roundtrip and admin. It is nil
	// in consume mode. Creating it does not connect.
	shared *kgo.Client
	adm    *kadm.Client

	problems *problemLog // the client's own warnings, kept as a hint

	salt string // random, per Driver, so two runs never share ids
	seq  *atomic.Uint64

	// password is a SASL password given by a script. nil on the command
	// line, where password_env names the variable that holds it.
	password *string
}

// New returns a ready-to-use Driver for target. target.URL is
// kafka://host[:port] or kafkas://host[:port].
//
// New returns an error only for a configuration problem, never for the
// cluster being unreachable, which Do reports per call.
func New(target protocol.Target, timeout time.Duration) (*Driver, error) {
	return build(target, timeout, nil, false)
}

// NewConn is New for a scenario script. It makes only the connection: the
// client, its login and its producer settings (client_id, sasl, username,
// acks, compression). It needs no topic and no mode. The job of each call
// is then set with Call. password is the SASL password, which the script
// already holds, so password_env is not used with it.
func NewConn(target protocol.Target, timeout time.Duration, password *string) (*Driver, error) {
	return build(target, timeout, password, true)
}

// Call returns a Driver for one call of a script: the same connection as d,
// with the job that opts and body describe. opts must hold the same
// connection options d was made with. The result shares d's client, so
// closing it is d's job, not the caller's.
func (d *Driver) Call(opts map[string]string, body []byte, timeout time.Duration) (*Driver, error) {
	nd := *d
	nd.timeout = timeout
	nd.target.Body = body
	nd.target.Options = opts
	if err := nd.target.RejectUnknownOptions(Options...); err != nil {
		return nil, fmt.Errorf("kafka: %w", err)
	}
	if err := nd.readOptions(); err != nil {
		return nil, err
	}
	if nd.mode == modeProduce || nd.mode == modeRoundtrip {
		if _, err := nd.producerOptions(); err != nil {
			return nil, err
		}
	}
	return &nd, nil
}

func build(target protocol.Target, timeout time.Duration, password *string, connOnly bool) (*Driver, error) {
	d := &Driver{target: target, timeout: timeout, password: password, problems: &problemLog{}, seq: &atomic.Uint64{}}

	u, err := url.Parse(target.URL)
	if err != nil {
		return nil, fmt.Errorf("kafka: parsing target URL: %w", urlerr.Inner(err))
	}
	port := u.Port()
	useTLS := false
	switch u.Scheme {
	case "kafka":
		if port == "" {
			port = "9092"
		}
	case "kafkas":
		useTLS = true
		if port == "" {
			port = "9093"
		}
	default:
		return nil, fmt.Errorf("kafka: unsupported scheme %q, want kafka:// or kafkas://", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("kafka: target %q has no host", urlerr.Mask(target.URL))
	}
	seed := net.JoinHostPort(u.Hostname(), port)

	if err := target.RejectUnknownOptions(Options...); err != nil {
		return nil, fmt.Errorf("kafka: %w", err)
	}
	if connOnly {
		// Only the connection is made here. Every call sets its own job.
		d.mode = modeProduce
		if err := d.target.RejectUnknownOptions("client_id", "sasl", "username", "password_env", "acks", "compression"); err != nil {
			return nil, fmt.Errorf("kafka: %w", err)
		}
	} else if err := d.readOptions(); err != nil {
		return nil, err
	}

	mech, err := d.saslMechanism()
	if err != nil {
		return nil, err
	}

	// The client refuses a timeout under 1s for its own limits. The real
	// limit of each call is the context in Do, so this is only a backstop.
	backstop := max(timeout, time.Second)
	if connOnly {
		// A script chooses a timeout for each call, and the client lives
		// longer than one call. The call's context is the real limit.
		backstop = max(timeout, time.Minute)
		d.timeout = backstop
		defer func() { d.timeout = timeout }()
	}
	clientID := target.Option("client_id", "vegaload")
	d.base = []kgo.Opt{
		kgo.SeedBrokers(seed),
		kgo.ClientID(clientID),
		kgo.DialTimeout(backstop),
		kgo.RetryTimeout(backstop),
		kgo.RequestTimeoutOverhead(backstop),
		kgo.UnknownTopicRetries(0),
		kgo.WithLogger(d.problems),
	}
	if useTLS {
		d.base = append(d.base, kgo.DialTLSConfig(&tls.Config{
			InsecureSkipVerify: target.InsecureSkipVerify,
			MinVersion:         tls.VersionTLS12,
		}))
	}
	if mech != nil {
		d.base = append(d.base, kgo.SASL(mech))
	}

	var salt [4]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return nil, fmt.Errorf("kafka: reading random bytes: %w", err)
	}
	d.salt = hex.EncodeToString(salt[:])

	if d.mode != modeConsume {
		opts := d.base
		if d.mode == modeProduce || d.mode == modeRoundtrip {
			po, err := d.producerOptions()
			if err != nil {
				return nil, err
			}
			opts = append(append([]kgo.Opt(nil), d.base...), po...)
		}
		cl, err := kgo.NewClient(opts...)
		if err != nil {
			return nil, fmt.Errorf("kafka: creating the client: %w", err)
		}
		d.shared = cl
		d.adm = kadm.NewClient(cl)
	}
	return d, nil
}

func (d *Driver) readOptions() error {
	t := d.target
	d.mode = t.Option("mode", modeProduce)
	switch d.mode {
	case modeProduce, modeConsume, modeRoundtrip, modeAdmin:
	default:
		return fmt.Errorf("kafka: mode=%q, want produce, consume, roundtrip, or admin", d.mode)
	}

	var err error
	if d.count, err = t.OptionInt("count", 1); err != nil {
		return fmt.Errorf("kafka: %w", err)
	}
	if d.count < 1 || d.count > maxCount {
		return fmt.Errorf("kafka: count=%d, want 1 to %d", d.count, maxCount)
	}
	parts, err := t.OptionInt("partitions", 1)
	if err != nil {
		return fmt.Errorf("kafka: %w", err)
	}
	repl, err := t.OptionInt("replication", 1)
	if err != nil {
		return fmt.Errorf("kafka: %w", err)
	}
	if parts < 1 || parts > 10000 {
		return fmt.Errorf("kafka: partitions=%d, want 1 to 10000", parts)
	}
	if repl < 1 || repl > 100 {
		return fmt.Errorf("kafka: replication=%d, want 1 to 100", repl)
	}
	d.parts, d.repl = int32(parts), int16(repl)

	d.topic = t.Option("topic", "")
	d.key = t.Option("key", "")
	d.expect = []byte(t.Option("expect", ""))
	d.action = t.Option("action", "")

	switch t.Option("from", "start") {
	case "start":
	case "end":
		d.fromEnd = true
	default:
		return fmt.Errorf("kafka: from=%q, want start or end", t.Options["from"])
	}

	// An option that the chosen mode ignores is an error, like a typo.
	only := func(keys []string, modes ...string) error {
		for _, m := range modes {
			if d.mode == m {
				return nil
			}
		}
		for _, k := range keys {
			if _, ok := t.Options[k]; ok {
				return fmt.Errorf("kafka: option %s is only used in %s mode", k, strings.Join(modes, " or "))
			}
		}
		return nil
	}
	if err := only([]string{"key", "acks", "compression"}, modeProduce, modeRoundtrip); err != nil {
		return err
	}
	if err := only([]string{"from"}, modeConsume); err != nil {
		return err
	}
	if err := only([]string{"action", "partitions", "replication"}, modeAdmin); err != nil {
		return err
	}
	if _, ok := t.Options["count"]; ok && d.mode == modeAdmin {
		return errors.New("kafka: option count is not used in admin mode")
	}

	if d.mode == modeAdmin {
		switch d.action {
		case actListTopics, actListGroups, actDescribe:
		case actCreateTopic, actDeleteTopic, actTopicLifecycle:
			if d.topic == "" {
				return fmt.Errorf("kafka: action %s needs -opt topic=NAME", d.action)
			}
		case "":
			return errors.New("kafka: mode=admin needs -opt action=list_topics, create_topic, delete_topic, topic_lifecycle, list_groups, or describe_cluster")
		default:
			return fmt.Errorf("kafka: action=%q, want list_topics, create_topic, delete_topic, topic_lifecycle, list_groups, or describe_cluster", d.action)
		}
		if len(d.expect) > 0 && d.action != actListTopics && d.action != actListGroups && d.action != actDescribe {
			return fmt.Errorf("kafka: expect is only used by the list and describe actions in admin mode")
		}
		return nil
	}

	if d.topic == "" {
		return errors.New("kafka: topic is required: -opt topic=NAME")
	}
	if len(d.expect) > 0 && d.mode != modeConsume {
		return errors.New("kafka: expect is only used in consume mode or by admin list actions")
	}
	if d.mode == modeConsume && len(t.Body) > 0 {
		return errors.New("kafka: consume mode does not send a body")
	}
	if strings.Contains(d.topic, "{id}") && d.mode == modeConsume {
		return errors.New("kafka: {id} in the topic is for the modes that create things: produce, roundtrip and admin")
	}
	return nil
}

func (d *Driver) producerOptions() ([]kgo.Opt, error) {
	t := d.target
	opts := []kgo.Opt{
		kgo.DisableIdempotentWrite(),
		kgo.RecordDeliveryTimeout(max(d.timeout, time.Second)),
		kgo.ProduceRequestTimeout(max(d.timeout, time.Second)),
	}
	switch a := t.Option("acks", "all"); a {
	case "all":
		opts = append(opts, kgo.RequiredAcks(kgo.AllISRAcks()))
	case "leader":
		opts = append(opts, kgo.RequiredAcks(kgo.LeaderAck()))
	case "none":
		if d.mode == modeRoundtrip {
			return nil, errors.New("kafka: roundtrip needs the offset of each record, so acks=none cannot be used")
		}
		opts = append(opts, kgo.RequiredAcks(kgo.NoAck()))
	default:
		return nil, fmt.Errorf("kafka: acks=%q, want all, leader, or none", a)
	}
	switch c := t.Option("compression", "none"); c {
	case "none":
		opts = append(opts, kgo.ProducerBatchCompression(kgo.NoCompression()))
	case "gzip":
		opts = append(opts, kgo.ProducerBatchCompression(kgo.GzipCompression()))
	case "snappy":
		opts = append(opts, kgo.ProducerBatchCompression(kgo.SnappyCompression()))
	case "lz4":
		opts = append(opts, kgo.ProducerBatchCompression(kgo.Lz4Compression()))
	case "zstd":
		opts = append(opts, kgo.ProducerBatchCompression(kgo.ZstdCompression()))
	default:
		return nil, fmt.Errorf("kafka: compression=%q, want none, gzip, snappy, lz4, or zstd", c)
	}
	return opts, nil
}

func (d *Driver) saslMechanism() (sasl.Mechanism, error) {
	t := d.target
	name := t.Option("sasl", "")
	user := t.Option("username", "")
	pwEnv := t.Option("password_env", "")
	if name == "" {
		if user != "" || pwEnv != "" || d.password != nil {
			return nil, errors.New("kafka: username and password need -opt sasl=plain, scram-sha-256, or scram-sha-512")
		}
		return nil, nil
	}
	if user == "" {
		return nil, errors.New("kafka: sasl needs -opt username=NAME")
	}
	var pw string
	if d.password != nil {
		if pwEnv != "" {
			return nil, errors.New("kafka: give a password or password_env, not both")
		}
		pw = *d.password
	} else {
		if pwEnv == "" {
			return nil, errors.New("kafka: sasl needs -opt password_env=NAME, the name of an environment variable that holds the password")
		}
		var ok bool
		if pw, ok = os.LookupEnv(pwEnv); !ok {
			return nil, fmt.Errorf("kafka: environment variable %s (password_env) is not set", pwEnv)
		}
	}
	switch name {
	case "plain":
		return plain.Auth{User: user, Pass: pw}.AsMechanism(), nil
	case "scram-sha-256":
		return scram.Auth{User: user, Pass: pw}.AsSha256Mechanism(), nil
	case "scram-sha-512":
		return scram.Auth{User: user, Pass: pw}.AsSha512Mechanism(), nil
	}
	return nil, fmt.Errorf("kafka: sasl=%q, want plain, scram-sha-256, or scram-sha-512", name)
}

// Name implements protocol.Protocol.
func (d *Driver) Name() string { return "kafka" }

// Close implements protocol.Protocol. It closes the shared client.
func (d *Driver) Close() error {
	if d.shared != nil {
		d.shared.Close()
	}
	return nil
}

func (d *Driver) newID() string {
	return "vegaload-" + d.salt + "-" + strconv.FormatUint(d.seq.Add(1), 36)
}

// Record is one Kafka record: one that was produced (its partition and
// offset are where the broker stored it) or one that was read.
type Record struct {
	Topic     string
	Partition int32
	Offset    int64
	Key       []byte
	Value     []byte
}

// Reply is what a call read or wrote, besides the result: the records of
// produce, consume and roundtrip, and the text answer of an admin action.
type Reply struct {
	Records []Record
	Text    string
}

// Do implements protocol.Protocol.
func (d *Driver) Do(parent context.Context) (protocol.Result, error) {
	res, _ := d.Run(parent)
	return res, nil
}

// Run is Do, and it also returns the records and text of the call. A load
// test does not need them. A scenario script does.
func (d *Driver) Run(parent context.Context) (protocol.Result, Reply) {
	ctx, cancel := context.WithTimeout(parent, d.timeout)
	defer cancel()

	id := d.newID()
	var (
		sent, got int64
		err       error
		rep       Reply
	)
	switch d.mode {
	case modeProduce:
		var recs []*kgo.Record
		recs, sent, err = d.produce(ctx, id)
		rep.Records = toRecords(recs)
	case modeConsume:
		got, rep.Records, err = d.consume(ctx, d.replaceID(d.topic, id), nil)
	case modeRoundtrip:
		var recs []*kgo.Record
		recs, sent, err = d.produce(ctx, id)
		if err == nil {
			got, rep.Records, err = d.consume(ctx, recs[0].Topic, recs)
		}
	case modeAdmin:
		got, rep.Text, err = d.adminCall(ctx, id)
	}
	if err != nil {
		return protocol.Result{BytesSent: sent, BytesReceived: got, Err: d.explain(parent, ctx, err)}, rep
	}
	return protocol.Result{Success: true, BytesSent: sent, BytesReceived: got}, rep
}

func toRecords(recs []*kgo.Record) []Record {
	out := make([]Record, len(recs))
	for i, r := range recs {
		out[i] = Record{Topic: r.Topic, Partition: r.Partition, Offset: r.Offset, Key: r.Key, Value: r.Value}
	}
	return out
}

// explain gives an error from the client a clear text. The end of the
// run keeps its own error, and a timeout is named as one.
func (d *Driver) explain(parent, ctx context.Context, err error) error {
	hint := d.problems.recent(d.timeout + time.Second)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		if parent.Err() != nil {
			return parent.Err()
		}
		if hint != "" {
			return fmt.Errorf("kafka: no answer within -timeout (%s); the client reported: %s", d.timeout, hint)
		}
		return fmt.Errorf("kafka: no answer within -timeout (%s)", d.timeout)
	}
	if !strings.HasPrefix(err.Error(), "kafka:") {
		err = fmt.Errorf("kafka: %w", err)
	}
	// A record that "timed out" or a failed request often hides the real
	// reason, such as a refused connection or a wrong password.
	if hint != "" && strings.Contains(err.Error(), "timed out") {
		return fmt.Errorf("%w; the client reported: %s", err, hint)
	}
	return err
}

// problemLog is a kgo.Logger. It does not print anything. It keeps the last
// error the client reported at warning level or above, such as "unable to
// dial" or "SASL authentication failed", because the error that reaches
// the caller is often only "timed out".
type problemLog struct {
	mu   sync.Mutex
	text string
	at   time.Time
}

func (p *problemLog) Level() kgo.LogLevel { return kgo.LogLevelWarn }

func (p *problemLog) Log(_ kgo.LogLevel, msg string, kv ...any) {
	for i := 0; i+1 < len(kv); i += 2 {
		if k, _ := kv[i].(string); k == "err" {
			p.mu.Lock()
			p.text, p.at = fmt.Sprintf("%s: %v", msg, kv[i+1]), time.Now()
			p.mu.Unlock()
			return
		}
	}
}

// recent returns the last problem, if it happened within the last d.
func (p *problemLog) recent(d time.Duration) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.text == "" || time.Since(p.at) > d {
		return ""
	}
	return p.text
}

func (d *Driver) replaceID(s, id string) string { return strings.ReplaceAll(s, "{id}", id) }

// produce sends count records and returns them, with the delivery
// result filled in, and the bytes sent.
func (d *Driver) produce(ctx context.Context, id string) ([]*kgo.Record, int64, error) {
	topic := d.replaceID(d.topic, id)
	key := []byte(d.replaceID(d.key, id))
	value := d.target.Body
	if bytes.Contains(value, []byte("{id}")) {
		value = bytes.ReplaceAll(value, []byte("{id}"), []byte(id))
	}
	recs := make([]*kgo.Record, d.count)
	for i := range recs {
		r := &kgo.Record{Topic: topic, Value: value}
		if len(key) > 0 {
			r.Key = key
		}
		recs[i] = r
	}
	var sent int64
	for _, r := range d.shared.ProduceSync(ctx, recs...) {
		if r.Err != nil {
			return nil, sent, fmt.Errorf("kafka: producing to %s: %w", topic, r.Err)
		}
		sent += int64(len(r.Record.Key) + len(r.Record.Value))
	}
	return recs, sent, nil
}

// consume reads records and returns the bytes of their values. With want
// nil it reads count records from the start or the end of the topic.
// With want set, it reads exactly those records, which are already
// delivered, and checks that each one comes back the same.
func (d *Driver) consume(ctx context.Context, topic string, want []*kgo.Record) (int64, []Record, error) {
	opts := append([]kgo.Opt(nil), d.base...)
	opts = append(opts, kgo.FetchMaxWait(min(d.timeout, time.Second)))

	type pos struct {
		part int32
		off  int64
	}
	var pending map[pos][]byte
	need := d.count
	if want != nil {
		pending = make(map[pos][]byte, len(want))
		first := map[int32]int64{}
		for _, r := range want {
			pending[pos{r.Partition, r.Offset}] = r.Value
			if o, ok := first[r.Partition]; !ok || r.Offset < o {
				first[r.Partition] = r.Offset
			}
		}
		parts := map[int32]kgo.Offset{}
		for p, o := range first {
			parts[p] = kgo.NewOffset().At(o)
		}
		opts = append(opts, kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{topic: parts}))
		need = len(want)
	} else {
		off := kgo.NewOffset().AtStart()
		if d.fromEnd {
			off = kgo.NewOffset().AtEnd()
		}
		opts = append(opts, kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(off))
	}

	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return 0, nil, fmt.Errorf("kafka: creating the client: %w", err)
	}
	defer cl.Close()

	var got int64
	var read []Record
	for n := 0; n < need; {
		fetches := cl.PollFetches(ctx)
		if fetches.IsClientClosed() {
			return got, read, errors.New("kafka: the client closed")
		}
		if ctx.Err() != nil {
			return got, read, ctx.Err()
		}
		var firstErr error
		fetches.EachError(func(t string, p int32, err error) {
			if firstErr == nil {
				firstErr = fmt.Errorf("kafka: reading %s partition %d: %w", t, p, err)
			}
		})
		if firstErr != nil {
			return got, read, firstErr
		}
		for it := fetches.RecordIter(); !it.Done(); {
			r := it.Next()
			if want != nil {
				v, ok := pending[pos{r.Partition, r.Offset}]
				if !ok {
					continue // another record in the same batch
				}
				if !bytes.Equal(v, r.Value) {
					return got, read, fmt.Errorf("kafka: record %s[%d]@%d came back different from what was sent", topic, r.Partition, r.Offset)
				}
				delete(pending, pos{r.Partition, r.Offset})
			} else if len(d.expect) > 0 && !bytes.Contains(r.Value, d.expect) {
				return got, read, fmt.Errorf("kafka: record %s[%d]@%d did not contain %q (it starts with %q)", topic, r.Partition, r.Offset, d.expect, clip(r.Value))
			}
			got += int64(len(r.Value))
			read = append(read, Record{Topic: r.Topic, Partition: r.Partition, Offset: r.Offset, Key: r.Key, Value: r.Value})
			if n++; n >= need {
				break
			}
		}
	}
	return got, read, nil
}

// adminCall runs admin and stops waiting for it when ctx ends. The admin
// requests of the client wait for the broker's answer without watching
// ctx, so a broker that never answers would hold the call for the whole
// backstop time. The request is left to finish by itself, and it ends
// within that backstop. It uses the shared client, which stays usable.
func (d *Driver) adminCall(ctx context.Context, id string) (int64, string, error) {
	type out struct {
		n    int64
		text string
		err  error
	}
	ch := make(chan out, 1)
	go func() {
		n, text, err := d.admin(ctx, id)
		ch <- out{n, text, err}
	}()
	select {
	case o := <-ch:
		return o.n, o.text, o.err
	case <-ctx.Done():
		return 0, "", ctx.Err()
	}
}

// admin does one admin action and returns the bytes of its answer.
func (d *Driver) admin(ctx context.Context, id string) (int64, string, error) {
	topic := d.replaceID(d.topic, id)
	switch d.action {
	case actCreateTopic:
		return 0, "", d.createTopic(ctx, topic)
	case actDeleteTopic:
		return 0, "", d.deleteTopic(ctx, topic)
	case actTopicLifecycle:
		if err := d.createTopic(ctx, topic); err != nil {
			return 0, "", err
		}
		return 0, "", d.deleteTopic(ctx, topic)
	case actListTopics:
		ts, err := d.adm.ListTopics(ctx)
		if err != nil {
			return 0, "", fmt.Errorf("kafka: listing topics: %w", err)
		}
		names := ts.Names()
		sort.Strings(names)
		return d.listing(strings.Join(names, "\n"))
	case actListGroups:
		gs, err := d.adm.ListGroups(ctx)
		if err != nil {
			return 0, "", fmt.Errorf("kafka: listing groups: %w", err)
		}
		names := make([]string, 0, len(gs))
		for _, g := range gs.Sorted() {
			names = append(names, g.Group)
		}
		return d.listing(strings.Join(names, "\n"))
	case actDescribe:
		m, err := d.adm.BrokerMetadata(ctx)
		if err != nil {
			return 0, "", fmt.Errorf("kafka: describing the cluster: %w", err)
		}
		return d.listing(fmt.Sprintf("cluster %q, controller %d, %d brokers", m.Cluster, m.Controller, len(m.Brokers)))
	}
	return 0, "", fmt.Errorf("kafka: unknown action %q", d.action)
}

// listing checks the optional expect text and returns the size of text.
func (d *Driver) listing(text string) (int64, string, error) {
	if len(d.expect) > 0 && !strings.Contains(text, string(d.expect)) {
		return int64(len(text)), text, fmt.Errorf("kafka: the answer did not contain %q", d.expect)
	}
	return int64(len(text)), text, nil
}

func (d *Driver) createTopic(ctx context.Context, topic string) error {
	if _, err := d.adm.CreateTopic(ctx, d.parts, d.repl, nil, topic); err != nil {
		return fmt.Errorf("kafka: creating topic %s: %w", topic, err)
	}
	return nil
}

func (d *Driver) deleteTopic(ctx context.Context, topic string) error {
	rs, err := d.adm.DeleteTopics(ctx, topic)
	if err != nil {
		return fmt.Errorf("kafka: deleting topic %s: %w", topic, err)
	}
	if err := rs.Error(); err != nil {
		return fmt.Errorf("kafka: deleting topic %s: %w", topic, err)
	}
	return nil
}

func clip(b []byte) string {
	if len(b) > 40 {
		return string(b[:40]) + "..."
	}
	return string(b)
}
