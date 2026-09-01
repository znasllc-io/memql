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
			name:      "a developer sees both admins and their own, not the owner's",
			actor:     dev,
			role:      auth.RoleDeveloper,
			sees:      []string{acctAdmin, acctAdmin2, acctDev},
			blind:     []string{acctOwner},
			rationale: "developer (300) outranks admin (200) -- the flipped ladder, measured",
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

	_, err := eng.Execute(rankActorCtx(writer, auth.RoleWriter), `query clientAccountsAll(includeArchived: true)`)
	if err == nil {
		t.Fatal("a writer's read of the accounts registry was ALLOWED. @requiresRank(\"admin\") is " +
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
// a run of rows the reader must NOT see, followed by one they must, and
// requires the visible one to come back.
func TestAPageFullOfHigherRankedRowsDoesNotReadAsExhaustion(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("rankpage4834")

	admin := "padmin-" + suffix
	owner := "powner-" + suffix
	seedPrincipal(t, eng, admin, auth.RoleAdmin)
	seedPrincipal(t, eng, owner, auth.RoleOwner)

	// A wall of owner-owned rows the admin may not see. Named so they sort
	// BEFORE the visible one under the query's `sort "name", "asc"`, which is
	// what puts them in the first scan window.
	for i := 0; i < 25; i++ {
		seedAccountOwnedBy(t, eng, owner, auth.RoleOwner,
			fmt.Sprintf("v1:accounts:account:aa%02d-%s", i, suffix),
			fmt.Sprintf("AAA hidden %02d", i))
	}
	visible := "v1:accounts:account:zz-" + suffix
	seedAccountOwnedBy(t, eng, admin, auth.RoleAdmin, visible, "ZZZ visible")

	got := accountsVisibleTo(t, eng, admin, auth.RoleAdmin)
	if got == nil {
		t.Fatal("the admin's read was refused")
	}
	if !got[BareShortId(visible)] {
		t.Fatalf("the admin's OWN account did not come back behind 25 rows they may not see.\n"+
			"That is the pagination trap executor_filter.go documents: if the rank term were "+
			"post-filtered rather than pushed down, the scan window would fill with the hidden "+
			"rows, the short page would read as exhaustion, and everything past it -- including "+
			"this row -- becomes unreachable. got %d row(s)", len(got))
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
		{"a maintenance actor", auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("seedSelfAccount"))},
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

	// The rank-bound guard (memql#2072) resolves the CREATOR's rank through
	// auth.UserIdentityFromContext, which reads the token's claims rather than
	// the AccessContext -- so the claims have to carry the role, or the
	// creator ranks 0 and may author nothing.
	seeder := "alias-seeder-" + suffix
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: seeder, Role: auth.RoleOwner,
	})
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{
		Subject: seeder,
		Claims:  map[string]any{"sub": seeder, "role": string(auth.RoleOwner)},
	})
	ctx = auth.ContextWithInternalOrigin(ctx)
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
