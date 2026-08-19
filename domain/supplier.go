package domain

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// SupplierAddrPrefix is what every Pocket operator address starts with. Shared
// by config and the transports because a supplier list that silently matches
// nothing is the expensive failure here, and two copies of that check is two
// things to get wrong.
const SupplierAddrPrefix = "pokt1"

// SupplierPolicy is an allow/deny list of supplier operator addresses.
//
// It is deliberately NOT the QoS/reputation machinery this repo bans (see
// CLAUDE.md): it keeps no state, measures nothing, and never changes its mind.
// It is a caller declaring which suppliers they are willing to pay — a routing
// decision they own, which is the whole point of running your own access point.
// Anything that scores suppliers from observed behaviour is still SAGE's job.
//
// Two callers can declare one. The operator does it in config, once per app,
// for the process lifetime. A client does it per request, via headers (see
// transport/suppliers.go), which is what lets an external QoS process sit in
// front of a long-running pocket-ap and change its mind without a restart.
//
// The two compose by AND, never by replacement: see Permits.
//
// Deny is applied first and wins, so an address in both lists is denied. That
// ordering is the safe one: the reason to deny a supplier is that it is doing
// something wrong, and a stale allowlist entry must not resurrect it.
// It has two INDEPENDENT dimensions, deliberately kept in separate fields:
//
//   - by supplier operator address (Allow/Deny) — names one supplier exactly;
//   - by endpoint host (AllowHosts/DenyHosts) — names the relay miner a supplier
//     sits behind, and therefore every supplier behind it.
//
// The second is not a finer version of the first, it is a coarser and different
// one: one relay-miner operator routinely runs many supplier stakes behind a
// single host (on beta all 32 pnf-pocket-beta suppliers answer on
// rm.beta.infra.pocket.network). So "drop every supplier behind this operator"
// is expressible ONLY by host — the address set behind a host is session-scoped
// and cannot be enumerated in advance — while "drop this one supplier" is
// expressible only by address.
//
// They are separate fields rather than one mixed list because in a mixed list an
// entry's meaning would depend on whether it happened to parse as an address.
// Each list rejects the other's content loudly (see ValidateSupplierAddr and
// ValidateHostPattern), so a paste into the wrong one is an error rather than a
// list that quietly matches nothing.
//
// All dimensions in force must permit a supplier: deny is deny, and a non-empty
// allow list of either kind is exhaustive within its own dimension.
type SupplierPolicy struct {
	// Allow, when non-empty, is exhaustive: every supplier not named is dropped.
	Allow []EndpointAddr
	// Deny removes suppliers from whatever remains.
	Deny []EndpointAddr

	// AllowHosts, when non-empty, is exhaustive over endpoint hosts.
	// DenyHosts removes every supplier behind the named hosts.
	//
	// Each entry is a host, optionally with a port, optionally with a leading
	// "*." to match subdomains: "rm.example.com", "rm.example.com:8545",
	// "*.example.com". A URL is rejected — see ValidateHostPattern.
	AllowHosts []string
	DenyHosts  []string
}

// Empty reports whether this policy would change anything.
func (p SupplierPolicy) Empty() bool {
	return len(p.Allow) == 0 && len(p.Deny) == 0 &&
		len(p.AllowHosts) == 0 && len(p.DenyHosts) == 0
}

// Permits reports whether a supplier survives the policy.
func (p SupplierPolicy) Permits(supplier EndpointAddr) bool {
	for _, denied := range p.Deny {
		if denied == supplier {
			return false
		}
	}
	if len(p.Allow) == 0 {
		return true
	}
	for _, allowed := range p.Allow {
		if allowed == supplier {
			return true
		}
	}
	return false
}

// PermitsEndpoint applies BOTH dimensions to one endpoint: its operator address
// and the host of the URL it serves rpcType on. This is what a Selector calls.
//
// An endpoint that advertises no URL for rpcType is passed through untouched:
// it is about to be dropped for not supporting the type, and that is the
// Selector's decision to make, not a host-policy verdict.
func (p SupplierPolicy) PermitsEndpoint(ep Endpoint, rpcType RPCType) bool {
	if !p.Permits(ep.Supplier) {
		return false
	}
	if len(p.AllowHosts) == 0 && len(p.DenyHosts) == 0 {
		return true
	}

	rawURL, ok := ep.URL(rpcType)
	if !ok {
		return true
	}
	host, port, ok := hostPortOfURL(rawURL)
	if !ok {
		// The URL does not parse, so it cannot be checked against a host policy.
		// Fail CLOSED: an operator who named the hosts they will pay must not be
		// routed to one we could not identify. It would fail to dial anyway.
		return false
	}

	for _, pattern := range p.DenyHosts {
		if hostMatches(host, port, pattern) {
			return false
		}
	}
	if len(p.AllowHosts) == 0 {
		return true
	}
	for _, pattern := range p.AllowHosts {
		if hostMatches(host, port, pattern) {
			return true
		}
	}
	return false
}

// defaultPorts fills in the port a URL leaves implicit, so that an operator who
// writes "rm.example.com:443" still matches "https://rm.example.com" — which is
// how suppliers actually advertise. Without this, adding the port a browser
// would show you silently matches nothing.
var defaultPorts = map[string]string{
	"http": "80", "ws": "80",
	"https": "443", "wss": "443",
}

// hostPortOfURL extracts the lowercased host and port from a supplier URL.
func hostPortOfURL(rawURL string) (host, port string, ok bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	host, port = splitHostPort(u.Host)
	if port == "" {
		port = defaultPorts[strings.ToLower(u.Scheme)]
	}
	return host, port, true
}

// splitHostPort lowercases a host[:port], tolerating a missing port and IPv6
// brackets. net.SplitHostPort errors on a bare host, which is the common case.
func splitHostPort(hostPort string) (host, port string) {
	if h, p, err := net.SplitHostPort(hostPort); err == nil {
		return strings.ToLower(h), p
	}
	return strings.ToLower(strings.Trim(hostPort, "[]")), ""
}

// hostMatches reports whether an endpoint's host[:port] matches one pattern.
//
// Matching is on the PARSED host, never on a substring of the URL. A substring
// denylist fails OPEN on https://evil.example/rm.beta.infra.pocket.network —
// and failing open is the wrong direction for a deny.
//
// A pattern with no port matches any port, because that is what someone writing
// a bare hostname means. A pattern WITH a port is exact, which is how you name
// one listener on a host that serves several.
func hostMatches(host, port, pattern string) bool {
	pHost, pPort := splitHostPort(strings.TrimSpace(pattern))
	if pPort != "" && pPort != port {
		return false
	}
	// "*.example.com" matches any subdomain but NOT the apex, which is the
	// conventional reading — an operator who wants both writes both.
	if suffix, found := strings.CutPrefix(pHost, "*"); found {
		return strings.HasSuffix(host, suffix)
	}
	return host == pHost
}

// ValidateSupplierAddr rejects anything that is not an operator address.
//
// The failure being prevented is silence: an address that never matches makes an
// allowlist drop every supplier and a denylist deny nobody, and both read as
// "the network is broken". It also catches a host pattern pasted into an address
// list, which is the mistake the two-dimension design invites.
func ValidateSupplierAddr(addr string) error {
	if !strings.HasPrefix(addr, SupplierAddrPrefix) {
		return fmt.Errorf("%q is not a %s… supplier operator address", addr, SupplierAddrPrefix)
	}
	return nil
}

// ValidateHostPattern rejects anything that is not a bare host pattern.
//
// A URL is the mistake to catch: pasting "https://rm.example.com/" here is the
// obvious thing to try, it can never match a parsed host, and as a denylist
// entry it would fail open. An operator address is caught for the mirror reason
// — it belongs in the other list.
func ValidateHostPattern(pattern string) error {
	p := strings.TrimSpace(pattern)
	switch {
	case p == "":
		return fmt.Errorf("host pattern is empty")
	case strings.Contains(p, "://") || strings.Contains(p, "/"):
		return fmt.Errorf("%q is a URL, not a host — write just the host, optionally with a port (\"rm.example.com\", \"rm.example.com:443\", \"*.example.com\")", pattern)
	case strings.HasPrefix(p, SupplierAddrPrefix):
		return fmt.Errorf("%q is a supplier operator address — it belongs in the address allow/deny list, not the host list", pattern)
	}
	return nil
}

// supplierPolicyKey types the context key so nothing else can collide with it.
type supplierPolicyKey struct{}

// WithSupplierPolicy attaches a request-scoped supplier policy to ctx.
//
// Context is the carrier because the policy is set by a front adapter and read
// by a Selector, with the relay core in between — and the core must stay
// ignorant of it. Threading it through Relay/RelayStream/Prepare as a parameter
// instead would put a routing-preference field in the signature of every seam
// that never looks at it.
func WithSupplierPolicy(ctx context.Context, p SupplierPolicy) context.Context {
	if p.Empty() {
		return ctx
	}
	return context.WithValue(ctx, supplierPolicyKey{}, p)
}

// SupplierPolicyFromContext returns the request-scoped policy, or the zero
// value (which permits everything) when none was attached.
func SupplierPolicyFromContext(ctx context.Context) SupplierPolicy {
	if ctx == nil {
		return SupplierPolicy{}
	}
	p, _ := ctx.Value(supplierPolicyKey{}).(SupplierPolicy)
	return p
}
