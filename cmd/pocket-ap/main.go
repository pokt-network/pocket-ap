// Command pocket-ap is a self-hosted, drop-in RPC proxy for Pocket Network's
// Shannon protocol. Stake an app, run this, point your RPC_URL at a local
// listener — your traffic is signed and relayed straight to suppliers, no
// gateway middleman.
//
// Two modes share the same config and the same relay core:
//
//	pocket-ap serve -config c.yaml       # run the front listeners
//	pocket-ap call  -config c.yaml ...   # one-shot relay, print the response
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pokt-network/pocket-ap/config"
	"github.com/pokt-network/pocket-ap/domain"
	"github.com/pokt-network/pocket-ap/health"
	"github.com/pokt-network/pocket-ap/internal/safego"
	"github.com/pokt-network/pocket-ap/pocket"
	"github.com/pokt-network/pocket-ap/relay"
	"github.com/pokt-network/pocket-ap/selector"
	"github.com/pokt-network/pocket-ap/transport"
)

// Build info, stamped at release time via -ldflags. The names are goreleaser's
// defaults (main.version/commit/date), so its zero-config build sets them.
//
// A build with no stamp says "dev", which is honest: a colleague reporting a bug
// from a `go build` should not look like they are on a release.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// defaultMaxAttempts caps relay failover: try up to this many suppliers before
// giving up. Shared by both modes so "call" reproduces "serve" behaviour.
const defaultMaxAttempts = 3

const usage = `pocket-ap — self-hosted Pocket Network RPC proxy

usage:
  pocket-ap serve --config <path>           run the front listeners (default)
  pocket-ap call  --config <path> [flags]   one-shot relay, print the response
  pocket-ap version                         print build info

run "pocket-ap <command> --help" for a command's flags.
one dash or two both work: --config and -config are the same flag.
`

// dispatch maps a command line to a subcommand and its remaining arguments.
//
// Split out of main so the routing can be tested: every entry point below is one
// somebody actually types, and three of them used to land somewhere unhelpful.
func dispatch(argv []string) (cmd string, args []string) {
	cmd, args = "serve", argv
	// A leading flag rather than a verb keeps the original bare
	// "pocket-ap -config x" invocation working.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	if len(argv) == 0 {
		// A bare invocation is someone finding out what this is, not someone who
		// meant to serve with no config. It used to reach runServe and fail with
		// `config: read : open : no such file or directory` — an empty path in a
		// message about a file, which reads as a bug in the program rather than
		// as a missing argument.
		return "help-exit2", nil
	}
	switch argv[0] {
	// -version/--version: what everyone types, and it would otherwise reach the
	// serve flagset and die as an unknown flag.
	case "-version", "--version":
		return "version", nil
	// Help is the same trap and a worse one: without this the leading-flag rule
	// sends -h to the SERVE flagset, which answers "Usage of serve:" and two
	// flags — so the most common way to ask what a program does is the one way
	// that hides `call` and `version` entirely.
	case "-h", "-help", "--help":
		return "help", nil
	}
	return cmd, args
}

func main() {
	cmd, args := dispatch(os.Args[1:])

	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "call":
		err = runCall(args)
	case "version":
		fmt.Println(versionString())
		return
	case "help":
		// stdout: an explicit request for help is answered, not diagnosed, and
		// `pocket-ap help | less` should show something.
		fmt.Print(usage)
		return
	case "help-exit2":
		// stderr and non-zero: nothing was asked for, so this is a diagnostic.
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// versionString reports the build. First thing to ask for in a bug report, so it
// carries the commit too — a version alone cannot identify a build from a fork
// or an unreleased branch.
func versionString() string {
	return fmt.Sprintf("pocket-ap %s (commit %s, built %s, %s/%s, %s)",
		version, commit, date, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// resolvedApp is one configured app after startup has answered the two questions
// config alone cannot: what address does this key derive, and what service is
// that app staked for.
type resolvedApp struct {
	signer    *pocket.Signer
	addr      string
	serviceID domain.ServiceID
	suppliers selector.Policy
}

// buildCore constructs the pieces every mode needs: the full-node clients, one
// signer per configured app, and the signer that routes between them.
//
// Services are NOT resolved here — that needs the network, and the two modes
// want different failure behaviour for it. See resolveApps.
func buildCore(cfg *config.Config) (*pocket.FullNode, []*pocket.Signer, *pocket.MultiSigner, error) {
	fn, err := pocket.NewFullNode(cfg.FullNode.GRPCHostPort, cfg.FullNode.GRPCInsecure, cfg.FullNode.RPCURL)
	if err != nil {
		return nil, nil, nil, err
	}

	apps := cfg.AppList()
	signers := make([]*pocket.Signer, 0, len(apps))
	seen := map[string]int{}
	for i, a := range apps {
		signer, err := pocket.NewSigner(a.PrivateKeyHex, fn)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("app %d: %w", i, err)
		}
		// Two different keys deriving one address is not possible; two config
		// entries for the same app is (different keys are checked in config, but
		// the same key reached by different routes is not). Catch it here, where
		// the address is known.
		if prev, dup := seen[signer.AppAddr()]; dup {
			return nil, nil, nil, fmt.Errorf("apps %d and %d are the same app (%s)", prev, i, signer.AppAddr())
		}
		seen[signer.AppAddr()] = i
		signers = append(signers, signer)
	}
	return fn, signers, pocket.NewMultiSigner(signers...), nil
}

// resolveApps pairs each signer with the service its app is staked for.
//
// The service is read from the chain, because poktroll allows an app exactly one
// service and so the key already determines it (pocket.Signer.ServiceID). A
// configured service_id is treated as an assertion to check rather than the
// source of truth: agreeing costs nothing, and disagreeing means the operator
// believes this key is a different app than it is — which is the failure mode
// deriveAppAddr exists to make loud, so it is fatal here too.
//
// When service_id IS configured, a discovery failure is not fatal: we already
// have what we need, and refusing to start because the full node is briefly
// unreachable would be a regression — nothing else in startup requires the node
// to answer.
func resolveApps(ctx context.Context, cfg *config.Config, signers []*pocket.Signer, logger *slog.Logger) ([]resolvedApp, error) {
	configured := cfg.AppList()
	out := make([]resolvedApp, 0, len(signers))

	for i, signer := range signers {
		want := domain.ServiceID(configured[i].ServiceID)

		got, err := signer.ServiceID(ctx)
		switch {
		case err != nil && want == "":
			return nil, fmt.Errorf("app %s: cannot determine which service it is staked for: %w (set apps[%d].service_id to skip the lookup)", signer.AppAddr(), err, i)
		case err != nil:
			logger.Warn("could not verify the configured service against the chain",
				"app", signer.AppAddr(), "service_id", want, "error", err)
		case want != "" && got != want:
			return nil, fmt.Errorf("app %s is staked for service %q, but the config says %q — one of them is wrong, and relaying would fail every request with \"session not found\"",
				signer.AppAddr(), got, want)
		case want == "":
			logger.Info("discovered service from the app key", "app", signer.AppAddr(), "service_id", got)
			want = got
		}

		out = append(out, resolvedApp{
			signer:    signer,
			addr:      signer.AppAddr(),
			serviceID: want,
			suppliers: supplierPolicy(configured[i].Suppliers),
		})
	}
	return out, nil
}

// supplierPolicy converts the config form into the selector's.
func supplierPolicy(p config.SupplierPolicy) selector.Policy {
	out := selector.Policy{
		Allow:      make([]domain.EndpointAddr, 0, len(p.Allow)),
		Deny:       make([]domain.EndpointAddr, 0, len(p.Deny)),
		AllowHosts: p.AllowHosts,
		DenyHosts:  p.DenyHosts,
	}
	for _, a := range p.Allow {
		out.Allow = append(out.Allow, domain.EndpointAddr(a))
	}
	for _, d := range p.Deny {
		out.Deny = append(out.Deny, domain.EndpointAddr(d))
	}
	return out
}

// serviceApps and supplierPolicies project the resolved set into what the
// session manager and the selector each need.
func serviceApps(apps []resolvedApp) []pocket.ServiceApp {
	out := make([]pocket.ServiceApp, 0, len(apps))
	for _, a := range apps {
		out = append(out, pocket.ServiceApp{ServiceID: a.serviceID, AppAddr: a.addr})
	}
	return out
}

func supplierPolicies(apps []resolvedApp) map[domain.ServiceID]selector.Policy {
	out := make(map[domain.ServiceID]selector.Policy, len(apps))
	for _, a := range apps {
		if !a.suppliers.Empty() {
			out[a.serviceID] = a.suppliers
		}
	}
	return out
}

// orEnv falls back to an environment variable when a flag was not passed.
func orEnv(v, key string) string {
	if v != "" {
		return v
	}
	return os.Getenv(key)
}

// applyNetworkFlag handles -network for both subcommands.
//
// It logs at WARN for a network whose relays cost real value. That is the one
// mistake this flag makes easy — switching is now a word, so the word has to say
// what it costs — and it is a log line rather than a prompt because a proxy is
// something people run from systemd, where a prompt is a hang.
func applyNetworkFlag(cfg *config.Config, name string) error {
	if name == "" {
		return nil
	}
	n, err := cfg.SetNetwork(name)
	if err != nil {
		return err
	}
	if n.SpendsRealValue {
		slog.Warn("network selected: every relay spends real POKT from your app's stake",
			"network", name, "chain_id", n.ChainID, "grpc", n.GRPCHostPort)
	} else {
		slog.Info("network selected", "network", name, "chain_id", n.ChainID, "grpc", n.GRPCHostPort)
	}
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "pocket-ap serve — run the front listeners\n\nflags:\n")
		printFlags(fs, os.Stderr)
	}
	configPath := fs.String("config", "", "path to config YAML (or set POCKET_AP_CONFIG)")
	network := fs.String("network", "", "point the full node at a named network ("+strings.Join(config.NetworkNames(), "|")+"), overriding the config")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(orEnv(*configPath, "POCKET_AP_CONFIG"))
	if err != nil {
		return err
	}
	if err := applyNetworkFlag(cfg, *network); err != nil {
		return err
	}
	if len(cfg.Listeners) == 0 {
		return fmt.Errorf("config: serve needs at least one listener")
	}

	// --- lifecycle (early: resolving apps reaches the full node) ---
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- build the Shannon core (seams) ---
	fn, signers, signer, err := buildCore(cfg)
	if err != nil {
		return err
	}

	apps, err := resolveApps(ctx, cfg, signers, logger)
	if err != nil {
		return err
	}
	if err := resolveListenerServices(cfg, apps); err != nil {
		return err
	}

	sessions, err := pocket.NewSessionManager(fn, serviceApps(apps))
	if err != nil {
		return err
	}

	// In-memory only, for this process: counters reset on restart, nothing is
	// written or exported. Attached via WithObservers rather than by making the
	// Selector observe, so a QoS selector can still take that seat later.
	stats := health.NewStats(time.Now())

	validator := pocket.NewValidator(fn)
	// One Selector instance, shared by both flows, so anything observing the
	// outcome feed sees stateless and stateful relays alike.
	//
	// The supplier allow/deny Filter goes INSIDE WithObservers, not around it:
	// the Relayer finds its Observer by type-asserting the Selector, and Filter
	// is not one — wrapping the other way round would silently stop /health
	// counting.
	sel := relay.WithObservers(
		selector.Filter{Inner: selector.Random{}, Policies: supplierPolicies(apps)},
		stats,
	)

	// One sender covering every RPC type: gRPC relays reach the miner over gRPC
	// (its trailer folding only happens on that path), everything else over HTTP.
	grpcSender := pocket.NewGRPCSenderMode(cfg.GRPCMode)
	defer func() { _ = grpcSender.Close() }()
	sender := pocket.NewMultiSender(pocket.NewHTTPSender(30*time.Second), grpcSender)
	relayer := &relay.Relayer{
		Sessions:  sessions,
		Signer:    signer,
		Validator: validator,
		Selector:  sel,
		Sender:    sender,
		// Same object: SendStream is the streaming half of the same sender. The
		// HTTP front always streams, because only the response can say whether it
		// is a stream, and a non-streaming response is simply one batch.
		StreamSender: sender,
		MaxAttempts:  defaultMaxAttempts,
	}

	// The stateful flow: same seams, different lifecycle. Signer and Validator
	// are the same objects — they carry both the HTTP and the raw-frame paths.
	bridge := &relay.Bridge{
		Sessions:      sessions,
		Signer:        signer,
		Validator:     validator,
		Selector:      sel,
		RPCTypeHeader: pocket.RPCTypeHeader,
	}

	if err := sessions.Start(ctx); err != nil {
		return err
	}

	// --- front listeners (one per configured listener) ---
	var transports []transport.Transport
	for _, l := range cfg.Listeners {
		serviceID, rpcType, err := l.Parsed()
		if err != nil {
			return err
		}
		transports = append(transports, transport.New(transport.Options{
			Addr:           l.Addr,
			ServiceID:      serviceID,
			RPCType:        rpcType,
			Relay:          relayer.Relay,
			Stream:         relayer.RelayStream,
			Prepare:        bridge.Prepare,
			ChainHeight:    sessions.LatestBlockHeight,
			AllowedOrigins: l.AllowedOrigins,
			AllowedHosts:   l.AllowedHosts,
			MaxConnections: l.MaxConnections,
		}))
	}

	var wg sync.WaitGroup
	for _, t := range transports {
		wg.Add(1)
		go func(t transport.Transport) {
			// Listed first so it runs last: defers unwind LIFO, and a panic raised
			// by wg.Done() itself still has to be contained.
			defer safego.Recover(logger, "cmd.transport.serve")
			defer wg.Done()
			if err := t.Serve(ctx); err != nil {
				logger.Error("transport stopped", "rpc_type", t.RPCType().String(), "error", err)
			}
		}(t)
	}

	// --- admin listener (opt-in: only when admin.addr is set) ---
	var admin *health.Server
	if cfg.Admin.Addr != "" {
		admin = health.New(cfg.Admin.Addr, appInfos(apps), sessions, stats)
		wg.Add(1)
		go func() {
			defer safego.Recover(logger, "cmd.admin.serve")
			defer wg.Done()
			if err := admin.Serve(ctx); err != nil {
				logger.Error("health endpoint stopped", "error", err)
			}
		}()
	}

	logger.Info("pocket-ap up", "listeners", len(transports), "apps", len(apps),
		"services", sessions.Services(), "admin_addr", cfg.Admin.Addr)
	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, t := range transports {
		_ = t.Close(shutdownCtx)
	}
	if admin != nil {
		_ = admin.Close(shutdownCtx)
	}
	wg.Wait()
	return nil
}

// resolveListenerServices fills in each listener's service_id and rejects the
// ones that name a service no configured app can pay for.
//
// Omitting service_id is allowed only with a single app, and that is not a
// convenience shortcut — with one app there is exactly one service it could
// possibly mean (an app stakes for exactly one), so requiring it would be asking
// the operator to restate a fact with no alternative. With several apps the port
// genuinely has to say which one it fronts.
//
// The unknown-service check is what turns a typo from a runtime mystery into a
// startup error: without it the listener binds, accepts traffic, and fails every
// relay at session fetch.
func resolveListenerServices(cfg *config.Config, apps []resolvedApp) error {
	known := make(map[domain.ServiceID]struct{}, len(apps))
	for _, a := range apps {
		known[a.serviceID] = struct{}{}
	}

	for i := range cfg.Listeners {
		l := &cfg.Listeners[i]
		if l.ServiceID == "" {
			if len(apps) != 1 {
				return fmt.Errorf("listener %s: service_id is required — %d apps are configured, so there is no unambiguous default", l.Addr, len(apps))
			}
			l.ServiceID = string(apps[0].serviceID)
			continue
		}
		if _, ok := known[domain.ServiceID(l.ServiceID)]; !ok {
			return fmt.Errorf("listener %s: no configured app is staked for service %q (configured: %v)", l.Addr, l.ServiceID, serviceIDs(apps))
		}
	}
	return nil
}

func serviceIDs(apps []resolvedApp) []domain.ServiceID {
	out := make([]domain.ServiceID, 0, len(apps))
	for _, a := range apps {
		out = append(out, a.serviceID)
	}
	return out
}

func appInfos(apps []resolvedApp) []health.AppInfo {
	out := make([]health.AppInfo, 0, len(apps))
	for _, a := range apps {
		out = append(out, health.AppInfo{Address: a.addr, ServiceID: a.serviceID})
	}
	return out
}
