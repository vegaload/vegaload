package netapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/protocol/ftp/ftptest"
	"github.com/vegaload/vegaload/internal/protocol/sftp/sftptest"
	"github.com/vegaload/vegaload/internal/report"
	"github.com/vegaload/vegaload/internal/safety"
)

func TestCall_RecordsOneFinishedCall(t *testing.T) {
	s := ftptest.Start(t)
	s.Apply(ftptest.Config{Files: map[string][]byte{"/pub/data.bin": []byte("hello ftp")}})
	c := NewProtoClient(nil, 5*time.Second)
	defer c.Close()
	col := report.NewCollector()
	c.SetCallRecorder(col)

	reply, err := c.Call(context.Background(), "ftp.download", ProtoCall{
		URL:     s.URL(),
		Options: map[string]string{"path": "/pub/data.bin"},
	})
	if err != nil || !reply.OK {
		t.Fatalf("download: %v ok=%v err=%s", err, reply != nil && reply.OK, errText(reply))
	}
	res := col.Finish("fixed-vus", time.Second)
	p, ok := res.Protocol("ftp")
	if !ok || p.Total != 1 || p.Failed != 0 || len(p.Operations) != 1 || p.Operations[0].Name != "download" {
		t.Fatalf("protocol = %+v ok %v", p, ok)
	}
	if p.BytesReceived != reply.BytesReceived || p.Operations[0].BytesReceived != reply.BytesReceived {
		t.Fatalf("bytes %d, reply %d", p.BytesReceived, reply.BytesReceived)
	}

	denied, err := c.Call(context.Background(), "ftp.upload", ProtoCall{
		URL:     s.URL(),
		Options: map[string]string{"path": "/up-{id}"},
		Body:    []byte("x"),
	})
	if err != nil || denied.OK || !denied.Refused || !strings.Contains(denied.Error, "allow_writes") {
		t.Fatalf("denied: %v %+v", err, denied)
	}
	if got := col.Finish("fixed-vus", time.Second).Protocols[0].Total; got != 1 {
		t.Fatalf("refused upload was recorded, total %d", got)
	}

	if _, err := c.Call(context.Background(), "ftp.not-a-function", ProtoCall{URL: s.URL()}); err == nil {
		t.Fatal("unknown function returned no error")
	}
	if n := len(col.Finish("fixed-vus", time.Second).Protocols[0].Operations); n != 1 {
		t.Fatalf("unknown function was stored, %d operations", n)
	}
}

func TestCall_RunEndedIsNotRecorded(t *testing.T) {
	s := ftptest.Start(t)
	s.Apply(ftptest.Config{Files: map[string][]byte{"/pub/data.bin": []byte("hello ftp")}})
	c := NewProtoClient(nil, 5*time.Second)
	defer c.Close()
	col := report.NewCollector()
	c.SetCallRecorder(col)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = c.Call(ctx, "ftp.download", ProtoCall{
		URL:     s.URL(),
		Options: map[string]string{"path": "/pub/data.bin"},
	})
	if col.Finish("fixed-vus", time.Second).Protocols != nil {
		t.Fatal("a call cut off by the end of the run was recorded")
	}
}

func TestHTTP_RecordsAndSkipsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("abcdef"))
	}))
	defer srv.Close()

	col := report.NewCollector()
	client := NewHTTPClient(nil, time.Second)
	client.SetCallRecorder(col)
	resp, err := client.Do(context.Background(), "get", srv.URL, []byte("hi"), Options{})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("do: %v %+v", err, resp)
	}
	p, ok := col.Finish("fixed-vus", time.Second).Protocol("http")
	if !ok || p.Total != 1 || p.Failed != 0 || p.Operations[0].Name != "GET /" || p.BytesSent != 2 || p.BytesReceived != 6 {
		t.Fatalf("%+v ok %v", p, ok)
	}

	blocked := NewHTTPClient(func(string) error {
		return errors.New("host is not localhost or allowlisted")
	}, time.Second)
	blocked.SetCallRecorder(col)
	if _, err := blocked.Do(context.Background(), "POST", "http://example.com/", nil, Options{}); err == nil {
		t.Fatal("blocked host was allowed")
	}
	if got := col.Finish("fixed-vus", time.Second).Protocols[0].Total; got != 1 {
		t.Fatalf("blocked call was recorded, total %d", got)
	}

	other := NewHTTPClient(nil, time.Second)
	fresh := report.NewCollector()
	other.SetCallRecorder(fresh)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = other.Do(ctx, "GET", srv.URL, nil, Options{})
	if fresh.Finish("fixed-vus", time.Second).Protocols != nil {
		t.Fatal("an HTTP call cut off by the end of the run was recorded")
	}

	verbs := NewHTTPClient(nil, time.Second)
	verbs.SetCallRecorder(fresh)
	_, err = verbs.Do(context.Background(), "TRACE", srv.URL, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	op := fresh.Finish("fixed-vus", time.Second).Protocols[0].Operations[0].Name
	if op != "OTHER /" {
		t.Fatalf("verb = %s, want OTHER /", op)
	}
}

func TestXferMeasures_NewConnectionAndSkippedPhase(t *testing.T) {
	rec := report.NewCollector()
	RecordXferMeasures(rec, "ftp", "upload", true, XferTiming{UploadMs: 0, ConnectMs: 0, LoginMs: 0})
	RecordXferMeasures(rec, "ftp", "list", true, XferTiming{ConnectMs: 4, LoginMs: 2})
	got := rec.Finish("fixed-vus", time.Second)
	// The list call above did not record a call, only measures. upload_time
	// is present at 0 because upload ran that phase. connect_time is not,
	// because 0 means the connection was reused.
	if len(got.Protocols) != 1 {
		t.Fatalf("%d protocols", len(got.Protocols))
	}
	names := map[string]int64{}
	for _, m := range got.Protocols[0].Measures {
		names[m.Name] = m.Count
	}
	if names["upload_time"] != 1 || names["connect_time"] != 1 || names["login_time"] != 1 {
		t.Fatalf("measures = %+v", names)
	}
	if _, ok := names["download_time"]; ok {
		t.Fatal("list recorded download_time")
	}

	checkConnMeasures(t, "ftp", func(conn string) (calls int, positive int, conns int) {
		s := ftptest.Start(t)
		s.Apply(ftptest.Config{Files: map[string][]byte{"/pub/data.bin": []byte("hello ftp")}})
		c := NewProtoClient(nil, 5*time.Second)
		t.Cleanup(c.Close)
		col := report.NewCollector()
		c.SetCallRecorder(col)
		for i := 0; i < 4; i++ {
			r, err := c.Call(context.Background(), "ftp.download", ProtoCall{
				URL: s.URL(),
				Options: map[string]string{
					"path": "/pub/data.bin", "connection": conn,
				},
			})
			if err != nil || !r.OK {
				t.Fatalf("%s call %d: %v %s", conn, i, err, errText(r))
			}
			calls++
			if r.XferTiming.ConnectMs > 0 {
				positive++
			}
		}
		p, _ := col.Finish("fixed-vus", time.Second).Protocol("ftp")
		return measureCount(p, "connect_time"), positive, s.ConnCount()
	})

	pw := "secret"
	checkConnMeasures(t, "sftp", func(conn string) (calls int, positive int, conns int) {
		s := sftptest.Start(t)
		s.Apply(sftptest.Config{Files: map[string][]byte{"/pub/data.bin": []byte("hello sftp")}})
		c := NewProtoClient(nil, 5*time.Second)
		t.Cleanup(c.Close)
		col := report.NewCollector()
		c.SetCallRecorder(col)
		for i := 0; i < 4; i++ {
			r, err := c.Call(context.Background(), "sftp.download", ProtoCall{
				URL: s.URL(), Insecure: true, Password: &pw,
				Options: map[string]string{
					"path": "/pub/data.bin", "connection": conn,
				},
			})
			if err != nil || !r.OK {
				t.Fatalf("%s call %d: %v %s", conn, i, err, errText(r))
			}
			calls++
			if r.XferTiming.ConnectMs > 0 {
				positive++
			}
		}
		p, _ := col.Finish("fixed-vus", time.Second).Protocol("sftp")
		return measureCount(p, "connect_time"), positive, s.ConnCount()
	})
}

func checkConnMeasures(t *testing.T, protocol string, run func(conn string) (measured, positive, conns int)) {
	t.Helper()
	shared, sharedPositive, sharedConns := run("shared")
	perCall, perPositive, perConns := run("per_call")
	if shared != sharedPositive {
		t.Errorf("%s shared: connect_time %d, replies with ConnectMs > 0: %d", protocol, shared, sharedPositive)
	}
	if shared > 1 {
		t.Errorf("%s shared recorded connect_time %d times, want at most 1", protocol, shared)
	}
	if perCall != perPositive {
		t.Errorf("%s per_call: connect_time %d, replies with ConnectMs > 0: %d", protocol, perCall, perPositive)
	}
	if perConns <= sharedConns {
		t.Errorf("%s per_call opened %d connections, shared opened %d", protocol, perConns, sharedConns)
	}
}

func measureCount(p report.ProtocolResult, name string) int {
	for _, m := range p.Measures {
		if m.Name == name {
			return int(m.Count)
		}
	}
	return 0
}

func errText(r *ProtoReply) string {
	if r == nil {
		return ""
	}
	return r.Error
}

// A phase that took under 1 ms reports 0. When the call worked it did run,
// so it must be counted, or the percentiles keep only the slow runs. When the
// call failed, a 0 may mean the phase was never reached.
func TestXferMeasures_ZeroMeansRanOnlyWhenTheCallWorked(t *testing.T) {
	counts := func(op string, ok bool, tm XferTiming) map[string]int64 {
		rec := report.NewCollector()
		RecordXferMeasures(rec, "ftp", op, ok, tm)
		out := map[string]int64{}
		res := rec.Finish("fixed-vus", time.Second)
		if len(res.Protocols) == 0 {
			return out
		}
		for _, m := range res.Protocols[0].Measures {
			out[m.Name] = m.Count
		}
		return out
	}
	got := counts("roundtrip", true, XferTiming{UploadMs: 5, DownloadMs: 0, DeleteMs: 0})
	if got["upload_time"] != 1 || got["download_time"] != 1 || got["delete_time"] != 1 {
		t.Fatalf("worked roundtrip: %v", got)
	}
	if got := counts("upload", false, XferTiming{}); len(got) != 0 {
		t.Fatalf("failed upload with no time recorded %v", got)
	}
	got = counts("roundtrip", false, XferTiming{UploadMs: 7})
	if got["upload_time"] != 1 || len(got) != 1 {
		t.Fatalf("failed roundtrip: %v", got)
	}
}

// Each refusal kind sets Refused. The wording of the error is not what
// decides whether the call is recorded.
func TestRefusalFlag_AllowWrites(t *testing.T) {
	assertRefused(t, "ftp.upload", ProtoCall{
		URL: "ftp://127.0.0.1:9/", Options: map[string]string{"path": "/up-{id}"}, Body: []byte("x"),
	})
	pw := "secret"
	assertRefused(t, "sftp.upload", ProtoCall{
		URL: "sftp://127.0.0.1:9/", Options: map[string]string{"username": "vegaload", "path": "/up-{id}"}, Body: []byte("x"),
		Password: &pw, Insecure: true,
	})
}

func TestRefusalFlag_AllowAdmin(t *testing.T) {
	assertRefused(t, "ftp.upload", ProtoCall{
		URL: "ftp://127.0.0.1:9/", Options: map[string]string{"path": "/fixed.bin", "allow_writes": "true"}, Body: []byte("x"),
	})
}

func TestRefusalFlag_NeedsID(t *testing.T) {
	assertRefused(t, "ftp.roundtrip", ProtoCall{
		URL: "ftp://127.0.0.1:9/", Options: map[string]string{"path": "/fixed.bin", "allow_writes": "true", "size": "1"},
	})
}

func TestRefusalFlag_NotLocalhost(t *testing.T) {
	c := NewProtoClient(func(host string) error {
		if safety.IsAllowed(host, nil) {
			return nil
		}
		return safety.HostRefusal(host)
	}, time.Second)
	defer c.Close()
	col := report.NewCollector()
	c.SetCallRecorder(col)
	reply, err := c.Call(context.Background(), "ftp.connect", ProtoCall{URL: "ftp://example.com/"})
	if !safety.IsHostRefusal(err) || reply == nil || !reply.Refused {
		t.Fatalf("not localhost: err=%v reply=%+v", err, reply)
	}
	if col.Finish("fixed-vus", time.Second).Protocols != nil {
		t.Fatal("a refused host was recorded")
	}
}

func TestRefusalFlag_Allowlisted(t *testing.T) {
	check := func(host string) error {
		if safety.IsAllowed(host, []string{"example.com"}) {
			return nil
		}
		return safety.HostRefusal(host)
	}
	if err := check("example.com"); err != nil || safety.IsHostRefusal(err) {
		t.Fatalf("allowlisted host was refused: %v", err)
	}
	if !safety.IsHostRefusal(check("other.example")) {
		t.Fatal("a host off the list was not refused")
	}
}

func assertRefused(t *testing.T, name string, call ProtoCall) {
	t.Helper()
	c := NewProtoClient(nil, time.Second)
	defer c.Close()
	col := report.NewCollector()
	c.SetCallRecorder(col)
	reply, err := c.Call(context.Background(), name, call)
	if err != nil || reply == nil || reply.OK || !reply.Refused {
		t.Fatalf("%s: err=%v reply=%+v", name, err, reply)
	}
	if col.Finish("fixed-vus", time.Second).Protocols != nil {
		t.Fatalf("%s was recorded", name)
	}
}

// The check runs on every script call, so it must not allocate.
func TestKnownProtoFn_NoAlloc(t *testing.T) {
	ftp := ProtoFunctions()["ftp"]
	sftp := ProtoFunctions()["sftp"]
	if len(ftp) == 0 || len(sftp) == 0 {
		t.Fatal("ftp and sftp calls are missing from ProtoFunctions")
	}
	allocs := testing.AllocsPerRun(1000, func() {
		for _, fn := range ftp {
			knownProtoFn("ftp", fn)
		}
		for _, fn := range sftp {
			knownProtoFn("sftp", fn)
		}
		knownProtoFn("ftp", "not-a-call")
		knownProtoFn("nope", "download")
	})
	if allocs != 0 {
		t.Fatalf("knownProtoFn allocated %v times per run", allocs)
	}
}
