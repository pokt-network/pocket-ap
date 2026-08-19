package relay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pokt-network/pocket-ap/domain"
)

// observingStub is a Selector that also implements Observer, i.e. what a QoS
// selector looks like from the Relayer's side.
type observingStub struct {
	stubSelector
	seen []observed
}

type observed struct {
	supplier domain.EndpointAddr
	outcome  Outcome
}

func (o *observingStub) Observe(supplier domain.EndpointAddr, outcome Outcome) {
	o.seen = append(o.seen, observed{supplier: supplier, outcome: outcome})
}

// newObservedRelayer wires a Relayer whose Selector observes, so tests can read
// the feed back.
func newObservedRelayer(eps []domain.Endpoint, signer Signer, sender Sender, validator Validator) (*Relayer, *observingStub) {
	sel := &observingStub{stubSelector: stubSelector{ordered: eps}}
	return &Relayer{
		Sessions:    stubSessions{session: &domain.Session{ID: "s1", Endpoints: eps}},
		Signer:      signer,
		Validator:   validator,
		Selector:    sel,
		Sender:      sender,
		MaxAttempts: 3,
	}, sel
}

// The whole point of the optional-interface design: selector.Random and any
// other plain Selector must keep working, untouched and un-called.
func TestRelay_NonObservingSelectorIsNotCalled(t *testing.T) {
	eps := []domain.Endpoint{endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a")}
	r := newRelayer(t, eps, &stubSigner{}, &stubSender{}, &stubValidator{}, 3)

	// stubSelector does not implement Observer. This must not panic.
	if _, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{}); err != nil {
		t.Fatalf("Relay with a non-observing selector: %v", err)
	}
}

func TestObserve_SuccessReportsSupplierAndContext(t *testing.T) {
	eps := []domain.Endpoint{endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a")}
	r, sel := newObservedRelayer(eps, &stubSigner{}, &stubSender{}, &stubValidator{})

	if _, err := r.Relay(context.Background(), "my-service", domain.RPCTypeJSONRPC, domain.RelayInput{}); err != nil {
		t.Fatalf("Relay: %v", err)
	}

	if len(sel.seen) != 1 {
		t.Fatalf("observed %d outcomes, want 1", len(sel.seen))
	}
	got := sel.seen[0]
	if got.supplier != "supplierA" {
		t.Errorf("supplier = %q, want supplierA", got.supplier)
	}
	if !got.outcome.Success {
		t.Error("Success = false, want true")
	}
	if got.outcome.Err != nil {
		t.Errorf("Err = %v, want nil on success", got.outcome.Err)
	}
	// ServiceID and RPCType are what keep the feed unambiguous when one Relayer
	// serves several services and a supplier serves several of them.
	if got.outcome.ServiceID != domain.ServiceID("my-service") {
		t.Errorf("ServiceID = %q, want my-service", got.outcome.ServiceID)
	}
	if got.outcome.RPCType != domain.RPCTypeJSONRPC {
		t.Errorf("RPCType = %v, want json_rpc", got.outcome.RPCType)
	}
}

func TestObserve_SendFailureIsReportedAgainstThatSupplier(t *testing.T) {
	eps := []domain.Endpoint{
		endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a"),
		endpoint("supplierB", domain.RPCTypeJSONRPC, "http://b"),
	}
	sendErr := errors.New("connection refused")
	sender := &stubSender{failFor: map[string]error{"http://a": sendErr}}
	r, sel := newObservedRelayer(eps, &stubSigner{}, sender, &stubValidator{})

	if _, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{}); err != nil {
		t.Fatalf("Relay: %v", err)
	}

	// One failure, then one success: the feed must show both, in order, so a QoS
	// selector can penalise A and credit B.
	if len(sel.seen) != 2 {
		t.Fatalf("observed %d outcomes, want 2 (the failure and the failover)", len(sel.seen))
	}
	if sel.seen[0].supplier != "supplierA" || sel.seen[0].outcome.Success {
		t.Errorf("first outcome = %+v, want supplierA failed", sel.seen[0])
	}
	if !errors.Is(sel.seen[0].outcome.Err, sendErr) {
		t.Errorf("first outcome Err = %v, want the send error", sel.seen[0].outcome.Err)
	}
	if sel.seen[1].supplier != "supplierB" || !sel.seen[1].outcome.Success {
		t.Errorf("second outcome = %+v, want supplierB succeeded", sel.seen[1])
	}
}

// Serving a response that fails signature validation is the supplier's fault, so
// it must count against them.
func TestObserve_ValidateFailureIsReported(t *testing.T) {
	eps := []domain.Endpoint{
		endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a"),
		endpoint("supplierB", domain.RPCTypeJSONRPC, "http://b"),
	}
	validateErr := errors.New("bad signature")
	validator := &stubValidator{failFor: map[domain.EndpointAddr]error{"supplierA": validateErr}}
	r, sel := newObservedRelayer(eps, &stubSigner{}, &stubSender{}, validator)

	if _, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{}); err != nil {
		t.Fatalf("Relay: %v", err)
	}

	if len(sel.seen) != 2 {
		t.Fatalf("observed %d outcomes, want 2", len(sel.seen))
	}
	if sel.seen[0].supplier != "supplierA" || sel.seen[0].outcome.Success {
		t.Errorf("first outcome = %+v, want supplierA failed validation", sel.seen[0])
	}
	if !errors.Is(sel.seen[0].outcome.Err, validateErr) {
		t.Errorf("first outcome Err = %v, want the validation error", sel.seen[0].outcome.Err)
	}
}

// A signing failure is our bug, not the supplier's. Reporting it would poison the
// feed and get an innocent supplier demoted.
func TestObserve_SignFailureIsNotReported(t *testing.T) {
	eps := []domain.Endpoint{endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a")}
	signer := &stubSigner{failFor: map[domain.EndpointAddr]error{"supplierA": errors.New("no ring")}}
	r, sel := newObservedRelayer(eps, signer, &stubSender{}, &stubValidator{})

	if _, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{}); err == nil {
		t.Fatal("Relay returned nil error on a signing failure")
	}
	if len(sel.seen) != 0 {
		t.Errorf("observed %+v, want nothing — a signing failure is not the supplier's fault", sel.seen)
	}
}

// An endpoint with no URL for the requested type is skipped before any network
// call, so there is no outcome to report for it.
func TestObserve_SkippedEndpointIsNotReported(t *testing.T) {
	eps := []domain.Endpoint{
		endpoint("supplierA", domain.RPCTypeREST, "http://a-rest"), // no JSON-RPC URL
		endpoint("supplierB", domain.RPCTypeJSONRPC, "http://b"),
	}
	r, sel := newObservedRelayer(eps, &stubSigner{}, &stubSender{}, &stubValidator{})

	if _, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{}); err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if len(sel.seen) != 1 {
		t.Fatalf("observed %d outcomes, want 1 — the skipped endpoint was never attempted", len(sel.seen))
	}
	if sel.seen[0].supplier != "supplierB" {
		t.Errorf("supplier = %q, want supplierB", sel.seen[0].supplier)
	}
}

// Latency has to be real: a QoS selector ranking on it is useless if it is zero.
func TestObserve_LatencyIsMeasuredOverTheSend(t *testing.T) {
	eps := []domain.Endpoint{endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a")}
	const delay = 20 * time.Millisecond
	r, sel := newObservedRelayer(eps, &stubSigner{}, &slowSender{delay: delay}, &stubValidator{})

	if _, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{}); err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if len(sel.seen) != 1 {
		t.Fatalf("observed %d outcomes, want 1", len(sel.seen))
	}
	if got := sel.seen[0].outcome.Latency; got < delay {
		t.Errorf("Latency = %v, want at least the sender's %v delay", got, delay)
	}
}

// Every attempt is reported, so the feed and the failover chain cannot drift.
func TestObserve_EveryAttemptIsReported(t *testing.T) {
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
	r, sel := newObservedRelayer(eps, &stubSigner{}, sender, &stubValidator{})

	if _, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{}); err == nil {
		t.Fatal("Relay returned nil error with every endpoint down")
	}
	if len(sel.seen) != 3 {
		t.Fatalf("observed %d outcomes, want 3 (one per attempt)", len(sel.seen))
	}
	for i, want := range []domain.EndpointAddr{"supplierA", "supplierB", "supplierC"} {
		if sel.seen[i].supplier != want {
			t.Errorf("outcome %d supplier = %q, want %q", i, sel.seen[i].supplier, want)
		}
		if sel.seen[i].outcome.Success {
			t.Errorf("outcome %d reported success, want failure", i)
		}
	}
}

// slowSender delays each send so latency is measurable.
type slowSender struct{ delay time.Duration }

func (s *slowSender) Send(_ context.Context, url string, _ []byte, _ domain.RPCType) ([]byte, error) {
	time.Sleep(s.delay)
	return []byte("resp:" + url), nil
}
