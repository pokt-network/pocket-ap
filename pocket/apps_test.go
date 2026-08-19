package pocket

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	apptypes "github.com/pokt-network/poktroll/x/application/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"

	"github.com/pokt-network/pocket-ap/domain"
)

// appNode answers GetApp per address, which fakeNode does not — the whole point
// of these tests is that several apps exist at once.
type appNode struct {
	fakeNode
	apps map[string]*apptypes.Application
	err  error
}

func (a *appNode) GetApp(_ context.Context, appAddr string) (*apptypes.Application, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.appCalls++
	if a.err != nil {
		return nil, a.err
	}
	app, ok := a.apps[appAddr]
	if !ok {
		return nil, errors.New("no app " + appAddr)
	}
	return app, nil
}

func stakedApp(addr string, services ...string) *apptypes.Application {
	cfgs := make([]*sharedtypes.ApplicationServiceConfig, 0, len(services))
	for _, s := range services {
		cfgs = append(cfgs, &sharedtypes.ApplicationServiceConfig{ServiceId: s})
	}
	return &apptypes.Application{Address: addr, ServiceConfigs: cfgs}
}

// signerFor builds a Signer with no SDK behind it. Enough for the app lookups —
// the ring signing needs the real chain and is deliberately not faked.
func signerWithAddr(addr string, node nodeClient) *Signer {
	// slog.Default() is set to a discard handler by this package's test init.
	return &Signer{appAddr: addr, fn: node, logger: slog.Default()}
}

// The service is derivable because poktroll allows an app exactly one — that is
// the fact that makes config service_id optional, so it is worth pinning.
func TestSignerServiceID_ReadsTheAppsOneService(t *testing.T) {
	node := &appNode{apps: map[string]*apptypes.Application{
		"pokt1a": stakedApp("pokt1a", "pnf-pocket-beta"),
	}}
	s := signerWithAddr("pokt1a", node)

	got, err := s.ServiceID(context.Background())
	if err != nil {
		t.Fatalf("ServiceID: %v", err)
	}
	if got != domain.ServiceID("pnf-pocket-beta") {
		t.Errorf("ServiceID = %q", got)
	}
}

// Discovery must reuse the signer's app cache, or every app pays a second
// GetApp on top of the one the first relay already makes to build its ring.
func TestSignerServiceID_SharesTheAppCacheWithSigning(t *testing.T) {
	node := &appNode{apps: map[string]*apptypes.Application{
		"pokt1a": stakedApp("pokt1a", "svc"),
	}}
	s := signerWithAddr("pokt1a", node)

	for i := 0; i < 3; i++ {
		if _, err := s.ServiceID(context.Background()); err != nil {
			t.Fatalf("ServiceID: %v", err)
		}
	}
	if _, _, apps := node.counts(); apps != 1 {
		t.Errorf("GetApp called %d times, want 1 — the app cache is not shared", apps)
	}
}

// An unstaked app is the failure this replaces: without the check it relays
// happily until GetSession, then fails every request with "session not found"
// while the key looks fine.
func TestSignerServiceID_UnstakedAppSaysSo(t *testing.T) {
	node := &appNode{apps: map[string]*apptypes.Application{
		"pokt1a": stakedApp("pokt1a"), // staked for nothing
	}}
	s := signerWithAddr("pokt1a", node)

	_, err := s.ServiceID(context.Background())
	if err == nil {
		t.Fatal("ServiceID succeeded for an app staked for no service")
	}
	if !strings.Contains(err.Error(), "no service") {
		t.Errorf("error = %v, want it to say the app is staked for no service", err)
	}
}

// If this ever fires, the 1:1 app/service assumption under multi-app is gone and
// the error should say that rather than picking one arbitrarily.
func TestSignerServiceID_MultiServiceAppIsRejected(t *testing.T) {
	node := &appNode{apps: map[string]*apptypes.Application{
		"pokt1a": stakedApp("pokt1a", "svc-a", "svc-b"),
	}}
	s := signerWithAddr("pokt1a", node)

	if _, err := s.ServiceID(context.Background()); err == nil {
		t.Fatal("ServiceID picked one of two services instead of failing")
	}
}

// MultiSigner routes on the session's app, which is what lets one process hold
// several keys without the relay core knowing.
func TestMultiSigner_RoutesOnTheSessionsApp(t *testing.T) {
	node := &appNode{}
	a := signerWithAddr("pokt1a", node)
	b := signerWithAddr("pokt1b", node)
	m := NewMultiSigner(a, b)

	for _, want := range []*Signer{a, b} {
		got, err := m.signerFor(&domain.Session{AppAddr: want.AppAddr()})
		if err != nil {
			t.Fatalf("signerFor(%s): %v", want.AppAddr(), err)
		}
		if got != want {
			t.Errorf("signerFor(%s) returned the wrong signer", want.AppAddr())
		}
	}
}

// A session for an app we hold no key for is a wiring bug between the session
// manager and the signer set. The error names the app so it is findable.
func TestMultiSigner_UnknownAppNamesTheApp(t *testing.T) {
	m := NewMultiSigner(signerWithAddr("pokt1a", &appNode{}))

	_, err := m.SignRelay(context.Background(),
		&domain.Session{AppAddr: "pokt1zzz", ServiceID: "svc"},
		domain.Endpoint{}, domain.RPCTypeJSONRPC, domain.RelayInput{})
	if err == nil {
		t.Fatal("SignRelay signed for an app with no configured key")
	}
	for _, want := range []string{"pokt1zzz", "svc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// The single-app case must go through exactly the same code as multi-app, or the
// wiring everyone actually runs is the untested one.
func TestMultiSigner_SingleAppIsNotASpecialCase(t *testing.T) {
	only := signerWithAddr("pokt1a", &appNode{})
	m := NewMultiSigner(only)

	if addrs := m.AppAddrs(); len(addrs) != 1 || addrs[0] != "pokt1a" {
		t.Fatalf("AppAddrs() = %v", addrs)
	}
	got, err := m.signerFor(&domain.Session{AppAddr: "pokt1a"})
	if err != nil || got != only {
		t.Errorf("signerFor = (%v, %v), want the only signer", got, err)
	}
}
