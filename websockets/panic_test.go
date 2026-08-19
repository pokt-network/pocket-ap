package websockets

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/pokt-network/pocket-ap/internal/safego"
)

// panicProcessor reproduces the shape of the crash pocket/pubkey.go documents: a
// supplier whose account has never signed a transaction makes the SDK return
// (nil, nil), and the nil pubkey boxed into a nil any blew up a single-value
// type assertion inside ValidateFrame — which runs on the bridge's own
// goroutine, so it took the process down with every other listener and app.
type panicProcessor struct{}

func (panicProcessor) ProcessClientMessage([]byte) ([]byte, error) {
	panic("nil pubkey in a type assertion")
}

func (panicProcessor) ProcessEndpointMessage(data []byte) ([]byte, error) { return data, nil }

// Two claims, and the second is the one that is easy to miss.
//
// The process surviving is proved by this test returning at all — before the
// fix the panic escaped and killed the test binary, so there was nothing left to
// assert with.
//
// The bridge still having to SHUT DOWN is the other half. transport.WS.handle
// parks a handler goroutine on Done() for the connection's whole life and the
// connection limiter releases its slot from the same defer, so a routing loop
// that merely stopped would leak both — permanently, since an idle
// subscription's socket never closes on its own. Containment without the
// shutdown would trade a crashed process for a wedged one.
func TestBridge_APanickingProcessorClosesTheBridgeInsteadOfTheProcess(t *testing.T) {
	before := safego.Panics()

	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	supplier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer supplier.Close()

	done := make(chan struct{})
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := StartBridge(context.Background(), testLogger(), OriginPolicy{}, r, w,
			"ws"+strings.TrimPrefix(supplier.URL, "http"), nil, panicProcessor{})
		if err != nil {
			return
		}
		<-b.Done()
		close(done)
	}))
	defer front.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"method":"eth_subscribe"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Done() never closed: the panic was contained but the bridge wedged, " +
			"leaking the handler goroutine and its connection slot")
	}

	// The client is told, rather than left holding a socket nothing will answer —
	// and told the truth. 1011 is what ErrBridgeMessageProcessing means; the
	// bridge reaching 1012 "service restarting, please reconnect" would mean the
	// panic was caught by the outer safego.Go wrapping run() instead, which keeps
	// the process alive but reports a poison frame as a routine restart.
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, readErr := client.ReadMessage()
	closeErr, ok := readErr.(*websocket.CloseError)
	if !ok {
		t.Fatalf("client got %q, not a close frame", readErr)
	}
	if !isSendableCloseCode(closeErr.Code) {
		t.Errorf("client received close code %d, which RFC 6455 §7.4.1 forbids sending", closeErr.Code)
	}
	if closeErr.Code != websocket.CloseInternalServerErr {
		t.Errorf("client close code = %d, want %d (1011 message processing error); "+
			"%d would mean the frame's own failure was reported as a restart",
			closeErr.Code, websocket.CloseInternalServerErr, closeErr.Code)
	}

	if got := safego.Panics() - before; got == 0 {
		t.Error("safego.Panics() did not move: a contained panic that is never counted is " +
			"invisible to /health, which is the only place an operator would see it")
	}
}
