package pocket

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-ap/domain"
)

// fakeMiner is a relay miner's gRPC service: an UnknownServiceHandler on the
// relay method path, exactly as pocket-relay-miner registers it
// (relay_grpc_service.go RegisterWithServer).
type fakeMiner struct {
	srv  *grpc.Server
	addr string

	mu         sync.Mutex
	gotMethod  string
	gotRequest *servicetypes.RelayRequest
	gotMD      metadata.MD
	calls      int

	reply    *servicetypes.RelayResponse
	replyErr error
}

func newFakeMiner(t *testing.T) *fakeMiner {
	t.Helper()
	f := &fakeMiner{reply: &servicetypes.RelayResponse{Payload: []byte("pong")}}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f.addr = lis.Addr().String()

	f.srv = grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.Method(stream.Context())
		md, _ := metadata.FromIncomingContext(stream.Context())

		f.mu.Lock()
		f.gotMethod = method
		f.mu.Unlock()

		// The real miner rejects anything but its relay path
		// (relay_grpc_service.go HandleUnknownService). A fake that answers every
		// method would let a wrong path pass unnoticed.
		if method != relayServiceMethodPath {
			return status.Errorf(codes.Unimplemented, "unknown method: %s", method)
		}

		req := &servicetypes.RelayRequest{}
		if err := stream.RecvMsg(req); err != nil {
			return err
		}

		f.mu.Lock()
		f.calls++
		f.gotRequest = req
		f.gotMD = md
		reply, replyErr := f.reply, f.replyErr
		f.mu.Unlock()

		if replyErr != nil {
			return replyErr
		}
		return stream.SendMsg(reply)
	}))

	go func() { _ = f.srv.Serve(lis) }()
	t.Cleanup(f.srv.Stop)
	return f
}

func (f *fakeMiner) seen() (method string, req *servicetypes.RelayRequest, md metadata.MD, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotMethod, f.gotRequest, f.gotMD, f.calls
}

func signedRequest() []byte {
	req := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:    "pokt1app",
				ServiceId:             "juno",
				SessionEndBlockHeight: 476840,
			},
			SupplierOperatorAddress: "pokt1supplier",
		},
		Payload: []byte("grpc-frame-bytes"),
	}
	bz, _ := req.Marshal()
	return bz
}

// --- grpcTarget --------------------------------------------------------------

// Suppliers advertise http(s) URLs; gRPC dials host:port. The scheme is the only
// TLS signal, so it must be read rather than stripped — the miner's own client
// strips it and always dials insecure, which works locally and fails against any
// real https endpoint.
func TestGRPCTarget(t *testing.T) {
	tests := []struct {
		name       string
		url        string
		wantTarget string
		wantTLS    bool
		wantErr    bool
	}{
		{"https gets TLS and the default port", "https://rm.example.com", "rm.example.com:443", true, false},
		{"https with an explicit port", "https://rm.example.com:8443", "rm.example.com:8443", true, false},
		{"http is insecure, default port", "http://rm.example.com", "rm.example.com:80", false, false},
		{"http with an explicit port", "http://127.0.0.1:9000", "127.0.0.1:9000", false, false},
		{"grpcs is TLS", "grpcs://rm.example.com:443", "rm.example.com:443", true, false},
		{"grpc is insecure", "grpc://rm.example.com:9000", "rm.example.com:9000", false, false},
		{"scheme case is ignored", "HTTPS://rm.example.com", "rm.example.com:443", true, false},
		{"bare host:port passes through", "127.0.0.1:9000", "127.0.0.1:9000", false, false},
		{"unknown scheme is refused", "ftp://rm.example.com", "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, tls, err := grpcTarget(tt.url)
			if (err != nil) != tt.wantErr {
				t.Fatalf("grpcTarget(%q) err = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if target != tt.wantTarget {
				t.Errorf("target = %q, want %q", target, tt.wantTarget)
			}
			if tls != tt.wantTLS {
				t.Errorf("useTLS = %v, want %v — the scheme is the only TLS signal", tls, tt.wantTLS)
			}
		})
	}
}

// --- Send, against a real gRPC server ----------------------------------------

// The relay must arrive as the TYPED RelayRequest the miner expects, on the
// method path it registers, with the routing metadata it reads.
func TestGRPCSender_SendsTypedRequestOnTheRelayMethodPath(t *testing.T) {
	miner := newFakeMiner(t)
	s := NewGRPCSender()
	defer s.Close()

	respBz, err := s.Send(context.Background(), "http://"+miner.addr, signedRequest(), domain.RPCTypeGRPC)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	method, req, _, _ := miner.seen()
	if method != relayServiceMethodPath {
		t.Errorf("method = %q, want %q", method, relayServiceMethodPath)
	}
	if req == nil {
		t.Fatal("the miner received no RelayRequest")
	}
	// The signed request must survive the marshal/unmarshal round trip intact:
	// the payload is what gets hashed for onchain proof verification.
	if string(req.Payload) != "grpc-frame-bytes" {
		t.Errorf("payload = %q, want it carried verbatim", req.Payload)
	}
	if req.Meta.SupplierOperatorAddress != "pokt1supplier" {
		t.Errorf("supplier = %q, want it carried", req.Meta.SupplierOperatorAddress)
	}
	if req.Meta.SessionHeader == nil || req.Meta.SessionHeader.ServiceId != "juno" {
		t.Error("the session header did not survive")
	}

	// The reply comes back marshaled, for Validator.ValidateResponse to verify.
	got := &servicetypes.RelayResponse{}
	if err := got.Unmarshal(respBz); err != nil {
		t.Fatalf("the response is not a marshaled RelayResponse: %v", err)
	}
	if string(got.Payload) != "pong" {
		t.Errorf("payload = %q, want pong", got.Payload)
	}
}

// rpc-type metadata is the gRPC analogue of the Rpc-Type header, carrying the
// same numeric contract. Get it wrong and the miner routes to the wrong backend
// — or falls back to its default, which would look like a working relay against
// the wrong service.
func TestGRPCSender_SendsRPCTypeMetadata(t *testing.T) {
	miner := newFakeMiner(t)
	s := NewGRPCSender()
	defer s.Close()

	if _, err := s.Send(context.Background(), "http://"+miner.addr, signedRequest(), domain.RPCTypeGRPC); err != nil {
		t.Fatalf("Send: %v", err)
	}

	_, _, md, _ := miner.seen()
	got := md.Get(rpcTypeMetadataKey)
	if len(got) != 1 {
		t.Fatalf("rpc-type metadata = %v, want exactly one value", got)
	}
	// GRPC == 1 in the on-chain enum, and this must agree with what the HTTP
	// sender stamps for the same type.
	if got[0] != "1" {
		t.Errorf("rpc-type = %q, want 1 (GRPC)", got[0])
	}
	if got[0] != rpcTypeHeader(domain.RPCTypeGRPC) {
		t.Errorf("rpc-type metadata %q diverged from the Rpc-Type header %q", got[0], rpcTypeHeader(domain.RPCTypeGRPC))
	}
}

// One connection per supplier, reused. grpc.NewClient is lazy so this is not
// about dial cost — it is about not leaking a client per relay.
func TestGRPCSender_ReusesOneConnectionPerURL(t *testing.T) {
	miner := newFakeMiner(t)
	s := NewGRPCSender()
	defer s.Close()

	url := "http://" + miner.addr
	for i := 0; i < 5; i++ {
		if _, err := s.Send(context.Background(), url, signedRequest(), domain.RPCTypeGRPC); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	conns := 0
	s.conns.Range(func(_, _ any) bool { conns++; return true })
	if conns != 1 {
		t.Errorf("cached connections = %d, want 1 for one URL", conns)
	}
	if _, _, _, calls := miner.seen(); calls != 5 {
		t.Errorf("miner saw %d relays, want 5", calls)
	}
}

// Concurrent first-relays to one supplier must not each build a connection.
func TestGRPCSender_ConcurrentFirstSendsDialOnce(t *testing.T) {
	miner := newFakeMiner(t)
	s := NewGRPCSender()
	defer s.Close()

	url := "http://" + miner.addr
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Send(context.Background(), url, signedRequest(), domain.RPCTypeGRPC); err != nil {
				t.Errorf("Send: %v", err)
			}
		}()
	}
	wg.Wait()

	conns := 0
	s.conns.Range(func(_, _ any) bool { conns++; return true })
	if conns != 1 {
		t.Errorf("cached connections = %d, want 1 — concurrent first sends each dialled", conns)
	}
}

func TestGRPCSender_MinerErrorPropagates(t *testing.T) {
	miner := newFakeMiner(t)
	miner.mu.Lock()
	miner.replyErr = errors.New("supplier refused")
	miner.mu.Unlock()

	s := NewGRPCSender()
	defer s.Close()

	if _, err := s.Send(context.Background(), "http://"+miner.addr, signedRequest(), domain.RPCTypeGRPC); err == nil {
		t.Fatal("Send hid a miner error")
	}
}

func TestGRPCSender_RejectsMalformedRelayRequest(t *testing.T) {
	miner := newFakeMiner(t)
	s := NewGRPCSender()
	defer s.Close()

	if _, err := s.Send(context.Background(), "http://"+miner.addr, []byte("not a protobuf at all"), domain.RPCTypeGRPC); err == nil {
		t.Fatal("Send accepted bytes that are not a RelayRequest")
	}
}

func TestGRPCSender_BadURL(t *testing.T) {
	s := NewGRPCSender()
	defer s.Close()

	if _, err := s.Send(context.Background(), "ftp://nope.example.com", signedRequest(), domain.RPCTypeGRPC); err == nil {
		t.Fatal("Send accepted an unsupported scheme")
	}
}

func TestGRPCSender_UnreachableSupplier(t *testing.T) {
	s := NewGRPCSender()
	defer s.Close()

	// Port 9 discards.
	if _, err := s.Send(context.Background(), "http://127.0.0.1:9", signedRequest(), domain.RPCTypeGRPC); err == nil {
		t.Fatal("Send succeeded against a dead supplier")
	}
}

// --- MultiSender routing -----------------------------------------------------

// The Sender seam takes rpcType precisely so a sender can route on it — which is
// why adding gRPC changed no seam.
func TestMultiSender_RoutesGRPCToTheGRPCSenderAndTheRestToHTTP(t *testing.T) {
	miner := newFakeMiner(t)
	httpSender := NewHTTPSender(5 * 1e9)
	m := NewMultiSender(httpSender, NewGRPCSender())
	defer m.Close()

	// gRPC reaches the gRPC miner.
	if _, err := m.Send(context.Background(), "http://"+miner.addr, signedRequest(), domain.RPCTypeGRPC); err != nil {
		t.Fatalf("gRPC Send: %v", err)
	}
	if _, _, _, calls := miner.seen(); calls != 1 {
		t.Errorf("the gRPC miner saw %d relays, want 1", calls)
	}

	// A non-gRPC type must NOT reach it — it would arrive as an HTTP POST and be
	// meaningless to a gRPC server.
	_, _ = m.Send(context.Background(), "http://"+miner.addr, signedRequest(), domain.RPCTypeJSONRPC)
	if _, _, _, calls := miner.seen(); calls != 1 {
		t.Errorf("a JSON-RPC relay reached the gRPC path (calls now %d)", calls)
	}
}

// A gRPC reply is one complete RelayResponse — the miner buffers even
// server-streaming and never forwards full-duplex — so it must come back with no
// streaming Content-Type, which makes RelayStream treat it as exactly one batch.
func TestMultiSender_SendStreamGRPCIsASingleBatch(t *testing.T) {
	miner := newFakeMiner(t)
	m := NewMultiSender(NewHTTPSender(5*1e9), NewGRPCSender())
	defer m.Close()

	body, header, status, err := m.SendStream(context.Background(), "http://"+miner.addr, signedRequest(), domain.RPCTypeGRPC)
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	defer body.Close()

	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	// No streaming content type: relay.isStreamingResponse must say false, or the
	// body would be split on a delimiter that is not there.
	if len(header) != 0 {
		t.Errorf("header = %v, want none — a gRPC reply must not look like a stream", header)
	}

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	resp := &servicetypes.RelayResponse{}
	if err := resp.Unmarshal(got); err != nil {
		t.Fatalf("body is not a marshaled RelayResponse: %v", err)
	}
	if string(resp.Payload) != "pong" {
		t.Errorf("payload = %q", resp.Payload)
	}
}

// With no gRPC sender wired, gRPC must not silently fall through to HTTP — that
// would lose grpc-status and look like a working relay.
func TestMultiSender_NilGRPCFallsBackToHTTP(t *testing.T) {
	m := NewMultiSender(NewHTTPSender(1e9), nil)
	// Nothing is listening; the point is only that it takes the HTTP path rather
	// than panicking on the nil.
	if _, err := m.Send(context.Background(), "http://127.0.0.1:9", signedRequest(), domain.RPCTypeGRPC); err == nil {
		t.Fatal("expected the HTTP path to fail against a dead port")
	}
}

// The method path and metadata key are the relay miner's wire constants, not
// ours (pocket-relay-miner relay_grpc_service.go:31 and resolveGRPCRelayRPCType).
//
// Pinned to literals because everything else here — including the fake miner —
// refers to them by symbol, so changing one would keep both sides agreeing and
// the suite green while no real miner answered us. Same trap as
// relay.StreamDelimiter.
func TestGRPCWireConstants(t *testing.T) {
	const (
		wireMethod      = "/pocket.service.RelayService/SendRelay"
		wireMetadataKey = "rpc-type"
	)
	if relayServiceMethodPath != wireMethod {
		t.Errorf("relayServiceMethodPath = %q, want %q — the miner returns Unimplemented for anything else",
			relayServiceMethodPath, wireMethod)
	}
	if rpcTypeMetadataKey != wireMetadataKey {
		t.Errorf("rpcTypeMetadataKey = %q, want %q — the miner reads this to pick the backend",
			rpcTypeMetadataKey, wireMetadataKey)
	}
}
