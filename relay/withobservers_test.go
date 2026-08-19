package relay

import (
	"context"
	"testing"

	"github.com/pokt-network/pocket-ap/domain"
)

// countingObserver is a bare Observer, i.e. what health.Stats is.
type countingObserver struct {
	seen []domain.EndpointAddr
}

func (c *countingObserver) Observe(supplier domain.EndpointAddr, _ Outcome) {
	c.seen = append(c.seen, supplier)
}

// WithObservers exists because the Relayer takes its Observer from the Selector
// alone: without it, attaching metrics would mean the Selector could not be a
// QoS selector too.
func TestWithObservers_PlainSelectorGainsAFeed(t *testing.T) {
	eps := []domain.Endpoint{endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a")}
	obs := &countingObserver{}

	r := &Relayer{
		Sessions:  stubSessions{session: &domain.Session{ID: "s1", Endpoints: eps}},
		Signer:    &stubSigner{},
		Validator: &stubValidator{},
		// stubSelector does not implement Observer on its own.
		Selector:    WithObservers(stubSelector{ordered: eps}, obs),
		Sender:      &stubSender{},
		MaxAttempts: 3,
	}

	if _, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{}); err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if len(obs.seen) != 1 || obs.seen[0] != "supplierA" {
		t.Errorf("observer saw %v, want [supplierA]", obs.seen)
	}
}

// Wrapping a Selector that already observes must not silence it — otherwise
// attaching metrics would blind a QoS selector.
func TestWithObservers_SelfObservingSelectorKeepsItsFeed(t *testing.T) {
	eps := []domain.Endpoint{endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a")}
	qos := &observingStub{stubSelector: stubSelector{ordered: eps}}
	metrics := &countingObserver{}

	r := &Relayer{
		Sessions:    stubSessions{session: &domain.Session{ID: "s1", Endpoints: eps}},
		Signer:      &stubSigner{},
		Validator:   &stubValidator{},
		Selector:    WithObservers(qos, metrics),
		Sender:      &stubSender{},
		MaxAttempts: 3,
	}

	if _, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{}); err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if len(qos.seen) != 1 {
		t.Errorf("the wrapped selector saw %d outcomes, want 1 — wrapping must not steal its feed", len(qos.seen))
	}
	if len(metrics.seen) != 1 {
		t.Errorf("the added observer saw %d outcomes, want 1", len(metrics.seen))
	}
}

func TestWithObservers_FanOutToSeveral(t *testing.T) {
	eps := []domain.Endpoint{endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a")}
	a, b, c := &countingObserver{}, &countingObserver{}, &countingObserver{}

	r := &Relayer{
		Sessions:    stubSessions{session: &domain.Session{ID: "s1", Endpoints: eps}},
		Signer:      &stubSigner{},
		Validator:   &stubValidator{},
		Selector:    WithObservers(stubSelector{ordered: eps}, a, b, c),
		Sender:      &stubSender{},
		MaxAttempts: 3,
	}

	if _, err := r.Relay(context.Background(), "svc", domain.RPCTypeJSONRPC, domain.RelayInput{}); err != nil {
		t.Fatalf("Relay: %v", err)
	}
	for i, obs := range []*countingObserver{a, b, c} {
		if len(obs.seen) != 1 {
			t.Errorf("observer %d saw %d outcomes, want 1", i, len(obs.seen))
		}
	}
}

// No observers means no wrapper: the Selector comes back untouched, so
// selector.Random keeps costing nothing and the Relayer's type-assert still
// finds no Observer.
func TestWithObservers_NoObserversDoesNotWrap(t *testing.T) {
	// stubSelector holds a slice, so it is uncomparable — assert on the observable
	// property instead: an unwrapped plain Selector must not gain an Observer.
	if _, ok := WithObservers(stubSelector{}).(Observer); ok {
		t.Error("WithObservers with no observers still produced an Observer")
	}
	// And with an observer, it must.
	if _, ok := WithObservers(stubSelector{}, &countingObserver{}).(Observer); !ok {
		t.Error("WithObservers with an observer did not produce an Observer")
	}
}

// Selection must still work through the wrapper.
func TestWithObservers_SelectStillDelegates(t *testing.T) {
	eps := []domain.Endpoint{endpoint("supplierA", domain.RPCTypeJSONRPC, "http://a")}
	sel := &recordingSelector{ordered: eps}
	wrapped := WithObservers(sel, &countingObserver{})

	got, err := wrapped.Select(context.Background(), "my-service", eps, domain.RPCTypeJSONRPC)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Select returned %d endpoints, want 1", len(got))
	}
	if sel.gotService != domain.ServiceID("my-service") {
		t.Errorf("wrapped selector got serviceID %q, want it passed through", sel.gotService)
	}
}
