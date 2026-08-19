// Package config loads the access-point YAML config. Value types only, sensible
// zero-value defaults — same discipline as SAGE's config package.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/pokt-network/pocket-ap/domain"
)

// Config is the top-level access-point configuration.
type Config struct {
	// App is the single-app form, and stays the common case.
	App App `yaml:"app"`
	// Apps is the multi-app form: one process, one key per staked app, one set
	// of listeners each. Mutually exclusive with App — see validate().
	Apps      []App      `yaml:"apps"`
	FullNode  FullNode   `yaml:"fullnode"`
	Listeners []Listener `yaml:"listeners"`
	Admin     Admin      `yaml:"admin"`

	// GRPCMode picks the framing used to reach a supplier's relay miner for gRPC
	// relays. It is global rather than per-listener because it describes the path
	// to the suppliers, not the front door a client dials.
	//
	// Empty (auto) tries native gRPC once per supplier host and remembers the
	// answer — correct in both deployments, which is why it is the default.
	// "native" forces HTTP/2, right when nothing terminates it in between.
	// "web" forces gRPC-Web over HTTP/1.1, which is what survives an ingress
	// that terminates HTTP/2: the miner answers native calls "gRPC requires
	// HTTP/2", while gRPC-Web crosses untouched because it carries its trailers
	// inside the body.
	GRPCMode string `yaml:"grpc_mode"`
}

// Admin configures the /health listener. It binds its own port, separate from
// every relay listener, because those proxy each path they receive verbatim —
// mounting /health on one would steal a route from the service being proxied.
//
// Opt-in: empty Addr means no admin listener at all. "pocket-ap call" never
// starts one.
type Admin struct {
	Addr string `yaml:"addr"`
}

// App holds one staked application: the key that signs its relays, the service
// it is staked for, and which suppliers may serve it.
type App struct {
	// PrivateKeyHex is the app's own secp256k1 key, hex-encoded. If empty, it is
	// read from the POCKET_APP_PRIVATE_KEY env var so the key never has to live
	// in a committed file. (POCKET_APP_PRIVATE_KEYS, comma-separated, fills the
	// multi-app form the same way.)
	PrivateKeyHex string `yaml:"private_key_hex"`

	// ServiceID is OPTIONAL, because it is derivable from the key.
	//
	// poktroll allows an application exactly one service — ValidateAppServiceConfigs
	// in x/shared/types/service_configs.go rejects anything else with
	// "application must have exactly one service" — so key -> app address ->
	// onchain app -> its one service is a complete derivation with no ambiguity
	// to resolve. Leave this empty and startup discovers it.
	//
	// Setting it is not redundant, but it is not a second source of truth either:
	// startup compares it against the chain and refuses to run on a mismatch. So
	// it buys a loud error for the "wrong key pasted" case instead of the
	// silent-wrong-app failure deriveAppAddr already warns about, and costs a
	// line of config.
	ServiceID string `yaml:"service_id"`

	// Suppliers restricts which suppliers may serve this app's relays.
	//
	// It lives on the app rather than on a listener or the top level because the
	// protocol makes app, service and supplier set line up 1:1:1 — one app has
	// one service, and a supplier list only means anything relative to a service.
	// A global list would be actively wrong once a second app is configured: the
	// suppliers serving service A do not serve service B, so allowing A's would
	// leave B with nothing to relay to.
	Suppliers SupplierPolicy `yaml:"suppliers"`
}

// SupplierPolicy is a static allow/deny list of supplier operator addresses
// (pokt1…), applied before selection.
//
// This is NOT the QoS/reputation machinery CLAUDE.md bans from this repo. It
// keeps no state, measures nothing, and learns nothing — it is the operator
// saying which suppliers they are willing to pay, which is exactly the kind of
// control a self-hosted access point exists to give. Anything that scores
// suppliers from observed behaviour still belongs in SAGE.
//
// Deny is applied first and wins: an address in both lists is denied.
//
// The *_hosts lists are the second dimension: they name the relay-miner host an
// endpoint answers on, and therefore every supplier behind it — which an address
// list cannot express, because that set is session-scoped. Both dimensions apply
// and both must permit a supplier. See domain.SupplierPolicy.
type SupplierPolicy struct {
	// Allow, when non-empty, is exhaustive — every supplier not named is dropped.
	// ⚠️ A one-entry allowlist therefore removes failover: there is no second
	// supplier left to try when it is down.
	Allow []string `yaml:"allow"`
	// Deny removes suppliers from whatever remains. Empty means deny nothing.
	Deny []string `yaml:"deny"`

	// AllowHosts, when non-empty, is exhaustive over endpoint hosts. DenyHosts
	// removes every supplier answering on the named hosts. Entries are bare hosts
	// with an optional port and an optional leading "*." — never URLs.
	AllowHosts []string `yaml:"allow_hosts"`
	DenyHosts  []string `yaml:"deny_hosts"`
}

// FullNode is the Shannon full node the proxy queries. Two transports are
// needed: gRPC (session/app/account/params) and CometBFT RPC (block height).
type FullNode struct {
	GRPCHostPort string `yaml:"grpc_host_port"`
	GRPCInsecure bool   `yaml:"grpc_insecure"`
	RPCURL       string `yaml:"rpc_url"`
}

// Listener binds a local address to one (service, rpcType). A client points its
// RPC_URL at Addr; the proxy stamps the Rpc-Type header for the miner.
type Listener struct {
	Addr string `yaml:"addr"`

	// ServiceID is optional when exactly one app is configured: that app has
	// exactly one service (see App.ServiceID), so there is nothing to choose.
	// With several apps it is required — the port has to say which one it fronts.
	ServiceID string `yaml:"service_id"`

	RPCType string `yaml:"rpc_type"`

	// AllowedOrigins allowlists browser Origin headers. Applies to EVERY rpc
	// type, not just websocket.
	//
	// Empty (the default) rejects every browser origin while still allowing
	// native clients — node, curl, go — which send no Origin at all. That
	// matters: this proxy is unauthenticated and holds the app's private key, and
	// CORS does not save you. CORS stops a page READING a cross-origin response;
	// it does not stop the request being SENT, and the relay is billed to your
	// stake either way. See transport/cors.go.
	//
	// "*" disables the check entirely; do not use it to silence an error.
	AllowedOrigins []string `yaml:"allowed_origins"`

	// AllowedHosts allowlists Host header values, which is what closes DNS
	// rebinding: a rebound page is same-origin to the browser, so a GET carries
	// no Origin and the allowlist above never runs — but the browser still sends
	// the name it thinks it dialled.
	//
	// Empty derives from Addr. Bound to loopback (the default, and the only case
	// rebinding can attack) it answers to localhost only. Bound wider it is not
	// checked: you exposed it deliberately, and Docker service names or LAN IPs
	// cannot be guessed. "*" disables the check.
	AllowedHosts []string `yaml:"allowed_hosts"`

	// MaxConnections caps concurrent WebSocket connections on this listener.
	// WebSocket only — an HTTP request completes and releases its resources, a
	// WebSocket does not.
	//
	// 0 (the default) means websockets.DefaultMaxConnections, NOT "unlimited":
	// an unbounded count of long-lived connections is the whole failure being
	// prevented, so it must not be what saying nothing gets you. Negative
	// disables the cap.
	MaxConnections int `yaml:"max_connections"`
}

// Parsed returns the listener's ServiceID and validated RPCType.
func (l Listener) Parsed() (domain.ServiceID, domain.RPCType, error) {
	t, err := domain.ParseRPCType(l.RPCType)
	if err != nil {
		return "", domain.RPCTypeUnknown, fmt.Errorf("listener %s: %w", l.Addr, err)
	}
	return domain.ServiceID(l.ServiceID), t, nil
}

// Load reads and validates the config at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // unknown fields error at load time (SAGE convention)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	cfg.applyKeyEnv()

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyKeyEnv fills in keys from the environment, so neither form has to keep a
// private key in a file. The env is a fallback, never an override: a key that IS
// in the config wins, and POCKET_APP_PRIVATE_KEYS is read only when the config
// named no app at all — otherwise setting it globally would silently add apps to
// every config on the machine.
func (c *Config) applyKeyEnv() {
	if len(c.Apps) > 0 || c.App.PrivateKeyHex != "" {
		return
	}
	if keys := os.Getenv("POCKET_APP_PRIVATE_KEYS"); keys != "" {
		for _, k := range strings.Split(keys, ",") {
			if k = strings.TrimSpace(k); k != "" {
				c.Apps = append(c.Apps, App{PrivateKeyHex: k})
			}
		}
		return
	}
	c.App.PrivateKeyHex = os.Getenv("POCKET_APP_PRIVATE_KEY")
}

// AppList normalizes the two config forms into one list. Everything downstream
// works from this, so nothing else has to know that "app:" and "apps:" exist.
func (c *Config) AppList() []App {
	if len(c.Apps) > 0 {
		return c.Apps
	}
	if c.App.PrivateKeyHex != "" {
		return []App{c.App}
	}
	return nil
}

func (c *Config) validate() error {
	if len(c.Apps) > 0 && c.App.PrivateKeyHex != "" {
		return fmt.Errorf("config: set either app or apps, not both — with both, which key signs a relay would depend on load order")
	}
	if len(c.AppList()) == 0 {
		return fmt.Errorf("config: no app key — set app.private_key_hex, apps[].private_key_hex, POCKET_APP_PRIVATE_KEY or POCKET_APP_PRIVATE_KEYS")
	}
	if err := c.validateApps(); err != nil {
		return err
	}
	if c.FullNode.GRPCHostPort == "" {
		return fmt.Errorf("config: fullnode.grpc_host_port required")
	}
	if c.FullNode.RPCURL == "" {
		return fmt.Errorf("config: fullnode.rpc_url required")
	}
	// Caught here rather than shrugged off at the sender, where a typo would
	// silently mean "auto" and the operator would never learn their pin was
	// ignored — which is the whole reason to pin a framing.
	switch c.GRPCMode {
	case "", "native", "web":
	default:
		return fmt.Errorf("config: grpc_mode %q: want \"\" (auto), \"native\" or \"web\"", c.GRPCMode)
	}
	// Listeners may be empty: "pocket-ap call" needs an app + full node but no
	// front listener. The serve path enforces that it has at least one.
	//
	// service_id is NOT checked here. It is optional (one app => one service, so
	// there is nothing to disambiguate) and resolving it needs the app set, which
	// the serve path assembles.
	for _, l := range c.Listeners {
		if l.Addr == "" {
			return fmt.Errorf("config: listener needs an addr")
		}
		if _, _, err := l.Parsed(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validateApps() error {
	seenKey := map[string]int{}
	for i, a := range c.AppList() {
		if a.PrivateKeyHex == "" {
			return fmt.Errorf("config: apps[%d] has no private_key_hex", i)
		}
		if prev, dup := seenKey[a.PrivateKeyHex]; dup {
			// Same key twice is the same app twice, which would then claim the
			// same service twice. Caught here rather than at service resolution
			// so the error names the config, not the chain.
			return fmt.Errorf("config: apps[%d] repeats the key already used by apps[%d]", i, prev)
		}
		seenKey[a.PrivateKeyHex] = i

		// Both dimensions are checked here, and each rejects the other's content:
		// an address that never matches makes an allowlist drop everything and a
		// denylist do nothing, and a URL in a host list does the same. Silent
		// either way, which is why it fails at startup instead.
		for _, list := range [][]string{a.Suppliers.Allow, a.Suppliers.Deny} {
			for _, addr := range list {
				if err := domain.ValidateSupplierAddr(addr); err != nil {
					return fmt.Errorf("config: apps[%d] supplier %w", i, err)
				}
			}
		}
		for _, list := range [][]string{a.Suppliers.AllowHosts, a.Suppliers.DenyHosts} {
			for _, pattern := range list {
				if err := domain.ValidateHostPattern(pattern); err != nil {
					return fmt.Errorf("config: apps[%d] supplier host %w", i, err)
				}
			}
		}
	}
	return nil
}
