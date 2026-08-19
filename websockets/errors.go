package websockets

import "errors"

// Bridge failure modes. They are distinguished because the caller reacts
// differently to each: an endpoint that advertised WebSocket but will not accept
// a connection is the supplier's fault and worth failing over, whereas a client
// handshake failure is not.
var (
	// ErrBridgeClientUpgradeFailed: the inbound client handshake failed. Our
	// side or the client's — never the supplier's.
	ErrBridgeClientUpgradeFailed = errors.New("websockets: client upgrade failed")

	// ErrBridgeOriginRejected: the browser origin is not allowlisted. Distinct
	// from a generic upgrade failure so the log can say so plainly — this is the
	// error someone will hit while wiring up a browser client.
	ErrBridgeOriginRejected = errors.New("websockets: origin not allowed")

	// ErrBridgeEndpointUnavailable: the supplier's WS endpoint would not accept
	// a connection.
	ErrBridgeEndpointUnavailable = errors.New("websockets: endpoint unavailable")

	// ErrBridgeConnectionFailed: a read or write failed once the bridge was up.
	ErrBridgeConnectionFailed = errors.New("websockets: connection failed")

	// ErrBridgeMessageProcessing: the MessageProcessor rejected a frame —
	// signing failed, or a supplier frame did not validate.
	ErrBridgeMessageProcessing = errors.New("websockets: message processing failed")

	// ErrBridgeSessionExpired: the Shannon session backing this bridge ended.
	// Terminal by design: pocket-ap closes the bridge at session boundaries and
	// lets the client reconnect rather than re-signing live sockets.
	ErrBridgeSessionExpired = errors.New("websockets: session expired")

	// ErrBridgeContextCanceled: the bridge context was cancelled, i.e. shutdown.
	ErrBridgeContextCanceled = errors.New("websockets: context canceled")
)
