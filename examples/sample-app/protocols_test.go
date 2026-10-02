package main

// protocols_test.go covers what this change added to the sample app: the
// HTTP/2 (h2c), WebSocket, and gRPC sides. The original REST tests live
// in main_test.go and are unchanged.
//
// Every test runs against an in-process server on a random local port
// (httptest.NewServer, or a net.Listen on 127.0.0.1:0), so the suite
// needs no running sample app and can't collide with one that is
// running.
//
// Tests that create widgets set store.failPercent to 0 (or 100), so the
// ~3% injected failure can't make them flaky. The original REST tests
// instead accept either outcome.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// --- HTTP/2 (h2c) on the shared -addr port ---

// TestH2C checks that the shared port speaks HTTP/2 cleartext when a
// client opens with the HTTP/2 preface, as VegaLoad's http2 driver does,
// and that the REST handlers answer normally over it.
func TestH2C(t *testing.T) {
	srv := httptest.NewServer(newHTTPHandler(newStore()))
	defer srv.Close()

	// An HTTP/2 client over plain TCP: prior-knowledge h2c, the same way
	// VegaLoad's http2 driver talks to an http:// target.
	client := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}

	resp, err := client.Get(srv.URL + "/widgets")
	if err != nil {
		t.Fatalf("GET /widgets over h2c: %v", err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Errorf("proto = %s, want HTTP/2", resp.Proto)
	}
	var widgets []Widget
	if err := json.NewDecoder(resp.Body).Decode(&widgets); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(widgets) != 3 {
		t.Errorf("got %d widgets, want 3", len(widgets))
	}
}

// TestHTTP1StillWorksBehindH2C guards the other half of the h2c wrapper:
// a plain HTTP/1.1 client on the same port must still be served as
// HTTP/1.1, so -protocol http1 keeps testing what it says.
func TestHTTP1StillWorksBehindH2C(t *testing.T) {
	srv := httptest.NewServer(newHTTPHandler(newStore()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 1 || resp.StatusCode != http.StatusOK {
		t.Errorf("got %s %d, want HTTP/1.1 200", resp.Proto, resp.StatusCode)
	}
}

// --- WebSocket: GET /ws/echo ---

// TestWebSocketEcho checks the echo end to end: text and binary
// messages come back unchanged and with the same type, and several
// messages work on one connection, not just VegaLoad's single round
// trip.
func TestWebSocketEcho(t *testing.T) {
	// Through the h2c wrapper, as in main, to prove upgrades pass through.
	srv := httptest.NewServer(newHTTPHandler(newStore()))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/echo"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dialing %s: %v", url, err)
	}
	defer conn.Close()

	for _, tc := range []struct {
		typ int
		msg string
	}{
		{websocket.TextMessage, "hello"},
		{websocket.BinaryMessage, "\x00\x01\x02"},
		{websocket.TextMessage, "a second message on the same connection"},
	} {
		if err := conn.WriteMessage(tc.typ, []byte(tc.msg)); err != nil {
			t.Fatalf("write: %v", err)
		}
		typ, got, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ != tc.typ || string(got) != tc.msg {
			t.Errorf("echo = (%d, %q), want (%d, %q)", typ, got, tc.typ, tc.msg)
		}
	}
}

// TestWebSocketRejectsCrossOrigin pins down the origin check described
// on upgrader in main.go: a handshake that claims to come from another
// site's page is refused with 403.
func TestWebSocketRejectsCrossOrigin(t *testing.T) {
	srv := httptest.NewServer(newHTTPHandler(newStore()))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/echo"
	_, resp, err := websocket.DefaultDialer.Dial(url, http.Header{"Origin": {"https://evil.example"}})
	if err == nil {
		t.Fatal("cross-origin dial succeeded, want it rejected")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("want a 403 for a cross-origin upgrade, got %v", resp)
	}
}

// --- gRPC on -grpc-addr ---

// startGRPC serves s over gRPC on a random local port and returns a
// client connection to it. The connection uses grpc-go's standard
// protobuf codec, as any ordinary gRPC client would. The server is
// stopped and the connection closed when the test ends.
func startGRPC(t *testing.T, s *store) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newGRPCServer(s)
	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// widgetsDesc builds, by hand, the same descriptor protoc would produce
// from widgets.proto. The tests send and receive real protobuf messages
// built from it, so they check the server against the published
// contract rather than against its own encoder.
func widgetsDesc(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	field := func(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type, label descriptorpb.FieldDescriptorProto_Label, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{
			Name:     proto.String(name),
			JsonName: proto.String(name),
			Number:   proto.Int32(num),
			Type:     typ.Enum(),
			Label:    label.Enum(),
		}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	msg := func(name string, fields ...*descriptorpb.FieldDescriptorProto) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{Name: proto.String(name), Field: fields}
	}
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("widgets.proto"),
		Package: proto.String("widgets.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			msg("Widget",
				field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, opt, ""),
				field("name", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING, opt, "")),
			msg("ListWidgetsRequest"),
			msg("ListWidgetsResponse",
				field("widgets", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE,
					descriptorpb.FieldDescriptorProto_LABEL_REPEATED, ".widgets.v1.Widget")),
			msg("GetWidgetRequest", field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, opt, "")),
			msg("CreateWidgetRequest", field("name", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING, opt, "")),
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("building widgets.proto descriptor: %v", err)
	}
	return fd
}

// newMsg returns an empty dynamic message of the named widgets.v1 type.
// A dynamicpb.Message is a real proto.Message built from a descriptor at
// run time, so it is encoded and decoded by the standard protobuf
// library, the same code generated stubs would use.
func newMsg(fd protoreflect.FileDescriptor, name string) *dynamicpb.Message {
	return dynamicpb.NewMessage(fd.Messages().ByName(protoreflect.Name(name)))
}

// callTimeout bounds a single RPC, so a broken server fails the test
// quickly instead of hanging it.
func callTimeout(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestGRPCHealth calls the standard health service with its generated
// client, both for the whole server ("") and for WidgetService by name.
// It also shows that generated proto messages still work through the
// forced wireCodec.
func TestGRPCHealth(t *testing.T) {
	conn := startGRPC(t, newStore())
	for _, svc := range []string{"", widgetServiceName} {
		resp, err := healthpb.NewHealthClient(conn).Check(callTimeout(t), &healthpb.HealthCheckRequest{Service: svc})
		if err != nil {
			t.Fatalf("Health/Check(%q): %v", svc, err)
		}
		if resp.Status != healthpb.HealthCheckResponse_SERVING {
			t.Errorf("Health/Check(%q) = %v, want SERVING", svc, resp.Status)
		}
	}
}

// TestGRPCListWidgets decodes ListWidgets' hand-encoded response with
// the real protobuf library and checks the seeded widgets come back.
func TestGRPCListWidgets(t *testing.T) {
	fd := widgetsDesc(t)
	conn := startGRPC(t, newStore())

	resp := newMsg(fd, "ListWidgetsResponse")
	if err := conn.Invoke(callTimeout(t), "/widgets.v1.WidgetService/ListWidgets", newMsg(fd, "ListWidgetsRequest"), resp); err != nil {
		t.Fatalf("ListWidgets: %v", err)
	}
	list := resp.Get(resp.Descriptor().Fields().ByName("widgets")).List()
	if list.Len() != 3 {
		t.Fatalf("got %d widgets, want 3", list.Len())
	}
	first := list.Get(0).Message()
	if id, name := first.Get(first.Descriptor().Fields().ByName("id")).Int(), first.Get(first.Descriptor().Fields().ByName("name")).String(); id != 1 || name != "sprocket" {
		t.Errorf("first widget = {%d %q}, want {1 \"sprocket\"}", id, name)
	}
}

// TestGRPCGetWidget covers both outcomes: a known id returns the widget,
// and an unknown one returns NOT_FOUND (REST's 404).
func TestGRPCGetWidget(t *testing.T) {
	fd := widgetsDesc(t)
	conn := startGRPC(t, newStore())

	req := newMsg(fd, "GetWidgetRequest")
	req.Set(req.Descriptor().Fields().ByName("id"), protoreflect.ValueOfInt64(2))
	resp := newMsg(fd, "Widget")
	if err := conn.Invoke(callTimeout(t), "/widgets.v1.WidgetService/GetWidget", req, resp); err != nil {
		t.Fatalf("GetWidget(2): %v", err)
	}
	if name := resp.Get(resp.Descriptor().Fields().ByName("name")).String(); name != "gear" {
		t.Errorf("GetWidget(2).name = %q, want \"gear\"", name)
	}

	req.Set(req.Descriptor().Fields().ByName("id"), protoreflect.ValueOfInt64(999))
	err := conn.Invoke(callTimeout(t), "/widgets.v1.WidgetService/GetWidget", req, newMsg(fd, "Widget"))
	if status.Code(err) != codes.NotFound {
		t.Errorf("GetWidget(999) error = %v, want NotFound", err)
	}
}

// TestGRPCCreateWidget_SharedWithREST creates a widget over gRPC, then
// reads it back over REST from the same store, proving both APIs share
// one data set.
func TestGRPCCreateWidget_SharedWithREST(t *testing.T) {
	fd := widgetsDesc(t)
	s := newStore()
	s.failPercent = 0
	conn := startGRPC(t, s)

	req := newMsg(fd, "CreateWidgetRequest")
	req.Set(req.Descriptor().Fields().ByName("name"), protoreflect.ValueOfString("flange"))
	resp := newMsg(fd, "Widget")
	if err := conn.Invoke(callTimeout(t), "/widgets.v1.WidgetService/CreateWidget", req, resp); err != nil {
		t.Fatalf("CreateWidget: %v", err)
	}
	id := resp.Get(resp.Descriptor().Fields().ByName("id")).Int()
	if id != 4 {
		t.Errorf("created id = %d, want 4", id)
	}

	// The same store backs the REST API.
	srv := httptest.NewServer(newHTTPHandler(s))
	defer srv.Close()
	httpResp, err := http.Get(srv.URL + "/widgets/4")
	if err != nil {
		t.Fatal(err)
	}
	defer httpResp.Body.Close()
	var w Widget
	if err := json.NewDecoder(httpResp.Body).Decode(&w); err != nil || w.Name != "flange" {
		t.Errorf("GET /widgets/4 = %+v (err %v), want the widget created over gRPC", w, err)
	}
}

// TestGRPCCreateWidget_Errors checks both refusal paths of
// store.createChecked as seen over gRPC: a missing name is
// INVALID_ARGUMENT, and the injected failure (forced to 100% here) is
// UNAVAILABLE.
func TestGRPCCreateWidget_Errors(t *testing.T) {
	fd := widgetsDesc(t)

	s := newStore()
	s.failPercent = 0
	conn := startGRPC(t, s)
	err := conn.Invoke(callTimeout(t), "/widgets.v1.WidgetService/CreateWidget", newMsg(fd, "CreateWidgetRequest"), newMsg(fd, "Widget"))
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateWidget with no name: error = %v, want InvalidArgument", err)
	}

	failing := newStore()
	failing.failPercent = 100
	conn = startGRPC(t, failing)
	req := newMsg(fd, "CreateWidgetRequest")
	req.Set(req.Descriptor().Fields().ByName("name"), protoreflect.ValueOfString("flange"))
	err = conn.Invoke(callTimeout(t), "/widgets.v1.WidgetService/CreateWidget", req, newMsg(fd, "Widget"))
	if status.Code(err) != codes.Unavailable {
		t.Errorf("CreateWidget with injected failure: error = %v, want Unavailable", err)
	}
}

// rawClientCodec mimics VegaLoad's gRPC driver: raw bytes in and out,
// under a content-subtype no standard server knows. It is registered
// under its own test-only name, so it can't clash with anything the real
// driver registers.
type rawClientCodec struct{}

func (rawClientCodec) Name() string                  { return "vegaload-raw-test" }
func (rawClientCodec) Marshal(v any) ([]byte, error) { return v.([]byte), nil }
func (rawClientCodec) Unmarshal(data []byte, v any) error {
	*(v.(*[]byte)) = append([]byte(nil), data...)
	return nil
}

// TestGRPCAcceptsRawBytesClients is the test closest to how VegaLoad
// itself talks to the app. It sends hand-written bytes under a
// non-standard content-subtype, using the same bytes the README gives
// for -body, and checks the server answers instead of rejecting an
// unknown codec. This is the behavior ForceServerCodec buys (see
// wireCodec in grpc.go).
func TestGRPCAcceptsRawBytesClients(t *testing.T) {
	encoding.RegisterCodec(rawClientCodec{})
	conn := startGRPC(t, newStore())
	opt := grpc.CallContentSubtype(rawClientCodec{}.Name())

	// The bytes the README tells VegaLoad users to send with -body.
	var out []byte
	if err := conn.Invoke(callTimeout(t), "/grpc.health.v1.Health/Check", []byte{}, &out, opt); err != nil {
		t.Fatalf("Health/Check with raw bytes: %v", err)
	}
	if err := conn.Invoke(callTimeout(t), "/widgets.v1.WidgetService/GetWidget", []byte("\x08\x01"), &out, opt); err != nil {
		t.Fatalf("GetWidget with raw bytes: %v", err)
	}
	if want := string(encodeWidget(Widget{ID: 1, Name: "sprocket"})); string(out) != want {
		t.Errorf("GetWidget raw response = %q, want %q", out, want)
	}
}
