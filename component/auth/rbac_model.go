package auth

// RBAC model: the Go-side mirror of the DSL-defined role + capability model
// (dsl/rbac/, epic memql#2062). E1.5 (memql#2073) migrates the scattered,
// hardcoded role-constant checks in rbac.go to resolve through THIS single
// model -- a role's RANK plus its CAPABILITY set (verb x resourceType grants),
// matching the DSL base-role seeds (dsl/rbac/seeds.memql). The Can* helpers in
// rbac.go are reduced to thin adapters over roleCapability / roleRank here, so
// "what owner/developer/admin/writer/reader may do" has ONE definition that
// agrees with the DSL catalog (the load-tested source of truth).
//
// Legacy-slug reconciliation (the locked E1.5 decision):
//   - developer / admin / owner  -> the DSL base roles of the same slug.
//   - writer  -> the `user` (member) tier: read/write data plane (CanWrite),
//     read on the catalogs. Same capability profile as the base `user` role.
//   - reader  -> the `viewer` tier: read-only. A low-rank capability profile
//     below user; reads the data plane + catalogs, writes nothing.
//
// The capability sets below are byte-for-byte the grants authored as seeds in
// dsl/rbac/seeds.memql for owner/developer/admin/user, plus the viewer profile
// for the reader slug. A conformance test (rbac_model_test.go) pins that every
// legacy Can* result is preserved across all five slugs.

// verbResource is a (verb, resourceType) capability key. Verbs + resource
// types mirror the DSL v1:rbac:capability enum / vocabulary.
type verbResource struct {
	verb     string
	resource string
}

// Base ranks -- mirror dsl/rbac/seeds.memql (HIGHER == more privileged). The
// viewer tier (reader) sits below user; spaced so custom roles can slot in.
const (
	rankOwner     = 400
	rankDeveloper = 300
	rankAdmin     = 200
	rankUser      = 100 // the `user`/member tier (writer maps here)
	rankViewer    = 50  // the read-only tier (reader maps here)
	rankUnknown   = 0
)

// roleRank returns the numeric rank for a (possibly legacy) role slug, HIGHER
// == more privileged. Unknown slugs get rankUnknown (least privileged) so a
// gate that bounds "strictly below the actor" denies by default.
func roleRank(r Role) int {
	switch r {
	case RoleOwner:
		return rankOwner
	case RoleDeveloper:
		return rankDeveloper
	case RoleAdmin:
		return rankAdmin
	case RoleWriter:
		return rankUser // writer -> member tier
	case RoleReader:
		return rankViewer // reader -> viewer tier
	default:
		return rankUnknown
	}
}

// capabilitySets maps each role slug to its grant set -- the (verb, resource)
// pairs the role holds. Mirrors the predefined seed grants in
// dsl/rbac/seeds.memql (owner/developer/admin/user) plus the viewer profile for
// reader. The Can* adapters look up membership here.
var capabilitySets = map[Role]map[verbResource]bool{
	RoleOwner: setOf(
		// principal (user management) -- full.
		vr("read", "principal"), vr("create", "principal"), vr("update", "principal"), vr("delete", "principal"),
		// construct (authoring + inline).
		vr("create", "construct"), vr("update", "construct"), vr("delete", "construct"), vr("execute", "construct"),
		// data plane -- full.
		vr("read", "data"), vr("create", "data"), vr("update", "data"), vr("delete", "data"),
		// deployment, agent, group, role.
		vr("execute", "deployment"),
		vr("create", "agent"),
		vr("create", "group"),
		vr("read", "role"), vr("create", "role"), vr("update", "role"), vr("delete", "role"),
		vr("create", "admission"),
	),
	RoleDeveloper: setOf(
		// engineering: authoring + inline + data + forward deploy. NO principal mgmt.
		vr("read", "principal"),
		vr("create", "construct"), vr("update", "construct"), vr("delete", "construct"), vr("execute", "construct"),
		vr("read", "data"), vr("create", "data"), vr("update", "data"), vr("delete", "data"),
		vr("execute", "deployment"),
		vr("read", "agent"),
		vr("read", "role"),
		// ADMISSION: may hand somebody a credential that lets them into this
		// cluster -- a user invitation or an enrolment link.
		//
		// This is the one piece of people-work a developer holds, and it is
		// deliberately NOT `create` on `principal`: that is the
		// user-management authority AtLeastAdmin tests, gating fourteen
		// operations including role changes, suspensions and recovery-key
		// rotation. Admission is its own resource because the resource
		// vocabulary is open by design -- v1:rbac:capability documents
		// resourceType as "an open string (not a closed enum) so product
		// layers can introduce their own resource kinds without an engine
		// change".
		//
		// A developer still holds NO update or delete on `principal`, so they
		// cannot edit, suspend or re-role anybody who accepts.
		vr("create", "admission"),
	),
	RoleAdmin: setOf(
		// user-management: full principal verbs + agent/group + data + deploy. NO authoring.
		vr("read", "principal"), vr("create", "principal"), vr("update", "principal"), vr("delete", "principal"),
		vr("read", "construct"),
		vr("read", "data"), vr("create", "data"), vr("update", "data"), vr("delete", "data"),
		vr("execute", "deployment"),
		vr("create", "agent"),
		vr("create", "group"),
		vr("read", "role"), vr("create", "role"), vr("update", "role"),
		vr("create", "admission"),
	),
	RoleWriter: setOf(
		// writer -> member tier: read/write data plane + read catalogs. No mgmt/authoring/deploy.
		vr("read", "data"), vr("create", "data"), vr("update", "data"), vr("delete", "data"),
		vr("read", "agent"), vr("read", "group"),
	),
	RoleReader: setOf(
		// reader -> viewer tier: read-only.
		vr("read", "data"), vr("read", "agent"), vr("read", "group"),
	),
}

func vr(verb, resource string) verbResource { return verbResource{verb: verb, resource: resource} }

func setOf(keys ...verbResource) map[verbResource]bool {
	m := make(map[verbResource]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// roleHasCapability reports whether the role holds the (verb, resource) grant.
// The single membership predicate every migrated Can* adapter calls.
func roleHasCapability(r Role, verb, resource string) bool {
	set, ok := capabilitySets[r]
	if !ok {
		return false
	}
	return set[verbResource{verb: verb, resource: resource}]
}

// Resource types + verbs -- the canonical RBAC vocabulary, mirroring the DSL
// v1:rbac:capability enum / resourceType values. Exported so the request-path
// enforcement (E1.6, memql#2074) names them without inlining string literals.
const (
	VerbRead    = "read"
	VerbCreate  = "create"
	VerbUpdate  = "update"
	VerbDelete  = "delete"
	VerbExecute = "execute"

	ResourcePrincipal  = "principal"
	ResourceConstruct  = "construct"
	ResourceData       = "data"
	ResourceDeployment = "deployment"
	ResourceAdmission  = "admission"
	ResourceAgent      = "agent"
	ResourceGroup      = "group"
	ResourceRole       = "role"
)

// Capable is the canonical, single server-side authorization primitive: does
// the given role hold the (verb x resourceType) capability in the consolidated
// RBAC model (epic memql#2062)? Every enforcement decision on the request path
// -- handlers, executor guards, the migrated Can* adapters -- resolves through
// THIS function, so there is exactly one definition of "may role R do verb V on
// resource T", and it agrees with the load-tested DSL capability catalog
// (dsl/rbac/seeds.memql).
//
// The decision is pure: a lookup against the static capability sets, with no DB
// access and no per-node state. That is what makes authorization decisions
// CONSISTENT across nodes (E1.6 multi-node acceptance) -- the same role resolves
// to the same decision on every replica, because the model carries no
// node-local state to diverge.
//
// Relational governance (who-can-manage-whom over (actor, target)) is a
// separate, complementary primitive: GovernPrincipal / CanCreatePrincipal
// (rbac_governance.go). Capable answers "does this role hold the grant at all";
// GovernPrincipal narrows the principal-resource verbs by rank + self.
func Capable(role Role, verb, resourceType string) bool {
	return roleHasCapability(role, verb, resourceType)
}

// GrantsPrincipalAuthorityBeyond reports whether `target` holds any verb on
// the `principal` resource that `actor` does not.
//
// RANK IS NOT AUTHORITY, and this exists because of the one pair where they
// disagree. developer ranks 300 and admin 200, so every "must strictly
// outrank" rule admits developer -> admin -- while admin holds create, update
// and delete on `principal` and developer holds only read. A gate that mints a
// credential FOR an account (an enrolment link registers a passkey as that
// person; an invitation creates one at a chosen role) therefore cannot ask
// "do I outrank them". It has to ask this.
//
// Left unasked, the gap is a two-move path to owner: mint a credential for an
// existing admin, sign in as them, then use the uncapped SetUserRole to make
// anybody an owner.
//
// SCOPED TO `principal`, NOT TO EVERY RESOURCE, and that is deliberate. Full
// capability containment refuses developer -> writer, because writer holds
// read-on-group and developer does not, and even owner -> developer, because
// developer holds read-on-agent and owner does not. Both are nonsense here.
// `principal` is the governance-bearing resource -- the one whose verbs ARE
// user management -- so it is the one that decides whether minting a
// credential hands the minter authority they lack.
//
// It COMPLEMENTS GovernPrincipal rather than replacing it: the owner carve-out
// is still what refuses admin -> owner, since both hold the same four verbs
// and this predicate sees no difference between them.
func GrantsPrincipalAuthorityBeyond(actor, target Role) bool {
	for vr := range capabilitySets[target] {
		if vr.resource != ResourcePrincipal {
			continue
		}
		if !capabilitySets[actor][vr] {
			return true
		}
	}
	return false
}

// RoleRank exposes a role's numeric rank (HIGHER == more privileged) from the
// consolidated model -- the value the relational governance predicates compare.
// Exported so enforcement call sites can resolve a rank without re-deriving the
// mapping. Unknown slugs return the least-privileged rank.
func RoleRank(role Role) int {
	return roleRank(role)
}
