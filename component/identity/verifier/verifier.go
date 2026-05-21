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

	// RevocationEpoch is the value of v1:identity:user.revocationEpoch
	// at the moment the JWT was minted. The verifier rejects any
	// token whose claim is strictly less than the user's current
	// epoch -- bumping the user's epoch (via
	// mutationBumpUserRevocationEpoch) invalidates every pre-bump
	// token. Zero for PATs (PATs have their own revocation surface).
	// See #106 + threat-model §5.3.
	RevocationEpoch int64

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

// EpochResolver returns the current v1:identity:user.revocationEpoch
// for the supplied user id. Implementations typically run a memQL
// query (queryUserById) and read payload.revocationEpoch. The
// verifier calls this once per stream-open and again on a periodic
// in-stream re-check; missing / failed reads (err != nil) are
// treated as "trust the token" so a transient identity-service
// outage doesn't disconnect every authenticated stream.
//
// Returning (0, nil) is the no-revocation case -- a user who has
// never been bulk-revoked has epoch 0 by default, and tokens with
// claim 0 are admitted (the rejection rule is strict-greater).
//
// nil resolver disables the epoch check entirely. Useful for the
// identity service binary, which already owns the source of truth
// and doesn't need to ask itself.
type EpochResolver func(ctx context.Context, userId string) (int64, error)

// Verifier is the per-node JWT+PAT verifier. Reads from a JWKSCache
// to validate JWTs; delegates to PATVerifier for mql_pat_* tokens.
//
// The verifier itself is a stateless logic object — JWKSCache,
// PATVerifier, and the optional EpochResolver are the only state
// it depends on.
type Verifier struct {
	cache         *JWKSCache
	cfg           Config
	pat           PATVerifier
	logger        *slog.Logger
	issuer        string
	audience      string
	epochResolver EpochResolver
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

// WithEpochResolver wires the revocation-epoch resolver onto the
// verifier. Returns the same *Verifier for fluent setup; the
// resolver is applied in place. Call once during binary bootstrap
// after the engine is up but before any auth-gated stream starts.
//
// Passing nil clears the resolver -- the epoch check becomes a
// no-op. Useful for tests that don't want to spin up the lookup.
func (v *Verifier) WithEpochResolver(r EpochResolver) *Verifier {
	if v == nil {
		return v
	}
	v.epochResolver = r
	return v
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
			return nil, fmt.Errorf("verifier: unknown kid %q", kid)
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

	out := &VerifiedClaims{
		UserId:          stringClaim(cm, "sub"),
		Email:           stringClaim(cm, "email"),
		Name:            stringClaim(cm, "name"),
		Role:            stringClaim(cm, "role"),
		Internal:        boolClaim(cm, "internal"),
		SessionId:       stringClaim(cm, "sid"),
		RevocationEpoch: int64Claim(cm, "revocation_epoch"),
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

	// Per-stream revocation check. Resolves the user's current
	// epoch and rejects when the token's claim is stale. No-op when
	// the resolver isn't wired (e.g. identity service binary), or
	// when the resolver itself errs (transient lookup failure -- we
	// don't want to disconnect every authenticated stream because
	// one DB query timed out). Run AFTER the standard JWT validation
	// so we don't pay a DB roundtrip for malformed-token requests.
	if v.epochResolver != nil && out.UserId != "" {
		current, rerr := v.epochResolver(ctx, out.UserId)
		if rerr == nil && current > out.RevocationEpoch {
			return nil, fmt.Errorf("verifier: token revoked (epoch %d, current %d)", out.RevocationEpoch, current)
		}
		if rerr != nil {
			v.warn("verifier: revocation epoch resolve failed (fail-open)", "user_id", out.UserId, "error", rerr)
		}
	}

	return out, nil
}

// BindRevocationWatcher returns a context that gets canceled when
// the user's revocationEpoch advances past the token's claim. Caller
// should use the returned context for the duration of the stream;
// downstream handlers see ctx.Err() == context.Canceled when a bulk-
// revoke bump invalidates them.
//
// No-op (returns parent unchanged) when:
//   - vc is nil
//   - the verifier has no EpochResolver wired (resolver is the only
//     way to know if the epoch has moved)
//   - RevocationCheckInterval is zero or negative (caller opt-out)
//   - vc.Source != SourceJWT (PATs have a separate revocation surface)
//
// Per-stream goroutine; cheap (one timer, one DB read per interval).
// The watcher exits when the parent context is canceled OR when the
// epoch rolls forward.
func (v *Verifier) BindRevocationWatcher(parent context.Context, vc *VerifiedClaims) context.Context {
	if v == nil || vc == nil || v.epochResolver == nil {
		return parent
	}
	if vc.Source != SourceJWT {
		return parent
	}
	interval := v.cfg.RevocationCheckInterval
	if interval <= 0 {
		return parent
	}
	ctx, cancel := context.WithCancel(parent)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, err := v.epochResolver(ctx, vc.UserId)
				if err != nil {
					v.warn("verifier: revocation-watcher resolve failed (continuing)",
						"user_id", vc.UserId, "error", err)
					continue
				}
				if current > vc.RevocationEpoch {
					v.warn("verifier: revocation-watcher canceling stream",
						"user_id", vc.UserId,
						"token_epoch", vc.RevocationEpoch,
						"current_epoch", current)
					cancel()
					return
				}
			}
		}
	}()
	return ctx
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

// int64Claim extracts a numeric claim as int64. JSON numbers arrive
// as float64 from jwt.MapClaims; truncate any fractional component.
// Missing / non-numeric claims return 0 (the no-revocation baseline).
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
