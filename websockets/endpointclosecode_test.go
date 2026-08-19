package websockets

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Session rollover is the most common way a bridge ends here, and until this fix
// it told the relay miner "session ended, please reconnect" — 1012, a code RFC
// 6455 §7.4.1 defines as something a SERVER tells a client. We are the miner's
// client, so it was being asked to reconnect to us, which is not a thing it
// does.
//
// Asserted through Shutdown over real sockets rather than against
// endpointCloseCode directly: a mapping function that the production path never
// consults passes its own unit tests perfectly. Both peers are read in the same
// run, because "the client still gets 1012" is half the claim — a fix that
// changed both directions would be a different bug.
func TestBridge_EachPeerGetsACloseCodeThatFitsItsRole(t *testing.T) {
	supplierCode := make(chan int, 1)

	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	supplier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, readErr := conn.ReadMessage()
		if closeErr, ok := readErr.(*websocket.CloseError); ok {
			supplierCode <- closeErr.Code
			return
		}
		// Anything else means the frame was rejected or the socket just dropped;
		// 0 fails the assertion below with the code it did not get.
		supplierCode <- 0
	}))
	defer supplier.Close()

	bridges := make(chan *Bridge, 1)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := StartBridge(context.Background(), testLogger(), OriginPolicy{}, r, w,
			"ws"+strings.TrimPrefix(supplier.URL, "http"), nil, &echoProcessor{})
		if err != nil {
			return
		}
		bridges <- b
		<-b.Done()
	}))
	defer front.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer func() { _ = client.Close() }()

	var b *Bridge
	select {
	case b = <-bridges:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge never started")
	}

	// The real rollover path: transport.WS.watchExpiry calls exactly this when the
	// chain height reaches the session's end height.
	b.Shutdown(ErrBridgeSessionExpired)

	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, clientErr := client.ReadMessage()
	clientClose, ok := clientErr.(*websocket.CloseError)
	if !ok {
		t.Fatalf("client got %q, not a close frame", clientErr)
	}
	if clientClose.Code != websocket.CloseServiceRestart {
		t.Errorf("client close code = %d, want %d (1012 service restart): downstream, "+
			"'please reconnect' is exactly what we mean and must not have changed",
			clientClose.Code, websocket.CloseServiceRestart)
	}

	select {
	case got := <-supplierCode:
		if got != websocket.CloseGoingAway {
			t.Errorf("supplier close code = %d, want %d (1001 going away): upstream we are the "+
				"CLIENT, so 1011/1012/1013 invert the roles — %d asks the miner to reconnect to us",
				got, websocket.CloseGoingAway, got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supplier never observed a close frame")
	}
}

// The three server-role codes are the ones that invert upstream; everything else
// has to survive untouched. The miner's own 4000 at session expiry is the case
// that matters most — flattening an application code would destroy the reason
// mid-flight, which is the same thing sanitizeCloseCode is careful not to do.
func TestEndpointCloseCode_OnlyServerRoleCodesAreRemapped(t *testing.T) {
	remapped := map[int]bool{
		websocket.CloseInternalServerErr: true, // 1011
		websocket.CloseServiceRestart:    true, // 1012
		websocket.CloseTryAgainLater:     true, // 1013
	}

	for code := 1000; code <= 4999; code++ {
		got := endpointCloseCode(code)
		if remapped[code] {
			if got != websocket.CloseGoingAway {
				t.Errorf("endpointCloseCode(%d) = %d, want %d (1001)", code, got, websocket.CloseGoingAway)
			}
			continue
		}
		if got != code {
			t.Errorf("endpointCloseCode(%d) = %d, want it unchanged", code, got)
		}
	}
}

// Whatever the remap produces still has to be legal to put on the wire, or we
// have swapped a role error for the 1006-class bug sanitizeCloseCode exists to
// prevent.
func TestEndpointCloseCode_OutputIsAlwaysSendable(t *testing.T) {
	for code := 0; code <= 5100; code++ {
		if got := endpointCloseCode(sanitizeCloseCode(code)); !isSendableCloseCode(got) {
			t.Fatalf("endpointCloseCode(sanitizeCloseCode(%d)) = %d, which is not sendable", code, got)
		}
	}
}
