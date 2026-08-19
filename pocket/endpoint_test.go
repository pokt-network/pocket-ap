package pocket

import (
	"sort"
	"testing"

	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"

	"github.com/pokt-network/pocket-ap/domain"
)

// session builds a poktroll session from a supplier→(rpcType→url) description.
func session(serviceID string, suppliers map[string]map[sharedtypes.RPCType]string) *sessiontypes.Session {
	// Stable order: sync.Map/range order would otherwise make failures flaky.
	names := make([]string, 0, len(suppliers))
	for name := range suppliers {
		names = append(names, name)
	}
	sort.Strings(names)

	out := &sessiontypes.Session{
		SessionId: "sess-1",
		Header:    &sessiontypes.SessionHeader{ServiceId: serviceID},
	}
	for _, name := range names {
		endpoints := make([]*sharedtypes.SupplierEndpoint, 0, len(suppliers[name]))
		types := make([]int, 0, len(suppliers[name]))
		for t := range suppliers[name] {
			types = append(types, int(t))
		}
		sort.Ints(types)
		for _, t := range types {
			rpcType := sharedtypes.RPCType(t)
			endpoints = append(endpoints, &sharedtypes.SupplierEndpoint{
				Url:     suppliers[name][rpcType],
				RpcType: rpcType,
			})
		}
		out.Suppliers = append(out.Suppliers, &sharedtypes.Supplier{
			OperatorAddress: name,
			Services: []*sharedtypes.SupplierServiceConfig{
				{ServiceId: serviceID, Endpoints: endpoints},
			},
		})
	}
	return out
}

func find(eps []domain.Endpoint, supplier domain.EndpointAddr) (domain.Endpoint, bool) {
	for _, ep := range eps {
		if ep.Supplier == supplier {
			return ep, true
		}
	}
	return domain.Endpoint{}, false
}

// One domain.Endpoint per supplier, carrying that supplier's URLs keyed by type.
// Selection filters on those keys, so a dropped type means a supplier silently
// disappears for that RPC type.
func TestEndpointsFromSession(t *testing.T) {
	s := session("pnf-anvil", map[string]map[sharedtypes.RPCType]string{
		"supA": {sharedtypes.RPCType_JSON_RPC: "https://a/rpc"},
		"supB": {
			sharedtypes.RPCType_JSON_RPC: "https://b/rpc",
			sharedtypes.RPCType_REST:     "https://b/rest",
		},
	})

	got := endpointsFromSession(s)
	if len(got) != 2 {
		t.Fatalf("endpoints = %d, want one per supplier (2)", len(got))
	}

	a, ok := find(got, "supA")
	if !ok {
		t.Fatal("supA missing")
	}
	if url, ok := a.URL(domain.RPCTypeJSONRPC); !ok || url != "https://a/rpc" {
		t.Errorf("supA json_rpc url = %q (ok=%v)", url, ok)
	}
	if a.SupportsType(domain.RPCTypeREST) {
		t.Error("supA reports REST support it never advertised")
	}

	b, ok := find(got, "supB")
	if !ok {
		t.Fatal("supB missing")
	}
	if url, _ := b.URL(domain.RPCTypeREST); url != "https://b/rest" {
		t.Errorf("supB rest url = %q", url)
	}
	// A supplier may serve different URLs per type — the reason selection is
	// keyed by type rather than by supplier alone.
	jsonURL, _ := b.URL(domain.RPCTypeJSONRPC)
	if jsonURL == "https://b/rest" {
		t.Error("supB's per-type URLs collapsed into one")
	}
}

// Every native Shannon type must survive the mapping. GRPC in particular: SAGE's
// map omits it (endpoint.go:65) and we deliberately added it, so nothing
// upstream would catch its loss.
func TestEndpointsFromSession_MapsEveryRPCType(t *testing.T) {
	tests := []struct {
		shared sharedtypes.RPCType
		want   domain.RPCType
	}{
		{sharedtypes.RPCType_JSON_RPC, domain.RPCTypeJSONRPC},
		{sharedtypes.RPCType_REST, domain.RPCTypeREST},
		{sharedtypes.RPCType_COMET_BFT, domain.RPCTypeCometBFT},
		{sharedtypes.RPCType_GRPC, domain.RPCTypeGRPC},
		{sharedtypes.RPCType_WEBSOCKET, domain.RPCTypeWebSocket},
	}
	for _, tt := range tests {
		t.Run(tt.want.String(), func(t *testing.T) {
			s := session("svc", map[string]map[sharedtypes.RPCType]string{
				"supA": {tt.shared: "https://a/x"},
			})
			got := endpointsFromSession(s)
			if len(got) != 1 {
				t.Fatalf("endpoints = %d, want 1", len(got))
			}
			if !got[0].SupportsType(tt.want) {
				t.Errorf("%v did not map to domain %v — the supplier vanishes for this type", tt.shared, tt.want)
			}
		})
	}
}

// The mapping must cover every domain type, or a whole RPC type silently has no
// endpoints anywhere.
func TestRPCTypeMappingCoversEveryDomainType(t *testing.T) {
	want := map[domain.RPCType]bool{
		domain.RPCTypeJSONRPC: true, domain.RPCTypeREST: true,
		domain.RPCTypeCometBFT: true, domain.RPCTypeGRPC: true,
		domain.RPCTypeWebSocket: true,
	}
	got := map[domain.RPCType]bool{}
	for _, v := range rpcTypeMapping {
		got[v] = true
	}
	for rt := range want {
		if !got[rt] {
			t.Errorf("no shared type maps to domain %v", rt)
		}
	}
	if _, ok := got[domain.RPCTypeUnknown]; ok {
		t.Error("something maps to RPCTypeUnknown")
	}
}

// An unknown/future type from the chain must be skipped, not crash or become
// RPCTypeUnknown (which would make it look selectable).
func TestEndpointsFromSession_SkipsUnknownRPCType(t *testing.T) {
	s := session("svc", map[string]map[sharedtypes.RPCType]string{
		"supA": {
			sharedtypes.RPCType_JSON_RPC:    "https://a/rpc",
			sharedtypes.RPCType_UNKNOWN_RPC: "https://a/mystery",
		},
	})
	got := endpointsFromSession(s)
	if len(got) != 1 {
		t.Fatalf("endpoints = %d, want 1", len(got))
	}
	if got[0].SupportsType(domain.RPCTypeUnknown) {
		t.Error("an unknown rpc type became a selectable endpoint")
	}
	if !got[0].SupportsType(domain.RPCTypeJSONRPC) {
		t.Error("the known type was lost alongside the unknown one")
	}
}

func TestEndpointsFromSession_EmptySession(t *testing.T) {
	got := endpointsFromSession(&sessiontypes.Session{
		SessionId: "s", Header: &sessiontypes.SessionHeader{ServiceId: "svc"},
	})
	if len(got) != 0 {
		t.Errorf("endpoints = %d, want 0 for a session with no suppliers", len(got))
	}
}

// The supplier address is what every relay is signed against and billed to, so
// it must come through exactly.
func TestEndpointsFromSession_CarriesSupplierAddress(t *testing.T) {
	const addr = "pokt18na0p7t37du6s5yufvajfatwhkv362qyjytxvz"
	s := session("svc", map[string]map[sharedtypes.RPCType]string{
		addr: {sharedtypes.RPCType_JSON_RPC: "https://a/rpc"},
	})
	got := endpointsFromSession(s)
	if len(got) != 1 {
		t.Fatalf("endpoints = %d, want 1", len(got))
	}
	if got[0].Supplier != domain.EndpointAddr(addr) {
		t.Errorf("supplier = %q, want %q", got[0].Supplier, addr)
	}
}
