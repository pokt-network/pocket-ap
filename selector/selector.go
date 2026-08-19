// Package selector picks supplier endpoints for a relay. v0 ships a dumb random
// selector with no QoS. The Selector seam (relay.Selector) lets callers bolt on
// reputation/latency-aware selection later without touching the relay core.
package selector

import (
	"context"
	"math/rand/v2"

	"github.com/pokt-network/pocket-ap/domain"
)

// Random filters endpoints to those supporting the requested RPC type, then
// returns them in random order. The first is the primary; the rest are failover.
type Random struct{}

// Select implements relay.Selector. ctx and serviceID are ignored: random
// selection keeps no state, so it has nothing to keep apart per service, and
// nothing on the request can change how it picks — narrowing the candidate set
// is selector.Filter's job, one layer up.
func (Random) Select(_ context.Context, _ domain.ServiceID, endpoints []domain.Endpoint, rpcType domain.RPCType) ([]domain.Endpoint, error) {
	supported := make([]domain.Endpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		if ep.SupportsType(rpcType) {
			supported = append(supported, ep)
		}
	}
	if len(supported) == 0 {
		return nil, domain.ErrNoEndpoint
	}
	rand.Shuffle(len(supported), func(i, j int) {
		supported[i], supported[j] = supported[j], supported[i]
	})
	return supported, nil
}
