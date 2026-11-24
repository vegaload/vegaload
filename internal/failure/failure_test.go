package failure

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vegaload/vegaload/internal/secrets"
)

func TestReason(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"nil":              {nil, ""},
		"status":           {Status(503), "status 503"},
		"wrapped status":   {fmt.Errorf("http1: request failed: %w", Status(404)), "status 404"},
		"context deadline": {fmt.Errorf("netapi: %w", context.DeadlineExceeded), Timeout},
		"os deadline":      {&net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, Timeout},
		"url timeout":      {&url.Error{Op: "Get", URL: "http://x", Err: timeoutErr{}}, Timeout},
		"refused":          {&net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}, ConnectionRefused},
		"reset":            {&net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}, ConnectionReset},
		"dns":              {&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "nope.invalid", IsNotFound: true}}, DNS},
		"dns timeout":      {&net.DNSError{Err: "i/o timeout", Name: "slow.invalid", IsTimeout: true}, Timeout},
		"tls authority":    {&url.Error{Op: "Get", URL: "https://x", Err: x509.UnknownAuthorityError{}}, TLS},
		"other":            {errors.New("js: boom"), Other},

		// A scenario's protocol call reports its error as text only.
		"text timeout":         {errors.New("read tcp 127.0.0.1:1->127.0.0.1:2: i/o timeout"), Timeout},
		"text timed out":       {errors.New("redis: the call timed out"), Timeout},
		"text refused":         {errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), ConnectionRefused},
		"text refused windows": {errors.New("No connection could be made because the target machine actively refused it."), ConnectionRefused},
		"text reset":           {errors.New("read: connection reset by peer"), ConnectionReset},
		"text dns":             {errors.New("dial tcp: lookup nope.invalid: no such host"), DNS},
		"text tls":             {errors.New("tls: failed to verify certificate"), TLS},
		"empty text":           {errors.New(""), Other},
	}
	for name, c := range cases {
		if got := Reason(c.err); got != c.want {
			t.Errorf("%s: Reason(%v) = %q, want %q", name, c.err, got, c.want)
		}
	}
}

func TestKind(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"nil":     {nil, ""},
		"status":  {fmt.Errorf("http1: request failed: %w", Status(503)), "status 503"},
		"timeout": {fmt.Errorf("netapi: %w", context.DeadlineExceeded), Timeout},
		"refused": {errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), ConnectionRefused},
		"dns":     {&net.DNSError{Err: "no such host", Name: "nope.invalid", IsNotFound: true}, DNS},
		"tls":     {&url.Error{Op: "Get", URL: "https://x", Err: x509.UnknownAuthorityError{}}, TLS},
		"reset":   {errors.New("read: connection reset by peer"), KindOther},
		"other":   {errors.New("js: boom"), KindOther},
	}
	for name, c := range cases {
		if got := Kind(c.err); got != c.want {
			t.Errorf("%s: Kind(%v) = %q, want %q", name, c.err, got, c.want)
		}
	}
	if KindOf("something new") != KindOther {
		t.Error("an unknown reason is not other")
	}
}

func TestSample_MasksRedactsAndCuts(t *testing.T) {
	secrets.Reset()
	t.Cleanup(secrets.Reset)
	secrets.Register("s3cr3t-token")

	err := fmt.Errorf("Get \"redis://bob:hunter2@db:6379/0\": refused\nheader s3cr3t-token was sent")
	got := Sample(err)
	for _, leak := range []string{"hunter2", "s3cr3t-token", "\n"} {
		if strings.Contains(got, leak) {
			t.Fatalf("Sample leaks %q: %s", leak, got)
		}
	}
	if !strings.Contains(got, "bob:***@db") || !strings.Contains(got, secrets.Marker) {
		t.Fatalf("Sample = %q", got)
	}

	long := Sample(errors.New(strings.Repeat("é", 300)))
	if !strings.HasSuffix(long, "...") || len(long) > MaxSample+3 || !utf8.ValidString(long) {
		t.Fatalf("long sample = %d bytes, valid=%v", len(long), utf8.ValidString(long))
	}
	if Sample(nil) != "" {
		t.Fatal("nil error has a sample")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "slow" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// A real client timeout is named timeout, not by the words of its message.
func TestReason_RealHTTPClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	_, err := (&http.Client{Timeout: 20 * time.Millisecond}).Get(srv.URL)
	if err == nil {
		t.Fatal("the request did not time out")
	}
	if got := Reason(err); got != Timeout {
		t.Errorf("Reason(%v) = %q, want %q", err, got, Timeout)
	}
}

// A real refused connection is named from its error type.
func TestReason_RealRefusedConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, err = (&http.Client{Timeout: 2 * time.Second}).Get("http://" + addr)
	if err == nil {
		t.Fatal("a closed port answered")
	}
	if got := Reason(err); got != ConnectionRefused {
		t.Errorf("Reason(%v) = %q, want %q", err, got, ConnectionRefused)
	}
}
