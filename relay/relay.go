// Package relay holds the core relay flow and the seam interfaces the rest of
// the app plugs into. Interfaces are defined here (consumer side, Go idiom); the
// shannon package provides the concrete implementations.
package relay

import (
	"context"
	"fmt"
	"time"

	"github.com/pokt-network/pocket-ap/domain"
)

// SessionSource fetches and caches the current session for a service, and runs
// the background block-height poller that drives rotation.
//
// Concrete impl: shannon.SessionManager (lift from SAGE protocol/shannon/sessions.go).
type SessionSource interface {
	Session(ctx context.Context, serviceID domain.ServiceID) (*domain.Session, error)
	Start(ctx context.Context) error
}

// Signer builds a Shannon RelayRequest for (session, endpoint), ring-signs it
// off-chain, and returns the marshaled wire bytes ready to POST.
//
// Concrete impl: shannon.Signer (lift from SAGE protocol/shannon/signer.go +
// the request-building half of relayer.go SendRelay).
type Signer interface {
	SignRelay(ctx context.Context, session *domain.Session, endpoint domain.Endpoint, rpcType domain.RPCType, in domain.RelayInput) (relayReqBz []byte, err error)
}

// Validator verifies the supplier's RelayResponse signature and unwraps the
// inner HTTP response.
//
// Concrete impl: shannon.FullNode.ValidateResponse (wraps sdk.ValidateRelayResponse).
type Validator interface {
	ValidateResponse(supplier domain.EndpointAddr, respBz []byte) (*domain.RelayResult, error)
}

// Selector returns the endpoints that support rpcType, in the order they should
// be tried (first = primary, rest = failover).
//
// serviceID is passed even though the endpoints already come from that service's
// session: a single Relayer serves every configured service, so a quality-aware
// Selector needs it to keep per-service state apart. Without it a Selector could
// learn from the Observer feed (which carries ServiceID) but not act on it —
// quality would collapse to one global average across unrelated services.
// selector.Random ignores it.
//
// ctx carries anything the CALLER declared about this one relay — today, the
// per-request supplier allow/deny lists a front adapter lifts off the request
// headers (domain.SupplierPolicyFromContext). It is on the interface rather than
// threaded through the relay core because the core must not know that such a
// preference exists: it fetches a session and hands the endpoints over, and what
// narrows them is entirely between the front door and the Selector.
//
// Concrete impl: selector.Random.
type Selector interface {
	Select(ctx context.Context, serviceID domain.ServiceID, endpoints []domain.Endpoint, rpcType domain.RPCType) ([]domain.Endpoint, error)
}

// Sender POSTs signed relay bytes to a supplier URL and returns the raw response
// body. Abstracted so it can be swapped (h2c for gRPC, custom transport, tests).
type Sender interface {
	Send(ctx context.Context, url string, relayReqBz []byte, rpcType domain.RPCType) (respBz []byte, err error)
}

// Outcome is how one relay attempt against one supplier turned out. It is the
// feed a quality-aware Selector learns from.
//
// ServiceID and RPCType are carried because a single Relayer serves every
// configured service, and one supplier can serve several of them: without them
// the feed is ambiguous, and a supplier that is fast on one service and slow on
// another would collapse into a meaningless average.
//
// Latency covers the Send only — the network round trip to the supplier.
// Validation is local signature checking on our own CPU, so folding it in would
// measure us rather than them. A validation failure is still reported here
// (Success false) because serving an unverifiable response is the supplier's
// fault, and it carries the Send latency.
type Outcome struct {
	ServiceID domain.ServiceID
	RPCType   domain.RPCType
	Success   bool
	Latency   time.Duration
	Err       error
}

// Observer is an OPTIONAL companion to Selector. A Selector that also implements
// Observer is told how each attempt it picked turned out, which is what lets it
// learn; Relayer type-asserts its Selector to Observer and calls it only when
// implemented, so selector.Random needs no change and pays nothing.
//
// Observe runs synchronously on the relay hot path, once per attempt.
// Implementations MUST NOT block: update a counter or hand off to a buffered
// channel and return. Anything slower belongs behind that channel.
//
// A signing failure is deliberately NOT reported: it is our fault, not the
// supplier's, and blaming a supplier for our bug would poison the feed.
type Observer interface {
	Observe(supplier domain.EndpointAddr, outcome Outcome)
}

// observingSelector delegates selection to the embedded Selector and fans every
// outcome out to observers.
type observingSelector struct {
	Selector
	observers []Observer
}

func (o observingSelector) Observe(supplier domain.EndpointAddr, outcome Outcome) {
	for _, ob := range o.observers {
		ob.Observe(supplier, outcome)
	}
}

// WithObservers attaches observers to a Selector.
//
// The Relayer takes its Observer from its Selector alone, which means only one
// thing can watch the feed. That is fine until two want it — health counters and
// a QoS selector, say. WithObservers resolves that: the result still selects via
// sel, and forwards each outcome to every observer. If sel observes in its own
// right (a QoS selector does), it keeps receiving the feed too.
//
// Observers are called in order, synchronously, on the relay hot path — the same
// contract Observer itself carries: do not block.
func WithObservers(sel Selector, observers ...Observer) Selector {
	if len(observers) == 0 {
		return sel
	}
	// A Selector that already observes must not lose its feed by being wrapped.
	if selObserver, ok := sel.(Observer); ok {
		observers = append([]Observer{selObserver}, observers...)
	}
	return observingSelector{Selector: sel, observers: observers}
}

// Relayer composes the seams into the v0 stateless relay flow:
//
//	fetch session -> select endpoints(type) -> [sign -> send -> validate] with failover
type Relayer struct {
	Sessions    SessionSource
	Signer      Signer
	Validator   Validator
	Selector    Selector
	Sender      Sender
	MaxAttempts int // failover cap; 0 => try all selected endpoints

	// StreamSender is required by RelayStream and unused by Relay. A response
	// only declares itself streaming once it arrives, so anything serving
	// arbitrary requests should call RelayStream and wire this.
	StreamSender StreamSender
}

// Relay runs the flow for one inbound request. It is the RelayFunc every
// transport adapter calls.
func (r *Relayer) Relay(ctx context.Context, serviceID domain.ServiceID, rpcType domain.RPCType, in domain.RelayInput) (*domain.RelayResult, error) {
	session, err := r.Sessions.Session(ctx, serviceID)
	if err != nil {
		return nil, fmt.Errorf("relay: session fetch failed for %s: %w", serviceID, err)
	}

	ordered, err := r.Selector.Select(ctx, serviceID, session.Endpoints, rpcType)
	if err != nil {
		return nil, fmt.Errorf("relay: endpoint selection failed for %s/%s: %w", serviceID, rpcType, err)
	}
	if len(ordered) == 0 {
		return nil, domain.ErrNoEndpoint
	}

	attempts := r.MaxAttempts
	if attempts <= 0 || attempts > len(ordered) {
		attempts = len(ordered)
	}

	// A Selector that also implements Observer learns from what it picked.
	// selector.Random does not, so this is nil for the default wiring.
	observer, _ := r.Selector.(Observer)
	observe := func(supplier domain.EndpointAddr, success bool, latency time.Duration, err error) {
		if observer == nil {
			return
		}
		observer.Observe(supplier, Outcome{
			ServiceID: serviceID,
			RPCType:   rpcType,
			Success:   success,
			Latency:   latency,
			Err:       err,
		})
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		ep := ordered[i]
		url, ok := ep.URL(rpcType)
		if !ok {
			lastErr = domain.ErrNoEndpoint
			continue
		}

		relayReqBz, err := r.Signer.SignRelay(ctx, session, ep, rpcType, in)
		if err != nil {
			// Signing failure is not endpoint-specific; abort rather than retry.
			// Not observed: the supplier did nothing wrong.
			return nil, fmt.Errorf("relay: sign failed: %w", err)
		}

		start := time.Now()
		respBz, err := r.Sender.Send(ctx, url, relayReqBz, rpcType)
		latency := time.Since(start)
		if err != nil {
			observe(ep.Supplier, false, latency, err)
			lastErr = fmt.Errorf("relay: send to %s failed: %w", ep.Supplier, err)
			continue // failover to next endpoint
		}

		result, err := r.Validator.ValidateResponse(ep.Supplier, respBz)
		if err != nil {
			observe(ep.Supplier, false, latency, err)
			lastErr = fmt.Errorf("relay: validate from %s failed: %w", ep.Supplier, err)
			continue // failover
		}

		observe(ep.Supplier, true, latency, nil)
		return result, nil
	}

	return nil, fmt.Errorf("relay: all %d attempt(s) failed: %w", attempts, lastErr)
}
