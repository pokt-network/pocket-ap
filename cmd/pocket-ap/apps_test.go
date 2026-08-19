package main

import (
	"strings"
	"testing"

	"github.com/pokt-network/pocket-ap/config"
	"github.com/pokt-network/pocket-ap/domain"
)

func app(serviceID domain.ServiceID) resolvedApp {
	return resolvedApp{addr: "pokt1" + string(serviceID), serviceID: serviceID}
}

// One app has exactly one service, so a listener that names none is not
// ambiguous — requiring service_id there would be asking the operator to restate
// a fact with no alternative.
func TestResolveListenerServices_FillsInTheOnlyApp(t *testing.T) {
	cfg := &config.Config{Listeners: []config.Listener{
		{Addr: "127.0.0.1:8545", RPCType: "json_rpc"},
		{Addr: "127.0.0.1:8546", RPCType: "websocket"},
	}}

	if err := resolveListenerServices(cfg, []resolvedApp{app("pnf-pocket-beta")}); err != nil {
		t.Fatalf("resolveListenerServices: %v", err)
	}
	for i, l := range cfg.Listeners {
		if l.ServiceID != "pnf-pocket-beta" {
			t.Errorf("listeners[%d].service_id = %q, want the single app's service", i, l.ServiceID)
		}
	}
}

// With several apps the port has to say which one it fronts: picking for them
// would silently bill the wrong stake.
func TestResolveListenerServices_RequiresServiceIDWithSeveralApps(t *testing.T) {
	cfg := &config.Config{Listeners: []config.Listener{{Addr: "127.0.0.1:8545", RPCType: "json_rpc"}}}

	err := resolveListenerServices(cfg, []resolvedApp{app("svc-a"), app("svc-b")})
	if err == nil {
		t.Fatal("resolveListenerServices guessed a service with two apps configured")
	}
	if !strings.Contains(err.Error(), "service_id is required") {
		t.Errorf("error = %v", err)
	}
}

// A service no app is staked for is caught at startup rather than at the first
// relay: otherwise the port binds, accepts traffic, and fails everything with
// "session not found".
func TestResolveListenerServices_RejectsAnUnstakedService(t *testing.T) {
	cfg := &config.Config{Listeners: []config.Listener{
		{Addr: "127.0.0.1:8545", ServiceID: "typo-service", RPCType: "json_rpc"},
	}}

	err := resolveListenerServices(cfg, []resolvedApp{app("svc-a")})
	if err == nil {
		t.Fatal("resolveListenerServices accepted a listener for a service no app is staked for")
	}
	for _, want := range []string{"typo-service", "svc-a"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// An explicit service_id that matches must be left alone — several apps, several
// listeners, each pinned.
func TestResolveListenerServices_KeepsExplicitServices(t *testing.T) {
	cfg := &config.Config{Listeners: []config.Listener{
		{Addr: "127.0.0.1:8545", ServiceID: "svc-b", RPCType: "json_rpc"},
		{Addr: "127.0.0.1:8546", ServiceID: "svc-a", RPCType: "rest"},
	}}

	if err := resolveListenerServices(cfg, []resolvedApp{app("svc-a"), app("svc-b")}); err != nil {
		t.Fatalf("resolveListenerServices: %v", err)
	}
	if cfg.Listeners[0].ServiceID != "svc-b" || cfg.Listeners[1].ServiceID != "svc-a" {
		t.Errorf("listeners = %+v, want the configured services untouched", cfg.Listeners)
	}
}

// Only apps with a policy end up in the map: the Filter's fast path is a miss,
// so an empty policy must not become an entry.
func TestSupplierPolicies_OnlyIncludesConfiguredOnes(t *testing.T) {
	apps := []resolvedApp{
		app("svc-a"),
		{addr: "pokt1b", serviceID: "svc-b", suppliers: supplierPolicy(config.SupplierPolicy{
			Allow: []string{"pokt1supX"},
		})},
	}

	got := supplierPolicies(apps)
	if len(got) != 1 {
		t.Fatalf("policies = %+v, want only svc-b", got)
	}
	policy, ok := got["svc-b"]
	if !ok {
		t.Fatal("svc-b has no policy")
	}
	if len(policy.Allow) != 1 || policy.Allow[0] != domain.EndpointAddr("pokt1supX") {
		t.Errorf("svc-b allow = %v", policy.Allow)
	}
}

// The session manager is fed from the resolved set, so the pairing has to
// survive the projection.
func TestServiceApps_PairsEachServiceWithItsApp(t *testing.T) {
	got := serviceApps([]resolvedApp{
		{addr: "pokt1a", serviceID: "svc-a"},
		{addr: "pokt1b", serviceID: "svc-b"},
	})
	if len(got) != 2 {
		t.Fatalf("serviceApps = %+v", got)
	}
	if got[0].ServiceID != "svc-a" || got[0].AppAddr != "pokt1a" ||
		got[1].ServiceID != "svc-b" || got[1].AppAddr != "pokt1b" {
		t.Errorf("serviceApps = %+v, want each service paired with its own app", got)
	}
}

// -service is optional for a single-app config, including one with no listeners
// at all — which is the shape a call-only config has.
func TestResolveTarget_DefaultsToTheOnlyApp(t *testing.T) {
	cfg := &config.Config{}

	serviceID, rpcType, err := resolveTarget(cfg, []resolvedApp{app("svc-a")}, "", "json_rpc")
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if serviceID != "svc-a" || rpcType != domain.RPCTypeJSONRPC {
		t.Errorf("resolveTarget = (%v, %v)", serviceID, rpcType)
	}
}

func TestResolveTarget_AmbiguousWithSeveralApps(t *testing.T) {
	cfg := &config.Config{}

	_, _, err := resolveTarget(cfg, []resolvedApp{app("svc-a"), app("svc-b")}, "", "json_rpc")
	if err == nil {
		t.Fatal("resolveTarget guessed a service with two apps configured")
	}
	if !strings.Contains(err.Error(), "-service is required") {
		t.Errorf("error = %v", err)
	}
}
