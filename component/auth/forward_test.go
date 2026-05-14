package auth

import (
	"context"
	"testing"
)

// TestForwardedClaimsRoundtrip verifies that packing a UserIdentity into
// the map<string,string> shape used on the inter-node RPC wire and then
// rebuilding it on the worker yields the same identity the BFF saw.
func TestForwardedClaimsRoundtrip(t *testing.T) {
	original := UserIdentity{
		Subject:     "auth0|abc123",
		Email:       "alice@example.com",
		PhoneNumber: "+15551234567",
		Role:        "admin",
		FirstName:   "Alice",
		LastName:    "Example",
	}

	claims := ForwardedClaimsFromIdentity(original)
	if got := claims["sub"]; got != "auth0|abc123" {
		t.Errorf("sub claim: want %q, got %q", "auth0|abc123", got)
	}
	if got := claims["email"]; got != "alice@example.com" {
		t.Errorf("email claim: want %q, got %q", "alice@example.com", got)
	}

	ctx := ContextWithForwardedClaims(context.Background(), claims)
	tok := TokenInfoFromContext(ctx)
	if tok == nil {
		t.Fatal("TokenInfoFromContext returned nil after ContextWithForwardedClaims")
	}
	if tok.Subject != "auth0|abc123" {
		t.Errorf("rebuilt subject: want %q, got %q", "auth0|abc123", tok.Subject)
	}

	got, err := UserIdentityFromContext(ctx)
	if err != nil {
		t.Fatalf("UserIdentityFromContext: %v", err)
	}
	if got.Subject != original.Subject {
		t.Errorf("subject: want %q, got %q", original.Subject, got.Subject)
	}
	if got.Email != original.Email {
		t.Errorf("email: want %q, got %q", original.Email, got.Email)
	}
	if got.Role != original.Role {
		t.Errorf("role: want %q, got %q", original.Role, got.Role)
	}
	if got.FirstName != original.FirstName {
		t.Errorf("firstName: want %q, got %q", original.FirstName, got.FirstName)
	}
}

// TestForwardedClaimsLocalDevMarker verifies the no-auth dev shim marker
// survives the roundtrip so worker-side logs can distinguish a
// synthesised local-dev principal from a real identity.
func TestForwardedClaimsLocalDevMarker(t *testing.T) {
	id := UserIdentity{
		Subject: "local-dev",
		Email:   "local-dev@memql.local",
	}
	claims := ForwardedClaimsWithLocalDev(id)
	if claims["memql_local_dev"] != "1" {
		t.Errorf("memql_local_dev marker missing: got %q", claims["memql_local_dev"])
	}

	ctx := ContextWithForwardedClaims(context.Background(), claims)
	tok := TokenInfoFromContext(ctx)
	if tok == nil {
		t.Fatal("TokenInfoFromContext returned nil")
	}
	if tok.Claims["memql_local_dev"] != "1" {
		t.Errorf("marker not propagated into claims: %v", tok.Claims["memql_local_dev"])
	}
}

// TestForwardedClaimsEmpty verifies the helpers no-op cleanly when the
// claims map is empty (e.g. a forwarded request with no principal).
func TestForwardedClaimsEmpty(t *testing.T) {
	if got := TokenInfoFromForwardedClaims(nil); got != nil {
		t.Errorf("expected nil TokenInfo for empty claims, got %+v", got)
	}
	if got := TokenInfoFromForwardedClaims(map[string]string{}); got != nil {
		t.Errorf("expected nil TokenInfo for empty claims, got %+v", got)
	}
	ctx := ContextWithForwardedClaims(context.Background(), nil)
	if TokenInfoFromContext(ctx) != nil {
		t.Error("expected no TokenInfo in context after empty claims")
	}
}
