package protocol

import (
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

// TTFBMeasure is the measure a call's time to first byte is recorded under
// (report.TTFBMeasure is the same name).
const TTFBMeasure = "ttfb"

// FirstByte times one HTTP call from its start to the first byte of the
// first answer, so the time of a redirected call is its first hop's. Like a
// call's latency, it includes connecting.
type FirstByte struct {
	start time.Time
	at    atomic.Int64 // nanoseconds after start; 0 until the first byte
}

// TraceFirstByte returns req traced by a FirstByte that starts now. Call it
// just before the request is sent.
func TraceFirstByte(req *http.Request) (*http.Request, *FirstByte) {
	fb := &FirstByte{start: time.Now()}
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() {
		fb.at.CompareAndSwap(0, max(int64(time.Since(fb.start)), 1))
	}}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace)), fb
}

// TTFB is the time to first byte, or 0 when no answer came.
func (f *FirstByte) TTFB() time.Duration { return time.Duration(f.at.Load()) }
