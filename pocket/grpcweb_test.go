package pocket

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	"google.golang.org/grpc"

	"github.com/pokt-network/pocket-ap/domain"
)

// --- byte-exactness ----------------------------------------------------------

// rawMiner answers with bytes chosen by the test, verbatim. ForceServerCodec is
// what makes that possible: the default proto codec would re-encode the reply
// canonically and destroy the very thing under test.
type rawMiner struct {
	srv  *grpc.Server
	addr string

	mu     sync.Mutex
	gotReq []byte
	reply  []byte
}

func newRawMiner(t *testing.T, reply []byte) *rawMiner {
	t.Helper()
	f := &rawMiner{reply: reply}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f.addr = lis.Addr().String()

	f.srv = grpc.NewServer(
		grpc.ForceServerCodec(rawCodec{}),
		grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
			var req []byte
			if err := stream.RecvMsg(&req); err != nil {
				return err
			}
			f.mu.Lock()
			f.gotReq = req
			f.mu.Unlock()
			return stream.SendMsg(&f.reply)
		}),
	)
	go func() { _ = f.srv.Serve(lis) }()
	t.Cleanup(f.srv.Stop)
	return f
}

// nonCanonicalRelayResponse hand-encodes a RelayResponse with its fields in
// descending order. Protobuf accepts any field order on the wire, but Marshal
// always emits ascending — so these bytes survive a relay only if nothing
// decoded and re-encoded them.
//
// This is not a contrived worry. The supplier signs the exact bytes it put on
// the wire, so a re-encode means ValidateResponse checks that signature against
// something the supplier never produced. Nothing about the failure would point
// here: it looks like a lying supplier, and only sometimes.
func nonCanonicalRelayResponse(t *testing.T) []byte {
	t.Helper()
	payload := []byte("grpc-reply-bytes")

	var out []byte
	// Field 2 (payload) first...
	out = append(out, 0x12, byte(len(payload)))
	out = append(out, payload...)
	// ...then field 1 (meta), an empty embedded message.
	out = append(out, 0x0a, 0x00)

	// Sanity: it must decode, or the test proves nothing about a real reply.
	var resp servicetypes.RelayResponse
	if err := resp.Unmarshal(out); err != nil {
		t.Fatalf("hand-built response does not decode: %v", err)
	}
	if canonical, _ := resp.Marshal(); string(canonical) == string(out) {
		t.Fatal("hand-built response is already canonical; it cannot detect a re-encode")
	}
	return out
}

// The reason rawCodec exists. Before it, Send unmarshaled the reply into a
// RelayResponse and re-marshaled it for the Validator.
func TestGRPCSender_ReturnsSupplierBytesVerbatim(t *testing.T) {
	want := nonCanonicalRelayResponse(t)
	miner := newRawMiner(t, want)

	s := NewGRPCSenderMode(GRPCModeNative)
	defer func() { _ = s.Close() }()

	got, err := s.Send(context.Background(), miner.addr, signedRequest(), domain.RPCTypeGRPC)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("Send re-encoded the reply:\n got %x\nwant %x\nthe supplier's signature covers the bytes it sent, not a re-encoding of them", got, want)
	}
}

// The request half of the same contract: the app signs the RelayRequest, so the
// miner has to receive the bytes we signed.
func TestGRPCSender_SendsRequestBytesVerbatim(t *testing.T) {
	miner := newRawMiner(t, nonCanonicalRelayResponse(t))

	s := NewGRPCSenderMode(GRPCModeNative)
	defer func() { _ = s.Close() }()

	want := signedRequest()
	if _, err := s.Send(context.Background(), miner.addr, want, domain.RPCTypeGRPC); err != nil {
		t.Fatalf("Send: %v", err)
	}

	miner.mu.Lock()
	got := miner.gotReq
	miner.mu.Unlock()
	if string(got) != string(want) {
		t.Errorf("miner received re-encoded request bytes:\n got %x\nwant %x", got, want)
	}
}

// --- gRPC-Web ----------------------------------------------------------------

// webMiner is a relay miner behind an ingress that terminates HTTP/2: it speaks
// gRPC-Web over ordinary HTTP/1.1 and nothing else.
type webMiner struct {
	srv *httptest.Server

	mu         mutexAlias
	gotPath    string
	gotBody    []byte
	gotRPCType string
	gotCType   string
	calls      int
	// nativeProbes counts HTTP/2 connection prefaces. Go's http.Server hands the
	// "PRI *" preface to the handler rather than rejecting it, so a native gRPC
	// attempt against an HTTP/1.1 front door is visible here — which is exactly
	// what auto mode must stop doing once it has learnt the answer.
	nativeProbes int
	reply        []byte
	statusCode   int // grpc-status put in the trailer frame
	statusMsg    string
	trailerOnly  bool
}

// mutexAlias keeps the struct literal readable; it is just a sync.Mutex.
type mutexAlias = sync.Mutex

func newWebMiner(t *testing.T, reply []byte) *webMiner {
	t.Helper()
	m := &webMiner{reply: reply}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PRI" {
			m.mu.Lock()
			m.nativeProbes++
			m.mu.Unlock()
			// Answering anything is fine: grpc-go fails on the missing HTTP/2
			// preface either way, which is what a real HTTP/1.1 front door does.
			w.WriteHeader(http.StatusHTTPVersionNotSupported)
			return
		}
		body, _ := io.ReadAll(r.Body)

		m.mu.Lock()
		m.calls++
		m.gotPath = r.URL.Path
		m.gotBody = body
		m.gotRPCType = r.Header.Get("rpc-type")
		m.gotCType = r.Header.Get("Content-Type")
		reply, code, msg, trailerOnly := m.reply, m.statusCode, m.statusMsg, m.trailerOnly
		m.mu.Unlock()

		w.Header().Set("Content-Type", grpcWebContentType)
		w.WriteHeader(http.StatusOK)

		var out []byte
		if !trailerOnly {
			out = append(out, encodeGRPCFrame(0, reply)...)
		}
		trailers := fmt.Sprintf("grpc-status: %d\r\n", code)
		if msg != "" {
			trailers += "grpc-message: " + msg + "\r\n"
		}
		out = append(out, encodeGRPCFrame(grpcTrailerFlag, []byte(trailers))...)
		_, _ = w.Write(out)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *webMiner) seen() (path string, body []byte, rpcType, ctype string, calls int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gotPath, m.gotBody, m.gotRPCType, m.gotCType, m.calls
}

func TestGRPCSender_WebModeRoundTrip(t *testing.T) {
	want := nonCanonicalRelayResponse(t)
	miner := newWebMiner(t, want)

	s := NewGRPCSenderMode(GRPCModeWeb)
	defer func() { _ = s.Close() }()

	reqBz := signedRequest()
	got, err := s.Send(context.Background(), miner.srv.URL, reqBz, domain.RPCTypeGRPC)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("reply = %x, want %x", got, want)
	}

	path, body, rpcType, ctype, calls := miner.seen()
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if path != relayServiceMethodPath {
		t.Errorf("path = %q, want %q", path, relayServiceMethodPath)
	}
	if ctype != grpcWebContentType {
		t.Errorf("Content-Type = %q, want %q", ctype, grpcWebContentType)
	}
	// The same routing contract the HTTP sender's Rpc-Type header carries; without
	// it the miner cannot tell which backend to hit.
	if want := rpcTypeHeader(domain.RPCTypeGRPC); rpcType != want {
		t.Errorf("rpc-type = %q, want %q", rpcType, want)
	}
	// The body is one length-prefixed frame around the signed request, unaltered.
	if len(body) < grpcFrameHeaderLen {
		t.Fatalf("body too short: %d bytes", len(body))
	}
	if body[0] != 0 {
		t.Errorf("request frame flag = %#x, want 0 (a data frame)", body[0])
	}
	if n := binary.BigEndian.Uint32(body[1:grpcFrameHeaderLen]); int(n) != len(reqBz) {
		t.Errorf("frame length = %d, want %d", n, len(reqBz))
	}
	if string(body[grpcFrameHeaderLen:]) != string(reqBz) {
		t.Error("framed request bytes differ from the signed request")
	}
}

// grpc-status is the only place a gRPC call says whether it worked; it is not in
// the body. Treating a non-zero status as success would hand the Validator a
// reply that is not a RelayResponse at all.
func TestGRPCSender_WebModeSurfacesNonZeroStatus(t *testing.T) {
	miner := newWebMiner(t, nil)
	miner.statusCode = 12 // UNIMPLEMENTED
	miner.statusMsg = "unknown method"
	miner.trailerOnly = true

	s := NewGRPCSenderMode(GRPCModeWeb)
	defer func() { _ = s.Close() }()

	_, err := s.Send(context.Background(), miner.srv.URL, signedRequest(), domain.RPCTypeGRPC)
	if err == nil {
		t.Fatal("Send succeeded on grpc-status 12")
	}
	if !strings.Contains(err.Error(), "12") || !strings.Contains(err.Error(), "unknown method") {
		t.Errorf("error %q loses the status code or message", err)
	}
}

// --- auto mode ---------------------------------------------------------------

// The deployment this whole mode exists for: an ingress terminates HTTP/2, so a
// native attempt cannot even complete the preface, and gRPC-Web is the only
// framing that gets through.
func TestGRPCSender_AutoFallsBackToWebAndRemembers(t *testing.T) {
	want := nonCanonicalRelayResponse(t)
	miner := newWebMiner(t, want)

	s := NewGRPCSenderMode(GRPCModeAuto)
	defer func() { _ = s.Close() }()

	got, err := s.Send(context.Background(), miner.srv.URL, signedRequest(), domain.RPCTypeGRPC)
	if err != nil {
		t.Fatalf("first Send: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("reply = %x, want %x", got, want)
	}

	host, _, err := grpcTarget(miner.srv.URL)
	if err != nil {
		t.Fatalf("grpcTarget: %v", err)
	}
	if _, remembered := s.webOnly.Load(host); !remembered {
		t.Error("host not recorded as web-only; auto mode would re-learn this on every relay")
	}

	miner.mu.Lock()
	probesAfterFirst := miner.nativeProbes
	miner.mu.Unlock()
	if probesAfterFirst != 1 {
		t.Errorf("native probes after the first relay = %d, want 1", probesAfterFirst)
	}

	if _, err := s.Send(context.Background(), miner.srv.URL, signedRequest(), domain.RPCTypeGRPC); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	if _, _, _, _, calls := miner.seen(); calls != 2 {
		t.Errorf("gRPC-Web calls = %d, want 2", calls)
	}

	// The point of the memo: the second relay makes no native attempt at all.
	// Without it every gRPC relay would pay a doomed HTTP/2 handshake first.
	miner.mu.Lock()
	probesAfterSecond := miner.nativeProbes
	miner.mu.Unlock()
	if probesAfterSecond != 1 {
		t.Errorf("native probes after the second relay = %d, want 1 — the host was re-probed", probesAfterSecond)
	}
}

// A supplier that is simply broken must not be silently retried in a different
// protocol — that turns one legible failure into two illegible ones, and hides
// the actual fault.
func TestGRPCSender_AutoDoesNotFallBackOnOtherErrors(t *testing.T) {
	miner := newFakeMiner(t)
	// Under the miner's own lock: the handler goroutine is already running, and
	// the race detector is entitled to flag a bare field write here.
	miner.mu.Lock()
	miner.replyErr = errors.New("backend on fire")
	miner.mu.Unlock()

	s := NewGRPCSenderMode(GRPCModeAuto)
	defer func() { _ = s.Close() }()

	_, err := s.Send(context.Background(), miner.addr, signedRequest(), domain.RPCTypeGRPC)
	if err == nil {
		t.Fatal("Send succeeded against a failing miner")
	}

	host, _, _ := grpcTarget(miner.addr)
	if _, remembered := s.webOnly.Load(host); remembered {
		// The error is printed because which error was misclassified is the whole
		// diagnosis, and without it this failure is unreproducible guesswork.
		t.Errorf("a plain backend error was mistaken for a framing problem; error was: %v", err)
	}
}

func TestIsNotHTTP2(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"miner 505", errors.New("rpc error: 505 HTTP Version Not Supported"), true},
		{"miner text", errors.New("gRPC requires HTTP/2"), true},
		{"plain HTTP/1.1 server", errors.New("error reading server preface: http2: frame too large, note that the frame header looked like an HTTP/1.1 header"), true},
		{"h1 header sniff", errors.New("frame looked like an HTTP/1.1 header"), true},
		{"connection refused", errors.New("connection refused"), false},
		{"backend error", errors.New("backend on fire"), false},
		// Ambiguous on purpose: a transient reset produces this too, and the
		// answer is memoized for the process lifetime. A real HTTP/1.1 front door
		// always adds the "looked like an HTTP/1.1 header" clause above.
		{"bare preface error is NOT conclusive", errors.New("error reading server preface: EOF"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNotHTTP2(tt.err); got != tt.want {
				t.Errorf("isNotHTTP2(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// --- framing helpers ---------------------------------------------------------

func TestDecodeGRPCWebResponse(t *testing.T) {
	msg := []byte("relay-response-bytes")
	body := append(encodeGRPCFrame(0, msg),
		encodeGRPCFrame(grpcTrailerFlag, []byte("grpc-status: 0\r\ngrpc-message: \r\n"))...)

	gotMsg, trailers, err := decodeGRPCWebResponse(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(gotMsg) != string(msg) {
		t.Errorf("message = %q, want %q", gotMsg, msg)
	}
	if trailers["grpc-status"] != "0" {
		t.Errorf("trailers = %v, want grpc-status 0", trailers)
	}
}

// A trailers-only reply carries no data frame. That is not an error — the status
// is the answer — so decoding must not invent one or fail.
func TestDecodeGRPCWebResponse_TrailersOnly(t *testing.T) {
	body := encodeGRPCFrame(grpcTrailerFlag, []byte("grpc-status: 5\r\ngrpc-message: not found\r\n"))

	msg, trailers, err := decodeGRPCWebResponse(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg != nil {
		t.Errorf("message = %q, want nil", msg)
	}
	code, text := grpcStatusFrom(trailers, nil)
	if code != 5 || text != "not found" {
		t.Errorf("status = (%d, %q), want (5, \"not found\")", code, text)
	}
}

// A truncated body must be an error, not a silently short message: a partial
// RelayResponse would fail signature validation and be blamed on the supplier.
func TestDecodeGRPCWebResponse_RejectsTruncatedFrames(t *testing.T) {
	full := encodeGRPCFrame(0, []byte("relay-response-bytes"))

	for _, tt := range []struct {
		name string
		body []byte
	}{
		{"header cut short", full[:3]},
		{"payload cut short", full[:len(full)-4]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := decodeGRPCWebResponse(tt.body); err == nil {
				t.Error("decode accepted a truncated body")
			}
		})
	}
}

// The status can arrive either way. The in-body frame wins: a reply carrying one
// is the more specific answer.
func TestGRPCStatusFrom_PrefersTrailerFrame(t *testing.T) {
	header := http.Header{"Grpc-Status": {"13"}, "Grpc-Message": {"from header"}}

	if code, msg := grpcStatusFrom(map[string]string{"grpc-status": "5", "grpc-message": "from frame"}, header); code != 5 || msg != "from frame" {
		t.Errorf("got (%d, %q), want (5, \"from frame\")", code, msg)
	}
	if code, msg := grpcStatusFrom(nil, header); code != 13 || msg != "from header" {
		t.Errorf("header fallback got (%d, %q), want (13, \"from header\")", code, msg)
	}
	// Absent everywhere is OK, which is what gRPC itself does.
	if code, _ := grpcStatusFrom(nil, http.Header{}); code != 0 {
		t.Errorf("absent status = %d, want 0", code)
	}
}

func TestGRPCWebURL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"https", "https://rm.example.com", "https://rm.example.com" + relayServiceMethodPath, false},
		{"https with port", "https://rm.example.com:8443", "https://rm.example.com:8443" + relayServiceMethodPath, false},
		{"http", "http://rm.example.com", "http://rm.example.com" + relayServiceMethodPath, false},
		{"grpcs maps to https", "grpcs://rm.example.com", "https://rm.example.com" + relayServiceMethodPath, false},
		{"grpc maps to http", "grpc://rm.example.com", "http://rm.example.com" + relayServiceMethodPath, false},
		// A staked URL carrying a path is unexpected, but dropping it would send
		// the relay somewhere the supplier never advertised.
		{"keeps staked path", "https://rm.example.com/relay/", "https://rm.example.com/relay" + relayServiceMethodPath, false},
		{"bare host:port implies http", "127.0.0.1:8545", "http://127.0.0.1:8545" + relayServiceMethodPath, false},
		{"empty", "", "", true},
		{"unknown scheme", "ftp://rm.example.com", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := grpcWebURL(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("grpcWebURL(%q) = %q, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("grpcWebURL(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("grpcWebURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
