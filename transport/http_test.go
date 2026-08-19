package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pokt-network/pocket-ap/domain"
)

// countingRelay records how many relays were actually spent — the only number
// that matters here, because a relay is billed to the app's stake whether or not
// the caller can read the response.
type countingRelay struct {
	calls  atomic.Int64
	result *domain.RelayResult
}

func (c *countingRelay) fn(_ context.Context, _ domain.ServiceID, _ domain.RPCType, _ domain.RelayInput) (*domain.RelayResult, error) {
	c.calls.Add(1)
	if c.result != nil {
		return c.result, nil
	}
	return &domain.RelayResult{StatusCode: 200, Body: []byte(`{"result":"0x0"}`)}, nil
}

func newHTTPForTest(relay RelayFunc, allowedOrigins []string) *HTTP {
	return NewHTTP("127.0.0.1:0", "pnf-anvil", domain.RPCTypeJSONRPC, relay, allowedOrigins, []string{"*"})
}

// THE attack, and the reason this check exists. A page can POST with
// Content-Type: text/plain — a CORS "simple request", so no preflight — and the
// browser sends it. CORS then stops the page reading the response, which is
// cold comfort: the relay is already signed with the app's key and billed to its
// stake. A blind quota drain.
//
// The request must therefore cost ZERO relays, not merely return an unreadable
// answer.
func TestHTTP_CrossOriginSimpleRequestCostsNoRelay(t *testing.T) {
	relay := &countingRelay{}
	h := newHTTPForTest(relay.fn, nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","method":"eth_blockNumber"}`))
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Content-Type", "text/plain") // the bypass: not preflighted
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", rec.Code)
	}
	if n := relay.calls.Load(); n != 0 {
		t.Errorf("%d relays spent for a cross-origin page — the request must cost nothing", n)
	}
}

// Native clients send no Origin and must be untouched: they are the target user.
func TestHTTP_NativeClientWithNoOriginIsRelayed(t *testing.T) {
	relay := &countingRelay{}
	h := newHTTPForTest(relay.fn, nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
	if n := relay.calls.Load(); n != 1 {
		t.Errorf("relays = %d, want 1", n)
	}
	// Nothing CORS-related should be invented for a caller that never asked.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q on a native client, want none", got)
	}
}

func TestHTTP_AllowlistedOriginIsRelayedAndReadable(t *testing.T) {
	relay := &countingRelay{}
	h := newHTTPForTest(relay.fn, []string{"http://localhost:3000"})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
	if n := relay.calls.Load(); n != 1 {
		t.Errorf("relays = %d, want 1", n)
	}
	// Without this the allowlist would be a lie: relayed, but unreadable.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the origin echoed back", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin — the response depends on it", got)
	}
}

// A preflight is addressed to this proxy, not the backend, so it must be
// answered locally — and must not spend a relay on browser bookkeeping.
func TestHTTP_PreflightIsAnsweredLocallyAndCostsNoRelay(t *testing.T) {
	relay := &countingRelay{}
	h := newHTTPForTest(relay.fn, []string{"http://localhost:3000"})

	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Content-Type, X-Custom")
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204", rec.Code)
	}
	if n := relay.calls.Load(); n != 0 {
		t.Errorf("%d relays spent on a preflight, want 0", n)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("Access-Control-Allow-Origin = %q", got)
	}
	// Echoed, not invented: this proxy cannot know which headers the backend wants.
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "Content-Type, X-Custom" {
		t.Errorf("Access-Control-Allow-Headers = %q, want the requested headers echoed", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Errorf("Access-Control-Allow-Methods = %q, want POST included", got)
	}
}

func TestHTTP_PreflightFromRejectedOriginIsForbidden(t *testing.T) {
	relay := &countingRelay{}
	h := newHTTPForTest(relay.fn, nil)

	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q for a rejected origin, want none", got)
	}
}

// Passthrough must survive the CORS work: a service may serve its own OPTIONS
// route, and only a real preflight (which carries Access-Control-Request-Method)
// belongs to us.
func TestHTTP_PlainOPTIONSIsRelayedNotIntercepted(t *testing.T) {
	relay := &countingRelay{}
	h := newHTTPForTest(relay.fn, nil)

	req := httptest.NewRequest(http.MethodOptions, "/v1/status", nil)
	// No Access-Control-Request-Method: this is not a preflight.
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	if n := relay.calls.Load(); n != 1 {
		t.Errorf("relays = %d, want 1 — a non-preflight OPTIONS belongs to the backend", n)
	}
	if rec.Code == http.StatusNoContent {
		t.Error("a plain OPTIONS was answered locally as if it were a preflight")
	}
}

// A backend that sends its own Access-Control-Allow-Origin must not produce two
// of them: browsers reject a response with duplicates outright.
func TestHTTP_BackendCORSHeaderIsReplacedNotDuplicated(t *testing.T) {
	relay := &countingRelay{result: &domain.RelayResult{
		StatusCode: 200,
		Header:     map[string][]string{"Access-Control-Allow-Origin": {"https://backend-says-this.example"}},
		Body:       []byte(`{}`),
	}}
	h := newHTTPForTest(relay.fn, []string{"http://localhost:3000"})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	got := rec.Header().Values("Access-Control-Allow-Origin")
	if len(got) != 1 {
		t.Fatalf("Access-Control-Allow-Origin appears %d times (%v), want exactly 1 — duplicates are rejected by browsers", len(got), got)
	}
	if got[0] != "http://localhost:3000" {
		t.Errorf("Access-Control-Allow-Origin = %q, want ours to win over the backend's", got[0])
	}
}

// The backend's headers must still reach a native client verbatim: the CORS work
// applies only to browser callers, so passthrough stays honest for everyone else.
func TestHTTP_BackendHeadersPassThroughForNativeClients(t *testing.T) {
	relay := &countingRelay{result: &domain.RelayResult{
		StatusCode: 200,
		Header: map[string][]string{
			"Access-Control-Allow-Origin": {"https://backend-says-this.example"},
			"X-Backend-Header":            {"kept"},
		},
		Body: []byte(`{}`),
	}}
	h := newHTTPForTest(relay.fn, nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	if got := rec.Header().Get("X-Backend-Header"); got != "kept" {
		t.Errorf("X-Backend-Header = %q, want it passed through", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://backend-says-this.example" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the backend's own value untouched for a native client", got)
	}
}

// The zero value is what a config with no allowed_origins produces, and it must
// be the safe one.
func TestHTTP_ZeroConfigRejectsBrowsers(t *testing.T) {
	relay := &countingRelay{}
	h := NewHTTP("127.0.0.1:0", "svc", domain.RPCTypeJSONRPC, relay.fn, nil, []string{"*"})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", rec.Code)
	}
	if relay.calls.Load() != 0 {
		t.Error("a relay was spent under the default config")
	}
}

func TestHTTP_WildcardOptsEverythingIn(t *testing.T) {
	relay := &countingRelay{}
	h := newHTTPForTest(relay.fn, []string{"*"})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://anything.example.com")
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
	// Echoed, never the literal "*": that is incompatible with credentialed
	// requests and claims more than we mean.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://anything.example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the origin echoed", got)
	}
}

func TestIsCORSPreflight(t *testing.T) {
	tests := []struct {
		name   string
		method string
		acrm   string
		want   bool
	}{
		{"real preflight", http.MethodOptions, "POST", true},
		{"OPTIONS without ACRM is the backend's", http.MethodOptions, "", false},
		{"POST is never a preflight", http.MethodPost, "POST", false},
		{"GET is never a preflight", http.MethodGet, "GET", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, "/", nil)
			if tt.acrm != "" {
				r.Header.Set("Access-Control-Request-Method", tt.acrm)
			}
			if got := isCORSPreflight(r); got != tt.want {
				t.Errorf("isCORSPreflight = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- streaming (SSE / NDJSON) ------------------------------------------------

// streamRelay delivers a fixed set of batches, standing in for relay.RelayStream.
type streamRelay struct {
	batches []string
	status  int
	header  map[string][]string
	err     error
	errAt   int // fail after this many batches (0 = never)
}

func (s *streamRelay) fn(_ context.Context, _ domain.ServiceID, _ domain.RPCType, _ domain.RelayInput, onBatch func(*domain.RelayResult) error) error {
	if s.err != nil && s.errAt == 0 {
		return s.err
	}
	for i, b := range s.batches {
		status := s.status
		if status == 0 {
			status = 200
		}
		res := &domain.RelayResult{StatusCode: status, Header: s.header, Body: []byte(b)}
		if err := onBatch(res); err != nil {
			return err
		}
		if s.err != nil && i+1 >= s.errAt {
			return s.err
		}
	}
	return nil
}

// flushRecorder counts flushes, so a test can prove batches leave as they arrive
// rather than piling up until EOF.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushRecorder) Flush() { f.flushes++; f.ResponseRecorder.Flush() }

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func newStreamHTTP(stream StreamFunc) *HTTP {
	h := NewHTTP("127.0.0.1:0", "pnf-anvil", domain.RPCTypeREST, nil, nil, []string{"*"})
	h.stream = stream
	return h
}

// Batches must reach the client concatenated, in order — the client sees one
// continuous body, not the fact that it arrived signed in pieces.
func TestHTTP_StreamConcatenatesBatchesInOrder(t *testing.T) {
	h := newStreamHTTP((&streamRelay{batches: []string{"tok1 ", "tok2 ", "tok3"}}).fn)

	rec := newFlushRecorder()
	h.handle(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`)))

	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "tok1 tok2 tok3" {
		t.Errorf("body = %q, want the batches concatenated in order", got)
	}
}

// The whole point of streaming: each batch is flushed as it arrives. Without
// this they queue in Go's buffer and land in one lump at EOF, silently undoing
// the miner's 100ms batching.
func TestHTTP_StreamFlushesEachBatch(t *testing.T) {
	h := newStreamHTTP((&streamRelay{batches: []string{"a", "b", "c"}}).fn)

	rec := newFlushRecorder()
	h.handle(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`)))

	if rec.flushes != 3 {
		t.Errorf("flushes = %d, want 3 — one per batch, or tokens arrive in one lump", rec.flushes)
	}
}

// HTTP sends headers once, before the body. The first batch's are the only ones
// that can be used; later batches' are already unreachable on the wire.
func TestHTTP_StreamUsesFirstBatchHeadersOnly(t *testing.T) {
	h := newStreamHTTP((&streamRelay{
		batches: []string{"a", "b"},
		status:  201,
		header:  map[string][]string{"Content-Type": {"text/event-stream"}},
	}).fn)

	rec := newFlushRecorder()
	h.handle(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`)))

	if rec.Code != 201 {
		t.Errorf("code = %d, want 201 from the first batch", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want the first batch's", got)
	}
}

// A failure before anything is written can still be an honest HTTP error.
func TestHTTP_StreamFailureBeforeAnyBatchIs502(t *testing.T) {
	h := newStreamHTTP((&streamRelay{err: errors.New("no session")}).fn)

	rec := newFlushRecorder()
	h.handle(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`)))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d, want 502", rec.Code)
	}
}

// Once the status line is on the wire it cannot be recalled. Aborting the
// handler is the only way left to signal an incomplete stream — a clean EOF
// would look like a complete answer, and the client would believe a truncated
// LLM reply was the whole thing.
func TestHTTP_StreamFailureAfterDeliveryAbortsTheConnection(t *testing.T) {
	h := newStreamHTTP((&streamRelay{
		batches: []string{"partial"},
		err:     errors.New("supplier died"),
		errAt:   1,
	}).fn)

	rec := newFlushRecorder()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a mid-stream failure ended with a clean EOF — the client cannot tell the answer was truncated")
		}
		if r != http.ErrAbortHandler {
			t.Errorf("panic = %v, want http.ErrAbortHandler", r)
		}
	}()

	h.handle(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`)))
}

// A browser streaming from an allowlisted origin still needs CORS on the first
// batch, or it cannot read the stream at all.
func TestHTTP_StreamWritesCORSOnFirstBatch(t *testing.T) {
	h := NewHTTP("127.0.0.1:0", "pnf-anvil", domain.RPCTypeREST, nil, []string{"http://localhost:3000"}, []string{"*"})
	h.stream = (&streamRelay{batches: []string{"a", "b"}}).fn

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Origin", "http://localhost:3000")
	rec := newFlushRecorder()

	h.handle(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the origin — a browser cannot read the stream without it", got)
	}
}

// The security gates must run before the stream path too, or streaming would be
// a way around them.
func TestHTTP_StreamStillGatesOriginAndHost(t *testing.T) {
	t.Run("cross-origin rejected", func(t *testing.T) {
		// Assert the relay was never entered, not that the body lacks the batch:
		// http.Error's own "origin not allowed" text would satisfy a naive
		// substring check, and a relay that ran has already been paid for.
		var called atomic.Bool
		h := newStreamHTTP(func(_ context.Context, _ domain.ServiceID, _ domain.RPCType, _ domain.RelayInput, onBatch func(*domain.RelayResult) error) error {
			called.Store(true)
			return onBatch(&domain.RelayResult{StatusCode: 200, Body: []byte("leaked")})
		})

		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
		req.Header.Set("Origin", "https://evil.example.com")
		h.policy = OriginPolicy{} // default: no browser origins
		rec := newFlushRecorder()

		h.handle(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("code = %d, want 403", rec.Code)
		}
		if called.Load() {
			t.Error("the stream relay ran for a rejected origin — the request must cost nothing")
		}
	})

	t.Run("bad host rejected", func(t *testing.T) {
		var called atomic.Bool
		h := NewHTTP("127.0.0.1:8545", "svc", domain.RPCTypeREST, nil, nil, nil)
		h.stream = func(_ context.Context, _ domain.ServiceID, _ domain.RPCType, _ domain.RelayInput, _ func(*domain.RelayResult) error) error {
			called.Store(true)
			return nil
		}

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "evil.example.com:8545"
		rec := newFlushRecorder()

		h.handle(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("code = %d, want 403", rec.Code)
		}
		if called.Load() {
			t.Error("the stream relay ran for a rebound Host")
		}
	})
}

// A preflight must never reach the stream path.
func TestHTTP_StreamPreflightAnsweredLocally(t *testing.T) {
	relay := &streamRelay{batches: []string{"a"}}
	h := NewHTTP("127.0.0.1:0", "svc", domain.RPCTypeREST, nil, []string{"http://localhost:3000"}, []string{"*"})
	h.stream = relay.fn

	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := newFlushRecorder()

	h.handle(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty — a preflight must not be relayed", rec.Body.String())
	}
}
