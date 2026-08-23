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
//  1. `sub` must already be a canonical v1:identity:user id (every
//     identity-service-issued JWT carries one). Anything else is
//     rejected with ErrUserNotProvisioned.
//  2. userByIdSystem(userId) -> user row (for Role + email). @serverOnly:
//     this is the call that makes caller-scoping circular (#2800).
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

	// #2800: userByIdSystem is @serverOnly -- it projects the full user row
	// (primaryEmail, phone, birthdate, role, ...) and is resolved by a
	// caller-supplied id, so it must not be reachable from the wire. THIS
	// call is the reason it cannot simply be scoped to actor.userId: it runs
	// inside ResolveAccessContext, i.e. it is how the actor gets built. A
	// caller-scope filter here would be circular and would fail auth for
	// everyone.
	//
	// The internal stamp goes on a context this code constructs, never on one
	// handed in by a request handler -- see auth.ContextWithInternalOrigin.
	userQuery := fmt.Sprintf(`query userByIdSystem(userId: %s)`, quoteJSON(userId))
	user, err := r.Engine.ExecuteShaped(ContextWithInternalOrigin(ctx), userQuery)
	if err != nil {
		return nil, fmt.Errorf("userByIdSystem: %w", err)
	}
	userRow := firstRow(user)
	if userRow == nil {
		return nil, ErrUserNotProvisioned
	}

	return &AccessContext{
		UserId:       userId,
		PrimaryEmail: stringField(userRow, "primaryEmail"),
		// Off the SAME row, on the read that has already happened. See the
		// note on AccessContext.DisplayName for why it is not an
		// authorization input and is not in the actor envelope.
		DisplayName: stringField(userRow, "displayName"),
		Role:        applyBadgeRoleCeiling(claims, Role(stringField(userRow, "role"))),
	}, nil
}

// applyBadgeRoleCeiling clamps the resolved role for class="badge"
// shared-terminal operator grants (memql#2513). The grant token's
// role_ceiling claim carries the TERMINAL's role at grant time; the
// operator's effective role is at most that ceiling, so a privileged
// operator badging into a low-privilege kiosk never elevates the
// stream. Every other class passes through unchanged.
func applyBadgeRoleCeiling(claims map[string]any, resolved Role) Role {
	if stringClaim(claims, "class") != "badge" {
		return resolved
	}
	ceiling := Role(strings.ToLower(stringClaim(claims, "role_ceiling")))
	if !IsValidRole(ceiling) {
		// A badge grant without a usable ceiling is clamped to the
		// least-privileged role rather than trusted at row level --
		// fail closed on a malformed grant.
		ceiling = RoleReader
	}
	return RoleAtMost(resolved, ceiling)
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
		// Best-effort, from the OIDC-style name claims the identity issuer
		// stamps (component/identity/jwt.go). This path runs when there is no
		// user row to read yet, so the claim is the only name that exists;
		// when the row lands, LoadFromClaims takes over and the row wins.
		// Empty is fine -- a client renders the email it already holds.
		DisplayName: displayNameFromClaims(claims),
		Role:        applyBadgeRoleCeiling(claims, role),
	}
}

// displayNameFromClaims composes a name out of whatever the token carries.
//
// `name` first when the issuer stamped one, else given_name + family_name
// joined -- the same convention v1:identity:user.displayName documents for
// itself ("firstName + ' ' + lastName when both are populated"), so the
// fallback and the row agree on what a name looks like rather than
// producing two different renderings of one person.
func displayNameFromClaims(claims map[string]any) string {
	if name := stringClaim(claims, "name"); name != "" {
		return name
	}
	parts := make([]string, 0, 2)
	for _, key := range []string{"given_name", "family_name"} {
		if v := stringClaim(claims, key); v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " ")
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
