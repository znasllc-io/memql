package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// QueryRunner is the narrow engine interface the identity resolver
// needs. It executes a MemQL function call and returns the shaped
// output as a Go value. Production passes a thin wrapper around the
// MemQL engine; tests pass a stub.
type QueryRunner interface {
	ExecuteShaped(ctx context.Context, query string) (any, error)
}

// IdentityResolver bundles the engine adapter + logger needed to
// resolve an AccessContext from JWT claims. One instance is shared
// across all streams.
type IdentityResolver struct {
	Engine QueryRunner
	Logger *slog.Logger
}

// NewIdentityResolver constructs an IdentityResolver ready to load
// user context via the engine.
func NewIdentityResolver(engine QueryRunner, logger *slog.Logger) *IdentityResolver {
	return &IdentityResolver{Engine: engine, Logger: logger}
}

// ErrUserNotProvisioned means the caller has a valid JWT but no
// v1:identity:user record exists yet. Callers typically fall back to
// a claims-derived AccessContext so first-login requests racing the
// magic-link verifier's user-row insert don't fail with
// PermissionDenied.
var ErrUserNotProvisioned = errors.New("auth: user not provisioned in database")

// LoadFromClaims resolves the AccessContext for the current JWT
// claims. On success the returned AccessContext has UserId, Role,
// PrimaryEmail, and IdentityId populated.
//
// The lookup walks:
//   1. `sub` must already be a canonical v1:identity:user id (every
//      identity-service-issued JWT carries one). Anything else is
//      rejected with ErrUserNotProvisioned.
//   2. queryUserById(userId) -> user row (for Role + email).
//
// If step 2 returns no rows, ErrUserNotProvisioned is returned so the
// caller can decide whether to short-circuit with a claims-based
// fallback. All other engine errors propagate.
func (r *IdentityResolver) LoadFromClaims(ctx context.Context, claims map[string]any) (*AccessContext, error) {
	if r == nil || r.Engine == nil {
		return nil, errors.New("identity resolver: engine not configured")
	}
	subject := stringClaim(claims, "sub")
	if subject == "" {
		return nil, errors.New("identity resolver: missing subject claim")
	}

	if !strings.HasPrefix(subject, "v1:identity:user:") {
		return nil, ErrUserNotProvisioned
	}
	userId := subject

	userQuery := fmt.Sprintf(`queryUserById({"userId": %s})`, quoteJSON(userId))
	user, err := r.Engine.ExecuteShaped(ctx, userQuery)
	if err != nil {
		return nil, fmt.Errorf("userById: %w", err)
	}
	userRow := firstRow(user)
	if userRow == nil {
		return nil, ErrUserNotProvisioned
	}

	return &AccessContext{
		UserId:       userId,
		PrimaryEmail: stringField(userRow, "primaryEmail"),
		Role:         Role(stringField(userRow, "role")),
	}, nil
}

// FallbackFromClaims returns a best-effort AccessContext derived
// solely from the JWT claims, used when LoadFromClaims reports
// ErrUserNotProvisioned (typically the first request on a brand-new
// login, racing the magic-link verifier's user+identity inserts).
func FallbackFromClaims(claims map[string]any) *AccessContext {
	subject := stringClaim(claims, "sub")
	email := firstNonEmptyClaim(claims, "email", "preferred_username")
	roleStr := stringClaim(claims, "role")
	role := Role(strings.ToLower(strings.TrimSpace(roleStr)))
	if !IsValidRole(role) {
		role = RoleReader
	}
	return &AccessContext{
		UserId:       subject,
		PrimaryEmail: email,
		Role:         role,
	}
}

// --- result helpers ----------------------------------------------------

func firstRow(result any) map[string]any {
	switch v := result.(type) {
	case nil:
		return nil
	case map[string]any:
		if data, ok := v["data"]; ok {
			return firstRow(data)
		}
		return v
	case []any:
		if len(v) == 0 {
			return nil
		}
		if m, ok := v[0].(map[string]any); ok {
			return m
		}
		return nil
	case []map[string]any:
		if len(v) == 0 {
			return nil
		}
		return v[0]
	case json.RawMessage:
		var decoded any
		if err := json.Unmarshal(v, &decoded); err == nil {
			return firstRow(decoded)
		}
	}
	return nil
}

func stringField(row map[string]any, key string) string {
	if row == nil {
		return ""
	}
	if v, ok := row[key].(string); ok {
		return v
	}
	return ""
}

func stringClaim(claims map[string]any, key string) string {
	if claims == nil {
		return ""
	}
	if v, ok := claims[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func firstNonEmptyClaim(claims map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := stringClaim(claims, k); v != "" {
			return v
		}
	}
	return ""
}

func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
