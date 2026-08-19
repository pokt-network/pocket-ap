package pocket

import (
	"context"
	"sync"

	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	sdk "github.com/pokt-network/shannon-sdk"
)

// Compile-time assertion: this is what sdk.ValidateRelayResponse wants.
var _ sdk.PublicKeyFetcher = (*cachingPubKeyFetcher)(nil)

// cachingPubKeyFetcher memoizes supplier public keys.
//
// WHY: sdk.ValidateRelayResponse fetches the supplier's public key to verify
// every signature, and sdk.AccountClient.GetPubKeyFromAddress (account.go:54) is
// a bare gRPC query with no caching — it hits the full node every single time.
// So each relay paid TWO network round trips: one to the supplier, and one to
// the full node purely to re-fetch a key that had not changed. Against a remote
// full node those are comparable in cost, so it roughly doubled relay latency.
//
// It was worse on WebSocket: ValidateFrame runs per FRAME, so every pushed
// subscription message was serialized behind a full-node RPC.
//
// The signer already caches its two equivalents (appCache, ringCache); this is
// the one the validation side was missing.
//
// Safe to cache indefinitely: an account's public key is derived from its key
// pair and does not rotate. The entries are also bounded by the supplier set of
// the sessions this app actually uses.
//
// Shared process-wide, hung off FullNode: the response path and every app's
// Signer verify and sign against the same addresses, and a per-owner cache would
// re-fetch the same gateway key once per app.
type cachingPubKeyFetcher struct {
	inner sdk.PublicKeyFetcher
	keys  sync.Map // address -> cryptotypes.PubKey
}

func newCachingPubKeyFetcher(inner sdk.PublicKeyFetcher) *cachingPubKeyFetcher {
	return &cachingPubKeyFetcher{inner: inner}
}

// GetPubKeyFromAddress returns a cached key, fetching on first use.
//
// Deliberately no singleflight: a burst of first-relays to one supplier may
// briefly fetch more than once and the last write wins, which is harmless
// because the value is identical every time. Locking the whole map across a
// network call would be the worse trade.
//
// ⚠️ A NIL KEY IS NEVER CACHED, and that one line is load-bearing twice over.
//
// The SDK answers "this account has no key onchain" as (nil, nil) — account.go:69
// passes through BaseAccount.GetPubKey(), which is nil until the account's first
// transaction. Storing that:
//   - PANICKED on the next lookup for the same address. A nil cryptotypes.PubKey
//     boxes into a nil `any`, so the single-value assertion below blew up with
//     "interface conversion: interface is nil". On the WebSocket path that WAS
//     fatal to the PROCESS — ValidateFrame runs in the bridge's own goroutine
//     (websockets/bridge.go route()), which had no recover(), so one frame from
//     a keyless supplier took down every listener and every app. internal/safego
//     now contains that class, so the same bug would cost one bridge instead of
//     the binary. That is containment, not a licence to reintroduce this: the
//     bridge still dies, the client still reconnects, and the fix here is what
//     keeps a keyless supplier from being a fault at all.
//   - would be wrong the moment that account transacts, and with no expiry would
//     stay wrong for the life of the process. On the ring path a nil member
//     blocks signing for the WHOLE app, not just one supplier.
//
// SAGE caches the nil with a 15m TTL plus a per-address refetch gate plus
// invalidate-on-failed-ring (pubkeycache.go, signer.go invalidateRingKeys). That
// machinery exists to make a stale negative safe at 66k RPS, where re-asking per
// relay would hammer the full node. Here a keyless supplier is one of ~32, is
// failed over immediately, and this proxy serves a handful of relays a second —
// so simply re-asking is cheaper than anything that makes caching nil safe.
// Do not "complete" this by adding a negative TTL.
//
// The two-value assertion is belt-and-braces: nothing nil reaches the map now, so
// it cannot fail — but an unchecked assertion is exactly what panicked, and
// falling through to a refetch on an impossible value beats crashing on one.
func (f *cachingPubKeyFetcher) GetPubKeyFromAddress(ctx context.Context, address string) (cryptotypes.PubKey, error) {
	if cached, ok := f.keys.Load(address); ok {
		if key, isKey := cached.(cryptotypes.PubKey); isKey && key != nil {
			return key, nil
		}
	}
	key, err := f.inner.GetPubKeyFromAddress(ctx, address)
	if err != nil {
		return nil, err
	}
	if key == nil {
		// Pass the SDK's own answer through unchanged: ValidateRelayResponse turns
		// it into ErrRelayResponseValidationNilSupplierPubKey, which names the
		// supplier. Deciding here what a missing key means would take that away
		// from the two callers, who need different things — see getOrCreateRing.
		return nil, nil
	}
	f.keys.Store(address, key)
	return key, nil
}
