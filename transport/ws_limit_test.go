package transport

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/pokt-network/pocket-ap/domain"
	"github.com/pokt-network/pocket-ap/relay"
	"github.com/pokt-network/pocket-ap/websockets"
)

// Past the cap the listener must refuse before doing any relay work, and it must
// refuse as an honest HTTP error: the rejection happens pre-upgrade, so a status
// code is still available and is far more legible than a close frame.
func TestWS_RefusesBeyondTheConnectionCap(t *testing.T) {
	supplier := newFakeSupplier(t)
	ws, url := startWS(t, preparerFor(supplier, &passthroughProcessor{}), nil)
	ws.limiter = websockets.NewConnectionLimiter(2)

	var live []*websocket.Conn
	for i := 0; i < 2; i++ {
		c, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatalf("dial %d refused within the cap: %v", i, err)
		}
		defer func() { _ = c.Close() }()
		live = append(live, c)
	}
	waitForActive(t, ws, 2)

	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("third dial succeeded with a cap of 2")
	}
	if resp == nil {
		t.Fatalf("third dial failed without an HTTP response: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	// A rejection must not consume a slot; if it did, the cap would ratchet down.
	if got := ws.limiter.Active(); got != 2 {
		t.Errorf("Active() = %d after a rejection, want 2", got)
	}

	// Closing a live connection frees its slot, so the next client gets in. This
	// is what proves the defer covers the whole connection lifetime rather than
	// firing when the handler is entered.
	_ = live[0].Close()
	waitForActive(t, ws, 1)

	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial after a slot freed: %v", err)
	}
	_ = c.Close()
}

// The reservation sits before prepare, so a flood arriving at capacity costs one
// atomic load each rather than a session lookup each — and a rejected client
// must never reach the Shannon side at all.
func TestWS_CapIsCheckedBeforePrepare(t *testing.T) {
	supplier := newFakeSupplier(t)

	prepares := make(chan struct{}, 16)
	inner := preparerFor(supplier, &passthroughProcessor{})
	counting := func(ctx context.Context, s domain.ServiceID, r domain.RPCType) (*relay.Prepared, error) {
		prepares <- struct{}{}
		return inner(ctx, s, r)
	}

	ws, url := startWS(t, counting, nil)
	ws.limiter = websockets.NewConnectionLimiter(1)

	// Fill the single slot.
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	waitForActive(t, ws, 1)
	<-prepares // the accepted connection's own prepare

	if _, _, err := websocket.DefaultDialer.Dial(url, nil); err == nil {
		t.Fatal("dial succeeded at capacity")
	}

	select {
	case <-prepares:
		t.Error("prepare ran for a connection the cap rejected")
	case <-time.After(200 * time.Millisecond):
	}
}

func waitForActive(t *testing.T, ws *WS, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ws.limiter.Active() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Active() = %d, want %d", ws.limiter.Active(), want)
}
