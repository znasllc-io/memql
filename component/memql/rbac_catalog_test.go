package memql

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// The catalog's RESOLUTION is tested over rows built in memory rather than over
// a database, so it runs in `make test` on every machine with no DSN. The
// helpers below build the same memorynodes.MemoryNode values the two SELECTs
// return, and hand them to buildRbacCatalog -- the production builder -- so
// this exercises the shipped code rather than a parallel implementation of it.

type roleRow struct {
	id         string
	slug       string
	name       string
	rank       int
	active     bool
	aliases    []string
	account    string
	predefined bool
	// olderBy backdates the row so the newest-first collapse has something to
	// collapse. Zero means "now".
	olderBy time.Duration
}

type capabilityRow struct {
	id       string
	roleSlug string
	verb     string
	resource string
	effect   string
	active   bool
	olderBy  time.Duration
}

func roleNodes(rows ...roleRow) []memorynodes.MemoryNode {
	out := make([]memorynodes.MemoryNode, 0, len(rows))
	for _, r := range rows {
		payload := map[string]any{
			"slug":       r.slug,
			"name":       r.name,
			"rank":       r.rank,
			"active":     r.active,
			"predefined": r.predefined,
		}
		if len(r.aliases) > 0 {
			payload["aliases"] = r.aliases
		}
		if r.account != "" {
			payload["accountId"] = r.account
		}
		raw, _ := json.Marshal(payload)
		out = append(out, memorynodes.MemoryNode{
			ID:        r.id,
			Concept:   conceptRbacRole,
			CreatedAt: time.Now().UTC().Add(-r.olderBy),
			Payload:   raw,
		})
	}
	return out
}

func capabilityNodes(rows ...capabilityRow) []memorynodes.MemoryNode {
	out := make([]memorynodes.MemoryNode, 0, len(rows))
	for _, c := range rows {
		effect := c.effect
		if effect == "" {
			effect = "allow"
		}
		raw, _ := json.Marshal(map[string]any{
			"roleSlug":     c.roleSlug,
			"verb":         c.verb,
			"resourceType": c.resource,
			"effect":       effect,
			"active":       c.active,
		})
		id := c.id
		if id == "" {
			id = c.roleSlug + "-" + c.verb + "-" + c.resource
		}
		out = append(out, memorynodes.MemoryNode{
			ID:        id,
			Concept:   conceptRbacCapability,
			CreatedAt: time.Now().UTC().Add(-c.olderBy),
			Payload:   raw,
		})
	}
	return out
}

// TestCatalogCollapsesToTheNewestVersionOfEachRow. MemQL rows are append-only,
// so the role catalog holds every VERSION of every role and its version count
// grows with UPTIME -- a re-seed on every boot. The SQL does the collapse; this
// asserts the builder agrees when handed both versions, because a builder that
// took the last row it saw would silently invert the answer if the ORDER BY
// were ever edited.
func TestCatalogCollapsesToTheNewestVersionOfEachRow(t *testing.T) {
	cat := buildRbacCatalog(
		roleNodes(
			roleRow{id: "support-lead", slug: "support-lead", name: "Support Lead", rank: 150, active: true},
			roleRow{id: "support-lead", slug: "support-lead", name: "Support Lead", rank: 350, active: true, olderBy: time.Hour},
		),
		capabilityNodes(capabilityRow{roleSlug: "support-lead", verb: "read", resource: "principal", active: true}),
	)

	rank, ok := cat.Rank("support-lead")
	if !ok || rank != 150 {
		t.Fatalf("Rank(support-lead) = %d,%v -- the collapse must take the newest version, not the first row seen", rank, ok)
	}
	if cat.Name("support-lead") != "Support Lead" {
		t.Fatalf("Name(support-lead) = %q, want %q", cat.Name("support-lead"), "Support Lead")
	}
}

// TestDenyWins is D9. The catalog resolves it once, so no gate downstream has
// to remember the rule.
func TestDenyWins(t *testing.T) {
	cat := buildRbacCatalog(
		roleNodes(roleRow{id: "finance", slug: "finance", name: "Finance", rank: 150, active: true}),
		capabilityNodes(
			capabilityRow{id: "a", roleSlug: "finance", verb: "execute", resource: "deployment", effect: "allow", active: true},
			capabilityRow{id: "d", roleSlug: "finance", verb: "execute", resource: "deployment", effect: "deny", active: true},
			capabilityRow{id: "b", roleSlug: "finance", verb: "read", resource: "data", effect: "allow", active: true},
		),
	)

	if cat.Holds("finance", "execute", "deployment") {
		t.Fatal("a deny row must win over an allow for the same (role, verb, resource)")
	}
	if !cat.Holds("finance", "read", "data") {
		t.Fatal("a deny on one pair must not affect another")
	}
	for _, vr := range cat.Grants("finance") {
		if vr.Verb == "execute" && vr.Resource == "deployment" {
			t.Fatal("Grants must report the RESOLVED allow set -- a denied pair is not granted, " +
				"and the subset guard reads this to decide what a creator may hand out")
		}
	}
}

// TestADenyRowSurvivesTheAllowBeingInactive is the corner the obvious
// implementation gets wrong: filter the inactive rows out first and a deny
// whose allow was deactivated has nothing left to override, which is fine --
// but a deny that is ITSELF inactive must stop overriding.
func TestAnInactiveDenyStopsOverriding(t *testing.T) {
	cat := buildRbacCatalog(
		roleNodes(roleRow{id: "finance", slug: "finance", rank: 150, active: true}),
		capabilityNodes(
			capabilityRow{id: "a", roleSlug: "finance", verb: "execute", resource: "deployment", effect: "allow", active: true},
			capabilityRow{id: "d", roleSlug: "finance", verb: "execute", resource: "deployment", effect: "deny", active: false},
		),
	)
	if !cat.Holds("finance", "execute", "deployment") {
		t.Fatal("a deactivated deny row must not override a live allow")
	}
}

func TestAliasesResolveToTheirRung(t *testing.T) {
	cat := buildRbacCatalog(
		roleNodes(
			roleRow{id: "user", slug: "user", name: "Member", rank: 100, active: true, aliases: []string{"writer"}},
			roleRow{id: "viewer", slug: "viewer", name: "Viewer", rank: 50, active: true, aliases: []string{"reader"}},
		),
		capabilityNodes(capabilityRow{roleSlug: "user", verb: "create", resource: "data", active: true}),
	)

	if rank, ok := cat.Rank("writer"); !ok || rank != 100 {
		t.Fatalf("Rank(writer) = %d,%v -- an alias must resolve to its rung", rank, ok)
	}
	if !cat.Holds("writer", "create", "data") {
		t.Fatal("Holds must resolve an alias: `writer` is the spelling every ordinary " +
			"principal's user row carries, and the catalog seeds the rung as `user`")
	}
	if !cat.Active("writer") {
		t.Fatal("Active must resolve an alias -- answering false here reads as " +
			"\"every ordinary member's role is retired\"")
	}
	if cat.Name("writer") != "Member" {
		t.Fatalf("Name(writer) = %q, want the rung's name %q", cat.Name("writer"), "Member")
	}
}

// TestASlugAlwaysWinsOverAnAlias is the security-relevant half of the alias
// pass, inherited from rankLadder: applying aliases inline made "a slug already
// taken by a base role wins" depend on iteration order, so a custom role could
// claim an alias whose base role had not been read yet. `writer` and `reader`
// are alias-only rungs -- no row carries them as a slug -- so nothing would
// ever reclaim them, and a developer minting a rank-299 role aliased `reader`
// would promote every reader in the cluster to 299.
func TestASlugAlwaysWinsOverAnAlias(t *testing.T) {
	cat := buildRbacCatalog(
		roleNodes(
			// Newest first, as the rows come back: the attacker's role is read
			// BEFORE the base role whose alias it is trying to claim.
			roleRow{id: "sneaky", slug: "sneaky", name: "Sneaky", rank: 299, active: true, aliases: []string{"reader", "viewer"}},
			roleRow{id: "viewer", slug: "viewer", name: "Viewer", rank: 50, active: true, aliases: []string{"reader"}, predefined: true, olderBy: time.Hour},
		),
		nil,
	)

	if rank, _ := cat.Rank("viewer"); rank != 50 {
		t.Fatalf("Rank(viewer) = %d -- a SLUG must always win over another role's alias claim, "+
			"or a custom role re-points an existing rung", rank)
	}
	if rank, _ := cat.Rank("reader"); rank != 50 {
		t.Fatalf("Rank(reader) = %d -- `reader` is an alias-only rung, so nothing reclaims it "+
			"and the first alias claim seen would stand forever", rank)
	}
}

// TestInactiveRoleKeepsItsRungAsAFactAndNoneAsAnAnswer is the split this
// catalog draws, and it is the subtle one.
//
// The design record says a holder of a deactivated role is treated "as unknown:
// nothing, everywhere, until re-roled" -- so the LADDER, which is what every
// rank floor and every rank-visible read resolves through, must not carry the
// rung. But D8 is deactivate-never-delete, and the role builtins refuse a
// create that reuses a retired slug or a retired rank, so the CATALOG must
// still carry it.
//
// Rank is therefore the catalog fact and ladder() is the authorization answer,
// and they part company exactly here.
func TestInactiveRoleKeepsItsRungAsAFactAndNoneAsAnAnswer(t *testing.T) {
	cat := buildRbacCatalog(
		roleNodes(roleRow{id: "retired", slug: "retired", name: "Retired", rank: 120, active: false}),
		capabilityNodes(capabilityRow{roleSlug: "retired", verb: "read", resource: "data", active: true}),
	)

	if cat.Holds("retired", "read", "data") {
		t.Fatal("a deactivated role must hold nothing (D8)")
	}
	if cat.Active("retired") {
		t.Fatal("Active must report false for a deactivated role")
	}
	if rank, ok := cat.Rank("retired"); !ok || rank != 120 {
		t.Fatalf("Rank(retired) = %d,%v -- the catalog must keep carrying a retired role, or "+
			"roleCreate cannot refuse a create that reuses its slug or its rung", rank, ok)
	}
	if got := cat.ladder().rankOf("retired"); got != 0 {
		t.Fatalf("ladder().rankOf(retired) = %d, want 0 -- a rung that outlives retirement keeps "+
			"clearing every @requiresRank floor while the role holds nothing", got)
	}
	if slugs := cat.Slugs(); len(slugs) != 1 || slugs[0] != "retired" {
		t.Fatalf("Slugs() = %v -- it must list a retired role, since that is what the taken "+
			"guards read", slugs)
	}
}

func TestAnInactiveGrantIsIgnored(t *testing.T) {
	cat := buildRbacCatalog(
		roleNodes(roleRow{id: "lead", slug: "lead", rank: 150, active: true}),
		capabilityNodes(
			capabilityRow{id: "live", roleSlug: "lead", verb: "read", resource: "data", active: true},
			capabilityRow{id: "dead", roleSlug: "lead", verb: "delete", resource: "data", active: false},
		),
	)
	if cat.Holds("lead", "delete", "data") {
		t.Fatal("a deactivated capability row must be ignored at resolution time")
	}
	if !cat.Holds("lead", "read", "data") {
		t.Fatal("its live sibling must still hold")
	}
}

func TestScopeIsEmptyForAGlobalRoleAndSetForAScopedOne(t *testing.T) {
	cat := buildRbacCatalog(
		roleNodes(
			roleRow{id: "global", slug: "global", rank: 150, active: true},
			roleRow{id: "scoped", slug: "scoped", rank: 140, active: true, account: "acct-1"},
		),
		nil,
	)
	if got := cat.Scope("global"); got != "" {
		t.Fatalf("Scope(global) = %q, want empty", got)
	}
	if got := cat.Scope("scoped"); got != "acct-1" {
		t.Fatalf("Scope(scoped) = %q, want acct-1", got)
	}
}

func TestUnknownSlugAnswersNothing(t *testing.T) {
	cat := buildRbacCatalog(roleNodes(roleRow{id: "owner", slug: "owner", rank: 400, active: true}), nil)
	if _, ok := cat.Rank("ghost"); ok {
		t.Fatal("Rank must report ok=false for a slug the catalog does not carry")
	}
	if cat.Holds("ghost", "read", "data") || cat.Active("ghost") || cat.Name("ghost") != "" ||
		cat.Scope("ghost") != "" || len(cat.Grants("ghost")) != 0 {
		t.Fatal("every method must answer nothing for an unknown slug")
	}
}

// TestTheCatalogIsTheLadder is the reason rankLadder reads this structure: two
// resolutions of the same rows could disagree, and the row gate and the data
// gate would then answer differently about one row.
func TestTheCatalogIsTheLadder(t *testing.T) {
	cat := buildRbacCatalog(
		roleNodes(
			roleRow{id: "user", slug: "user", rank: 100, active: true, aliases: []string{"writer"}},
			roleRow{id: "lead", slug: "lead", rank: 150, active: true},
		),
		nil,
	)
	ladder := cat.ladder()
	if got := ladder.rankOf("writer"); got != 100 {
		t.Fatalf("ladder.rankOf(writer) = %d, want 100", got)
	}
	if got := ladder.rankOf("lead"); got != 150 {
		t.Fatalf("ladder.rankOf(lead) = %d, want 150", got)
	}
	slugs := ladder.knownSlugs()
	sort.Strings(slugs)
	if len(slugs) == 0 {
		t.Fatal("the ladder derived from the catalog must name its rungs")
	}
}

// TestCatalogSatisfiesTheAuthInterface is a compile-time assertion made
// runtime-visible: the engine's catalog IS what component/auth resolves
// through, and a signature drift here would otherwise surface as a nil
// interface at boot.
func TestCatalogSatisfiesTheAuthInterface(t *testing.T) {
	var _ auth.CapabilityCatalog = buildRbacCatalog(nil, nil)
}

// TestACatalogWithNoRoleRowsCountsZero pins the predicate
// ReloadCapabilityCatalog's empty-catalog guard tests.
//
// The guard is `cat == nil || cat.roleCount == 0`. If roleCount ever came to
// mean something else -- aliases included, inactive roles excluded -- the
// guard would stop firing on the state it exists for, silently, and the next
// fresh-database boot would install an empty catalog again. Capability rows
// without roles still count zero: a grant naming a role that is not there is
// not a readable catalog.
func TestACatalogWithNoRoleRowsCountsZero(t *testing.T) {
	if got := buildRbacCatalog(nil, nil).roleCount; got != 0 {
		t.Fatalf("no rows: roleCount = %d, want 0", got)
	}

	orphan := buildRbacCatalog(nil, capabilityNodes(capabilityRow{
		id: "cap-owner-create-construct", roleSlug: "owner",
		verb: "create", resource: "construct", effect: "allow", active: true,
	}))
	if got := orphan.roleCount; got != 0 {
		t.Fatalf("capabilities but no roles: roleCount = %d, want 0", got)
	}

	real := buildRbacCatalog(roleNodes(roleRow{id: "owner", slug: "owner", rank: 400, active: true}), nil)
	if got := real.roleCount; got != 1 {
		t.Fatalf("one role row: roleCount = %d, want 1", got)
	}

	// A DEACTIVATED role still counts: the catalog carries it (D8 is
	// deactivate-never-delete), so this is not the empty state and the guard
	// must not fire on it.
	off := buildRbacCatalog(roleNodes(roleRow{id: "owner", slug: "owner", rank: 400, active: false}), nil)
	if got := off.roleCount; got != 1 {
		t.Fatalf("one deactivated role row: roleCount = %d, want 1", got)
	}
}

// TestAnEmptyCatalogIsRefusedRatherThanInstalled is the guard itself.
//
// Installing a snapshot with no roles is a cluster-wide authoring lockout --
// component/auth's roleHasCapability short-circuits the compiled mirror the
// moment a catalog is installed, so an empty one answers false for every role
// including the owner. A fresh-database boot reaches this state on every
// start, because the catalog loads before the seed materializer writes
// v1:rbac:role.
func TestAnEmptyCatalogIsRefusedRatherThanInstalled(t *testing.T) {
	t.Cleanup(func() { auth.SetCapabilityCatalog(nil) })

	e := &MemQLEngine{}

	if err := e.installCapabilityCatalog(nil); err != errCatalogHasNoRoles {
		t.Fatalf("nil catalog: err = %v, want errCatalogHasNoRoles", err)
	}
	if err := e.installCapabilityCatalog(buildRbacCatalog(nil, nil)); err != errCatalogHasNoRoles {
		t.Fatalf("empty catalog: err = %v, want errCatalogHasNoRoles", err)
	}
	if auth.InstalledCapabilityCatalog() != nil {
		t.Fatal("a refused catalog was installed anyway -- the mirror is no longer answering")
	}
	// The owner must still answer from the mirror, which is the whole point.
	if !auth.CapableFor(context.Background(), auth.Subject{Role: auth.RoleOwner}, auth.VerbCreate, auth.ResourceConstruct) {
		t.Fatal("owner lost create x construct after a refused install")
	}

	// A ROLE-ONLY SNAPSHOT IS THE LIKELIER HALF. The reload fires on four
	// topics, so the first role row to land triggers a read that sees roles
	// and no capabilities yet -- and a catalog with roles but no grants
	// answers false for every pair of every role, owner included.
	roleOnly := buildRbacCatalog(roleNodes(roleRow{id: "owner", slug: "owner", rank: 400, active: true}), nil)
	if err := e.installCapabilityCatalog(roleOnly); err != errCatalogHasNoRoles {
		t.Fatalf("a catalog with roles but NO capabilities was installed: err = %v", err)
	}
	if auth.InstalledCapabilityCatalog() != nil {
		t.Fatal("a role-only catalog was installed -- every role now holds nothing")
	}

	real := buildRbacCatalog(
		roleNodes(roleRow{id: "owner", slug: "owner", rank: 400, active: true}),
		capabilityNodes(capabilityRow{
			id: "cap-owner-create-construct", roleSlug: "owner",
			verb: "create", resource: "construct", effect: "allow", active: true,
		}),
	)
	if err := e.installCapabilityCatalog(real); err != nil {
		t.Fatalf("a catalog with a role AND a capability was refused: %v", err)
	}
	if auth.InstalledCapabilityCatalog() == nil {
		t.Fatal("a valid catalog was not installed")
	}
}
