package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/pokt-network/pocket-ap/domain"
)

// --- stubs ------------------------------------------------------------------

// stubStreamSender hands back a canned body + headers, as a relay miner would.
type stubStreamSender struct {
	body    string
	header  map[string][]string
	err     error
	calls   int
	failFor map[string]error // by url
}

func (s *stubStreamSender) SendStream(_ context.Context, url string, _ []byte, _ domain.RPCType) (io.ReadCloser, map[string][]string, int, error) {
	s.calls++
	if err, ok := s.failFor[url]; ok {
		return nil, nil, 0, err
	}
	if s.err != nil {
		return nil, nil, 0, s.err
	}
	return io.NopCloser(strings.NewReader(s.body)), s.header, 200, nil
}

// errAfterReader yields some bytes then fails, standing in for a stream that
// dies mid-write (client timeout, connection reset).
type errAfterReader struct {
	data []byte
	pos  int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, errors.New("connection reset")
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
func (r *errAfterReader) Close() error { return nil }

type readerStreamSender struct {
	body   io.ReadCloser
	header map[string][]string
}

func (s *readerStreamSender) SendStream(_ context.Context, _ string, _ []byte, _ domain.RPCType) (io.ReadCloser, map[string][]string, int, error) {
	return s.body, s.header, 200, nil
}

// batchValidator treats each batch as a literal token: "good:X" validates to X,
// anything else fails. Keeps the tests about the streaming, not the crypto.
type batchValidator struct {
	seen []string
}

func (v *batchValidator) ValidateResponse(_ domain.EndpointAddr, respBz []byte) (*domain.RelayResult, error) {
	s := string(respBz)
	v.seen = append(v.seen, s)
	body, ok := strings.CutPrefix(s, "good:")
	if !ok {
		return nil, fmt.Errorf("bad batch %q", s)
	}
	return &domain.RelayResult{StatusCode: 200, Body: []byte(body)}, nil
}

func sseHeader() map[string][]string {
	return map[string][]string{"Content-Type": {"text/event-stream"}}
}

func newStreamRelayer(sender StreamSender, validator Validator, eps []domain.Endpoint) *Relayer {
	return &Relayer{
		Sessions:     stubSessions{session: &domain.Session{ID: "s1", Endpoints: eps}},
		Signer:       &stubSigner{},
		Validator:    validator,
		Selector:     stubSelector{ordered: eps},
		StreamSender: sender,
		MaxAttempts:  3,
	}
}

func collect(t *testing.T, r *Relayer) ([]string, error) {
	t.Helper()
	var got []string
	err := r.RelayStream(context.Background(), "svc", domain.RPCTypeREST, domain.RelayInput{},
		func(res *domain.RelayResult) error {
			got = append(got, string(res.Body))
			return nil
		})
	return got, err
}

// --- tests ------------------------------------------------------------------

// The wire format: the miner joins signed batches with the delimiter, so each
// one must be validated and delivered separately. Handing the whole blob to the
// validator — which is what Relay would do — fails, and that is the bug this
// path exists to avoid.
func TestRelayStream_SplitsAndDeliversEachBatch(t *testing.T) {
	body := "good:a" + StreamDelimiter + "good:b" + StreamDelimiter + "good:c" + StreamDelimiter
	eps := []domain.Endpoint{endpoint("supA", domain.RPCTypeREST, "http://a")}
	r := newStreamRelayer(&stubStreamSender{body: body, header: sseHeader()}, &batchValidator{}, eps)

	got, err := collect(t, r)
	if err != nil {
		t.Fatalf("RelayStream: %v", err)
	}
	want := []string{"a", "b", "c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("batches = %v, want %v", got, want)
	}
}

// A normal (non-streaming) response must behave exactly as Relay does: one
// batch, the whole body. The caller cannot know in advance which it will get.
func TestRelayStream_NonStreamingIsOneBatch(t *testing.T) {
	eps := []domain.Endpoint{endpoint("supA", domain.RPCTypeREST, "http://a")}
	r := newStreamRelayer(
		&stubStreamSender{body: "good:whole", header: map[string][]string{"Content-Type": {"application/json"}}},
		&batchValidator{}, eps)

	got, err := collect(t, r)
	if err != nil {
		t.Fatalf("RelayStream: %v", err)
	}
	if len(got) != 1 || got[0] != "whole" {
		t.Errorf("batches = %v, want one whole body", got)
	}
}

// The delimiter must not be treated as data on a non-streaming response, nor a
// plain body split on a delimiter that never appears.
func TestRelayStream_NonStreamingBodyIsNotSplit(t *testing.T) {
	// A JSON body that happens to contain the delimiter text.
	body := "good:x" + StreamDelimiter + "y"
	eps := []domain.Endpoint{endpoint("supA", domain.RPCTypeREST, "http://a")}
	v := &batchValidator{}
	r := newStreamRelayer(
		&stubStreamSender{body: body, header: map[string][]string{"Content-Type": {"application/json"}}},
		v, eps)

	_, _ = collect(t, r)
	if len(v.seen) != 1 || v.seen[0] != body {
		t.Errorf("validator saw %v, want the whole body once — a non-streaming response must never be split", v.seen)
	}
}

// Detection comes off the miner's Content-Type, which it copies from the
// backend. Parameters must not defeat it.
func TestIsStreamingResponse(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		want        bool
	}{
		{"sse", "text/event-stream", true},
		{"sse with charset", "text/event-stream; charset=utf-8", true},
		{"sse odd case", "Text/Event-Stream", true},
		{"ndjson", "application/x-ndjson", true},
		{"ndjson with charset", "application/x-ndjson; charset=utf-8", true},
		{"json is not a stream", "application/json", false},
		{"plain text is not a stream", "text/plain", false},
		{"empty", "", false},
		{"garbage", "not a media type at all;;;", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := map[string][]string{}
			if tt.contentType != "" {
				h["Content-Type"] = []string{tt.contentType}
			}
			if got := isStreamingResponse(h); got != tt.want {
				t.Errorf("isStreamingResponse(%q) = %v, want %v", tt.contentType, got, tt.want)
			}
		})
	}
}

// Header keys arrive in whatever case the miner used.
func TestIsStreamingResponse_HeaderKeyCaseInsensitive(t *testing.T) {
	for _, key := range []string{"Content-Type", "content-type", "CONTENT-TYPE"} {
		if !isStreamingResponse(map[string][]string{key: {"text/event-stream"}}) {
			t.Errorf("header key %q was not matched", key)
		}
	}
}

// Clean EOF: a trailing batch with no delimiter is a COMPLETE final batch (the
// server finished writing, then closed). Dropping it loses the last token of
// every stream.
func TestRelayStream_TrailingBatchKeptOnCleanEOF(t *testing.T) {
	body := "good:a" + StreamDelimiter + "good:last" // no trailing delimiter
	eps := []domain.Endpoint{endpoint("supA", domain.RPCTypeREST, "http://a")}
	r := newStreamRelayer(&stubStreamSender{body: body, header: sseHeader()}, &batchValidator{}, eps)

	got, err := collect(t, r)
	if err != nil {
		t.Fatalf("RelayStream: %v", err)
	}
	if len(got) != 2 || got[1] != "last" {
		t.Errorf("batches = %v, want the final unterminated batch kept", got)
	}
}

// Reader error: a trailing batch with no delimiter is a TRUNCATED protobuf. It
// must be dropped, not validated — validating it fails, and that failure would
// discard the valid batches already delivered.
func TestRelayStream_TruncatedTrailingBatchDroppedOnReadError(t *testing.T) {
	// "good:a" is delimiter-terminated and whole. "trunc" never terminates.
	body := "good:a" + StreamDelimiter + "trunc"
	eps := []domain.Endpoint{endpoint("supA", domain.RPCTypeREST, "http://a")}
	v := &batchValidator{}
	r := &Relayer{
		Sessions:     stubSessions{session: &domain.Session{ID: "s1", Endpoints: eps}},
		Signer:       &stubSigner{},
		Validator:    v,
		Selector:     stubSelector{ordered: eps},
		StreamSender: &readerStreamSender{body: &errAfterReader{data: []byte(body)}, header: sseHeader()},
		MaxAttempts:  3,
	}

	var got []string
	err := r.RelayStream(context.Background(), "svc", domain.RPCTypeREST, domain.RelayInput{},
		func(res *domain.RelayResult) error {
			got = append(got, string(res.Body))
			return nil
		})

	// The stream broke, so an error is right...
	if err == nil {
		t.Fatal("RelayStream hid a broken stream")
	}
	// ...but the complete batch before it must still have been delivered.
	if len(got) != 1 || got[0] != "a" {
		t.Errorf("delivered %v, want the complete batch kept despite the reset", got)
	}
	// And the truncated tail must never reach the validator.
	for _, seen := range v.seen {
		if seen == "trunc" {
			t.Error("the truncated trailing batch was validated — its failure would discard the good batches before it")
		}
	}
}

// Failover is safe only before anything is delivered.
func TestRelayStream_FailsOverWhenSendFails(t *testing.T) {
	eps := []domain.Endpoint{
		endpoint("supA", domain.RPCTypeREST, "http://a"),
		endpoint("supB", domain.RPCTypeREST, "http://b"),
	}
	sender := &stubStreamSender{
		body:    "good:fromB",
		header:  sseHeader(),
		failFor: map[string]error{"http://a": errors.New("refused")},
	}
	r := newStreamRelayer(sender, &batchValidator{}, eps)

	got, err := collect(t, r)
	if err != nil {
		t.Fatalf("RelayStream: %v", err)
	}
	if len(got) != 1 || got[0] != "fromB" {
		t.Errorf("batches = %v, want the second supplier's", got)
	}
}

// A first batch that fails validation before anything reached the client is
// still failover-safe — same as the non-streaming path.
func TestRelayStream_FailsOverWhenNothingDelivered(t *testing.T) {
	eps := []domain.Endpoint{
		endpoint("supA", domain.RPCTypeREST, "http://a"),
		endpoint("supB", domain.RPCTypeREST, "http://b"),
	}
	// Both endpoints get the same canned body, so make the first batch bad and
	// rely on the per-supplier validator instead.
	v := &supplierAwareValidator{bad: "supA"}
	r := newStreamRelayer(&stubStreamSender{body: "good:x", header: sseHeader()}, v, eps)

	got, err := collect(t, r)
	if err != nil {
		t.Fatalf("RelayStream: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("batches = %v, want one from the failover supplier", got)
	}
}

// supplierAwareValidator rejects everything from one supplier.
type supplierAwareValidator struct{ bad domain.EndpointAddr }

func (v *supplierAwareValidator) ValidateResponse(supplier domain.EndpointAddr, respBz []byte) (*domain.RelayResult, error) {
	if supplier == v.bad {
		return nil, errors.New("bad signature")
	}
	return &domain.RelayResult{StatusCode: 200, Body: bytes.TrimPrefix(respBz, []byte("good:"))}, nil
}

// Once a batch has been handed to the client we are committed: continuing on a
// different supplier would splice two token streams together and the client
// would never know.
func TestRelayStream_NoFailoverAfterDelivery(t *testing.T) {
	eps := []domain.Endpoint{
		endpoint("supA", domain.RPCTypeREST, "http://a"),
		endpoint("supB", domain.RPCTypeREST, "http://b"),
	}
	// First batch good, second bad: delivery happens, then validation fails.
	body := "good:first" + StreamDelimiter + "BAD" + StreamDelimiter
	sender := &stubStreamSender{body: body, header: sseHeader()}
	r := newStreamRelayer(sender, &batchValidator{}, eps)

	got, err := collect(t, r)
	if err == nil {
		t.Fatal("RelayStream hid a mid-stream failure")
	}
	if !strings.Contains(err.Error(), "after delivery") {
		t.Errorf("err = %v, want it to say the failure came after delivery", err)
	}
	if len(got) != 1 || got[0] != "first" {
		t.Errorf("delivered %v, want the good batch kept", got)
	}
	if sender.calls != 1 {
		t.Errorf("sender called %d times, want 1 — must not fail over mid-stream", sender.calls)
	}
}

// A client that hangs up must stop the relay rather than paying a supplier to
// finish a stream nobody is reading.
func TestRelayStream_OnBatchErrorStopsTheRelay(t *testing.T) {
	body := "good:a" + StreamDelimiter + "good:b" + StreamDelimiter
	eps := []domain.Endpoint{endpoint("supA", domain.RPCTypeREST, "http://a")}
	v := &batchValidator{}
	r := newStreamRelayer(&stubStreamSender{body: body, header: sseHeader()}, v, eps)

	calls := 0
	err := r.RelayStream(context.Background(), "svc", domain.RPCTypeREST, domain.RelayInput{},
		func(*domain.RelayResult) error {
			calls++
			return errors.New("client gone")
		})
	if err == nil {
		t.Fatal("RelayStream continued after the client went away")
	}
	if calls != 1 {
		t.Errorf("onBatch called %d times, want 1 — stop on the first failure", calls)
	}
}

func TestRelayStream_RequiresAStreamSender(t *testing.T) {
	eps := []domain.Endpoint{endpoint("supA", domain.RPCTypeREST, "http://a")}
	r := newStreamRelayer(nil, &batchValidator{}, eps)
	r.StreamSender = nil

	if _, err := collect(t, r); err == nil {
		t.Fatal("RelayStream ran with no StreamSender")
	}
}

// A signing failure is ours, not the supplier's: abort, do not fail over.
func TestRelayStream_SignFailureAborts(t *testing.T) {
	eps := []domain.Endpoint{
		endpoint("supA", domain.RPCTypeREST, "http://a"),
		endpoint("supB", domain.RPCTypeREST, "http://b"),
	}
	sender := &stubStreamSender{body: "good:x", header: sseHeader()}
	r := newStreamRelayer(sender, &batchValidator{}, eps)
	r.Signer = &stubSigner{failFor: map[domain.EndpointAddr]error{"supA": errors.New("no ring")}}

	if _, err := collect(t, r); err == nil {
		t.Fatal("RelayStream hid a signing failure")
	}
	if sender.calls != 0 {
		t.Errorf("sender called %d times, want 0 — nothing was signed to send", sender.calls)
	}
}

// The outcome feed must see streaming relays too, or health counters silently
// under-report every inference request.
func TestRelayStream_ReportsOutcomes(t *testing.T) {
	eps := []domain.Endpoint{endpoint("supA", domain.RPCTypeREST, "http://a")}
	sel := &observingStub{stubSelector: stubSelector{ordered: eps}}
	r := newStreamRelayer(&stubStreamSender{body: "good:a" + StreamDelimiter, header: sseHeader()}, &batchValidator{}, eps)
	r.Selector = sel

	if _, err := collect(t, r); err != nil {
		t.Fatalf("RelayStream: %v", err)
	}
	if len(sel.seen) != 1 {
		t.Fatalf("observed %d outcomes, want 1", len(sel.seen))
	}
	if !sel.seen[0].outcome.Success || sel.seen[0].supplier != "supA" {
		t.Errorf("outcome = %+v, want a success for supA", sel.seen[0])
	}
	if sel.seen[0].outcome.ServiceID != "svc" {
		t.Errorf("ServiceID = %q, want svc", sel.seen[0].outcome.ServiceID)
	}
}

// Empty tokens (a trailing delimiter yields one) must not become empty batches.
func TestRelayStream_TrailingDelimiterDoesNotEmitAnEmptyBatch(t *testing.T) {
	body := "good:only" + StreamDelimiter
	eps := []domain.Endpoint{endpoint("supA", domain.RPCTypeREST, "http://a")}
	v := &batchValidator{}
	r := newStreamRelayer(&stubStreamSender{body: body, header: sseHeader()}, v, eps)

	got, err := collect(t, r)
	if err != nil {
		t.Fatalf("RelayStream: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("batches = %v, want exactly one", got)
	}
	if len(v.seen) != 1 {
		t.Errorf("validator saw %v, want one batch — the trailing delimiter is not data", v.seen)
	}
}

// StreamDelimiter is the relay miner's wire constant, not ours to choose
// (pocket-relay-miner relayer/http_stream.go:19). Every other test refers to it
// symbolically, so they would all still pass if someone changed it — and we
// would silently stop parsing real streams. Pin the literal.
func TestStreamDelimiterIsTheWireConstant(t *testing.T) {
	const wire = "||POKT_STREAM||"
	if StreamDelimiter != wire {
		t.Errorf("StreamDelimiter = %q, want %q — this is the relay miner's constant; changing it breaks every real stream",
			StreamDelimiter, wire)
	}
}
