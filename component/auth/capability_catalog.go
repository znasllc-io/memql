package auth

// THE RUNTIME CAPABILITY CATALOG (epic memql#5166, decisions D1 and D2).
//
// Epic memql#2062 made roles DATA -- a catalog with spaced ranks, capabilities
// as (verb x resource) rows, a rank-bounded createRole -- and the rank record's
// D5 said a custom role slots into the same ladder. What never followed was the
// half that makes a custom role DO anything: every runtime gate resolved
// through the compiled `capabilitySets` map in rbac_model.go, keyed by five
// legacy slugs. So a custom role could be created, ranked correctly, could not
// be assigned, and would have held no permission if it could.
//
// This file is the seam that fixes it. The engine loads both catalog concepts
// at boot, installs an implementation of CapabilityCatalog here, and from then
// on every Capable call and every Can* adapter reads the ROWS. The compiled map
// is demoted to the seed's MIRROR: it answers only until the rows are readable,
// and TestSeedMatchesCompiledMirror fails the build when the two disagree in
// either direction.
//
// ===========================================================================
// AN INSTALLED CATALOG IS AUTHORITATIVE. IT IS NOT A FIRST LOOKUP.
// ===========================================================================
//
// The natural spelling is "ask the catalog, and fall back to the compiled map
// when it has no answer". That spelling is wrong, and wrong in the direction
// that matters: `admin` is a slug the compiled map knows in full, so a catalog
// that has genuinely stopped carrying admin -- deactivated, renamed, deleted --
// would keep handing out admin's four principal verbs forever, with the rows
// saying otherwise and nothing to notice. The rows are the cluster's roles;
// once they are readable, a slug they do not carry holds NOTHING and ranks 0.
//
// The mirror answers only when NO catalog is installed at all, which is a
// lifecycle state rather than a lookup miss: a node between boot and its first
// successful load, or one whose database is unreachable. Distinguishing the two
// is why every resolver here returns a (answer, answered) pair rather than a
// bare bool: "the catalog says no" and "there is no catalog to ask" are
// different facts, and only one of them may reach the compiled map.
//
// ===========================================================================
// WHY A PACKAGE-LEVEL HOLDER RATHER THAN A PARAMETER
// ===========================================================================
//
// Capable, roleHasCapability and the eleven Can* adapters are free functions
// called from roughly forty sites across four Go modules -- the gRPC data gate,
// the identity admin surface, the executor guards, the delegation resolver.
// Threading a resolver through all of them is a refactor this epic does not
// need, and the half-finished version of it is worse than either end state:
// the sites that took the parameter would read the rows while the sites that
// did not would read the mirror, and the two would disagree about the same
// caller on the same request.
//
// The holder is an atomic.Pointer, so installing a fresh snapshot on a role
// event is a single store and every in-flight request keeps reading the
// snapshot it started with.

import (
	"strings"
	"sync/atomic"
)

// VerbResource is one (verb, resourceType) capability pair -- the exported
// twin of the unexported verbResource the compiled mirror is keyed by. Two
// types rather than one because the mirror's is a package-private detail of
// rbac_model.go and this one crosses a module boundary: component/memql builds
// the catalog and hands it here.
type VerbResource struct {
	Verb     string
	Resource string
}

// CapabilityCatalog is the cluster's role catalog, resolved.
//
// Implemented by component/memql over the v1:rbac:role and v1:rbac:capability
// rows; installed here by the engine at boot and replaced on every role or
// capability event. No method takes an actor, a context or a database, because
// a decision needing one of those could not be made on the request path where
// these are called.
//
// EVERY METHOD ACCEPTS A SLUG OR ONE OF ITS ALIASES, with no exceptions and no
// canonicalising helper for callers to remember. The user row's five spellings
// (owner/admin/developer/writer/reader) are not the catalog's five
// (owner/developer/admin/user/viewer): `writer` aliases `user` and `reader`
// aliases `viewer`. That mapping is DATA on the role row, and the whole reason
// it is data is that every consumer needing its own copy is how the engine and
// MemQL OS came to ship two ladders that disagreed. A method here that resolved
// aliases and a sibling that did not would rebuild exactly that split one
// interface further in: Active("writer") answering false would read as "every
// ordinary member's role is retired".
type CapabilityCatalog interface {
	// Rank resolves a slug to a rung. The bool is "this cluster has such a
	// role", which is a different question from whether it is active.
	Rank(slug string) (rank int, ok bool)

	// Name is the role's display name ("Owner", "Support Lead"). Empty for a
	// slug the catalog does not carry. It is on this interface rather than
	// read separately because MyAccess reports the name beside the rank, and
	// a second read of the same rows would be a second source for one fact.
	Name(slug string) string

	// Holds is the decision: does this role hold this pair? An allow row is
	// present AND no deny row overrides it -- the catalog resolves that
	// itself, so no gate has to remember that deny wins. False for an unknown
	// slug and false for every pair of a deactivated role.
	Holds(slug, verb, resource string) bool

	// Grants is the resolved allow set, for the subset guard that refuses a
	// creator handing out what they do not hold.
	Grants(slug string) []VerbResource

	// Scope is the account a role is scoped to (D10), or "" for a global one.
	// Scope governs who may HOLD the role, never who may see it exists.
	Scope(slug string) (accountId string)

	// Active reports the role's lifecycle flag.
	//
	// A DEACTIVATED ROLE ANSWERS "NOTHING, EVERYWHERE" -- no capability and,
	// through catalogRank below, no rank either. The catalog still CARRIES it,
	// because D8 is deactivate-never-delete and its slug and rung stay taken;
	// Rank is the catalog FACT and this is the lifecycle question, and the two
	// part company exactly here.
	Active(slug string) bool
}

// installed holds the catalog the engine installed, or nil.
var installed atomic.Pointer[CapabilityCatalog]

// SetCapabilityCatalog installs (or, with nil, clears) the cluster's catalog.
//
// Called by the engine at boot and again on every role/capability event.
// Clearing is for tests: production code never withdraws a catalog, because a
// node that reverted to the mirror mid-life would hand out the base grants for
// slugs the rows had since changed.
func SetCapabilityCatalog(c CapabilityCatalog) {
	if c == nil {
		installed.Store(nil)
		return
	}
	installed.Store(&c)
}

// InstalledCapabilityCatalog returns the installed catalog, or nil.
//
// Exported for the two readers outside this package that need the catalog
// itself rather than a decision derived from it: the MyAccess handler (slug ->
// name and rank) and the role builtins (the taken slugs, the taken ranks and
// the caller's resolved grant set).
func InstalledCapabilityCatalog() CapabilityCatalog {
	if p := installed.Load(); p != nil {
		return *p
	}
	return nil
}

// catalogHolds answers a (slug, verb, resource) question, reporting whether it
// answered at all.
func catalogHolds(slug, verb, resource string) (held bool, answered bool) {
	cat := InstalledCapabilityCatalog()
	if cat == nil {
		return false, false
	}
	return ScopedRoleMayUse(Role(slug), resource) && cat.Holds(normalizeSlug(slug), verb, resource), true
}

// catalogRank resolves a slug's rung for AUTHORIZATION, reporting whether it
// answered.
//
// An installed catalog that does not carry the slug ANSWERS -- with rank 0 --
// rather than declining, for the reason in the header. The two returns are
// therefore "did the catalog speak", not "did it recognise the slug".
//
// A DEACTIVATED ROLE RANKS 0 HERE even though the catalog knows its rung. The
// design record's failure-mode section says a holder of a retired role is
// treated "as unknown: nothing, everywhere, until re-roled", and a rank is not
// exempt from "everywhere": a rung that survived retirement would keep clearing
// every @requiresRank floor and keep the holder visible to their old peers
// under rankVisible, while Holds answered false for every pair. Half-retired is
// the one state this must not produce.
func catalogRank(slug string) (rank int, answered bool) {
	cat := InstalledCapabilityCatalog()
	if cat == nil {
		return 0, false
	}
	slug = normalizeSlug(slug)
	if !cat.Active(slug) {
		return 0, true
	}
	if r, ok := cat.Rank(slug); ok {
		return r, true
	}
	return 0, true
}

// catalogGrants returns a role's resolved allow set, reporting whether the
// catalog answered.
func catalogGrants(slug string) (grants []VerbResource, answered bool) {
	cat := InstalledCapabilityCatalog()
	if cat == nil {
		return nil, false
	}
	return cat.Grants(normalizeSlug(slug)), true
}

// catalogAssignable reports whether a slug names a role this cluster can hand
// out: known to the catalog AND active. The second return says whether the
// catalog answered at all, so a caller can fall back to the compiled five.
func catalogAssignable(slug string) (assignable bool, answered bool) {
	cat := InstalledCapabilityCatalog()
	if cat == nil {
		return false, false
	}
	slug = normalizeSlug(slug)
	if _, ok := cat.Rank(slug); !ok {
		return false, true
	}
	return cat.Active(slug), true
}

// normalizeSlug is the one fold applied to every slug entering this file.
//
// Trim and lowercase, and nothing else. Every slug in play is a lowercase
// value the cluster wrote -- the catalog's `slug`, the user row's `role` -- and
// the fold is here rather than at each call site because a role string arrives
// from a JWT claim, a row payload and a builtin argument, three writers with
// three ideas about whitespace. It is deliberately NOT a wider normalisation:
// a slug that differs from what the cluster stored by anything but case or
// padding is a slug the cluster did not store.
func normalizeSlug(slug string) string {
	return strings.ToLower(strings.TrimSpace(slug))
}
