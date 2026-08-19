package pocket

import (
	"bytes"
	"testing"

	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sdktypes "github.com/pokt-network/shannon-sdk/types"

	"github.com/pokt-network/pocket-ap/domain"
)

func wsSession() *domain.Session {
	return &domain.Session{
		ID:      "sess-1",
		AppAddr: "pokt1app",
		Raw: &sessiontypes.Session{
			SessionId: "sess-1",
			Header: &sessiontypes.SessionHeader{
				ApplicationAddress:      "pokt1app",
				ServiceId:               "pnf-anvil",
				SessionEndBlockHeight:   476840,
				SessionStartBlockHeight: 476830,
			},
		},
	}
}

// THE critical property of the WS path. The relay miner writes RelayRequest
// .Payload verbatim to the backend socket AND hashes those exact bytes for
// onchain proof verification, so the payload must be byte-identical to the
// client's frame. Wrapping it in an HTTP envelope — which is exactly what the
// HTTP path's SignRelay does — makes proofs fail at claim time, long after any
// test or smoke run would have caught it.
func TestBuildFrameRelayRequest_PayloadIsRawAndUntouched(t *testing.T) {
	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)

	got, err := buildFrameRelayRequest(wsSession(), "supplierA", payload)
	if err != nil {
		t.Fatalf("buildFrameRelayRequest: %v", err)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Errorf("payload was altered:\n got: %q\nwant: %q", got.Payload, payload)
	}
}

// The specific regression to guard: if someone "helpfully" makes the WS path
// match the HTTP path, the payload becomes a serialized HTTP request. It would
// still be valid bytes and still round-trip in tests — and still break onchain.
func TestBuildFrameRelayRequest_PayloadIsNotAnHTTPEnvelope(t *testing.T) {
	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe"}`)

	got, err := buildFrameRelayRequest(wsSession(), "supplierA", payload)
	if err != nil {
		t.Fatalf("buildFrameRelayRequest: %v", err)
	}
	// A POKTHTTPRequest envelope would deserialize; a raw JSON-RPC frame must not.
	if _, err := sdktypes.DeserializeHTTPRequest(got.Payload); err == nil {
		t.Error("the frame payload deserialized as a POKTHTTPRequest — it has been HTTP-wrapped, which breaks onchain proof verification")
	}
}

// Binary frames must survive untouched too: subscriptions are not always JSON,
// and a byte-for-byte hash does not care what the bytes mean.
func TestBuildFrameRelayRequest_BinaryPayloadSurvives(t *testing.T) {
	payload := []byte{0x00, 0x01, 0xff, 0xfe, 0x00}

	got, err := buildFrameRelayRequest(wsSession(), "supplierA", payload)
	if err != nil {
		t.Fatalf("buildFrameRelayRequest: %v", err)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Errorf("binary payload altered: got %v, want %v", got.Payload, payload)
	}
}

func TestBuildFrameRelayRequest_EmptyPayload(t *testing.T) {
	got, err := buildFrameRelayRequest(wsSession(), "supplierA", []byte{})
	if err != nil {
		t.Fatalf("buildFrameRelayRequest: %v", err)
	}
	if len(got.Payload) != 0 {
		t.Errorf("payload = %v, want empty", got.Payload)
	}
}

// The session header is what ties the relay to a session the chain recognises,
// and the supplier address to the one being billed. Both are carried, not
// derived, so a mix-up here is silent.
func TestBuildFrameRelayRequest_CarriesSessionHeaderAndSupplier(t *testing.T) {
	got, err := buildFrameRelayRequest(wsSession(), "supplierA", []byte("x"))
	if err != nil {
		t.Fatalf("buildFrameRelayRequest: %v", err)
	}
	if got.Meta.SupplierOperatorAddress != "supplierA" {
		t.Errorf("supplier = %q", got.Meta.SupplierOperatorAddress)
	}
	if got.Meta.SessionHeader == nil {
		t.Fatal("session header missing")
	}
	if got.Meta.SessionHeader.ServiceId != "pnf-anvil" {
		t.Errorf("service = %q", got.Meta.SessionHeader.ServiceId)
	}
	if got.Meta.SessionHeader.SessionEndBlockHeight != 476840 {
		t.Errorf("end height = %d", got.Meta.SessionHeader.SessionEndBlockHeight)
	}
}

// domain.Session.Raw is an `any` so domain stays dependency-free, which means a
// wrong or missing value is a runtime problem. Fail loudly rather than signing
// something malformed.
func TestBuildFrameRelayRequest_RejectsSessionWithoutRawHeader(t *testing.T) {
	tests := []struct {
		name    string
		session *domain.Session
	}{
		{"nil Raw", &domain.Session{ID: "s1"}},
		{"Raw of the wrong type", &domain.Session{ID: "s1", Raw: "not a session"}},
		{"Raw session with no header", &domain.Session{ID: "s1", Raw: &sessiontypes.Session{SessionId: "s1"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := buildFrameRelayRequest(tt.session, "supplierA", []byte("x")); err == nil {
				t.Error("built a relay request from a session with no usable header")
			}
		})
	}
}

// RPCTypeHeader is exported for relay.Bridge, which needs the wire value for the
// handshake without importing poktroll. It must agree with the sender's mapping.
func TestRPCTypeHeaderExported(t *testing.T) {
	if got := RPCTypeHeader(domain.RPCTypeWebSocket); got != "2" {
		t.Errorf("RPCTypeHeader(websocket) = %q, want 2", got)
	}
	if got := RPCTypeHeader(domain.RPCTypeUnknown); got != "" {
		t.Errorf("RPCTypeHeader(unknown) = %q, want empty", got)
	}
	// Same source of truth as the HTTP path.
	for _, rt := range []domain.RPCType{
		domain.RPCTypeJSONRPC, domain.RPCTypeREST, domain.RPCTypeCometBFT,
		domain.RPCTypeGRPC, domain.RPCTypeWebSocket,
	} {
		if RPCTypeHeader(rt) != rpcTypeHeader(rt) {
			t.Errorf("RPCTypeHeader(%s) diverged from rpcTypeHeader", rt)
		}
	}
}
