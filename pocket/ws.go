package pocket

import (
	"context"
	"fmt"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sdk "github.com/pokt-network/shannon-sdk"

	"github.com/pokt-network/pocket-ap/domain"
	"github.com/pokt-network/pocket-ap/relay"
)

// Compile-time assertions: the WS seams are satisfied by the same types that
// serve the HTTP path, so both share one signer and one validator.
var (
	_ relay.FrameSigner    = (*Signer)(nil)
	_ relay.FrameValidator = (*Validator)(nil)
)

// buildFrameRelayRequest wraps a raw WS frame in an unsigned RelayRequest.
//
// Split out from SignFrame because the rest of SignFrame reaches the network
// (getApp) and cannot be unit tested, while THIS is the part that must not
// regress: the payload has to stay byte-identical to what the client sent. If
// that ever changes, proofs fail onchain at claim time — long after any test or
// smoke run would have noticed.
func buildFrameRelayRequest(session *domain.Session, supplier domain.EndpointAddr, payload []byte) (*servicetypes.RelayRequest, error) {
	raw, ok := session.Raw.(*sessiontypes.Session)
	if !ok || raw.Header == nil {
		return nil, fmt.Errorf("SignFrame: session missing raw header")
	}
	return &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader:           raw.Header,
			SupplierOperatorAddress: string(supplier),
		},
		Payload: payload, // raw — no HTTP envelope, see SignFrame
	}, nil
}

// SignFrame ring-signs one raw WebSocket frame for a supplier.
//
// THE PAYLOAD IS RAW FRAME BYTES — DO NOT WRAP IT IN AN HTTP ENVELOPE. This is
// the one place the WS path must not follow SignRelay, which serializes an
// http.Request via sdktypes.SerializeHTTPRequest.
//
// Two reasons, and the second is unforgiving:
//  1. The relay miner's WS bridge writes RelayRequest.Payload verbatim to the
//     backend socket — no DeserializeHTTPRequest, no envelope unwrap.
//  2. Those exact bytes are hashed for onchain proof verification, so the proxy
//     and the miner must agree bit-for-bit on the encoding. An HTTP envelope
//     would make proofs fail to validate — a failure that shows up onchain at
//     claim time, not here.
//
// The frame type (text vs binary) rides in the WebSocket frame metadata, not the
// payload; the bridge preserves it on both hops.
//
// LIFT: SAGE protocol/shannon/ws_processor.go ProcessClientMessage.
func (s *Signer) SignFrame(ctx context.Context, session *domain.Session, supplier domain.EndpointAddr, payload []byte) ([]byte, error) {
	unsignedReq, err := buildFrameRelayRequest(session, supplier, payload)
	if err != nil {
		return nil, err
	}

	app, err := s.getApp(ctx, session.AppAddr)
	if err != nil {
		return nil, fmt.Errorf("SignFrame: fetch app for signing: %w", err)
	}

	signed, err := s.signRelayRequest(ctx, unsignedReq, app)
	if err != nil {
		return nil, err
	}

	reqBz, err := signed.Marshal()
	if err != nil {
		return nil, fmt.Errorf("SignFrame: marshal signed request: %w", err)
	}
	return reqBz, nil
}

// ValidateFrame verifies the supplier's signed frame and returns the inner
// payload — again raw, with no HTTP decoding, mirroring SignFrame. The miner
// puts the backend's raw WS frame straight into RelayResponse.Payload, so
// running DeserializeHTTPResponse over it (as ValidateResponse does for HTTP)
// would fail on bytes that were never an HTTP response.
//
// LIFT: SAGE protocol/shannon/ws_processor.go ProcessEndpointMessage.
func (v *Validator) ValidateFrame(supplier domain.EndpointAddr, respBz []byte) ([]byte, error) {
	relayResp, err := sdk.ValidateRelayResponse(
		context.Background(),
		sdk.SupplierAddress(string(supplier)),
		respBz,
		v.pubKeys,
	)
	if err != nil {
		return nil, fmt.Errorf("validate relay frame from %s: %w%s", supplier, err, minerErrDetail(relayResp))
	}
	return relayResp.Payload, nil
}

// RPCTypeHeader returns the Rpc-Type wire value the relay miner routes on, or ""
// for an unknown type. Exported because the WebSocket handshake carries it as a
// header rather than the sender stamping it per request, so relay.Bridge needs
// it without importing poktroll's shared types.
func RPCTypeHeader(rpcType domain.RPCType) string { return rpcTypeHeader(rpcType) }
