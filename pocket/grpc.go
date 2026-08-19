package pocket

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/encoding/gzip" // registers the gzip compressor for negotiation
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-ap/domain"
	"github.com/pokt-network/pocket-ap/relay"
)

// relayServiceMethodPath is the relay miner's gRPC entry point. The miner
// registers it through an UnknownServiceHandler rather than a generated service,
// so there is no stub to call and the path is invoked directly.
//
// Wire constant, not ours: pocket-relay-miner relayer/relay_grpc_service.go:31.
const relayServiceMethodPath = "/pocket.service.RelayService/SendRelay"

// rpcTypeMetadataKey tells the miner which backend to route to. It is the gRPC
// analogue of the Rpc-Type header the HTTP sender stamps, and carries the same
// numeric values — the miner reads it in resolveGRPCRelayRPCType
// (relay_grpc_service.go) and it wins over any Content-Type heuristic.
const rpcTypeMetadataKey = "rpc-type"

// gRPC framings, selectable per deployment (config: grpc_mode).
//
// Both exist because neither works everywhere, and which one a supplier needs is
// a property of its front door, not of Pocket. A proxy sitting next to a relay
// miner reaches it over h2c and native gRPC is correct. A proxy reaching one
// through an ingress that terminates HTTP/2 and forwards HTTP/1.1 cannot use
// native gRPC at all — the miner answers "gRPC requires HTTP/2" — but gRPC-Web
// crosses that same ingress untouched, because it carries its trailers as a
// frame inside the body rather than as HTTP trailers.
const (
	// GRPCModeAuto tries native once per supplier host and remembers the answer.
	// The zero value, because it is the only setting correct in both deployments.
	GRPCModeAuto = ""
	// GRPCModeNative forces native gRPC over HTTP/2.
	GRPCModeNative = "native"
	// GRPCModeWeb forces gRPC-Web over HTTP/1.1.
	GRPCModeWeb = "web"
)

// grpcWebContentType is the framing spoken in web mode. "+proto" because the
// message is a protobuf RelayRequest; the base64 "-text" variant exists for
// browsers that cannot carry binary bodies, which is not a problem we have.
const grpcWebContentType = "application/grpc-web+proto"

// grpcFrameHeaderLen is the length-prefixed message header both gRPC and
// gRPC-Web use: one flag byte, then a big-endian uint32 length.
const grpcFrameHeaderLen = 5

// grpcTrailerFlag marks a gRPC-Web trailer frame — the thing that lets gRPC-Web
// survive an HTTP/1.1 hop that would drop real trailers.
const grpcTrailerFlag = 0x80

// Compile-time assertion.
var _ relay.Sender = (*GRPCSender)(nil)

// GRPCSender relays over gRPC to the relay miner's RelayService.
//
// WHY THIS EXISTS, given HTTPSender already POSTs relays: gRPC carries its
// status in HTTP/2 trailers, and Shannon's POKTHTTPResponse has no trailer
// field. The miner solves that with mergeTrailersIntoHeader — folding
// grpc-status into the response headers — but ONLY on its gRPC service path.
// proxy.go, the HTTP path, never folds and never branches on a gRPC backend. So
// a gRPC relay sent over HTTPSender would silently lose grpc-status and the
// client could not interpret the reply.
//
// Scope, per the miner (relay_grpc_service.go:653-657): unary and buffered
// server-streaming. Full-duplex is explicitly not forwarded, so a response is
// always exactly one RelayResponse and there is nothing to stream here.
//
// Two framings are supported; see GRPCModeAuto. Which one a supplier needs
// depends on its front door, so auto mode learns it per host.
type GRPCSender struct {
	mode       string
	httpClient *http.Client
	logger     *slog.Logger

	// conns caches one client per supplier URL. grpc.NewClient is lazy, so this
	// is not about dial cost — it is about not leaking a connection per relay.
	conns sync.Map // url -> *grpc.ClientConn
	mu    sync.Mutex

	// webOnly records hosts that answered a native attempt with "not HTTP/2".
	// Without it, auto mode would re-learn the same fact on every single relay.
	webOnly sync.Map // host -> struct{}
}

// NewGRPCSender builds a sender for gRPC relays in auto mode.
func NewGRPCSender() *GRPCSender { return NewGRPCSenderMode(GRPCModeAuto) }

// NewGRPCSenderMode builds a sender pinned to a framing. An unrecognised mode is
// treated as auto — a config typo must not silently disable gRPC.
//
// The HTTP client carries no Timeout: the relay's ctx already bounds the call,
// and a second whole-request deadline here would only be a way to cut one short.
func NewGRPCSenderMode(mode string) *GRPCSender {
	switch mode {
	case GRPCModeNative, GRPCModeWeb:
	default:
		mode = GRPCModeAuto
	}
	return &GRPCSender{
		mode:       mode,
		httpClient: &http.Client{},
		logger:     slog.Default().With("component", "grpc_sender"),
	}
}

// Close shuts every cached connection down.
func (s *GRPCSender) Close() error {
	var firstErr error
	s.conns.Range(func(_, v any) bool {
		if conn, ok := v.(*grpc.ClientConn); ok {
			if err := conn.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return true
	})
	return firstErr
}

// Send relays one signed RelayRequest and returns the marshaled RelayResponse,
// which Validator.ValidateResponse then verifies.
//
// Bytes go in and bytes come out, untouched. That is not just seam tidiness: the
// supplier's signature covers the exact bytes it put on the wire, so decoding
// the reply and re-encoding it before validating would check that signature
// against something the supplier never produced. Protobuf round trips are not
// guaranteed byte-identical, so the failure would be intermittent and would look
// like a lying supplier.
func (s *GRPCSender) Send(ctx context.Context, endpointURL string, relayReqBz []byte, rpcType domain.RPCType) ([]byte, error) {
	host, _, err := grpcTarget(endpointURL)
	if err != nil {
		return nil, err
	}

	switch s.mode {
	case GRPCModeWeb:
		return s.sendWeb(ctx, endpointURL, relayReqBz, rpcType)
	case GRPCModeNative:
		return s.sendNative(ctx, endpointURL, relayReqBz, rpcType)
	}

	// Auto: straight to web for a host already known not to speak HTTP/2.
	if _, known := s.webOnly.Load(host); known {
		return s.sendWeb(ctx, endpointURL, relayReqBz, rpcType)
	}

	resp, err := s.sendNative(ctx, endpointURL, relayReqBz, rpcType)
	if err == nil || !isNotHTTP2(err) {
		return resp, err
	}

	// Only this one failure means "wrong framing for this front door". Anything
	// else is a real error and must not be retried as a different protocol — a
	// broken supplier silently re-attempted in another framing is a bug that
	// hides itself.
	if _, already := s.webOnly.LoadOrStore(host, struct{}{}); !already {
		// Once per host: which framing a supplier resolved to is the first thing
		// to check when gRPC behaves differently between deployments, and it is
		// invisible otherwise.
		s.logger.Info("supplier does not speak HTTP/2, using gRPC-Web for this host",
			"host", host, "error", err)
	}
	return s.sendWeb(ctx, endpointURL, relayReqBz, rpcType)
}

// sendNative performs the relay as a real gRPC call over HTTP/2.
func (s *GRPCSender) sendNative(ctx context.Context, endpointURL string, relayReqBz []byte, rpcType domain.RPCType) ([]byte, error) {
	conn, err := s.connFor(endpointURL)
	if err != nil {
		return nil, err
	}

	// Tell the miner which backend to route to, the same contract the HTTP
	// sender's Rpc-Type header carries — same mapping, same source of truth.
	if v := rpcTypeHeader(rpcType); v != "" {
		ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs(rpcTypeMetadataKey, v))
	}

	// The miner serves this through an UnknownServiceHandler, so there is no
	// generated stub: open the stream against the method path directly. It is
	// declared bidirectional to match the server's handler, even though this
	// exchange is one message each way.
	//
	// ForceCodec is what keeps the wire bytes verbatim in both directions; the
	// default proto codec would decode the response and cost us the exact bytes
	// the supplier signed.
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{
		StreamName:    "SendRelay",
		ServerStreams: true,
		ClientStreams: true,
	}, relayServiceMethodPath, grpc.ForceCodec(rawCodec{}))
	if err != nil {
		return nil, fmt.Errorf("grpc sender: open stream to %s: %w", endpointURL, err)
	}

	if err := stream.SendMsg(&relayReqBz); err != nil {
		return nil, fmt.Errorf("grpc sender: send to %s: %w", endpointURL, err)
	}
	if err := stream.CloseSend(); err != nil {
		return nil, fmt.Errorf("grpc sender: close send to %s: %w", endpointURL, err)
	}

	var respBz []byte
	if err := stream.RecvMsg(&respBz); err != nil {
		return nil, fmt.Errorf("grpc sender: receive from %s: %w", endpointURL, err)
	}
	return respBz, nil
}

// sendWeb performs the relay as a gRPC-Web call over ordinary HTTP/1.1.
func (s *GRPCSender) sendWeb(ctx context.Context, endpointURL string, relayReqBz []byte, rpcType domain.RPCType) ([]byte, error) {
	target, err := grpcWebURL(endpointURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target,
		bytes.NewReader(encodeGRPCFrame(0, relayReqBz)))
	if err != nil {
		return nil, fmt.Errorf("grpc-web sender: build request: %w", err)
	}
	req.Header.Set("Content-Type", grpcWebContentType)
	if v := rpcTypeHeader(rpcType); v != "" {
		req.Header.Set(rpcTypeMetadataKey, v)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grpc-web sender: %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("grpc-web sender: read response from %s: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grpc-web sender: %s returned HTTP %d: %s",
			target, resp.StatusCode, truncateForError(string(body), 200))
	}

	message, trailers, err := decodeGRPCWebResponse(body)
	if err != nil {
		return nil, fmt.Errorf("grpc-web sender: %s: %w", target, err)
	}

	// The status arrives either as HTTP headers (a trailers-only reply) or as the
	// in-body trailer frame. Both are legal; the frame wins because a reply
	// carrying one is the more specific answer.
	if code, msg := grpcStatusFrom(trailers, resp.Header); code != 0 {
		return nil, fmt.Errorf("grpc-web sender: %s returned grpc-status %d: %s", target, code, msg)
	}
	if message == nil {
		return nil, fmt.Errorf("grpc-web sender: %s returned no message frame", target)
	}
	return message, nil
}

// connFor returns the cached client for a supplier URL, dialing on first use.
func (s *GRPCSender) connFor(endpointURL string) (*grpc.ClientConn, error) {
	if c, ok := s.conns.Load(endpointURL); ok {
		return c.(*grpc.ClientConn), nil
	}

	// Serialize dialing so a burst of first relays to one supplier does not build
	// a connection each and leak all but one.
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.conns.Load(endpointURL); ok {
		return c.(*grpc.ClientConn), nil
	}

	target, useTLS, err := grpcTarget(endpointURL)
	if err != nil {
		return nil, err
	}
	conn, err := connectGRPC(target, !useTLS)
	if err != nil {
		return nil, fmt.Errorf("grpc sender: dial %s: %w", target, err)
	}
	s.conns.Store(endpointURL, conn)
	return conn, nil
}

// grpcTarget turns a supplier's advertised URL into a gRPC dial target, and says
// whether it wants TLS.
//
// Suppliers advertise https:// / http:// URLs, which gRPC cannot dial — it wants
// host:port. The scheme is the only signal for TLS, so it is read rather than
// discarded: the miner's own client strips the scheme and then always dials
// insecure (cmd/relay/grpc.go), which works against a local test miner and would
// fail against any real https endpoint.
func grpcTarget(endpointURL string) (target string, useTLS bool, err error) {
	// Checked before parsing, because url.Parse REJECTS a bare "host:port" — it
	// reads "127.0.0.1" as a scheme and fails on the colon. A bare address is
	// what a local or in-cluster endpoint looks like, and it implies no TLS.
	if !strings.Contains(endpointURL, "://") {
		if endpointURL == "" {
			return "", false, fmt.Errorf("grpc sender: empty endpoint url")
		}
		return endpointURL, false, nil
	}

	u, err := url.Parse(endpointURL)
	if err != nil {
		return "", false, fmt.Errorf("grpc sender: parse url %q: %w", endpointURL, err)
	}

	switch strings.ToLower(u.Scheme) {
	case "https", "grpcs":
		useTLS = true
	case "http", "grpc":
		useTLS = false
	default:
		return "", false, fmt.Errorf("grpc sender: url %q has scheme %q, want http(s) or grpc(s)", endpointURL, u.Scheme)
	}

	host := u.Host
	if host == "" {
		return "", false, fmt.Errorf("grpc sender: url %q has no host", endpointURL)
	}
	// Default the port from the scheme, since gRPC requires an explicit one.
	if u.Port() == "" {
		if useTLS {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	return host, useTLS, nil
}

// rawCodec hands gRPC the message bytes verbatim in both directions, which is
// the whole point: see Send.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("rawCodec: expected *[]byte, got %T", v)
	}
	return *b, nil
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("rawCodec: expected *[]byte, got %T", v)
	}
	// Copy: the buffer belongs to the transport and is reused after this call.
	*b = append([]byte(nil), data...)
	return nil
}

// Name must not collide with a registered codec; grpc-go looks codecs up by it.
func (rawCodec) Name() string { return "pocket-ap-raw-bytes" }

// grpcWebURL turns a supplier's staked URL into the HTTP endpoint of the miner's
// relay service. Unlike grpcTarget this keeps the scheme, because gRPC-Web is
// ordinary HTTP and net/http needs one.
func grpcWebURL(endpointURL string) (string, error) {
	if endpointURL == "" {
		return "", fmt.Errorf("grpc-web sender: empty endpoint url")
	}
	// A bare "host:port" is what an in-cluster endpoint looks like, and it
	// implies no TLS. url.Parse would read the host as a scheme and reject it.
	if !strings.Contains(endpointURL, "://") {
		return "http://" + endpointURL + relayServiceMethodPath, nil
	}

	u, err := url.Parse(endpointURL)
	if err != nil {
		return "", fmt.Errorf("grpc-web sender: parse url %q: %w", endpointURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "grpcs":
		u.Scheme = "https"
	case "http", "grpc":
		u.Scheme = "http"
	default:
		return "", fmt.Errorf("grpc-web sender: url %q has scheme %q, want http(s) or grpc(s)", endpointURL, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("grpc-web sender: url %q has no host", endpointURL)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + relayServiceMethodPath
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// encodeGRPCFrame wraps a message in the length-prefixed framing shared by gRPC
// and gRPC-Web.
func encodeGRPCFrame(flag byte, msg []byte) []byte {
	out := make([]byte, grpcFrameHeaderLen+len(msg))
	out[0] = flag
	binary.BigEndian.PutUint32(out[1:grpcFrameHeaderLen], uint32(len(msg)))
	copy(out[grpcFrameHeaderLen:], msg)
	return out
}

// decodeGRPCWebResponse splits a gRPC-Web body into the response message and the
// trailer key/values. A unary call carries one data frame; a trailers-only reply
// carries none, which is not an error — the status says what happened.
func decodeGRPCWebResponse(body []byte) (message []byte, trailers map[string]string, err error) {
	for off := 0; off < len(body); {
		if off+grpcFrameHeaderLen > len(body) {
			return nil, nil, fmt.Errorf("truncated frame header at byte %d", off)
		}
		flag := body[off]
		size := int(binary.BigEndian.Uint32(body[off+1 : off+grpcFrameHeaderLen]))
		start := off + grpcFrameHeaderLen
		if size < 0 || start+size > len(body) {
			return nil, nil, fmt.Errorf("frame at byte %d claims %d bytes, only %d remain", off, size, len(body)-start)
		}

		if flag&grpcTrailerFlag != 0 {
			trailers = parseGRPCTrailers(body[start : start+size])
		} else if message == nil {
			message = body[start : start+size]
		}
		off = start + size
	}
	return message, trailers, nil
}

// parseGRPCTrailers reads the HTTP-header-shaped block gRPC-Web puts in its
// final frame ("grpc-status: 0\r\ngrpc-message: ...\r\n").
func parseGRPCTrailers(payload []byte) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(string(payload), "\r\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	return out
}

// grpcStatusFrom resolves the call outcome, preferring the in-body trailers and
// falling back to HTTP headers. Neither present is a success: gRPC treats an
// absent grpc-status as OK.
func grpcStatusFrom(trailers map[string]string, header http.Header) (int, string) {
	if raw, ok := trailers["grpc-status"]; ok {
		code, _ := strconv.Atoi(raw)
		return code, trailers["grpc-message"]
	}
	if raw := header.Get("Grpc-Status"); raw != "" {
		code, _ := strconv.Atoi(raw)
		return code, header.Get("Grpc-Message")
	}
	return 0, ""
}

// isNotHTTP2 reports whether an error means "this front door does not speak
// HTTP/2" — the one failure worth retrying in a different framing.
//
// Every marker here must be DEFINITIVE, because the answer is memoized for the
// process lifetime: a false positive pins a healthy supplier to gRPC-Web
// forever, on one bad observation. The relay miner replies 505 "gRPC requires
// HTTP/2" when a native request reaches it over HTTP/1.1 (what an ingress
// terminating HTTP/2 produces), and a plain HTTP/1.1 server fails earlier, in
// the preface, with a frame header grpc-go explicitly identifies as HTTP/1.1.
//
// ⚠️ A bare "error reading server preface" is deliberately NOT matched. It is
// ambiguous — a transient reset or an overloaded peer produces it too — and it
// is redundant, because a genuine HTTP/1.1 front door always carries the
// "looked like an HTTP/1.1 header" clause alongside it. Matching it cost us an
// intermittent test failure under load, which is the harmless version of pinning
// a real supplier to the wrong framing off a blip.
func isNotHTTP2(err error) bool {
	if err == nil {
		return false
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.Unimplemented {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "505") ||
		strings.Contains(msg, "HTTP Version Not Supported") ||
		strings.Contains(msg, "gRPC requires HTTP/2") ||
		strings.Contains(msg, "looked like an HTTP/1.1 header")
}

func truncateForError(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Compile-time assertions: MultiSender fills both sender seams.
var (
	_ relay.Sender       = (*MultiSender)(nil)
	_ relay.StreamSender = (*MultiSender)(nil)
)

// MultiSender routes a relay to the transport its RPC type needs: gRPC relays go
// to the miner over gRPC, everything else over HTTP POST.
//
// The Sender seam takes rpcType precisely so a sender can do this, which is why
// no seam changed to add gRPC.
type MultiSender struct {
	HTTP *HTTPSender
	GRPC *GRPCSender
}

// NewMultiSender builds a sender covering every RPC type.
func NewMultiSender(http *HTTPSender, grpc *GRPCSender) *MultiSender {
	return &MultiSender{HTTP: http, GRPC: grpc}
}

// Send routes by RPC type.
func (m *MultiSender) Send(ctx context.Context, url string, relayReqBz []byte, rpcType domain.RPCType) ([]byte, error) {
	if rpcType == domain.RPCTypeGRPC && m.GRPC != nil {
		return m.GRPC.Send(ctx, url, relayReqBz, rpcType)
	}
	return m.HTTP.Send(ctx, url, relayReqBz, rpcType)
}

// SendStream routes by RPC type too.
//
// A gRPC relay has nothing to stream: the miner buffers even server-streaming
// into one RelayResponse and does not forward full-duplex at all. So the reply
// is handed back as a single complete body with no streaming Content-Type, which
// RelayStream then treats as exactly one batch — the same path a normal HTTP
// response takes.
func (m *MultiSender) SendStream(ctx context.Context, url string, relayReqBz []byte, rpcType domain.RPCType) (io.ReadCloser, map[string][]string, int, error) {
	if rpcType == domain.RPCTypeGRPC && m.GRPC != nil {
		respBz, err := m.GRPC.Send(ctx, url, relayReqBz, rpcType)
		if err != nil {
			return nil, nil, 0, err
		}
		return io.NopCloser(bytes.NewReader(respBz)), nil, 200, nil
	}
	return m.HTTP.SendStream(ctx, url, relayReqBz, rpcType)
}

// Close releases the gRPC connections.
func (m *MultiSender) Close() error {
	if m.GRPC != nil {
		return m.GRPC.Close()
	}
	return nil
}
