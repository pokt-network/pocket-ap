package selector

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/pokt-network/pocket-ap/domain"
)

func endpoint(supplier string, types ...domain.RPCType) domain.Endpoint {
	urls := make(map[domain.RPCType]string, len(types))
	for _, t := range types {
		urls[t] = "http://" + supplier + "/" + t.String()
	}
	return domain.Endpoint{Supplier: domain.EndpointAddr(supplier), URLs: urls}
}

func suppliers(eps []domain.Endpoint) []string {
	out := make([]string, 0, len(eps))
	for _, ep := range eps {
		out = append(out, string(ep.Supplier))
	}
	sort.Strings(out)
	return out
}

// The rpc-type filter is what stops a JSON-RPC relay being sent to a supplier
// that only advertises REST.
func TestRandomSelectFiltersByRPCType(t *testing.T) {
	eps := []domain.Endpoint{
		endpoint("a", domain.RPCTypeJSONRPC),
		endpoint("b", domain.RPCTypeREST),
		endpoint("c", domain.RPCTypeJSONRPC, domain.RPCTypeREST),
		endpoint("d", domain.RPCTypeGRPC),
	}

	got, err := Random{}.Select(context.Background(), "svc", eps, domain.RPCTypeJSONRPC)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	want := []string{"a", "c"}
	gotSuppliers := suppliers(got)
	if len(gotSuppliers) != len(want) {
		t.Fatalf("selected %v, want exactly %v", gotSuppliers, want)
	}
	for i := range want {
		if gotSuppliers[i] != want[i] {
			t.Fatalf("selected %v, want %v", gotSuppliers, want)
		}
	}
}

func TestRandomSelectNoneSupportTypeReturnsErrNoEndpoint(t *testing.T) {
	eps := []domain.Endpoint{
		endpoint("a", domain.RPCTypeJSONRPC),
		endpoint("b", domain.RPCTypeJSONRPC),
	}
	got, err := Random{}.Select(context.Background(), "svc", eps, domain.RPCTypeGRPC)
	if !errors.Is(err, domain.ErrNoEndpoint) {
		t.Errorf("err = %v, want ErrNoEndpoint", err)
	}
	if got != nil {
		t.Errorf("endpoints = %v, want nil on error", got)
	}
}

func TestRandomSelectEmptyInput(t *testing.T) {
	got, err := Random{}.Select(context.Background(), "svc", nil, domain.RPCTypeJSONRPC)
	if !errors.Is(err, domain.ErrNoEndpoint) {
		t.Errorf("err = %v, want ErrNoEndpoint", err)
	}
	if got != nil {
		t.Errorf("endpoints = %v, want nil", got)
	}
}

// Select shuffles in place, so it must shuffle its own copy — mutating the
// caller's slice would reorder the session's cached endpoints, which are shared
// across concurrent relays.
func TestRandomSelectDoesNotMutateInput(t *testing.T) {
	eps := []domain.Endpoint{
		endpoint("a", domain.RPCTypeJSONRPC),
		endpoint("b", domain.RPCTypeJSONRPC),
		endpoint("c", domain.RPCTypeJSONRPC),
		endpoint("d", domain.RPCTypeJSONRPC),
		endpoint("e", domain.RPCTypeJSONRPC),
	}
	before := suppliers(eps)
	originalOrder := make([]domain.EndpointAddr, len(eps))
	for i, ep := range eps {
		originalOrder[i] = ep.Supplier
	}

	// Select repeatedly: a single call could coincidentally shuffle to the same
	// order, but 50 in-place shuffles of the caller's slice would not go unnoticed.
	for i := 0; i < 50; i++ {
		if _, err := (Random{}).Select(context.Background(), "svc", eps, domain.RPCTypeJSONRPC); err != nil {
			t.Fatalf("Select: %v", err)
		}
	}

	for i, ep := range eps {
		if ep.Supplier != originalOrder[i] {
			t.Fatalf("Select reordered the caller's slice: %v -> %v", originalOrder, suppliers(eps))
		}
	}
	if len(suppliers(eps)) != len(before) {
		t.Error("Select changed the caller's slice length")
	}
}

// All supported endpoints must come back — the rest are the failover chain, so
// dropping any would silently shrink retry coverage.
func TestRandomSelectReturnsEverySupportedEndpoint(t *testing.T) {
	eps := make([]domain.Endpoint, 0, 20)
	for _, s := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		eps = append(eps, endpoint(s, domain.RPCTypeJSONRPC))
	}
	got, err := Random{}.Select(context.Background(), "svc", eps, domain.RPCTypeJSONRPC)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(got) != len(eps) {
		t.Errorf("selected %d endpoints, want all %d", len(got), len(eps))
	}
}
