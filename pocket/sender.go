package pocket

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"

	"github.com/pokt-network/pocket-ap/domain"
	"github.com/pokt-network/pocket-ap/relay"
)

// Compile-time assertion: HTTPSender satisfies the Sender seam.
var _ relay.Sender = (*HTTPSender)(nil)

// HTTPSender POSTs signed relay bytes to a supplier URL. This is real (no SDK
// needed) and mirrors SAGE protocol/shannon/transport.go sendHTTP.
type HTTPSender struct {
	client *http.Client
	// streamClient carries no whole-request Timeout — that would sever a
	// long-lived stream mid-answer — but it is NOT unbounded: its transport sets
	// ResponseHeaderTimeout, so a supplier that never starts answering fails and
	// the relay fails over. See streamTransport.
	streamClient *http.Client
}

// maxResponseBytes caps a supplier's response. Suppliers are in the threat
// model: a hostile or broken one returning an unbounded body would otherwise
// exhaust memory.
const maxResponseBytes = 64 << 20 // 64 MiB

// NewHTTPSender builds a sender with the given per-request timeout.
func NewHTTPSender(timeout time.Duration) *HTTPSender {
	return &HTTPSender{
		client:       &http.Client{Timeout: timeout, Transport: relayTransport()},
		streamClient: &http.Client{Transport: streamTransport(timeout)},
	}
}

// relayTransport is Go's default with a bigger per-host idle pool.
//
// The default MaxIdleConnsPerHost is 2. Suppliers are reached over TLS, so a
// connection that is not pooled costs a fresh TCP + TLS handshake on the next
// relay — and beta puts every supplier behind ONE host, so 2 is easy to exhaust
// with a handful of concurrent relays.
func relayTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 32
	return t
}

// streamTransport bounds how long a supplier may take to START answering,
// without bounding how long it may then stream.
//
// A whole-request Timeout cannot be used here: an SSE/LLM stream is long-lived
// by design and a deadline would sever it mid-answer. But leaving the client
// with no bound at all — which is what shipped with the streaming work — means a
// supplier that accepts the connection and never replies hangs the relay
// forever: the HTTP front passes r.Context(), which carries no deadline of its
// own, so nothing times out and no failover ever fires. Measured: Send returned
// in 2s against a hung supplier, SendStream was still blocked at 4s.
//
// ResponseHeaderTimeout is the right bound: headers must arrive within it, the
// body may then take as long as it likes.
func streamTransport(headerTimeout time.Duration) *http.Transport {
	t := relayTransport()
	t.ResponseHeaderTimeout = headerTimeout
	return t
}

// Send POSTs the marshaled RelayRequest and returns the raw response body.
func (s *HTTPSender) Send(ctx context.Context, url string, relayReqBz []byte, rpcType domain.RPCType) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(relayReqBz))
	if err != nil {
		return nil, fmt.Errorf("sender: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if h := rpcTypeHeader(rpcType); h != "" {
		req.Header.Set("Rpc-Type", h)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sender: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A supplier is not trusted. Without a bound, one returning an endless body
	// OOMs the proxy, and the relay it is answering has already been paid for.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("sender: read response: %w", err)
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, fmt.Errorf("sender: response from %s exceeds %d bytes", url, maxResponseBytes)
	}
	return body, nil
}

// rpcTypeToShared maps domain RPC types to poktroll shared types. The relay
// miner reads the Rpc-Type header to select the correct backend service config.
// LIFT: SAGE protocol/shannon/transport.go:17.
var rpcTypeToShared = map[domain.RPCType]sharedtypes.RPCType{
	domain.RPCTypeJSONRPC:   sharedtypes.RPCType_JSON_RPC,
	domain.RPCTypeREST:      sharedtypes.RPCType_REST,
	domain.RPCTypeCometBFT:  sharedtypes.RPCType_COMET_BFT,
	domain.RPCTypeGRPC:      sharedtypes.RPCType_GRPC,
	domain.RPCTypeWebSocket: sharedtypes.RPCType_WEBSOCKET,
}

// rpcTypeHeader returns the Rpc-Type header value the relay miner routes on, or
// "" for an unmapped/unknown type (no header stamped).
func rpcTypeHeader(rpcType domain.RPCType) string {
	st, ok := rpcTypeToShared[rpcType]
	if !ok || st == sharedtypes.RPCType_UNKNOWN_RPC {
		return ""
	}
	return strconv.Itoa(int(st))
}

// Compile-time assertion: HTTPSender also opens streaming relays.
var _ relay.StreamSender = (*HTTPSender)(nil)

// SendStream POSTs the relay and returns the response body still open, so the
// caller can consume it as it arrives.
//
// Same request as Send — the difference is entirely in the response. When the
// backend streams (SSE/NDJSON), the relay miner batch-signs chunks and writes
// several RelayResponses into one long-lived body separated by
// relay.StreamDelimiter, so reading it whole (as Send does) would both defeat
// the streaming and hand the validator a blob that is not a single
// RelayResponse.
//
// The caller MUST close the returned body.
func (s *HTTPSender) SendStream(ctx context.Context, url string, relayReqBz []byte, rpcType domain.RPCType) (io.ReadCloser, map[string][]string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(relayReqBz))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("sender: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if h := rpcTypeHeader(rpcType); h != "" {
		req.Header.Set("Rpc-Type", h)
	}

	// Deliberately NOT s.client: its whole-request timeout would sever a working
	// token stream mid-answer. streamClient bounds time-to-first-byte instead
	// (ResponseHeaderTimeout), so a hung supplier still fails over.
	resp, err := s.streamClient.Do(req)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("sender: request failed: %w", err)
	}
	return resp.Body, resp.Header, resp.StatusCode, nil
}
