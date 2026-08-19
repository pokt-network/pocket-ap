package transport

import (
	"fmt"
	"strings"

	"github.com/pokt-network/pocket-ap/domain"
)

// Per-request supplier routing headers.
//
// The operator's config lists are fixed for the process lifetime, which makes
// them useless to anything that changes its mind — an external QoS process
// computing a supplier set per request cannot restart the proxy between
// requests. These headers are the per-request form of the same declaration:
//
//	user --(request)--> QoS --(request + allowed suppliers)--> pocket-ap
//
// Each holds a comma-separated list; repeating the header appends. They can only
// NARROW what the config already allows (see selector.Filter) — a header cannot
// add a supplier the operator excluded.
//
// The Suppliers pair names operator addresses; the Hosts pair names the
// relay-miner host an endpoint answers on, and so covers every supplier behind
// it — a set that is session-scoped and cannot be listed by address in advance.
// Separate headers rather than one list, for the reason on domain.SupplierPolicy.
//
// The "X-" prefix is deprecated by RFC 6648 for anything aiming at
// standardisation, and is kept deliberately anyway: these are stripped from the
// relayed request, so the name has to be one no backend could plausibly want.
const (
	HeaderAllowSuppliers = "X-Pocket-Allow-Suppliers"
	HeaderDenySuppliers  = "X-Pocket-Deny-Suppliers"
	HeaderAllowHosts     = "X-Pocket-Allow-Hosts"
	HeaderDenyHosts      = "X-Pocket-Deny-Hosts"
)

// TakeSupplierPolicy removes the supplier routing headers from header and
// returns the policy they describe. The zero policy means "the caller expressed
// no preference".
//
// It REMOVES rather than reads: the headers address this proxy, not the backend.
// Forwarding them would put them in the bytes the supplier signs — telling the
// supplier which of its competitors the caller ranked, and letting a backend see
// a header it has no business seeing. Same reasoning as a hop-by-hop header.
//
// Header maps come from both net/http (canonical keys) and the call CLI (keys as
// typed), so matching is case-insensitive.
func TakeSupplierPolicy(header map[string][]string) (domain.SupplierPolicy, error) {
	var (
		p   domain.SupplierPolicy
		err error
	)
	// Every header is taken before the first error is returned, so a request with
	// two bad lists does not need two round trips to fix — and, more importantly,
	// so a malformed header can never survive in the map and reach the backend.
	allow, allowErr := takeList(header, HeaderAllowSuppliers, domain.ValidateSupplierAddr)
	deny, denyErr := takeList(header, HeaderDenySuppliers, domain.ValidateSupplierAddr)
	allowHosts, allowHostErr := takeList(header, HeaderAllowHosts, domain.ValidateHostPattern)
	denyHosts, denyHostErr := takeList(header, HeaderDenyHosts, domain.ValidateHostPattern)

	for _, e := range []error{allowErr, denyErr, allowHostErr, denyHostErr} {
		if e != nil {
			err = e
			break
		}
	}
	if err != nil {
		return domain.SupplierPolicy{}, err
	}

	for _, a := range allow {
		p.Allow = append(p.Allow, domain.EndpointAddr(a))
	}
	for _, d := range deny {
		p.Deny = append(p.Deny, domain.EndpointAddr(d))
	}
	p.AllowHosts, p.DenyHosts = allowHosts, denyHosts
	return p, nil
}

// takeList pulls every value of one header out of the map and parses the
// comma-separated entries in them, validating each with validate.
func takeList(header map[string][]string, name string, validate func(string) error) ([]string, error) {
	var raw []string
	for k, vs := range header {
		if strings.EqualFold(k, name) {
			raw = append(raw, vs...)
			delete(header, k)
		}
	}
	if len(raw) == 0 {
		return nil, nil
	}

	var out []string
	for _, v := range raw {
		for _, field := range strings.Split(v, ",") {
			field = strings.TrimSpace(field)
			if field == "" {
				// A trailing comma or an empty header is a formatting slip, not an
				// instruction. Dropping it beats erroring: it changes nothing.
				continue
			}
			if err := validate(field); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			out = append(out, field)
		}
	}
	return out, nil
}
