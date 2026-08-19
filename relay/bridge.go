package relay

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/pokt-network/pocket-ap/domain"
)

// Relay-miner handshake headers. A WebSocket carries its routing on the upgrade
// rather than per request, so unlike the HTTP path — where pocket.HTTPSender
// stamps Rpc-Type on every send — these are set once, on the dial. Get them
// wrong and the miner treats the connection as anonymous and refuses to upgrade;
// there is no second chance further down.
//
// LIFT: SAGE protocol/shannon/ws_relayer.go:194-203.
const (
	HeaderTargetServiceID = "Target-Service-Id"
	HeaderAppAddress      = "App-Address"
	HeaderRPCType         = "Rpc-Type"
)

// FrameSigner ring-signs one raw WebSocket frame for a supplier.
//
// Separate from Signer because the payload encoding differs and the difference
// is not cosmetic: Signer.SignRelay wraps an HTTP request, while a WS frame must
// be signed as raw bytes — the miner writes them verbatim to the backend and
// hashes them for onchain proof verification. See pocket.SignFrame.
//
// Concrete impl: pocket.Signer.
type FrameSigner interface {
	SignFrame(ctx context.Context, session *domain.Session, supplier domain.EndpointAddr, payload []byte) (relayReqBz []byte, err error)
}

// FrameValidator verifies a supplier's signed frame and returns the raw inner
// payload — no HTTP decoding, mirroring FrameSigner.
//
// Concrete impl: pocket.Validator.
type FrameValidator interface {
	ValidateFrame(supplier domain.EndpointAddr, respBz []byte) (payload []byte, err error)
}

// Bridge composes the seams for the stateful WebSocket flow, the way Relayer
// does for the stateless one. It is deliberately a separate type: Relay is one
// round trip over a session it fetches itself, whereas a bridge is a long-lived
// connection pinned to one supplier and one session for its lifetime.
//
// Bridge does no socket work and imports no HTTP: it resolves the Shannon
// concerns (session, endpoint, signing) and hands the front adapter what it
// needs. transport.WS does the upgrading and pumping.
type Bridge struct {
	Sessions  SessionSource
	Signer    FrameSigner
	Validator FrameValidator
	Selector  Selector

	// RPCTypeHeader supplies the Rpc-Type wire value the miner routes on.
	// Injected rather than hardcoded so the mapping stays in one place — the
	// value is a protocol contract, not a constant we own. pocket.RPCTypeHeader.
	RPCTypeHeader func(domain.RPCType) string
}

// BridgeProcessor is the frame contract for one bridge: sign what the client
// sends, verify what the supplier returns, and stop signing once the session
// behind it retires.
//
// The first two methods mirror websockets.MessageProcessor exactly, which is how
// this package hands a processor to the socket layer without importing it — Go
// interfaces are structural. It is an interface rather than the concrete type so
// a front adapter can be tested without real signing.
type BridgeProcessor interface {
	ProcessClientMessage(data []byte) ([]byte, error)
	ProcessEndpointMessage(data []byte) ([]byte, error)
	Deactivate()
}

// Prepared is everything a front adapter needs to open a bridge.
type Prepared struct {
	Session     *domain.Session
	Supplier    domain.EndpointAddr
	EndpointURL string
	// Headers are the miner's handshake auth, as map[string][]string so this
	// package stays free of net/http (same reason domain.RelayInput does).
	Headers   map[string][]string
	Processor BridgeProcessor
}

// Prepare picks a session and a supplier for a new bridge, and builds the
// processor that will sign and validate its frames.
//
// Unlike Relay there is no failover here: a bridge is one client connection to
// one supplier, so a supplier that fails mid-stream ends the bridge and the
// client reconnects — which re-enters Prepare and reselects. Failing over
// underneath a live socket would silently swap which supplier is serving a
// subscription the client already believes is established.
func (b *Bridge) Prepare(ctx context.Context, serviceID domain.ServiceID, rpcType domain.RPCType) (*Prepared, error) {
	session, err := b.Sessions.Session(ctx, serviceID)
	if err != nil {
		return nil, fmt.Errorf("bridge: session fetch failed for %s: %w", serviceID, err)
	}

	ordered, err := b.Selector.Select(ctx, serviceID, session.Endpoints, rpcType)
	if err != nil {
		return nil, fmt.Errorf("bridge: endpoint selection failed for %s/%s: %w", serviceID, rpcType, err)
	}

	// Take the first endpoint that actually advertises a URL for this type.
	for _, ep := range ordered {
		url, ok := ep.URL(rpcType)
		if !ok {
			continue
		}
		// The app comes from the session, not from a field on this struct. The
		// miner bills the app named here and rejects the handshake if it is not
		// the one whose key signs the frames — and the session is the only thing
		// that knows both, so taking it from anywhere else is a mismatch waiting
		// to happen. It became load-bearing with multi-app: one Bridge now serves
		// several apps, so a single configured address would be wrong for all but
		// one of them.
		headers := map[string][]string{
			HeaderTargetServiceID: {string(serviceID)},
			HeaderAppAddress:      {session.AppAddr},
		}
		if b.RPCTypeHeader != nil {
			if v := b.RPCTypeHeader(rpcType); v != "" {
				headers[HeaderRPCType] = []string{v}
			}
		}
		return &Prepared{
			Session:     session,
			Supplier:    ep.Supplier,
			EndpointURL: url,
			Headers:     headers,
			Processor:   newFrameProcessor(ctx, b.Signer, b.Validator, session, ep.Supplier),
		}, nil
	}
	return nil, domain.ErrNoEndpoint
}

// Compile-time assertion.
var _ BridgeProcessor = (*FrameProcessor)(nil)

// FrameProcessor is the real BridgeProcessor: it signs client frames and
// validates supplier frames for one bridge.
//
// The session and supplier are fixed at construction and stay fixed for the
// bridge's life: pocket-ap closes a bridge at a session boundary rather than
// re-signing a live socket, so there is nothing here to rotate.
type FrameProcessor struct {
	ctx       context.Context
	signer    FrameSigner
	validator FrameValidator
	session   *domain.Session
	supplier  domain.EndpointAddr

	// active gates client frames. Flipped false when the session expires, so
	// supplier frames still in flight can drain out to the client while nothing
	// new is signed against a session the chain has already retired.
	active atomic.Bool
}

func newFrameProcessor(ctx context.Context, signer FrameSigner, validator FrameValidator, session *domain.Session, supplier domain.EndpointAddr) *FrameProcessor {
	p := &FrameProcessor{
		ctx:       ctx,
		signer:    signer,
		validator: validator,
		session:   session,
		supplier:  supplier,
	}
	p.active.Store(true)
	return p
}

// ErrSessionExpired is returned once the session behind a bridge has ended. The
// bridge treats it as terminal and closes with a reconnect hint.
var ErrSessionExpired = fmt.Errorf("bridge: session expired")

// Deactivate stops the processor signing further client frames. Called by the
// expiry watcher (phase 3).
func (p *FrameProcessor) Deactivate() { p.active.Store(false) }

// Session returns the session this bridge is pinned to.
func (p *FrameProcessor) Session() *domain.Session { return p.session }

// Supplier returns the supplier this bridge is pinned to.
func (p *FrameProcessor) Supplier() domain.EndpointAddr { return p.supplier }

// ProcessClientMessage signs a client frame into a RelayRequest for the miner.
func (p *FrameProcessor) ProcessClientMessage(data []byte) ([]byte, error) {
	if !p.active.Load() {
		return nil, ErrSessionExpired
	}
	return p.signer.SignFrame(p.ctx, p.session, p.supplier, data)
}

// ProcessEndpointMessage verifies a supplier frame and returns its raw payload.
//
// A validation failure is terminal for the bridge: we cannot hand the client
// bytes whose origin we could not verify, and unlike the HTTP path there is no
// second supplier to try under a live socket.
func (p *FrameProcessor) ProcessEndpointMessage(data []byte) ([]byte, error) {
	return p.validator.ValidateFrame(p.supplier, data)
}
