package transport

import (
	"net"
	"strings"
)

// HostPolicy decides which Host header values may reach a listener. It exists to
// stop DNS rebinding.
//
// The attack, in short: a page served from evil.com:8545 waits, its DNS
// re-answers 127.0.0.1, and the page then fetches evil.com:8545 again — which is
// now this proxy. The browser considers that SAME-ORIGIN (same scheme, host and
// port as the page it came from), so CORS is never consulted and the origin
// allowlist is never invoked. It is bypassed rather than defeated.
//
// What saves us is that the browser still sends the NAME it thinks it dialled:
//
//	Host: evil.com:8545     ← a rebound browser
//	Host: localhost:8545    ← a node backend
//	Host: 127.0.0.1:8545    ← curl
//
// Reject names we do not recognise and rebinding is dead.
//
// Scope note: cross-origin POST is already handled by the origin check, because
// browsers send Origin on every method except GET and HEAD — including
// same-origin POST. So JSON-RPC (POST-only) was never rebinding-exposed. This
// closes GET, which is what REST and CometBFT actually use. The prize either way
// is a quota drain, not data theft: what this proxy relays is public chain data.
type HostPolicy struct {
	// AllowedHosts is an exact-match allowlist of Host values, with or without a
	// port ("localhost", "rpc.internal:8545"). "*" disables the check.
	//
	// Empty means "derive from the bind address" — see enforces().
	AllowedHosts []string

	// BindAddr is the address the listener binds, as configured.
	BindAddr string
}

// enforces reports whether this listener checks Host at all.
//
// The default is deliberately conditional, because a blanket rule breaks real
// setups. A listener bound to loopback can only ever be reached as localhost, so
// enforcing costs nothing and kills rebinding — and that is exactly the case
// rebinding exists to attack. A listener bound wider has been deliberately
// exposed to the network, where Host values are legitimately a LAN IP, a Docker
// service name, or a reverse proxy's domain; guessing those would break Docker
// and LAN access for no gain, since an attacker on that network can reach the
// port directly and needs no rebinding at all.
//
// An explicit AllowedHosts always enforces: the operator has said what is valid.
func (p HostPolicy) enforces() bool {
	if len(p.AllowedHosts) > 0 {
		return true
	}
	return isLoopbackBind(p.BindAddr)
}

// Allows reports whether a request's Host header may be served.
func (p HostPolicy) Allows(hostHeader string) bool {
	if !p.enforces() {
		return true
	}

	host := hostname(hostHeader)
	if len(p.AllowedHosts) > 0 {
		for _, allowed := range p.AllowedHosts {
			if allowed == "*" {
				return true
			}
			if strings.EqualFold(hostname(allowed), host) {
				return true
			}
		}
		return false
	}

	// Derived policy: a loopback-bound listener answers to loopback names only.
	return isLoopbackHost(host)
}

// hostname strips the port and any IPv6 brackets from a Host value.
func hostname(hostHeader string) string {
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		return h
	}
	// No port, or malformed. Bare IPv6 arrives bracketed.
	return strings.Trim(hostHeader, "[]")
}

// isLoopbackHost reports whether a hostname refers to this machine.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	// Covers 127.0.0.0/8 and ::1. An empty or non-IP name is not loopback, which
	// is the safe answer: "" cannot be trusted and a name we do not know is
	// exactly what a rebound browser sends.
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isLoopbackBind reports whether a listener address is loopback-only.
func isLoopbackBind(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")

	// ":8545" and "0.0.0.0:8545" and "[::]:8545" are every interface, not
	// loopback: the listener is on the network and Host is not its defence.
	if host == "" {
		return false
	}
	return isLoopbackHost(host)
}
