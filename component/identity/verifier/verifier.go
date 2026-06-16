package verifier

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity/pat"
)

// Source identifies which credential family produced the claims.
// Mirrors pat.Source but exists here so callers don't have to import
// the pat package just to switch on it.
type Source string

const (
	SourceJWT Source = "jwt"
	SourcePAT Source = "pat"
)

// ErrUnknownKID is returned by VerifyBearer/verifyJWT when the token's
// kid header names a signing key absent from the (just-refreshed) JWKS
// cache. This is the distinguishing symptom of JWKS incoherence /
// signing-key skew (memql#1523): interceptors classify it as the
// metrics ReasonUnknownKID so the auth-reject-rate alert can name it.
var ErrUnknownKID = errors.New("verifier: unknown kid")

// JWT `class` claim values. Duplicated from
// component/identity.Class* constants so the verifier package
// doesn't need to import the identity package (which would invert
// the layering -- identity is the issuer, verifier is the per-node
// reader). See #105 / #109.
const (
	ClassUser           = "user"
	ClassNode           = "node"
	ClassVoiceAgent     = "voice_agent"
	ClassServiceAccount = "service_account"
)

// VerifiedClaims is the unified shape both the JWT and PAT paths
// produce. Mirrors pat.Claims plus a precomputed claims map for the
// auth-context bridge.
type VerifiedClaims struct {
	UserId     string
	Email      string
	Name       string
	Role       string
	Internal   bool
	Partitions map[string]string // empty for cluster owners + PATs
	SessionId  string            // empty for PATs
	PATID      string            // empty for JWTs
	ExpiresAt  time.Time
	Source     Source

	// RevocationEpoch is the per-user counter snapshot the token was
	// minted at. Populated from the JWT `revocation_epoch` claim;
	// always 0 for PATs (PAT revocation is row-state, not claim-state).
	// Used by EpochResolver-equipped interceptors to invalidate tokens
	// when the user's current epoch has advanced past the claim. See
	// memql#106.
	RevocationEpoch int64

	// Class identifies the credential family the token was minted
	// for. Populated from the JWT `class` claim; empty claim is
	// treated as ClassUser for backward compatibility. PATs surface
	// as a distinct Source (SourcePAT) so they don't need a class
	// claim. Surface-pinned filters read this to keep cross-class
	// tokens off the wrong wire (e.g. NodeService.Stream requires
	// ClassNode). See memql#105.
	Class string
	// NodeId / NodeType are populated for Class == ClassNode tokens.
	// Empty for any other class. The NodeService interceptor
	// cross-checks NodeHello.NodeId / NodeType against these so a
	// node-class token minted for one cluster node can't be replayed
	// to impersonate another.
	NodeId   string
	NodeType string

	// ClaimsMap is the value handed to auth.ContextWithClaims +
	// auth.BuildTokenInfo. Reflects the original JWT body for the
	// JWT path; synthesized for the PAT path so downstream RBAC
	// checks behave identically.
	ClaimsMap map[string]any
}

// PATVerifier is the narrow interface the verifier uses for the
// mql_pat_<...> path. Identity nodes wire a *pat.Verifier; nodes
// without PAT support pass nil and tokens with the prefix are
// rejected outright.
type PATVerifier interface {
	VerifyToken(ctx context.Context, token string) (*pat.Claims, error)
}

// Verifier is the per-node JWT+PAT verifier. Reads from a JWKSCache
// to validate JWTs; delegates to PATVerifier for mql_pat_* tokens.
//
// The verifier itself is a stateless logic object — JWKSCache and
// PATVerifier are the only state it depends on.
type Verifier struct {
	cache    *JWKSCache
	cfg      Config
	pat      PATVerifier
	logger   *slog.Logger
	issuer   string
	audience string
}

// New constructs a Verifier. cache must be non-nil and already
// performed its initial fetch. patVerifier is optional — pass nil
// on nodes that don't support PATs (the identity service is the
// authority that does).
func New(cfg Config, cache *JWKSCache, patVerifier PATVerifier, logger *slog.Logger) (*Verifier, error) {
	if cache == nil {
		return nil, errors.New("verifier.New: cache is required")
	}
	if !cfg.Enabled() {
		return nil, errors.New("verifier.New: config not enabled (IDENTITY_VERIFIER_BASE_URL not set)")
	}
	return &Verifier{
		cache:    cache,
		cfg:      cfg,
		pat:      patVerifier,
		logger:   logger,
		issuer:   cfg.EffectiveIssuer(),
		audience: cfg.EffectiveAudience(),
	}, nil
}

// VerifyBearer validates a bearer token and returns the unified
// claims shape. PAT tokens (mql_pat_*) route through PATVerifier;
// everything else is treated as a JWT.
func (v *Verifier) VerifyBearer(ctx context.Context, token string) (*VerifiedClaims, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("verifier: empty bearer token")
	}
	if pat.IsPATToken(token) {
		return v.verifyPAT(ctx, token)
	}
	return v.verifyJWT(ctx, token)
}

func (v *Verifier) verifyPAT(ctx context.Context, token string) (*VerifiedClaims, error) {
	if v.pat == nil {
		return nil, errors.New("verifier: PAT path not wired on this node")
	}
	c, err := v.pat.VerifyToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errors.New("verifier: PAT verifier returned nil claims")
	}
	cm := map[string]any{
		"sub":      c.UserId,
		"email":    c.Email,
		"name":     c.Name,
		"role":     c.Role,
		"internal": c.Internal,
		"pat_id":   c.PATID,
	}
	return &VerifiedClaims{
		UserId:    c.UserId,
		Email:     c.Email,
		Name:      c.Name,
		Role:      c.Role,
		Internal:  c.Internal,
		PATID:     c.PATID,
		ExpiresAt: c.ExpiresAt,
		Source:    SourcePAT,
		ClaimsMap: cm,
	}, nil
}

func (v *Verifier) verifyJWT(ctx context.Context, token string) (*VerifiedClaims, error) {
	now := time.Now().UTC()

	// Pre-parse to extract the kid header so we can pick the right
	// key (or trigger a force-refresh if the kid isn't cached yet).
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"EdDSA"}))
	unverified, _, err := parser.ParseUnverified(token, jwt.MapClaims{})
	if err != nil {
		return nil, fmt.Errorf("verifier: parse token header: %w", err)
	}
	kid, _ := unverified.Header["kid"].(string)
	if kid == "" {
		return nil, errors.New("verifier: token missing kid header")
	}

	pub, ok := v.cache.PublicKey(kid)
	if !ok {
		// Rotation overlap: identity may have just published a key
		// that hasn't propagated to our cache yet. Force-refresh
		// once and try again.
		if err := v.cache.ForceRefresh(ctx); err != nil {
			v.warn("verifier: force-refresh after unknown kid failed", "kid", kid, "error", err)
		}
		pub, ok = v.cache.PublicKey(kid)
		if !ok {
			return nil, fmt.Errorf("%w %q", ErrUnknownKID, kid)
		}
	}

	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
			return nil, fmt.Errorf("unexpected signing method %v (expected EdDSA)", t.Method.Alg())
		}
		return ed25519.PublicKey(pub), nil
	},
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithLeeway(30*time.Second),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	if err != nil {
		return nil, fmt.Errorf("verifier: validate token: %w", err)
	}
	if !parsed.Valid {
		return nil, errors.New("verifier: token marked invalid by parser")
	}

	mc, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("verifier: unexpected claims type")
	}
	cm := map[string]any(mc)

	// Resolve the class claim with the backward-compat fallback:
	// missing -> ClassUser. Centralized so downstream filters can
	// switch on it without re-applying the fallback.
	class := stringClaim(cm, "class")
	if class == "" {
		class = ClassUser
	}
	out := &VerifiedClaims{
		UserId:          stringClaim(cm, "sub"),
		Email:           stringClaim(cm, "email"),
		Name:            stringClaim(cm, "name"),
		Role:            stringClaim(cm, "role"),
		Internal:        boolClaim(cm, "internal"),
		SessionId:       stringClaim(cm, "sid"),
		RevocationEpoch: int64Claim(cm, "revocation_epoch"),
		Class:           class,
		NodeId:          stringClaim(cm, "node_id"),
		NodeType:        stringClaim(cm, "node_type"),
		Source:          SourceJWT,
		ClaimsMap:       cm,
	}
	if exp, ok := numericDate(cm, "exp"); ok {
		out.ExpiresAt = exp
	}
	if parts, ok := cm["partitions"].(map[string]any); ok {
		out.Partitions = make(map[string]string, len(parts))
		for k, v := range parts {
			if s, ok := v.(string); ok {
				out.Partitions[k] = s
			}
		}
	}
	return out, nil
}

// AttachToContext stamps the verified claims onto the context using
// the existing component/auth helpers. The returned context carries
// both the claims map and the structured TokenInfo so RBAC, identity
// resolution, and the security helpers all work without changes.
func AttachToContext(ctx context.Context, vc *VerifiedClaims) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if vc == nil {
		return ctx
	}
	ctx = auth.ContextWithClaims(ctx, vc.ClaimsMap)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(vc.ClaimsMap))
	return ctx
}

func (v *Verifier) warn(msg string, args ...any) {
	if v == nil || v.logger == nil {
		return
	}
	v.logger.Warn(msg, args...)
}

// ---------------------------------------------------------------------------
// claim extraction helpers — kept private to keep the package small
// ---------------------------------------------------------------------------

func stringClaim(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func boolClaim(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

// int64Claim extracts a JWT numeric claim into an int64. JSON numbers
// arrive as float64 on the jwt.MapClaims path; we fan out the integer
// types for completeness so a code path that hands us int / int64
// directly (synthesized claims maps in tests) still works.
func int64Claim(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case int32:
		return int64(v)
	}
	return 0
}

func numericDate(m map[string]any, key string) (time.Time, bool) {
	if m == nil {
		return time.Time{}, false
	}
	switch v := m[key].(type) {
	case float64:
		return time.Unix(int64(v), 0).UTC(), true
	case int64:
		return time.Unix(v, 0).UTC(), true
	case int:
		return time.Unix(int64(v), 0).UTC(), true
	}
	return time.Time{}, false
}
