package protocol_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/protocol"
	"github.com/vegaload/vegaload/internal/protocol/ftp"
	"github.com/vegaload/vegaload/internal/protocol/grpc"
	"github.com/vegaload/vegaload/internal/protocol/http1"
	"github.com/vegaload/vegaload/internal/protocol/http2"
	"github.com/vegaload/vegaload/internal/protocol/kafka"
	"github.com/vegaload/vegaload/internal/protocol/mqtt"
	"github.com/vegaload/vegaload/internal/protocol/mysql"
	"github.com/vegaload/vegaload/internal/protocol/postgres"
	"github.com/vegaload/vegaload/internal/protocol/rabbitmq"
	"github.com/vegaload/vegaload/internal/protocol/redis"
	"github.com/vegaload/vegaload/internal/protocol/socket"
	"github.com/vegaload/vegaload/internal/protocol/websocket"
	"github.com/vegaload/vegaload/internal/safety"
)

// A URL that cannot be parsed must not come back inside the error, because
// the URL can hold a password.
func TestBadURLErrorsDoNotRepeatThePassword(t *testing.T) {
	const secret = "hunter2-secret"
	mk := func(scheme string) protocol.Target {
		return protocol.Target{URL: scheme + "://alice:" + secret + "@[::1"}
	}
	cases := map[string]func() error{
		"redis":    func() error { _, err := redis.New(mk("redis"), time.Second); return err },
		"mysql":    func() error { _, err := mysql.New(mk("mysql"), time.Second); return err },
		"postgres": func() error { _, err := postgres.New(mk("postgres"), time.Second); return err },
		"mqtt":     func() error { _, err := mqtt.New(mk("mqtt"), time.Second); return err },
		"kafka":    func() error { _, err := kafka.New(mk("kafka"), time.Second); return err },
		"ws":       func() error { _, err := websocket.New(mk("ws"), time.Second); return err },
		"http2":    func() error { _, err := http2.New(mk("https"), time.Second); return err },
		"grpc":     func() error { _, err := grpc.New(mk("grpc"), time.Second); return err },
		"rabbitmq": func() error { _, err := rabbitmq.New(mk("amqp"), time.Second); return err },
		"ftp":      func() error { _, err := ftp.New(mk("ftp"), time.Second); return err },
		"safety":   func() error { _, err := safety.TargetHost(mk("redis").URL); return err },
		"socket":   func() error { _, err := socket.NewTCP(mk("tcp"), time.Second); return err },
		"http1": func() error {
			_, err := http1.New(mk("http"), time.Second).Do(context.Background())
			return err
		},
	}
	for name, f := range cases {
		err := f()
		if err == nil {
			t.Errorf("%s: want an error for a URL that does not parse", name)
			continue
		}
		// Never print err here: a failing run would write the password into the test log.
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error repeats the password", name)
		}
	}
}

// A URL that parses but has no host must not come back in the error either.
func TestNoHostErrorsDoNotRepeatThePassword(t *testing.T) {
	const secret = "hunter2-secret"
	mk := func(scheme string) protocol.Target {
		return protocol.Target{URL: scheme + "://alice:" + secret + "@"}
	}
	cases := map[string]func() error{
		"mqtt":   func() error { _, err := mqtt.New(mk("mqtt"), time.Second); return err },
		"kafka":  func() error { _, err := kafka.New(mk("kafka"), time.Second); return err },
		"redis":  func() error { _, err := redis.New(mk("redis"), time.Second); return err },
		"mysql":  func() error { _, err := mysql.New(mk("mysql"), time.Second); return err },
		"socket": func() error { _, err := socket.NewTCP(mk("tcp"), time.Second); return err },
		"safety": func() error { _, err := safety.TargetHost(mk("redis").URL); return err },
	}
	for name, f := range cases {
		err := f()
		if err == nil {
			t.Errorf("%s: want an error for a URL with no host", name)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error repeats the password", name)
		}
	}
}
