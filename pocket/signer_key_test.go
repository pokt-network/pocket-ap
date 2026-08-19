package pocket

import "testing"

// throwawayKey is a made-up secp256k1 key, never staked and never used. Real app
// keys live in local/ and must never reach a committed file.
const (
	throwawayKey  = "4bd7f2e1a9c3068b5d4f7e2a1c9b8d6e3f5a7c2b4d6e8f0a1c3e5b7d9f2a4c6e"
	throwawayAddr = "pokt162v6th9xg86v2gh80a8xddwtxwlfzxrnp4gq5k"
)

// deriveAppAddr IS the sovereign-app-key model: the app address is derived from
// the signing key rather than configured, which is what stops pocket-ap ever
// signing as a gateway. If this drifts, relays get signed for the wrong app and
// the miner rejects them — or worse, they are billed to someone else's stake.
//
// The pairing is pinned to a literal so a change in the derivation (curve, hash,
// or the "pokt" bech32 prefix) cannot pass silently.
func TestDeriveAppAddr(t *testing.T) {
	got, err := deriveAppAddr(throwawayKey)
	if err != nil {
		t.Fatalf("deriveAppAddr: %v", err)
	}
	if got != throwawayAddr {
		t.Errorf("deriveAppAddr = %q, want %q — the key→address derivation changed", got, throwawayAddr)
	}
}

func TestDeriveAppAddr_IsDeterministic(t *testing.T) {
	first, err := deriveAppAddr(throwawayKey)
	if err != nil {
		t.Fatalf("deriveAppAddr: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := deriveAppAddr(throwawayKey)
		if err != nil {
			t.Fatalf("deriveAppAddr: %v", err)
		}
		if again != first {
			t.Fatalf("deriveAppAddr is not deterministic: %q then %q", first, again)
		}
	}
}

// Every address must carry the "pokt" prefix: a bech32 address with the wrong
// HRP is a different chain's address and would be silently unusable.
func TestDeriveAppAddr_UsesPoktPrefix(t *testing.T) {
	got, err := deriveAppAddr(throwawayKey)
	if err != nil {
		t.Fatalf("deriveAppAddr: %v", err)
	}
	if len(got) < 5 || got[:5] != "pokt1" {
		t.Errorf("address = %q, want the pokt bech32 prefix", got)
	}
}

// Distinct keys must not collapse to one address — the sanity check that we are
// deriving from the key at all rather than returning something constant.
func TestDeriveAppAddr_DifferentKeysDifferentAddresses(t *testing.T) {
	other := "1111111111111111111111111111111111111111111111111111111111111111"
	a, err := deriveAppAddr(throwawayKey)
	if err != nil {
		t.Fatalf("deriveAppAddr: %v", err)
	}
	b, err := deriveAppAddr(other)
	if err != nil {
		t.Fatalf("deriveAppAddr: %v", err)
	}
	if a == b {
		t.Error("two different keys derived the same address")
	}
}

// Every one of these used to return a plausible pokt1… address and no error,
// which is the worst possible outcome: the proxy starts, signs as an app that is
// not yours, and every relay fails with "session not found" while the key looks
// fine in the config.
func TestDeriveAppAddr_RejectsMalformedKeys(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"not hex", "zzzz"},
		{"odd length", "abc"},
		{"empty", ""},
		{"truncated paste", "4bd7f2e1"},
		{"half a key", "4bd7f2e1a9c3068b5d4f7e2a1c9b8d6e"},
		{"one byte too long", throwawayKey + "ff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := deriveAppAddr(tt.key); err == nil {
				t.Errorf("deriveAppAddr(%q) succeeded, want an error", tt.key)
			}
		})
	}
}

// The specific horror: a key with one extra byte derived the SAME address as the
// correct key, so the extra byte was silently dropped.
func TestDeriveAppAddr_OverlongKeyIsNotSilentlyTruncated(t *testing.T) {
	if _, err := deriveAppAddr(throwawayKey + "ff"); err == nil {
		t.Fatal("a 33-byte key was accepted — it used to derive the same address as the 32-byte key, hiding the mistake")
	}
}

// A wrong-length key must not reach the signer either.
func TestNewSigner_RejectsWrongLengthKey(t *testing.T) {
	if _, err := NewSigner("4bd7f2e1a9c3068b5d4f7e2a1c9b8d6e", &FullNode{}); err == nil {
		t.Error("NewSigner accepted a 16-byte key")
	}
}

// The signer must expose the address it derived, not one it was told: main.go
// hands AppAddr() to the session manager and the WS handshake, so a mismatch
// between "who we sign as" and "who we claim to be" would be invisible here and
// fatal at the miner.
func TestNewSigner_AppAddrMatchesTheKey(t *testing.T) {
	// A zero-value FullNode is fine: NewSigner only reads fn.PubKeyFetcher(),
	// which builds the cache lazily and dials nothing.
	s, err := NewSigner(throwawayKey, &FullNode{})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if s.AppAddr() != throwawayAddr {
		t.Errorf("AppAddr() = %q, want %q", s.AppAddr(), throwawayAddr)
	}
}

func TestNewSigner_RejectsBadKey(t *testing.T) {
	if _, err := NewSigner("not-a-key", &FullNode{}); err == nil {
		t.Error("NewSigner accepted a non-hex key")
	}
}
