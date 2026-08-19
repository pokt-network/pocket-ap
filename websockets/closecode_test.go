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

// The bug in full, end to end over real sockets: a supplier that drops its TCP
// connection with no close handshake makes gorilla report *CloseError{1006},
// which extractCloseInfo captures and determineCloseCode hands back. Without
// sanitization that 1006 is encoded into a frame written to BOTH peers, and the
// client rejects it — "websocket: bad close code 1006" — so it never learns why
// it was disconnected.
//
// Asserting the client gets a legible *CloseError is what makes this a
// reproduction rather than a description: before the fix, this test fails on the
// error text.
func TestBridge_AbruptEndpointCloseReachesClientAsASendableCode(t *testing.T) {
	// A supplier that hijacks the connection and closes the TCP socket outright,
	// skipping the close handshake entirely.
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	supplier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// NetConn().Close() rather than conn.Close(): no close frame is sent.
		_ = conn.UnderlyingConn().Close()
	}))
	defer supplier.Close()

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := StartBridge(context.Background(), testLogger(), OriginPolicy{}, r, w,
			"ws"+strings.TrimPrefix(supplier.URL, "http"), nil, &echoProcessor{})
		if err != nil {
			return
		}
		<-b.Done()
	}))
	defer front.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer func() { _ = client.Close() }()

	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, readErr := client.ReadMessage()
	if readErr == nil {
		t.Fatal("client read succeeded; expected the bridge to close it")
	}

	closeErr, ok := readErr.(*websocket.CloseError)
	if !ok {
		// This is the shipped-bug branch: gorilla fails the frame before it ever
		// becomes a CloseError.
		t.Fatalf("client got %q, not a close frame — a reserved code was put on the wire", readErr)
	}
	if !isSendableCloseCode(closeErr.Code) {
		t.Errorf("client received close code %d, which RFC 6455 §7.4.1 forbids sending", closeErr.Code)
	}
	if closeErr.Code != websocket.CloseInternalServerErr {
		t.Errorf("close code = %d, want %d (1011): the honest answer when the failure is on our side",
			closeErr.Code, websocket.CloseInternalServerErr)
	}
}

// A peer chooses the number we are handed, not us, so every input has to come
// out sendable — including ones no RFC defines.
func TestSanitizeCloseCode_EveryInputBecomesSendable(t *testing.T) {
	for code := 0; code <= 5100; code++ {
		if got := sanitizeCloseCode(code); !isSendableCloseCode(got) {
			t.Fatalf("sanitizeCloseCode(%d) = %d, which is not sendable", code, got)
		}
	}
}

// Legal codes must survive untouched. The application range matters most: the
// bridge propagates supplier-chosen codes such as 4000 for session expiry, and
// flattening those to 1011 would destroy the reason mid-flight.
func TestSanitizeCloseCode_PassesLegalCodesThrough(t *testing.T) {
	tests := []struct {
		name string
		code int
		want int
	}{
		{"normal closure", websocket.CloseNormalClosure, websocket.CloseNormalClosure},
		{"service restart", websocket.CloseServiceRestart, websocket.CloseServiceRestart},
		{"try again later", websocket.CloseTryAgainLater, websocket.CloseTryAgainLater},
		{"application range low", 3000, 3000},
		{"supplier session expiry", 4000, 4000},
		{"application range high", 4999, 4999},

		{"1005 no status received", websocket.CloseNoStatusReceived, websocket.CloseInternalServerErr},
		{"1006 abnormal closure", websocket.CloseAbnormalClosure, websocket.CloseInternalServerErr},
		{"1015 TLS handshake", websocket.CloseTLSHandshake, websocket.CloseInternalServerErr},
		{"below the protocol range", 999, websocket.CloseInternalServerErr},
		{"between the ranges", 2000, websocket.CloseInternalServerErr},
		{"above the application range", 5000, websocket.CloseInternalServerErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeCloseCode(tt.code); got != tt.want {
				t.Errorf("sanitizeCloseCode(%d) = %d, want %d", tt.code, got, tt.want)
			}
		})
	}
}

// The one failure that lands after the client upgrade but before a bridge
// exists. A bare Close there skips the handshake and the client reports 1006 or
// a protocol error — as if it had done something wrong, when the endpoint we
// picked for it is down. 1013 says the actionable thing: reconnect, and a
// different supplier gets selected.
func TestStartBridge_EndpointDialFailureClosesClientWith1013(t *testing.T) {
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Port 1 on loopback: nothing listens, so ConnectEndpoint fails after the
		// client upgrade has already succeeded.
		_, _ = StartBridge(context.Background(), testLogger(), OriginPolicy{}, r, w,
			"ws://127.0.0.1:1", nil, &echoProcessor{})
	}))
	defer front.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer func() { _ = client.Close() }()

	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, readErr := client.ReadMessage()

	closeErr, ok := readErr.(*websocket.CloseError)
	if !ok {
		t.Fatalf("client got %v, want a close frame", readErr)
	}
	if closeErr.Code != websocket.CloseTryAgainLater {
		t.Errorf("close code = %d, want %d (1013 try again later)", closeErr.Code, websocket.CloseTryAgainLater)
	}
}
