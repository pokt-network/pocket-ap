package relay

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pokt-network/pocket-ap/domain"
)

// --- stub seams -------------------------------------------------------------

type stubSessions struct {
	session *domain.Session
	err     error
}

func (s stubSessions) Session(context.Context, domain.ServiceID) (*domain.Session, error) {
	return s.session, s.err
}
func (s stubSessions) Start(context.Context) error { return nil }

type stubSelector struct {
	ordered []domain.Endpoint
	err     error
}

func (s stubSelector) Select(context.Context, domain.ServiceID, []domain.Endpoint, domain.RPCType) ([]domain.Endpoint, error) {
	return s.ordered, s.err
}

// recordingSelector captures what the Relayer handed Select.
type recordingSelector struct {
	ordered    []domain.Endpoint
	gotService domain.ServiceID
	gotRPCType domain.RPCType
	gotCount   int
}

func (s *recordingSelector) Select(_ context.Context, serviceID domain.ServiceID, _ []domain.Endpoint, rpcType domain.RPCType) ([]domain.Endpoint, error) {
	s.gotService = serviceID
	s.gotRPCType = rpcType
	s.gotCount++
	return s.ordered, nil
}

// stubSigner fails for any supplier in failFor, and records every call.
type stubSigner struct {
	failFor map[domain.EndpointAddr]error
	calls   []domain.EndpointAddr
}

func (s *stubSigner) SignRelay(_ context.Context, _ *domain.Session, ep domain.Endpoint, _ domain.RPCType, _ domain.RelayInput) ([]byte, error) {
	s.calls = append(s.calls, ep.Supplier)
	if err, ok := s.failFor[ep.Supplier]; ok {
		return nil, err
	}
	return []byte("signed:" + string(ep.Supplier)), nil
}

// stubSender fails for any URL in failFor, and records every URL it was given.
type stubSender struct {
	failFor map[string]error
	calls   []string
}

func (s *stubSender) Send(_ context.Context, url string, _ []byte, _ domain.RPCType) ([]byte, error) {
	s.calls = append(s.calls, url)
	if err, ok := s.failFor[url]; ok {
		return nil, err
	}
	return []byte("resp:" + url), nil
}

// stubValidator fails for any supplier in failFor.
type stubValidator struct {
	failFor map[domain.EndpointAddr]error
	calls   []domain.EndpointAddr
}

func (v *stubValidator) ValidateResponse(supplier domain.EndpointAddr, respBz []byte) (*domain.RelayResult, error) {
	v.calls = append(v.calls, supplier)
	if err, ok := v.failFor[supplier]; ok {
		return nil, err
	}
	return &domain.RelayResult{StatusCode: 200, Body: respBz}, nil
}

// --- helpers ----------------------------------------------------------------

func endpoint(supplier string, rpcType domain.RPCType, url string) domain.Endpoint {
	return domain.Endpoint{
		Supplier: domain.EndpointAddr(supplier),
		URLs:     map[domain.RPCType]string{rpcType: url},
	}
}

// newRelayer wires a Relayer whose session and selection always succeed, so each
// test can focus on the sign/send/validate loop.
func newRelayer(t *testing.T, ordered []domain.Endpoint, signer Signer, sender Sender, validator Validator, maxAttempts int) *Relayer {
	t.Helper()
	return &Relayer{
		Sessions:    stubSessions{session: &domain.Session{ID: "s1", Endpoints: ordered}},
		Signer:      signer,
		Validator:   validator,
		Selector:    stubSelector{ordered: ordered},
		Sender:      sender,
		MaxAttempts: maxAttempts,
	}
}

// --- tests ------------------------------------------------------------------

func TestRelay_FirstEndpointSucceeds(t *testing.T) {
	eps := []domain.Endpoint{endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a")}
	sender := &stubSender{}
	validator := &stubValidator{}
	r := newRelayer(t, eps, &stubSigner{}, sender, validator, 3)

	result, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{})
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if string(result.Body) != "resp:http://a" {
		t.Errorf("body = %q, want %q", result.Body, "resp:http://a")
	}
	if len(sender.calls) != 1 {
		t.Errorf("sender called %d times, want 1 (no needless failover)", len(sender.calls))
	}
}

// Select must be handed the serviceID it is picking for. A quality-aware
// Selector keys its per-service state on this; passing the wrong one (or none)
// would blend unrelated services into a single average, which is the whole
// reason the parameter exists.
func TestRelay_SelectReceivesServiceIDAndRPCType(t *testing.T) {
	eps := []domain.Endpoint{endpoint("supplierA", domain.RPCTypeREST, "http://a")}
	sel := &recordingSelector{ordered: eps}
	r := &Relayer{
		Sessions:    stubSessions{session: &domain.Session{ID: "s1", Endpoints: eps}},
		Signer:      &stubSigner{},
		Validator:   &stubValidator{},
		Selector:    sel,
		Sender:      &stubSender{},
		MaxAttempts: 3,
	}

	if _, err := r.Relay(context.Background(), "my-service", domain.RPCTypeREST, domain.RelayInput{}); err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if sel.gotCount != 1 {
		t.Errorf("Select called %d times, want 1", sel.gotCount)
	}
	if sel.gotService != domain.ServiceID("my-service") {
		t.Errorf("Select got serviceID %q, want my-service", sel.gotService)
	}
	if sel.gotRPCType != domain.RPCTypeREST {
		t.Errorf("Select got rpcType %v, want rest", sel.gotRPCType)
	}
}

func TestRelay_FailsOverOnSendError(t *testing.T) {
	eps := []domain.Endpoint{
		endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a"),
		endpoint("supplierB", domain.RPCTypeJSONRPC, "http://b"),
	}
	sender := &stubSender{failFor: map[string]error{"http://a": errors.New("connection refused")}}
	r := newRelayer(t, eps, &stubSigner{}, sender, &stubValidator{}, 3)

	result, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{})
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if string(result.Body) != "resp:http://b" {
		t.Errorf("body = %q, want the second endpoint's response", result.Body)
	}
	if len(sender.calls) != 2 {
		t.Errorf("sender calls = %v, want both endpoints tried in order", sender.calls)
	}
}

func TestRelay_FailsOverOnValidateError(t *testing.T) {
	eps := []domain.Endpoint{
		endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a"),
		endpoint("supplierB", domain.RPCTypeJSONRPC, "http://b"),
	}
	validator := &stubValidator{
		failFor: map[domain.EndpointAddr]error{"supplierA": errors.New("bad signature")},
	}
	r := newRelayer(t, eps, &stubSigner{}, &stubSender{}, validator, 3)

	result, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{})
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if string(result.Body) != "resp:http://b" {
		t.Errorf("body = %q, want the second endpoint's response", result.Body)
	}
	if len(validator.calls) != 2 {
		t.Errorf("validator calls = %v, want a failover after the bad signature", validator.calls)
	}
}

// A signing failure is our fault, not the endpoint's, so retrying other
// suppliers would just repeat it. relay.go aborts instead of failing over.
func TestRelay_SignErrorAbortsWithoutFailover(t *testing.T) {
	eps := []domain.Endpoint{
		endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a"),
		endpoint("supplierB", domain.RPCTypeJSONRPC, "http://b"),
	}
	signer := &stubSigner{failFor: map[domain.EndpointAddr]error{"supplierA": errors.New("no ring")}}
	sender := &stubSender{}
	r := newRelayer(t, eps, signer, sender, &stubValidator{}, 3)

	_, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{})
	if err == nil {
		t.Fatal("Relay returned nil error on a signing failure")
	}
	if !strings.Contains(err.Error(), "sign failed") {
		t.Errorf("error = %v, want it to name the signing failure", err)
	}
	if len(signer.calls) != 1 {
		t.Errorf("signer called %d times, want 1 — signing failure must not fail over", len(signer.calls))
	}
	if len(sender.calls) != 0 {
		t.Errorf("sender called %d times, want 0 — nothing was signed to send", len(sender.calls))
	}
}

func TestRelay_MaxAttemptsCapsFailover(t *testing.T) {
	eps := []domain.Endpoint{
		endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a"),
		endpoint("supplierB", domain.RPCTypeJSONRPC, "http://b"),
		endpoint("supplierC", domain.RPCTypeJSONRPC, "http://c"),
		endpoint("supplierD", domain.RPCTypeJSONRPC, "http://d"),
	}
	sender := &stubSender{failFor: map[string]error{
		"http://a": errors.New("down"),
		"http://b": errors.New("down"),
		"http://c": errors.New("down"),
		"http://d": errors.New("down"),
	}}
	r := newRelayer(t, eps, &stubSigner{}, sender, &stubValidator{}, 2)

	if _, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{}); err == nil {
		t.Fatal("Relay returned nil error with every endpoint down")
	}
	if len(sender.calls) != 2 {
		t.Errorf("sender called %d times, want 2 (MaxAttempts cap)", len(sender.calls))
	}
}

// MaxAttempts <= 0 means "try them all", and a cap above the endpoint count must
// not run past the end of the slice.
func TestRelay_MaxAttemptsBounds(t *testing.T) {
	tests := []struct {
		name        string
		maxAttempts int
		wantCalls   int
	}{
		{"zero tries every endpoint", 0, 3},
		{"negative tries every endpoint", -1, 3},
		{"cap larger than endpoint count is clamped", 99, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eps := []domain.Endpoint{
				endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a"),
				endpoint("supplierB", domain.RPCTypeJSONRPC, "http://b"),
				endpoint("supplierC", domain.RPCTypeJSONRPC, "http://c"),
			}
			sender := &stubSender{failFor: map[string]error{
				"http://a": errors.New("down"),
				"http://b": errors.New("down"),
				"http://c": errors.New("down"),
			}}
			r := newRelayer(t, eps, &stubSigner{}, sender, &stubValidator{}, tt.maxAttempts)

			if _, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{}); err == nil {
				t.Fatal("Relay returned nil error with every endpoint down")
			}
			if len(sender.calls) != tt.wantCalls {
				t.Errorf("sender called %d times, want %d", len(sender.calls), tt.wantCalls)
			}
		})
	}
}

func TestRelay_AllAttemptsFailedWrapsLastError(t *testing.T) {
	eps := []domain.Endpoint{endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a")}
	sentinel := errors.New("the-underlying-cause")
	sender := &stubSender{failFor: map[string]error{"http://a": sentinel}}
	r := newRelayer(t, eps, &stubSigner{}, sender, &stubValidator{}, 1)

	_, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{})
	if err == nil {
		t.Fatal("Relay returned nil error when the only endpoint failed")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to unwrap to the underlying cause", err)
	}
}

func TestRelay_SessionFetchFailureAborts(t *testing.T) {
	r := &Relayer{
		Sessions:  stubSessions{err: errors.New("node unreachable")},
		Signer:    &stubSigner{},
		Validator: &stubValidator{},
		Selector:  stubSelector{},
		Sender:    &stubSender{},
	}
	_, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{})
	if err == nil {
		t.Fatal("Relay returned nil error when the session fetch failed")
	}
	if !strings.Contains(err.Error(), "session fetch failed") {
		t.Errorf("error = %v, want it to name the session fetch", err)
	}
}

func TestRelay_SelectorErrorAborts(t *testing.T) {
	r := &Relayer{
		Sessions:  stubSessions{session: &domain.Session{ID: "s1"}},
		Signer:    &stubSigner{},
		Validator: &stubValidator{},
		Selector:  stubSelector{err: domain.ErrNoEndpoint},
		Sender:    &stubSender{},
	}
	_, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{})
	if err == nil {
		t.Fatal("Relay returned nil error when selection failed")
	}
	if !errors.Is(err, domain.ErrNoEndpoint) {
		t.Errorf("error = %v, want it to unwrap to ErrNoEndpoint", err)
	}
}

func TestRelay_EmptySelectionReturnsErrNoEndpoint(t *testing.T) {
	r := &Relayer{
		Sessions:  stubSessions{session: &domain.Session{ID: "s1"}},
		Signer:    &stubSigner{},
		Validator: &stubValidator{},
		Selector:  stubSelector{ordered: nil},
		Sender:    &stubSender{},
	}
	_, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{})
	if !errors.Is(err, domain.ErrNoEndpoint) {
		t.Errorf("error = %v, want ErrNoEndpoint", err)
	}
}

// An endpoint that the selector returned but which carries no URL for this RPC
// type must be skipped, not sent to.
func TestRelay_EndpointWithoutURLForTypeIsSkipped(t *testing.T) {
	eps := []domain.Endpoint{
		endpoint("supplierA", domain.RPCTypeREST, "http://a-rest"), // no JSON-RPC URL
		endpoint("supplierB", domain.RPCTypeJSONRPC, "http://b"),
	}
	sender := &stubSender{}
	r := newRelayer(t, eps, &stubSigner{}, sender, &stubValidator{}, 3)

	result, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{})
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if string(result.Body) != "resp:http://b" {
		t.Errorf("body = %q, want the endpoint that supports the type", result.Body)
	}
	if len(sender.calls) != 1 || sender.calls[0] != "http://b" {
		t.Errorf("sender calls = %v, want only the JSON-RPC endpoint", sender.calls)
	}
}
