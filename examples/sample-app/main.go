// Command sample-app is a tiny, deliberately imperfect service for
// VegaLoad's own getting-started walkthrough (see ../README.md): there
// is nothing here VegaLoad depends on, it's just something worth load
// testing. It keeps one in-memory list of "widgets" and serves it over
// all four of VegaLoad's protocols, so every driver has a real target:
//
// On -addr (default 127.0.0.1:8080), HTTP/1.1 and HTTP/2 cleartext (h2c):
//
//	GET  /health        -- always 200, near-zero latency
//	GET  /widgets        -- list widgets, 10-40ms simulated latency
//	GET  /widgets/{id}   -- one widget, or 404
//	POST /widgets        -- create a widget, 20-60ms latency, and a
//	                         ~3% random 500 to give a baseline run some
//	                         real failures for `vegaload diagnose` to
//	                         explain
//	GET  /ws/echo        -- WebSocket: echoes each message back,
//	                         5-15ms simulated latency per message
//
// On -grpc-addr (default 127.0.0.1:9090), gRPC (see grpc.go and
// widgets.proto): the standard grpc.health.v1.Health service, plus
// widgets.v1.WidgetService with the same list/get/create operations,
// latency, and injected failure rate as the REST API above. Both APIs
// share one store, so a widget created over gRPC shows up over REST.
//
// Its own go.mod, separate from VegaLoad's module -- it's a target to
// test, not part of the tool. The HTTP/1.1 side needs only the standard
// library. The other protocols use the same third-party modules (and the
// same versions) VegaLoad's own drivers use: golang.org/x/net/http2 for
// h2c, gorilla/websocket, and google.golang.org/grpc plus
// google.golang.org/protobuf. That way the target and the load generator
// speak each protocol through the same implementations.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// Widget is the sample app's one resource.
type Widget struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type store struct {
	mu      sync.Mutex
	widgets []Widget
	nextID  int

	// failPercent is the chance, out of 100, that createChecked
	// returns errTransient instead of creating anything. It is set once
	// in newStore and never changed afterwards; tests that need a
	// deterministic create set it to 0 before using the store.
	failPercent int
}

func newStore() *store {
	return &store{
		widgets:     []Widget{{ID: 1, Name: "sprocket"}, {ID: 2, Name: "gear"}, {ID: 3, Name: "cam"}},
		nextID:      4,
		failPercent: 3,
	}
}

func (s *store) list() []Widget {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Widget, len(s.widgets))
	copy(out, s.widgets)
	return out
}

func (s *store) get(id int) (Widget, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.widgets {
		if w.ID == id {
			return w, true
		}
	}
	return Widget{}, false
}

func (s *store) create(name string) Widget {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := Widget{ID: s.nextID, Name: name}
	s.nextID++
	s.widgets = append(s.widgets, w)
	return w
}

// The two ways createChecked can refuse a create. They are sentinel
// errors (compared with errors.Is) so each API can map them onto its own
// status codes without parsing messages:
//
//	                  REST (POST /widgets)   gRPC (CreateWidget)
//	errTransient      500                    UNAVAILABLE
//	errNameRequired   400                    INVALID_ARGUMENT
var (
	// errTransient is the injected ~3% failure on create. It stands in
	// for a real service's intermittent faults: a dropped DB connection,
	// a timeout to a dependency.
	errTransient = errors.New("simulated transient failure")
	// errNameRequired is a create with an empty or missing name.
	errNameRequired = errors.New(`expected a non-empty "name"`)
)

// createChecked is the one create path both APIs use, so REST and gRPC
// behave identically under load. Before this change the POST /widgets
// handler did these steps inline; they moved here unchanged, in the same
// order, so REST behavior is the same as before:
//
//  1. 20-60ms simulated latency;
//  2. a failPercent chance (3 by default) of errTransient. This is
//     checked before validation, so even an invalid request can draw
//     the random failure, exactly as the original handler did;
//  3. errNameRequired if name is blank;
//  4. otherwise, create the widget.
//
// The same store, and so the same widget IDs, back both APIs.
func (s *store) createChecked(name string) (Widget, error) {
	simulateLatency(20, 60)
	// A ~3% random failure rate, so a baseline run has a few real
	// failures in it -- enough for `vegaload diagnose` to have
	// something to say, without drowning out the passing majority.
	if rand.Intn(100) < s.failPercent {
		return Widget{}, errTransient
	}
	if strings.TrimSpace(name) == "" {
		return Widget{}, errNameRequired
	}
	return s.create(name), nil
}

// simulateLatency sleeps for a random duration in [minMS, maxMS) --
// real services are never instant, and a flat 0ms response makes for a
// boring first load test report.
func simulateLatency(minMS, maxMS int) {
	time.Sleep(time.Duration(minMS+rand.Intn(maxMS-minMS)) * time.Millisecond)
}

// upgrader switches a GET /ws/echo request from HTTP to the WebSocket
// protocol (the HTTP "101 Switching Protocols" handshake).
//
// It keeps gorilla/websocket's default origin check on purpose, so:
//   - clients that send no Origin header are accepted. That covers
//     VegaLoad and command-line tools like websocat;
//   - a browser page served from this same host is accepted;
//   - a page on some other site is refused with 403. Without this, any
//     website open in your browser could quietly open sockets to the app
//     on localhost (cross-site WebSocket hijacking). It's harmless for an
//     echo server, but it's the wrong habit to model in an example.
var upgrader = websocket.Upgrader{}

// wsEcho serves GET /ws/echo: it upgrades the connection, then echoes
// every message back to the sender, keeping its type (text or binary).
//
// VegaLoad's WebSocket driver opens a fresh connection per iteration,
// sends its -body once, waits for one reply, and closes. That measures
// handshake plus one round trip, and the per-message latency below keeps
// it realistic. Because the handler loops until the client closes, it
// also suits a client that keeps one connection open and sends many
// messages.
func wsEcho(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written an HTTP error response (400 for
		// a non-WebSocket request, 403 for a refused origin).
		return
	}
	defer conn.Close()

	// Cap incoming messages at 64 KiB, so one client can't make the
	// server buffer an arbitrarily large message. A larger message
	// closes the connection with an error.
	conn.SetReadLimit(64 << 10)

	for {
		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			// The client closed the connection (normal at the end of
			// every VegaLoad iteration), or sent something invalid.
			return
		}
		// 5-15ms of "processing" per message, so the WebSocket report
		// has a latency distribution to look at, as the HTTP
		// endpoints do.
		simulateLatency(5, 15)
		if err := conn.WriteMessage(msgType, msg); err != nil {
			return
		}
	}
}

func newMux(s *store) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
	})

	mux.HandleFunc("GET /widgets", func(w http.ResponseWriter, r *http.Request) {
		simulateLatency(10, 40)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.list()) //nolint:errcheck
	})

	mux.HandleFunc("GET /widgets/{id}", func(w http.ResponseWriter, r *http.Request) {
		simulateLatency(10, 40)
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		widget, ok := s.get(id)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(widget) //nolint:errcheck
	})

	mux.HandleFunc("POST /widgets", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		// A malformed or missing body leaves Name empty, which
		// createChecked rejects as errNameRequired. The original handler
		// also answered both cases with the same 400, so the decode
		// error itself isn't needed.
		_ = json.NewDecoder(r.Body).Decode(&body)

		// Latency, the injected failure, and validation all happen in
		// createChecked, shared with gRPC's CreateWidget (see grpc.go).
		// This handler only maps the outcome onto HTTP status codes.
		created, err := s.createChecked(body.Name)
		switch {
		case errors.Is(err, errTransient):
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		case err != nil:
			http.Error(w, "expected a JSON body with a non-empty \"name\"", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(created) //nolint:errcheck
	})

	// WebSocket. A WebSocket handshake is an ordinary GET with
	// "Upgrade: websocket" headers, so it routes like any other GET.
	mux.HandleFunc("GET /ws/echo", wsEcho)

	return mux
}

// newHTTPHandler is the handler that actually serves -addr. It wraps the
// mux so one port speaks both HTTP/1.1 and HTTP/2.
//
// HTTP/2 normally runs over TLS, where client and server agree on it
// during the TLS handshake (ALPN). Go's standard server only does it that
// way. This app has no certificate, so it uses h2c instead: HTTP/2
// cleartext, where the client simply starts talking HTTP/2 on a plain
// TCP connection ("prior knowledge"). That is exactly what VegaLoad's
// http2 driver does for an http:// target, and what
// `curl --http2-prior-knowledge` does.
//
// h2c.NewHandler looks at each new connection. If it opens with the
// HTTP/2 preface, it is served by the http2.Server; anything else
// (HTTP/1.1 requests, including WebSocket upgrades) passes straight
// through to the mux unchanged. So -protocol http1 and -protocol http2
// hit the very same handlers, and their reports compare like for like.
func newHTTPHandler(s *store) http.Handler {
	return h2c.NewHandler(newMux(s), &http2.Server{})
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "address for HTTP/1.1, HTTP/2 (h2c), and WebSocket")
	grpcAddr := flag.String("grpc-addr", "127.0.0.1:9090", "address for gRPC")
	flag.Parse()

	// One store shared by both servers: a widget created through either
	// API is visible through the other.
	s := newStore()

	// gRPC gets its own port rather than sharing -addr. grpc-go can be
	// mounted inside an HTTP handler, but that mode is documented as
	// experimental and slower than its native server. For a load-testing
	// target, a server that behaves like production gRPC matters more
	// than saving a port. Listening here, before anything starts, means
	// a port that's already taken fails immediately with a clear message.
	grpcLis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatalf("gRPC listener: %v", err)
	}

	// Run both servers concurrently. Each normally runs forever, so if
	// either returns at all (e.g. -addr is already in use), log why and
	// exit: a half-running sample app would make runs against the other
	// protocol fail confusingly.
	errc := make(chan error, 2)
	go func() {
		errc <- http.ListenAndServe(*addr, newHTTPHandler(s))
	}()
	go func() {
		errc <- newGRPCServer(s).Serve(grpcLis)
	}()

	log.Printf("sample-app listening:")
	log.Printf("  http://%s    HTTP/1.1 + h2c: GET /health, GET /widgets, GET /widgets/{id}, POST /widgets", *addr)
	log.Printf("  ws://%s/ws/echo    WebSocket echo", *addr)
	log.Printf("  %s    gRPC: grpc.health.v1.Health, widgets.v1.WidgetService", *grpcAddr)

	log.Fatal(<-errc)
}
