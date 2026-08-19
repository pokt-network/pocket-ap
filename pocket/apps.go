package pocket

import (
	"context"
	"fmt"

	"github.com/pokt-network/pocket-ap/domain"
	"github.com/pokt-network/pocket-ap/relay"
)

// Compile-time assertions: MultiSigner covers both signing seams, so one process
// can hold several app keys without the relay core knowing.
var (
	_ relay.Signer      = (*MultiSigner)(nil)
	_ relay.FrameSigner = (*MultiSigner)(nil)
)

// ServiceID reports the single Shannon service this app is staked for, read from
// the chain.
//
// There is exactly one, and that is a protocol rule rather than a convention:
// poktroll's ValidateAppServiceConfigs (x/shared/types/service_configs.go)
// rejects any stake with a service count other than 1 —
// "application must have exactly one service". So the signing key determines the
// service completely, and asking the operator to also type it into the config is
// asking them to repeat something we can look up.
//
// It costs no extra round trip on the relay path: the app is fetched through the
// same cache SignRelay uses to build the ring, so discovering the service warms
// exactly the entry the first relay needs.
func (s *Signer) ServiceID(ctx context.Context) (domain.ServiceID, error) {
	app, err := s.getApp(ctx, s.appAddr)
	if err != nil {
		return "", fmt.Errorf("app %s: %w", s.appAddr, err)
	}

	switch len(app.ServiceConfigs) {
	case 1:
		return domain.ServiceID(app.ServiceConfigs[0].ServiceId), nil
	case 0:
		// The failure this replaces is horrible: an unstaked (or wrong-network,
		// or wrong-key) app relays fine right up to GetSession and then fails
		// every request with "session not found".
		return "", fmt.Errorf("app %s is staked for no service — check the key is the app's own, and that the app is staked on the network this full node serves", s.appAddr)
	default:
		// Unreachable against a chain enforcing the rule above, which is why it
		// says so: hitting it means the protocol changed and the 1:1 assumption
		// under multi-app config needs revisiting, not that this app is odd.
		return "", fmt.Errorf("app %s is staked for %d services, but poktroll allows exactly one — the app/service mapping this proxy relies on no longer holds", s.appAddr, len(app.ServiceConfigs))
	}
}

// MultiSigner routes signing to the key that owns the session's app.
//
// It exists because one process now serves several apps, and every session
// already carries the app it was fetched for — so nothing in the relay core has
// to change or even learn that multiple keys exist. The seams take a session;
// the session names the app; this picks the key.
type MultiSigner struct {
	byAddr map[string]*Signer
}

// NewMultiSigner indexes signers by the app address each key derives.
//
// A single signer is the common case and stays a one-entry map rather than a
// special case: identical code path, so multi-app cannot regress the single-app
// wiring everyone actually runs.
func NewMultiSigner(signers ...*Signer) *MultiSigner {
	byAddr := make(map[string]*Signer, len(signers))
	for _, s := range signers {
		byAddr[s.AppAddr()] = s
	}
	return &MultiSigner{byAddr: byAddr}
}

// AppAddrs returns every app address this signer can sign for.
func (m *MultiSigner) AppAddrs() []string {
	out := make([]string, 0, len(m.byAddr))
	for addr := range m.byAddr {
		out = append(out, addr)
	}
	return out
}

// signerFor resolves the key for a session's app.
func (m *MultiSigner) signerFor(session *domain.Session) (*Signer, error) {
	s, ok := m.byAddr[session.AppAddr]
	if !ok {
		// Reachable only if a session was fetched for an app this process has no
		// key for, which means the session manager and the signer set disagree —
		// a wiring bug, so the message says which app rather than "sign failed".
		return nil, fmt.Errorf("no signing key configured for app %s (service %s)", session.AppAddr, session.ServiceID)
	}
	return s, nil
}

// SignRelay implements relay.Signer.
func (m *MultiSigner) SignRelay(ctx context.Context, session *domain.Session, endpoint domain.Endpoint, rpcType domain.RPCType, in domain.RelayInput) ([]byte, error) {
	s, err := m.signerFor(session)
	if err != nil {
		return nil, fmt.Errorf("SignRelay: %w", err)
	}
	return s.SignRelay(ctx, session, endpoint, rpcType, in)
}

// SignFrame implements relay.FrameSigner.
func (m *MultiSigner) SignFrame(ctx context.Context, session *domain.Session, supplier domain.EndpointAddr, payload []byte) ([]byte, error) {
	s, err := m.signerFor(session)
	if err != nil {
		return nil, fmt.Errorf("SignFrame: %w", err)
	}
	return s.SignFrame(ctx, session, supplier, payload)
}
