package transport

import (
	"net/http"

	"github.com/pokt-network/pocket-ap/websockets"
)

// OriginPolicy decides which browser origins may reach a listener. Aliased from
// websockets so both front adapters enforce one rule rather than two copies of
// it — the WebSocket and HTTP listeners face the same threat and must not drift.
type OriginPolicy = websockets.OriginPolicy

// Why the plain HTTP listener needs an origin check at all:
//
// CORS stops a malicious page READING a cross-origin response. It does not stop
// the request being SENT. A page can POST to http://localhost:8545 with
// Content-Type: text/plain — a CORS "simple request", so no preflight — and the
// browser will deliver it. The attacker learns nothing from the response, but
// the relay has already happened: signed with the app's key, billed to the
// app's stake. A blind quota drain, not data theft.
//
// (Content-Type: application/json is NOT simple, so it preflights and fails
// against a proxy that sends no CORS headers. The attack works precisely by
// using the wrong content type.)
//
// Same rule as the WebSocket side: no Origin means a native client — node, curl,
// go — and is allowed; any browser origin is rejected unless allowlisted.

// isCORSPreflight reports whether r is a browser preflight rather than a real
// OPTIONS request meant for the backend.
//
// The distinction matters for passthrough: a service may legitimately serve its
// own OPTIONS route, and that must still be relayed. A preflight is identified
// by Access-Control-Request-Method, which only a browser sends.
func isCORSPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
}

// writeCORSHeaders grants an allowlisted origin permission to read the response.
//
// Without these, allowlisting an origin would be useless: the request would be
// relayed and the browser would still refuse to hand the page the response, so
// the config knob would look broken. They use Set, not Add, and are written
// AFTER the backend's headers are copied — a backend that sends its own
// Access-Control-Allow-Origin would otherwise produce two of them, which
// browsers reject outright.
//
// The origin is echoed rather than answered with "*": "*" is incompatible with
// credentialed requests and says more than we mean. No Allow-Credentials is
// sent — pocket-ap has no cookies or auth to carry.
func writeCORSHeaders(w http.ResponseWriter, r *http.Request, origin string) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	// The response depends on the request's Origin, so any cache must key on it.
	h.Set("Vary", "Origin")

	if !isCORSPreflight(r) {
		return
	}
	// Preflight only: describe what the real request may look like.
	h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	// Echo what was asked for: this proxy does not know which headers the
	// backend cares about, and inventing a list would break callers.
	if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
		h.Set("Access-Control-Allow-Headers", reqHeaders)
	}
	h.Set("Access-Control-Max-Age", "600")
}
