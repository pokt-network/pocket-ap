package transport

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/pokt-network/pocket-ap/domain"
	"github.com/pokt-network/pocket-ap/relay"
)

func init() { slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) }

// fakeSupplier echoes frames and records the handshake headers.
type fakeSupplier struct {
	srv *httptest.Server

	mu         sync.Mutex
	gotHeaders http.Header
}

func newFakeSupplier(t *testing.T) *fakeSupplier {
	t.Helper()
	f := &fakeSupplier{}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.gotHeaders = r.Header.Clone()
		f.mu.Unlock()

		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSupplier) wsURL() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") }

func (f *fakeSupplier) headers() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotHeaders
}

// passthroughProcessor stands in for relay.FrameProcessor: Shannon signing is
// covered by relay's own tests, so this keeps the transport tests about the
// transport.
type passthroughProcessor struct{ clientErr error }

func (p *passthroughProcessor) ProcessClientMessage(d []byte) ([]byte, error) {
	if p.clientErr != nil {
		return nil, p.clientErr
	}
	return d, nil
}
func (p *passthroughProcessor) ProcessEndpointMessage(d []byte) ([]byte, error) { return d, nil }
func (p *passthroughProcessor) Deactivate()                                     {}

// freeAddr reserves a loopback port and releases it, so the listener under test
// can bind a real address.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// startWS runs a WS listener with no session watching.
func startWS(t *testing.T, prepare PrepareFunc, allowedOrigins []string) (*WS, string) {
	t.Helper()
	return startWSWithHeight(t, prepare, nil, allowedOrigins, 0)
}

// startWSWithHeight runs a WS listener that watches sessions against chainHeight.
// A non-zero expiryCheck overrides the production tick so tests need not wait
// seconds for a boundary.
func startWSWithHeight(t *testing.T, prepare PrepareFunc, chainHeight func() int64, allowedOrigins []string, expiryCheck time.Duration) (*WS, string) {
	t.Helper()
	addr := freeAddr(t)
	ws := NewWS(addr, "pnf-anvil", domain.RPCTypeWebSocket, prepare, chainHeight, allowedOrigins, nil)
	if expiryCheck > 0 {
		ws.expiryCheck = expiryCheck
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = ws.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("WS.Serve did not return after cancel")
		}
	})

	// Wait for the listener rather than sleeping blind.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
			_ = c.Close()
			return ws, "ws://" + addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("WS listener never came up")
	return nil, ""
}

func preparerFor(supplier *fakeSupplier, proc relay.BridgeProcessor) PrepareFunc {
	return func(_ context.Context, serviceID domain.ServiceID, rpcType domain.RPCType) (*relay.Prepared, error) {
		return &relay.Prepared{
			Session:     &domain.Session{ID: "s1", AppAddr: "pokt1app"},
			Supplier:    "supplierA",
			EndpointURL: supplier.wsURL(),
			Headers: map[string][]string{
				relay.HeaderTargetServiceID: {string(serviceID)},
				relay.HeaderAppAddress:      {"pokt1app"},
				relay.HeaderRPCType:         {"2"},
			},
			Processor: proc,
		}, nil
	}
}

// --- tests ------------------------------------------------------------------

func TestWS_EndToEndFrameRoundTrip(t *testing.T) {
	supplier := newFakeSupplier(t)
	_, url := startWS(t, preparerFor(supplier, &passthroughProcessor{}), nil)

	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"method":"eth_subscribe"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, got, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != `{"method":"eth_subscribe"}` {
		t.Errorf("round trip = %q", got)
	}
}

func TestWS_PassesMinerHandshakeHeaders(t *testing.T) {
	supplier := newFakeSupplier(t)
	_, url := startWS(t, preparerFor(supplier, &passthroughProcessor{}), nil)

	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	_ = client.WriteMessage(websocket.TextMessage, []byte("x"))
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = client.ReadMessage()

	got := supplier.headers()
	for name, want := range map[string]string{
		relay.HeaderTargetServiceID: "pnf-anvil",
		relay.HeaderAppAddress:      "pokt1app",
		relay.HeaderRPCType:         "2",
	} {
		if got.Get(name) != want {
			t.Errorf("handshake header %s = %q, want %q", name, got.Get(name), want)
		}
	}
}

// The security default, enforced at the listener a client actually reaches.
func TestWS_RejectsCrossOriginByDefault(t *testing.T) {
	supplier := newFakeSupplier(t)
	_, url := startWS(t, preparerFor(supplier, &passthroughProcessor{}), nil)

	headers := http.Header{}
	headers.Set("Origin", "https://evil.example.com")
	_, resp, err := websocket.DefaultDialer.Dial(url, headers)
	if err == nil {
		t.Fatal("cross-origin dial succeeded against the default policy")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %v, want 403", resp)
	}
}

func TestWS_AllowsConfiguredOrigin(t *testing.T) {
	supplier := newFakeSupplier(t)
	_, url := startWS(t, preparerFor(supplier, &passthroughProcessor{}), []string{"http://localhost:3000"})

	headers := http.Header{}
	headers.Set("Origin", "http://localhost:3000")
	client, _, err := websocket.DefaultDialer.Dial(url, headers)
	if err != nil {
		t.Fatalf("allowlisted origin was rejected: %v", err)
	}
	defer client.Close()
}

// Native clients send no Origin and must keep working — they are the target user.
func TestWS_AllowsNativeClientWithNoOrigin(t *testing.T) {
	supplier := newFakeSupplier(t)
	_, url := startWS(t, preparerFor(supplier, &passthroughProcessor{}), nil)

	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("native client rejected: %v", err)
	}
	defer client.Close()
}

// A Prepare failure happens before the upgrade, so it can still be an honest
// HTTP error rather than an opaque close code.
func TestWS_PrepareFailureIs502BeforeUpgrade(t *testing.T) {
	prepare := func(context.Context, domain.ServiceID, domain.RPCType) (*relay.Prepared, error) {
		return nil, errors.New("no session")
	}
	_, url := startWS(t, prepare, nil)

	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("dial succeeded despite Prepare failing")
	}
	if resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %v, want 502", resp)
	}
}

// Shutdown must close live bridges: a WebSocket never completes on its own, so
// http.Server.Shutdown alone would block on an idle subscription until timeout.
func TestWS_CloseShutsDownLiveBridges(t *testing.T) {
	supplier := newFakeSupplier(t)
	ws, url := startWS(t, preparerFor(supplier, &passthroughProcessor{}), nil)

	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// Make sure the bridge is registered before closing.
	_ = client.WriteMessage(websocket.TextMessage, []byte("x"))
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = client.ReadMessage()

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		done <- ws.Close(ctx)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked on a live bridge instead of shutting it down")
	}

	// The client must be told to reconnect, not just dropped.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = client.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("client err = %v, want a close frame", err)
	}
	if closeErr.Code != websocket.CloseServiceRestart {
		t.Errorf("close code = %d, want CloseServiceRestart(%d)", closeErr.Code, websocket.CloseServiceRestart)
	}
}

func TestWS_BridgesAreTrackedAndReleased(t *testing.T) {
	supplier := newFakeSupplier(t)
	ws, url := startWS(t, preparerFor(supplier, &passthroughProcessor{}), nil)

	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = client.WriteMessage(websocket.TextMessage, []byte("x"))
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = client.ReadMessage()

	if n := ws.bridges.len(); n != 1 {
		t.Errorf("tracked bridges = %d, want 1", n)
	}

	_ = client.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ws.bridges.len() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("tracked bridges = %d after client left, want 0 — bridges leak", ws.bridges.len())
}

func TestWS_ServeWithoutPrepareFuncFailsLoudly(t *testing.T) {
	ws := NewWS(freeAddr(t), "svc", domain.RPCTypeWebSocket, nil, nil, nil, nil)
	if err := ws.Serve(context.Background()); !errors.Is(err, errNoPrepareFunc) {
		t.Errorf("Serve err = %v, want errNoPrepareFunc", err)
	}
}

// transport.New must route the stateful type to WS and everything else to HTTP.
func TestNew_RoutesByRPCType(t *testing.T) {
	tests := []struct {
		rpcType domain.RPCType
		wantWS  bool
	}{
		{domain.RPCTypeWebSocket, true},
		{domain.RPCTypeJSONRPC, false},
		{domain.RPCTypeREST, false},
		{domain.RPCTypeCometBFT, false},
		{domain.RPCTypeGRPC, false},
	}
	for _, tt := range tests {
		t.Run(tt.rpcType.String(), func(t *testing.T) {
			got := New(Options{Addr: "127.0.0.1:0", ServiceID: "svc", RPCType: tt.rpcType})
			_, isWS := got.(*WS)
			if isWS != tt.wantWS {
				t.Errorf("New(%s) gave WS=%v, want %v", tt.rpcType, isWS, tt.wantWS)
			}
			if got.RPCType() != tt.rpcType {
				t.Errorf("RPCType() = %v, want %v", got.RPCType(), tt.rpcType)
			}
		})
	}
}

// --- session expiry (phase 3) ------------------------------------------------

// preparerWithSession is preparerFor plus a chosen session end height.
func preparerWithSession(supplier *fakeSupplier, proc relay.BridgeProcessor, endHeight int64) PrepareFunc {
	return func(_ context.Context, serviceID domain.ServiceID, _ domain.RPCType) (*relay.Prepared, error) {
		return &relay.Prepared{
			Session:     &domain.Session{ID: "s1", AppAddr: "pokt1app", EndBlockHeight: endHeight},
			Supplier:    "supplierA",
			EndpointURL: supplier.wsURL(),
			Headers:     map[string][]string{relay.HeaderTargetServiceID: {string(serviceID)}},
			Processor:   proc,
		}, nil
	}
}

// countingProcessor records how many client frames it signed, so a test can show
// signing genuinely stopped rather than merely being asked to.
type countingProcessor struct {
	mu          sync.Mutex
	signed      int
	deactivated bool
}

func (p *countingProcessor) ProcessClientMessage(d []byte) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.deactivated {
		return nil, relay.ErrSessionExpired
	}
	p.signed++
	return d, nil
}
func (p *countingProcessor) ProcessEndpointMessage(d []byte) ([]byte, error) { return d, nil }
func (p *countingProcessor) Deactivate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deactivated = true
}
func (p *countingProcessor) state() (signed int, deactivated bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.signed, p.deactivated
}

// A connection outlives the session it was signed under, so the boundary has to
// close it — and the client has to be told to reconnect, not just dropped.
func TestWS_SessionExpiryClosesBridgeWithReconnectHint(t *testing.T) {
	supplier := newFakeSupplier(t)
	proc := &countingProcessor{}
	var height atomic.Int64
	height.Store(100)

	_, url := startWSWithHeight(t, preparerWithSession(supplier, proc, 200), height.Load, nil, 10*time.Millisecond)

	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// Still inside the session: the bridge must stay up.
	if err := client.WriteMessage(websocket.TextMessage, []byte("before")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := client.ReadMessage(); err != nil {
		t.Fatalf("bridge closed while the session was still live: %v", err)
	}

	// Cross the boundary.
	height.Store(200)

	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = client.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("client err = %v, want a close frame", err)
	}
	if closeErr.Code != websocket.CloseServiceRestart {
		t.Errorf("close code = %d, want CloseServiceRestart(%d) — expiry is routine, not a fault", closeErr.Code, websocket.CloseServiceRestart)
	}
	if !strings.Contains(closeErr.Text, "reconnect") {
		t.Errorf("close text = %q, want it to tell the client to reconnect", closeErr.Text)
	}

	if _, deactivated := proc.state(); !deactivated {
		t.Error("processor was not deactivated — frames could still be signed against a retired session")
	}
}

// Exactly at the end height counts as ended: EndBlockHeight is the last block of
// the session, and the chain has moved past it.
func TestWS_ExpiryBoundaryIsInclusive(t *testing.T) {
	tests := []struct {
		name        string
		height      int64
		wantExpired bool
	}{
		{"one block before the end", 199, false},
		{"exactly at the end height", 200, true},
		{"past the end", 201, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			supplier := newFakeSupplier(t)
			proc := &countingProcessor{}
			_, url := startWSWithHeight(t, preparerWithSession(supplier, proc, 200),
				func() int64 { return tt.height }, nil, 10*time.Millisecond)

			client, _, err := websocket.DefaultDialer.Dial(url, nil)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer client.Close()

			_ = client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			_, _, readErr := client.ReadMessage()
			gotExpired := errors.As(readErr, new(*websocket.CloseError))

			if gotExpired != tt.wantExpired {
				t.Errorf("height %d vs end 200: expired = %v, want %v (err: %v)",
					tt.height, gotExpired, tt.wantExpired, readErr)
			}
		})
	}
}

// Losing sight of the chain must not tear down live bridges on a guess: height 0
// means no poll has succeeded, not that every session ended.
func TestWS_ZeroHeightDoesNotExpireBridges(t *testing.T) {
	supplier := newFakeSupplier(t)
	proc := &countingProcessor{}
	_, url := startWSWithHeight(t, preparerWithSession(supplier, proc, 200),
		func() int64 { return 0 }, nil, 10*time.Millisecond)

	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	_ = client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := client.ReadMessage(); errors.As(err, new(*websocket.CloseError)) {
		t.Error("bridge was closed while the poller reported no height — a dead poller must not look like universal expiry")
	}
}

// No height source at all: the bridge still works, it just never self-expires.
//
// The connection is deliberately held open across many watcher ticks. Calling a
// nil chainHeight would panic on the watcher's goroutine and take the process
// down, so the test has to actually reach that call — an earlier version closed
// the client before the first tick and passed against a mutant with the nil
// guard removed, i.e. it proved nothing.
func TestWS_NilChainHeightDoesNotExpireOrPanic(t *testing.T) {
	supplier := newFakeSupplier(t)
	proc := &countingProcessor{}
	const tick = 5 * time.Millisecond
	_, url := startWSWithHeight(t, preparerWithSession(supplier, proc, 200), nil, nil, tick)

	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// Sit through ~20 ticks. With the nil guard gone this is where it dies.
	time.Sleep(20 * tick)

	if err := client.WriteMessage(websocket.TextMessage, []byte("x")); err != nil {
		t.Fatalf("write after many watcher ticks: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := client.ReadMessage(); err != nil {
		t.Fatalf("bridge broke with no height source: %v", err)
	}
}

// THE fan-out case. SAGE broadcasts expiry over one shared channel, so with more
// than one bridge some never learn their session ended (ws_relayer.go:248-255).
// Every bridge here reads the height itself, so ALL of them must close.
func TestWS_EveryConcurrentBridgeSeesExpiry(t *testing.T) {
	supplier := newFakeSupplier(t)
	var height atomic.Int64
	height.Store(100)

	_, url := startWSWithHeight(t, preparerWithSession(supplier, &countingProcessor{}, 200), height.Load, nil, 10*time.Millisecond)

	const bridges = 8
	clients := make([]*websocket.Conn, 0, bridges)
	for i := 0; i < bridges; i++ {
		c, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		defer c.Close()
		clients = append(clients, c)
	}

	height.Store(200)

	// Every one of them, not just whichever consumed the event first.
	for i, c := range clients {
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _, err := c.ReadMessage()
		var closeErr *websocket.CloseError
		if !errors.As(err, &closeErr) {
			t.Errorf("bridge %d never learned its session ended (err: %v)", i, err)
			continue
		}
		if closeErr.Code != websocket.CloseServiceRestart {
			t.Errorf("bridge %d close code = %d, want CloseServiceRestart", i, closeErr.Code)
		}
	}
}

// The watcher must not outlive its bridge.
func TestWS_ExpiryWatcherStopsWhenBridgeCloses(t *testing.T) {
	supplier := newFakeSupplier(t)
	_, url := startWSWithHeight(t, preparerWithSession(supplier, &countingProcessor{}, 200),
		func() int64 { return 100 }, nil, 5*time.Millisecond)

	before := runtime.NumGoroutine()
	for i := 0; i < 10; i++ {
		c, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		_ = c.Close()
	}

	// Goroutine counts settle asynchronously; allow them to drain.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("goroutines grew from %d to %d after 10 closed bridges — watchers are leaking",
		before, runtime.NumGoroutine())
}

// The WS listener must enforce Host too. Browsers always send Origin on a
// WebSocket handshake, so this side was already covered against rebinding — but
// two listeners enforcing different rules is how one of them ends up wrong, and
// an untested check is one refactor away from being deleted.
func TestWS_RejectsUnrecognisedHost(t *testing.T) {
	supplier := newFakeSupplier(t)
	addr := freeAddr(t)
	// Loopback bind, no explicit hosts: the derived policy applies.
	ws := NewWS(addr, "pnf-anvil", domain.RPCTypeWebSocket,
		preparerFor(supplier, &passthroughProcessor{}), nil, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ws.Serve(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// gorilla puts the URL's host in the Host header, so dialling by a name that
	// resolves to loopback is what a rebound browser looks like on the wire.
	headers := http.Header{}
	headers.Set("Host", "evil.example.com")
	_, resp, err := websocket.DefaultDialer.Dial("ws://"+addr, headers)
	if err == nil {
		t.Fatal("a websocket with an unrecognised Host was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %v, want 403", resp)
	}
}

// --- per-request supplier policy ---

// capturingPreparer records the ctx Prepare was called with, which is where the
// caller's supplier preference has to land: a bridge picks its supplier once, so
// this is the only moment the preference can apply.
func capturingPreparer(supplier *fakeSupplier, proc relay.BridgeProcessor, got *domain.SupplierPolicy, mu *sync.Mutex) PrepareFunc {
	inner := preparerFor(supplier, proc)
	return func(ctx context.Context, serviceID domain.ServiceID, rpcType domain.RPCType) (*relay.Prepared, error) {
		mu.Lock()
		*got = domain.SupplierPolicyFromContext(ctx)
		mu.Unlock()
		return inner(ctx, serviceID, rpcType)
	}
}

func TestWS_SupplierHeaderReachesPrepare(t *testing.T) {
	supplier := newFakeSupplier(t)
	var (
		mu  sync.Mutex
		got domain.SupplierPolicy
	)
	_, url := startWS(t, capturingPreparer(supplier, &passthroughProcessor{}, &got, &mu), nil)

	header := http.Header{}
	header.Set(HeaderAllowSuppliers, "pokt1aaa,pokt1bbb")
	header.Set(HeaderDenySuppliers, "pokt1ccc")
	client, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(got.Allow) != 2 || got.Allow[0] != "pokt1aaa" {
		t.Errorf("allow = %v, want [pokt1aaa pokt1bbb]", got.Allow)
	}
	if len(got.Deny) != 1 || got.Deny[0] != "pokt1ccc" {
		t.Errorf("deny = %v, want [pokt1ccc]", got.Deny)
	}
}

// The miner handshake is built from scratch in relay.Bridge.Prepare, so the
// client's headers never reach the supplier. Pinned because "it happens not to
// be forwarded" is one refactor away from "it is".
func TestWS_SupplierHeaderNeverReachesTheSupplier(t *testing.T) {
	supplier := newFakeSupplier(t)
	_, url := startWS(t, preparerFor(supplier, &passthroughProcessor{}), nil)

	header := http.Header{}
	header.Set(HeaderAllowSuppliers, "pokt1aaa")
	client, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	_ = client.WriteMessage(websocket.TextMessage, []byte("x"))
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = client.ReadMessage()

	if v := supplier.headers().Get(HeaderAllowSuppliers); v != "" {
		t.Errorf("supplier saw %s = %q", HeaderAllowSuppliers, v)
	}
}

// Rejected before Prepare: a malformed list is an instruction we cannot honour,
// and a session lookup for a connection we are about to refuse is wasted.
func TestWS_MalformedSupplierHeaderIs400BeforePrepare(t *testing.T) {
	var prepared atomic.Int64
	prepare := func(context.Context, domain.ServiceID, domain.RPCType) (*relay.Prepared, error) {
		prepared.Add(1)
		return nil, errors.New("should not be reached")
	}
	_, url := startWS(t, prepare, nil)

	header := http.Header{}
	header.Set(HeaderDenySuppliers, "not-an-address")
	_, resp, err := websocket.DefaultDialer.Dial(url, header)
	if err == nil {
		t.Fatal("dial succeeded with a malformed supplier header")
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %v, want 400", resp)
	}
	if n := prepared.Load(); n != 0 {
		t.Errorf("Prepare ran %d times for a request we refused", n)
	}
}
