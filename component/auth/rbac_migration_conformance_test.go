package auth

import "testing"

// allSlugs is every role slug the migrated helpers must keep behaving
// identically for -- the five legacy slugs plus an unknown slug (which must
// stay least-privileged).
var allSlugs = []Role{RoleOwner, RoleAdmin, RoleDeveloper, RoleWriter, RoleReader, Role("bogus")}

// --- legacy oracles: the EXACT pre-E1.5 implementations, inlined here so the
//     conformance test pins that the capability-model refactor preserves every
//     boolean result byte-for-byte. If a future edit to the model changes a
//     result, this test fails with the offending (slug, helper) pair.

func legacyIsPrivileged(r Role) bool { return r == RoleOwner || r == RoleAdmin }
func legacyAtLeastAdmin(r Role) bool { return r == RoleOwner || r == RoleAdmin }
func legacyAtLeastDeveloper(r Role) bool {
	return r == RoleOwner || r == RoleAdmin || r == RoleDeveloper
}
func legacyCanWrite(r Role) bool {
	return r == RoleOwner || r == RoleAdmin || r == RoleDeveloper || r == RoleWriter
}
func legacyCanAuthor(r Role) bool    { return r == RoleOwner || r == RoleDeveloper }
func legacyCanRunInline(r Role) bool { return r == RoleOwner || r == RoleDeveloper }
func legacyCanRead(r Role) bool      { return IsValidRole(r) }
func legacyCanCreateAgent(r Role) bool {
	return legacyAtLeastAdmin(r)
}
func legacyCanManageGroup(r Role) bool { return legacyAtLeastAdmin(r) }

func legacyRoleLevel(r Role) int {
	switch r {
	case RoleOwner:
		return 0
	case RoleAdmin:
		return 1
	case RoleDeveloper:
		return 1
	case RoleWriter:
		return 2
	case RoleReader:
		return 3
	default:
		return 3
	}
}

func legacyCanViewUser(caller, target UserContext) bool {
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

func legacyCanManageUser(caller, target UserContext) bool {
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

func legacyCanDeleteUser(caller, target UserContext) bool {
	if !legacyCanManageUser(caller, target) {
		return false
	}
	if caller.Role == RoleOwner && caller.ID == target.ID {
		return false
	}
	return true
}

// TestRBACMigrationPreservesUnaryHelpers pins that every single-role Can*
// helper returns the SAME result as its pre-E1.5 implementation, for every
// slug. This is the "no behavioral RBAC regression" acceptance gate.
func TestRBACMigrationPreservesUnaryHelpers(t *testing.T) {
	for _, slug := range allSlugs {
		u := UserContext{ID: "u", Role: slug}
		checks := []struct {
			name           string
			got, wantOracle bool
		}{
			{"IsPrivilegedUser", IsPrivilegedUser(u), legacyIsPrivileged(slug)},
			{"AtLeastAdmin", AtLeastAdmin(u), legacyAtLeastAdmin(slug)},
			{"AtLeastDeveloper", AtLeastDeveloper(u), legacyAtLeastDeveloper(slug)},
			{"CanWrite", CanWrite(u), legacyCanWrite(slug)},
			{"CanAuthor", CanAuthor(u), legacyCanAuthor(slug)},
			{"CanRunInline", CanRunInline(u), legacyCanRunInline(slug)},
			{"CanRead", CanRead(u), legacyCanRead(slug)},
			{"CanCreateAgent", CanCreateAgent(u), legacyCanCreateAgent(slug)},
			{"CanManageGroup", CanManageGroup(u), legacyCanManageGroup(slug)},
		}
		for _, c := range checks {
			if c.got != c.wantOracle {
				t.Errorf("%s(%q) = %v, want %v (behavioral regression vs pre-E1.5)", c.name, slug, c.got, c.wantOracle)
			}
		}
		if got, want := RoleLevel(slug), legacyRoleLevel(slug); got != want {
			t.Errorf("RoleLevel(%q) = %d, want %d (delegation-cap ordering regression)", slug, got, want)
		}
	}
}

// TestRBACMigrationPreservesRelationalHelpers pins CanViewUser / CanManageUser
// / CanDeleteUser across EVERY (caller, target) slug pair AND both the self
// (same id) and cross-user (different id) cases -- the full truth table.
func TestRBACMigrationPreservesRelationalHelpers(t *testing.T) {
	ids := [][2]string{{"a", "a"}, {"a", "b"}} // self, then distinct
	for _, callerSlug := range allSlugs {
		for _, targetSlug := range allSlugs {
			for _, pair := range ids {
				caller := UserContext{ID: pair[0], Role: callerSlug}
				target := UserContext{ID: pair[1], Role: targetSlug}

				if got, want := CanViewUser(caller, target), legacyCanViewUser(caller, target); got != want {
					t.Errorf("CanViewUser(caller=%q/%s, target=%q/%s) = %v, want %v",
						pair[0], callerSlug, pair[1], targetSlug, got, want)
				}
				if got, want := CanManageUser(caller, target), legacyCanManageUser(caller, target); got != want {
					t.Errorf("CanManageUser(caller=%q/%s, target=%q/%s) = %v, want %v",
						pair[0], callerSlug, pair[1], targetSlug, got, want)
				}
				if got, want := CanDeleteUser(caller, target), legacyCanDeleteUser(caller, target); got != want {
					t.Errorf("CanDeleteUser(caller=%q/%s, target=%q/%s) = %v, want %v",
						pair[0], callerSlug, pair[1], targetSlug, got, want)
				}
			}
		}
	}
}
