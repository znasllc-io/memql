package auth

import (
	"context"
	"errors"
	"strings"
	"time"
)

const developerRole = "developer"

// UserIdentity captures the normalized login context for an authenticated user.
type UserIdentity struct {
	Subject     string
	Email       string
	PhoneNumber string
	Role        string
	FirstName   string
	LastName    string
}

// DelegationContext captures the identity chain when an agent acts under
// delegated authority. It is attached to the request context alongside
// the standard UserIdentity.
type DelegationContext struct {
	// DelegationId is the node ID of the v1:identity:delegation record.
	DelegationId string
	// IdentityId is the v1:identity:identity node ID.
	IdentityId string
	// DelegatingIdentity is the resolved identity details of the delegating party.
	DelegatingIdentity UserIdentity
	// IdentityType is "human" or "synthetic".
	IdentityType string
	// GuardianSubject is the authenticated subject of the guardian (for synthetic identities).
	GuardianSubject string
	// AgentId is the v1:agents:agent node ID performing the action.
	AgentId string
	// RoleCeiling is the maximum role allowed under this delegation.
	RoleCeiling Role
	// Scopes is the set of allowed operations (empty = all within role ceiling).
	Scopes []string
	// ExpiresAt is when this delegation expires (zero value = persistent).
	ExpiresAt time.Time
}

// ErrUserIdentityNotFound indicates that an authenticated identity could not be resolved.
var ErrUserIdentityNotFound = errors.New("user identity not available in context")

// UserIdentityFromContext extracts the best-effort user identity from the request context.
// The result may have empty fields if the upstream token omitted them.
func UserIdentityFromContext(ctx context.Context) (UserIdentity, error) {
	token := TokenInfoFromContext(ctx)
	if token == nil {
		return UserIdentity{}, ErrUserIdentityNotFound
	}
	return buildIdentityFromToken(token), nil
}

func buildIdentityFromToken(token *TokenInfo) UserIdentity {
	id := UserIdentity{
		Subject: strings.TrimSpace(token.Subject),
	}

	id.Email = firstNonEmpty(
		claimValue(token.Claims, "email"),
		claimValue(token.Claims, "preferred_username"),
		claimValue(token.Claims, "user"),
		id.Subject,
	)

	id.PhoneNumber = firstNonEmpty(
		claimValue(token.Claims, "phoneNumber"),
		claimValue(token.Claims, "phone_number"),
		claimValue(token.Claims, "phone"),
	)

	id.Role = resolveIdentityRoleFromClaims(token.Claims)

	first, last := extractNamesFromClaims(token.Claims)
	id.FirstName = first
	id.LastName = last

	return id
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func claimValue(claims map[string]any, key string) string {
	if claims == nil {
		return ""
	}
	if value, ok := claimString(claims, key); ok {
		return value
	}
	return ""
}

var identityRoleClaimKeys = []string{"role", "roles", "allowed_roles", "allowedRoles", "default_role", "defaultRole"}

func resolveIdentityRoleFromClaims(claims map[string]any) string {
	candidates := collectIdentityRoleClaims(claims)
	return preferredRole(candidates)
}

func collectIdentityRoleClaims(claims map[string]any) []string {
	if len(claims) == 0 {
		return nil
	}

	seen := make(map[string]struct{})
	var roles []string

	addRole := func(role string) {
		trimmed := strings.TrimSpace(role)
		if trimmed == "" {
			return
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		roles = append(roles, trimmed)
	}

	gatherIdentityRolesFromMap(claims, addRole)

	if meta, ok := claims["app_metadata"].(map[string]any); ok {
		gatherIdentityRolesFromMap(meta, addRole)
	}
	if user, ok := claims["user"].(map[string]any); ok {
		gatherIdentityRolesFromMap(user, addRole)
	}

	return roles
}

func gatherIdentityRolesFromMap(source map[string]any, add func(string)) {
	if source == nil {
		return
	}
	for _, key := range identityRoleClaimKeys {
		if value, ok := source[key]; ok {
			appendIdentityRolesFromClaimValue(value, add)
		}
	}
}

func appendIdentityRolesFromClaimValue(value any, add func(string)) {
	if value == nil {
		return
	}
	switch typed := value.(type) {
	case string:
		add(typed)
	case []string:
		for _, entry := range typed {
			add(entry)
		}
	case []any:
		for _, entry := range typed {
			appendIdentityRolesFromClaimValue(entry, add)
		}
	case map[string]any:
		gatherIdentityRolesFromMap(typed, add)
	}
}

func preferredRole(candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	for _, role := range candidates {
		if strings.EqualFold(role, developerRole) {
			return strings.TrimSpace(role)
		}
	}
	return candidates[0]
}

func extractNamesFromClaims(claims map[string]any) (string, string) {
	first := firstNonEmpty(
		claimValue(claims, "given_name"),
		claimValue(claims, "givenName"),
		claimValue(claims, "first_name"),
		claimValue(claims, "firstName"),
	)
	last := firstNonEmpty(
		claimValue(claims, "family_name"),
		claimValue(claims, "familyName"),
		claimValue(claims, "last_name"),
		claimValue(claims, "lastName"),
	)

	if first == "" || last == "" {
		if full := claimValue(claims, "name"); full != "" {
			fullFirst, fullLast := splitFullName(full)
			if first == "" {
				first = fullFirst
			}
			if last == "" {
				last = fullLast
			}
		}
	}

	return first, last
}

func splitFullName(value string) (string, string) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", ""
	}
	parts := strings.Fields(trimmed)
	if len(parts) == 0 {
		return "", ""
	}
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], strings.Join(parts[1:], " ")
}
