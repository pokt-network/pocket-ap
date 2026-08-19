package transport

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-ap/domain"
)

// --- the header/trailer split ------------------------------------------------

func TestIsGRPCTrailerHeader(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"grpc-status", true},
		{"Grpc-Status", true}, // http.Header canonicalises; the miner folds under this form
		{"GRPC-STATUS", true},
		{"grpc-message", true},
		{"grpc-status-details-bin", true},
		{"content-type", false},
		{"grpc-encoding", false}, // a real header: it describes the body, not the outcome
		{"grpc-accept-encoding", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isGRPCTrailerHeader(tt.name); got != tt.want {
				t.Errorf("isGRPCTrailerHeader(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// The status must leave the headers entirely: a gRPC client handed grpc-status
// as a header alongside a body cannot interpret the reply.
func TestSplitGRPCTrailers(t *testing.T) {
	header := map[string][]string{
		"Content-Type":  {"application/grpc"},
		"Grpc-Status":   {"0"},
		"Grpc-Message":  {""},
		"Grpc-Encoding": {"gzip"},
	}

	headers, trailers := splitGRPCTrailers(header)

	if _, ok := headers["Grpc-Status"]; ok {
		t.Error("Grpc-Status stayed in the headers — a client cannot read a status that arrives before the body")
	}
	if got := trailers["Grpc-Status"]; len(got) != 1 || got[0] != "0" {
		t.Errorf("Grpc-Status trailer = %v, want [0]", got)
	}
	if _, ok := trailers["Grpc-Message"]; !ok {
		t.Error("Grpc-Message did not move to the trailers")
	}
	// grpc-encoding describes the body, so it is a genuine header.
	if got := headers["Grpc-Encoding"]; len(got) != 1 || got[0] != "gzip" {
		t.Errorf("Grpc-Encoding = %v, want it left as a header", got)
	}
	if got := headers["Content-Type"]; len(got) != 1 {
		t.Errorf("Content-Type = %v, want it left as a header", got)
	}
}

func TestSplitGRPCTrailers_NoGRPCHeaders(t *testing.T) {
	headers, trailers := splitGRPCTrailers(map[string][]string{"Content-Type": {"application/json"}})
	if len(trailers) != 0 {
		t.Errorf("trailers = %v, want none", trailers)
	}
	if len(headers) != 1 {
		t.Errorf("headers = %v, want the one header kept", headers)
	}
}

// --- end to end, with a REAL gRPC client -------------------------------------

// grpcRelay returns a relay result shaped like the miner's: a gRPC message body
// and grpc-status folded into the headers.
func grpcRelay(status string, body []byte) StreamFunc {
	return func(_ context.Context, _ domain.ServiceID, _ domain.RPCType, _ domain.RelayInput, onBatch func(*domain.RelayResult) error) error {
		return onBatch(&domain.RelayResult{
			StatusCode: 200,
			Header: map[string][]string{
				"Content-Type": {"application/grpc"},
				"Grpc-Status":  {status},
			},
			Body: body,
		})
	}
}

// startGRPCListener runs a WS-free HTTP listener for the gRPC type, on a real port.
func startGRPCListener(t *testing.T, stream StreamFunc) string {
	t.Helper()
	addr := freeAddr(t)
	h := NewHTTP(addr, "juno", domain.RPCTypeGRPC, nil, nil, []string{"*"})
	h.stream = stream

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Serve did not return")
		}
	})

	waitForListener(t, addr)
	return addr
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("listener never came up")
}

// THE test: a real grpc-go client must be able to talk to this listener at all.
// Without h2c it cannot even connect — Go's http.Server negotiates HTTP/2 only
// over TLS ALPN, and a gRPC client on a plain socket speaks h2c.
func TestHTTP_GRPCListenerAcceptsARealGRPCClient(t *testing.T) {
	// A single gRPC frame: 1 byte compression flag, 4 bytes big-endian length,
	// then the message.
	frame := []byte{0x00, 0x00, 0x00, 0x00, 0x02, 0x08, 0x2a}
	addr := startGRPCListener(t, grpcRelay("0", frame))

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Invoke with raw codec-free bytes: we only care that the transport works.
	var reply []byte
	err = conn.Invoke(ctx, "/pocket.service.RelayService/SendRelay", []byte{}, &reply, grpc.ForceCodec(rawCodec{}))
	if err != nil {
		t.Fatalf("a real gRPC client could not complete the call: %v", err)
	}
}

// grpc-status: 0 in the TRAILERS is what tells the client the call succeeded. If
// it arrived as a header, grpc-go reports "malformed header" or hangs.
func TestHTTP_GRPCStatusArrivesAsATrailer(t *testing.T) {
	frame := []byte{0x00, 0x00, 0x00, 0x00, 0x02, 0x08, 0x2a}
	addr := startGRPCListener(t, grpcRelay("0", frame))

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var reply []byte
	err = conn.Invoke(ctx, "/pocket.service.RelayService/SendRelay", []byte{}, &reply, grpc.ForceCodec(rawCodec{}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if st, _ := status.FromError(err); st.Code() != codes.OK {
		t.Errorf("status = %v, want OK", st.Code())
	}
	// The message must survive: the frame is what the backend actually sent.
	if len(reply) == 0 {
		t.Error("the gRPC message body did not reach the client")
	}
}

// A non-zero grpc-status must reach the client as that status, not as a generic
// transport failure — otherwise every backend error looks like a broken proxy.
func TestHTTP_NonZeroGRPCStatusReachesTheClient(t *testing.T) {
	// 5 = NotFound.
	addr := startGRPCListener(t, grpcRelay("5", nil))

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var reply []byte
	err = conn.Invoke(ctx, "/pocket.service.RelayService/SendRelay", []byte{}, &reply, grpc.ForceCodec(rawCodec{}))
	if err == nil {
		t.Fatal("a grpc-status of 5 was reported as success")
	}
	if st, _ := status.FromError(err); st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound — the backend's status must survive the relay", st.Code())
	}
}

// h2c must be on for gRPC and OFF for everything else.
//
// Tested by protocol, not by whether a gRPC call errors: without h2c the
// CONNECTION fails, with h2c it connects and then fails on a non-gRPC response —
// both produce an error, so the obvious test proves nothing. An HTTP/2
// prior-knowledge client distinguishes them.
func h2cClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
}

func TestHTTP_H2CEnabledOnGRPCListeners(t *testing.T) {
	addr := startGRPCListener(t, grpcRelay("0", []byte("x")))

	resp, err := h2cClient().Post("http://"+addr, "application/grpc", strings.NewReader(""))
	if err != nil {
		t.Fatalf("an HTTP/2 cleartext client could not reach the gRPC listener: %v", err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Errorf("proto = %s, want HTTP/2 — a gRPC client cannot speak anything else", resp.Proto)
	}
}

func TestHTTP_H2CDisabledOnStatelessListeners(t *testing.T) {
	addr := freeAddr(t)
	h := NewHTTP(addr, "eth", domain.RPCTypeJSONRPC, (&countingRelay{}).fn, nil, []string{"*"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Serve(ctx) }()
	waitForListener(t, addr)

	// HTTP/2 prior knowledge must NOT be served here.
	resp, err := h2cClient().Post("http://"+addr, "application/json", strings.NewReader(`{}`))
	if err == nil {
		_ = resp.Body.Close()
		t.Errorf("the JSON-RPC listener served HTTP/2 (proto %s) — h2c is on where it should not be", resp.Proto)
	}

	// And plain HTTP/1.1 still works there.
	plain, err := http.Post("http://"+addr, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("HTTP/1.1 POST failed: %v", err)
	}
	defer plain.Body.Close()
	if plain.StatusCode != http.StatusOK {
		t.Errorf("code = %d, want 200", plain.StatusCode)
	}
}

// Trailers must not be invented for non-gRPC listeners: a JSON-RPC backend that
// happened to send a "Grpc-Status" header should see it passed through as a
// header, untouched, because we are a transparent proxy for everything else.
func TestHTTP_NonGRPCListenerDoesNotSplitTrailers(t *testing.T) {
	h := newStreamHTTP(grpcRelay("0", []byte("body")))
	h.rpcType = domain.RPCTypeJSONRPC

	rec := newFlushRecorder()
	h.handle(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`)))

	if got := rec.Header().Get("Grpc-Status"); got != "0" {
		t.Errorf("Grpc-Status = %q on a JSON-RPC listener, want it passed through as a header", got)
	}
}

// rawCodec passes bytes through, so the tests exercise the transport rather than
// protobuf.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) {
	if b, ok := v.([]byte); ok {
		return b, nil
	}
	return nil, nil
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	if p, ok := v.(*[]byte); ok {
		*p = append((*p)[:0], data...)
	}
	return nil
}

func (rawCodec) Name() string { return "raw" }
