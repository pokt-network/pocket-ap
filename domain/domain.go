// Package domain holds the transport-agnostic types shared across the access
// point. It has zero external dependencies so every other package can import it
// without pulling in the Shannon/cosmos stack.
package domain

import (
	"errors"
	"fmt"
	"strings"
)

// ServiceID is a Shannon service identifier (e.g. "eth", "poly").
type ServiceID string

// EndpointAddr is a supplier operator address that terminates a relay.
type EndpointAddr string

// RPCType mirrors poktroll sharedtypes.RPCType. All five are native to Shannon;
// the network routes on the Rpc-Type header the proxy stamps.
type RPCType uint8

const (
	RPCTypeUnknown RPCType = iota
	RPCTypeJSONRPC
	RPCTypeREST
	RPCTypeCometBFT
	RPCTypeGRPC
	RPCTypeWebSocket
)

// Stateless reports whether this type is request/response (one HTTP round trip)
// vs stateful/streaming (WebSocket, streaming gRPC). The stateless family is
// served by transport.HTTP in v0; the stateful family is phase-2 (transport.WS).
func (r RPCType) Stateless() bool {
	switch r {
	case RPCTypeJSONRPC, RPCTypeREST, RPCTypeCometBFT, RPCTypeGRPC:
		return true
	default:
		return false
	}
}

func (r RPCType) String() string {
	switch r {
	case RPCTypeJSONRPC:
		return "json_rpc"
	case RPCTypeREST:
		return "rest"
	case RPCTypeCometBFT:
		return "comet_bft"
	case RPCTypeGRPC:
		return "grpc"
	case RPCTypeWebSocket:
		return "websocket"
	default:
		return "unknown"
	}
}

// ParseRPCType maps a config string to an RPCType.
func ParseRPCType(s string) (RPCType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "json_rpc", "jsonrpc", "json-rpc":
		return RPCTypeJSONRPC, nil
	case "rest", "http":
		return RPCTypeREST, nil
	case "comet_bft", "cometbft", "comet-bft", "tendermint":
		return RPCTypeCometBFT, nil
	case "grpc":
		return RPCTypeGRPC, nil
	case "websocket", "ws":
		return RPCTypeWebSocket, nil
	default:
		return RPCTypeUnknown, fmt.Errorf("unknown rpc type %q", s)
	}
}

// Endpoint is one supplier's set of reachable URLs, keyed by RPC type. A single
// supplier can advertise different URLs per type, so selection must filter by
// the requested type.
type Endpoint struct {
	Supplier EndpointAddr
	URLs     map[RPCType]string
}

// SupportsType reports whether the endpoint advertises a URL for the type.
func (e Endpoint) SupportsType(t RPCType) bool {
	_, ok := e.URLs[t]
	return ok
}

// URL returns the endpoint URL for the given type.
func (e Endpoint) URL(t RPCType) (string, bool) {
	u, ok := e.URLs[t]
	return u, ok
}

// Session is the minimal view of a Shannon session the proxy needs. Raw carries
// the underlying *sessiontypes.Session (as any, to keep this package dep-free)
// for the signer to read the SessionHeader without re-fetching.
type Session struct {
	ID             string
	ServiceID      ServiceID
	AppAddr        string
	EndBlockHeight int64
	Endpoints      []Endpoint
	Raw            any
}

// RelayInput is a captured inbound request, decoupled from net/http so the relay
// core and stubs stay transport-agnostic and retry-safe (Body is a reusable
// buffer, not a single-use stream).
type RelayInput struct {
	Method string
	Path   string // path + raw query, relative to the supplier base URL
	Header map[string][]string
	Body   []byte
}

// RelayResult is the response to write back to the client.
type RelayResult struct {
	StatusCode int
	Header     map[string][]string
	Body       []byte
}

var (
	ErrNoApp           = errors.New("no app configured for service")
	ErrNoEndpoint      = errors.New("no endpoint supports the requested rpc type")
	ErrUnsupportedType = errors.New("unsupported rpc type")
	ErrNotImplemented  = errors.New("not implemented: lift from SAGE (see TODO)")
)
