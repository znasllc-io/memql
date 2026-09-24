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

// roleRank returns the numeric rank for a role slug, HIGHER == more
// privileged. Unknown slugs get rankUnknown (least privileged) so a gate that
// bounds "strictly below the actor" denies by default.
//
// READS THE CATALOG WHEN ONE IS INSTALLED (epic memql#5166, D1 and D2). The
// switch below is the seed's MIRROR and answers only before the rows are
// readable; with a catalog installed, a slug it does not carry ranks 0 rather
// than falling through -- see capability_catalog.go's header for why that
// fallback would be an escalation rather than a courtesy.
func roleRank(r Role) int {
	if rank, answered := catalogRank(string(r)); answered {
		return rank
	}
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

// capabilitySets is THE SEED'S MIRROR, not the model (epic memql#5166, D1).
//
// It maps each legacy role slug to its grant set -- the (verb, resource) pairs
// the role holds -- and it mirrors the predefined seed grants in
// dsl/rbac/seeds.memql (owner/developer/admin/user) plus the viewer profile for
// reader. Until memql#5166 it WAS the model: every runtime gate resolved
// through it, so a custom role authored as data held nothing.
//
// It is now consulted only while no CapabilityCatalog is installed, which is a
// narrow and real window rather than a fallback anybody should design against:
// the identity node's gates run before the seed is readable, and a node whose
// database is unreachable still has to rank its five base slugs rather than
// ranking every principal at 0 and refusing the whole cluster its own rows.
//
// TestSeedMatchesCompiledMirror (seed_mirror_parity_test.go) reads
// dsl/rbac/seeds.memql the way the ladder parity test does and fails the build
// when a pair differs IN EITHER DIRECTION -- a pair only the seeds hold is a
// permission that appears seconds after boot and not before; a pair only this
// map holds is one that exists at boot and then vanishes.
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
		// `update` on group is what every group builtin but groupCreate
		// checks (epic memql#5165, D7). Placing somebody into a client's
		// group is an edit OF THE GROUP -- the person's own row is
		// untouched -- so it is not `update` on principal, which a
		// developer deliberately does not hold.
		vr("create", "group"), vr("update", "group"),
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
		vr("create", "group"), vr("update", "group"),
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

// appReadFloors is the mirror of the APP half of the seeds (epic memql#5288,
// task memql#5300): `read` on `app:<id>` for every OS app, and on
// `app:<id>/<section>` for every floored section, on exactly the roles the
// MemQL OS registry's hand-written `roles:` floor admits today. Keyed by the
// USER-ROW spelling for the reason capabilitySets is (writer is the member
// tier, reader the viewer tier).
//
// It is a TABLE OF ROLE SETS rather than a rule, deliberately. A rule
// ("min admin means owner, developer, admin") would be a third copy of the
// floor semantics beside the OS's roleAdmits and the seeds' own reading of
// it, and the point of a mirror is to be pinned, not to be clever:
// TestSeedMatchesCompiledMirror holds every pair here equal to the seeds in
// both directions, and TestOsRegistryFloorsMatchTheAppSeeds holds the seeds
// equal to the registry. Merged into capabilitySets at init, below.
var appReadFloors = map[string][]Role{
	"app:accounts":              {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:accounts/logs":         {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:campaigns":             {RoleOwner, RoleDeveloper, RoleAdmin, RoleWriter, RoleReader},
	"app:campaigns/logs":        {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:cluster":               {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:cluster/modules":       {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:cluster/origins":       {RoleOwner},
	"app:cluster/audit":         {RoleOwner},
	"app:cluster/logs":          {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:cluster/automations":   {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:concepts":              {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:concepts/logs":         {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:files":                 {RoleOwner, RoleDeveloper, RoleAdmin, RoleWriter, RoleReader},
	"app:files/logs":            {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:deployables":           {RoleOwner, RoleDeveloper, RoleAdmin, RoleWriter, RoleReader},
	"app:deployables/logs":      {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:fleet":                 {RoleOwner, RoleDeveloper, RoleAdmin, RoleWriter, RoleReader},
	"app:fleet/logs":            {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:logs":                  {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:logs/stream":           {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:materializer":          {RoleOwner, RoleDeveloper, RoleAdmin, RoleWriter, RoleReader},
	"app:materializer/logs":     {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:users":                 {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:users/logs":            {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:training":              {RoleOwner, RoleDeveloper, RoleAdmin, RoleWriter},
	"app:training/logs":         {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:nexus":                 {RoleOwner, RoleDeveloper, RoleAdmin, RoleWriter, RoleReader},
	"app:nexus/logs":            {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:settings":              {RoleOwner, RoleDeveloper, RoleAdmin, RoleWriter, RoleReader},
	"app:settings/access":       {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:settings/cluster":      {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:settings/language":     {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:settings/benchmarks":   {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:settings/integrations": {RoleOwner, RoleDeveloper},
	"app:settings/providers":    {RoleOwner, RoleDeveloper},
	"app:settings/levels":       {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:settings/rules":        {RoleOwner, RoleDeveloper},
	"app:settings/decisions":    {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:settings/tokens":       {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:settings/keys":         {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:settings/logs":         {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:identity":              {RoleOwner, RoleDeveloper, RoleAdmin, RoleWriter, RoleReader},
	"app:identity/logs":         {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:bin":                   {RoleOwner, RoleDeveloper, RoleAdmin, RoleWriter, RoleReader},
	"app:bin/logs":              {RoleOwner, RoleDeveloper, RoleAdmin},
	"app:ask":                   {RoleOwner, RoleDeveloper, RoleAdmin, RoleWriter, RoleReader},
	"app:setup":                 {RoleOwner, RoleDeveloper},
}

// appPartGrants is the mirror of the `execute` app seeds: the Deployables
// PARTS (task memql#5301), on owner and developer, and the Modules pack switch.
var appPartGrants = map[string][]Role{
	"app:deployables/sources": {RoleOwner, RoleDeveloper},
	"app:deployables/deploy":  {RoleOwner, RoleDeveloper},
	"app:deployables/publish": {RoleOwner, RoleDeveloper},
	"app:deployables/retire":  {RoleOwner, RoleDeveloper},
	"app:deployables/domains": {RoleOwner, RoleDeveloper},
	// Publishing a CANDIDATE version and exercising it against a development
	// store (epic memql#5531). Owner and developer, and the line between this
	// part and `publish` is the one that epic draws: preparing and proving a
	// storefront changes nothing about what the public is served, while
	// PROMOTING is what makes the public see it -- so a person may hold the
	// first without the second. Setting the preview BINDING is not in here: it
	// names a v1:shopify:store row, so it carries `store` like every other act
	// that does.
	"app:deployables/preview": {RoleOwner, RoleDeveloper},
	// Attaching the Shopify store a storefront fronts (memql#5541). Owner
	// alone until v1:shopify:store took a read floor at developer (Connect
	// Shopify, D3): before it, a developer holding the part would have been
	// drawn a control and served no rows (memql#5216). A developer may now
	// bind ANY storefront they can write to any store on the cluster: every
	// account-tied client storefront, live ones included, because the site
	// tier's account grant admits staff to write them -- the same reach that
	// lets a developer pause, archive or delete those sites. D3 accepts that,
	// and every store row is made by a cluster owner or server code (D15), so
	// the Storefront token references a binding can expose are ones an owner
	// or Connect chose.
	"app:deployables/store": {RoleOwner, RoleDeveloper},
	// The pack switch in Cluster > Modules (Connect Shopify design, D4). A
	// developer holding it flips only a STOREFRONT pack: the other half of
	// that rule is component/memql's AuthorizeSetPackEnabled, and an owner
	// flips any pack there without asking this table.
	"app:cluster/modules": {RoleOwner, RoleDeveloper},
}

// The app tables fold into capabilitySets before anything reads it, so the
// mirror stays ONE map to every reader (roleHasCapability, principalGrantsOf,
// the parity gate) and the tables above are only a more legible way of
// writing 167 entries.
func init() {
	for resource, roles := range appReadFloors {
		for _, role := range roles {
			capabilitySets[role][vr(VerbRead, resource)] = true
		}
	}
	for resource, roles := range appPartGrants {
		for _, role := range roles {
			capabilitySets[role][vr(VerbExecute, resource)] = true
		}
	}
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
//
// The catalog answers when one is installed; the mirror below answers only
// before the rows are readable (epic memql#5166, D1).
func roleHasCapability(r Role, verb, resource string) bool {
	if held, answered := catalogHolds(string(r), verb, resource); answered {
		return held
	}
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

// THERE IS NO ROLE-SHAPED Capable(role, verb, resource) ANY MORE (epic
// memql#5296). The canonical decision is CapableFor(ctx, subject, verb,
// resource) in grant_resolver.go, which starts from roleHasCapability below --
// catalog-first, exactly as the old function was -- and overlays the actor's
// group and user grants. A role-only question is a Subject carrying a role and
// nothing else; nothing on the request path should be asking one.
//
// Consistency across nodes still holds for the role level as before -- every
// replica installs the SAME catalog rows and reloads on the same events -- and
// for the grant levels by construction: grants are read per request, so there
// is no per-node state to diverge.

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
	for _, vr := range principalGrantsOf(target) {
		if !roleHasCapability(actor, vr.Verb, vr.Resource) {
			return true
		}
	}
	return false
}

// principalGrantsOf lists a role's grants on the `principal` resource, from
// the catalog when one is installed and from the compiled mirror otherwise.
//
// The two readers are separate because the shapes differ -- the catalog speaks
// the exported VerbResource across a module boundary, the mirror the
// package-private one -- and merging them would mean exporting the mirror's key
// type, which is a detail of this file rather than a contract.
func principalGrantsOf(role Role) []VerbResource {
	if grants, answered := catalogGrants(string(role)); answered {
		out := make([]VerbResource, 0, len(grants))
		for _, vr := range grants {
			if vr.Resource == ResourcePrincipal {
				out = append(out, vr)
			}
		}
		return out
	}
	out := make([]VerbResource, 0, 4)
	for vr := range capabilitySets[role] {
		if vr.resource == ResourcePrincipal {
			out = append(out, VerbResource{Verb: vr.verb, Resource: vr.resource})
		}
	}
	return out
}

// RoleRank exposes a role's numeric rank (HIGHER == more privileged) from the
// consolidated model -- the value the relational governance predicates compare.
// Exported so enforcement call sites can resolve a rank without re-deriving the
// mapping. Unknown slugs return the least-privileged rank.
func RoleRank(role Role) int {
	return roleRank(role)
}
