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
//
// E1.5 (memql#2073): this legacy level is now DERIVED from the DSL-aligned rank
// model (rbac_model.go: roleRank, where HIGHER == more privileged) so there is
// one source of truth. The mapping preserves the historical levels exactly:
// owner=0, admin/developer=1 (the privileged tier -- different power axes,
// same delegation-capping level), writer=2, reader=3. The level is used only
// for delegation-ceiling capping (RoleAtMost); the relative ordering it
// encodes is what matters, not the absolute numbers.
func RoleLevel(r Role) int {
	switch roleRank(r) {
	case rankOwner:
		return 0
	case rankAdmin, rankDeveloper:
		return 1
	case rankUser:
		return 2
	default:
		// reader (viewer tier) + unknown roles: least privileged.
		return 3
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
// This is used to determine access to privileged features like the System
// assistant. E1.5 (memql#2073): "privileged" == holds user-management
// authority == the create-on-principal capability (owner + admin in the DSL
// model). Developer is engineering power and is deliberately NOT privileged
// here, exactly as before.
func IsPrivilegedUser(u UserContext) bool {
	return roleHasCapability(u.Role, "create", "principal")
}

// AtLeastAdmin returns true if the user has owner or admin role. E1.5: this is
// the user-management gate == the create-on-principal capability (owner +
// admin), preserving the prior owner||admin result.
func AtLeastAdmin(u UserContext) bool {
	return roleHasCapability(u.Role, "create", "principal")
}

// AtLeastDeveloper returns true if the user has owner, admin, or developer
// role. This is the deploy-action gate for cutting versions + deploying
// (memql#1876): engineering power (developer) plus the two admin tiers can
// ship, while writer/reader stay read-only. Rollback keeps the stricter
// AtLeastAdmin gate -- developer can deploy forward but not roll back. E1.5:
// the gate == the execute-on-deployment capability (owner/developer/admin).
func AtLeastDeveloper(u UserContext) bool {
	return roleHasCapability(u.Role, "execute", "deployment")
}

// CanWrite returns true if the user has owner, admin, developer, or writer
// role. Readers cannot write. E1.5: == the create-on-data capability (the
// read/write data plane); reader (viewer tier) lacks it.
func CanWrite(u UserContext) bool {
	return roleHasCapability(u.Role, "create", "data")
}

// CanAuthor reports whether the user may author DSL constructs (the MCP
// `define` op, Tier 2). Engineering power: owner or developer only -- admin
// does NOT gain authoring (#1529 §4). E1.5: == the create-on-construct
// capability.
func CanAuthor(u UserContext) bool {
	return roleHasCapability(u.Role, "create", "construct")
}

// CanRunInline reports whether the user may run ad-hoc inline DSL (the MCP
// `query` op + inline definitions, Tier 3). owner or developer only. E1.5: ==
// the execute-on-construct capability.
func CanRunInline(u UserContext) bool {
	return roleHasCapability(u.Role, "execute", "construct")
}

// CanRead returns true if the user has any valid role. All five slugs can
// read. E1.5: == the read-on-data capability, which every valid role holds.
func CanRead(u UserContext) bool {
	return roleHasCapability(u.Role, "read", "data")
}

// CanCreateAgent returns true if the user can create agents (owner or admin
// only). E1.5: == the create-on-agent capability.
func CanCreateAgent(u UserContext) bool {
	return roleHasCapability(u.Role, "create", "agent")
}

// CanManageGroup returns true if the user can manage groups (owner or admin
// only). E1.5: == the create-on-group capability.
func CanManageGroup(u UserContext) bool {
	return roleHasCapability(u.Role, "create", "group")
}

// CanViewUser returns true if the caller can view the target user.
// Owner sees everyone. Admin sees everyone except other owners. Writers
// and readers see only themselves (user management is not their concern).
//
// E1.5 (memql#2073): expressed via the model -- a caller may view a target if
// they're the same user, OR they hold read-on-principal AND can reach the
// target's rank. read-on-principal is held by owner/developer/admin in the DSL
// model; the prior behavior restricted *viewing other users* to owner/admin
// (developer was never wired into CanViewUser callers for cross-user reads),
// so the management-authority gate (create-on-principal == owner/admin) is the
// faithful reproduction here. The owner-target carve-out (admin can't see
// other owners) falls out of the rank rule: admin (200) cannot out-rank an
// owner (400). A conformance test pins the full truth table.
func CanViewUser(caller, target UserContext) bool {
	if caller.ID != "" && caller.ID == target.ID {
		return true
	}
	if !roleHasCapability(caller.Role, "read", "principal") || !roleHasCapability(caller.Role, "create", "principal") {
		// Only user-managers (owner/admin -- they hold read AND create on the
		// principal resource) may view OTHER users. developer holds read on
		// principal but not create, so it is not a cross-user viewer here --
		// matching the prior behavior (writer/reader/developer: self only).
		return false
	}
	// owner sees everyone; admin sees everyone except other owners.
	return caller.Role == RoleOwner || target.Role != RoleOwner
}

// CanManageUser returns true if the caller can manage the target user.
// Owners manage everyone (except they cannot manage other owners, preventing
// mutual-demotion lockout). Admins manage writers and readers. Writers and
// readers have no management authority beyond themselves.
//
// E1.5 (memql#2073): expressed via the model. The caller must hold
// update-on-principal (user-management authority == owner/admin); then the
// relational rule is "self OR out-rank the target", with the owner-can't-
// manage-other-owner lockout carve-out preserved explicitly (it differs from
// the E1.3 GovernPrincipal owner-manages-owner rule precisely because this
// legacy helper prevents mutual owner demotion; #2074 reconciles enforcement).
// A conformance test pins the full truth table against the prior implementation.
func CanManageUser(caller, target UserContext) bool {
	// Owner mutual-demotion lockout: an owner cannot manage ANOTHER owner.
	if caller.Role == RoleOwner && target.Role == RoleOwner && caller.ID != target.ID {
		return false
	}

	if caller.ID != "" && caller.ID == target.ID {
		return true
	}

	if !roleHasCapability(caller.Role, "update", "principal") {
		return false // not a user-manager (writer/reader/developer): self only.
	}
	// owner manages everyone (modulo the owner-owner lockout handled above);
	// admin manages a VALID target it strictly out-ranks (writer/reader tier).
	// The IsValidRole guard preserves the prior behavior where admin manages
	// only the writer/reader slugs -- an unknown/invalid target slug is not
	// manageable (it isn't a real principal in the role model).
	if caller.Role == RoleOwner {
		return true
	}
	return IsValidRole(target.Role) && roleRank(caller.Role) > roleRank(target.Role)
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
