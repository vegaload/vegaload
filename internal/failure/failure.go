// Package failure names why a request failed, with one name from a small,
// fixed list, so the report can say how many requests failed for each
// reason. A name never carries text from a target or a script (a URL, an
// id, a secret), and a run of a million failed calls still has only a
// handful of names.
//
// It knows nothing about any protocol driver: it reads the standard
// library's error types and, for an error that is only text (the reply of
// a scenario's protocol call), a few well-known phrases.
package failure

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// The reasons other than a status. A status failure is named by Status,
// for example "status 503".
const (
	Timeout           = "timeout"
	ConnectionRefused = "connection refused"
	ConnectionReset   = "connection reset"
	DNS               = "dns"
	TLS               = "tls"
	Other             = "error"
)

// StatusError is a call that got an answer with a status the call does not
// accept, such as HTTP 500.
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string { return "status " + strconv.Itoa(e.Code) }

// Status returns the error of a call answered with a status it does not
// accept.
func Status(code int) error { return &StatusError{Code: code} }

// Reason names why err failed. It returns "" for a nil error.
func Reason(err error) string {
	if err == nil {
		return ""
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Error()
	}
	if isTimeout(err) {
		return Timeout
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return ConnectionRefused
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return ConnectionReset
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return DNS
	}
	if isTLS(err) {
		return TLS
	}
	return fromText(err.Error())
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var te interface{ Timeout() bool }
	return errors.As(err, &te) && te.Timeout()
}

func isTLS(err error) bool {
	var (
		header    tls.RecordHeaderError
		alert     tls.AlertError
		verify    *tls.CertificateVerificationError
		authority x509.UnknownAuthorityError
		hostname  x509.HostnameError
		invalid   x509.CertificateInvalidError
	)
	return errors.As(err, &header) || errors.As(err, &alert) || errors.As(err, &verify) ||
		errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid)
}

// textReasons are the phrases a driver's error text uses, in the order
// they are tried. Windows words a refused and a reset connection its own
// way, so both forms are listed.
var textReasons = []struct {
	reason  string
	phrases []string
}{
	{Timeout, []string{"timeout", "timed out", "deadline exceeded"}},
	{ConnectionRefused, []string{"connection refused", "actively refused"}},
	{ConnectionReset, []string{"connection reset", "forcibly closed"}},
	{DNS, []string{"no such host", "server misbehaving"}},
	{TLS, []string{"tls:", "x509:", "certificate"}},
}

func fromText(msg string) string {
	msg = strings.ToLower(msg)
	for _, tr := range textReasons {
		for _, p := range tr.phrases {
			if strings.Contains(msg, p) {
				return tr.reason
			}
		}
	}
	return Other
}
