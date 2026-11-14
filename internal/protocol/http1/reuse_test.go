package http1

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/protocol"
)

func countingServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var opened atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &opened
}

func doConcurrently(t *testing.T, d *Driver, vus, calls int) {
	t.Helper()
	var wg sync.WaitGroup
	for i := 0; i < vus; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < calls; j++ {
				if res, err := d.Do(context.Background()); err != nil || !res.Success {
					t.Errorf("Do = %+v, %v", res, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// Every VU shares one Driver. With the pool sized from MaxConns, N VUs keep
// about N connections, however many calls they make.
func TestDriver_PoolSizedFromMaxConnsReusesConnections(t *testing.T) {
	srv, opened := countingServer(t)
	const vus, calls = 20, 30
	d, err := New(protocol.Target{URL: srv.URL, MaxConns: vus}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	doConcurrently(t, d, vus, calls)
	if n := opened.Load(); n > vus {
		t.Errorf("%d VUs making %d calls each opened %d connections, want at most %d", vus, calls, n, vus)
	}
}

func TestDriver_NoConnectionReuseOpensOnePerCall(t *testing.T) {
	srv, opened := countingServer(t)
	d, err := New(protocol.Target{URL: srv.URL, MaxConns: 4, NoConnectionReuse: true}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	doConcurrently(t, d, 1, 10)
	if n := opened.Load(); n != 10 {
		t.Errorf("10 calls with no reuse opened %d connections, want 10", n)
	}
}
