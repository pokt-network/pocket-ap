package domain

import (
	"context"
	"testing"
)

func TestSupplierPolicy_DenyWins(t *testing.T) {
	p := SupplierPolicy{
		Allow: []EndpointAddr{"supA", "supB"},
		Deny:  []EndpointAddr{"supB"},
	}
	if !p.Permits("supA") {
		t.Error("supA is allowed and not denied, want permitted")
	}
	// The reason to deny a supplier is that it is misbehaving. A stale allowlist
	// entry must not resurrect it.
	if p.Permits("supB") {
		t.Error("supB is denied AND allowed, want denied")
	}
	if p.Permits("supC") {
		t.Error("supC is absent from a non-empty allowlist, want dropped")
	}
}

func TestSupplierPolicy_EmptyPermitsEverything(t *testing.T) {
	var p SupplierPolicy
	if !p.Empty() {
		t.Error("zero policy is not Empty")
	}
	if !p.Permits("anything") {
		t.Error("zero policy must permit everything")
	}
}

func TestSupplierPolicy_ContextRoundTrip(t *testing.T) {
	want := SupplierPolicy{Allow: []EndpointAddr{"supA"}}
	ctx := WithSupplierPolicy(context.Background(), want)

	got := SupplierPolicyFromContext(ctx)
	if len(got.Allow) != 1 || got.Allow[0] != "supA" {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// The relay path asks every time, and almost every relay carries no preference.
// Not storing the zero value keeps that path from allocating a context node per
// request for nothing.
func TestSupplierPolicy_EmptyPolicyIsNotStored(t *testing.T) {
	base := context.Background()
	if ctx := WithSupplierPolicy(base, SupplierPolicy{}); ctx != base {
		t.Error("an empty policy derived a new context")
	}
}

// A Selector may be called from a path that never set one — tests, future
// callers. It must read as "no preference", not panic and not deny everything.
func TestSupplierPolicy_AbsentReadsAsNoPreference(t *testing.T) {
	if got := SupplierPolicyFromContext(context.Background()); !got.Empty() {
		t.Errorf("got %+v, want the zero policy", got)
	}
	//nolint:staticcheck // deliberately nil: a nil ctx must not panic here.
	if got := SupplierPolicyFromContext(nil); !got.Empty() {
		t.Errorf("nil ctx: got %+v, want the zero policy", got)
	}
}

// --- host dimension ---

func endpointOn(supplier, url string) Endpoint {
	return Endpoint{Supplier: EndpointAddr(supplier), URLs: map[RPCType]string{RPCTypeJSONRPC: url}}
}

func TestSupplierPolicy_HostMatching(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		url     string
		want    bool
	}{
		{"exact host", "rm.example.com", "https://rm.example.com", true},
		{"different host", "rm.example.com", "https://other.example.com", false},
		{"case insensitive", "RM.Example.COM", "https://rm.example.com", true},
		// A path must never make a host match: the whole reason to parse rather
		// than substring-match is that https://evil.example/rm.example.com is not
		// rm.example.com, and a substring denylist would fail OPEN on it.
		{"host in the path is not the host", "rm.example.com", "https://evil.example/rm.example.com", false},
		{"host as a prefix of another", "example.com", "https://example.com.evil.net", false},
		// A bare pattern means "any port"; a pattern with one is exact.
		{"bare pattern matches any port", "rm.example.com", "https://rm.example.com:8545", true},
		{"port must match when named", "rm.example.com:8545", "https://rm.example.com:8545", true},
		{"wrong port when named", "rm.example.com:8545", "https://rm.example.com:9999", false},
		// The scheme's default port is what suppliers actually leave implicit.
		{"https default port fills in", "rm.example.com:443", "https://rm.example.com", true},
		{"http default port fills in", "rm.example.com:80", "http://rm.example.com", true},
		{"wss default port fills in", "rm.example.com:443", "wss://rm.example.com", true},
		{"default port mismatch", "rm.example.com:80", "https://rm.example.com", false},
		// "*." is subdomains, conventionally NOT the apex.
		{"wildcard matches a subdomain", "*.example.com", "https://rm.example.com", true},
		{"wildcard matches a deep subdomain", "*.example.com", "https://a.b.example.com", true},
		{"wildcard does not match the apex", "*.example.com", "https://example.com", false},
		{"wildcard does not match a sibling", "*.example.com", "https://example.com.evil.net", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := SupplierPolicy{AllowHosts: []string{tt.pattern}}
			if got := p.PermitsEndpoint(endpointOn("supA", tt.url), RPCTypeJSONRPC); got != tt.want {
				t.Errorf("allow %q vs %q = %v, want %v", tt.pattern, tt.url, got, tt.want)
			}
			// A denylist is the same match, inverted. Checking both directions is
			// what catches a matcher that fails open for deny.
			d := SupplierPolicy{DenyHosts: []string{tt.pattern}}
			if got := d.PermitsEndpoint(endpointOn("supA", tt.url), RPCTypeJSONRPC); got != !tt.want {
				t.Errorf("deny %q vs %q = %v, want %v", tt.pattern, tt.url, got, !tt.want)
			}
		})
	}
}

// The point of the host dimension: several suppliers share one relay-miner host
// (all 32 beta suppliers do), and an address list cannot name that set because
// it is session-scoped.
func TestSupplierPolicy_HostDenyCoversEverySupplierBehindIt(t *testing.T) {
	p := SupplierPolicy{DenyHosts: []string{"rm.beta.infra.pocket.network"}}
	for _, sup := range []string{"pokt1a", "pokt1b", "pokt1c"} {
		ep := endpointOn(sup, "https://rm.beta.infra.pocket.network")
		if p.PermitsEndpoint(ep, RPCTypeJSONRPC) {
			t.Errorf("%s survived a deny on the host it answers on", sup)
		}
	}
}

// Both dimensions apply. An address allowlist does not rescue a denied host, and
// a host allowlist does not rescue a denied address.
func TestSupplierPolicy_DimensionsAreANDed(t *testing.T) {
	p := SupplierPolicy{
		Allow:     []EndpointAddr{"pokt1a"},
		DenyHosts: []string{"bad.example.com"},
	}
	if p.PermitsEndpoint(endpointOn("pokt1a", "https://bad.example.com"), RPCTypeJSONRPC) {
		t.Error("an allowed address survived a denied host")
	}
	if !p.PermitsEndpoint(endpointOn("pokt1a", "https://good.example.com"), RPCTypeJSONRPC) {
		t.Error("an allowed address on an undenied host was dropped")
	}

	q := SupplierPolicy{Deny: []EndpointAddr{"pokt1a"}, AllowHosts: []string{"good.example.com"}}
	if q.PermitsEndpoint(endpointOn("pokt1a", "https://good.example.com"), RPCTypeJSONRPC) {
		t.Error("a denied address survived an allowed host")
	}
}

// A URL we cannot parse cannot be checked against a host policy. Fail CLOSED:
// an operator who named the hosts they will pay must not be routed to one we
// could not identify.
func TestSupplierPolicy_UnparseableURLIsDroppedWhenHostsMatter(t *testing.T) {
	p := SupplierPolicy{AllowHosts: []string{"rm.example.com"}}
	if p.PermitsEndpoint(endpointOn("supA", "://not a url"), RPCTypeJSONRPC) {
		t.Error("an endpoint whose URL does not parse survived a host allowlist")
	}
	d := SupplierPolicy{DenyHosts: []string{"rm.example.com"}}
	if d.PermitsEndpoint(endpointOn("supA", "://not a url"), RPCTypeJSONRPC) {
		t.Error("an endpoint whose URL does not parse survived a host denylist")
	}
}

// An endpoint with no URL for this type is about to be dropped for not
// supporting it. That is the Selector's call, not a host verdict — otherwise the
// diagnostics would blame the host policy for an rpc-type mismatch.
func TestSupplierPolicy_EndpointWithoutThisTypePassesHostChecks(t *testing.T) {
	p := SupplierPolicy{AllowHosts: []string{"rm.example.com"}}
	ep := Endpoint{Supplier: "supA", URLs: map[RPCType]string{RPCTypeWebSocket: "wss://other.example.com"}}
	if !p.PermitsEndpoint(ep, RPCTypeJSONRPC) {
		t.Error("an endpoint with no URL for this rpc type was judged by host policy")
	}
}

// Each list rejects the other's content, so a paste into the wrong one is loud.
func TestValidate_ListsRejectEachOthersContent(t *testing.T) {
	if err := ValidateHostPattern("https://rm.example.com/"); err == nil {
		t.Error("a URL was accepted as a host pattern — it can never match, and as a deny it fails open")
	}
	if err := ValidateHostPattern("rm.example.com/path"); err == nil {
		t.Error("a host with a path was accepted")
	}
	if err := ValidateHostPattern("pokt1abc"); err == nil {
		t.Error("an operator address was accepted as a host pattern")
	}
	if err := ValidateHostPattern(""); err == nil {
		t.Error("an empty host pattern was accepted")
	}
	if err := ValidateSupplierAddr("rm.example.com"); err == nil {
		t.Error("a host was accepted as an operator address")
	}
	if err := ValidateHostPattern("*.example.com:443"); err != nil {
		t.Errorf("a legitimate wildcard-with-port pattern was rejected: %v", err)
	}
	if err := ValidateSupplierAddr("pokt1abc"); err != nil {
		t.Errorf("a legitimate address was rejected: %v", err)
	}
}

func TestSupplierPolicy_HostListsCountTowardsEmpty(t *testing.T) {
	if (SupplierPolicy{AllowHosts: []string{"x.example.com"}}).Empty() {
		t.Error("a host allowlist reported Empty — Filter would skip it entirely")
	}
	if (SupplierPolicy{DenyHosts: []string{"x.example.com"}}).Empty() {
		t.Error("a host denylist reported Empty")
	}
}
