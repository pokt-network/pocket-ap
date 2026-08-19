package websockets

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/websocket"
)

// OriginPolicy decides which browser origins may open a bridge.
//
// This is the one place pocket-ap deliberately diverges from SAGE. SAGE's
// upgrader allows every origin (`CheckOrigin: return true`) because SAGE is an
// authenticated gateway — the caller has already proven who they are.
//
// pocket-ap has no such gate: it listens on localhost, unauthenticated, holding
// the app's private key. WebSocket is NOT covered by the same-origin policy the
// way fetch/XHR is — a browser will happily open ws://localhost:8546 from any
// page, and unlike a cross-origin POST there is no preflight and no read
// restriction. So any site the user visits could relay through their staked app
// key and spend their quota. That is cross-site WebSocket hijacking, and copying
// SAGE's upgrader verbatim would ship it.
//
// The default (AllowedOrigins empty) permits requests carrying NO Origin header
// — native clients: Node backends, Go, curl, wscat, which is the target user —
// and rejects every browser origin. Set AllowedOrigins to opt specific ones in.
type OriginPolicy struct {
	// AllowedOrigins is an exact-match allowlist of Origin header values, e.g.
	// "http://localhost:3000". Empty means no browser origin is allowed.
	// The single entry "*" disables the check entirely — this hands any website
	// the user visits the ability to relay with their key. Do not use it to
	// silence an error.
	AllowedOrigins []string
}

// Allows reports whether an Origin header value may talk to this proxy. An empty
// origin means the caller is not a browser.
//
// Exported because the plain HTTP listener needs the identical rule: it is
// reachable cross-origin too, and a second copy of a security decision is a
// second thing to get wrong. transport aliases this type so the import reads
// sensibly there.
func (p OriginPolicy) Allows(origin string) bool {
	// No Origin header: not a browser. Native clients are the point of this
	// proxy, and a non-browser caller could forge any origin anyway, so the
	// header only ever protects against the browser threat model.
	if origin == "" {
		return true
	}
	for _, allowed := range p.AllowedOrigins {
		if allowed == "*" {
			return true
		}
		if strings.EqualFold(allowed, origin) {
			return true
		}
	}
	return false
}

// checkOrigin implements gorilla's CheckOrigin contract.
func (p OriginPolicy) checkOrigin(r *http.Request) bool {
	return p.Allows(r.Header.Get("Origin"))
}

// UpgradeClient upgrades an incoming HTTP request to a WebSocket, enforcing the
// origin policy. Returns ErrBridgeOriginRejected when the origin is not allowed
// and ErrBridgeClientUpgradeFailed (wrapped) on any other handshake failure.
func UpgradeClient(logger *slog.Logger, policy OriginPolicy, r *http.Request, w http.ResponseWriter) (*websocket.Conn, error) {
	// Check before handing off to gorilla: gorilla's own CheckOrigin failure is
	// reported as a generic 403 that we cannot distinguish from other handshake
	// errors, and a rejected origin is worth naming precisely — both in the log
	// and to whoever is trying to work out why their browser cannot connect.
	if !policy.checkOrigin(r) {
		origin := r.Header.Get("Origin")
		logger.Warn("websocket: rejected cross-origin upgrade",
			"origin", origin,
			"hint", "set admin/listener allowed_origins to permit this browser origin",
		)
		http.Error(w, "websocket: origin not allowed", http.StatusForbidden)
		return nil, fmt.Errorf("%w: %q", ErrBridgeOriginRejected, origin)
	}

	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     policy.checkOrigin,
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an HTTP error to w.
		logger.Error("websocket: client upgrade failed", "err", err)
		return nil, fmt.Errorf("%w: %w", ErrBridgeClientUpgradeFailed, err)
	}
	return conn, nil
}

// ConnectEndpoint dials the supplier's WebSocket endpoint. headers carries the
// relay miner's required handshake auth; without it the miner treats the
// connection as anonymous and rejects the upgrade.
func ConnectEndpoint(logger *slog.Logger, rawURL string, headers http.Header) (*websocket.Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		logger.Error("websocket: invalid endpoint URL", "url", rawURL, "err", err)
		return nil, fmt.Errorf("%w: invalid URL %q: %w", ErrBridgeEndpointUnavailable, rawURL, err)
	}

	dialer := websocket.Dialer{ReadBufferSize: 4096, WriteBufferSize: 4096}

	conn, _, err := dialer.Dial(u.String(), headers)
	if err != nil {
		logger.Error("websocket: endpoint connection failed", "url", u.String(), "err", err)
		return nil, fmt.Errorf("%w: dial %q: %w", ErrBridgeEndpointUnavailable, u.String(), err)
	}
	return conn, nil
}
