package selector

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pokt-network/pocket-ap/domain"
)

// recordingInner captures what the Filter passed down, which is the only way to
// tell "filtered" from "reordered".
type recordingInner struct {
	got []domain.Endpoint
}

func (r *recordingInner) Select(_ context.Context, _ domain.ServiceID, endpoints []domain.Endpoint, _ domain.RPCType) ([]domain.Endpoint, error) {
	r.got = endpoints
	return endpoints, nil
}

func endpoints(suppliers ...string) []domain.Endpoint {
	out := make([]domain.Endpoint, 0, len(suppliers))
	for _, s := range suppliers {
		out = append(out, domain.Endpoint{
			Supplier: domain.EndpointAddr(s),
			URLs:     map[domain.RPCType]string{domain.RPCTypeJSONRPC: "https://" + s},
		})
	}
	return out
}

func suppliersOf(eps []domain.Endpoint) []string {
	out := make([]string, 0, len(eps))
	for _, ep := range eps {
		out = append(out, string(ep.Supplier))
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An allowlist is exhaustive: it is how an external process drives selection, so
// anything it does not name must not be relayed to.
func TestFilter_AllowIsExhaustive(t *testing.T) {
	inner := &recordingInner{}
	f := Filter{
		Inner:    inner,
		Policies: map[domain.ServiceID]Policy{"svc": {Allow: []domain.EndpointAddr{"supA", "supC"}}},
	}

	if _, err := f.Select(context.Background(), "svc", endpoints("supA", "supB", "supC"), domain.RPCTypeJSONRPC); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := suppliersOf(inner.got); !equal(got, []string{"supA", "supC"}) {
		t.Errorf("inner saw %v, want only the allowlisted suppliers", got)
	}
}

func TestFilter_DenyRemoves(t *testing.T) {
	inner := &recordingInner{}
	f := Filter{
		Inner:    inner,
		Policies: map[domain.ServiceID]Policy{"svc": {Deny: []domain.EndpointAddr{"supB"}}},
	}

	if _, err := f.Select(context.Background(), "svc", endpoints("supA", "supB", "supC"), domain.RPCTypeJSONRPC); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := suppliersOf(inner.got); !equal(got, []string{"supA", "supC"}) {
		t.Errorf("inner saw %v, want supB removed", got)
	}
}

// Deny wins over allow, and the ordering is not arbitrary: a supplier is denied
// because it is misbehaving, and a stale allowlist entry must not resurrect it.
func TestFilter_DenyBeatsAllow(t *testing.T) {
	inner := &recordingInner{}
	f := Filter{
		Inner: inner,
		Policies: map[domain.ServiceID]Policy{"svc": {
			Allow: []domain.EndpointAddr{"supA", "supB"},
			Deny:  []domain.EndpointAddr{"supB"},
		}},
	}

	if _, err := f.Select(context.Background(), "svc", endpoints("supA", "supB"), domain.RPCTypeJSONRPC); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := suppliersOf(inner.got); !equal(got, []string{"supA"}) {
		t.Errorf("inner saw %v, want supB denied despite being allowed", got)
	}
}

// Policies are per service because supplier sets are: allowing service A's
// suppliers must not empty service B.
func TestFilter_OtherServicesPassThroughUntouched(t *testing.T) {
	inner := &recordingInner{}
	f := Filter{
		Inner:    inner,
		Policies: map[domain.ServiceID]Policy{"svc-a": {Allow: []domain.EndpointAddr{"supA"}}},
	}

	if _, err := f.Select(context.Background(), "svc-b", endpoints("supX", "supY"), domain.RPCTypeJSONRPC); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := suppliersOf(inner.got); !equal(got, []string{"supX", "supY"}) {
		t.Errorf("inner saw %v, want svc-b untouched by svc-a's policy", got)
	}
}

// A policy that filters everything out is a config problem, and the error has to
// say so — the alternative reads as "the network has no suppliers", which sends
// the operator looking in entirely the wrong place.
func TestFilter_EmptyResultBlamesTheConfig(t *testing.T) {
	f := Filter{
		Inner:    &recordingInner{},
		Policies: map[domain.ServiceID]Policy{"svc": {Allow: []domain.EndpointAddr{"supZ"}}},
	}

	_, err := f.Select(context.Background(), "svc", endpoints("supA", "supB"), domain.RPCTypeJSONRPC)
	if err == nil {
		t.Fatal("Select succeeded with every supplier filtered out")
	}
	if !errors.Is(err, domain.ErrNoEndpoint) {
		t.Errorf("error does not wrap ErrNoEndpoint: %v", err)
	}
	for _, want := range []string{"allow/deny", "svc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// The default wiring configures no policy at all, so that path must not change
// what selection sees.
func TestFilter_NoPolicyIsATransparentPassthrough(t *testing.T) {
	inner := &recordingInner{}
	f := Filter{Inner: inner}

	eps := endpoints("supA", "supB")
	got, err := f.Select(context.Background(), "svc", eps, domain.RPCTypeJSONRPC)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(got) != 2 || !equal(suppliersOf(inner.got), []string{"supA", "supB"}) {
		t.Errorf("inner saw %v, want the list unchanged", suppliersOf(inner.got))
	}
}

// Filter composes with the real selector: the rpc-type filtering and shuffling
// still happen, on the reduced set.
func TestFilter_WrapsRandom(t *testing.T) {
	f := Filter{
		Inner:    Random{},
		Policies: map[domain.ServiceID]Policy{"svc": {Deny: []domain.EndpointAddr{"supB"}}},
	}

	for i := 0; i < 20; i++ {
		got, err := f.Select(context.Background(), "svc", endpoints("supA", "supB", "supC"), domain.RPCTypeJSONRPC)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d endpoints, want 2", len(got))
		}
		for _, ep := range got {
			if ep.Supplier == "supB" {
				t.Fatal("a denied supplier survived Random")
			}
		}
	}
}

// --- per-request policy (the header form) ---

// The point of the header: with no config policy at all, an external QoS process
// still gets to pick, per request, without restarting the proxy.
func TestFilter_RequestPolicyNarrowsWithNoConfigPolicy(t *testing.T) {
	inner := &recordingInner{}
	f := Filter{Inner: inner}

	ctx := domain.WithSupplierPolicy(context.Background(), Policy{Allow: []domain.EndpointAddr{"supB"}})
	if _, err := f.Select(ctx, "svc", endpoints("supA", "supB", "supC"), domain.RPCTypeJSONRPC); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := suppliersOf(inner.got); !equal(got, []string{"supB"}) {
		t.Errorf("inner saw %v, want only the requested supplier", got)
	}
}

// THE security property. The listener is unauthenticated, so a request that
// could ADD a supplier would hand routing to anyone who can reach the port. The
// config list is a ceiling; a header can only cut below it.
func TestFilter_RequestPolicyCannotWidenTheConfigAllowlist(t *testing.T) {
	inner := &recordingInner{}
	f := Filter{
		Inner:    inner,
		Policies: map[domain.ServiceID]Policy{"svc": {Allow: []domain.EndpointAddr{"supA"}}},
	}

	// The caller asks for supB, which the operator never allowed.
	ctx := domain.WithSupplierPolicy(context.Background(), Policy{Allow: []domain.EndpointAddr{"supA", "supB"}})
	if _, err := f.Select(ctx, "svc", endpoints("supA", "supB"), domain.RPCTypeJSONRPC); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := suppliersOf(inner.got); !equal(got, []string{"supA"}) {
		t.Errorf("inner saw %v, want supB still excluded by the config allowlist", got)
	}
}

// Same property from the other side: a config DENY is not undone by a request
// that allows the denied supplier. Deny is how an operator takes a misbehaving
// supplier out of rotation, and a client must not be able to put it back.
func TestFilter_RequestPolicyCannotUndoAConfigDeny(t *testing.T) {
	inner := &recordingInner{}
	f := Filter{
		Inner:    inner,
		Policies: map[domain.ServiceID]Policy{"svc": {Deny: []domain.EndpointAddr{"supB"}}},
	}

	ctx := domain.WithSupplierPolicy(context.Background(), Policy{Allow: []domain.EndpointAddr{"supB"}})
	_, err := f.Select(ctx, "svc", endpoints("supA", "supB"), domain.RPCTypeJSONRPC)
	if err == nil {
		t.Fatal("Select succeeded: a request re-enabled a supplier the operator denied")
	}
	if !errors.Is(err, domain.ErrNoEndpoint) {
		t.Errorf("error does not wrap ErrNoEndpoint: %v", err)
	}
}

// A request deny narrows a config allowlist — the QoS process taking one
// supplier out for this request only.
func TestFilter_RequestDenyNarrowsTheConfigAllowlist(t *testing.T) {
	inner := &recordingInner{}
	f := Filter{
		Inner:    inner,
		Policies: map[domain.ServiceID]Policy{"svc": {Allow: []domain.EndpointAddr{"supA", "supB"}}},
	}

	ctx := domain.WithSupplierPolicy(context.Background(), Policy{Deny: []domain.EndpointAddr{"supB"}})
	if _, err := f.Select(ctx, "svc", endpoints("supA", "supB", "supC"), domain.RPCTypeJSONRPC); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := suppliersOf(inner.got); !equal(got, []string{"supA"}) {
		t.Errorf("inner saw %v, want the intersection", got)
	}
}

// Two sources can empty the set, and the operator debugging it needs to know
// which — otherwise they rewrite a config that was never the problem.
func TestFilter_EmptyResultDistinguishesConfigFromRequest(t *testing.T) {
	f := Filter{Inner: &recordingInner{}}

	ctx := domain.WithSupplierPolicy(context.Background(), Policy{Allow: []domain.EndpointAddr{"supZ"}})
	_, err := f.Select(ctx, "svc", endpoints("supA", "supB"), domain.RPCTypeJSONRPC)
	if err == nil {
		t.Fatal("Select succeeded with every supplier filtered out")
	}
	if !strings.Contains(err.Error(), "request allow/deny 1/0") {
		t.Errorf("error %q does not attribute the filtering to the request", err)
	}
	if !strings.Contains(err.Error(), "config allow/deny 0/0") {
		t.Errorf("error %q does not exonerate the config", err)
	}
}

// Nothing on the request means nothing changes — the path every relay without
// the header takes.
func TestFilter_NoRequestPolicyIsATransparentPassthrough(t *testing.T) {
	inner := &recordingInner{}
	f := Filter{Inner: inner}

	ctx := domain.WithSupplierPolicy(context.Background(), Policy{})
	if _, err := f.Select(ctx, "svc", endpoints("supA", "supB"), domain.RPCTypeJSONRPC); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := suppliersOf(inner.got); !equal(got, []string{"supA", "supB"}) {
		t.Errorf("inner saw %v, want the list unchanged", got)
	}
}

// --- host dimension through the Filter ---

// hostEndpoints builds endpoints whose supplier and host differ, which is the
// case the host dimension exists for: many suppliers, one relay-miner host.
func hostEndpoints(pairs map[string]string) []domain.Endpoint {
	out := make([]domain.Endpoint, 0, len(pairs))
	for supplier, url := range pairs {
		out = append(out, domain.Endpoint{
			Supplier: domain.EndpointAddr(supplier),
			URLs:     map[domain.RPCType]string{domain.RPCTypeJSONRPC: url},
		})
	}
	return out
}

func TestFilter_ConfigHostDenyDropsEverySupplierBehindIt(t *testing.T) {
	inner := &recordingInner{}
	f := Filter{
		Inner:    inner,
		Policies: map[domain.ServiceID]Policy{"svc": {DenyHosts: []string{"bad.example.com"}}},
	}

	eps := hostEndpoints(map[string]string{
		"supA": "https://bad.example.com",
		"supB": "https://bad.example.com",
		"supC": "https://good.example.com",
	})
	if _, err := f.Select(context.Background(), "svc", eps, domain.RPCTypeJSONRPC); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := suppliersOf(inner.got); !equal(got, []string{"supC"}) {
		t.Errorf("inner saw %v, want only the supplier off the denied host", got)
	}
}

// The per-request form of the same thing — a QoS process steering by operator
// rather than by individual supplier.
func TestFilter_RequestHostListNarrows(t *testing.T) {
	inner := &recordingInner{}
	f := Filter{Inner: inner}

	eps := hostEndpoints(map[string]string{
		"supA": "https://rm.one.example",
		"supB": "https://rm.two.example",
	})
	ctx := domain.WithSupplierPolicy(context.Background(), Policy{AllowHosts: []string{"rm.one.example"}})
	if _, err := f.Select(ctx, "svc", eps, domain.RPCTypeJSONRPC); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := suppliersOf(inner.got); !equal(got, []string{"supA"}) {
		t.Errorf("inner saw %v, want only the supplier on the requested host", got)
	}
}

// Narrow-only holds on the host axis too: a request cannot name a host the
// config excluded.
func TestFilter_RequestHostListCannotWidenTheConfig(t *testing.T) {
	inner := &recordingInner{}
	f := Filter{
		Inner:    inner,
		Policies: map[domain.ServiceID]Policy{"svc": {AllowHosts: []string{"rm.one.example"}}},
	}

	eps := hostEndpoints(map[string]string{
		"supA": "https://rm.one.example",
		"supB": "https://rm.two.example",
	})
	ctx := domain.WithSupplierPolicy(context.Background(), Policy{AllowHosts: []string{"rm.two.example"}})
	_, err := f.Select(ctx, "svc", eps, domain.RPCTypeJSONRPC)
	if err == nil {
		t.Fatal("Select succeeded: a request reached a host the config excluded")
	}
	if !errors.Is(err, domain.ErrNoEndpoint) {
		t.Errorf("error does not wrap ErrNoEndpoint: %v", err)
	}
}

// An empty set from the host axis must not read as an address problem, or the
// operator rewrites the wrong list.
func TestFilter_EmptyResultNamesTheHostDimension(t *testing.T) {
	f := Filter{
		Inner:    &recordingInner{},
		Policies: map[domain.ServiceID]Policy{"svc": {DenyHosts: []string{"rm.example.com"}}},
	}

	eps := hostEndpoints(map[string]string{"supA": "https://rm.example.com"})
	_, err := f.Select(context.Background(), "svc", eps, domain.RPCTypeJSONRPC)
	if err == nil {
		t.Fatal("Select succeeded with every supplier filtered out")
	}
	if !strings.Contains(err.Error(), "config allow/deny 0/0, hosts 0/1") {
		t.Errorf("error %q does not attribute the filtering to a config host denylist", err)
	}
}
