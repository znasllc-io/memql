package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// A stand-in identity provider: discovery, JWKS, and a signing key, so the
// verification path is exercised against real signatures rather than a stub.
type fakeIdP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	kid    string
	issuer string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	p := &fakeIdP{key: key, kid: "test-key-1"}
	mux := http.NewServeMux()
	mux.HandleFunc(DiscoveryPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.issuer,
			"authorization_endpoint":                p.issuer + "/authorize",
			"token_endpoint":                        p.issuer + "/token",
			"jwks_uri":                              p.issuer + "/jwks",
			"code_challenge_methods_supported":      []string{"S256"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{"kty": "RSA", "kid": p.kid, "n": n, "e": e}},
		})
	})
	// A TLS server, not a plaintext one: Discover REFUSES an http issuer, and
	// that refusal is correct -- the keys that verify an id token must not be
	// fetched over a channel anyone on the path can choose. Adding a test-only
	// escape hatch to the production path would remove exactly the check
	// TestDiscoveryRefusesPlaintext exists to keep.
	p.srv = httptest.NewTLSServer(mux)
	p.issuer = p.srv.URL
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeIdP) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = p.kid
	s, err := tok.SignedString(p.key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func (p *fakeIdP) claims(aud, nonce string) jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss": p.issuer, "sub": "upstream-subject-1", "aud": aud, "nonce": nonce,
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(),
		"email": "person@example.com", "email_verified": true, "name": "A Person",
	}
}

// -----------------------------------------------------------------------------
// discovery
// -----------------------------------------------------------------------------

func TestDiscoveryRefusesAnIssuerMismatch(t *testing.T) {
	// OIDC Discovery §4.3, and not a formality: without it, anything that can
	// answer for a hostname could name a DIFFERENT issuer, and every id token
	// this cluster then accepted would be validated against the wrong
	// authority.
	p := newFakeIdP(t)
	p.issuer = "https://somebody.else.example.com"

	d := &Discoverer{HTTP: p.srv.Client()}
	_, err := d.Discover(context.Background(), p.srv.URL)
	if err == nil {
		t.Fatal("a document naming another issuer was accepted")
	}
	if !strings.Contains(err.Error(), "names issuer") {
		t.Errorf("error does not name the mismatch: %v", err)
	}
}

func TestDiscoveryRefusesPlaintext(t *testing.T) {
	// The keys that verify an id token would be fetched over a channel anyone
	// on the path can choose.
	d := &Discoverer{}
	if _, err := d.Discover(context.Background(), "http://idp.example.com"); err == nil {
		t.Fatal("a plaintext issuer was accepted")
	}
}

func TestDiscoveryCachesAndDoesNotRefetch(t *testing.T) {
	p := newFakeIdP(t)
	hits := 0
	wrapped := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		p.srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer wrapped.Close()
	p.issuer = wrapped.URL

	d := &Discoverer{HTTP: wrapped.Client(), TTL: time.Hour}
	for i := 0; i < 3; i++ {
		if _, err := d.Discover(context.Background(), wrapped.URL); err != nil {
			t.Fatalf("discover: %v", err)
		}
	}
	if hits != 1 {
		t.Errorf("fetched %d times, want 1 -- this is on the sign-in path", hits)
	}
}

func TestMetadataRequiresJWKS(t *testing.T) {
	// Without JWKS there is no way to verify an id token, and an unverified id
	// token is a JSON document a stranger handed us.
	m := Metadata{Issuer: "https://i", AuthorizationEndpoint: "https://i/a", TokenEndpoint: "https://i/t"}
	if err := m.Valid(); err == nil {
		t.Fatal("a document with no jwks_uri was accepted")
	}
}

// -----------------------------------------------------------------------------
// id token verification -- the security surface
// -----------------------------------------------------------------------------

func verifier(t *testing.T, p *fakeIdP) *KeySet {
	t.Helper()
	return &KeySet{URL: p.issuer + "/jwks", HTTP: p.srv.Client()}
}

func TestIDTokenHappyPath(t *testing.T) {
	p := newFakeIdP(t)
	raw := p.sign(t, p.claims("this-cluster", "the-nonce"))

	got, err := verifier(t, p).VerifyIDToken(context.Background(), raw, VerifyParams{
		Issuer: p.issuer, ClientID: "this-cluster", Nonce: "the-nonce",
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Subject != "upstream-subject-1" || got.Email != "person@example.com" || !got.EmailVerified {
		t.Errorf("claims not read back: %+v", got)
	}
}

func TestIDTokenMintedForAnotherApplicationIsRefused(t *testing.T) {
	// THE CONFUSED DEPUTY. Without the audience check, an id token minted for
	// ANY application in the same tenant would sign somebody in here.
	p := newFakeIdP(t)
	raw := p.sign(t, p.claims("some-other-app", "the-nonce"))

	_, err := verifier(t, p).VerifyIDToken(context.Background(), raw, VerifyParams{
		Issuer: p.issuer, ClientID: "this-cluster", Nonce: "the-nonce",
	})
	if err == nil {
		t.Fatal("an id token minted for another application was accepted")
	}
}

func TestIDTokenFromAnotherIssuerIsRefused(t *testing.T) {
	p := newFakeIdP(t)
	c := p.claims("this-cluster", "the-nonce")
	c["iss"] = "https://evil.example.com"
	raw := p.sign(t, c)

	if _, err := verifier(t, p).VerifyIDToken(context.Background(), raw, VerifyParams{
		Issuer: p.issuer, ClientID: "this-cluster", Nonce: "the-nonce",
	}); err == nil {
		t.Fatal("an id token from another issuer was accepted")
	}
}

func TestIDTokenNonceMustMatchTheRequest(t *testing.T) {
	// state proves the CALLBACK belongs to this sign-in; nonce proves the
	// TOKEN does. Two different replays.
	p := newFakeIdP(t)
	raw := p.sign(t, p.claims("this-cluster", "a-different-nonce"))

	if _, err := verifier(t, p).VerifyIDToken(context.Background(), raw, VerifyParams{
		Issuer: p.issuer, ClientID: "this-cluster", Nonce: "the-nonce",
	}); err == nil {
		t.Fatal("a replayed id token was accepted")
	}
}

func TestIDTokenWithNoNonceIsRefusedWhenOneWasRequested(t *testing.T) {
	p := newFakeIdP(t)
	c := p.claims("this-cluster", "")
	delete(c, "nonce")
	if _, err := verifier(t, p).VerifyIDToken(context.Background(), p.sign(t, c), VerifyParams{
		Issuer: p.issuer, ClientID: "this-cluster", Nonce: "the-nonce",
	}); err == nil {
		t.Fatal("an id token carrying no nonce was accepted")
	}
}

func TestIDTokenAlgNoneIsRefused(t *testing.T) {
	// The classic JWT failure. `alg` is attacker-controlled, so the accepted
	// set is an allow-list rather than whatever the header says.
	p := newFakeIdP(t)
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, p.claims("this-cluster", "the-nonce"))
	tok.Header["kid"] = p.kid
	raw, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, err := verifier(t, p).VerifyIDToken(context.Background(), raw, VerifyParams{
		Issuer: p.issuer, ClientID: "this-cluster", Nonce: "the-nonce",
	}); err == nil {
		t.Fatal("an alg=none id token was accepted")
	}
}

func TestIDTokenSignedByAnotherKeyIsRefused(t *testing.T) {
	p := newFakeIdP(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, p.claims("this-cluster", "the-nonce"))
	tok.Header["kid"] = p.kid // claims the real kid, signed with a different key
	raw, err := tok.SignedString(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier(t, p).VerifyIDToken(context.Background(), raw, VerifyParams{
		Issuer: p.issuer, ClientID: "this-cluster", Nonce: "the-nonce",
	}); err == nil {
		t.Fatal("an id token signed by an unknown key was accepted")
	}
}

func TestIDTokenExpiredIsRefused(t *testing.T) {
	p := newFakeIdP(t)
	c := p.claims("this-cluster", "the-nonce")
	c["exp"] = time.Now().Add(-time.Minute).Unix()
	if _, err := verifier(t, p).VerifyIDToken(context.Background(), p.sign(t, c), VerifyParams{
		Issuer: p.issuer, ClientID: "this-cluster", Nonce: "the-nonce",
	}); err == nil {
		t.Fatal("an expired id token was accepted")
	}
}

func TestGroupsClaimIsReadOnlyWhenNamed(t *testing.T) {
	p := newFakeIdP(t)
	c := p.claims("this-cluster", "n")
	c["groups"] = []any{"g-admins", "g-everyone"}

	k := verifier(t, p)
	base := VerifyParams{Issuer: p.issuer, ClientID: "this-cluster", Nonce: "n"}
	raw := p.sign(t, c)

	unnamed, err := k.VerifyIDToken(context.Background(), raw, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(unnamed.Groups) != 0 {
		t.Errorf("groups read with no claim configured: %v", unnamed.Groups)
	}

	named := base
	named.GroupsClaim = "groups"
	got, err := k.VerifyIDToken(context.Background(), raw, named)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 2 {
		t.Errorf("groups = %v, want two", got.Groups)
	}
}
