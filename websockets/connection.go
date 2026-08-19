package websockets

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// MessageSource identifies which side of the bridge a message originates from.
type MessageSource int

const (
	SourceClient   MessageSource = iota
	SourceEndpoint MessageSource = iota
)

func (m MessageSource) String() string {
	if m == SourceClient {
		return "client"
	}
	return "endpoint"
}

// message is an internal envelope routing data through the bridge's msgChan.
type message struct {
	source      MessageSource
	messageType int
	data        []byte
}

// Connection wraps a gorilla websocket.Conn with:
//   - a write mutex (gorilla is not safe for concurrent writes)
//   - thread-safe close-code storage for bidirectional propagation
//   - a labelled source for logging and routing
type Connection struct {
	conn   *websocket.Conn
	logger *slog.Logger
	source MessageSource

	writeMu sync.Mutex

	closeInfoMu   sync.Mutex
	lastCloseCode int
	lastCloseText string
}

// maxFrameBytes caps a single inbound WebSocket frame.
//
// gorilla does NOT bound reads by default: its check is
// `if c.readLimit > 0 && c.readLength > c.readLimit` (conn.go:924), so the zero
// value means unlimited. Without SetReadLimit, one frame from a hostile supplier
// — or a broken client — is buffered whole into memory, and suppliers are in the
// threat model.
//
// Generous: relay envelopes are small, but an LLM stream chunk need not be.
const maxFrameBytes = 16 << 20 // 16 MiB

// NewConnection creates a Connection wrapper. It starts no goroutines.
func NewConnection(conn *websocket.Conn, source MessageSource, logger *slog.Logger) *Connection {
	conn.SetReadLimit(maxFrameBytes)
	return &Connection{conn: conn, logger: logger, source: source}
}

// ReadMessage reads the next message. Safe from a single goroutine; do not call
// concurrently — gorilla permits only one reader.
func (c *Connection) ReadMessage() (messageType int, data []byte, err error) {
	return c.conn.ReadMessage()
}

// writeWait bounds a single data-frame write. Without it, a peer that stops
// reading (a paused browser tab, a wedged backend) blocks the write forever, and
// because writes are serialised on writeMu that one stall takes the whole bridge
// with it. gorilla's control-frame writes already carry their own deadline; this
// is the data-frame path.
//
// LIFT: PATH added this hardening in 0f960d95 (websockets/connection.go). SAGE
// never carried it back, and this package was lifted from SAGE — so it arrived
// here missing.
const writeWait = 10 * time.Second

// WriteMessage writes a data message. Safe from multiple goroutines.
func (c *Connection) WriteMessage(messageType int, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
		return err
	}
	return c.conn.WriteMessage(messageType, data)
}

// WriteControl writes a control frame (close, ping, pong). Safe to call
// concurrently with WriteMessage.
func (c *Connection) WriteControl(messageType int, data []byte, deadline time.Time) error {
	// gorilla's WriteControl takes its own internal lock, but we serialise with
	// WriteMessage for correctness across gorilla versions.
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteControl(messageType, data, deadline)
}

// Close closes the underlying network connection immediately.
func (c *Connection) Close() error { return c.conn.Close() }

// GetCloseInfo returns the close code and text captured from the last read
// error, or 0/"" when no close frame arrived. Safe from multiple goroutines.
func (c *Connection) GetCloseInfo() (code int, text string) {
	c.closeInfoMu.Lock()
	defer c.closeInfoMu.Unlock()
	return c.lastCloseCode, c.lastCloseText
}

// SetCloseInfo records a close code and text, normally pulled off a
// *websocket.CloseError. Safe from multiple goroutines.
func (c *Connection) SetCloseInfo(code int, text string) {
	c.closeInfoMu.Lock()
	defer c.closeInfoMu.Unlock()
	c.lastCloseCode = code
	c.lastCloseText = text
}

// extractCloseInfo pulls a close code and text out of a ReadMessage error.
// Returns 0,"" when the error is not a close error. Non-standard codes (e.g.
// 4000) are captured too, which is how a supplier's own close reason reaches
// the client.
func extractCloseInfo(err error) (int, string) {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		return closeErr.Code, closeErr.Text
	}
	return 0, ""
}
