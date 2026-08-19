package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pokt-network/pocket-ap/domain"
)

// The derived policy is the whole design: enforce where rebinding can reach
// (loopback), stay out of the way where the operator has deliberately exposed
// the listener and legitimate Host values cannot be guessed.
func TestHostPolicy_EnforcesOnlyWhenBoundToLoopback(t *testing.T) {
	tests := []struct {
		name string
		bind string
		want bool
	}{
		{"127.0.0.1 with port", "127.0.0.1:8545", true},
		{"127.0.0.1 other loopback address", "127.0.0.2:8545", true},
		{"ipv6 loopback", "[::1]:8545", true},
		{"localhost by name", "localhost:8545", true},
		{"every interface, shorthand", ":8545", false},
		{"every interface, explicit", "0.0.0.0:8545", false},
		{"every interface, ipv6", "[::]:8545", false},
		{"a specific LAN address", "192.168.1.50:8545", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (HostPolicy{BindAddr: tt.bind}).enforces(); got != tt.want {
				t.Errorf("bind %q: enforces = %v, want %v", tt.bind, got, tt.want)
			}
		})
	}
}

// An explicit allowlist always applies — the operator has said what is valid, so
// the bind address stops being the deciding factor.
func TestHostPolicy_ExplicitAllowlistEnforcesRegardlessOfBind(t *testing.T) {
	p := HostPolicy{AllowedHosts: []string{"rpc.internal"}, BindAddr: "0.0.0.0:8545"}
	if !p.enforces() {
		t.Fatal("an explicit allowlist did not enforce on a wide bind")
	}
	if !p.Allows("rpc.internal:8545") {
		t.Error("the allowlisted host was rejected")
	}
	if p.Allows("evil.example.com:8545") {
		t.Error("a host outside the allowlist was accepted")
	}
}

func TestHostPolicy_LoopbackBindAllowsLocalNamesOnly(t *testing.T) {
	p := HostPolicy{BindAddr: "127.0.0.1:8545"}
	tests := []struct {
		host string
		want bool
	}{
		{"localhost:8545", true},
		{"localhost", true},
		{"LOCALHOST:8545", true}, // hostnames are case-insensitive
		{"127.0.0.1:8545", true},
		{"127.0.0.1", true},
		{"[::1]:8545", true},
		{"::1", true},
		// THE attack: a rebound browser sends the name it believes it dialled.
		{"evil.example.com:8545", false},
		{"evil.example.com", false},
		// A public IP that happens to point here is not a name we answer to.
		{"1.2.3.4:8545", false},
		// No Host at all cannot be trusted.
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := p.Allows(tt.host); got != tt.want {
				t.Errorf("Allows(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

// Bound wide, the check is off: an attacker on that network reaches the port
// directly and needs no rebinding, so enforcing would only break Docker service
// names and LAN access.
func TestHostPolicy_WideBindAllowsAnything(t *testing.T) {
	p := HostPolicy{BindAddr: ":8545"}
	for _, host := range []string{"evil.example.com:8545", "192.168.1.50:8545", "pocket-ap:8545", ""} {
		if !p.Allows(host) {
			t.Errorf("Allows(%q) = false on a wide bind, want true", host)
		}
	}
}

func TestHostPolicy_WildcardDisablesTheCheck(t *testing.T) {
	p := HostPolicy{AllowedHosts: []string{"*"}, BindAddr: "127.0.0.1:8545"}
	if !p.Allows("evil.example.com:8545") {
		t.Error(`"*" did not disable the host check`)
	}
}

// Ports must not make two spellings of the same host disagree.
func TestHostPolicy_PortIsIgnoredWhenMatching(t *testing.T) {
	p := HostPolicy{AllowedHosts: []string{"rpc.internal:8545"}, BindAddr: "0.0.0.0:8545"}
	for _, host := range []string{"rpc.internal", "rpc.internal:8545", "rpc.internal:9999"} {
		if !p.Allows(host) {
			t.Errorf("Allows(%q) = false, want true — the port is not part of the identity here", host)
		}
	}
}

func TestHostname(t *testing.T) {
	tests := []struct{ in, want string }{
		{"localhost:8545", "localhost"},
		{"localhost", "localhost"},
		{"127.0.0.1:8545", "127.0.0.1"},
		{"[::1]:8545", "::1"},
		{"[::1]", "::1"},
		{"::1", "::1"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := hostname(tt.in); got != tt.want {
				t.Errorf("hostname(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// --- through the real handler --------------------------------------------

// The rebinding case, end to end: same-origin means no Origin header, so the
// origin check never fires. Only the Host gives it away — and the relay must
// cost nothing.
func TestHTTP_RebindingGETIsRejectedAndCostsNoRelay(t *testing.T) {
	relay := &countingRelay{}
	// Loopback bind, no explicit hosts: the derived policy applies.
	h := NewHTTP("127.0.0.1:8545", "pnf-anvil", domain.RPCTypeREST, relay.fn, nil, nil)

	// A rebound browser: no Origin (it believes this is same-origin), and the
	// Host is the name it thinks it dialled.
	req := httptest.NewRequest(http.MethodGet, "/cosmos/base/tendermint/v1beta1/node_info", nil)
	req.Host = "evil.example.com:8545"
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", rec.Code)
	}
	if n := relay.calls.Load(); n != 0 {
		t.Errorf("%d relays spent for a rebound browser, want 0", n)
	}
}

func TestHTTP_LoopbackHostIsServed(t *testing.T) {
	relay := &countingRelay{}
	h := NewHTTP("127.0.0.1:8545", "pnf-anvil", domain.RPCTypeJSONRPC, relay.fn, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Host = "localhost:8545"
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200 — localhost is exactly who this is for", rec.Code)
	}
	if n := relay.calls.Load(); n != 1 {
		t.Errorf("relays = %d, want 1", n)
	}
}

// Binding wide must not start rejecting the Host values that made someone bind
// wide in the first place.
func TestHTTP_WideBindDoesNotCheckHost(t *testing.T) {
	relay := &countingRelay{}
	h := NewHTTP(":8545", "pnf-anvil", domain.RPCTypeJSONRPC, relay.fn, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Host = "192.168.1.50:8545" // reached over the LAN, as intended
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200 — a wide bind means LAN access was the point", rec.Code)
	}
}

// Host is checked before Origin: a rebound GET carries no Origin, so relying on
// the origin check alone would let it through.
func TestHTTP_HostIsCheckedBeforeOrigin(t *testing.T) {
	relay := &countingRelay{}
	// "*" origins: if Host were checked second (or not at all), this would pass.
	h := NewHTTP("127.0.0.1:8545", "pnf-anvil", domain.RPCTypeJSONRPC, relay.fn, []string{"*"}, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "evil.example.com:8545"
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403 — Host must gate even when every origin is allowed", rec.Code)
	}
	if n := relay.calls.Load(); n != 0 {
		t.Errorf("%d relays spent, want 0", n)
	}
}
