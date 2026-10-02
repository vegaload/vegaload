package main

// grpc.go is the sample app's gRPC side. It serves two services on
// -grpc-addr (default 127.0.0.1:9090):
//
//   - grpc.health.v1.Health, the standard health-checking service that
//     ships with grpc-go. It is the easiest gRPC target to point VegaLoad
//     at, because its request can be an empty message (no -body needed).
//   - widgets.v1.WidgetService, described in widgets.proto: list, get and
//     create, backed by the same *store as the REST API in main.go.
//
// # Why there is no generated code
//
// The usual way to build a gRPC server is to run protoc on a .proto file
// and implement the Go interface it generates. That would force anyone
// editing this demo to install protoc and two Go plugins, and keep the
// generated files in sync, for a three-method service.
//
// Instead, the WidgetService handlers receive each request as the raw
// protobuf bytes off the wire and build each response the same way,
// using the protowire package (the low-level encoder/decoder underneath
// google.golang.org/protobuf). On the wire this is ordinary protobuf,
// byte for byte what widgets.proto describes, so every kind of gRPC
// client works against it:
//
//   - VegaLoad, which sends its -body flag as already-encoded bytes;
//   - grpcurl, given -proto widgets.proto;
//   - stubs generated from widgets.proto in any language.
//
// protocols_test.go proves this by talking to the server with real
// protobuf messages built from a descriptor matching widgets.proto,
// rather than with this file's own encoder.
//
// # Protobuf wire format, in brief
//
// A message is a sequence of fields. Each field starts with a varint
// "tag" = (field number << 3) | wire type. The two wire types used here:
//
//	0 (varint): an integer, e.g. int64 id.     Tag for field 1: 1<<3|0 = 0x08
//	2 (bytes):  a length, then that many bytes; Tag for field 1: 1<<3|2 = 0x0a
//	            used for strings and nested     Tag for field 2: 2<<3|2 = 0x12
//	            messages.
//
// So GetWidgetRequest{id: 2} is the two bytes 08 02, and
// CreateWidgetRequest{name: "flange"} is 0a 06 66 6c 61 6e 67 65
// (tag, length 6, then the UTF-8 bytes). Those are the -body values the
// README gives for VegaLoad. A field with its default value (0, "") is
// simply omitted, which is why an empty -body is a valid empty message.

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// wireCodec is the one codec this server uses for every call. A gRPC
// codec turns a Go value into message bytes and back; grpc-go normally
// picks one per call from the request's content-subtype (the part after
// "application/grpc+" in the content-type header), defaulting to proto.
//
// This codec handles two kinds of value:
//
//   - *[]byte / []byte: passed through untouched. The WidgetService
//     handlers below use this to see the raw request bytes and return raw
//     response bytes.
//   - proto.Message: marshalled with the standard protobuf library. The
//     health service's generated types go down this path, so it behaves
//     exactly as it would on a normal server.
//
// It is installed with grpc.ForceServerCodec (see newGRPCServer), which
// makes the server ignore the content-subtype entirely. That matters for
// VegaLoad: its gRPC driver sends "application/grpc+vegaload-raw", a
// subtype no standard server has a codec for. Stock grpc-go servers
// currently fall back to proto with a logged warning (and say a future
// release will reject it instead); forcing the codec here means the
// sample app accepts VegaLoad's requests regardless of that.
type wireCodec struct{}

// Name is required by the encoding.Codec interface. With
// ForceServerCodec the name is never used to look the codec up, so it
// just reports the format the bytes are actually in.
func (wireCodec) Name() string { return "proto" }

// Marshal encodes a handler's response for the wire.
func (wireCodec) Marshal(v any) ([]byte, error) {
	switch m := v.(type) {
	case []byte:
		// A WidgetService handler already encoded its response.
		return m, nil
	case proto.Message:
		// A generated message, e.g. HealthCheckResponse.
		return proto.Marshal(m)
	default:
		return nil, fmt.Errorf("wireCodec: cannot marshal %T", v)
	}
}

// Unmarshal decodes an incoming request into whatever the handler asked
// for via its dec callback.
func (wireCodec) Unmarshal(data []byte, v any) error {
	switch m := v.(type) {
	case *[]byte:
		// Copy rather than alias: grpc-go may reuse data's buffer once
		// Unmarshal returns.
		*m = append((*m)[:0], data...)
		return nil
	case proto.Message:
		return proto.Unmarshal(data, m)
	default:
		return fmt.Errorf("wireCodec: cannot unmarshal into %T", v)
	}
}

// newGRPCServer builds the gRPC server: wireCodec forced for every call,
// the health service, and WidgetService backed by s. The caller starts
// it with Serve on a listener (see main).
func newGRPCServer(s *store) *grpc.Server {
	srv := grpc.NewServer(grpc.ForceServerCodec(wireCodec{}))

	// The health service answers Check for named services. "" is the
	// conventional name for "the server as a whole"; registering the
	// widget service's own name too lets a client check it specifically.
	// Both always report SERVING: this sample has no dependency that
	// could make it unhealthy.
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	hs.SetServingStatus(widgetServiceName, healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, hs)

	// s is passed as the service implementation; grpc-go hands it back
	// to each handler as the srv argument (see rawUnary).
	srv.RegisterService(&widgetServiceDesc, s)
	return srv
}

// widgetServiceName is the fully qualified service name from
// widgets.proto ("package.Service"). A client calls a method as
// "/widgets.v1.WidgetService/<Method>".
const widgetServiceName = "widgets.v1.WidgetService"

// widgetServiceDesc is what protoc would normally generate: the table
// grpc-go uses to route "/widgets.v1.WidgetService/<Method>" to a
// handler. Writing it by hand is the one piece of "generated code" this
// file replaces, and it is only a few lines.
var widgetServiceDesc = grpc.ServiceDesc{
	ServiceName: widgetServiceName,
	// RegisterService checks that the implementation passed to it
	// satisfies HandlerType. Generated code puts a service interface
	// here; the empty interface accepts any value, which suits the
	// *store this file passes in.
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "ListWidgets", Handler: rawUnary(listWidgets)},
		{MethodName: "GetWidget", Handler: rawUnary(getWidget)},
		{MethodName: "CreateWidget", Handler: rawUnary(createWidget)},
	},
	// No streaming RPCs: VegaLoad's gRPC driver only makes unary calls.
	Streams: nil,
	// Informational only (shown by some tooling); names the contract.
	Metadata: "widgets.proto",
}

// rawUnary adapts a simple "request bytes in, response bytes out"
// function to grpc.MethodHandler, the signature grpc-go calls for every
// unary RPC. It:
//
//  1. asks grpc-go to decode the request with dec. Because the target is
//     a *[]byte, wireCodec hands over the raw message bytes;
//  2. recovers the *store registered in newGRPCServer from srv;
//  3. runs fn directly, or through the server's interceptor if one is
//     configured. None is today, but generated code always honours
//     interceptors, so this does too and adding one later just works.
//
// fn returns either encoded response bytes or an error. Errors made with
// status.Error(code, msg) reach the client with that gRPC status code,
// which is what VegaLoad records as the call's status; any other error
// would arrive as codes.Unknown.
func rawUnary(fn func(s *store, req []byte) ([]byte, error)) grpc.MethodHandler {
	return func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		var req []byte
		if err := dec(&req); err != nil {
			return nil, err
		}
		s := srv.(*store)
		if interceptor == nil {
			return fn(s, req)
		}
		info := &grpc.UnaryServerInfo{Server: srv}
		return interceptor(ctx, req, info, func(ctx context.Context, r any) (any, error) {
			return fn(s, r.([]byte))
		})
	}
}

// listWidgets implements ListWidgets(ListWidgetsRequest) returns
// (ListWidgetsResponse). The request has no fields, so its bytes are
// ignored. Same 10-40ms latency as GET /widgets.
func listWidgets(s *store, _ []byte) ([]byte, error) {
	simulateLatency(10, 40)
	// ListWidgetsResponse { repeated Widget widgets = 1; }
	// A repeated message field is encoded as one length-delimited field
	// 1 per element, each wrapping a complete encoded Widget.
	var out []byte
	for _, w := range s.list() {
		out = protowire.AppendTag(out, 1, protowire.BytesType)
		out = protowire.AppendBytes(out, encodeWidget(w))
	}
	// Zero widgets encodes as zero bytes: a valid, empty response.
	return out, nil
}

// getWidget implements GetWidget(GetWidgetRequest) returns (Widget).
// Same 10-40ms latency as GET /widgets/{id}; NOT_FOUND plays the role of
// the REST API's 404.
func getWidget(s *store, req []byte) ([]byte, error) {
	simulateLatency(10, 40)

	// GetWidgetRequest { int64 id = 1; } -- pick out field 1 if present.
	// If it's absent, id stays 0 (protobuf's default), which matches no
	// widget and so falls through to NOT_FOUND.
	var id int64
	err := decodeFields(req, func(num protowire.Number, typ protowire.Type, b []byte) (int, error) {
		if num == 1 && typ == protowire.VarintType {
			v, n := protowire.ConsumeVarint(b)
			id = int64(v)
			return n, protowire.ParseError(n) // nil when n >= 0
		}
		return -1, nil // not a field we know: let decodeFields skip it
	})
	if err != nil {
		// Truncated or otherwise unparseable bytes.
		return nil, status.Errorf(codes.InvalidArgument, "malformed GetWidgetRequest: %v", err)
	}

	w, ok := s.get(int(id))
	if !ok {
		return nil, status.Errorf(codes.NotFound, "widget %d not found", id)
	}
	return encodeWidget(w), nil
}

// createWidget implements CreateWidget(CreateWidgetRequest) returns
// (Widget). It shares store.createChecked with POST /widgets, so it has
// the same 20-60ms latency and the same ~3% injected failure, reported
// here as UNAVAILABLE (gRPC's conventional "transient, retry later"
// code) where REST uses 500. A missing or empty name is
// INVALID_ARGUMENT, REST's 400.
func createWidget(s *store, req []byte) ([]byte, error) {
	// CreateWidgetRequest { string name = 1; }
	var name string
	err := decodeFields(req, func(num protowire.Number, typ protowire.Type, b []byte) (int, error) {
		if num == 1 && typ == protowire.BytesType {
			v, n := protowire.ConsumeString(b)
			name = v
			return n, protowire.ParseError(n)
		}
		return -1, nil
	})
	if err != nil {
		// Most often a hand-written -body whose length byte doesn't
		// match the name, e.g. $'\x0a\x05flange' (5) for a 6-letter
		// name: the decoder runs out of bytes and reports that here.
		// Note this fails fast, before createChecked's latency.
		return nil, status.Errorf(codes.InvalidArgument, "malformed CreateWidgetRequest: %v", err)
	}

	w, err := s.createChecked(name)
	switch {
	case errors.Is(err, errTransient):
		return nil, status.Error(codes.Unavailable, err.Error())
	case err != nil:
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return encodeWidget(w), nil
}

// encodeWidget encodes Widget { int64 id = 1; string name = 2; }.
// Both fields are always written. Strictly, proto3 omits fields that hold
// their default value, but every real widget has a non-zero id and a
// non-empty name, so the output is identical to what protoc-generated
// code would produce.
func encodeWidget(w Widget) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.VarintType) // 0x08
	b = protowire.AppendVarint(b, uint64(w.ID))
	b = protowire.AppendTag(b, 2, protowire.BytesType) // 0x12
	b = protowire.AppendString(b, w.Name)              // length, then bytes
	return b
}

// decodeFields walks every field of an encoded protobuf message in order.
// For each one it reads the tag, then calls field with the field number,
// its wire type, and the bytes that follow the tag.
//
// field must return one of:
//
//   - (n >= 0, nil): it consumed the field's value, which was n bytes;
//   - (-1, nil): it doesn't know this field. decodeFields skips the value
//     using the wire type alone. That is protobuf's forward-compatibility
//     rule: a newer client may send fields an older server ignores;
//   - (_, err): the value was malformed. Decoding stops and err is
//     returned.
//
// Any malformed tag or skipped value also stops decoding with an error.
func decodeFields(msg []byte, field func(num protowire.Number, typ protowire.Type, b []byte) (int, error)) error {
	for len(msg) > 0 {
		num, typ, n := protowire.ConsumeTag(msg)
		if n < 0 {
			return protowire.ParseError(n)
		}
		msg = msg[n:]

		n, err := field(num, typ, msg)
		if err != nil {
			return err
		}
		if n < 0 {
			n = protowire.ConsumeFieldValue(num, typ, msg)
			if n < 0 {
				return protowire.ParseError(n)
			}
		}
		msg = msg[n:]
	}
	return nil
}
