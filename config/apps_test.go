package config

import (
	"strings"
	"testing"
)

// The multi-app form is the answer to "one process per service", which is what
// running several staked apps used to cost. Nothing downstream reads App or Apps
// directly — AppList() is the single normalized view — so this is the seam that
// has to hold.
func TestLoadMultipleApps(t *testing.T) {
	t.Setenv("POCKET_APP_PRIVATE_KEY", "")
	t.Setenv("POCKET_APP_PRIVATE_KEYS", "")

	cfg := `
apps:
  - private_key_hex: "deadbeef"
    service_id: "pnf-pocket-beta"
    suppliers:
      allow:
        - "pokt1supplierA"
      deny:
        - "pokt1supplierB"
  - private_key_hex: "cafebabe"
fullnode:
  grpc_host_port: "grpc.example.com:443"
  rpc_url: "https://rpc.example.com"
listeners:
  - addr: "127.0.0.1:8545"
    service_id: "pnf-pocket-beta"
    rpc_type: "json_rpc"
`
	loaded, err := Load(write(t, cfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	apps := loaded.AppList()
	if len(apps) != 2 {
		t.Fatalf("AppList() = %d apps, want 2", len(apps))
	}
	if apps[0].ServiceID != "pnf-pocket-beta" {
		t.Errorf("apps[0].service_id = %q", apps[0].ServiceID)
	}
	// The second app names no service: it is derived from the key at startup,
	// which is the point of making the field optional.
	if apps[1].ServiceID != "" {
		t.Errorf("apps[1].service_id = %q, want empty (discovered at startup)", apps[1].ServiceID)
	}
	if len(apps[0].Suppliers.Allow) != 1 || apps[0].Suppliers.Allow[0] != "pokt1supplierA" {
		t.Errorf("apps[0] allow = %v", apps[0].Suppliers.Allow)
	}
	if len(apps[0].Suppliers.Deny) != 1 || apps[0].Suppliers.Deny[0] != "pokt1supplierB" {
		t.Errorf("apps[0] deny = %v", apps[0].Suppliers.Deny)
	}
}

// AppList is what every caller uses, so the single-app form has to arrive there
// looking exactly like a one-entry multi-app config. Otherwise the common case
// takes a different code path from the tested one.
func TestAppListNormalizesTheSingleAppForm(t *testing.T) {
	t.Setenv("POCKET_APP_PRIVATE_KEY", "")
	t.Setenv("POCKET_APP_PRIVATE_KEYS", "")

	loaded, err := Load(write(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	apps := loaded.AppList()
	if len(apps) != 1 || apps[0].PrivateKeyHex != "deadbeef" {
		t.Fatalf("AppList() = %+v, want the single app", apps)
	}
}

// A listener may leave service_id out: with one app there is exactly one service
// it could mean. The config layer must accept that — resolving it needs the app
// set, which only the serve path has.
func TestLoadListenerWithoutServiceID(t *testing.T) {
	t.Setenv("POCKET_APP_PRIVATE_KEY", "")
	t.Setenv("POCKET_APP_PRIVATE_KEYS", "")

	cfg := strings.Replace(validConfig, "    service_id: \"pnf-anvil\"\n", "", 1)
	loaded, err := Load(write(t, cfg))
	if err != nil {
		t.Fatalf("Load rejected a listener with no service_id: %v", err)
	}
	if loaded.Listeners[0].ServiceID != "" {
		t.Errorf("service_id = %q, want it left empty for the serve path to fill", loaded.Listeners[0].ServiceID)
	}
}

// Multi-app must be reachable without writing any key to disk — the whole reason
// POCKET_APP_PRIVATE_KEY exists in the first place.
func TestLoadAppKeysFromEnv(t *testing.T) {
	t.Setenv("POCKET_APP_PRIVATE_KEY", "")
	t.Setenv("POCKET_APP_PRIVATE_KEYS", "key-one, key-two ,")

	cfg := strings.Replace(validConfig, "app:\n  private_key_hex: \"deadbeef\"\n", "", 1)
	loaded, err := Load(write(t, cfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	apps := loaded.AppList()
	if len(apps) != 2 {
		t.Fatalf("AppList() = %d apps, want 2 (blank entries dropped)", len(apps))
	}
	// Whitespace around a comma-separated list is what a human types.
	if apps[0].PrivateKeyHex != "key-one" || apps[1].PrivateKeyHex != "key-two" {
		t.Errorf("apps = %+v, want the trimmed env keys", apps)
	}
}

// The plural env var must not quietly add apps to a config that already names
// one: it is a machine-wide setting, and a config that says "app:" has been
// explicit about which app it runs.
func TestConfiguredKeyBeatsTheEnvList(t *testing.T) {
	t.Setenv("POCKET_APP_PRIVATE_KEY", "")
	t.Setenv("POCKET_APP_PRIVATE_KEYS", "key-one,key-two")

	loaded, err := Load(write(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	apps := loaded.AppList()
	if len(apps) != 1 || apps[0].PrivateKeyHex != "deadbeef" {
		t.Errorf("AppList() = %+v, want only the configured app", apps)
	}
}
