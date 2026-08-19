package websockets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- test doubles -----------------------------------------------------------

// echoProcessor tags frames so a test can prove each direction ran through the
// right hook — the bug that matters here is the two being swapped.
type echoProcessor struct {
	mu             sync.Mutex
	clientFrames   [][]byte
	endpointFrames [][]byte
	clientErr      error
	endpointErr    error
	clientPrefix   string
	endpointPrefix string
}

func (p *echoProcessor) ProcessClientMessage(data []byte) ([]byte, error) {
	p.mu.Lock()
	p.clientFrames = append(p.clientFrames, append([]byte(nil), data...))
	p.mu.Unlock()
	if p.clientErr != nil {
		return nil, p.clientErr
	}
	return []byte(p.clientPrefix + string(data)), nil
}

func (p *echoProcessor) ProcessEndpointMessage(data []byte) ([]byte, error) {
	p.mu.Lock()
	p.endpointFrames = append(p.endpointFrames, append([]byte(nil), data...))
	p.mu.Unlock()
	if p.endpointErr != nil {
		return nil, p.endpointErr
	}
	return []byte(p.endpointPrefix + string(data)), nil
}

func (p *echoProcessor) seen() (client, endpoint int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.clientFrames), len(p.endpointFrames)
}

// fakeSupplier stands in for a relay miner's WebSocket endpoint. It records the
// handshake headers (the miner auth contract) and echoes frames back.
type fakeSupplier struct {
	srv *httptest.Server

	mu         sync.Mutex
	gotHeaders http.Header
	received   [][]byte

	// onConnect, if set, runs with the live conn instead of echoing.
	onConnect func(conn *websocket.Conn)
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

		if f.onConnect != nil {
			f.onConnect(conn)
			return
		}
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.received = append(f.received, append([]byte(nil), data...))
			f.mu.Unlock()
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

// bridgeHarness wires a client → bridge → fakeSupplier chain over real sockets.
type bridgeHarness struct {
	client   *websocket.Conn
	bridge   *Bridge
	supplier *fakeSupplier
	proc     *echoProcessor
}

func startHarness(t *testing.T, policy OriginPolicy, proc *echoProcessor, headers http.Header) *bridgeHarness {
	t.Helper()
	supplier := newFakeSupplier(t)

	var (
		bridge  *Bridge
		bridgeC = make(chan *Bridge, 1)
	)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := StartBridge(context.Background(), testLogger(), policy, r, w, supplier.wsURL(), headers, proc)
		if err != nil {
			return
		}
		bridgeC <- b
		<-b.Done()
	}))
	t.Cleanup(front.Close)

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	select {
	case bridge = <-bridgeC:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge never started")
	}
	return &bridgeHarness{client: client, bridge: bridge, supplier: supplier, proc: proc}
}

// --- tests ------------------------------------------------------------------

// The core contract: a client frame reaches the supplier having gone through
// ProcessClientMessage, and the reply comes back having gone through
// ProcessEndpointMessage. Swapping the two would still "work" superficially.
func TestBridge_RoutesBothDirectionsThroughTheRightHook(t *testing.T) {
	proc := &echoProcessor{clientPrefix: "signed:", endpointPrefix: "validated:"}
	h := startHarness(t, OriginPolicy{}, proc, nil)

	if err := h.client.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatalf("client write: %v", err)
	}

	_ = h.client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, got, err := h.client.ReadMessage()
	if err != nil {
		t.Fatalf("client read: %v", err)
	}

	// client "hello" -> ProcessClientMessage -> "signed:hello" -> supplier echoes
	// -> ProcessEndpointMessage -> "validated:signed:hello"
	if want := "validated:signed:hello"; string(got) != want {
		t.Errorf("client got %q, want %q", got, want)
	}
	if c, e := proc.seen(); c != 1 || e != 1 {
		t.Errorf("processor saw client=%d endpoint=%d frames, want 1 and 1", c, e)
	}
}

// The handshake headers are the miner's auth contract: without them the miner
// treats the connection as anonymous and refuses the upgrade. They are set once,
// on the dial, so nothing downstream can repair a mistake here.
func TestBridge_PassesEndpointHandshakeHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set("Target-Service-Id", "pnf-anvil")
	headers.Set("App-Address", "pokt1app")
	headers.Set("Rpc-Type", "2")

	h := startHarness(t, OriginPolicy{}, &echoProcessor{}, headers)
	// Force the handshake to have completed before asserting.
	_ = h.client.WriteMessage(websocket.TextMessage, []byte("x"))
	_ = h.client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = h.client.ReadMessage()

	got := h.supplier.headers()
	for name, want := range map[string]string{
		"Target-Service-Id": "pnf-anvil",
		"App-Address":       "pokt1app",
		"Rpc-Type":          "2",
	} {
		if got.Get(name) != want {
			t.Errorf("supplier handshake header %s = %q, want %q", name, got.Get(name), want)
		}
	}
}

// Frame type rides in the WebSocket metadata, not the payload, so a binary
// subscription must not silently arrive as text.
func TestBridge_PreservesFrameType(t *testing.T) {
	proc := &echoProcessor{}
	h := startHarness(t, OriginPolicy{}, proc, nil)

	if err := h.client.WriteMessage(websocket.BinaryMessage, []byte{0x01, 0x02, 0x03}); err != nil {
		t.Fatalf("client write: %v", err)
	}
	_ = h.client.SetReadDeadline(time.Now().Add(3 * time.Second))
	mt, got, err := h.client.ReadMessage()
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if mt != websocket.BinaryMessage {
		t.Errorf("frame type = %d, want BinaryMessage(%d)", mt, websocket.BinaryMessage)
	}
	if len(got) != 3 {
		t.Errorf("payload = %v, want 3 bytes round-tripped", got)
	}
}

// A signing failure must tear the bridge down rather than silently drop the
// frame — a client left waiting forever on a subscription is the worse failure.
func TestBridge_ClientProcessingErrorShutsDown(t *testing.T) {
	proc := &echoProcessor{clientErr: errors.New("sign failed")}
	h := startHarness(t, OriginPolicy{}, proc, nil)

	_ = h.client.WriteMessage(websocket.TextMessage, []byte("hello"))

	select {
	case <-h.bridge.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("bridge stayed up after a processing error")
	}
}

// A supplier frame that fails validation is terminal too: we cannot hand the
// client bytes we could not verify.
func TestBridge_EndpointProcessingErrorShutsDown(t *testing.T) {
	proc := &echoProcessor{endpointErr: errors.New("bad signature")}
	h := startHarness(t, OriginPolicy{}, proc, nil)

	_ = h.client.WriteMessage(websocket.TextMessage, []byte("hello"))

	select {
	case <-h.bridge.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("bridge stayed up after a validation failure")
	}
}

func TestBridge_ClientDisconnectShutsDown(t *testing.T) {
	h := startHarness(t, OriginPolicy{}, &echoProcessor{}, nil)
	_ = h.client.Close()

	select {
	case <-h.bridge.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("bridge stayed up after the client vanished")
	}
}

// Shutdown converges from every path and must be safe to call repeatedly and
// concurrently — session expiry and a read error can race.
func TestBridge_ShutdownIsIdempotentAndConcurrencySafe(t *testing.T) {
	h := startHarness(t, OriginPolicy{}, &echoProcessor{}, nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.bridge.Shutdown(fmt.Errorf("shutdown %d", i))
		}(i)
	}
	wg.Wait()

	select {
	case <-h.bridge.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("bridge never finished shutting down")
	}
	// And once more after the fact.
	h.bridge.Shutdown(errors.New("again"))
}

// Session expiry is routine, not a fault: the client must be told to reconnect,
// which is the whole reason closing beats re-signing.
func TestBridge_SessionExpiryClosesWithServiceRestart(t *testing.T) {
	h := startHarness(t, OriginPolicy{}, &echoProcessor{}, nil)

	h.bridge.Shutdown(ErrBridgeSessionExpired)

	_ = h.client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err := h.client.ReadMessage()
	if err == nil {
		t.Fatal("client read succeeded, want a close error")
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("err = %v, want a websocket close error", err)
	}
	if closeErr.Code != websocket.CloseServiceRestart {
		t.Errorf("close code = %d, want CloseServiceRestart(%d)", closeErr.Code, websocket.CloseServiceRestart)
	}
	if !strings.Contains(closeErr.Text, "reconnect") {
		t.Errorf("close text = %q, want it to tell the client to reconnect", closeErr.Text)
	}
}

// A close code the supplier actually sent must reach the client intact, rather
// than being flattened into a generic internal error.
func TestBridge_PropagatesEndpointCloseCode(t *testing.T) {
	supplier := newFakeSupplier(t)
	supplier.onConnect = func(conn *websocket.Conn) {
		msg := websocket.FormatCloseMessage(4000, "supplier said so")
		_ = conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
		time.Sleep(50 * time.Millisecond)
	}

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := StartBridge(context.Background(), testLogger(), OriginPolicy{}, r, w,
			supplier.wsURL(), nil, &echoProcessor{})
		if err != nil {
			return
		}
		<-b.Done()
	}))
	defer front.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = client.ReadMessage()

	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("err = %v, want a close error", err)
	}
	if closeErr.Code != 4000 {
		t.Errorf("close code = %d, want the supplier's 4000 propagated", closeErr.Code)
	}
}

// startBridgeErr dials a front server that calls StartBridge with endpointURL,
// and returns StartBridge's error. The error travels by channel: it is produced
// on the server's handler goroutine and read here, so a shared variable would be
// both racy and readable before the handler has run.
func startBridgeErr(t *testing.T, endpointURL string, dialHeaders http.Header) (*http.Response, error) {
	t.Helper()
	errC := make(chan error, 1)

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := StartBridge(context.Background(), testLogger(), OriginPolicy{}, r, w,
			endpointURL, nil, &echoProcessor{})
		errC <- err
	}))
	defer front.Close()

	client, resp, dialErr := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http"), dialHeaders)
	if dialErr == nil {
		// The client upgrade can succeed before the endpoint dial fails; drain and
		// close so the handler is not left blocked.
		_ = client.SetReadDeadline(time.Now().Add(time.Second))
		_, _, _ = client.ReadMessage()
		_ = client.Close()
	}

	select {
	case err := <-errC:
		return resp, err
	case <-time.After(3 * time.Second):
		t.Fatal("StartBridge never returned")
		return nil, nil
	}
}

// A rejected origin must fail before the upgrade, with a 403 and no bridge.
func TestStartBridge_RejectsCrossOriginBeforeUpgrading(t *testing.T) {
	supplier := newFakeSupplier(t)

	headers := http.Header{}
	headers.Set("Origin", "https://evil.example.com")
	resp, err := startBridgeErr(t, supplier.wsURL(), headers)

	if !errors.Is(err, ErrBridgeOriginRejected) {
		t.Errorf("StartBridge err = %v, want ErrBridgeOriginRejected", err)
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %v, want 403", resp)
	}
}

// A supplier that advertised WebSocket but will not accept a connection is the
// supplier's fault, and the caller needs to tell that apart to fail over.
func TestStartBridge_EndpointUnavailableIsDistinguishable(t *testing.T) {
	// Port 9 discards: nothing accepts a WebSocket there.
	_, err := startBridgeErr(t, "ws://127.0.0.1:9", nil)
	if !errors.Is(err, ErrBridgeEndpointUnavailable) {
		t.Errorf("StartBridge err = %v, want ErrBridgeEndpointUnavailable", err)
	}
}

func TestStartBridge_InvalidEndpointURL(t *testing.T) {
	_, err := startBridgeErr(t, "://not a url", nil)
	if !errors.Is(err, ErrBridgeEndpointUnavailable) {
		t.Errorf("StartBridge err = %v, want ErrBridgeEndpointUnavailable", err)
	}
}

// Cancelling the parent context must take the bridge down: this is how shutdown
// reaches every live socket.
func TestBridge_ContextCancellationShutsDown(t *testing.T) {
	supplier := newFakeSupplier(t)
	ctx, cancel := context.WithCancel(context.Background())

	bridgeC := make(chan *Bridge, 1)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := StartBridge(ctx, testLogger(), OriginPolicy{}, r, w, supplier.wsURL(), nil, &echoProcessor{})
		if err != nil {
			return
		}
		bridgeC <- b
		<-b.Done()
	}))
	defer front.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	bridge := <-bridgeC
	cancel()

	select {
	case <-bridge.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("bridge survived context cancellation")
	}
}

// Many frames in flight, exercising the read loops and the routing loop under
// -race. This is the closest offline stand-in for a busy eth_subscribe stream.
func TestBridge_SustainedFrameFlow(t *testing.T) {
	proc := &echoProcessor{}
	h := startHarness(t, OriginPolicy{}, proc, nil)

	const frames = 50
	for i := 0; i < frames; i++ {
		if err := h.client.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("frame-%d", i))); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	for i := 0; i < frames; i++ {
		_ = h.client.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, _, err := h.client.ReadMessage(); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if c, e := proc.seen(); c != frames || e != frames {
		t.Errorf("processor saw client=%d endpoint=%d, want %d each", c, e, frames)
	}
}
