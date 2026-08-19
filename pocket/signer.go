package pocket

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/pokt-network/poktroll/pkg/crypto/rings"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	ring "github.com/pokt-network/ring-go"
	sdk "github.com/pokt-network/shannon-sdk"
	sdktypes "github.com/pokt-network/shannon-sdk/types"

	"github.com/pokt-network/pocket-ap/domain"
	"github.com/pokt-network/pocket-ap/relay"
)

// Compile-time assertion: Signer satisfies the Signer seam.
var _ relay.Signer = (*Signer)(nil)

// ringCacheKey uniquely identifies a cached ring by app address and session end
// height (ring composition only changes at session boundaries).
type ringCacheKey struct {
	appAddress       string
	sessionEndHeight uint64
}

// Signer builds and ring-signs Shannon RelayRequests off-chain. DO NOT
// reimplement the crypto — shannon-sdk + poktroll/pkg/crypto/rings + ring-go do
// it, and the signature is verified by the network.
//
// LIFT (near-verbatim): SAGE protocol/shannon/signer.go (ring build/cache/sign),
// the request-building half of relayer.go SendRelay (:166-215), and the key->addr
// derivation from apps.go:58.
// ⚠️ NEVER %+v / %#v THIS STRUCT, or anything embedding it.
//
// sdk.Signer keeps the raw key in an EXPORTED field —
// `PrivateKeyHex string // retained for compatibility and debugging`
// (shannon-sdk signer.go:27) — so any struct dump of Signer, or of something
// holding one, prints the app's private key straight into the logs. Nothing does
// today; keep it that way.
type Signer struct {
	sdkSigner *sdk.Signer
	// pubKeys serves the ring members' keys. It is the same shared cache the
	// response path verifies against, and it sits UNDER ringCache: a ring is
	// discarded every session because its composition may change, but the keys of
	// the addresses in it cannot, so re-fetching them at every rollover is waste.
	pubKeys sdk.PublicKeyFetcher
	appAddr string
	fn      nodeClient
	logger  *slog.Logger

	ringCache         sync.Map // ringCacheKey -> *ring.Ring
	appCache          sync.Map // appAddr -> *apptypes.Application
	highestSessionEnd atomic.Uint64
	rolloverMu        sync.Mutex
}

// NewSigner creates the SDK signer, derives the app address from the private key
// (secp256k1 -> bech32 "pokt"), and borrows the full node's account client for
// pubkey fetching.
func NewSigner(privateKeyHex string, fn *FullNode) (*Signer, error) {
	sdkSigner, err := sdk.NewSignerFromHex(privateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("NewSigner: create SDK signer: %w", err)
	}

	appAddr, err := deriveAppAddr(privateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("NewSigner: %w", err)
	}

	return &Signer{
		sdkSigner: sdkSigner,
		pubKeys:   fn.PubKeyFetcher(),
		appAddr:   appAddr,
		fn:        fn,
		logger:    slog.Default().With("component", "signer"),
	}, nil
}

// AppAddr returns the bech32 app address derived from the signing key.
func (s *Signer) AppAddr() string { return s.appAddr }

// secp256k1KeyLen is the exact byte length of a secp256k1 private key.
const secp256k1KeyLen = 32

// deriveAppAddr computes the bech32 app address for a hex secp256k1 private key.
//
// This derivation IS the sovereign-app-key model — the app identity comes from
// the key, never from config — so a wrong key does not fail, it makes us a
// different app.
//
// The length check is not ceremony. Without it every one of these returns a
// plausible pokt1… address and no error:
//   - "" -> pokt1xcjufgh…
//   - a truncated paste -> some other address
//   - 33 bytes -> the SAME address as 32, silently ignoring the extra byte
//
// The failure that produces is horrible to debug: the proxy starts, derives an
// address for an app that does not exist or is not yours, and every relay comes
// back "session not found" while the key looks fine in the config.
//
// LIFT: apps.go:58 buildOwnedApps (which does not check the length either).
func deriveAppAddr(privateKeyHex string) (string, error) {
	privKeyBz, err := hex.DecodeString(privateKeyHex)
	if err != nil {
		return "", fmt.Errorf("deriveAppAddr: invalid hex key: %w", err)
	}
	if len(privKeyBz) != secp256k1KeyLen {
		return "", fmt.Errorf("deriveAppAddr: key is %d bytes, want %d — a wrong-length key derives a valid-looking address for the wrong app",
			len(privKeyBz), secp256k1KeyLen)
	}
	privKey := &secp256k1.PrivKey{Key: privKeyBz}
	appAddr, err := bech32.ConvertAndEncode("pokt", privKey.PubKey().Address())
	if err != nil {
		return "", fmt.Errorf("deriveAppAddr: encode address: %w", err)
	}
	return appAddr, nil
}

// buildTargetURL joins the supplier's base URL with the inbound request path,
// preserving path and query so REST and CometBFT reach the right backend route.
// SAGE hard-codes POST to the bare supplier URL because it is JSON-RPC-centric;
// a multi-type proxy must not.
//
// The leading slash is normalized rather than assumed: requests arriving through
// transport.HTTP carry r.URL.RequestURI(), which always has one, but "call"
// takes -path straight from a human.
func buildTargetURL(base, path string) string {
	if path == "" || path == "/" {
		return base
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return strings.TrimRight(base, "/") + path
}

// SignRelay builds the relay payload from the inbound request, wraps it in a
// RelayRequest for the endpoint, ring-signs it, and returns the wire bytes.
// LIFT: relayer.go:166-215 (build) + signer.go:58 (sign).
func (s *Signer) SignRelay(ctx context.Context, session *domain.Session, endpoint domain.Endpoint, rpcType domain.RPCType, in domain.RelayInput) ([]byte, error) {
	base, ok := endpoint.URL(rpcType)
	if !ok {
		return nil, domain.ErrNoEndpoint
	}

	httpReq, err := http.NewRequestWithContext(ctx, in.Method, buildTargetURL(base, in.Path), bytes.NewReader(in.Body))
	if err != nil {
		return nil, fmt.Errorf("SignRelay: build http request: %w", err)
	}
	for k, vs := range in.Header {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	if httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	// Serialize the HTTP request into the relay payload (opaque to the network).
	_, payloadBz, err := sdktypes.SerializeHTTPRequest(httpReq)
	if err != nil {
		return nil, fmt.Errorf("SignRelay: serialize http request: %w", err)
	}

	raw, ok := session.Raw.(*sessiontypes.Session)
	if !ok || raw.Header == nil {
		return nil, fmt.Errorf("SignRelay: session missing raw header")
	}

	unsignedReq := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader:           raw.Header,
			SupplierOperatorAddress: string(endpoint.Supplier),
		},
		Payload: payloadBz,
	}

	app, err := s.getApp(ctx, session.AppAddr)
	if err != nil {
		return nil, fmt.Errorf("SignRelay: fetch app for signing: %w", err)
	}

	signed, err := s.signRelayRequest(ctx, unsignedReq, app)
	if err != nil {
		return nil, err
	}

	reqBz, err := signed.Marshal()
	if err != nil {
		return nil, fmt.Errorf("SignRelay: marshal signed request: %w", err)
	}
	return reqBz, nil
}

// getApp returns a cached Application, fetching from the full node on first use.
// LIFT: apps.go:34 getApp.
func (s *Signer) getApp(ctx context.Context, appAddr string) (*apptypes.Application, error) {
	if cached, ok := s.appCache.Load(appAddr); ok {
		return cached.(*apptypes.Application), nil
	}
	app, err := s.fn.GetApp(ctx, appAddr)
	if err != nil {
		return nil, err
	}
	s.appCache.Store(appAddr, app)
	return app, nil
}

// signRelayRequest signs the request with the application's ring signature.
// LIFT: signer.go:58 signRelayRequest.
func (s *Signer) signRelayRequest(ctx context.Context, unsignedReq *servicetypes.RelayRequest, app *apptypes.Application) (*servicetypes.RelayRequest, error) {
	sessionEndHeight := uint64(unsignedReq.Meta.SessionHeader.SessionEndBlockHeight)

	appRing, err := s.getOrCreateRing(ctx, app, sessionEndHeight)
	if err != nil {
		return nil, fmt.Errorf("signRelayRequest: get ring for app %s: %w", app.Address, err)
	}

	// SignOffChainWithRing uses ring-go's hash-cache fast path. Safe here: relay
	// signing is off-chain / not consensus-critical, and the signature still
	// verifies identically at the relay miner.
	signed, err := s.sdkSigner.SignOffChainWithRing(ctx, unsignedReq, appRing)
	if err != nil {
		return nil, fmt.Errorf("signRelayRequest: sign: %w", err)
	}
	return signed, nil
}

// getOrCreateRing returns a cached ring for (appAddress, sessionEndHeight) or
// builds one from the app plus its delegated gateways.
// LIFT: signer.go:106 getOrCreateRing.
func (s *Signer) getOrCreateRing(ctx context.Context, app *apptypes.Application, sessionEndHeight uint64) (*ring.Ring, error) {
	s.evictStaleRingsOnRollover(sessionEndHeight)

	key := ringCacheKey{appAddress: app.Address, sessionEndHeight: sessionEndHeight}
	if cached, ok := s.ringCache.Load(key); ok {
		return cached.(*ring.Ring), nil
	}

	// Ring = app + delegated gateways (self-delegation => app twice).
	gatewayAddresses := rings.GetRingAddressesAtSessionEndHeight(app, sessionEndHeight)
	ringAddresses := make([]string, 0, 1+len(gatewayAddresses))
	ringAddresses = append(ringAddresses, app.Address)
	if len(gatewayAddresses) == 0 {
		ringAddresses = append(ringAddresses, app.Address)
	} else {
		ringAddresses = append(ringAddresses, gatewayAddresses...)
	}

	// Served from memory after each address is first seen. That is what stops a
	// session rollover from re-querying the full node for keys that cannot have
	// changed: ringCache is keyed by session end height, so it misses at EVERY
	// boundary (minutes on beta, ~20 on mainnet), and with no singleflight every
	// in-flight relay for this app misses at once and each one queried.
	pubKeys := make([]cryptotypes.PubKey, 0, len(ringAddresses))
	for _, addr := range ringAddresses {
		pubKey, err := s.pubKeys.GetPubKeyFromAddress(ctx, addr)
		if err != nil {
			return nil, fmt.Errorf("getOrCreateRing: pubkey for %s: %w", addr, err)
		}
		if pubKey == nil {
			// "No key onchain" arrives as (nil, nil) — see cachingPubKeyFetcher.
			// Caught here because rings.GetRingFromPubKeys would otherwise fail
			// somewhere far less legible, and because a ring member without a key
			// blocks signing for the whole app rather than one supplier. Nothing
			// nil is cached, so the next attempt re-asks the chain — no
			// invalidation machinery needed.
			return nil, fmt.Errorf("getOrCreateRing: no onchain public key for ring address %s — this app, or a gateway it is delegated to, has never signed a transaction", addr)
		}
		pubKeys = append(pubKeys, pubKey)
	}

	newRing, err := rings.GetRingFromPubKeys(pubKeys)
	if err != nil {
		return nil, fmt.Errorf("getOrCreateRing: build ring: %w", err)
	}

	actual, _ := s.ringCache.LoadOrStore(key, newRing)
	return actual.(*ring.Ring), nil
}

// evictStaleRingsOnRollover bounds the per-session ring caches on session
// rollover (keeps current + previous), and clears the SDK's SignerContext cache.
// LIFT: signer.go:170 evictStaleRingsOnRollover.
func (s *Signer) evictStaleRingsOnRollover(sessionEndHeight uint64) {
	if sessionEndHeight <= s.highestSessionEnd.Load() {
		return
	}

	s.rolloverMu.Lock()
	defer s.rolloverMu.Unlock()

	prevHighest := s.highestSessionEnd.Load()
	if sessionEndHeight <= prevHighest {
		return
	}
	s.highestSessionEnd.Store(sessionEndHeight)

	s.ringCache.Range(func(k, _ any) bool {
		if k.(ringCacheKey).sessionEndHeight < prevHighest {
			s.ringCache.Delete(k)
		}
		return true
	})
	s.sdkSigner.ClearSignerContextCache()
}
