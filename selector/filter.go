package selector

import (
	"context"
	"fmt"

	"github.com/pokt-network/pocket-ap/domain"
)

// Policy is a static allow/deny list of supplier operator addresses for one
// service. See domain.SupplierPolicy for the semantics; the alias keeps the
// name that reads best at the call site (selector.Policy) while the type itself
// lives in domain, where the transports can build one without importing this
// package.
type Policy = domain.SupplierPolicy

// Inner is the selector Filter wraps — structurally relay.Selector, restated
// here so this package keeps depending on nothing but domain.
type Inner interface {
	Select(ctx context.Context, serviceID domain.ServiceID, endpoints []domain.Endpoint, rpcType domain.RPCType) ([]domain.Endpoint, error)
}

// Filter applies the supplier policies in force for a relay, then delegates the
// ordering to Inner. Two can apply and BOTH must permit a supplier:
//
//   - the operator's, from config, keyed by service (Policies);
//   - the caller's, from this request's headers, carried on ctx.
//
// Each carries two dimensions — by operator address and by endpoint host — and
// all of them in force must agree. See domain.SupplierPolicy for why a host list
// is not a finer address list but a different axis.
//
// AND, not replacement, is the whole security story: a request can only ever
// narrow the set of suppliers the operator declared they would pay, never widen
// it. The listener is unauthenticated, so a header that could add a supplier the
// config excluded would hand routing to anyone who can reach the port.
//
// A relay with neither policy pays one map lookup and one context lookup.
//
// ⚠️ WRAP THIS ON THE INSIDE of relay.WithObservers, not around it. The Relayer
// takes its Observer by type-asserting its Selector, and Filter does not
// implement Observer — so relay.WithObservers(Filter{...}, stats) keeps the
// outcome feed, while Filter{Inner: relay.WithObservers(...)} silently drops it
// and /health stops counting.
type Filter struct {
	Inner    Inner
	Policies map[domain.ServiceID]Policy
}

// Select implements relay.Selector.
func (f Filter) Select(ctx context.Context, serviceID domain.ServiceID, endpoints []domain.Endpoint, rpcType domain.RPCType) ([]domain.Endpoint, error) {
	configured := f.Policies[serviceID]
	requested := domain.SupplierPolicyFromContext(ctx)
	if configured.Empty() && requested.Empty() {
		return f.Inner.Select(ctx, serviceID, endpoints, rpcType)
	}

	kept := make([]domain.Endpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		if configured.PermitsEndpoint(ep, rpcType) && requested.PermitsEndpoint(ep, rpcType) {
			kept = append(kept, ep)
		}
	}
	if len(kept) == 0 {
		// Distinct from "the session had no endpoints" and from "none support this
		// rpc type": a policy caused this, and the error has to say WHICH one — by
		// source AND by dimension — or the operator debugs their config while the
		// caller's header is at fault, or rewrites an address list when a host list
		// emptied the set. Wrapping ErrNoEndpoint keeps errors.Is working.
		return nil, fmt.Errorf(
			"selector: every supplier for %s was filtered out (%d in session; config allow/deny %d/%d, hosts %d/%d; request allow/deny %d/%d, hosts %d/%d): %w",
			serviceID, len(endpoints),
			len(configured.Allow), len(configured.Deny),
			len(configured.AllowHosts), len(configured.DenyHosts),
			len(requested.Allow), len(requested.Deny),
			len(requested.AllowHosts), len(requested.DenyHosts),
			domain.ErrNoEndpoint)
	}
	return f.Inner.Select(ctx, serviceID, kept, rpcType)
}
