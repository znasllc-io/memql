package identity

import (
	"testing"
	"time"
)

// TestBuildJWKS_PublishesCurrentAndPreviousDuringOverlap is the #1515
// defense-in-depth check: across a rotation the JWKS feed must publish
// BOTH the new (current) and the retiring (previous) key for the whole
// overlap window, so a verifier that fetches-on-unknown-kid finds either
// key in flight and no token verification gaps open up.
func TestBuildJWKS_PublishesCurrentAndPreviousDuringOverlap(t *testing.T) {
	km, err := NewKeyManager(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewKeyManager: %v", err)
	}
	if err := km.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	now := time.Now().UTC()

	// Single key before any rotation.
	first := BuildJWKS(km, now)
	if len(first.Keys) != 1 {
		t.Fatalf("pre-rotation JWKS = %d keys, want 1", len(first.Keys))
	}
	originalKID := first.Keys[0].Kid

	// Rotate with a 24h overlap; both keys must now be live.
	overlap := 24 * time.Hour
	if _, err := km.Rotate(overlap); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	newKID := km.Current().KID

	during := BuildJWKS(km, now)
	if len(during.Keys) != 2 {
		t.Fatalf("in-overlap JWKS = %d keys, want 2 (current + previous)", len(during.Keys))
	}
	got := map[string]bool{during.Keys[0].Kid: true, during.Keys[1].Kid: true}
	if !got[originalKID] {
		t.Errorf("in-overlap JWKS missing retiring key %q", originalKID)
	}
	if !got[newKID] {
		t.Errorf("in-overlap JWKS missing new current key %q", newKID)
	}
	for _, k := range during.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" || k.Alg != "EdDSA" || k.X == "" {
			t.Errorf("malformed JWK entry: %+v", k)
		}
	}

	// After the overlap elapses the retired key drops out, leaving only
	// the new current key.
	after := BuildJWKS(km, now.Add(overlap+time.Minute))
	if len(after.Keys) != 1 {
		t.Fatalf("post-overlap JWKS = %d keys, want 1", len(after.Keys))
	}
	if after.Keys[0].Kid != newKID {
		t.Errorf("post-overlap JWKS kid = %q, want current %q", after.Keys[0].Kid, newKID)
	}
}
