package config

import (
	"fmt"
	"sort"
	"strings"
)

// Network is a named set of public full-node endpoints. It exists because the
// two transports have to move together: pointing grpc_host_port at Beta while
// rpc_url still names MainNet produces a proxy that reads block height from one
// chain and sessions from another, which fails in a way that looks like the
// network is down rather than like a typo. Naming the pair makes that
// unrepresentable, the same way `pocketd --network=beta` sets --chain-id, --node
// and --grpc-addr together instead of leaving three chances to disagree.
//
// These are Pocket Network Foundation's public endpoints. They are shared
// infrastructure: fine to start on, worth replacing with your own node if you
// depend on this.
type Network struct {
	// GRPCHostPort answers session, app, account and shared-params queries.
	GRPCHostPort string
	// RPCURL is the CometBFT RPC endpoint, polled for the current block height.
	RPCURL string
	// SpendsRealValue is true for networks whose relays cost something a person
	// would miss. It drives a startup warning, not behaviour — nothing here
	// stops a MainNet relay, and nothing should.
	SpendsRealValue bool
	// ChainID is not used to reach anything; it is what a user sees in a
	// response and the fastest way to confirm they are where they meant to be.
	ChainID string
}

// networks is the SOURCE for these hostnames. They are also written in
// config.example.yaml, config.schema.yaml, README.md and AGENTS.md, and
// TestExampleEndpointsAreDocumentedEverywhere checks the docs against this map
// rather than the other way round — so a rotation is one edit here plus the
// failures telling you which docs still disagree.
var networks = map[string]Network{
	"beta": {
		GRPCHostPort: "sauron-grpc.beta.infra.pocket.network:443",
		RPCURL:       "https://sauron-rpc.beta.infra.pocket.network",
		ChainID:      "pocket-lego-testnet",
	},
	"main": {
		GRPCHostPort:    "sauron-grpc.infra.pocket.network:443",
		RPCURL:          "https://sauron-rpc.infra.pocket.network",
		SpendsRealValue: true,
		ChainID:         "pocket",
	},
}

// NetworkNames lists the known networks, sorted, for error messages and flag help.
func NetworkNames() []string {
	names := make([]string, 0, len(networks))
	for name := range networks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// LookupNetwork resolves a network name. The error names the alternatives
// because a typo here would otherwise surface much later as a full node that
// cannot be reached.
func LookupNetwork(name string) (Network, error) {
	n, ok := networks[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return Network{}, fmt.Errorf("unknown network %q: want one of %s", name, strings.Join(NetworkNames(), ", "))
	}
	return n, nil
}

// applyNetwork points the full node at a named network.
//
// Both endpoints are replaced together, and grpc_insecure is forced off: these
// are public endpoints reached over TLS, and leaving a config's `true` in place
// would send every session query across the internet in plaintext while the
// operator believed they had only switched networks.
func (c *Config) applyNetwork(n Network) {
	c.FullNode.GRPCHostPort = n.GRPCHostPort
	c.FullNode.RPCURL = n.RPCURL
	c.FullNode.GRPCInsecure = false
}
