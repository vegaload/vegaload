package http2

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/vegaload/vegaload/internal/protocol"
)

func countingH2CServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var opened atomic.Int64
	srv := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}), &http2.Server{}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &opened
}

func TestDriver_ConnectionReuseAndOptOut(t *testing.T) {
	for _, tc := range []struct {
		noReuse bool
		want    int64
	}{{false, 1}, {true, 10}} {
		srv, opened := countingH2CServer(t)
		d, err := New(protocol.Target{URL: srv.URL, NoConnectionReuse: tc.noReuse}, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 10; i++ {
			if res, err := d.Do(context.Background()); err != nil || !res.Success {
				t.Fatalf("Do = %+v, %v", res, err)
			}
		}
		d.Close()
		if n := opened.Load(); n != tc.want {
			t.Errorf("no reuse %v: 10 calls opened %d connections, want %d", tc.noReuse, n, tc.want)
		}
	}
}
