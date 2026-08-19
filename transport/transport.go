// Package transport holds the front adapters — the local listeners a client
// points its RPC_URL at. This is the seam where "more than JSON-RPC" lives:
// the core relay flow is transport-agnostic, each adapter maps one lifecycle.
//
//	stateless request/response (JSON-RPC, REST, CometBFT, unary gRPC) -> HTTP
//	stateful streaming        (WebSocket)                             -> WS
package transport

import (
	"context"
	"errors"
	"time"

	"github.com/pokt-network/pocket-ap/domain"
	"github.com/pokt-network/pocket-ap/relay"
	"github.com/pokt-network/pocket-ap/websockets"
)

// Listener hardening. Deliberately NOT ReadTimeout/WriteTimeout: a websocket or
// an SSE token stream is long-lived by design and a whole-request deadline would
// cut it off mid-answer. These bound only the handshake.
const (
	readHeaderTimeout = 10 * time.Second
	maxHeaderBytes    = 1 << 20 // 1 MiB
)

var errNoPrepareFunc = errors.New("transport: websocket listener has no PrepareFunc wired")

// RelayFunc is the stateless relay entry point. Implemented by
// relay.Relayer.Relay.
type RelayFunc func(ctx context.Context, serviceID domain.ServiceID, rpcType domain.RPCType, in domain.RelayInput) (*domain.RelayResult, error)

// StreamFunc relays and delivers each validated response batch to onBatch as it
// arrives. Implemented by relay.Relayer.RelayStream.
//
// The HTTP front uses this rather than RelayFunc because a response only
// declares itself streaming once it arrives — the backend decides, not the
// client — so a listener serving arbitrary requests has to be ready for both. A
// non-streaming response is simply one batch.
type StreamFunc func(ctx context.Context, serviceID domain.ServiceID, rpcType domain.RPCType, in domain.RelayInput, onBatch func(*domain.RelayResult) error) error

// PrepareFunc resolves the Shannon side of a new WebSocket bridge — session,
// supplier, URL, handshake headers, frame processor. Implemented by
// relay.Bridge.Prepare.
//
// The stateful flow needs its own entry point because RelayFunc is shaped for
// one round trip: it takes a captured request and returns a result, which a
// long-lived connection has neither of.
type PrepareFunc func(ctx context.Context, serviceID domain.ServiceID, rpcType domain.RPCType) (*relay.Prepared, error)

// Transport is one front listener bound to a (service, rpcType) on an address.
type Transport interface {
	RPCType() domain.RPCType
	Serve(ctx context.Context) error // blocks until ctx cancelled or fatal error
	Close(ctx context.Context) error
}

// Options configures one front listener. It is a struct rather than positional
// arguments because the two families need different collaborators — the
// stateless one wants Relay, the stateful one wants Prepare and AllowedOrigins —
// and a six-argument constructor with two nils in it reads like a mistake.
type Options struct {
	Addr      string
	ServiceID domain.ServiceID
	RPCType   domain.RPCType

	// Relay serves the stateless family. Used only when Stream is nil.
	Relay RelayFunc

	// Stream serves the stateless family with streaming support (SSE/NDJSON).
	// Preferred over Relay when set.
	Stream StreamFunc

	// Prepare serves the stateful family.
	Prepare PrepareFunc

	// ChainHeight reports the newest block height seen. WebSocket bridges watch
	// it to notice their session ending, since a connection outlives the session
	// it was signed under. pocket.SessionManager.LatestBlockHeight implements it.
	ChainHeight func() int64

	// AllowedOrigins allowlists browser Origins. Applies to BOTH families: the
	// plain HTTP listener is reachable cross-origin too, and a relay it serves is
	// spent whether or not the caller can read the answer. Empty rejects every
	// browser origin while still allowing native clients — the safe default. See
	// transport/cors.go.
	AllowedOrigins []string

	// AllowedHosts allowlists Host headers, closing DNS rebinding. Empty derives
	// from Addr: a loopback-bound listener answers to localhost only; a listener
	// bound wider is not checked, because it was deliberately exposed and its
	// legitimate Host values (LAN IP, Docker service name, proxy domain) cannot
	// be guessed. See transport/hosts.go.
	AllowedHosts []string

	// MaxConnections caps concurrent WebSocket connections. WebSocket only: an
	// HTTP request completes and gives its resources back, a WebSocket does not.
	// 0 means websockets.DefaultMaxConnections; negative disables the cap.
	MaxConnections int
}

// New builds the adapter for a listener based on its RPC type: the stateless
// family shares one HTTP adapter; WebSocket gets the WS adapter.
func New(opts Options) Transport {
	switch opts.RPCType {
	case domain.RPCTypeWebSocket:
		ws := NewWS(opts.Addr, opts.ServiceID, opts.RPCType, opts.Prepare, opts.ChainHeight, opts.AllowedOrigins, opts.AllowedHosts)
		// 0 keeps NewWS's default cap; only an explicit value replaces it, and a
		// negative one turns the cap off.
		if opts.MaxConnections != 0 {
			ws.limiter = websockets.NewConnectionLimiter(opts.MaxConnections)
		}
		return ws
	default:
		// JSON-RPC, REST, CometBFT, unary gRPC — all HTTP the miner replays.
		h := NewHTTP(opts.Addr, opts.ServiceID, opts.RPCType, opts.Relay, opts.AllowedOrigins, opts.AllowedHosts)
		h.stream = opts.Stream
		return h
	}
}
