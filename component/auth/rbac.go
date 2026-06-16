package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type (
	Role string

	UserContext struct {
		ID     string
		Email  string
		Role   Role
		Groups []string
	}
)

const (
	RoleOwner Role = "owner"
	RoleAdmin Role = "admin"
	// RoleDeveloper is engineering power, not admin power (MCP epic #1529,
	// Phase 2 #1532): it can author constructs + run inline DSL + write data,
	// but CANNOT manage users/identities (that stays admin/owner). owner is a
	// superset of developer; admin gains neither authoring nor inline.
	RoleDeveloper Role = "developer"
	RoleWriter    Role = "writer"
	RoleReader    Role = "reader"
)

// ValidRoles returns all valid role values.
func ValidRoles() []Role {
	return []Role{RoleOwner, RoleAdmin, RoleDeveloper, RoleWriter, RoleReader}
}

// RoleLevel returns the numeric privilege level for a role.
// Lower values indicate higher privilege: owner=0, admin=1, writer=2, reader=3.
func RoleLevel(r Role) int {
	switch r {
	case RoleOwner:
		return 0
	case RoleAdmin:
		return 1
	// developer sits in the privileged tier alongside admin (different power
	// axis -- engineering, not user-management). The numeric level is used only
	// for delegation-ceiling capping (RoleAtMost); 1 keeps a developer above
	// writer/reader so a writer/reader ceiling caps it down, and a developer
	// ceiling never elevates a lower role.
	case RoleDeveloper:
		return 1
	case RoleWriter:
		return 2
	case RoleReader:
		return 3
	default:
		return 3 // Unknown roles treated as least privileged
	}
}

// RoleAtMost returns the more restrictive of two roles (higher privilege level).
// Used to cap an agent's effective role to the delegation ceiling.
func RoleAtMost(identity, ceiling Role) Role {
	if RoleLevel(identity) >= RoleLevel(ceiling) {
		return identity
	}
	return ceiling
}

// ScopeAllows checks if a set of delegation scopes permits a given operation.
// An empty scope set means all operations are allowed.
// Scopes support glob-style matching with * wildcards:
//
//	"query:*"              matches any query operation
//	"mutation:cognition.*" matches any cognition mutation
//	"*"                    matches everything
func ScopeAllows(scopes []string, operation string) bool {
	if len(scopes) == 0 {
		return true
	}

	for _, scope := range scopes {
		if scopeMatches(scope, operation) {
			return true
		}
	}
	return false
}

// scopeMatches checks if a single scope pattern matches an operation.
func scopeMatches(pattern, operation string) bool {
	if pattern == "*" {
		return true
	}

	if !strings.Contains(pattern, "*") {
		return pattern == operation
	}

	// Handle trailing wildcard: "query:*" matches "query:activeAgents"
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(operation, prefix)
	}

	return false
}

// EffectiveRole returns the role an agent effectively operates under,
// considering the delegation ceiling and the delegating identity's role.
func EffectiveRole(dc *DelegationContext) Role {
	if dc == nil {
		return RoleReader
	}

	identityRole := Role(dc.DelegatingIdentity.Role)
	if !isValidRole(identityRole) {
		identityRole = RoleReader
	}

	return RoleAtMost(identityRole, dc.RoleCeiling)
}

// IsDelegationExpired returns true if the delegation has a non-zero expiry
// that is in the past.
func IsDelegationExpired(dc *DelegationContext) bool {
	if dc == nil {
		return true
	}
	if dc.ExpiresAt.IsZero() {
		return false // Persistent delegation
	}
	return time.Now().After(dc.ExpiresAt)
}

// UserFromContext extracts the authenticated user from the request context.
func UserFromContext(ctx context.Context) (UserContext, bool) {
	claims, ok := ClaimsFromContext(ctx)
	if !ok {
		return UserContext{}, false
	}

	id := stringClaimRBAC(claims, "sub")
	if id == "" {
		// Fall back to email when subject is missing. Authorizer always provides
		// an email claim for authenticated users and it remains stable per user.
		id = stringClaimRBAC(claims, "email")
	}
	if id == "" {
		return UserContext{}, false
	}

	role := resolveRoleFromClaims(claims)
	groups := extractGroupsFromClaims(claims)

	user := UserContext{
		ID:     id,
		Email:  stringClaimRBAC(claims, "email"),
		Role:   Role(role),
		Groups: groups,
	}

	// When delegation context is present, cap the effective role to the
	// delegation ceiling so delegated agents cannot exceed their grant.
	if dc, ok := DelegationFromContext(ctx); ok && !IsDelegationExpired(dc) {
		user.Role = EffectiveRole(dc)
	}

	return user, true
}

// IsOwner returns true if the user has the owner role.
func IsOwner(u UserContext) bool {
	return u.Role == RoleOwner
}

// IsAdmin returns true if the user has the admin role.
func IsAdmin(u UserContext) bool {
	return u.Role == RoleAdmin
}

// IsWriter returns true if the user has the writer role.
func IsWriter(u UserContext) bool {
	return u.Role == RoleWriter
}

// IsReader returns true if the user has the reader role.
func IsReader(u UserContext) bool {
	return u.Role == RoleReader
}

// IsPrivilegedUser returns true when the user has owner or admin role.
// This is used to determine access to privileged features like the System assistant.
func IsPrivilegedUser(u UserContext) bool {
	return u.Role == RoleOwner || u.Role == RoleAdmin
}

// AtLeastAdmin returns true if the user has owner or admin role.
func AtLeastAdmin(u UserContext) bool {
	return u.Role == RoleOwner || u.Role == RoleAdmin
}

// CanWrite returns true if the user has owner, admin, developer, or writer
// role. Readers cannot write.
func CanWrite(u UserContext) bool {
	return u.Role == RoleOwner || u.Role == RoleAdmin || u.Role == RoleDeveloper || u.Role == RoleWriter
}

// CanAuthor reports whether the user may author DSL constructs (the MCP
// `define` op, Tier 2). Engineering power: owner or developer only -- admin
// does NOT gain authoring (#1529 §4).
func CanAuthor(u UserContext) bool {
	return u.Role == RoleOwner || u.Role == RoleDeveloper
}

// CanRunInline reports whether the user may run ad-hoc inline DSL (the MCP
// `query` op + inline definitions, Tier 3). owner or developer only.
func CanRunInline(u UserContext) bool {
	return u.Role == RoleOwner || u.Role == RoleDeveloper
}

// CanRead returns true if the user has any valid role. All four roles can read.
func CanRead(u UserContext) bool {
	return IsValidRole(u.Role)
}

// CanCreateAgent returns true if the user can create agents (owner or admin only).
func CanCreateAgent(u UserContext) bool {
	return AtLeastAdmin(u)
}

// CanManageGroup returns true if the user can manage groups (owner or admin only).
func CanManageGroup(u UserContext) bool {
	return AtLeastAdmin(u)
}

// CanViewUser returns true if the caller can view the target user.
// Owner sees everyone. Admin sees everyone except other owners. Writers
// and readers see only themselves (user management is not their concern).
func CanViewUser(caller, target UserContext) bool {
	if caller.ID != "" && caller.ID == target.ID {
		return true
	}

	switch caller.Role {
	case RoleOwner:
		return true
	case RoleAdmin:
		return target.Role != RoleOwner
	case RoleWriter, RoleReader:
		return false
	default:
		return false
	}
}

// CanManageUser returns true if the caller can manage the target user.
// Owners manage everyone (except they cannot manage other owners, preventing
// mutual-demotion lockout). Admins manage writers and readers. Writers and
// readers have no management authority beyond themselves.
func CanManageUser(caller, target UserContext) bool {
	if caller.Role == RoleOwner && target.Role == RoleOwner && caller.ID != target.ID {
		return false
	}

	if caller.ID != "" && caller.ID == target.ID {
		return true
	}

	switch caller.Role {
	case RoleOwner:
		return true
	case RoleAdmin:
		return target.Role == RoleWriter || target.Role == RoleReader
	case RoleWriter, RoleReader:
		return false
	default:
		return false
	}
}

// CanDeleteUser returns true if the caller can delete the target user.
func CanDeleteUser(caller, target UserContext) bool {
	if !CanManageUser(caller, target) {
		return false
	}

	if caller.Role == RoleOwner && caller.ID == target.ID {
		return false
	}

	return true
}

// Unauthorized writes an HTTP 401 Unauthorized response.
func Unauthorized(w http.ResponseWriter) {
	if w == nil {
		return
	}
	http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
}

// Forbidden writes an HTTP 403 Forbidden response.
func Forbidden(w http.ResponseWriter) {
	if w == nil {
		return
	}
	http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
}

func stringClaimRBAC(claims map[string]any, key string) string {
	if claims == nil {
		return ""
	}

	value, ok := claims[key]
	if !ok {
		return ""
	}

	return strings.TrimSpace(fmt.Sprint(value))
}

func normalizeRole(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// migrateRole maps legacy role values to the current role system
// (owner / admin / developer / writer / reader).
//
// Mapping rationale:
//   - developer: now a FIRST-CLASS role (#1532) -- no longer migrated to
//     admin; it passes through unchanged.
//   - manager: retired middle-band role, map to writer (keeps write access
//     but drops the ill-defined user-management authority).
//   - user: retired default role, map to reader (view-only).
//   - advocate / member / guest: early experimental labels, all mapped to
//     the most restrictive role (reader).
func migrateRole(role string) string {
	switch role {
	case "manager":
		return "writer"
	case "user":
		return "reader"
	case "advocate", "member", "guest":
		return "reader"
	default:
		return role
	}
}

// IsValidRole returns true if the given role is one of the valid roles.
func IsValidRole(role Role) bool {
	switch role {
	case RoleOwner, RoleAdmin, RoleDeveloper, RoleWriter, RoleReader:
		return true
	default:
		return false
	}
}

// isValidRole is an unexported alias kept for backward compatibility within this package.
func isValidRole(role Role) bool {
	return IsValidRole(role)
}

func resolveRoleFromClaims(claims map[string]any) Role {
	if claims == nil {
		return RoleReader
	}

	candidate := normalizeRole(stringClaimRBAC(claims, "role"))
	if candidate == "" {
		candidate = extractRoleFromRolesClaim(claims)
	}

	// Migrate legacy role values
	candidate = migrateRole(candidate)

	role := Role(candidate)
	if !isValidRole(role) {
		return RoleReader
	}
	return role
}

func extractRoleFromRolesClaim(claims map[string]any) string {
	raw, ok := claims["roles"]
	if !ok || raw == nil {
		return ""
	}

	switch typed := raw.(type) {
	case []string:
		for _, entry := range typed {
			if normalized := normalizeRole(entry); normalized != "" {
				return normalized
			}
		}
	case []any:
		for _, entry := range typed {
			if str, ok := entry.(string); ok {
				if normalized := normalizeRole(str); normalized != "" {
					return normalized
				}
			}
		}
	case string:
		for _, entry := range strings.Fields(typed) {
			if normalized := normalizeRole(entry); normalized != "" {
				return normalized
			}
		}
	}

	return ""
}

// extractGroupsFromClaims reads the groups claim from JWT claims.
func extractGroupsFromClaims(claims map[string]any) []string {
	if claims == nil {
		return nil
	}

	raw, ok := claims["groups"]
	if !ok || raw == nil {
		return nil
	}

	switch typed := raw.(type) {
	case []string:
		return typed
	case []any:
		result := make([]string, 0, len(typed))
		for _, entry := range typed {
			if str, ok := entry.(string); ok {
				result = append(result, str)
			}
		}
		return result
	}

	return nil
}
