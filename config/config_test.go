package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pokt-network/pocket-ap/domain"
)

// write drops cfg into a temp file and returns its path.
func write(t *testing.T, cfg string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const validConfig = `
app:
  private_key_hex: "deadbeef"
fullnode:
  grpc_host_port: "grpc.example.com:443"
  grpc_insecure: false
  rpc_url: "https://rpc.example.com"
listeners:
  - addr: "127.0.0.1:8545"
    service_id: "pnf-anvil"
    rpc_type: "json_rpc"
`

func TestLoadValid(t *testing.T) {
	// Isolate from a real key in the developer's environment.
	t.Setenv("POCKET_APP_PRIVATE_KEY", "")
	t.Setenv("POCKET_APP_PRIVATE_KEYS", "")

	cfg, err := Load(write(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.App.PrivateKeyHex != "deadbeef" {
		t.Error("app key not loaded from file")
	}
	if cfg.FullNode.GRPCHostPort != "grpc.example.com:443" {
		t.Errorf("grpc_host_port = %q", cfg.FullNode.GRPCHostPort)
	}
	if cfg.FullNode.GRPCInsecure {
		t.Error("grpc_insecure = true, want false")
	}
	if len(cfg.Listeners) != 1 {
		t.Fatalf("listeners = %d, want 1", len(cfg.Listeners))
	}

	serviceID, rpcType, err := cfg.Listeners[0].Parsed()
	if err != nil {
		t.Fatalf("Parsed: %v", err)
	}
	if serviceID != domain.ServiceID("pnf-anvil") || rpcType != domain.RPCTypeJSONRPC {
		t.Errorf("Parsed() = (%v, %v)", serviceID, rpcType)
	}
}

// KnownFields(true) is deliberate: a typo'd key must fail loudly at load rather
// than silently running with a default.
func TestLoadUnknownFieldErrors(t *testing.T) {
	t.Setenv("POCKET_APP_PRIVATE_KEY", "")
	t.Setenv("POCKET_APP_PRIVATE_KEYS", "")

	cfg := strings.Replace(validConfig, "grpc_insecure: false", "grpc_insecureee: false", 1)
	_, err := Load(write(t, cfg))
	if err == nil {
		t.Fatal("Load accepted an unknown field, want an error")
	}
	if !strings.Contains(err.Error(), "grpc_insecureee") {
		t.Errorf("error = %v, want it to name the unknown field", err)
	}
}

// The key may be kept out of the config file entirely, so it never has to live
// in something committable.
func TestLoadAppKeyFromEnv(t *testing.T) {
	t.Setenv("POCKET_APP_PRIVATE_KEY", "key-from-env")
	t.Setenv("POCKET_APP_PRIVATE_KEYS", "")

	cfg := strings.Replace(validConfig, `private_key_hex: "deadbeef"`, `private_key_hex: ""`, 1)
	loaded, err := Load(write(t, cfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.App.PrivateKeyHex != "key-from-env" {
		t.Errorf("app key = %q, want the env value", loaded.App.PrivateKeyHex)
	}
}

// A key in the file wins; the env var is only a fallback.
func TestLoadFileKeyBeatsEnv(t *testing.T) {
	t.Setenv("POCKET_APP_PRIVATE_KEY", "key-from-env")
	t.Setenv("POCKET_APP_PRIVATE_KEYS", "")

	loaded, err := Load(write(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.App.PrivateKeyHex != "deadbeef" {
		t.Errorf("app key = %q, want the file value to win", loaded.App.PrivateKeyHex)
	}
}

// Listeners are a serve-time concern, enforced in runServe — a config for
// "pocket-ap call" carries none, and must still load.
func TestLoadWithoutListeners(t *testing.T) {
	t.Setenv("POCKET_APP_PRIVATE_KEY", "")
	t.Setenv("POCKET_APP_PRIVATE_KEYS", "")

	cfg := validConfig[:strings.Index(validConfig, "listeners:")]
	loaded, err := Load(write(t, cfg))
	if err != nil {
		t.Fatalf("Load rejected a listener-less config: %v", err)
	}
	if len(loaded.Listeners) != 0 {
		t.Errorf("listeners = %d, want 0", len(loaded.Listeners))
	}
}

func TestLoadValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{
			name: "missing app key",
			mutate: func(c string) string {
				return strings.Replace(c, `private_key_hex: "deadbeef"`, `private_key_hex: ""`, 1)
			},
			wantErr: "private_key_hex",
		},
		{
			name: "missing grpc host port",
			mutate: func(c string) string {
				return strings.Replace(c, `grpc_host_port: "grpc.example.com:443"`, `grpc_host_port: ""`, 1)
			},
			wantErr: "grpc_host_port",
		},
		{
			name: "missing rpc url",
			mutate: func(c string) string {
				return strings.Replace(c, `rpc_url: "https://rpc.example.com"`, `rpc_url: ""`, 1)
			},
			wantErr: "rpc_url",
		},
		{
			name:    "listener without addr",
			mutate:  func(c string) string { return strings.Replace(c, `addr: "127.0.0.1:8545"`, `addr: ""`, 1) },
			wantErr: "addr",
		},
		{
			// One app has exactly one service, so both forms of the key are
			// live at once and "which one signs" would depend on load order.
			name: "both app and apps",
			mutate: func(c string) string {
				return c + "apps:\n  - private_key_hex: \"cafebabe\"\n"
			},
			wantErr: "not both",
		},
		{
			name: "same key twice",
			mutate: func(c string) string {
				return strings.Replace(c,
					"app:\n  private_key_hex: \"deadbeef\"\n",
					"apps:\n  - private_key_hex: \"deadbeef\"\n  - private_key_hex: \"deadbeef\"\n", 1)
			},
			wantErr: "repeats the key",
		},
		{
			// A supplier address that cannot match is silent and expensive: an
			// allowlist drops every supplier, a denylist denies nobody.
			name: "supplier address is not an address",
			mutate: func(c string) string {
				return strings.Replace(c,
					"app:\n  private_key_hex: \"deadbeef\"\n",
					"app:\n  private_key_hex: \"deadbeef\"\n  suppliers:\n    allow:\n      - \"supplier-3\"\n", 1)
			},
			wantErr: "operator address",
		},
		{
			name: "listener with unknown rpc type",
			mutate: func(c string) string {
				return strings.Replace(c, `rpc_type: "json_rpc"`, `rpc_type: "carrier-pigeon"`, 1)
			},
			wantErr: "carrier-pigeon",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("POCKET_APP_PRIVATE_KEY", "")
			t.Setenv("POCKET_APP_PRIVATE_KEYS", "")

			_, err := Load(write(t, tt.mutate(validConfig)))
			if err == nil {
				t.Fatalf("Load accepted an invalid config, want an error naming %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to name %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("Load of a nonexistent path returned nil error")
	}
}
