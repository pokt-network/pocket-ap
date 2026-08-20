package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testKey = "4bd7f2e1a9c3068b5d4f7e2a1c9b8d6e3f5a7c2b4d6e8f0a1c3e5b7d9f2a4c6e"

// The two endpoints of a network have to move together. A config that reaches
// one chain for sessions and another for block height fails in a way that looks
// like an outage, so the shorthand must set both or neither.
func TestNetwork_SetsBothTransports(t *testing.T) {
	for _, name := range NetworkNames() {
		t.Run(name, func(t *testing.T) {
			var cfg Config
			n, err := cfg.SetNetwork(name)
			if err != nil {
				t.Fatalf("SetNetwork(%q): %v", name, err)
			}
			if cfg.FullNode.GRPCHostPort != n.GRPCHostPort || cfg.FullNode.RPCURL != n.RPCURL {
				t.Errorf("SetNetwork(%q) left fullnode at %q/%q, want %q/%q",
					name, cfg.FullNode.GRPCHostPort, cfg.FullNode.RPCURL, n.GRPCHostPort, n.RPCURL)
			}
			if !strings.HasPrefix(n.RPCURL, "https://") || !strings.HasSuffix(n.GRPCHostPort, ":443") {
				t.Errorf("network %q is not TLS: grpc %q, rpc %q — these are public endpoints and plaintext would ship every query in the clear",
					name, n.GRPCHostPort, n.RPCURL)
			}
		})
	}
}

// grpc_insecure survives from the file otherwise, so someone switching from a
// local node to a public network would keep sending queries in plaintext while
// believing they had only changed networks.
func TestNetwork_ForcesTLSOnTheGRPCTransport(t *testing.T) {
	cfg := Config{FullNode: FullNode{GRPCInsecure: true}}
	if _, err := cfg.SetNetwork("beta"); err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}
	if cfg.FullNode.GRPCInsecure {
		t.Error("SetNetwork left grpc_insecure: true against a public endpoint")
	}
}

// Only "beta" and "main" are real, and a typo has to be caught here: it would
// otherwise surface much later as a full node that cannot be reached.
func TestNetwork_UnknownNameNamesTheAlternatives(t *testing.T) {
	var cfg Config
	_, err := cfg.SetNetwork("mainnet")
	if err == nil {
		t.Fatal("SetNetwork(\"mainnet\") succeeded; want an error")
	}
	for _, want := range NetworkNames() {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the valid network %q", err, want)
		}
	}
}

// Naming a network AND an endpoint is refused rather than resolved. Either
// precedence silently discards something the operator wrote, and the resulting
// failure reads as a broken network rather than a misconfigured one.
func TestNetwork_ConfigCannotSetBothNetworkAndEndpoints(t *testing.T) {
	t.Setenv("POCKET_APP_PRIVATE_KEY", testKey)
	t.Setenv("POCKET_APP_PRIVATE_KEYS", "")

	for _, field := range []string{
		"  grpc_host_port: \"sauron-grpc.infra.pocket.network:443\"",
		"  rpc_url: \"https://sauron-rpc.infra.pocket.network\"",
	} {
		_, err := Load(write(t, "network: \"beta\"\nfullnode:\n"+field+"\n"))
		if err == nil {
			t.Fatalf("config with network AND %s loaded; want an error", strings.TrimSpace(field))
		}
		if !strings.Contains(err.Error(), "network") {
			t.Errorf("error %q does not mention the network key", err)
		}
	}
}

// The flag is the override: an operator typing -network is stating an intent for
// this run that outranks the file. Without this, switching quickly would mean
// editing the config first, which is the thing the flag exists to avoid.
func TestNetwork_SetNetworkOverridesAnExplicitConfig(t *testing.T) {
	cfg := Config{FullNode: FullNode{
		GRPCHostPort: "localhost:9090",
		RPCURL:       "http://localhost:26657",
	}}
	n, err := cfg.SetNetwork("main")
	if err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}
	if cfg.FullNode.GRPCHostPort != n.GRPCHostPort {
		t.Errorf("fullnode.grpc_host_port is %q, want the network's %q", cfg.FullNode.GRPCHostPort, n.GRPCHostPort)
	}
	if !n.SpendsRealValue {
		t.Error("main is not marked as spending real value — that flag is what drives the startup warning")
	}
}

// networks.go is the SOURCE for these hostnames; the docs are copies. Checking
// the copies against the source means a rotation is one edit plus a list of
// which docs still disagree.
func TestNetworkEndpointsAreDocumentedEverywhere(t *testing.T) {
	for _, doc := range []string{"config.schema.yaml", "README.md", "AGENTS.md"} {
		body, err := os.ReadFile(filepath.Join("..", doc))
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		for _, name := range NetworkNames() {
			n, err := LookupNetwork(name)
			if err != nil {
				t.Fatalf("LookupNetwork(%q): %v", name, err)
			}
			for field, val := range map[string]string{"grpc": n.GRPCHostPort, "rpc": n.RPCURL} {
				if !strings.Contains(string(body), val) {
					t.Errorf("%s does not mention the %s %s endpoint %q from config/networks.go — that file is the source, and these docs are copies of it",
						doc, name, field, val)
				}
			}
		}
	}
}
