package transport

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/pokt-network/pocket-ap/domain"
	"github.com/pokt-network/pocket-ap/internal/safego"
	"github.com/pokt-network/pocket-ap/relay"
	"github.com/pokt-network/pocket-ap/websockets"
)

// WS is the stateful streaming front adapter. Where HTTP does one round trip per
// request, WS upgrades the client, opens one signed connection to a supplier,
// and pumps frames both ways until either side goes away.
//
// It owns the front concerns — listening, the origin policy, the upgrade — and
// delegates the Shannon concerns to relay.Bridge via PrepareFunc.

// expiryCheckInterval is how often a bridge asks whether its session has ended.
// The block poller only refreshes the height every blockPollInterval (10s), so
// checking much faster buys nothing; this notices a boundary within roughly one
// poll plus one tick.
const expiryCheckInterval = 5 * time.Second

type WS struct {
	addr        string
	serviceID   domain.ServiceID
	rpcType     domain.RPCType
	prepare     PrepareFunc
	chainHeight func() int64
	policy      websockets.OriginPolicy
	hosts       HostPolicy
	logger      *slog.Logger
	srv         *http.Server

	// expiryCheck is the watcher's tick. A field so tests need not wait seconds.
	expiryCheck time.Duration

	// bridges tracks live bridges so shutdown can close them; a WS listener's
	// connections outlive any single request, so http.Server.Shutdown alone would
	// wait on them forever.
	bridges *bridgeSet

	// limiter caps concurrent connections. Nil means no cap.
	limiter *websockets.ConnectionLimiter
}

// NewWS constructs the WebSocket adapter. chainHeight reports the newest block
// height seen and may be nil, in which case sessions are never watched — see
// watchExpiry.
func NewWS(addr string, serviceID domain.ServiceID, rpcType domain.RPCType, prepare PrepareFunc, chainHeight func() int64, allowedOrigins, allowedHosts []string) *WS {
	return &WS{
		addr:        addr,
		serviceID:   serviceID,
		rpcType:     rpcType,
		prepare:     prepare,
		chainHeight: chainHeight,
		policy:      websockets.OriginPolicy{AllowedOrigins: allowedOrigins},
		hosts:       HostPolicy{AllowedHosts: allowedHosts, BindAddr: addr},
		logger:      slog.Default().With("transport", "ws", "service", serviceID, "addr", addr),
		expiryCheck: expiryCheckInterval,
		bridges:     newBridgeSet(),
		limiter:     websockets.NewConnectionLimiter(websockets.DefaultMaxConnections),
	}
}

func (w *WS) RPCType() domain.RPCType { return w.rpcType }

// Serve starts the listener and blocks until ctx is cancelled.
func (w *WS) Serve(ctx context.Context) error {
	if w.prepare == nil {
		return errNoPrepareFunc
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) { w.handle(ctx, rw, r) })
	w.srv = &http.Server{
		Addr:    w.addr,
		Handler: mux,
		// Bounds a client that opens a connection and dawdles over the headers.
		// No ReadTimeout/WriteTimeout: those would sever long-lived streams and
		// websockets, which are the point of this listener.
		ReadHeaderTimeout: readHeaderTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	w.logger.Info("listening", "allowed_origins", w.policy.AllowedOrigins)
	errCh := make(chan error, 1)
	// Call, not Recover: a panic here has to reach errCh, or Serve would block on
	// a channel nothing will ever write to and the listener would read as running.
	go func() { errCh <- safego.Call(w.logger, "transport.ws.listen", w.srv.ListenAndServe) }()

	select {
	case <-ctx.Done():
		return w.Close(context.Background())
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// Close shuts every live bridge down, then the listener. Bridges first: an open
// WebSocket never completes on its own, so Shutdown would otherwise block until
// its timeout with the client none the wiser.
func (w *WS) Close(ctx context.Context) error {
	w.bridges.shutdownAll()
	if w.srv == nil {
		return nil
	}
	return w.srv.Shutdown(ctx)
}

// handle upgrades one client and bridges it to a supplier.
func (w *WS) handle(ctx context.Context, rw http.ResponseWriter, r *http.Request) {
	// Host first, same as the HTTP listener. Browsers always send Origin on a
	// WebSocket handshake, so this side was already covered against rebinding —
	// but the two listeners enforcing different rules is how one of them ends up
	// wrong later.
	if !w.hosts.Allows(r.Host) {
		w.logger.Warn("rejected websocket for an unrecognised Host", "host", r.Host)
		http.Error(rw, "host not allowed", http.StatusForbidden)
		return
	}

	// Reserve a slot before doing any work: a flood arriving at capacity then
	// costs one atomic load each rather than a session lookup each. A plain defer
	// covers every path because this handler blocks until the bridge closes —
	// PATH needs a goroutine handoff here only because its handler returns first.
	if !w.limiter.Acquire() {
		w.logger.Warn("rejected websocket: connection limit reached", "active", w.limiter.Active())
		http.Error(rw, "too many websocket connections", http.StatusServiceUnavailable)
		return
	}
	defer w.limiter.Release()

	// A bridge is pinned to one supplier for its life, so the caller's preference
	// applies once, here, at selection. Taken from a CLONE rather than from r:
	// unlike the HTTP path nothing forwards these headers upstream (the miner
	// handshake is built from scratch in relay.Bridge.Prepare), so there is nothing
	// to strip — only to read.
	suppliers, err := TakeSupplierPolicy(r.Header.Clone())
	if err != nil {
		w.logger.Warn("rejected websocket with a malformed supplier header", "error", err)
		http.Error(rw, "invalid supplier header: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Resolve the Shannon side before touching the client connection: a failure
	// here can still be an honest HTTP error, whereas after the upgrade the only
	// way to say anything is a close code.
	prepared, err := w.prepare(domain.WithSupplierPolicy(ctx, suppliers), w.serviceID, w.rpcType)
	if err != nil {
		w.logger.Warn("ws prepare failed", "error", err)
		http.Error(rw, "relay unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}

	logger := w.logger.With(
		"supplier", prepared.Supplier,
		"session", prepared.Session.ID,
		"url", prepared.EndpointURL,
	)

	bridge, err := websockets.StartBridge(
		ctx, logger, w.policy, r, rw,
		prepared.EndpointURL,
		http.Header(prepared.Headers),
		prepared.Processor,
	)
	if err != nil {
		// StartBridge already answered the client (403 for a rejected origin, or
		// the upgrade's own error). Nothing further to write.
		logger.Warn("ws bridge did not start", "error", err)
		return
	}

	w.bridges.add(bridge)
	defer w.bridges.remove(bridge)

	safego.Go(logger, "transport.ws.watchExpiry", func() { w.watchExpiry(bridge, prepared, logger) })

	logger.Info("ws bridge open", "session_end_height", prepared.Session.EndBlockHeight)
	<-bridge.Done()
	logger.Info("ws bridge closed")
}

// watchExpiry closes the bridge once its session ends.
//
// A bridge is pinned to one session, but connections outlive sessions, so
// something has to notice the boundary. pocket-ap closes and lets the client
// reconnect (which reselects a supplier and a fresh session) rather than
// re-signing a live socket.
//
// Each bridge watches its OWN session against a shared atomic height. That is
// deliberately not how SAGE does it: SAGE broadcasts expiry over one shared
// channel, so each bridge consumes events meant for others and drops them —
// with more than one bridge, some never learn their session ended (SAGE
// ws_relayer.go:248-255 concedes this: "Acceptable for v1 with typically 1–few
// bridges per service"). Here there is no shared channel and no registry, so
// that fan-out bug cannot exist: every bridge reads the height itself.
//
// The goroutine exits when the bridge closes for any reason, so it cannot
// outlive its connection.
func (w *WS) watchExpiry(bridge *websockets.Bridge, prepared *relay.Prepared, logger *slog.Logger) {
	// No height source: nothing to watch against. The bridge still works; it just
	// runs until the miner rejects a frame signed against a retired session.
	if w.chainHeight == nil || prepared.Session == nil {
		return
	}
	endHeight := prepared.Session.EndBlockHeight

	ticker := time.NewTicker(w.expiryCheck)
	defer ticker.Stop()

	for {
		select {
		case <-bridge.Done():
			return
		case <-ticker.C:
			height := w.chainHeight()
			if height < endHeight {
				continue
			}
			logger.Info("ws session ended, closing bridge so the client reconnects",
				"session_end_height", endHeight, "current_height", height)

			// Deactivate before Shutdown: it stops new client frames being signed
			// against a session the chain has retired, while supplier frames still
			// in flight drain out to the client.
			prepared.Processor.Deactivate()
			bridge.Shutdown(websockets.ErrBridgeSessionExpired)
			return
		}
	}
}
