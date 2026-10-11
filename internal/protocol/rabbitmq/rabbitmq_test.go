package rabbitmq

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/protocol/rabbitmq/rabbitmqtest"
)

func declareSink(t *testing.T, s *rabbitmqtest.Server) error {
	t.Helper()
	conn, err := amqp.Dial("amqp://guest:guest@" + s.Addr() + "/?heartbeat=0")
	if err != nil {
		return err
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	if _, err := ch.QueueDeclare("sink", false, false, false, false, nil); err != nil {
		return err
	}
	return ch.QueueBind("sink", "lost.#", "amq.topic", false, nil)
}

func deleteSink(t *testing.T, s *rabbitmqtest.Server) error {
	t.Helper()
	conn, err := amqp.Dial("amqp://guest:guest@" + s.Addr() + "/?heartbeat=0")
	if err != nil {
		return err
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	_, err = ch.QueueDelete("sink", false, false, false)
	return err
}

func mustDriver(t *testing.T, raw, body string, opts map[string]string, timeout time.Duration) *Driver {
	t.Helper()
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	d, err := New(protocol.Target{URL: raw, Body: []byte(body), Options: opts}, timeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestVhostFromPath(t *testing.T) {
	cases := []struct {
		path, want string
		given      bool
	}{
		{"", "/", false},
		{"/", "/", false},
		{"/orders", "orders", true},
		{"/%2F", "/", true},
		{"/a%2Fb", "a/b", true},
	}
	for _, c := range cases {
		got, given, err := vhostFromPath(c.path)
		if err != nil || got != c.want || given != c.given {
			t.Errorf("%q = %q given=%v %v", c.path, got, given, err)
		}
	}
}

func TestNew_DoesNotConnect(t *testing.T) {
	s := rabbitmqtest.Start(t)
	d := mustDriver(t, s.URL(), "hi", nil, 0)
	if s.ConnCount() != 0 {
		t.Fatal("New connected")
	}
	_ = d
}

func TestOptions_Rejected(t *testing.T) {
	s := rabbitmqtest.Start(t)
	secret := "hunter2-secret"
	cases := []struct {
		url, body string
		opts      map[string]string
		want      string
		hidden    string
	}{
		{url: "http://127.0.0.1:1", want: "unsupported scheme"},
		{url: "amqp://", want: "no host"},
		{url: "amqp://u:" + secret + "@127.0.0.1:1", want: "password", hidden: secret},
		{url: "amqp://127.0.0.1:1?heartbeat=10&token=" + secret, want: "heartbeat", hidden: secret},
		{url: "amqp://alice@127.0.0.1:1", opts: map[string]string{"username": "bob", "password_env": "X"}, want: "differ"},
		{url: s.URL() + "/orders", opts: map[string]string{"vhost": "other"}, want: "differ"},
		{url: s.URL(), opts: map[string]string{"count": "0"}, want: "count"},
		{url: s.URL(), opts: map[string]string{"count": "10001"}, want: "count"},
		{url: s.URL(), opts: map[string]string{"mode": "consume", "queue": "q", "prefetch": "0"}, want: "prefetch"},
		{url: s.URL(), opts: map[string]string{"channels": "0"}, want: "channels"},
		{url: s.URL(), opts: map[string]string{"heartbeat": "1ms"}, want: "heartbeat"},
		{url: s.URL(), opts: map[string]string{"priority": "256"}, want: "priority"},
		{url: s.URL(), opts: map[string]string{"expiration": "0s"}, want: "expiration"},
		{url: s.URL(), opts: map[string]string{"headers": `{"a":{"b":1}}`}, want: "headers"},
		{url: s.URL(), opts: map[string]string{"headers": "[]"}, want: "headers"},
		{url: s.URL(), opts: map[string]string{"queue": "q"}, want: "use routing_key"},
		{url: s.URL(), opts: map[string]string{"mode": "roundtrip", "bind_key": "a.*"}, want: "named exchange"},
		{url: s.URL(), opts: map[string]string{"mode": "roundtrip", "routing_key": "a"}, want: "default exchange"},
		{url: s.URL(), opts: map[string]string{"confirm": "false", "mandatory": "true"}, want: "confirm=true"},
		{url: s.URL(), opts: map[string]string{"allow_admin": "true"}, want: "allow_writes"},
		{url: s.URL(), opts: map[string]string{"username": "alice"}, want: "password_env"},
		{url: s.URL(), opts: map[string]string{"mode": "admin", "action": "queue_lifecycle", "queue": "plain", "allow_writes": "true"}, want: "{id}"},
		{url: s.URL(), opts: map[string]string{"exchange_type": "headers", "mode": "admin", "action": "exchange_declare", "exchange": "e", "allow_writes": "true"}, want: "headers"},
		{url: "amqps://127.0.0.1:1", opts: map[string]string{"tls": "false"}, want: "amqps"},
		{url: "amqp://alice:hunter2-secret@[::1", want: "parsing target URL", hidden: "hunter2-secret"},
	}
	for _, c := range cases {
		body := c.body
		if body == "" {
			body = "x"
		}
		_, err := New(protocol.Target{URL: c.url, Body: []byte(body), Options: c.opts}, time.Second)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s opts %v: %v", c.url, c.opts, err)
		}
		if c.hidden != "" && err != nil && strings.Contains(err.Error(), c.hidden) {
			t.Errorf("error contains secret: %v", err)
		}
	}
	if _, err := NewConn(protocol.Target{URL: s.URL(), Options: map[string]string{"username": "alice"}}, time.Second, nil); err == nil || !strings.Contains(err.Error(), "pass password: env.NAME") {
		t.Fatalf("script password hint: %v", err)
	}
	big := make([]byte, maxBody+1)
	if _, err := New(protocol.Target{URL: s.URL(), Body: big}, time.Second); err == nil || !strings.Contains(err.Error(), "16777216") {
		t.Fatalf("body cap: %v", err)
	}
}

func TestPublish_PropertiesAndBatch(t *testing.T) {
	s := rabbitmqtest.Start(t)
	d := mustDriver(t, s.URL(), "a-{id}-{n}", map[string]string{
		"routing_key": "k-{id}-{n}", "exchange": "amq.topic", "mandatory": "false",
		"count": "3", "persistent": "true", "content_type": "text/plain",
		"priority": "4", "expiration": "30s", "headers": `{"s":"v","n":7,"ok":true}`,
	}, 0)
	res, _ := d.Run(context.Background())
	if !res.Success {
		t.Fatal(res.Err)
	}
	pubs := s.Published()
	if len(pubs) != 3 {
		t.Fatalf("published %d", len(pubs))
	}
	if pubs[0].DeliveryMode != 2 || pubs[0].ContentType != "text/plain" || pubs[0].Priority != 4 || pubs[0].Expiration != "30000" {
		t.Fatalf("props %+v", pubs[0])
	}
	if pubs[0].Headers["s"] != "v" || pubs[0].Headers["n"] != int32(7) || pubs[0].Headers["ok"] != true {
		t.Fatalf("headers %#v", pubs[0].Headers)
	}
	if !strings.Contains(string(pubs[0].Body), pubs[0].MessageID) {
		t.Fatalf("body %q id %q", pubs[0].Body, pubs[0].MessageID)
	}
	if !strings.HasSuffix(string(pubs[2].Body), "-3") || !strings.HasSuffix(pubs[2].RoutingKey, "-3") {
		t.Fatalf("n replacement %q %q", pubs[2].Body, pubs[2].RoutingKey)
	}
	ids := map[string]bool{}
	for _, p := range pubs {
		if ids[p.MessageID] || p.Timestamp.IsZero() {
			t.Fatalf("id %q", p.MessageID)
		}
		ids[p.MessageID] = true
	}
	if res.BytesSent != int64(len(pubs[0].Body)+len(pubs[1].Body)+len(pubs[2].Body)) {
		t.Fatalf("bytes %d", res.BytesSent)
	}
}

func TestPublish_NotRoutedAndConfirmFalse(t *testing.T) {
	s := rabbitmqtest.Start(t)
	d := mustDriver(t, s.URL(), "x", map[string]string{"routing_key": "missing"}, time.Second)
	start := time.Now()
	res, _ := d.Run(context.Background())
	if res.Success || !strings.Contains(res.Err.Error(), "not routed") || !strings.Contains(res.Err.Error(), "mandatory=false") {
		t.Fatal(res.Err)
	}
	if time.Since(start) > 800*time.Millisecond {
		t.Fatal("not routed waited for the timeout")
	}
	d2 := mustDriver(t, s.URL(), "x", map[string]string{"routing_key": "missing", "mandatory": "false"}, time.Second)
	if res, _ = d2.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
	s.StallOn("basic.ack")
	d3 := mustDriver(t, s.URL(), "x", map[string]string{"confirm": "false", "routing_key": "nowhere"}, 2*time.Second)
	start = time.Now()
	if res, _ = d3.Run(context.Background()); !res.Success || time.Since(start) > time.Second {
		t.Fatalf("confirm=false %v after %s", res.Err, time.Since(start))
	}
}

func TestPublish_NackAndLargeBody(t *testing.T) {
	s := rabbitmqtest.Start(t)
	s.NackPublishes(true)
	d := mustDriver(t, s.URL(), "x", map[string]string{"mandatory": "false"}, time.Second)
	res, _ := d.Run(context.Background())
	if res.Success || !strings.Contains(res.Err.Error(), "nack") {
		t.Fatal(res.Err)
	}
	s.NackPublishes(false)
	body := strings.Repeat("ab", 150*1024)
	d2 := mustDriver(t, s.URL(), body, map[string]string{"mandatory": "false"}, 3*time.Second)
	res, _ = d2.Run(context.Background())
	if !res.Success {
		t.Fatal(res.Err)
	}
	if got := s.Published(); len(got) < 2 || string(got[len(got)-1].Body) != body {
		t.Fatalf("large body len %d", len(got[len(got)-1].Body))
	}
}

func TestConsume_AckExpectPrefetch(t *testing.T) {
	s := rabbitmqtest.Start(t)
	pub := mustDriver(t, s.URL(), "", map[string]string{"mode": "admin", "action": "queue_declare", "queue": "q", "allow_writes": "true"}, 0)
	if res, _ := pub.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
	send := mustDriver(t, s.URL(), "hello", map[string]string{"routing_key": "q", "count": "4"}, 0)
	if res, _ := send.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
	c := mustDriver(t, s.URL(), "", map[string]string{"mode": "consume", "queue": "q", "count": "2", "prefetch": "2", "expect": "hello"}, 0)
	res, rep := c.Run(context.Background())
	if !res.Success || len(rep.Messages) != 2 || rep.Messages[0].Redelivered {
		t.Fatalf("%v %+v", res.Err, rep.Messages)
	}
	if s.Depth("q") != 4 {
		t.Fatalf("requeue depth %d", s.Depth("q"))
	}
	if s.PeakUnacked("q") > 2 {
		t.Fatalf("peak unacked %d", s.PeakUnacked("q"))
	}
	c2 := mustDriver(t, s.URL(), "", map[string]string{"mode": "consume", "queue": "q", "count": "1"}, 0)
	_, rep = c2.Run(context.Background())
	if len(rep.Messages) != 1 || !rep.Messages[0].Redelivered {
		t.Fatalf("second read %+v", rep.Messages)
	}
	bad := mustDriver(t, s.URL(), "", map[string]string{"mode": "consume", "queue": "q", "expect": "nope"}, time.Second)
	res, _ = bad.Run(context.Background())
	if res.Success || s.Depth("q") != 4 {
		t.Fatalf("expect fail depth %d %v", s.Depth("q"), res.Err)
	}
	ack := mustDriver(t, s.URL(), "", map[string]string{"mode": "consume", "queue": "q", "count": "4", "ack": "ack", "allow_writes": "true"}, 0)
	res, rep = ack.Run(context.Background())
	if !res.Success || len(rep.Messages) != 4 || s.Depth("q") != 0 {
		t.Fatalf("ack %v depth %d n %d", res.Err, s.Depth("q"), len(rep.Messages))
	}
}

func TestConsume_MissingAndEmpty(t *testing.T) {
	s := rabbitmqtest.Start(t)
	d := mustDriver(t, s.URL(), "", map[string]string{"mode": "consume", "queue": "absent"}, time.Second)
	res, _ := d.Run(context.Background())
	if res.Success || !strings.Contains(res.Err.Error(), "NOT_FOUND") {
		t.Fatal(res.Err)
	}
	info := mustDriver(t, s.URL(), "", map[string]string{"mode": "admin", "action": "queue_declare", "queue": "q", "allow_writes": "true"}, 0)
	if res, _ = info.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
	empty := mustDriver(t, s.URL(), "", map[string]string{"mode": "consume", "queue": "q"}, 300*time.Millisecond)
	start := time.Now()
	res, _ = empty.Run(context.Background())
	if res.Success || !strings.Contains(res.Err.Error(), "timed out after") || time.Since(start) > time.Second {
		t.Fatalf("%v after %s", res.Err, time.Since(start))
	}
}

func TestRoundtrip_TopicDefaultAndLeaks(t *testing.T) {
	s := rabbitmqtest.Start(t)
	d := mustDriver(t, s.URL(), "ping-{id}", map[string]string{
		"mode": "roundtrip", "exchange": "amq.topic", "routing_key": "orders.{id}", "bind_key": "orders.*", "count": "5",
	}, 0)
	res, rep := d.Run(context.Background())
	if !res.Success || len(rep.Messages) != 5 {
		t.Fatalf("%v n=%d", res.Err, len(rep.Messages))
	}
	def := mustDriver(t, s.URL(), "via-default", map[string]string{"mode": "roundtrip"}, 0)
	res, rep = def.Run(context.Background())
	if !res.Success || len(rep.Messages) != 1 || rep.Messages[0].Body != "via-default" {
		t.Fatalf("default %v %+v", res.Err, rep.Messages)
	}
	miss := mustDriver(t, s.URL(), "x", map[string]string{
		"mode": "roundtrip", "exchange": "amq.topic", "routing_key": "no.match", "bind_key": "other.*",
	}, 2*time.Second)
	start := time.Now()
	res, _ = miss.Run(context.Background())
	if res.Success || !strings.Contains(res.Err.Error(), "not routed") || time.Since(start) > 800*time.Millisecond {
		t.Fatalf("%v after %s", res.Err, time.Since(start))
	}
	if err := declareSink(t, s); err != nil {
		t.Fatal(err)
	}
	parent, err := NewConn(protocol.Target{URL: s.URL()}, 2*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	stop := make(chan struct{})
	defer close(stop)
	pushed := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, name := range s.Queues() {
				if strings.HasPrefix(name, "amq.gen-") && s.Push(name, "foreign-id", "nope") {
					select {
					case pushed <- struct{}{}:
					default:
					}
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()
	for i := 0; i < 100; i++ {
		opts := map[string]string{
			"mode": "roundtrip", "exchange": "amq.topic",
			"routing_key": "orders.{id}", "bind_key": "orders.*",
		}
		timeout := 2 * time.Second
		want := ""
		switch i {
		case 3:
			opts["routing_key"] = "no.match"
			opts["bind_key"] = "other.*"
			want = "not routed"
		case 7:
			opts["routing_key"] = "lost.x"
			opts["bind_key"] = "mine.*"
			timeout = 400 * time.Millisecond
			want = "timed out"
		}
		c, err := parent.Call(opts, []byte("ping-{id}"), timeout)
		if err != nil {
			t.Fatal(err)
		}
		res, rep := c.Run(context.Background())
		if want == "" {
			if !res.Success {
				t.Fatalf("roundtrip %d: %v (connection close: %q)", i, res.Err, parent.link.closeReason())
			}
			for _, m := range rep.Messages {
				if m.Body == "nope" || m.MessageID == "foreign-id" {
					t.Fatalf("roundtrip kept a foreign message: %+v", m)
				}
			}
			continue
		}
		if res.Success || !strings.Contains(res.Err.Error(), want) {
			t.Fatalf("roundtrip %d: %v (connection close: %q)", i, res.Err, parent.link.closeReason())
		}
	}
	select {
	case <-pushed:
	default:
		t.Fatal("no foreign message was delivered to a roundtrip queue")
	}
	if err := deleteSink(t, s); err != nil {
		t.Fatal(err)
	}
	if q := s.Queues(); len(q) != 0 {
		t.Fatalf("leftover queues %v", q)
	}
}

// TestRun_WaitsForTheWatchdogBeforeTheNextCall is the timeout in
// TestRoundtrip_TopicDefaultAndLeaks. That call returns as soon as its
// budget ends, while the watchdog may still be closing the shared
// connection. The next call then fails with "channel/connection is not
// open". Run has to wait until the watchdog finishes.
func TestRun_WaitsForTheWatchdogBeforeTheNextCall(t *testing.T) {
	s := rabbitmqtest.Start(t)
	if err := declareSink(t, s); err != nil {
		t.Fatal(err)
	}
	parent, err := NewConn(protocol.Target{URL: s.URL()}, 2*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Close() })

	warm, err := parent.Call(map[string]string{"mode": "roundtrip"}, []byte("ping-{id}"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := warm.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}

	holdCh := make(chan struct{})
	holdConn := make(chan struct{})
	entered := make(chan struct{})
	closeCallMu.Lock()
	origCh, origConn := closeCallChannel, closeCallConn
	closeCallChannel = func(ch *amqp.Channel) error {
		<-holdCh
		return origCh(ch)
	}
	closeCallConn = func(c *amqp.Connection) error {
		close(entered)
		<-holdConn
		return origConn(c)
	}
	closeCallMu.Unlock()
	var releaseCh, releaseConn sync.Once
	release := func() {
		releaseCh.Do(func() { close(holdCh) })
		releaseConn.Do(func() { close(holdConn) })
	}
	t.Cleanup(func() {
		release()
		closeCallMu.Lock()
		closeCallChannel, closeCallConn = origCh, origConn
		closeCallMu.Unlock()
	})

	first, err := parent.Call(map[string]string{
		"mode": "roundtrip", "exchange": "amq.topic",
		"routing_key": "lost.x", "bind_key": "mine.*",
	}, []byte("ping-{id}"), 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan protocol.Result, 1)
	go func() {
		res, _ := first.Run(context.Background())
		firstDone <- res
	}()

	select {
	case <-entered:
	case res := <-firstDone:
		t.Fatalf("call returned before the watchdog closed the connection: %v (connection close: %q)", res.Err, parent.link.closeReason())
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog did not close the connection")
	}
	select {
	case res := <-firstDone:
		t.Fatalf("Run returned while the watchdog was still closing the connection: %v (connection close: %q)", res.Err, parent.link.closeReason())
	default:
	}

	releaseConn.Do(func() { close(holdConn) })
	var res protocol.Result
	select {
	case res = <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the watchdog finished")
	}
	if res.Success || !strings.Contains(res.Err.Error(), "timed out") {
		t.Fatalf("timed out call: %v (connection close: %q)", res.Err, parent.link.closeReason())
	}
	conns := s.ConnCount()
	releaseCh.Do(func() { close(holdCh) })

	next, err := parent.Call(map[string]string{"mode": "roundtrip"}, []byte("ping-{id}"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	res, _ = next.Run(context.Background())
	if !res.Success {
		t.Fatalf("next call: %v (connection close: %q)", res.Err, parent.link.closeReason())
	}
	if s.ConnCount() <= conns {
		t.Fatalf("next call reused the connection the watchdog closed (%d)", s.ConnCount())
	}
}

func TestAdmin_ActionsAndFlags(t *testing.T) {
	s := rabbitmqtest.Start(t)
	mk := func(opts map[string]string) *Driver {
		t.Helper()
		return mustDriver(t, s.URL(), "", opts, time.Second)
	}
	res, rep := mk(map[string]string{"mode": "admin", "action": "queue_declare", "queue": "q", "allow_writes": "true"}).Run(context.Background())
	if !res.Success || !strings.Contains(rep.Text, "queue=q") {
		t.Fatalf("%v %q", res.Err, rep.Text)
	}
	res, rep = mk(map[string]string{"mode": "admin", "action": "queue_info", "queue": "q"}).Run(context.Background())
	if !res.Success || !strings.Contains(rep.Text, "messages=0") {
		t.Fatalf("info %v %q", res.Err, rep.Text)
	}
	res, _ = mk(map[string]string{"mode": "admin", "action": "queue_declare", "queue": "q", "durable": "false", "allow_writes": "true"}).Run(context.Background())
	if res.Success || !strings.Contains(res.Err.Error(), "PRECONDITION_FAILED") {
		t.Fatal(res.Err)
	}
	before := s.ConnCount()
	res, _ = mk(map[string]string{"mode": "admin", "action": "queue_delete", "queue": "q"}).Run(context.Background())
	if res.Success || !strings.Contains(res.Err.Error(), "allow_admin") || s.ConnCount() != before {
		t.Fatalf("%v conns %d", res.Err, s.ConnCount())
	}
	res, _ = mk(map[string]string{"mode": "admin", "action": "queue_purge", "queue": "q", "allow_writes": "true"}).Run(context.Background())
	if res.Success || !strings.Contains(res.Err.Error(), "allow_admin") {
		t.Fatal(res.Err)
	}
	res, _ = mk(map[string]string{"mode": "admin", "action": "queue_delete", "queue": "q", "allow_writes": "true", "allow_admin": "true"}).Run(context.Background())
	if !res.Success {
		t.Fatal(res.Err)
	}
	res, rep = mk(map[string]string{"mode": "admin", "action": "queue_lifecycle", "queue": "tmp-{id}", "allow_writes": "true"}).Run(context.Background())
	if !res.Success || s.Depth("tmp") != 0 && len(s.Queues()) != 0 {
		t.Fatalf("%v %q queues %v", res.Err, rep.Text, s.Queues())
	}
	res, _ = mk(map[string]string{"mode": "admin", "action": "exchange_declare", "exchange": "ex", "exchange_type": "topic", "allow_writes": "true"}).Run(context.Background())
	if !res.Success {
		t.Fatal(res.Err)
	}
	res, _ = mk(map[string]string{"mode": "admin", "action": "exchange_delete", "exchange": "ex", "allow_writes": "true", "allow_admin": "true"}).Run(context.Background())
	if !res.Success {
		t.Fatal(res.Err)
	}
}

func TestSafety_BeforeTraffic(t *testing.T) {
	s := rabbitmqtest.Start(t)
	d := mustDriver(t, s.URL(), "", map[string]string{"mode": "consume", "queue": "q", "ack": "ack"}, 0)
	res, err := d.Do(context.Background())
	if err != nil || res.Success || !strings.Contains(res.Err.Error(), "use -opt allow_writes=true") || s.ConnCount() != 0 {
		t.Fatalf("err %v res %v conns %d", err, res.Err, s.ConnCount())
	}
	sc, err := NewConn(protocol.Target{URL: s.URL(), Options: map[string]string{
		"allow_writes": "true", "allow_admin": "true", "connection_name": "kept",
		"heartbeat": "5s", "channels": "3", "vhost": "/",
	}}, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := sc.Call(map[string]string{"mode": "consume", "queue": "q", "ack": "ack"}, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !c.allowWrites || !c.allowAdmin || !c.script || c.password != sc.password || c.link != sc.link || c.vhost != sc.vhost || c.connName != "kept" || c.heartbeat != 5*time.Second || c.channels != 3 || c.perCall != sc.perCall || c.useTLS != sc.useTLS || c.skipVerify != sc.skipVerify || c.username != sc.username {
		t.Fatalf("call dropped a connection flag: writes %v admin %v name %q", c.allowWrites, c.allowAdmin, c.connName)
	}
	// The parent was created with allow_writes, so this call is allowed to
	// remove messages. A parent without the flag must say how to set it.
	plain, err := NewConn(protocol.Target{URL: s.URL()}, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	denied, err := plain.Call(map[string]string{"mode": "consume", "queue": "q", "ack": "ack"}, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	res, _ = denied.Run(context.Background())
	if res.Success || !strings.Contains(res.Err.Error(), "pass allow_writes: true") {
		t.Fatal(res.Err)
	}
}

func TestConnection_SharedPerCallAndNames(t *testing.T) {
	s := rabbitmqtest.Start(t)
	d := mustDriver(t, s.URL()+"/orders", "x", map[string]string{
		"mandatory": "false", "connection_name": "vl-test", "heartbeat": "5s", "vhost": "orders",
	}, 0)
	s.Apply(rabbitmqtest.Config{Vhosts: []string{"/", "orders"}})
	var wg sync.WaitGroup
	errc := make(chan error, 16)
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, _ := d.Run(context.Background())
			if res.Err != nil {
				errc <- res.Err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
	if s.ConnCount() != 1 {
		t.Fatalf("connections %d", s.ConnCount())
	}
	info := s.Connections()
	if len(info) != 1 || info[0].Name != "vl-test" || info[0].Vhost != "orders" || info[0].Heartbeat != 5 {
		t.Fatalf("conn %+v", info)
	}
	p := mustDriver(t, s.URL(), "x", map[string]string{"connection": "per_call", "mandatory": "false"}, 0)
	for i := 0; i < 2; i++ {
		if res, _ := p.Run(context.Background()); !res.Success {
			t.Fatal(res.Err)
		}
	}
	if s.ConnCount() != 3 || s.OpenConns() != 1 {
		t.Fatalf("accepted %d open %d", s.ConnCount(), s.OpenConns())
	}
}

func TestNoRetry_AndChannelDeath(t *testing.T) {
	s := rabbitmqtest.Start(t)
	d := mustDriver(t, s.URL(), "x", map[string]string{"mandatory": "false"}, time.Second)
	s.DropConnectionOn("basic.publish")
	res, err := d.Do(context.Background())
	if err != nil || res.Success {
		t.Fatalf("drop err %v res %v", err, res.Err)
	}
	n := 0
	for _, m := range s.Methods() {
		if strings.HasSuffix(m, "basic.publish") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("publish seen %d times", n)
	}
	s.DropConnectionOn("")
	if res, _ = d.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
	s.CloseChannelOn("basic.publish", 406, "PRECONDITION_FAILED - test")
	res, _ = d.Run(context.Background())
	if res.Success || !strings.Contains(res.Err.Error(), "PRECONDITION_FAILED") {
		t.Fatal(res.Err)
	}
	s.CloseChannelOn("", 0, "")
	if res, _ = d.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
}

func TestStall_OpenAndConfirmEndAtTheTimeout(t *testing.T) {
	// admin and consume never send confirm.select, so a stall on that
	// method cannot fire in those modes.
	cases := []struct {
		name  string
		opts  map[string]string
		stall string
	}{
		{"publish channel.open", map[string]string{"mandatory": "false"}, "channel.open"},
		{"publish confirm.select", map[string]string{"mandatory": "false"}, "confirm.select"},
		{"roundtrip channel.open", map[string]string{"mode": "roundtrip"}, "channel.open"},
		{"roundtrip confirm.select", map[string]string{"mode": "roundtrip"}, "confirm.select"},
		{"admin channel.open", map[string]string{"mode": "admin", "action": "queue_declare", "queue": "q", "allow_writes": "true"}, "channel.open"},
		{"consume channel.open", map[string]string{"mode": "consume", "queue": "q"}, "channel.open"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := rabbitmqtest.Start(t)
			s.StallOn(tc.stall)
			d := mustDriver(t, s.URL(), "x", tc.opts, 400*time.Millisecond)
			start := time.Now()
			res, err := d.Do(context.Background())
			elapsed := time.Since(start)
			if err != nil || res.Success || !strings.Contains(res.Err.Error(), "timed out after 400ms") || elapsed > 2*time.Second {
				t.Fatalf("err %v res %v after %s", err, res.Err, elapsed)
			}
		})
	}
}

func TestPublish_StalledAckIsNotReused(t *testing.T) {
	s := rabbitmqtest.Start(t)
	s.StallOn("basic.ack")
	d := mustDriver(t, s.URL(), "x", map[string]string{"mandatory": "false"}, 400*time.Millisecond)
	start := time.Now()
	res, err := d.Do(context.Background())
	if err != nil || res.Success || !strings.Contains(res.Err.Error(), "timed out after 400ms") || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v res %v after %s", err, res.Err, time.Since(start))
	}
	s.StallOn("")
	for i := 0; i < 5; i++ {
		res, _ = d.Run(context.Background())
		if !res.Success {
			t.Fatalf("call %d: %v", i+1, res.Err)
		}
	}
}

func TestTimeouts_StallCancelAndUnreachable(t *testing.T) {
	s := rabbitmqtest.Start(t)
	before := runtime.NumGoroutine()
	s.StallOn("queue.declare")
	d := mustDriver(t, s.URL(), "", map[string]string{
		"mode": "admin", "action": "queue_declare", "queue": "q", "allow_writes": "true",
	}, 300*time.Millisecond)
	start := time.Now()
	res, err := d.Do(context.Background())
	if err != nil || res.Success || !strings.Contains(res.Err.Error(), "timed out after 300ms") || time.Since(start) > time.Second {
		t.Fatalf("err %v res %v after %s", err, res.Err, time.Since(start))
	}
	s.StallOn("")
	if res, _ = d.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
	_ = d.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > before+4 {
		time.Sleep(20 * time.Millisecond)
		runtime.Gosched()
	}
	if runtime.NumGoroutine() > before+4 {
		t.Fatalf("goroutines %d, before %d", runtime.NumGoroutine(), before)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d2 := mustDriver(t, s.URL(), "x", map[string]string{"mandatory": "false"}, time.Second)
	res, err = d2.Do(ctx)
	if err != nil || !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("cancel err %v res %v", err, res.Err)
	}
	miss := mustDriver(t, "amqp://127.0.0.1:1", "x", map[string]string{"mandatory": "false"}, 300*time.Millisecond)
	res, err = miss.Do(context.Background())
	if err != nil || res.Success || !strings.Contains(res.Err.Error(), "could not connect") && !strings.Contains(res.Err.Error(), "timed out") {
		t.Fatalf("unreachable err %v res %v", err, res.Err)
	}

	s.StallOn("basic.consume")
	cons := mustDriver(t, s.URL(), "", map[string]string{"mode": "consume", "queue": "q"}, 300*time.Millisecond)
	res, err = cons.Do(context.Background())
	if err != nil || res.Success || !strings.Contains(res.Err.Error(), "timed out after 300ms") {
		t.Fatalf("consume stall err %v res %v", err, res.Err)
	}
	s.StallOn("")
	_ = cons.Close()
	if res, _ = mustDriver(t, s.URL(), "", map[string]string{"mode": "admin", "action": "queue_info", "queue": "q"}, time.Second).Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}

	s.StallOn("basic.ack")
	pub := mustDriver(t, s.URL(), "x", map[string]string{"mandatory": "false"}, 300*time.Millisecond)
	res, err = pub.Do(context.Background())
	if err != nil || res.Success || !strings.Contains(res.Err.Error(), "timed out after 300ms") {
		t.Fatalf("confirm stall err %v res %v", err, res.Err)
	}
	s.StallOn("")
	_ = pub.Close()
	if res, _ = mustDriver(t, s.URL(), "x", map[string]string{"mandatory": "false"}, time.Second).Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
}

func TestPool_ChannelsOne(t *testing.T) {
	s := rabbitmqtest.Start(t)
	prep := mustDriver(t, s.URL(), "", map[string]string{"mode": "admin", "action": "queue_declare", "queue": "q", "allow_writes": "true"}, 0)
	if res, _ := prep.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
	send := mustDriver(t, s.URL(), "m", map[string]string{"routing_key": "q"}, 0)
	if res, _ := send.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
	modes := []map[string]string{
		{"mandatory": "false", "channels": "1"},
		{"mode": "consume", "queue": "q", "channels": "1"},
		{"mode": "roundtrip", "channels": "1"},
		{"mode": "admin", "action": "queue_info", "queue": "q", "channels": "1"},
	}
	for _, opts := range modes {
		d := mustDriver(t, s.URL(), "m", opts, 2*time.Second)
		done := make(chan struct{})
		go func() {
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					res, _ := d.Run(context.Background())
					if res.Err != nil {
						t.Errorf("%v: %v", opts["mode"], res.Err)
					}
				}()
			}
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("deadlock in mode %q", opts["mode"])
		}
	}
}

func TestLogin_PasswordEnvAndURLUser(t *testing.T) {
	s := rabbitmqtest.Start(t)
	secret := "s3cret-value"
	t.Setenv("VL_RMQ_PW", secret)
	s.Apply(rabbitmqtest.Config{Users: map[string]string{"alice": secret, "guest": "guest"}})
	d := mustDriver(t, s.URL(), "x", map[string]string{"username": "alice", "password_env": "VL_RMQ_PW", "mandatory": "false"}, 0)
	if res, _ := d.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
	bad := mustDriver(t, s.URL(), "x", map[string]string{"username": "alice", "password_env": "VL_RMQ_PW"}, 0)
	t.Setenv("VL_RMQ_PW", "wrong-"+secret)
	// The driver already read the env at New. Build another.
	b, err := New(protocol.Target{URL: s.URL(), Body: []byte("x"), Options: map[string]string{"username": "alice", "password_env": "VL_RMQ_PW", "mandatory": "false"}}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.Do(context.Background())
	if err != nil || res.Success || !strings.Contains(res.Err.Error(), "refused the login") || strings.Contains(res.Err.Error(), secret) || strings.Contains(res.Err.Error(), "wrong-") {
		t.Fatalf("err %v res %v", err, res.Err)
	}
	_ = bad
	fromURL := mustDriver(t, "amqp://alice@"+s.Addr(), "x", map[string]string{"password_env": "VL_RMQ_PW", "mandatory": "false"}, 0)
	t.Setenv("VL_RMQ_PW", secret)
	fromURL, err = New(protocol.Target{URL: "amqp://alice@" + s.Addr(), Body: []byte("x"), Options: map[string]string{"password_env": "VL_RMQ_PW", "mandatory": "false"}}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res, _ = fromURL.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
	pw := secret
	sc, err := NewConn(protocol.Target{URL: s.URL(), Options: map[string]string{"username": "alice"}}, time.Second, &pw)
	if err != nil {
		t.Fatal(err)
	}
	c, err := sc.Call(map[string]string{"mandatory": "false"}, []byte("x"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res, _ = c.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
}

func TestTLS(t *testing.T) {
	s := rabbitmqtest.StartTLS(t)
	d, err := New(protocol.Target{URL: s.URL(), Body: []byte("x"), Options: map[string]string{"mandatory": "false"}}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Do(context.Background())
	if err != nil || res.Success {
		t.Fatalf("verify err %v res %v", err, res.Err)
	}
	ok := mustDriver(t, s.URL(), "x", map[string]string{"tls": "skip-verify", "mandatory": "false"}, 0)
	if res, _ = ok.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
	ins := mustDriver(t, s.URL(), "x", map[string]string{"mandatory": "false"}, 0)
	ins.insecure = true
	ins.skipVerify = true
	// -insecure is applied at New. Build it that way.
	ins, err = New(protocol.Target{URL: s.URL(), Body: []byte("x"), Options: map[string]string{"mandatory": "false"}, InsecureSkipVerify: true}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res, _ = ins.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
}

func TestBlockedConnection(t *testing.T) {
	s := rabbitmqtest.Start(t)
	d := mustDriver(t, s.URL(), "x", map[string]string{"mandatory": "false"}, time.Second)
	if res, _ := d.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
	s.Block("low on disk")
	time.Sleep(50 * time.Millisecond)
	s.StallOn("basic.ack")
	d2 := mustDriver(t, s.URL(), "x", map[string]string{"mandatory": "false"}, 300*time.Millisecond)
	// Use the same connection so the blocked flag is on that link.
	res, _ := d.Run(context.Background())
	if res.Success || !strings.Contains(res.Err.Error(), "blocked this connection") || !strings.Contains(res.Err.Error(), "low on disk") {
		t.Fatal(res.Err)
	}
	_ = d2
}

func TestPublish_ManyReturnsDoNotFreeze(t *testing.T) {
	s := rabbitmqtest.Start(t)
	prep := mustDriver(t, s.URL(), "", map[string]string{
		"mode": "admin", "action": "queue_declare", "queue": "q", "allow_writes": "true",
	}, time.Second)
	if res, _ := prep.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
	d := mustDriver(t, s.URL(), "x", map[string]string{"count": "1000", "routing_key": "q"}, 10*time.Second)
	if res, _ := d.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
	if n := len(s.Published()); n != 1000 {
		t.Fatalf("published %d", n)
	}
}

func TestHeartbeat_ZeroIsNegotiated(t *testing.T) {
	s := rabbitmqtest.Start(t)
	d := mustDriver(t, s.URL(), "x", map[string]string{"heartbeat": "0", "mandatory": "false"}, time.Second)
	if res, _ := d.Run(context.Background()); !res.Success {
		t.Fatal(res.Err)
	}
	info := s.Connections()
	if len(info) != 1 || info[0].Heartbeat != 0 {
		t.Fatalf("heartbeat %+v", info)
	}
}

func TestLogin_EnvUnset(t *testing.T) {
	os.Unsetenv("VL_RMQ_MISSING")
	_, err := New(protocol.Target{URL: "amqp://127.0.0.1:1", Options: map[string]string{"password_env": "VL_RMQ_MISSING"}}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "not set") {
		t.Fatal(err)
	}
}
