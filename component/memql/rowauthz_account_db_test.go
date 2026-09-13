package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// The account grant against a real database (epic memql#5165, task memql#5169).
//
// The unit tests beside this file inject a resolved scope and measure the GATE.
// These measure the RESOLUTION: real membership rows, real group rows, the real
// SQL the filter compiles to, and the real plan cache. Every claim the design
// record makes about "a member of Acme's group reads Acme's rows and not
// Beta's" is a claim about this path.
//
// Subject concept: v1:platform:site, deliberately, rather than a fixture. It is
// one of the concepts the epic actually declared the argument on, so a change
// that silently dropped the declaration would fail here.

// seedGroup writes a group row through the real @serverOnly mutation.
func seedGroup(t *testing.T, eng *MemQLEngine, id, name, kind, accountId string) {
	t.Helper()
	q := fmt.Sprintf(
		`mutation writeGroup(groupId: %s, name: %s, kind: %s, accountId: %s, status: "active")`,
		langparser.QuoteString(id), langparser.QuoteString(name),
		langparser.QuoteString(kind), langparser.QuoteString(accountId))
	if _, err := eng.Execute(groupSeedCtx(), q); err != nil {
		t.Fatalf("seed group %s: %v", id, err)
	}
}

// seedMembership writes a membership row at the derived id.
func seedMembership(t *testing.T, eng *MemQLEngine, groupId, userId, status string) {
	t.Helper()
	q := fmt.Sprintf(
		`mutation writeGroupMembership(membershipId: %s, groupId: %s, userId: %s, origin: "added", status: %s)`,
		langparser.QuoteString(groupId+"-"+userId), langparser.QuoteString(groupId),
		langparser.QuoteString(userId), langparser.QuoteString(status))
	if _, err := eng.Execute(groupSeedCtx(), q); err != nil {
		t.Fatalf("seed membership %s/%s: %v", groupId, userId, err)
	}
}

// groupSeedCtx is the scaffolding actor. Internal origin because both writers
// are @serverOnly, and a cluster owner because the rows are unowned.
func groupSeedCtx() context.Context {
	ctx := auth.ContextWithInternalOrigin(
		auth.ContextWithAccess(context.Background(), &auth.AccessContext{
			UserId: "account-db-seeder",
			Role:   auth.RoleOwner,
		}))
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "account-db-seeder"})
}

// seedSiteFor writes a site row owned by `owner` and tied to `accountId`.
//
// A RAW INSERT rather than createSite: this is scaffolding standing in for the
// deployables flow, and createSite's hostname policy would refuse the made-up
// names these tests use.
func seedSiteFor(t *testing.T, eng *MemQLEngine, id, owner, accountId string) {
	t.Helper()
	payload := map[string]any{
		"ownerUserId": owner,
		"title":       id,
		"hostname":    id + ".example.test",
		"status":      "live",
		"kind":        "spa",
		"bundleRef":   "blob://sites/" + id + "/v1/",
	}
	if accountId != "" {
		payload["accountId"] = accountId
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal site: %v", err)
	}
	q := fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString(conceptPlatformSite), langparser.QuoteString(id), string(encoded))
	if _, err := eng.Execute(groupSeedCtx(), q); err != nil {
		t.Fatalf("seed site %s: %v", id, err)
	}
}

// sitesVisibleTo reads every site the caller may see, through the ordinary
// read path, and returns their bare ids.
func sitesVisibleTo(t *testing.T, eng *MemQLEngine, userId string, role auth.Role) map[string]bool {
	t.Helper()
	res, err := eng.Execute(rankActorCtx(userId, role),
		fmt.Sprintf(`concept==%s`, langparser.QuoteString(conceptPlatformSite)))
	if err != nil {
		t.Fatalf("read sites as %s: %v", userId, err)
	}
	out := map[string]bool{}
	for _, n := range res.Bundle.GetNodes() {
		out[BareShortId(n.GetId())] = true
	}
	return out
}

func TestAccountGrantAdmitsAMemberAcrossTheWholeReadPath(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("acctgrant")

	acme := "acme-" + suffix
	beta := "beta-" + suffix
	member := "member-" + suffix
	stranger := "stranger-" + suffix
	builder := "builder-" + suffix

	seedPrincipal(t, eng, member, auth.RoleWriter)
	seedPrincipal(t, eng, stranger, auth.RoleWriter)
	seedPrincipal(t, eng, builder, auth.RoleWriter)

	seedGroup(t, eng, "g-"+acme, "Acme", "account", acme)
	seedGroup(t, eng, "g-"+beta, "Beta", "account", beta)
	seedMembership(t, eng, "g-"+acme, member, "active")

	// Both sites are owned by somebody ELSE, which is the whole point: under
	// a plain owner tier neither would be visible to anybody but the builder.
	seedSiteFor(t, eng, "site-acme-"+suffix, builder, acme)
	seedSiteFor(t, eng, "site-beta-"+suffix, builder, beta)
	seedSiteFor(t, eng, "site-untied-"+suffix, builder, "")

	visible := sitesVisibleTo(t, eng, member, auth.RoleWriter)
	if !visible["site-acme-"+suffix] {
		t.Fatal("a member of Acme's group cannot read Acme's site -- the grant did not apply")
	}
	if visible["site-beta-"+suffix] {
		t.Fatal("a member of Acme's group can read BETA's site -- the grant is not scoped")
	}
	if visible["site-untied-"+suffix] {
		t.Fatal("a member can read an UNTIED site -- a row belonging to no client is its owner's")
	}

	// The negative control that makes the positive one mean something: a
	// person in NO group sees none of them.
	none := sitesVisibleTo(t, eng, stranger, auth.RoleWriter)
	if none["site-acme-"+suffix] || none["site-beta-"+suffix] {
		t.Fatal("somebody in no group can read a tied site, so the read above proves nothing")
	}
}

func TestAccountGrantIgnoresRemovedMembershipsAndArchivedGroups(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("acctstale")

	account := "acct-" + suffix
	removed := "removed-" + suffix
	archived := "archived-" + suffix
	builder := "builder-" + suffix

	for _, u := range []string{removed, archived, builder} {
		seedPrincipal(t, eng, u, auth.RoleWriter)
	}
	seedSiteFor(t, eng, "site-"+suffix, builder, account)

	// A membership that was made and then REMOVED. The old version is still
	// in the table -- rows are append-only -- so this is the case the
	// newest-version-per-id collapse exists for.
	seedGroup(t, eng, "g-live-"+suffix, "Live", "account", account)
	seedMembership(t, eng, "g-live-"+suffix, removed, "active")
	seedMembership(t, eng, "g-live-"+suffix, removed, "removed")

	// A live membership in a group that was archived afterwards.
	seedGroup(t, eng, "g-gone-"+suffix, "Gone", "custom", account)
	seedMembership(t, eng, "g-gone-"+suffix, archived, "active")
	if _, err := eng.Execute(groupSeedCtx(), fmt.Sprintf(
		`mutation writeGroup(groupId: %s, name: "Gone", kind: "custom", accountId: %s, status: "archived")`,
		langparser.QuoteString("g-gone-"+suffix), langparser.QuoteString(account))); err != nil {
		t.Fatalf("archive group: %v", err)
	}

	if sitesVisibleTo(t, eng, removed, auth.RoleWriter)["site-"+suffix] {
		t.Fatal("a REMOVED member still reads the account's site -- the version collapse is not happening")
	}
	if sitesVisibleTo(t, eng, archived, auth.RoleWriter)["site-"+suffix] {
		t.Fatal("a member of an ARCHIVED group still reads the account's site")
	}

	// The positive control: an active membership in an active group DOES
	// read it, so the two refusals above are about staleness rather than
	// about the grant never working.
	live := "live-" + suffix
	seedPrincipal(t, eng, live, auth.RoleWriter)
	seedMembership(t, eng, "g-live-"+suffix, live, "active")
	if !sitesVisibleTo(t, eng, live, auth.RoleWriter)["site-"+suffix] {
		t.Fatal("an active member cannot read the site, so both refusals above prove nothing")
	}
}

func TestAccountGrantAdmitsStaffToEveryTiedRow(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("acctstaff")

	dev := "dev-" + suffix
	builder := "builder-" + suffix
	seedPrincipal(t, eng, dev, auth.RoleDeveloper)
	seedPrincipal(t, eng, builder, auth.RoleWriter)

	seedSiteFor(t, eng, "site-a-"+suffix, builder, "acct-a-"+suffix)
	seedSiteFor(t, eng, "site-b-"+suffix, builder, "acct-b-"+suffix)
	seedSiteFor(t, eng, "site-untied-"+suffix, builder, "")

	// A developer is in NO group here. The staff rule is a rule, not rows.
	visible := sitesVisibleTo(t, eng, dev, auth.RoleDeveloper)
	if !visible["site-a-"+suffix] || !visible["site-b-"+suffix] {
		t.Fatalf("staff cannot read every tied site: %v", visible)
	}
	// "Every TIED row", not "every row". A site belonging to no client is
	// its owner's work, and staff reach it only if some other branch admits
	// them -- which at developer rank the rank branch does not, because
	// rankVisible is not declared on this concept.
	if visible["site-untied-"+suffix] {
		t.Fatal("staff read an UNTIED peer site -- the staff rule widened past what it says")
	}
}

func TestAccountGrantAdmitsAListFieldByOverlap(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("acctlist")

	acme := "acme-" + suffix
	member := "member-" + suffix
	builder := "builder-" + suffix
	seedPrincipal(t, eng, member, auth.RoleWriter)
	seedPrincipal(t, eng, builder, auth.RoleWriter)
	seedGroup(t, eng, "g-"+acme, "Acme", "account", acme)
	seedMembership(t, eng, "g-"+acme, member, "active")

	// v1:work:goal declares account="accountIds" -- the LIST form, and the
	// one the single jsonb containment test has to answer for as well.
	seedGoal := func(id string, accounts []string) {
		t.Helper()
		payload := map[string]any{
			"ownerUserId": builder,
			"statement":   id,
			"status":      "open",
			"origin":      "user",
		}
		if accounts != nil {
			payload["accountIds"] = accounts
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal goal: %v", err)
		}
		if _, err := eng.Execute(groupSeedCtx(), fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
			langparser.QuoteString("v1:work:goal"), langparser.QuoteString(id), string(encoded))); err != nil {
			t.Fatalf("seed goal %s: %v", id, err)
		}
	}
	seedGoal("goal-hit-"+suffix, []string{"other-" + suffix, acme})
	seedGoal("goal-miss-"+suffix, []string{"other-" + suffix})
	seedGoal("goal-empty-"+suffix, []string{})

	res, err := eng.Execute(rankActorCtx(member, auth.RoleWriter), `concept=="v1:work:goal"`)
	if err != nil {
		t.Fatalf("read goals: %v", err)
	}
	visible := map[string]bool{}
	for _, n := range res.Bundle.GetNodes() {
		visible[BareShortId(n.GetId())] = true
	}
	if !visible["goal-hit-"+suffix] {
		t.Fatal("a list field containing the member's account did not admit the row -- the overlap form is broken")
	}
	if visible["goal-miss-"+suffix] {
		t.Fatal("a list field WITHOUT the member's account admitted the row")
	}
	if visible["goal-empty-"+suffix] {
		t.Fatal("an EMPTY list admitted the row -- an untied row is not everyone's")
	}
}

func TestAccountGrantAdmitsAWriteToATiedRow(t *testing.T) {
	// Design D4: a member may WRITE a tied row whatever the owner's rank.
	// This is the one place the program widens the rank record's read-only
	// peer rule, and it is the purpose of the program.
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("acctwrite")

	acme := "acme-" + suffix
	member := "member-" + suffix
	stranger := "stranger-" + suffix
	builder := "builder-" + suffix
	for _, u := range []string{member, stranger, builder} {
		seedPrincipal(t, eng, u, auth.RoleWriter)
	}
	seedGroup(t, eng, "g-"+acme, "Acme", "account", acme)
	seedMembership(t, eng, "g-"+acme, member, "active")
	seedSiteFor(t, eng, "site-"+suffix, builder, acme)

	// A raw INSERT at the same id, which is how an append-only row is
	// updated: the write guard runs on the prior row either way, and that
	// guard is what is under test.
	writeAs := func(userId string) error {
		payload, _ := json.Marshal(map[string]any{
			"ownerUserId": builder,
			"title":       "edited by " + userId,
			"hostname":    "site-" + suffix + ".example.test",
			"status":      "live",
			"kind":        "spa",
			"bundleRef":   "blob://sites/site-" + suffix + "/v2/",
			"accountId":   acme,
		})
		_, err := eng.Execute(rankActorCtx(userId, auth.RoleWriter),
			fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
				langparser.QuoteString(conceptPlatformSite),
				langparser.QuoteString("site-"+suffix), string(payload)))
		return err
	}
	if err := writeAs(member); err != nil {
		t.Fatalf("a member writing a tied row was refused: %v", err)
	}
	if err := writeAs(stranger); err == nil {
		t.Fatal("somebody in no group wrote the tied row, so the admission above proves nothing")
	}
}

func TestAccountGrantAppliesToAReadIssuedAfterTheMembershipLands(t *testing.T) {
	// The grant is resolved PER REQUEST, so a membership written between two
	// reads changes the second one.
	//
	// EACH READ CARRIES ITS OWN QUERY STRING, and that is not a trick to make
	// the test pass -- it is the test being honest about what it measures.
	// Read results are cached for 60 seconds under a signature that folds in
	// the ACTOR but not their memberships (engine.go's planCacheSignature:
	// the account fingerprint joins the key only when the plan actually
	// carries an account term, which an UNBOUND generic browse never does).
	// So on the browse path a freshly-placed member waits out the TTL. That
	// is the rank scope's own pre-existing shape rather than anything this
	// epic introduces, and it is stated here rather than papered over,
	// because a test that quietly avoided it would leave the next reader
	// believing the browse is immediate.
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("acctfresh")

	acme := "acme-" + suffix
	member := "member-" + suffix
	builder := "builder-" + suffix
	seedPrincipal(t, eng, member, auth.RoleWriter)
	seedPrincipal(t, eng, builder, auth.RoleWriter)
	seedGroup(t, eng, "g-"+acme, "Acme", "account", acme)
	seedSiteFor(t, eng, "site-"+suffix, builder, acme)

	read := func(tag string) bool {
		t.Helper()
		// The extra always-true term makes the query string -- and therefore
		// the cache signature -- distinct per read.
		q := fmt.Sprintf(`concept==%s && id!=%s`,
			langparser.QuoteString(conceptPlatformSite), langparser.QuoteString("never-"+tag))
		res, err := eng.Execute(rankActorCtx(member, auth.RoleWriter), q)
		if err != nil {
			t.Fatalf("read %s: %v", tag, err)
		}
		for _, n := range res.Bundle.GetNodes() {
			if BareShortId(n.GetId()) == "site-"+suffix {
				return true
			}
		}
		return false
	}

	if read("before") {
		t.Fatal("the site is visible before any membership exists")
	}
	seedMembership(t, eng, "g-"+acme, member, "active")
	if !read("after") {
		t.Fatal("the site is still invisible after the membership landed -- the grant is not resolved per request")
	}
}

func TestAccountGrantReachesTheAccountView(t *testing.T) {
	// A PROPERTY AUTHORS MUST KNOW, and the one that turns "I declared the
	// argument and nothing changed" into an explicable answer.
	//
	// The tier's predicate is ANDed into a bound read. A query whose own
	// filter says `ownerUserId==actor.userId` therefore stays owner-scoped no
	// matter what the concept declares: the grant widens the TIER, and an AND
	// with a hand-written owner conjunct cannot be widened by anything.
	//
	// sitesForAccount WAS exactly that shape, and this test used to assert
	// that a member did NOT see the tied site through it. The query was
	// rewritten deliberately under design D4 of
	// docs/superpowers/specs/2026-09-11-app-access-grants-design.md
	// (memql#5303): the account view is the one read whose whole purpose is
	// the tie, and a hand-written `own || clusterOwner` conjunct there was
	// the tier's first two arms restated, minus the third. So the assertion
	// is now the positive one, and the negative control beside it is what
	// keeps it from proving nothing.
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("acctview")

	acme := "acme-" + suffix
	member := "member-" + suffix
	stranger := "stranger-" + suffix
	builder := "builder-" + suffix
	seedPrincipal(t, eng, member, auth.RoleWriter)
	seedPrincipal(t, eng, stranger, auth.RoleWriter)
	seedPrincipal(t, eng, builder, auth.RoleWriter)
	seedGroup(t, eng, "g-"+acme, "Acme", "account", acme)
	seedMembership(t, eng, "g-"+acme, member, "active")
	seedSiteFor(t, eng, "site-"+suffix, builder, acme)

	sees := func(userId string) bool {
		t.Helper()
		res, err := eng.Execute(rankActorCtx(userId, auth.RoleWriter),
			fmt.Sprintf(`query sitesForAccount(accountId: %s)`, langparser.QuoteString(acme)))
		if err != nil {
			t.Fatalf("sitesForAccount as %s: %v", userId, err)
		}
		for _, n := range res.Bundle.GetNodes() {
			if BareShortId(n.GetId()) == "site-"+suffix {
				return true
			}
		}
		return false
	}
	if !sees(member) {
		t.Fatal("sitesForAccount did not return a tied site to a member of that account's group -- " +
			"the account view narrows past the tier again. If a caller-scope conjunct was re-added " +
			"to the query, that reverses design D4 and this test should say so.")
	}
	if sees(stranger) {
		t.Fatal("sitesForAccount returned the site to somebody in no group, so the read above proves nothing")
	}
}

// THE GROUP READS THEMSELVES, through the real engine (epic memql#5165).
//
// integrations/groups reads every one of these under its synthetic
// cluster-owner actor, and its own tests drive a stub that evaluates no
// filter. So without this, the five queries the whole plug-in depends on are
// covered by nothing that would notice a filter change breaking them -- which
// is exactly what the `requiresDeveloperOrAbove` conjunct is: a change to
// every one of their filters, made to satisfy a gate.
func TestGroupQueriesAnswerForTheSystemActorAndRefuseBelowTheFloor(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("groupreads")

	account := "acct-" + suffix
	member := "member-" + suffix
	seedPrincipal(t, eng, member, auth.RoleWriter)
	seedGroup(t, eng, "g-"+suffix, "Readable", "custom", account)
	seedMembership(t, eng, "g-"+suffix, member, "active")

	// The plug-in's own actor: internal origin plus a cluster owner, which is
	// what groups.SystemActorContext stamps.
	sys := groupSeedCtx()

	rowsOf := func(t *testing.T, ctx context.Context, q string) int {
		t.Helper()
		res, err := eng.Execute(ctx, q)
		if err != nil {
			// A refusal is an answer -- the @requiresRank floor turning a
			// caller away -- and it is distinct from an empty result, which
			// is the distinction these queries rest on.
			return -1
		}
		return len(res.Bundle.GetNodes())
	}

	for _, tc := range []struct{ name, query string }{
		{"groupsAll", `query groupsAll(includeArchived: true)`},
		{"groupById", fmt.Sprintf(`query groupById(groupId: %s)`, langparser.QuoteString("g-"+suffix))},
		{"groupsForAccount", fmt.Sprintf(`query groupsForAccount(accountId: %s)`, langparser.QuoteString(account))},
		{"membersOfGroup", fmt.Sprintf(`query membersOfGroup(groupId: %s, includeRemoved: true)`, langparser.QuoteString("g-"+suffix))},
		{"groupsForUser", fmt.Sprintf(`query groupsForUser(userId: %s, includeRemoved: true)`, langparser.QuoteString(member))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rowsOf(t, sys, tc.query); got <= 0 {
				t.Fatalf("%s answered %d rows for the system actor. The plug-in reads through this "+
					"query under exactly that actor, so a filter it cannot satisfy makes every group "+
					"operation fail -- and no stub-driven test would see it.", tc.name, got)
			}
			// ...and a Member is refused rather than served, which is what
			// the @requiresRank floor and the conjunct together are for.
			if got := rowsOf(t, rankActorCtx(member, auth.RoleWriter), tc.query); got > 0 {
				t.Fatalf("%s answered %d rows to a writer -- these reads are admin-floored", tc.name, got)
			}
		})
	}
}

// The three @serverOnly account reads this epic added, for the same reason:
// each runs under a system actor at boot or on a schedule, and a conjunct that
// actor cannot satisfy turns the walk, the backfill or domain join into a
// silent no-op that reports success.
func TestTheServerOnlyAccountReadsAnswerForTheirSystemActors(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("sysreads")
	sys := groupSeedCtx()

	// An account with a domain that is not yet verified, which is what the
	// walk looks for, and joining on -- refused before verification, so it
	// is stamped through the walk's own writer rather than at create.
	account := "acct-" + suffix
	seedAccountOwnedBy(t, eng, "owner-"+suffix, auth.RoleOwner, account, "Acme "+suffix)
	if _, err := eng.Execute(sys, fmt.Sprintf(
		`mutation updateClientAccount(accountId: %s, domain: %s)`,
		langparser.QuoteString(account), langparser.QuoteString("acme-"+suffix+".test"))); err != nil {
		t.Fatalf("set the account's domain: %v", err)
	}

	if res, err := eng.Execute(sys, `query accountsForGroupSweep()`); err != nil {
		t.Fatalf("accountsForGroupSweep is unreadable by the seed materializer's actor: %v", err)
	} else if len(res.Bundle.GetNodes()) == 0 {
		t.Fatal("accountsForGroupSweep answered nothing -- the boot backfill would give no account " +
			"a group, and would report success doing it")
	}
	if res, err := eng.Execute(sys, `query accountsForDomainWalk()`); err != nil {
		t.Fatalf("accountsForDomainWalk is unreadable by the reconciler's actor: %v", err)
	} else if len(res.Bundle.GetNodes()) == 0 {
		t.Fatal("accountsForDomainWalk answered nothing -- every client's domain would stay " +
			"unverified forever, with the panel showing a record that is published correctly")
	}
	// accountForDomainJoin answers nothing here on purpose: the account is
	// not verified and not joining. What is asserted is that it RUNS -- an
	// error would mean domain join is refused rather than declining.
	if _, err := eng.Execute(sys, fmt.Sprintf(`query accountForDomainJoin(domain: %s)`,
		langparser.QuoteString("acme-"+suffix+".test"))); err != nil {
		t.Fatalf("accountForDomainJoin is unreadable by the groups actor: %v", err)
	}
}
