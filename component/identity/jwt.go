package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AccessTokenClaims is the JWT payload identity issues for every
// successful login. Keep this stable — every node in the cluster
// validates against this exact shape.
//
// Conventions:
//
//	sub          v1:identity:user.id (canonical user identifier)
//	email        the user's primary email at time of issue
//	name         display name (best-effort)
//	given_name   first name (OIDC-style claim) — the CoPresent profile
//	             shows this as "First name". Empty when the user row
//	             doesn't carry it yet.
//	family_name  last name (OIDC-style claim) — same lifecycle as
//	             given_name.
//	role         cluster-wide role: owner / admin / writer / reader.
//	             external users have empty role here; their scoping
//	             lives in `partitions`.
//	internal     true if the user's email matched an internal-domain
//	             entry at registration. UI surfaces use this for the
//	             "employee" / "external" distinction.
//	partitions   per-partition role grants. Map of partition name to
//	             role. Cluster owners have empty map (they bypass
//	             the ACL check entirely).
//	jti          unique token id, useful for the future "blacklist
//	             this specific token" capability if we ever need it.
//	sid          v1:identity:authSession.id — every refresh keeps the
//	             same sid so per-device revoke can target it.
type AccessTokenClaims struct {
	Email      string            `json:"email,omitempty"`
	Name       string            `json:"name,omitempty"`
	GivenName  string            `json:"given_name,omitempty"`
	FamilyName string            `json:"family_name,omitempty"`
	Role       string            `json:"role,omitempty"`
	Internal   bool              `json:"internal,omitempty"`
	Partitions map[string]string `json:"partitions,omitempty"`
	SessionId  string            `json:"sid,omitempty"`

	jwt.RegisteredClaims
}

// JWTIssuer mints access tokens. Wraps a KeyManager for signing-key
// material plus a Config for issuer / audience / TTL.
type JWTIssuer struct {
	keys     *KeyManager
	issuer   string
	audience string
	ttl      time.Duration
}

// NewJWTIssuer builds a JWTIssuer rooted at the given key manager
// and config. Returns an error if config doesn't have a usable issuer
// or audience.
func NewJWTIssuer(km *KeyManager, cfg Config) (*JWTIssuer, error) {
	if km == nil {
		return nil, errors.New("identity: nil KeyManager")
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("identity: cfg.BaseURL required for JWT issuer (sets the iss claim)")
	}
	if cfg.JWTAudience == "" {
		return nil, errors.New("identity: cfg.JWTAudience required for JWT issuer")
	}
	ttl := cfg.AccessTokenTTL
	if ttl <= 0 {
		ttl = time.Duration(DefaultAccessTokenTTLSeconds) * time.Second
	}
	return &JWTIssuer{
		keys:     km,
		issuer:   cfg.BaseURL,
		audience: cfg.JWTAudience,
		ttl:      ttl,
	}, nil
}

// IssueInput is the per-call payload for IssueAccessToken. Caller
// supplies the user identity; the issuer fills in iat/exp/iss/aud/jti.
type IssueInput struct {
	UserId     string
	Email      string
	Name       string
	GivenName  string
	FamilyName string
	Role       string
	Internal   bool
	Partitions map[string]string
	SessionId  string
	// TTLOverride is the per-call access-token lifetime. When > 0,
	// it replaces the issuer's boot-time default for THIS issuance
	// only. The HTTP handlers read the runtime-tunable value from
	// ClusterSettings (LiveTokenSettings.AccessTokenTTL) and pass
	// it through here so admin-edited TTLs apply on the next mint
	// without an identity restart. 0 = use the issuer default.
	TTLOverride time.Duration
}

// IssueAccessToken signs a fresh JWT for the given input. Returns the
// signed compact-form string plus the absolute expiry time so the
// caller can stamp the right value into v1:identity:authSession.
func (j *JWTIssuer) IssueAccessToken(in IssueInput, now time.Time) (string, time.Time, error) {
	if in.UserId == "" {
		return "", time.Time{}, errors.New("identity: IssueInput.UserId required")
	}
	mat := j.keys.Current()
	if mat == nil {
		return "", time.Time{}, errors.New("identity: no current signing key (was KeyManager.Load() called?)")
	}
	ttl := j.ttl
	if in.TTLOverride > 0 {
		ttl = in.TTLOverride
	}
	expiresAt := now.Add(ttl)

	jti, err := newJTI()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("identity: generate jti: %w", err)
	}

	claims := AccessTokenClaims{
		Email:      in.Email,
		Name:       in.Name,
		GivenName:  in.GivenName,
		FamilyName: in.FamilyName,
		Role:       in.Role,
		Internal:   in.Internal,
		Partitions: in.Partitions,
		SessionId:  in.SessionId,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    j.issuer,
			Subject:   in.UserId,
			Audience:  jwt.ClaimStrings{j.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			ID:        jti,
		},
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = mat.KID

	signed, err := tok.SignedString(mat.Private)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("identity: sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// VerifyAccessToken parses and validates a signed access token against
// the issuer's KeyManager. Returns the claims on success. Used both
// by the identity service itself (for refresh and admin requests) and
// — once Phase 2/5 lands — by every other node's auth interceptor
// against a JWKS-fetched public-key set.
//
// Strict checks: signature with the kid'd key, iss match, aud match,
// exp/nbf bounds, EdDSA algorithm only (downgrade-attack-proof).
func (j *JWTIssuer) VerifyAccessToken(raw string, now time.Time) (*AccessTokenClaims, error) {
	parsed, err := jwt.ParseWithClaims(raw, &AccessTokenClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
			return nil, fmt.Errorf("unexpected signing method %v (expected EdDSA)", t.Method.Alg())
		}
		kidIface, ok := t.Header["kid"]
		if !ok {
			return nil, errors.New("missing kid header")
		}
		kid, ok := kidIface.(string)
		if !ok {
			return nil, errors.New("kid header is not a string")
		}
		// Try current then previous (overlap window).
		if cur := j.keys.Current(); cur != nil && cur.KID == kid {
			return ed25519.PublicKey(cur.Public), nil
		}
		if prev := j.keys.Previous(); prev != nil && prev.KID == kid {
			return ed25519.PublicKey(prev.Public), nil
		}
		return nil, fmt.Errorf("unknown kid %q", kid)
	},
		jwt.WithIssuer(j.issuer),
		jwt.WithAudience(j.audience),
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithLeeway(30*time.Second),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	if err != nil {
		return nil, err
	}
	if !parsed.Valid {
		return nil, errors.New("token marked invalid by parser")
	}
	claims, ok := parsed.Claims.(*AccessTokenClaims)
	if !ok {
		return nil, errors.New("unexpected claims type")
	}
	return claims, nil
}

// newJTI returns a 128-bit random hex id suitable for the JWT `jti`
// claim.
func newJTI() (string, error) {
	const jtiBytes = 16
	buf := make([]byte, jtiBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", buf), nil
}
