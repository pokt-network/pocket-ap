package relay

import (
	"context"
	"errors"
	"testing"

	"github.com/pokt-network/pocket-ap/domain"
)

// --- stubs ------------------------------------------------------------------

type stubFrameSigner struct {
	err      error
	signed   [][]byte
	gotSess  *domain.Session
	gotSuppl domain.EndpointAddr
}

func (s *stubFrameSigner) SignFrame(_ context.Context, sess *domain.Session, supplier domain.EndpointAddr, payload []byte) ([]byte, error) {
	s.gotSess, s.gotSuppl = sess, supplier
	s.signed = append(s.signed, payload)
	if s.err != nil {
		return nil, s.err
	}
	return append([]byte("signed:"), payload...), nil
}

type stubFrameValidator struct {
	err      error
	gotSuppl domain.EndpointAddr
}

func (v *stubFrameValidator) ValidateFrame(supplier domain.EndpointAddr, respBz []byte) ([]byte, error) {
	v.gotSuppl = supplier
	if v.err != nil {
		return nil, v.err
	}
	return append([]byte("valid:"), respBz...), nil
}

func wsEndpoint(supplier, url string) domain.Endpoint {
	return domain.Endpoint{
		Supplier: domain.EndpointAddr(supplier),
		URLs:     map[domain.RPCType]string{domain.RPCTypeWebSocket: url},
	}
}

func newBridge(eps []domain.Endpoint) (*Bridge, *stubFrameSigner, *stubFrameValidator) {
	signer := &stubFrameSigner{}
	validator := &stubFrameValidator{}
	return &Bridge{
		Sessions:      stubSessions{session: &domain.Session{ID: "s1", AppAddr: "pokt1app", Endpoints: eps}},
		Signer:        signer,
		Validator:     validator,
		Selector:      stubSelector{ordered: eps},
		RPCTypeHeader: func(domain.RPCType) string { return "2" },
	}, signer, validator
}

// --- tests ------------------------------------------------------------------

func TestBridgePrepare_ResolvesSessionSupplierAndURL(t *testing.T) {
	eps := []domain.Endpoint{wsEndpoint("supplierA", "wss://a/ws")}
	b, _, _ := newBridge(eps)

	got, err := b.Prepare(context.Background(), "pnf-anvil", domain.RPCTypeWebSocket)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.Supplier != "supplierA" {
		t.Errorf("supplier = %q", got.Supplier)
	}
	if got.EndpointURL != "wss://a/ws" {
		t.Errorf("url = %q", got.EndpointURL)
	}
	if got.Session.ID != "s1" {
		t.Errorf("session = %q", got.Session.ID)
	}
	if got.Processor == nil {
		t.Fatal("Prepare returned no processor")
	}
}

// The miner's handshake auth is set once, on the dial. Getting it wrong means an
// anonymous connection and a refused upgrade, with nothing downstream able to
// fix it.
func TestBridgePrepare_BuildsMinerHandshakeHeaders(t *testing.T) {
	eps := []domain.Endpoint{wsEndpoint("supplierA", "wss://a/ws")}
	b, _, _ := newBridge(eps)

	got, err := b.Prepare(context.Background(), "pnf-anvil", domain.RPCTypeWebSocket)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	for header, want := range map[string]string{
		HeaderTargetServiceID: "pnf-anvil",
		HeaderAppAddress:      "pokt1app",
		HeaderRPCType:         "2",
	} {
		vs := got.Headers[header]
		if len(vs) != 1 || vs[0] != want {
			t.Errorf("header %s = %v, want [%s]", header, vs, want)
		}
	}
}

// One Bridge now serves every configured app, so the billed app has to come
// from the session it just fetched — the session is the only thing that knows
// both which service this is and which app pays for it. A single configured
// address would be wrong for every app but one, and the miner rejects a
// handshake whose App-Address is not the app that signs the frames.
func TestBridgePrepare_BillsTheSessionsApp(t *testing.T) {
	eps := []domain.Endpoint{wsEndpoint("supplierA", "wss://a/ws")}
	b, _, _ := newBridge(eps)
	b.Sessions = stubSessions{session: &domain.Session{ID: "s2", AppAddr: "pokt1second", Endpoints: eps}}

	got, err := b.Prepare(context.Background(), "svc-b", domain.RPCTypeWebSocket)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if vs := got.Headers[HeaderAppAddress]; len(vs) != 1 || vs[0] != "pokt1second" {
		t.Errorf("%s = %v, want [pokt1second] — the app must follow the session", HeaderAppAddress, vs)
	}
}

// The Rpc-Type value is a protocol contract owned by the pocket layer, so it is
// injected. A nil injector must not stamp a wrong value — it must stamp none.
func TestBridgePrepare_NoRPCTypeHeaderFuncOmitsTheHeader(t *testing.T) {
	eps := []domain.Endpoint{wsEndpoint("supplierA", "wss://a/ws")}
	b, _, _ := newBridge(eps)
	b.RPCTypeHeader = nil

	got, err := b.Prepare(context.Background(), "pnf-anvil", domain.RPCTypeWebSocket)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, ok := got.Headers[HeaderRPCType]; ok {
		t.Error("Rpc-Type stamped with no header func wired")
	}
}

// A supplier in the session that does not advertise a websocket URL must be
// skipped, not dialled.
func TestBridgePrepare_SkipsEndpointsWithoutAWebsocketURL(t *testing.T) {
	eps := []domain.Endpoint{
		{Supplier: "http-only", URLs: map[domain.RPCType]string{domain.RPCTypeJSONRPC: "https://a"}},
		wsEndpoint("supplierB", "wss://b/ws"),
	}
	b, _, _ := newBridge(eps)

	got, err := b.Prepare(context.Background(), "svc", domain.RPCTypeWebSocket)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.Supplier != "supplierB" {
		t.Errorf("supplier = %q, want the one advertising websocket", got.Supplier)
	}
}

func TestBridgePrepare_NoWebsocketEndpointsAtAll(t *testing.T) {
	eps := []domain.Endpoint{
		{Supplier: "http-only", URLs: map[domain.RPCType]string{domain.RPCTypeJSONRPC: "https://a"}},
	}
	b, _, _ := newBridge(eps)

	_, err := b.Prepare(context.Background(), "svc", domain.RPCTypeWebSocket)
	if !errors.Is(err, domain.ErrNoEndpoint) {
		t.Errorf("err = %v, want ErrNoEndpoint", err)
	}
}

func TestBridgePrepare_SessionFailureAborts(t *testing.T) {
	b, _, _ := newBridge(nil)
	b.Sessions = stubSessions{err: errors.New("node unreachable")}

	if _, err := b.Prepare(context.Background(), "svc", domain.RPCTypeWebSocket); err == nil {
		t.Fatal("Prepare succeeded with no session")
	}
}

func TestBridgePrepare_SelectorFailureAborts(t *testing.T) {
	b, _, _ := newBridge(nil)
	b.Selector = stubSelector{err: domain.ErrNoEndpoint}

	_, err := b.Prepare(context.Background(), "svc", domain.RPCTypeWebSocket)
	if !errors.Is(err, domain.ErrNoEndpoint) {
		t.Errorf("err = %v, want ErrNoEndpoint", err)
	}
}

// Select must be told which service it is picking for, same as the stateless
// path — the seam is service-aware on both flows or on neither.
func TestBridgePrepare_SelectReceivesServiceID(t *testing.T) {
	eps := []domain.Endpoint{wsEndpoint("supplierA", "wss://a/ws")}
	b, _, _ := newBridge(eps)
	sel := &recordingSelector{ordered: eps}
	b.Selector = sel

	if _, err := b.Prepare(context.Background(), "my-service", domain.RPCTypeWebSocket); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if sel.gotService != domain.ServiceID("my-service") {
		t.Errorf("Select got serviceID %q, want my-service", sel.gotService)
	}
	if sel.gotRPCType != domain.RPCTypeWebSocket {
		t.Errorf("Select got rpcType %v, want websocket", sel.gotRPCType)
	}
}

func TestFrameProcessor_SignsClientFramesAgainstItsPinnedSupplier(t *testing.T) {
	eps := []domain.Endpoint{wsEndpoint("supplierA", "wss://a/ws")}
	b, signer, _ := newBridge(eps)

	p, err := b.Prepare(context.Background(), "svc", domain.RPCTypeWebSocket)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	got, err := p.Processor.ProcessClientMessage([]byte("hello"))
	if err != nil {
		t.Fatalf("ProcessClientMessage: %v", err)
	}
	if string(got) != "signed:hello" {
		t.Errorf("frame = %q, want it signed", got)
	}
	if signer.gotSuppl != "supplierA" {
		t.Errorf("signed against %q, want the pinned supplier", signer.gotSuppl)
	}
	if signer.gotSess.ID != "s1" {
		t.Errorf("signed against session %q, want the pinned one", signer.gotSess.ID)
	}
}

func TestFrameProcessor_ValidatesEndpointFrames(t *testing.T) {
	eps := []domain.Endpoint{wsEndpoint("supplierA", "wss://a/ws")}
	b, _, validator := newBridge(eps)

	p, _ := b.Prepare(context.Background(), "svc", domain.RPCTypeWebSocket)

	got, err := p.Processor.ProcessEndpointMessage([]byte("frame"))
	if err != nil {
		t.Fatalf("ProcessEndpointMessage: %v", err)
	}
	if string(got) != "valid:frame" {
		t.Errorf("payload = %q", got)
	}
	if validator.gotSuppl != "supplierA" {
		t.Errorf("validated against %q, want the pinned supplier", validator.gotSuppl)
	}
}

// Once the session ends, no further client frame may be signed against it — the
// chain has retired that session and the miner would reject the relay.
func TestFrameProcessor_DeactivateStopsSigning(t *testing.T) {
	eps := []domain.Endpoint{wsEndpoint("supplierA", "wss://a/ws")}
	b, signer, _ := newBridge(eps)

	p, _ := b.Prepare(context.Background(), "svc", domain.RPCTypeWebSocket)
	p.Processor.Deactivate()

	_, err := p.Processor.ProcessClientMessage([]byte("hello"))
	if !errors.Is(err, ErrSessionExpired) {
		t.Errorf("err = %v, want ErrSessionExpired", err)
	}
	if len(signer.signed) != 0 {
		t.Errorf("signer was called %d times after expiry, want 0", len(signer.signed))
	}
}

// Supplier frames must still drain to the client after expiry: they were signed
// under a session that was valid when they were sent, and dropping them loses
// data the client already paid for.
func TestFrameProcessor_DeactivateStillDrainsEndpointFrames(t *testing.T) {
	eps := []domain.Endpoint{wsEndpoint("supplierA", "wss://a/ws")}
	b, _, _ := newBridge(eps)

	p, _ := b.Prepare(context.Background(), "svc", domain.RPCTypeWebSocket)
	p.Processor.Deactivate()

	got, err := p.Processor.ProcessEndpointMessage([]byte("frame"))
	if err != nil {
		t.Fatalf("endpoint frame rejected after expiry: %v", err)
	}
	if string(got) != "valid:frame" {
		t.Errorf("payload = %q", got)
	}
}

func TestFrameProcessor_SignErrorPropagates(t *testing.T) {
	eps := []domain.Endpoint{wsEndpoint("supplierA", "wss://a/ws")}
	b, signer, _ := newBridge(eps)
	signer.err = errors.New("no ring")

	p, _ := b.Prepare(context.Background(), "svc", domain.RPCTypeWebSocket)
	if _, err := p.Processor.ProcessClientMessage([]byte("hello")); err == nil {
		t.Fatal("ProcessClientMessage hid a signing failure")
	}
}

func TestFrameProcessor_ValidateErrorPropagates(t *testing.T) {
	eps := []domain.Endpoint{wsEndpoint("supplierA", "wss://a/ws")}
	b, _, validator := newBridge(eps)
	validator.err = errors.New("bad signature")

	p, _ := b.Prepare(context.Background(), "svc", domain.RPCTypeWebSocket)
	if _, err := p.Processor.ProcessEndpointMessage([]byte("frame")); err == nil {
		t.Fatal("ProcessEndpointMessage returned unverified bytes")
	}
}
