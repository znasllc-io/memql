package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// THE RANK RULES, END TO END AGAINST A REAL ENGINE AND DATABASE (epic
// memql#4832, task memql#4834 -- the design record's section E).
//
// The design says why this file has to exist, in as many words: "a green
// single-actor unit test is the false signal this area produces". The unit
// tests beside this one prove rankAdmitsRow answers correctly for a scope
// handed to it. Only these prove that the scope is RESOLVED from the real
// principal table, that the SQL half and the row gate agree about the same
// row, and that a page of peer-owned rows does not read as exhaustion.
//
// Postgres-gated like its neighbours; CI's db-tests lane sets
// MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.
//
// THE SUBJECT IS v1:accounts:account, which declares the rank tier for real
// (`rankVisible`, `unowned="admin"`). A fixture concept would have been
// easier and would have measured the gate rather than the feature: the
// accounts constructs also carry `@requiresRank`, and the interaction between
// a construct floor and a row tier is exactly where a plausible-looking
// implementation goes wrong.

// rankActorCtx builds a caller the request path would build: an AccessContext
// carrying the role, plus the TokenInfo the mutation executor attributes
// writes to.
func rankActorCtx(userId string, role auth.Role) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: userId,
		Role:   role,
	})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

// seedPrincipal writes a v1:identity:user row so the rank resolver can find
// it. Raw insert under internal origin: this is test scaffolding standing in
// for the identity service, not a path under test.
func seedPrincipal(t *testing.T, eng *MemQLEngine, userId string, role auth.Role) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"displayName":  userId,
		"primaryEmail": userId + "@example.test",
		"role":         string(role),
		"active":       true,
	})
	if err != nil {
		t.Fatalf("marshal principal: %v", err)
	}
	q := fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString(conceptIdentityUser),
		langparser.QuoteString(userId),
		string(payload))
	// A cluster-owner actor plus internal origin: the raw-insert owner stamp
	// refuses a write with no caller identity (memql#3175's finding-4
	// refusal), and this is scaffolding standing in for the identity service.
	seedCtx := auth.ContextWithInternalOrigin(
		auth.ContextWithAccess(context.Background(), &auth.AccessContext{
			UserId: "rank-db-seeder",
			Role:   auth.RoleOwner,
		}))
	seedCtx = auth.ContextWithToken(seedCtx, &auth.TokenInfo{Subject: "rank-db-seeder"})
	if _, err := eng.Execute(seedCtx, q); err != nil {
		t.Fatalf("seed principal %s: %v", userId, err)
	}
}

// seedAccountOwnedBy creates an account owned by one principal.
func seedAccountOwnedBy(t *testing.T, eng *MemQLEngine, owner string, role auth.Role, id, name string) {
	t.Helper()
	ctx := rankActorCtx(owner, role)
	q := fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: %s)`,
		langparser.QuoteString(id), langparser.QuoteString(name))
	if _, err := eng.Execute(ctx, q); err != nil {
		t.Fatalf("create account %s as %s (%s): %v", id, owner, role, err)
	}
}

// accountsVisibleTo runs the real query and returns the ids that came back.
func accountsVisibleTo(t *testing.T, eng *MemQLEngine, userId string, role auth.Role) map[string]bool {
	t.Helper()
	res, err := eng.Execute(rankActorCtx(userId, role), `query clientAccountsAll(includeArchived: true)`)
	if err != nil {
		// A refusal is an ANSWER here -- the @requiresRank floor refusing a
		// caller below admin -- and the caller distinguishes it from an empty
		// result, which is the distinction the whole feature rests on.
		return nil
	}
	out := map[string]bool{}
	for _, n := range res.Bundle.GetNodes() {
		out[BareShortId(n.GetId())] = true
	}
	return out
}

// TestRankVisibleReadsPerRankPairAgainstARealDatabase is the per-pair matrix
// the design asks for.
//
// D2 is `rank(roleOf(row.owner)) <= rank(actor)` -- PEERS INCLUDED at every
// rung -- and the pairs below are chosen so a `<` implementation and a `<=`
// one give different answers, which a same-rank-only or a strictly-below-only
// test would not distinguish.
func TestRankVisibleReadsPerRankPairAgainstARealDatabase(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("rank4834")

	// One principal per rung that can reach the app at all. writer/reader are
	// below the construct floor and are covered by the refusal case below.
	admin := "admin-" + suffix
	admin2 := "admin2-" + suffix
	dev := "dev-" + suffix
	owner := "owner-" + suffix

	seedPrincipal(t, eng, admin, auth.RoleAdmin)
	seedPrincipal(t, eng, admin2, auth.RoleAdmin)
	seedPrincipal(t, eng, dev, auth.RoleDeveloper)
	seedPrincipal(t, eng, owner, auth.RoleOwner)

	acctAdmin := "v1:accounts:account:a-" + suffix
	acctAdmin2 := "v1:accounts:account:a2-" + suffix
	acctDev := "v1:accounts:account:d-" + suffix
	acctOwner := "v1:accounts:account:o-" + suffix

	seedAccountOwnedBy(t, eng, admin, auth.RoleAdmin, acctAdmin, "Admin's client")
	seedAccountOwnedBy(t, eng, admin2, auth.RoleAdmin, acctAdmin2, "Another admin's client")
	seedAccountOwnedBy(t, eng, dev, auth.RoleDeveloper, acctDev, "Developer's client")
	seedAccountOwnedBy(t, eng, owner, auth.RoleOwner, acctOwner, "Owner's client")

	cases := []struct {
		name      string
		actor     string
		role      auth.Role
		sees      []string
		blind     []string
		rationale string
	}{
		{
			name:      "an admin sees their own, their PEER's, and nobody above",
			actor:     admin,
			role:      auth.RoleAdmin,
			sees:      []string{acctAdmin, acctAdmin2},
			blind:     []string{acctDev, acctOwner},
			rationale: "D2 includes peers (<=), and excludes every higher rung",
		},
		{
			name:      "a developer sees every account, the owner's included",
			actor:     dev,
			role:      auth.RoleDeveloper,
			sees:      []string{acctAdmin, acctAdmin2, acctDev, acctOwner},
			rationale: "developers and owners view and manage every account (owner's direction, 2026-11-10); the rank tier binds admins only (accountsStaff)",
		},
		{
			name:      "an owner sees every rung",
			actor:     owner,
			role:      auth.RoleOwner,
			sees:      []string{acctAdmin, acctAdmin2, acctDev, acctOwner},
			rationale: "owner is the top rung, so <= admits everyone",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := accountsVisibleTo(t, eng, c.actor, c.role)
			if got == nil {
				t.Fatalf("the read was REFUSED for %s; this actor clears the construct floor", c.role)
			}
			for _, id := range c.sees {
				if !got[BareShortId(id)] {
					t.Fatalf("%s cannot see %s. %s", c.role, id, c.rationale)
				}
			}
			for _, id := range c.blind {
				if got[BareShortId(id)] {
					t.Fatalf("%s CAN see %s, which is owned above their rank. %s", c.role, id, c.rationale)
				}
			}
		})
	}
}

// A developer reads AND edits an account the owner created; an admin still
// cannot see it (owner's direction, 2026-11-10: "developers and owners should
// have access" to every account, to view and to manage).
//
// The by-id read is the one the email preview (component/campaigns
// email_render.go), the template and layout checks and the Deployables Client
// card make as the signed-in person. Before this, a developer got zero rows
// there: the preview said "organization is not active or accessible" and the
// card showed the account's id instead of its name.
func TestADeveloperReadsAndEditsAnOwnersAccount(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("staff1110")

	owner := "sowner-" + suffix
	dev := "sdev-" + suffix
	admin := "sadmin-" + suffix
	seedPrincipal(t, eng, owner, auth.RoleOwner)
	seedPrincipal(t, eng, dev, auth.RoleDeveloper)
	seedPrincipal(t, eng, admin, auth.RoleAdmin)

	acct := "v1:accounts:account:s-" + suffix
	seedAccountOwnedBy(t, eng, owner, auth.RoleOwner, acct, "Owner's client")
	byId := fmt.Sprintf(`query clientAccountById(accountId: %s)`, langparser.QuoteString(BareShortId(acct)))

	res, err := eng.Execute(rankActorCtx(dev, auth.RoleDeveloper), byId)
	if err != nil {
		t.Fatalf("a developer's by-id read of the owner's account was refused: %v", err)
	}
	if n := len(res.Bundle.GetNodes()); n != 1 {
		t.Fatalf("a developer read %d rows of the owner's account by id, want 1", n)
	}

	rename := fmt.Sprintf(`mutation updateClientAccount(accountId: %s, name: "Renamed by a developer")`,
		langparser.QuoteString(BareShortId(acct)))
	if _, err := eng.Execute(rankActorCtx(dev, auth.RoleDeveloper), rename); err != nil {
		t.Fatalf("a developer could not edit the owner's account: %v", err)
	}
	res, err = eng.Execute(rankActorCtx(owner, auth.RoleOwner), byId)
	if err != nil || len(res.Bundle.GetNodes()) != 1 {
		t.Fatalf("the owner's read-back failed: %v", err)
	}
	if name := res.Bundle.GetNodes()[0].GetPayload().AsMap()["name"]; name != "Renamed by a developer" {
		t.Fatalf("after the developer's edit the account is named %v", name)
	}

	res, err = eng.Execute(rankActorCtx(admin, auth.RoleAdmin), byId)
	if err == nil && len(res.Bundle.GetNodes()) != 0 {
		t.Fatal("an admin can read an account owned above their rank; the rank tier must still bind admins")
	}
}

// The construct floor and the row tier are different gates, and a caller below
// the floor gets a REFUSAL rather than an empty page.
//
// The distinction is the whole reason the floor exists: an empty result is
// indistinguishable from "there is nothing here", which is the answer somebody
// who may not reach the surface should never be handed.
func TestACallerBelowTheConstructFloorIsRefusedNotEmptied(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("rankfloor4836")
	writer := "writer-" + suffix
	seedPrincipal(t, eng, writer, auth.RoleWriter)

	_, err := eng.Execute(rankActorCtx(writer, auth.RoleWriter), `query groupsForUser(userId: "other-user", includeRemoved: true)`)
	if err == nil {
		t.Fatal("a writer's lookup of another principal's groups was ALLOWED. @requiresRank(\"admin\") is " +
			"rank >= 200 and writer is 100; an empty page would be indistinguishable from an " +
			"empty cluster, which is why this refuses instead")
	}
	if !strings.Contains(err.Error(), "admin") {
		t.Fatalf("the refusal was %q, which does not name the role required", err)
	}
}

// THE PAGINATION INTERACTION, which the design gives its own bullet.
//
// A post-filter-only implementation fills the scan window with rows the gate
// then drops; a short page is read as EXHAUSTION by the cursor logic, the
// cursor is withdrawn, and everything past it becomes unreachable. This seeds
// a wall of rows the reader must NOT see, followed by one they must, and
// requires paging to reach the visible one.
//
// THE WALL IS DEEPER THAN A PAGE, which is what lets this test fail. It used
// to read the unpaginated query, whose page is MaxResults (500 rows), behind
// a 25-row wall: a post-filter-only gate dropped 25 of 500 rows, still filled
// most of the page, and the test passed against exactly the implementation it
// exists to refuse. It did so from 2026-09-22 (87138fb3f0, the organization
// boundary) to 2026-11-09, while no operator's rank term reached SQL. A test
// database holding a few hundred leftover organizations exposed it: the wall
// then fell inside the first 500-row window, the gate shortened that page,
// and the admin's own account became unreachable.
//
// AND IT PAGES until the row arrives or the cursor ends. With the term pushed
// down, earlier runs' accounts that this admin MAY see can still sort ahead of
// this run's row on a reused database, so one page would measure how crowded
// the database is rather than the trap.
func TestAPageFullOfHigherRankedRowsDoesNotReadAsExhaustion(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("rankpage4834")

	admin := "padmin-" + suffix
	owner := "powner-" + suffix
	seedPrincipal(t, eng, admin, auth.RoleAdmin)
	seedPrincipal(t, eng, owner, auth.RoleOwner)

	// An explicit page keeps the wall deeper than a page whatever the engine's
	// configured MaxResults is, and keeps the seeding cheap.
	const pageSize = 10
	const wall = 25

	// A wall of owner-owned rows the admin may not see. Named so they sort
	// BEFORE the visible one under the query's `sort "name", "asc"`, which is
	// what puts them in the first scan windows.
	hidden := map[string]bool{}
	for i := 0; i < wall; i++ {
		id := fmt.Sprintf("v1:accounts:account:aa%02d-%s", i, suffix)
		seedAccountOwnedBy(t, eng, owner, auth.RoleOwner, id, fmt.Sprintf("AAA hidden %02d", i))
		hidden[BareShortId(id)] = true
	}
	// Directly behind the wall, so the first page past it must carry it.
	visible := "v1:accounts:account:ab-" + suffix
	seedAccountOwnedBy(t, eng, admin, auth.RoleAdmin, visible, "AAB visible")

	call := fmt.Sprintf(`paginate(query clientAccountsAll(includeArchived: true), %d)`, pageSize)
	ctx := rankActorCtx(admin, auth.RoleAdmin)
	cursor, pages, rows, reached := "", 0, 0, false
	for !reached {
		// A bound, not an expectation: the row sorts directly behind the wall.
		if pages == 500 {
			t.Fatalf("paged %d times without reaching the admin's own account: the cursor is not "+
				"advancing, or %d accounts this admin may read sort ahead of it", pages, pages*pageSize)
		}
		res, err := eng.Execute(ContextWithCursor(ctx, cursor), call)
		if err != nil {
			t.Fatalf("page %d of the admin's read: %v", pages+1, err)
		}
		pages++
		for _, n := range res.Bundle.GetNodes() {
			rows++
			id := BareShortId(n.GetId())
			if hidden[id] {
				t.Fatalf("the admin read %s, an account owned above their rank", id)
			}
			if id == BareShortId(visible) {
				reached = true
			}
		}
		if cursor = res.GetMeta().Cursor; cursor == "" {
			break
		}
	}
	if !reached {
		t.Fatalf("the admin's OWN account did not come back behind %d rows they may not see: "+
			"the read ended after %d page(s) of %d holding %d row(s).\n"+
			"That is the pagination trap executor_filter.go documents: the rank term was "+
			"post-filtered rather than pushed down, the scan window filled with the hidden rows, "+
			"the short page read as exhaustion, and everything past it -- including this row -- "+
			"became unreachable.", wall, pages, pageSize, rows)
	}
}

// D4, per non-principal actor class: the rank rules do not govern them.
//
// The failure this prevents is a retention sweep that retires nothing, which
// looks exactly like a sweep with nothing to retire.
func TestNonPrincipalActorsAreNotGovernedByRank(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("rankd4")

	owner := "d4owner-" + suffix
	seedPrincipal(t, eng, owner, auth.RoleOwner)
	acct := "v1:accounts:account:d4-" + suffix
	seedAccountOwnedBy(t, eng, owner, auth.RoleOwner, acct, "Owned by an owner")

	for _, c := range []struct {
		name string
		ctx  context.Context
	}{
		{"a maintenance actor", auth.ContextWithAccess(context.Background(), auth.MaintenanceActor(auth.AutomationOriginEngine, "seedSelfAccount"))},
		{"the seed materializer", systemActorContext(context.Background())},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := auth.ContextWithToken(c.ctx, &auth.TokenInfo{Subject: "system"})
			// The write guard is what D4 is about: an unranked actor keeps the
			// cluster-owner escape it carries RoleOwner for.
			decl := rowAuthzDeclFor(conceptAccountsAccount)
			if decl == nil {
				t.Fatal("v1:accounts:account declares no tier; this test measures nothing")
			}
			if _, escaped := rowAuthzWriteEscapeFor(ctx, decl); !escaped {
				t.Fatalf("%s lost the write escape on the accounts tier. Every boot seed and "+
					"retention sweep over a rank concept would stop, silently.", c.name)
			}
			// And it can still READ the row -- the sweep has to find work
			// before it can do any.
			res, err := eng.Execute(auth.ContextWithInternalOrigin(ctx),
				fmt.Sprintf(`concept==%q && row.id==%q`, conceptAccountsAccount, acct))
			if err != nil {
				t.Fatalf("%s could not read the row it must sweep: %v", c.name, err)
			}
			if len(res.Bundle.GetNodes()) == 0 {
				t.Fatalf("%s read ZERO rows for an id that exists -- the shape of a sweep that "+
					"retires nothing and reports success", c.name)
			}
		})
	}
}

// TestCreateRoleAcceptsAliases is the gate that would have caught `aliases`
// landing null on every seeded role row.
//
// THE TEXT GATES COULD NOT. component/auth/role_ladder_client_parity_test.go
// parses `dsl/rbac/seeds.memql` as TEXT and compares numbers; memqllint loads
// the tree; TestEngineInitLoadsFullDSL boots the engine. All three were green
// while `createRole` neither declared nor accepted the field, so the seed
// materializer -- which writes base roles THROUGH this mutation -- dropped it
// on every write and every stored row carried null.
//
// What made it invisible is worth stating, because it is the reason a DSL
// field needs a test at the MUTATION and not only at the seed: the ENGINE has
// a compiled fallback (`auth.RoleRank`) that knows the legacy five, so nothing
// server-side misbehaved. MemQL OS has no fallback BY DESIGN -- deleting it
// was the point of D1 -- so `roleRungOf("writer")` answered nil, `roleRank`
// answered -1, and roleAdmits refused every gated surface to every writer and
// reader in the cluster. The OS suite passed throughout, because its harness
// installs a fixture ladder that has the aliases the database does not.
//
// Written against createRole rather than against the seeded rows so it is
// deterministic: the shared test database holds whatever a previous boot
// seeded, and a test that reads those rows measures that history rather than
// this contract.
func TestCreateRoleAcceptsAliases(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("alias4832")
	slug := "lead-" + suffix
	alias := "legacylead-" + suffix

	// THE SEED MATERIALIZER'S OWN ACTOR, which is the writer this test is
	// about -- base roles are written through createRole by exactly this
	// caller, and its dropped `aliases` is the defect.
	//
	// It also sidesteps a dependency that made an earlier version of this test
	// pass locally and fail in CI. The rank-bound guard (memql#2072) resolves
	// the CREATOR's rank by reading the role CATALOG, so an ordinary
	// owner-role caller ranks 400 against a seeded database and 0 against a
	// fresh one -- and 0 forbids authoring anything. A system actor is exempt
	// (isSystemActor), so this measures the mutation's accept list rather than
	// whether the catalog happened to be seeded first.
	ctx := auth.ContextWithInternalOrigin(systemActorContext(context.Background()))
	q := fmt.Sprintf(`mutation createRole(roleId: %s, slug: %s, name: %s, rank: 250, aliases: [%s])`,
		langparser.QuoteString("v1:rbac:role:"+slug),
		langparser.QuoteString(slug),
		langparser.QuoteString("Lead"),
		langparser.QuoteString(alias))
	if _, err := eng.Execute(ctx, q); err != nil {
		t.Fatalf("createRole with aliases: %v", err)
	}

	ladder := eng.rankLadder(context.Background())
	if got := ladder.rankOf(slug); got != 250 {
		t.Fatalf("the custom role's own slug ranks %d, want 250 -- the row did not land", got)
	}
	if _, stored := ladder.ranks[alias]; !stored {
		t.Fatalf("createRole DROPPED `aliases`: %q resolves to no rung in the ladder read from rows.\n"+
			"A field the mutation does not ACCEPT is a field the seed materializer writes as null,\n"+
			"in silence. The engine survives that through auth.RoleRank; MemQL OS deleted its\n"+
			"fallback deliberately (epic memql#4832, D1) and would refuse every gated surface to\n"+
			"every principal holding an alias-only role.", alias)
	}
	if got := ladder.rankOf(alias); got != 250 {
		t.Fatalf("the alias ranks %d, want 250 -- it resolved to the wrong rung", got)
	}
}
