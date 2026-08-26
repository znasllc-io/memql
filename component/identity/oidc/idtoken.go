package oidc

import (
	"context"
	"crypto"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ID-TOKEN VERIFICATION, AND WHY IT IS NOT REUSED FROM component/identity/verifier
// (memql#4611).
//
// The per-node verifier exists to check MemQL's OWN tokens, and it is Ed25519
// only -- that is this cluster's signing algorithm and narrowing to it is
// correct there. An upstream provider signs with whatever it signs with, and
// Entra ID (like most of the ecosystem) uses RS256. So this is a second, small
// JWKS reader rather than a widening of the first: widening the node verifier
// to accept RSA would mean every node in the mesh would accept an RSA-signed
// MemQL token, which is a strictly larger surface for a reason that has nothing
// to do with the mesh.
//
// ALGORITHMS ARE ALLOW-LISTED, NOT READ FROM THE HEADER. `alg` in a JWT header
// is attacker-controlled; the classic failure is accepting `none`, and the
// subtler one is accepting HMAC where the "key" is then the provider's PUBLIC
// key. Only RSA and ECDSA signature algorithms are admitted here, and the key
// looked up by `kid` must be of the matching family.

// allowedAlgs is the closed set of id-token signing algorithms. RS256 is what
// Entra ID and nearly everything else uses; the rest are here so a provider on
// a stronger curve is not refused for being stronger.
var allowedAlgs = []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256", "PS384", "PS512"}

// Claims is what the upstream says about a person, after verification.
//
// EVERY FIELD HERE IS A CLAIM, NOT A FACT ABOUT THIS CLUSTER. The mapping from
// these to a MemQL user is linking.go's, deliberately: this type must not grow
// a `Role` or a `UserId`, because those are decisions and this is evidence.
type Claims struct {
	// Issuer + Subject together are the STABLE identity. Email is not: people
	// change surname, addresses get reassigned, and an address is the one
	// claim a directory admin can move between principals.
	Issuer  string
	Subject string

	Email         string
	EmailVerified bool
	Name          string
	// Groups as the provider named them -- ids or names, whichever the claim
	// carries. Mapping them to a cluster role is policy and lives elsewhere.
	Groups []string

	IssuedAt  time.Time
	ExpiresAt time.Time
}

// VerifyParams are the values an id token must agree with.
type VerifyParams struct {
	Issuer   string
	ClientID string
	// Nonce as sent on the authorization request. Checked because state proves
	// the CALLBACK belongs to this sign-in and nonce proves the TOKEN does --
	// two different replays.
	Nonce string
	// GroupsClaim names the claim carrying group membership. Configurable
	// because there is no standard one: Entra uses `groups`, others use
	// `roles`. Empty means groups are not read at all.
	GroupsClaim string
	Now         time.Time
}

// VerifyIDToken checks an id token and returns what it says.
func (k *KeySet) VerifyIDToken(ctx context.Context, raw string, p VerifyParams) (Claims, error) {
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}

	parsed, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return k.key(ctx, kid, t.Method.Alg())
	},
		jwt.WithValidMethods(allowedAlgs),
		jwt.WithIssuer(strings.TrimRight(p.Issuer, "/")),
		// AUDIENCE IS THE CLUSTER'S OWN client_id. Without it, an id token
		// minted for ANY application in the same tenant would sign somebody in
		// here -- which is the whole shape of a confused-deputy attack, and it
		// is one line to prevent.
		jwt.WithAudience(p.ClientID),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	if err != nil {
		return Claims{}, fmt.Errorf("id token rejected: %w", err)
	}

	mc, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return Claims{}, errors.New("id token carries no claims object")
	}

	// NONCE, and it is REQUIRED because the authorization request always sends
	// one. A token with no nonce, when one was asked for, is either a replay
	// from a different request or a provider that ignored a mandatory
	// parameter; neither is something to sign somebody in on.
	if p.Nonce != "" {
		got, _ := mc["nonce"].(string)
		if got == "" {
			return Claims{}, errors.New("id token carries no nonce, but one was requested")
		}
		if subtleCompare(got, p.Nonce) != 1 {
			return Claims{}, errors.New("id token nonce does not match the authorization request")
		}
	}

	sub, _ := mc["sub"].(string)
	if strings.TrimSpace(sub) == "" {
		return Claims{}, errors.New("id token carries no subject")
	}

	claims := Claims{
		Issuer:        strings.TrimRight(p.Issuer, "/"),
		Subject:       sub,
		Email:         strings.TrimSpace(stringClaim(mc, "email")),
		EmailVerified: boolClaim(mc, "email_verified"),
		Name:          strings.TrimSpace(firstNonEmpty(stringClaim(mc, "name"), stringClaim(mc, "preferred_username"))),
		IssuedAt:      timeClaim(mc, "iat"),
		ExpiresAt:     timeClaim(mc, "exp"),
	}
	if p.GroupsClaim != "" {
		claims.Groups = stringListClaim(mc, p.GroupsClaim)
	}
	return claims, nil
}

// -----------------------------------------------------------------------------
// JWKS
// -----------------------------------------------------------------------------

// KeySet fetches and caches an upstream provider's signing keys.
type KeySet struct {
	URL  string
	HTTP *http.Client
	TTL  time.Duration

	mu      sync.Mutex
	keys    map[string]crypto.PublicKey
	fetched time.Time
}

// DefaultKeySetTTL is 15 minutes. Providers rotate signing keys on their own
// schedule and publish the new one before using it; a short TTL means a
// rotation costs at most one refresh rather than a restart.
const DefaultKeySetTTL = 15 * time.Minute

// key resolves a kid, refreshing once for an unknown one.
//
// THE REFRESH-ON-UNKNOWN-KID IS THE ROTATION STORY, and it mirrors what the
// node verifier already does for MemQL's own keys: a provider that rotates
// mid-TTL would otherwise fail every sign-in until the cache expired.
func (k *KeySet) key(ctx context.Context, kid, alg string) (crypto.PublicKey, error) {
	if kid == "" {
		return nil, errors.New("id token header names no kid, so its signing key cannot be identified")
	}
	ttl := k.TTL
	if ttl <= 0 {
		ttl = DefaultKeySetTTL
	}

	k.mu.Lock()
	key, ok := k.keys[kid]
	stale := time.Since(k.fetched) >= ttl
	k.mu.Unlock()
	if ok && !stale {
		return matchFamily(key, alg)
	}

	if err := k.refresh(ctx); err != nil {
		if ok {
			// A refresh failure with a usable cached key is not a sign-in
			// failure. Refusing here would make the provider's availability
			// this cluster's availability for the length of one outage.
			return matchFamily(key, alg)
		}
		return nil, err
	}

	k.mu.Lock()
	key, ok = k.keys[kid]
	k.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no signing key %q at %s", kid, k.URL)
	}
	return matchFamily(key, alg)
}

// matchFamily refuses a key whose type does not match the declared algorithm.
//
// This is the second half of the allow-list. `alg` is attacker-controlled, so
// admitting RS256 and then handing an RSA public key to an HMAC verifier is the
// classic confusion; jwt.WithValidMethods bars the symmetric families outright,
// and this bars the remaining mismatch.
func matchFamily(key crypto.PublicKey, alg string) (crypto.PublicKey, error) {
	isRSA := strings.HasPrefix(alg, "RS") || strings.HasPrefix(alg, "PS")
	if _, ok := key.(*rsa.PublicKey); ok != isRSA {
		return nil, fmt.Errorf("signing key type does not match the declared algorithm %q", alg)
	}
	return key, nil
}

func (k *KeySet) refresh(ctx context.Context) error {
	client := k.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch jwks %s: %w", k.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks %s answered %d", k.URL, resp.StatusCode)
	}

	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("parse jwks %s: %w", k.URL, err)
	}

	next := map[string]crypto.PublicKey{}
	for _, jwk := range doc.Keys {
		if jwk.Kid == "" || jwk.Kty != "RSA" {
			// Only RSA is materialised today, because it is what the providers
			// this targets publish. An EC key simply does not enter the map,
			// and a token signed by one fails as "no signing key <kid>" --
			// which names the real situation rather than crashing.
			continue
		}
		pub, err := rsaKeyFromJWK(jwk.N, jwk.E)
		if err != nil {
			continue
		}
		next[jwk.Kid] = pub
	}
	if len(next) == 0 {
		return fmt.Errorf("jwks %s published no usable keys", k.URL)
	}

	k.mu.Lock()
	k.keys = next
	k.fetched = time.Now()
	k.mu.Unlock()
	return nil
}

func rsaKeyFromJWK(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(nB64, "="))
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(eB64, "="))
	if err != nil {
		return nil, err
	}
	if len(eBytes) == 0 || len(eBytes) > 8 {
		return nil, errors.New("jwk exponent out of range")
	}
	padded := make([]byte, 8)
	copy(padded[8-len(eBytes):], eBytes)
	e := binary.BigEndian.Uint64(padded)
	if e == 0 || e > 1<<31 {
		return nil, errors.New("jwk exponent out of range")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(e)}, nil
}

// -----------------------------------------------------------------------------
// claim readers
// -----------------------------------------------------------------------------

func stringClaim(c jwt.MapClaims, key string) string {
	v, _ := c[key].(string)
	return v
}

func boolClaim(c jwt.MapClaims, key string) bool {
	switch v := c[key].(type) {
	case bool:
		return v
	case string:
		// Some providers stringify it. "true" is the only truthy spelling
		// accepted, because guessing here would mean treating an unverified
		// address as verified -- and a verified address is what authorizes
		// linking to an existing account (linking.go).
		return v == "true"
	}
	return false
}

func timeClaim(c jwt.MapClaims, key string) time.Time {
	switch v := c[key].(type) {
	case float64:
		return time.Unix(int64(v), 0).UTC()
	case int64:
		return time.Unix(v, 0).UTC()
	}
	return time.Time{}
}

func stringListClaim(c jwt.MapClaims, key string) []string {
	switch v := c[key].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		// A single-valued claim arrives unwrapped from some providers.
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// subtleCompare is a constant-time string compare returning 1 on equality.
// Constant time because the nonce is a secret the caller minted, and a timing
// oracle on it would let an attacker construct a matching one.
func subtleCompare(a, b string) int {
	if len(a) != len(b) {
		return 0
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	if diff == 0 {
		return 1
	}
	return 0
}
