package pocket

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sdk "github.com/pokt-network/shannon-sdk"
)

// fakePubKeys stands in for sdk.AccountClient. It counts calls per address,
// which is the whole point: every assertion below is about how many times the
// full node was asked, not about the key that came back.
type fakePubKeys struct {
	mu     sync.Mutex
	keys   map[string]cryptotypes.PubKey // absent => (nil, nil), the SDK's "no key onchain"
	err    error
	counts map[string]int
}

func newFakePubKeys() *fakePubKeys {
	return &fakePubKeys{keys: map[string]cryptotypes.PubKey{}, counts: map[string]int{}}
}

func (f *fakePubKeys) GetPubKeyFromAddress(_ context.Context, address string) (cryptotypes.PubKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[address]++
	if f.err != nil {
		return nil, f.err
	}
	// Deliberately NOT `return f.keys[address], nil` — a missing entry must
	// produce a nil interface exactly as BaseAccount.GetPubKey() does.
	key, ok := f.keys[address]
	if !ok {
		return nil, nil
	}
	return key, nil
}

func (f *fakePubKeys) count(address string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[address]
}

var _ sdk.PublicKeyFetcher = (*fakePubKeys)(nil)

// TestPubKeyCache_NilKeyNeverPanics is the regression test for a PROCESS-KILLING
// crash, not a perf detail.
//
// The SDK answers "this account has never signed a transaction" as (nil, nil)
// (account.go:69 -> BaseAccount.GetPubKey()). The cache used to store that and
// then assert `cached.(cryptotypes.PubKey)` on the next lookup — a single-value
// assertion on a nil interface, which panics. ValidateFrame runs in the
// WebSocket bridge's own goroutine, which had no recover() when this was found,
// so the second frame from a keyless supplier took down every listener and every
// app. internal/safego now bounds that blast radius to the one bridge; this test
// is what keeps the panic from happening in the first place.
//
// Mutation check: restore `f.keys.Store(address, key)` unconditionally and the
// second call here panics with
// "interface conversion: interface is nil, not types.PubKey".
func TestPubKeyCache_NilKeyNeverPanics(t *testing.T) {
	inner := newFakePubKeys() // knows no addresses => every answer is (nil, nil)
	cache := newCachingPubKeyFetcher(inner)

	for i := range 3 {
		key, err := cache.GetPubKeyFromAddress(context.Background(), "pokt1keyless")
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if key != nil {
			t.Fatalf("call %d: want a nil key passed through, got %v", i, key)
		}
	}

	// Re-asked every time, by design. A cached nil would be wrong the moment the
	// account transacts and, with no expiry, would stay wrong for the life of the
	// process — see the comment on GetPubKeyFromAddress for why we do not buy a
	// negative TTL to fix that.
	if got := inner.count("pokt1keyless"); got != 3 {
		t.Errorf("nil answers: inner queried %d times, want 3 (nil must never be cached)", got)
	}
}

// TestPubKeyCache_RealKeyIsServedFromMemory pins the cache's actual job: one
// full-node query per address, ever.
func TestPubKeyCache_RealKeyIsServedFromMemory(t *testing.T) {
	inner := newFakePubKeys()
	want := secp256k1.GenPrivKey().PubKey()
	inner.keys["pokt1supplier"] = want
	cache := newCachingPubKeyFetcher(inner)

	for i := range 5 {
		got, err := cache.GetPubKeyFromAddress(context.Background(), "pokt1supplier")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !got.Equals(want) {
			t.Fatalf("call %d: wrong key", i)
		}
	}

	if got := inner.count("pokt1supplier"); got != 1 {
		t.Errorf("inner queried %d times, want 1", got)
	}
}

// TestPubKeyCache_ErrorIsNotCached keeps a transient full-node failure from
// becoming permanent. Distinct from the nil case: nil is an *answer*, this is
// the absence of one.
func TestPubKeyCache_ErrorIsNotCached(t *testing.T) {
	inner := newFakePubKeys()
	inner.err = errors.New("full node unreachable")
	cache := newCachingPubKeyFetcher(inner)

	if _, err := cache.GetPubKeyFromAddress(context.Background(), "pokt1s"); err == nil {
		t.Fatal("want an error from the failing fetch")
	}

	inner.err = nil
	want := secp256k1.GenPrivKey().PubKey()
	inner.keys["pokt1s"] = want

	got, err := cache.GetPubKeyFromAddress(context.Background(), "pokt1s")
	if err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	if got == nil || !got.Equals(want) {
		t.Error("a failed fetch was cached; the address stayed broken after the node recovered")
	}
}

// TestFullNode_PubKeyFetcherIsShared proves the Validator and every Signer read
// one cache. Two caches would mean each app re-fetches a shared gateway's key,
// and the response path re-fetches keys the signing path already holds.
func TestFullNode_PubKeyFetcherIsShared(t *testing.T) {
	fn := &FullNode{}

	if a, b := fn.PubKeyFetcher(), fn.PubKeyFetcher(); a != b {
		t.Fatal("PubKeyFetcher handed out two different caches")
	}

	signer, err := NewSigner(throwawayKey, fn)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if signer.pubKeys != fn.PubKeyFetcher() {
		t.Error("the signer does not share the full node's key cache")
	}
	if NewValidator(fn).pubKeys != fn.PubKeyFetcher() {
		t.Error("the validator does not share the full node's key cache")
	}
}

// TestSigner_RingBuildsFromTheCachedKeys is the pocket-ap half of SAGE 88302f6.
//
// ringCache is keyed by session end height, so it misses at EVERY session
// boundary — minutes on beta, ~20 of them on mainnet. Ring *composition* is what
// can change there; the keys of the addresses in it cannot, because a public key
// is immutable once set. Before this, each rollover re-queried the full node for
// every ring member, and with no singleflight every concurrent in-flight relay
// for the app did so at once.
//
// Mutation check: point getOrCreateRing back at an uncached fetcher and the
// count below goes from 1 to 3.
func TestSigner_RingBuildsFromTheCachedKeys(t *testing.T) {
	inner := newFakePubKeys()
	signer, err := NewSigner(throwawayKey, &FullNode{})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	signer.pubKeys = newCachingPubKeyFetcher(inner)

	appAddr := signer.AppAddr()
	inner.keys[appAddr] = secp256k1.GenPrivKey().PubKey()
	app := &apptypes.Application{Address: appAddr}

	// Three different sessions: one rollover per call, so the ring cache misses
	// every time and the key lookup is what is being counted.
	for _, endHeight := range []uint64{100, 200, 300} {
		if _, err := signer.getOrCreateRing(context.Background(), app, endHeight); err != nil {
			t.Fatalf("getOrCreateRing at %d: %v", endHeight, err)
		}
	}

	if got := inner.count(appAddr); got != 1 {
		t.Errorf("full node queried %d times across 3 session rollovers, want 1", got)
	}
}

// TestSigner_RingRejectsAKeylessMember checks the (nil, nil) answer is caught
// where it is legible. rings.GetRingFromPubKeys would otherwise fail somewhere
// far from the cause — and a ring member without a key blocks signing for the
// WHOLE app, not one supplier, so the message has to say which address.
func TestSigner_RingRejectsAKeylessMember(t *testing.T) {
	inner := newFakePubKeys() // no keys at all => (nil, nil) for the app itself
	signer, err := NewSigner(throwawayKey, &FullNode{})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	signer.pubKeys = newCachingPubKeyFetcher(inner)

	app := &apptypes.Application{Address: signer.AppAddr()}
	_, err = signer.getOrCreateRing(context.Background(), app, 100)
	if err == nil {
		t.Fatal("want an error for a ring member with no onchain public key")
	}
	if !strings.Contains(err.Error(), signer.AppAddr()) {
		t.Errorf("error does not name the offending address: %v", err)
	}

	// Nothing was cached, so the next attempt re-asks the chain. This is what
	// buys us out of SAGE's invalidate-and-retry machinery: with no negative
	// entry there is nothing to invalidate.
	if _, err := signer.getOrCreateRing(context.Background(), app, 100); err == nil {
		t.Fatal("want the same error on the retry")
	}
	if got := inner.count(signer.AppAddr()); got != 2 {
		t.Errorf("full node queried %d times, want 2 — a keyless member must not be cached", got)
	}
}

// TestMinerErrDetail covers the miner's own account of its failures. Without it
// a failure INSIDE the relay miner is indistinguishable from one at the backend,
// and the miner is the only thing that can tell them apart.
func TestMinerErrDetail(t *testing.T) {
	if got := minerErrDetail(nil); got != "" {
		t.Errorf("nil response: got %q, want empty", got)
	}
	if got := minerErrDetail(&servicetypes.RelayResponse{}); got != "" {
		t.Errorf("no miner error: got %q, want empty", got)
	}

	got := minerErrDetail(&servicetypes.RelayResponse{
		RelayMinerError: &servicetypes.RelayMinerError{
			Codespace:   "relayer_proxy",
			Code:        1,
			Description: "invalid session in relayer request",
			Message:     "application has 0 service configs",
		},
	})
	for _, want := range []string{"relayer_proxy", "code=1", "invalid session", "0 service configs"} {
		if !strings.Contains(got, want) {
			t.Errorf("detail %q is missing %q", got, want)
		}
	}
}
