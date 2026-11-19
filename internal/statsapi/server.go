package statsapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// Server answers GET /stats and GET /healthz. Any other path is 404, and any
// other method 405.
type Server struct {
	ln  net.Listener
	srv *http.Server

	mu     sync.RWMutex
	source func() Stats
}

// Listen starts a server on addr (":9093" is every interface,
// "127.0.0.1:9093" loopback only). Until SetSource is called, /stats is
// Empty(StateStarting).
func Listen(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln}
	empty := Empty(StateStarting)
	s.source = func() Stats { return empty }
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/healthz", handleHealth)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// Addr is the address the server listens on, with the port it was given.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// SetSource makes /stats answer with what f returns. f is called once per
// request, from any goroutine.
func (s *Server) SetSource(f func() Stats) {
	s.mu.Lock()
	s.source = f
	s.mu.Unlock()
}

// Freeze makes /stats answer with st from now on.
func (s *Server) Freeze(st Stats) { s.SetSource(func() Stats { return st }) }

// Close stops the server, waiting at most a second for requests in flight.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := s.srv.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		return s.srv.Close()
	}
	return err
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if !allowed(w, r) {
		return
	}
	s.mu.RLock()
	f := s.source
	s.mu.RUnlock()
	writeJSON(w, r, f())
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	if !allowed(w, r) {
		return
	}
	writeJSON(w, r, map[string]bool{"ok": true})
}

// allowed answers a request that is not a GET or HEAD of the exact path, and
// reports whether the handler should go on.
func allowed(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/stats" && r.URL.Path != "/healthz" {
		http.NotFound(w, r)
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(append(body, '\n'))
}
