// Package websockets provides a generic, protocol-agnostic bidirectional
// WebSocket bridge between a client and a backend endpoint.
//
// The protocol layer hooks in through MessageProcessor; the bridge itself knows
// nothing about Shannon, signing, or Pocket. That separation is what lets the
// Shannon side stay ~80 lines.
//
//	Client ←─ clientConn ─→ Bridge ←─ endpointConn ─→ Endpoint (supplier)
//
// The two connections are separate fields so endpointConn could later be swapped
// to rotate suppliers inside one client session. Today a bridge is sticky: one
// endpoint for its lifetime, and it closes at session boundaries rather than
// re-signing.
//
// LIFT: SAGE websockets/ (bridge.go, connection.go, upgrade.go), with two
// deliberate changes — the origin check (see OriginPolicy; SAGE allows all
// origins because it is authenticated and we are not) and the removal of SAGE's
// "only WSRelayer may call StartBridge" rule, which existed to force reputation
// and observation onto every bridge. pocket-ap has neither by doctrine, so there
// is nothing to bypass.
package websockets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/pokt-network/pocket-ap/internal/safego"
)

// msgChanSize buffers frames between the read loops and the routing loop.
const msgChanSize = 32

// MessageProcessor transforms frames as they cross the bridge. The protocol
// layer implements it to sign and validate without the bridge knowing how.
type MessageProcessor interface {
	// ProcessClientMessage runs on every frame from the client before it is
	// forwarded to the endpoint.
	ProcessClientMessage(data []byte) ([]byte, error)

	// ProcessEndpointMessage runs on every frame from the endpoint before it is
	// forwarded to the client.
	ProcessEndpointMessage(data []byte) ([]byte, error)
}

// Bridge routes frames bidirectionally between a client and an endpoint.
//
// Lifecycle:
//  1. StartBridge upgrades the client, dials the endpoint, starts goroutines.
//  2. Each side runs an independent readLoop feeding msgChan.
//  3. The main loop reads msgChan, runs the MessageProcessor, writes to the
//     other side.
//  4. Any error triggers Shutdown: cancel context, send close frames, close both
//     connections, close done.
//
// Every failure path converges on Shutdown, which is idempotent.
type Bridge struct {
	ctx       context.Context
	cancelCtx context.CancelFunc
	logger    *slog.Logger

	clientConn   *Connection
	endpointConn *Connection

	processor MessageProcessor
	msgChan   chan message

	shutdownOnce sync.Once
	done         chan struct{}
}

// StartBridge upgrades the client connection, dials the endpoint, and starts
// routing. Block on Done() to wait for shutdown.
//
// A non-nil error means the bridge never started — the client handshake failed,
// the origin was rejected, or the endpoint would not accept a connection. Once
// it returns successfully, all further errors reach the client as close codes.
// Partial resources are cleaned up before any error return.
func StartBridge(
	ctx context.Context,
	logger *slog.Logger,
	policy OriginPolicy,
	clientReq *http.Request,
	clientWriter http.ResponseWriter,
	endpointURL string,
	endpointHeaders http.Header,
	processor MessageProcessor,
) (*Bridge, error) {
	logger = logger.With("component", "websocket_bridge")

	rawClient, err := UpgradeClient(logger, policy, clientReq, clientWriter)
	if err != nil {
		// UpgradeClient already wrote an HTTP error to clientWriter.
		return nil, fmt.Errorf("StartBridge: %w", err)
	}

	rawEndpoint, err := ConnectEndpoint(logger, endpointURL, endpointHeaders)
	if err != nil {
		// The client is a live WebSocket peer by now, and this is the one failure
		// that lands after the upgrade but before the bridge exists — so
		// Shutdown's close-frame path is not available yet. Closing the socket
		// bare skips the close handshake, and the client reports 1006 or a
		// protocol error: it reads as the client's fault when in truth the
		// endpoint we picked for it is down.
		//
		// 1013 (try again later) is the actionable answer: reconnecting reselects
		// a supplier, which is exactly what would fix it.
		closeClientWithReason(logger, rawClient, websocket.CloseTryAgainLater,
			"upstream endpoint unavailable, please reconnect")
		return nil, fmt.Errorf("StartBridge: %w", err)
	}

	bridgeCtx, cancelCtx := context.WithCancel(ctx)
	b := &Bridge{
		ctx:          bridgeCtx,
		cancelCtx:    cancelCtx,
		logger:       logger,
		clientConn:   NewConnection(rawClient, SourceClient, logger.With("conn", "client")),
		endpointConn: NewConnection(rawEndpoint, SourceEndpoint, logger.With("conn", "endpoint")),
		processor:    processor,
		msgChan:      make(chan message, msgChanSize),
		done:         make(chan struct{}),
	}

	safego.Go(logger, "websockets.bridge.run", b.run)
	return b, nil
}

// Done is closed once the bridge has fully shut down.
func (b *Bridge) Done() <-chan struct{} { return b.done }

// Shutdown tears the bridge down gracefully: cancel the context (stopping the
// read loops), send a close frame both ways, close both connections, then close
// done. Safe from any goroutine, any number of times.
func (b *Bridge) Shutdown(err error) {
	b.shutdownOnce.Do(func() {
		b.logger.Info("websocket: bridge shutting down", "err", err)

		// Cancel first, so readLoop stops sending on msgChan before we stop
		// draining it.
		b.cancelCtx()

		closeCode, closeText := b.determineCloseCode(err)
		// The single choke point between "what code do we mean" and "what code
		// goes on the wire". determineCloseCode propagates a peer's code by
		// design, and a peer that vanished hands us one we may not repeat.
		closeCode = sanitizeCloseCode(closeCode)
		clientMsg := websocket.FormatCloseMessage(closeCode, closeText)

		// The two peers do not get the same frame. pocket-ap sits in the middle —
		//
		//   client --(we are the server)-- pocket-ap --(we are the client)--> relay miner
		//
		// — so a code that is correct facing one direction is nonsense facing the
		// other. See endpointCloseCode.
		endpointMsg := clientMsg
		if endpointCode := endpointCloseCode(closeCode); endpointCode != closeCode {
			endpointMsg = websocket.FormatCloseMessage(endpointCode, closeText)
		}

		deadline := time.Now().Add(time.Second)

		for _, peer := range []struct {
			conn *Connection
			msg  []byte
		}{
			{b.clientConn, clientMsg},
			{b.endpointConn, endpointMsg},
		} {
			if peer.conn == nil {
				continue
			}
			if writeErr := peer.conn.WriteControl(websocket.CloseMessage, peer.msg, deadline); writeErr != nil {
				b.logger.Debug("websocket: could not send close frame", "err", writeErr)
			}
			_ = peer.conn.Close()
		}

		// msgChan is deliberately NOT closed: a readLoop could still be mid-send,
		// which would panic. Context cancellation is their exit signal, and the
		// channel is collected once they are gone.
		close(b.done)
	})
}

// run is the routing loop.
func (b *Bridge) run() {
	b.logger.Debug("websocket: bridge started")

	// Every exit from this function must close done. transport.WS.handle parks a
	// handler goroutine on Done() for the connection's whole life and the
	// connection limiter releases its slot from the same defer, so a routing loop
	// that stopped without shutting down would leak both — permanently, since an
	// idle subscription's socket never closes on its own. Shutdown is
	// sync.Once-guarded, so the paths below still choose the close code and this
	// only catches what they miss.
	defer b.Shutdown(ErrBridgeContextCanceled)

	// A read loop is one of the two pumps feeding msgChan, and readLoop's own
	// error path is skipped by a panic. One dying quietly would leave this loop
	// blocked on a channel nothing writes to, so containment here has to come
	// with a shutdown.
	pump := func(conn *Connection, name string) {
		safego.Go(b.logger, name, func() {
			defer func() {
				b.Shutdown(fmt.Errorf("%w: read loop from %v ended", ErrBridgeConnectionFailed, conn.source))
			}()
			b.readLoop(conn)
		})
	}
	pump(b.clientConn, "websockets.bridge.readLoop.client")
	pump(b.endpointConn, "websockets.bridge.readLoop.endpoint")

	for {
		select {
		case msg := <-b.msgChan:
			// The safego.Go wrapping run() would already keep a panicking frame
			// from reaching the process, and the defer above would already close
			// done. What this adds is the REASON: unwound through run() the
			// shutdown carries ErrBridgeContextCanceled, so the client is told
			// 1012 "service restarting, please reconnect" for a frame that is in
			// fact poison, and the log blames "run" rather than the routing step.
			// Caught here it takes the same exit as any other processing failure —
			// 1011, "message processing error" — which is what happened.
			if err := safego.Call(b.logger, "websockets.bridge.route", func() error {
				b.route(msg)
				return nil
			}); err != nil {
				b.Shutdown(fmt.Errorf("%w: %w", ErrBridgeMessageProcessing, err))
				return
			}
		case <-b.ctx.Done():
			b.Shutdown(ErrBridgeContextCanceled)
			return
		}
	}
}

// route runs one frame through the processor and writes it to the far side.
//
// The frame type (text vs binary) is preserved: it rides in the WebSocket frame
// metadata, not the payload, and the processor only ever sees the payload.
func (b *Bridge) route(msg message) {
	var (
		process func([]byte) ([]byte, error)
		dst     *Connection
		dstName string
	)
	switch msg.source {
	case SourceClient:
		process, dst, dstName = b.processor.ProcessClientMessage, b.endpointConn, "endpoint"
	case SourceEndpoint:
		process, dst, dstName = b.processor.ProcessEndpointMessage, b.clientConn, "client"
	default:
		return
	}

	processed, err := process(msg.data)
	if err != nil {
		b.logger.Error("websocket: frame processing failed", "source", msg.source, "err", err)
		b.Shutdown(fmt.Errorf("%w: %w", ErrBridgeMessageProcessing, err))
		return
	}
	if writeErr := dst.WriteMessage(msg.messageType, processed); writeErr != nil {
		b.logger.Error("websocket: write failed", "to", dstName, "err", writeErr)
		b.Shutdown(fmt.Errorf("%w: write to %s: %w", ErrBridgeConnectionFailed, dstName, writeErr))
	}
}

// readLoop pumps one connection into msgChan until it errors or the bridge is
// cancelled. A close frame's code is captured first so it can be propagated to
// the other side.
func (b *Bridge) readLoop(conn *Connection) {
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			if code, text := extractCloseInfo(err); code != 0 {
				conn.SetCloseInfo(code, text)
				b.logger.Debug("websocket: peer sent close frame",
					"source", conn.source, "code", code, "text", text)
			} else {
				b.logger.Debug("websocket: read error", "source", conn.source, "err", err)
			}
			b.Shutdown(fmt.Errorf("%w: read from %v: %w", ErrBridgeConnectionFailed, conn.source, err))
			return
		}

		select {
		case b.msgChan <- message{source: conn.source, messageType: msgType, data: data}:
		case <-b.ctx.Done():
			return
		}
	}
}

// closeClientWithReason sends a best-effort close frame to an already-upgraded
// client connection, then closes it.
//
// Both steps are best-effort: the client may already be gone, and there is
// nothing to be done about it either way — we are on our way out.
func closeClientWithReason(logger *slog.Logger, conn *websocket.Conn, closeCode int, reason string) {
	closeMsg := websocket.FormatCloseMessage(sanitizeCloseCode(closeCode), reason)
	if err := conn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(time.Second)); err != nil {
		logger.Debug("websocket: could not send close frame to client", "err", err)
	}
	if err := conn.Close(); err != nil {
		logger.Debug("websocket: could not close client connection", "err", err)
	}
}

// isSendableCloseCode reports whether a close code is legal to put on the wire.
//
// RFC 6455 §7.4.1 reserves 1005, 1006 and 1015 for what a local endpoint
// *infers* — no status was sent, the connection dropped, TLS failed — and
// forbids sending them. Everything else in the protocol range is fine, as is the
// 3000-4999 application range. Mirrors gorilla's validReceivedCloseCodes, which
// is the table our peers judge our frames by.
func isSendableCloseCode(code int) bool {
	switch code {
	case websocket.CloseNoStatusReceived, // 1005
		websocket.CloseAbnormalClosure, // 1006
		websocket.CloseTLSHandshake:    // 1015
		return false
	}
	if code >= 3000 && code <= 4999 {
		return true
	}
	return code >= 1000 && code <= 1014
}

// sanitizeCloseCode maps a code we may have inferred locally onto one we are
// allowed to send, leaving legal codes untouched.
//
// The bug this closes is entirely inside our own code. gorilla reports an
// abruptly dropped TCP connection as *CloseError{Code: 1006}; extractCloseInfo
// captures it (CloseAbnormalClosure is in its IsCloseError set), SetCloseInfo
// stores it, determineCloseCode hands it straight back, and FormatCloseMessage
// — which special-cases only 1005 — encodes it into a frame Shutdown then writes
// to BOTH peers. The receiver checks it against validReceivedCloseCodes, finds
// 1006 marked false, and rejects the whole frame: "websocket: bad close code
// 1006". So the client never learns why it was disconnected, and the relay miner
// rejects ours the same way.
//
// 1011 (internal server error) is the honest substitute: something went wrong on
// our side of the connection and we cannot say more. Application codes pass
// through — the bridge propagates supplier-chosen ones such as 4000 for session
// expiry, and those are explicitly valid to receive.
//
// LIFT: SAGE websockets/bridge.go (432263e), itself a port of PATH faad4777.
func sanitizeCloseCode(code int) int {
	if isSendableCloseCode(code) {
		return code
	}
	return websocket.CloseInternalServerErr // 1011
}

// endpointCloseCode adapts a client-facing close code for the UPSTREAM
// direction.
//
// pocket-ap is the server to the local client but the CLIENT to the relay
// miner, and RFC 6455 §7.4.1 defines 1011/1012/1013 as things a server tells a
// client: "internal server error", "service restarting, reconnect", "try again
// later". Sent upstream they invert the roles. "session ended, please
// reconnect" — which is what every session rollover sends, and rollover is the
// single most common way a bridge ends here — asks the miner to reconnect to
// us, which is not a thing it does; "internal server error" reports our own
// fault as though the supplier had one.
//
// 1001 Going Away is defined for both directions ("a server going down OR a
// browser having navigated away") and says it exactly: the peer that dialed you
// is leaving.
//
// Everything else passes through unchanged — 1000 means the same thing in both
// directions, and application codes (3000-4999, notably the miner's own 4000 at
// session expiry) are propagated deliberately, as they are through
// sanitizeCloseCode above.
//
// gorilla accepts 1012 on read, so this is not about a protocol error the way
// the 1006 in sanitizeCloseCode is; it is about the operator on the other end
// being told something true.
//
// LIFT: SAGE websockets/bridge.go (72ac553), itself a port of PATH d5ef007c.
func endpointCloseCode(clientCode int) int {
	switch clientCode {
	case websocket.CloseInternalServerErr, // 1011
		websocket.CloseServiceRestart, // 1012
		websocket.CloseTryAgainLater:  // 1013
		return websocket.CloseGoingAway // 1001
	}
	return clientCode
}

// determineCloseCode picks the close code the peers should see.
//
// A close code a peer actually sent wins, so the real reason survives the trip
// rather than being flattened into a generic internal error. Otherwise the
// shutdown error decides. Anything the client should retry maps to
// CloseServiceRestart — notably session expiry, which is routine and not a fault.
func (b *Bridge) determineCloseCode(err error) (int, string) {
	if b.endpointConn != nil {
		if code, text := b.endpointConn.GetCloseInfo(); code != 0 {
			return code, text
		}
	}
	if b.clientConn != nil {
		if code, text := b.clientConn.GetCloseInfo(); code != 0 {
			return code, text
		}
	}

	switch {
	case err == nil,
		errors.Is(err, ErrBridgeContextCanceled),
		errors.Is(err, context.Canceled):
		return websocket.CloseServiceRestart, "service restarting, please reconnect"

	case errors.Is(err, ErrBridgeSessionExpired):
		return websocket.CloseServiceRestart, "session ended, please reconnect"

	case errors.Is(err, ErrBridgeEndpointUnavailable):
		return websocket.CloseServiceRestart, "endpoint temporarily unavailable, please reconnect"

	case errors.Is(err, ErrBridgeMessageProcessing):
		return websocket.CloseInternalServerErr, "message processing error"

	case errors.Is(err, ErrBridgeConnectionFailed):
		return websocket.CloseInternalServerErr, "connection error"

	default:
		return websocket.CloseInternalServerErr, fmt.Sprintf("bridge error: %s", err.Error())
	}
}
