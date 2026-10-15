package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ValidatePushURL checks a push destination. It must be http or https, carry
// no credentials, and use https unless the host is loopback or insecure is set.
func ValidatePushURL(raw string, insecure bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("stream push URL: %s", sanitizeErr(err))
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("stream push URL %q: want http:// or https:// with a host", hostOnly(u))
	}
	if u.User != nil {
		return nil, fmt.Errorf("stream push URL %q: do not put credentials in the URL; set VEGALOAD_STREAM_TOKEN", hostOnly(u))
	}
	if u.Scheme == "http" && !insecure && !IsLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("stream push URL %q: http:// is only allowed for loopback hosts; use https:// or pass -stream-insecure", hostOnly(u))
	}
	return u, nil
}

// IsLoopbackHost reports whether host is localhost or a loopback IP.
func IsLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hostOnly is how a destination is named in messages and logs: the URL may
// carry a path or query that is itself a secret.
func hostOnly(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// sanitizeErr drops the URL that net/http puts in its errors.
func sanitizeErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
}

// PushOptions configures a PushSink.
type PushOptions struct {
	URL     *url.URL
	Token   string
	Version string
	// Zero values take the documented defaults.
	MaxBatch   int           // 100 events
	MaxDelay   time.Duration // 500 ms from the first event of a batch
	Timeout    time.Duration // 10 s per request
	BackoffMin time.Duration // 500 ms
	BackoffMax time.Duration // 10 s
	Client     *http.Client
}

// PushSink POSTs batches of NDJSON to an ingest endpoint
// (docs/streaming.md, "Push"). One batch is in flight at a time, so a receiver
// sees batches in order, and a retried batch repeats the same sequence range.
//
// 2xx acknowledges a batch. Network errors, 408, 429 and 5xx are retried with
// exponential backoff until the run is over and the deadline given to
// Recorder.Close passes. Any other 4xx drops the batch and adds a warning.
type PushSink struct {
	opt    PushOptions
	client *http.Client

	// Used only from the runner goroutine.
	buf     [][]byte
	first   time.Time
	seqFrom int64
	seqTo   int64
	runID   string

	ctx    context.Context
	cancel context.CancelFunc

	// drained is closed when the run is over (see Drainer).
	drained   chan struct{}
	drainOnce sync.Once

	mu        sync.Mutex
	warns     map[string]int
	order     []string
	failing   bool
	delivered int
	lost      int
}

// NewPushSink creates a sink for opt.URL, which must already have passed
// ValidatePushURL.
func NewPushSink(opt PushOptions) *PushSink {
	if opt.MaxBatch <= 0 {
		opt.MaxBatch = 100
	}
	if opt.MaxDelay <= 0 {
		opt.MaxDelay = 500 * time.Millisecond
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 10 * time.Second
	}
	if opt.BackoffMin <= 0 {
		opt.BackoffMin = 500 * time.Millisecond
	}
	if opt.BackoffMax <= 0 {
		opt.BackoffMax = 10 * time.Second
	}
	client := opt.Client
	if client == nil {
		client = &http.Client{}
	}
	// A redirect could send the token somewhere else, or from https to http.
	// Treat it as the answer it is: not a 2xx.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithCancel(context.Background())
	return &PushSink{opt: opt, client: &c, ctx: ctx, cancel: cancel, drained: make(chan struct{}), warns: map[string]int{}}
}

func (s *PushSink) Name() string { return "push " + hostOnly(s.opt.URL) }

func (s *PushSink) Write(line []byte) error {
	var head struct {
		RunID string `json:"runId"`
		Seq   int64  `json:"seq"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return err
	}
	if len(s.buf) == 0 {
		s.first = time.Now()
		s.seqFrom = head.Seq
		s.runID = head.RunID
	}
	s.seqTo = head.Seq
	s.buf = append(s.buf, append([]byte(nil), line...))
	if len(s.buf) >= s.opt.MaxBatch {
		return s.Flush()
	}
	return nil
}

func (s *PushSink) Deadline() (time.Time, bool) {
	if len(s.buf) == 0 {
		return time.Time{}, false
	}
	return s.first.Add(s.opt.MaxDelay), true
}

// Flush sends the pending batch, retrying where the rules say so. It returns
// an error only when the sink was aborted, which ends it.
func (s *PushSink) Flush() error {
	if len(s.buf) == 0 {
		return nil
	}
	batch, from, to, run := s.buf, s.seqFrom, s.seqTo, s.runID
	s.buf = nil
	body := append(bytes.Join(batch, []byte("\n")), '\n')

	for attempt := 0; ; attempt++ {
		status, retryAfter, err := s.post(body, run, from, to)
		switch {
		case err == nil && status >= 200 && status < 300:
			s.mu.Lock()
			s.failing = false
			s.delivered += len(batch)
			s.mu.Unlock()
			return nil
		case err == nil && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests && status < 500:
			// The receiver will not accept this batch however often it is sent.
			s.mu.Lock()
			s.lost += len(batch)
			s.mu.Unlock()
			s.warn(fmt.Sprintf("stream: %s answered %d; batches were dropped", s.Name(), status))
			return nil
		}
		if s.ctx.Err() != nil {
			s.mu.Lock()
			s.lost += len(batch)
			s.mu.Unlock()
			s.warn(fmt.Sprintf("stream: %s: gave up at exit; the last events (from %d) were not delivered", s.Name(), from))
			return errors.New("aborted")
		}
		reason := fmt.Sprintf("answered %d", status)
		if err != nil {
			reason = sanitizeErr(err)
		}
		s.mu.Lock()
		first := !s.failing
		s.failing = true
		s.mu.Unlock()
		if first {
			s.warn(fmt.Sprintf("stream: %s is not accepting events (%s); retrying", s.Name(), reason))
		}
		// Once the run is over the time left is short and the batch holds the
		// terminal event, so retry at the shortest interval instead of
		// doubling it. A Retry-After is still honoured.
		draining, wake := s.isDraining(), s.drained
		if draining {
			attempt, wake = 0, nil
		}
		t := time.NewTimer(s.backoff(attempt, retryAfter))
		select {
		case <-t.C:
		case <-wake:
		case <-s.ctx.Done():
		}
		t.Stop()
	}
}

// Drain implements Drainer.
func (s *PushSink) Drain() { s.drainOnce.Do(func() { close(s.drained) }) }

func (s *PushSink) isDraining() bool {
	select {
	case <-s.drained:
		return true
	default:
		return false
	}
}

func (s *PushSink) backoff(attempt int, retryAfter time.Duration) time.Duration {
	d := s.opt.BackoffMin
	for i := 0; i < attempt && d < s.opt.BackoffMax; i++ {
		d *= 2
	}
	if d > s.opt.BackoffMax {
		d = s.opt.BackoffMax
	}
	if retryAfter > d {
		d = retryAfter
	}
	// +-20% so that senders that failed together do not retry together.
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

func (s *PushSink) post(body []byte, run string, from, to int64) (status int, retryAfter time.Duration, err error) {
	ctx, cancel := context.WithTimeout(s.ctx, s.opt.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.opt.URL.String(), bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	h := req.Header
	h.Set("Content-Type", "application/x-ndjson")
	h.Set("X-Vegaload-Run-Id", run)
	h.Set("X-Vegaload-Seq-From", strconv.FormatInt(from, 10))
	h.Set("X-Vegaload-Seq-To", strconv.FormatInt(to, 10))
	if s.opt.Version != "" {
		h.Set("User-Agent", "vegaload/"+s.opt.Version)
	}
	if s.opt.Token != "" {
		h.Set("Authorization", "Bearer "+s.opt.Token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
			retryAfter = time.Duration(secs) * time.Second
			if retryAfter > 30*time.Second {
				retryAfter = 30 * time.Second
			}
		}
	}
	return resp.StatusCode, retryAfter, nil
}

// Abort makes a retrying Flush give up.
func (s *PushSink) Abort() { s.cancel() }

func (s *PushSink) Close() error {
	s.cancel()
	return nil
}

func (s *PushSink) warn(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.warns[msg]; !ok {
		s.order = append(s.order, msg)
	}
	s.warns[msg]++
}

// Warnings implements Warner: each distinct problem once, with a count when it
// repeated.
func (s *PushSink) Warnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.order))
	for _, m := range s.order {
		if n := s.warns[m]; n > 1 {
			m = fmt.Sprintf("%s (%d times)", m, n)
		}
		out = append(out, m)
	}
	return out
}

// Delivered returns how many events the receiver acknowledged and how many
// were given up on.
func (s *PushSink) Delivered() (delivered, lost int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delivered, s.lost
}
