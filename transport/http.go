package transport

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/pokt-network/pocket-ap/domain"
	"github.com/pokt-network/pocket-ap/internal/safego"
)

// maxRequestBodyBytes caps an inbound relay body. Generous — a signed relay is
// small, but batched JSON-RPC and REST uploads are not — and the point is only
// to have a bound at all rather than read whatever arrives into memory.
const maxRequestBodyBytes = 16 << 20 // 16 MiB

// HTTP is the stateless front adapter. One handler transparently reverse-proxies
// the entire inbound request (method + path + query + headers + body) into a
// relay, so it covers JSON-RPC, REST, CometBFT, and unary gRPC without parsing
// any of them. It does NOT understand eth_call — the payload is opaque.
//
// Config binds one HTTP adapter to one (serviceID, rpcType) per listener addr.
type HTTP struct {
	addr      string
	serviceID domain.ServiceID
	rpcType   domain.RPCType
	relay     RelayFunc
	stream    StreamFunc
	policy    OriginPolicy
	hosts     HostPolicy
	logger    *slog.Logger
	srv       *http.Server
}

// NewHTTP constructs a stateless HTTP adapter. allowedOrigins allowlists browser
// Origins (see cors.go); allowedHosts allowlists Host headers (see hosts.go).
// Both default to the safe thing: no browser origins, and — when bound to
// loopback — no Host but localhost.
func NewHTTP(addr string, serviceID domain.ServiceID, rpcType domain.RPCType, relay RelayFunc, allowedOrigins, allowedHosts []string) *HTTP {
	return &HTTP{
		addr:      addr,
		serviceID: serviceID,
		rpcType:   rpcType,
		relay:     relay,
		policy:    OriginPolicy{AllowedOrigins: allowedOrigins},
		hosts:     HostPolicy{AllowedHosts: allowedHosts, BindAddr: addr},
		logger:    slog.Default().With("transport", "http", "service", serviceID, "rpc_type", rpcType.String(), "addr", addr),
	}
}

func (h *HTTP) RPCType() domain.RPCType { return h.rpcType }

// Serve starts the listener and blocks until ctx is cancelled.
func (h *HTTP) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.handle)

	h.srv = &http.Server{Addr: h.addr, Handler: mux}
	if h.rpcType == domain.RPCTypeGRPC {
		// A gRPC client speaks HTTP/2, and over a plain TCP listener that means
		// h2c (cleartext) — Go's http.Server otherwise only negotiates HTTP/2 via
		// TLS ALPN, so a gRPC client simply cannot connect. Enabling unencrypted
		// HTTP/2 through Protocols is the net/http-native replacement for the
		// deprecated x/net/http2/h2c wrapper; HTTP/1.1 stays on too, so a plain
		// request to the same listener still works.
		//
		// Only for gRPC listeners: every other type is HTTP/1.1 request/response,
		// and h2c there would be surface area for nothing.
		protocols := new(http.Protocols)
		protocols.SetHTTP1(true)
		protocols.SetUnencryptedHTTP2(true)
		h.srv.Protocols = protocols
	}

	h.logger.Info("listening")
	errCh := make(chan error, 1)
	// Call, not Recover: a panic here has to reach errCh, or Serve would block on
	// a channel nothing will ever write to and the listener would read as running.
	go func() { errCh <- safego.Call(h.logger, "transport.http.listen", h.srv.ListenAndServe) }()

	select {
	case <-ctx.Done():
		return h.Close(context.Background())
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// Close gracefully shuts the listener down.
func (h *HTTP) Close(ctx context.Context) error {
	if h.srv == nil {
		return nil
	}
	return h.srv.Shutdown(ctx)
}

// handle captures the inbound request verbatim, relays it, and writes the
// supplier's response straight back to the client.
func (h *HTTP) handle(w http.ResponseWriter, r *http.Request) {
	// Host first: a rebound browser is same-origin to itself, so it carries no
	// Origin on a GET and would sail past the origin check. The name it dialled
	// is the only thing that gives it away.
	if !h.hosts.Allows(r.Host) {
		h.logger.Warn("rejected request for an unrecognised Host",
			"host", r.Host,
			"hint", "this listener is bound to loopback, so it answers to localhost only; set allowed_hosts to permit another name",
		)
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}

	// Gate on origin BEFORE relaying. A rejected request must cost nothing: the
	// whole point is that relaying it would already have spent the app's stake,
	// whether or not the caller can read what comes back.
	origin := r.Header.Get("Origin")
	if !h.policy.Allows(origin) {
		h.logger.Warn("rejected cross-origin request",
			"origin", origin,
			"hint", "set this listener's allowed_origins to permit this browser origin",
		)
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}

	// A preflight is addressed to this proxy — "may this origin talk to you?" —
	// not to the backend, so answer it here. Relaying it would spend a relay on
	// browser bookkeeping and could not work anyway: the backend does not know
	// our allowlist.
	if isCORSPreflight(r) {
		writeCORSHeaders(w, r, origin)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Cap the inbound body. Without this a single oversized POST — a fat-fingered
	// upload, a runaway client — is read entirely into memory before anything
	// else runs, and takes the process with it. MaxBytesReader also stops the
	// read at the limit rather than after.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	_ = r.Body.Close()

	in := domain.RelayInput{
		Method: r.Method,
		Path:   r.URL.RequestURI(), // path + raw query, preserved for REST
		Header: r.Header.Clone(),
		Body:   body,
	}

	// Lift the caller's supplier preference off the request and OUT of it: these
	// headers address this proxy, and everything left in in.Header is signed and
	// replayed to the backend. Taken from the clone, so the client's own request
	// object is untouched.
	suppliers, err := TakeSupplierPolicy(in.Header)
	if err != nil {
		h.logger.Warn("rejected request with a malformed supplier header", "error", err)
		// 400, not 502: nothing was relayed and nothing upstream is wrong. Relaying
		// it anyway would silently ignore an instruction the caller believes it gave.
		http.Error(w, "invalid supplier header: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx := domain.WithSupplierPolicy(r.Context(), suppliers)

	if h.stream != nil {
		h.serveStream(ctx, w, r, origin, in)
		return
	}

	result, err := h.relay(ctx, h.serviceID, h.rpcType, in)
	if err != nil {
		h.logger.Warn("relay failed", "error", err)
		// 502: we are a gateway and the upstream relay did not complete.
		http.Error(w, fmt.Sprintf("relay failed: %v", err), http.StatusBadGateway)
		return
	}

	for k, vs := range result.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	// After the backend's headers, so ours replace any Access-Control-Allow-Origin
	// it sent: two of those and the browser rejects the response outright. Only
	// for browser callers — a native client gets the backend's headers verbatim,
	// which keeps the passthrough honest for everyone who is not a browser.
	if origin != "" {
		writeCORSHeaders(w, r, origin)
	}
	if result.StatusCode == 0 {
		result.StatusCode = http.StatusOK
	}
	w.WriteHeader(result.StatusCode)
	_, _ = w.Write(result.Body)
}

// serveStream relays and writes each validated batch as it arrives.
//
// A response only declares itself streaming once it exists — the backend
// decides, and the relay miner reports it by copying the backend's
// Content-Type onto its own reply. So this path handles both: a normal response
// is exactly one batch and comes out byte-identical to the non-streaming path.
//
// The headers of the FIRST batch become the response's headers. There is nowhere
// else to put them: HTTP sends headers once, before the body, and by the second
// batch they are long gone on the wire.
func (h *HTTP) serveStream(ctx context.Context, w http.ResponseWriter, r *http.Request, origin string, in domain.RelayInput) {
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		// Without flushing, every batch would queue in Go's buffer and land on the
		// client in one lump at EOF — which is exactly what streaming exists to
		// avoid, and would silently undo the miner's 100ms batching.
		h.logger.Warn("response writer cannot flush; a streaming response would be delivered in one lump")
	}

	wroteHeader := false
	// gRPC status arrives folded into the headers and has to leave as trailers.
	// Held until the body is done, since that is what a trailer means.
	var grpcTrailers map[string][]string

	err := h.stream(ctx, h.serviceID, h.rpcType, in, func(result *domain.RelayResult) error {
		if !wroteHeader {
			header := result.Header
			if h.rpcType == domain.RPCTypeGRPC {
				header, grpcTrailers = splitGRPCTrailers(header)
			}
			for k, vs := range header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			if origin != "" {
				writeCORSHeaders(w, r, origin)
			}
			if result.StatusCode == 0 {
				result.StatusCode = http.StatusOK
			}
			w.WriteHeader(result.StatusCode)
			wroteHeader = true
		}

		if _, err := w.Write(result.Body); err != nil {
			// The client went away. Returning the error stops the relay rather than
			// paying a supplier to finish a stream nobody is reading.
			return fmt.Errorf("write to client: %w", err)
		}
		if canFlush {
			flusher.Flush()
		}
		return nil
	})

	if err != nil {
		h.logger.Warn("relay failed", "error", err, "header_sent", wroteHeader)
		if !wroteHeader {
			// Nothing has gone out yet, so an honest HTTP error is still possible.
			http.Error(w, fmt.Sprintf("relay failed: %v", err), http.StatusBadGateway)
			return
		}
		// Committed: the status line is already on the wire and cannot be recalled.
		// Closing mid-body is the only signal left that the stream is incomplete —
		// better than a clean EOF, which would look like a complete answer.
		panic(http.ErrAbortHandler)
	}

	// After the body: a gRPC client reads the status only once the message is in.
	writeGRPCTrailers(w, grpcTrailers)
}
