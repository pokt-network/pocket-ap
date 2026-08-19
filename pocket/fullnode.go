// Package pocket holds the Pocket Network implementations of the relay seams
// (full-node clients, response validation, session management, ring signing).
//
// "Pocket" — not "Shannon". Shannon is the current and only live Pocket protocol
// (Morse was sunset); naming the generation only reintroduces the confusion.
// The `// LIFT: SAGE protocol/shannon/...` comments are pointers into the SAGE
// source we port from, not a naming choice here.
package pocket

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/url"
	"sync"

	apptypes "github.com/pokt-network/poktroll/x/application/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	sdk "github.com/pokt-network/shannon-sdk"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// FullNode wraps the Shannon SDK clients needed to send relays: session / app /
// account / shared-params over gRPC, and block height over CometBFT RPC. It is a
// thin, raw wrapper — it returns poktroll types; conversion to domain types
// happens in the session manager and validator.
//
// LIFT: SAGE protocol/shannon/fullnode.go.
type FullNode struct {
	sessionClient *sdk.SessionClient
	blockClient   *sdk.BlockClient
	accountClient *sdk.AccountClient
	appClient     *sdk.ApplicationClient
	sharedClient  *sdk.SharedClient
	logger        *slog.Logger

	// pubKeys is the process-wide public-key cache. It lives here rather than in
	// the Validator because the SIGNER needs the same keys: verification looks up
	// suppliers, ring building looks up the app and its gateways, and with several
	// apps each Signer would otherwise re-fetch a shared gateway's key itself.
	pubKeys     *cachingPubKeyFetcher
	pubKeysOnce sync.Once
}

// nodeClient is the slice of FullNode that SessionManager and Signer actually
// use. *FullNode satisfies it, so this is a field type rather than an API change
// — no caller passes anything different.
//
// It exists because FullNode dials a real chain, which left the session cache,
// rotation, expiry and app-cache logic untestable: ~150 lines that /health and
// the WebSocket expiry watcher both depend on, sitting at 0%.
//
// It deliberately stops here. AccountClient() hands back a concrete
// *sdk.AccountClient, and the ring signing runs through concrete SDK calls;
// faking those would mean building scaffolding around the one thing CLAUDE.md
// says not to touch, and the network verifies it on every relay anyway.
type nodeClient interface {
	GetSession(ctx context.Context, serviceID, appAddr string) (*sessiontypes.Session, error)
	GetApp(ctx context.Context, appAddr string) (*apptypes.Application, error)
	GetCurrentBlockHeight(ctx context.Context) (int64, error)
}

// Compile-time assertion: the real client still fits the seam.
var _ nodeClient = (*FullNode)(nil)

// NewFullNode establishes the gRPC + RPC connections for all required clients.
func NewFullNode(grpcHostPort string, grpcInsecure bool, rpcURL string) (*FullNode, error) {
	logger := slog.Default().With("component", "fullnode")

	blockClient, err := newBlockClient(rpcURL)
	if err != nil {
		return nil, fmt.Errorf("NewFullNode: block client at %s: %w", rpcURL, err)
	}
	sessionClient, err := newSessionClient(grpcHostPort, grpcInsecure)
	if err != nil {
		return nil, fmt.Errorf("NewFullNode: session client at %s: %w", grpcHostPort, err)
	}
	appClient, err := newAppClient(grpcHostPort, grpcInsecure)
	if err != nil {
		return nil, fmt.Errorf("NewFullNode: app client at %s: %w", grpcHostPort, err)
	}
	accountClient, err := newAccountClient(grpcHostPort, grpcInsecure)
	if err != nil {
		return nil, fmt.Errorf("NewFullNode: account client at %s: %w", grpcHostPort, err)
	}
	sharedClient, err := newSharedClient(grpcHostPort, grpcInsecure)
	if err != nil {
		return nil, fmt.Errorf("NewFullNode: shared client at %s: %w", grpcHostPort, err)
	}

	return &FullNode{
		sessionClient: sessionClient,
		blockClient:   blockClient,
		accountClient: accountClient,
		appClient:     appClient,
		sharedClient:  sharedClient,
		logger:        logger,
	}, nil
}

// GetSession fetches the latest session for (serviceID, appAddr). Height 0 asks
// the node for the current session.
func (fn *FullNode) GetSession(ctx context.Context, serviceID, appAddr string) (*sessiontypes.Session, error) {
	session, err := fn.sessionClient.GetSession(ctx, appAddr, serviceID, 0)
	if err != nil {
		return nil, fmt.Errorf("GetSession: service %s app %s: %w", serviceID, appAddr, err)
	}
	if session == nil {
		return nil, fmt.Errorf("GetSession: nil session for service %s app %s", serviceID, appAddr)
	}
	return session, nil
}

// GetApp fetches the onchain application (carries delegatee addresses for ring
// derivation).
func (fn *FullNode) GetApp(ctx context.Context, appAddr string) (*apptypes.Application, error) {
	app, err := fn.appClient.GetApplication(ctx, appAddr)
	if err != nil {
		return nil, fmt.Errorf("GetApp: %s: %w", appAddr, err)
	}
	return &app, nil
}

// GetCurrentBlockHeight returns chain head (drives session rotation).
func (fn *FullNode) GetCurrentBlockHeight(ctx context.Context) (int64, error) {
	height, err := fn.blockClient.LatestBlockHeight(ctx)
	if err != nil {
		return 0, fmt.Errorf("GetCurrentBlockHeight: %w", err)
	}
	return height, nil
}

// GetSharedParams returns shared-module governance params (num_blocks_per_session,
// grace period). Currently unused — session end height ships inside the fetched
// session — but wired for when local rotation math is wanted.
func (fn *FullNode) GetSharedParams(ctx context.Context) (*sharedtypes.Params, error) {
	params, err := fn.sharedClient.GetParams(ctx)
	if err != nil {
		return nil, fmt.Errorf("GetSharedParams: %w", err)
	}
	return &params, nil
}

// AccountClient exposes the raw account client.
//
// Prefer PubKeyFetcher for public keys — this one queries the full node on every
// call. Kept exported because it is the account module, not just a key lookup.
func (fn *FullNode) AccountClient() *sdk.AccountClient { return fn.accountClient }

// PubKeyFetcher returns the shared, caching public-key fetcher. Every component
// that needs a key must go through this: the raw account client above is a gRPC
// round trip per call, which on the WebSocket path means one per FRAME.
//
// Built lazily so a zero-value &FullNode{} — which tests construct to exercise
// key derivation without a chain — is usable rather than handing out a nil.
func (fn *FullNode) PubKeyFetcher() sdk.PublicKeyFetcher {
	fn.pubKeysOnce.Do(func() { fn.pubKeys = newCachingPubKeyFetcher(fn.accountClient) })
	return fn.pubKeys
}

// --- client construction ---

func connectGRPC(hostPort string, insecureConn bool) (*grpc.ClientConn, error) {
	if insecureConn {
		return grpc.NewClient(hostPort, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	// MinVersion is set explicitly rather than left to the Go default. crypto/tls
	// already floors a client at 1.2, so this changes nothing today — it pins the
	// floor so it cannot drift with a toolchain bump, on the one dial function
	// both the full-node clients (session, app, account, shared) and the supplier
	// gRPC sender go through. A single site is the reason it is worth doing here:
	// two connections in one binary disagreeing about their floor is exactly the
	// kind of thing nobody notices.
	//
	// LIFT: SAGE protocol/shannon/fullnode.go (2f061bd, audit item S3).
	return grpc.NewClient(hostPort, grpc.WithTransportCredentials(
		credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12}),
	))
}

func newSessionClient(hostPort string, insecureConn bool) (*sdk.SessionClient, error) {
	conn, err := connectGRPC(hostPort, insecureConn)
	if err != nil {
		return nil, err
	}
	return &sdk.SessionClient{PoktNodeSessionFetcher: sdk.NewPoktNodeSessionFetcher(conn)}, nil
}

func newBlockClient(rpcURL string) (*sdk.BlockClient, error) {
	if _, err := url.Parse(rpcURL); err != nil {
		return nil, fmt.Errorf("invalid rpc url %s: %w", rpcURL, err)
	}
	fetcher, err := sdk.NewPoktNodeStatusFetcher(rpcURL)
	if err != nil {
		return nil, fmt.Errorf("status fetcher for %s: %w", rpcURL, err)
	}
	return &sdk.BlockClient{PoktNodeStatusFetcher: fetcher}, nil
}

func newAppClient(hostPort string, insecureConn bool) (*sdk.ApplicationClient, error) {
	conn, err := connectGRPC(hostPort, insecureConn)
	if err != nil {
		return nil, err
	}
	return &sdk.ApplicationClient{QueryClient: apptypes.NewQueryClient(conn)}, nil
}

func newAccountClient(hostPort string, insecureConn bool) (*sdk.AccountClient, error) {
	conn, err := connectGRPC(hostPort, insecureConn)
	if err != nil {
		return nil, err
	}
	return &sdk.AccountClient{PoktNodeAccountFetcher: sdk.NewPoktNodeAccountFetcher(conn)}, nil
}

func newSharedClient(hostPort string, insecureConn bool) (*sdk.SharedClient, error) {
	conn, err := connectGRPC(hostPort, insecureConn)
	if err != nil {
		return nil, err
	}
	return &sdk.SharedClient{QueryClient: sharedtypes.NewQueryClient(conn)}, nil
}
