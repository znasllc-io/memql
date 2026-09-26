package packages

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// account_tie_db_test.go -- the half of memql#5303 that has to run against a
// real engine, the same split grant_db_test.go states.
//
// account_tie_test.go proves the pipeline COPIES the account onto the run.
// It cannot prove what the copy is for: that a package tied to the cluster's
// own account is readable by the people that account's group admits, and by
// staff, and that an untied one is not. That is the `account="accountId"`
// argument of the owned tier (epic memql#5165) resolved over real membership
// rows and lowered to real SQL -- and the recording engine answers whatever
// it is canned with, so it passes a concept with the declaration and one
// without it equally well.
//
// Reads go through the GENERIC BROWSE, deliberately. packagesAll and its
// siblings carry a hand-written `(ownerUserId==actor.userId ||
// actor.isClusterOwner==true)` conjunct, and the tier is ANDed into a bound
// read rather than replacing its filter, so those stay owner-scoped whatever
// the concept declares (TestAccountGrantDoesNotWidenAQueryThatNarrowsItself
// in component/memql records this). What this test measures is the tier.
//
// Postgres-gated like its neighbours: a skip is what an unreachable database
// produces, and MEMQL_REQUIRE_DB=1 turns it into a failure.

const conceptIdentityUserForTie = "v1:identity:user"

// tieSeederCtx is the scaffolding actor: internal origin because the group
// writers are @serverOnly, and a cluster owner because the rows are unowned.
func tieSeederCtx() context.Context {
	ctx := auth.ContextWithInternalOrigin(
		auth.ContextWithAccess(context.Background(), &auth.AccessContext{
			UserId:    "account-tie-seeder",
			Role:      auth.RoleOwner,
			Synthetic: true,
			Unranked:  true,
		}))
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "account-tie-seeder"})
}

// tieActorCtx is one person, as the stream interceptor would present them.
func tieActorCtx(userId string, role auth.Role) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: userId,
		Role:   role,
	})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

// seedTiePrincipal writes a v1:identity:user row so the rank resolver can
// find it. Raw insert under internal origin: scaffolding standing in for the
// identity service.
func seedTiePrincipal(t *testing.T, eng *memql.MemQLEngine, userId string, role auth.Role) {
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
		langparser.QuoteString(conceptIdentityUserForTie), langparser.QuoteString(userId), string(payload))
	if _, err := eng.Execute(tieSeederCtx(), q); err != nil {
		t.Fatalf("seed principal %s: %v", userId, err)
	}
}

// seedTieGroup writes an account-kind group through the real @serverOnly
// mutation, exactly as integrations/groups' ensureAccountGroup does.
func seedTieGroup(t *testing.T, eng *memql.MemQLEngine, id, accountId string) {
	t.Helper()
	q := fmt.Sprintf(
		`mutation writeGroup(groupId: %s, name: %s, kind: "account", accountId: %s, status: "active")`,
		langparser.QuoteString(id), langparser.QuoteString(id), langparser.QuoteString(accountId))
	if _, err := eng.Execute(tieSeederCtx(), q); err != nil {
		t.Fatalf("seed group %s: %v", id, err)
	}
}

func seedTieMembership(t *testing.T, eng *memql.MemQLEngine, groupId, userId string) {
	t.Helper()
	q := fmt.Sprintf(
		`mutation writeGroupMembership(membershipId: %s, groupId: %s, userId: %s, origin: "added", status: "active")`,
		langparser.QuoteString(groupId+"-"+memql.BareShortId(userId)), langparser.QuoteString(groupId), langparser.QuoteString(userId))
	if _, err := eng.Execute(tieSeederCtx(), q); err != nil {
		t.Fatalf("seed membership %s/%s: %v", groupId, userId, err)
	}
}

// createTiedPackage registers a package the way the compose flow does: the
// ordinary caller-scoped createPackage under the OWNER's actor, naming the
// account (or not).
func createTiedPackage(t *testing.T, eng *memql.MemQLEngine, owner context.Context, packageId, name, accountId string) {
	t.Helper()
	if accountId == "" {
		// Historical untied roots predate mandatory organization selection.
		// New createPackage calls correctly default/require an organization;
		// load the old shape explicitly rather than pretending it is new work.
		access, _ := auth.AccessFromContext(owner)
		payload, _ := json.Marshal(map[string]any{"name": name, "ownerUserId": access.UserId, "sourceKind": "repo", "repoUrl": "https://github.com/acme/" + name, "status": "active", "updateAvailable": false})
		if _, err := eng.Execute(tieSeederCtx(), fmt.Sprintf(`insert("v1:platform:package", id=%s, payload=%s)`, langparser.QuoteString(packageId), payload)); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := eng.Execute(tieSeederCtx(), fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Package test organization")`, langparser.QuoteString(accountId))); err != nil {
		t.Fatal(err)
	}
	q := fmt.Sprintf(
		`mutation createPackage(packageId: %s, name: %s, sourceKind: "repo", repoUrl: %s`,
		langparser.QuoteString(packageId), langparser.QuoteString(name),
		langparser.QuoteString("https://github.com/acme/"+name))
	if accountId != "" {
		q += fmt.Sprintf(`, accountId: %s`, langparser.QuoteString(accountId))
	}
	q += ")"
	if _, err := eng.Execute(owner, q); err != nil {
		t.Fatalf("createPackage %s: %v", packageId, err)
	}
}

// rowsVisibleTo reads every row of a concept the caller may see, through the
// ordinary read path, and returns their bare ids.
//
// The extra always-true term makes each read's query string -- and so its
// plan-cache signature -- distinct, for the reason
// TestAccountGrantAppliesToAReadIssuedAfterTheMembershipLands gives.
func rowsVisibleTo(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, conceptId, tag string) map[string]bool {
	t.Helper()
	res, err := eng.Execute(ctx, fmt.Sprintf(`concept==%s && id!=%s`,
		langparser.QuoteString(conceptId), langparser.QuoteString("never-"+tag)))
	if err != nil {
		t.Fatalf("read %s: %v", conceptId, err)
	}
	out := map[string]bool{}
	for _, n := range res.Bundle.GetNodes() {
		out[memql.BareShortId(n.GetId())] = true
	}
	return out
}

func TestAPackageTiedToTheSelfAccountIsReadableByItsGroupAndByStaff(t *testing.T) {
	eng, db := dbEngine(t)
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())

	// The cluster's own account is a singleton at a literal id, and the group
	// the sweep makes for it grants that id. Seeded HERE rather than read,
	// because the scratch database has run no seed automation -- and a second
	// group granting the same account is exactly what the resolver unions.
	const selfAccount = "self"
	group := "g-self-" + suffix

	owner := "v1:identity:user:tie-owner-" + suffix
	developer := "v1:identity:user:tie-dev-" + suffix
	member := "v1:identity:user:tie-member-" + suffix
	stranger := "v1:identity:user:tie-stranger-" + suffix
	tied := "v1:platform:package:tie-tied-" + suffix
	untied := "v1:platform:package:tie-untied-" + suffix
	run := "v1:platform:packageDeployment:tie-run-" + suffix

	t.Cleanup(func() {
		for _, id := range []string{tied, untied, run, owner, developer, member, stranger, group, group + "-" + memql.BareShortId(member)} {
			_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
		}
	})

	seedTiePrincipal(t, eng, owner, auth.RoleOwner)
	seedTiePrincipal(t, eng, developer, auth.RoleDeveloper)
	seedTiePrincipal(t, eng, member, auth.RoleWriter)
	seedTiePrincipal(t, eng, stranger, auth.RoleWriter)
	seedTieGroup(t, eng, group, selfAccount)
	seedTieMembership(t, eng, group, member)

	ownerCtx := tieActorCtx(owner, auth.RoleOwner)
	createTiedPackage(t, eng, ownerCtx, tied, "tied-"+suffix, selfAccount)
	createTiedPackage(t, eng, ownerCtx, untied, "untied-"+suffix, "")

	tiedShort := memql.BareShortId(tied)
	untiedShort := memql.BareShortId(untied)

	// ---- staff: developer rank and above are standing members of every
	// account-kind group by RULE, and this developer is in no group at all.
	staff := rowsVisibleTo(t, eng, tieActorCtx(developer, auth.RoleDeveloper), "v1:platform:package", "dev-"+suffix)
	if !staff[tiedShort] {
		t.Fatal("a developer cannot read a package tied to the cluster's own account -- the account argument is not declared on v1:platform:package, or the staff rule does not reach it")
	}
	if staff[untiedShort] {
		t.Fatal("a developer reads an UNTIED package they do not own -- the tie widened past what it says")
	}

	// ---- a user-role member of the self account's group.
	people := rowsVisibleTo(t, eng, tieActorCtx(member, auth.RoleWriter), "v1:platform:package", "member-"+suffix)
	if !people[tiedShort] {
		t.Fatal("a member of the self account's group cannot read a package tied to it")
	}
	if people[untiedShort] {
		t.Fatal("a member reads an UNTIED package they do not own")
	}

	// ---- the negative control that makes the two reads above mean
	// something: a writer in no group sees neither.
	none := rowsVisibleTo(t, eng, tieActorCtx(stranger, auth.RoleWriter), "v1:platform:package", "stranger-"+suffix)
	if none[tiedShort] || none[untiedShort] {
		t.Fatal("somebody in no group can read the package, so the reads above prove nothing")
	}

	// ---- the run timeline is readable by the same people.
	//
	// Opened the way the pipeline opens it: openDeployment borrows the
	// package owner's actor and stamps internal origin, and copies the
	// account off the package row it was handed.
	s := &store{engine: eng, logger: discardLogger(), directDB: func() *sql.DB { return db.DB }}
	pkg, err := s.packageById(ownerCtx, tied)
	if err != nil || pkg == nil {
		t.Fatalf("read the tied package as its owner: %v (%v)", err, pkg)
	}
	if err := s.openDeployment(context.Background(), deploymentSeed{
		DeploymentId: run,
		PackageId:    tied,
		OwnerUserId:  rowString(pkg, "ownerUserId"),
		AccountId:    rowString(pkg, "accountId"),
		RequestedBy:  owner,
		NodeId:       "test-node",
		StartedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("openDeployment: %v", err)
	}
	runShort := memql.BareShortId(run)
	if !rowsVisibleTo(t, eng, tieActorCtx(developer, auth.RoleDeveloper), packageDeploymentConcept, "dev-run-"+suffix)[runShort] {
		t.Fatal("a developer cannot read the tied package's run -- the account did not ride onto v1:platform:packageDeployment")
	}
	if !rowsVisibleTo(t, eng, tieActorCtx(member, auth.RoleWriter), packageDeploymentConcept, "member-run-"+suffix)[runShort] {
		t.Fatal("a member of the self account's group cannot read the tied package's run")
	}
	if rowsVisibleTo(t, eng, tieActorCtx(stranger, auth.RoleWriter), packageDeploymentConcept, "stranger-run-"+suffix)[runShort] {
		t.Fatal("somebody in no group can read the run, so the two reads above prove nothing")
	}

	// ---- THE READS THE APP MAKES, and the gate a write runs behind.
	//
	// A tie nobody can see through the Deployables app does not deliver the
	// feature (design D4: "granting Deployables shows a person the
	// deployables they are admitted to by owner, account or rank"). So the
	// same question is asked again through packagesAll, packageDeployments
	// and sitesAll -- the OS's three seeds -- and through packageById, the
	// read every packages builtin resolves its target under the CALLER's
	// actor before writing anything. Each used to carry
	// `(ownerUserId==actor.userId || actor.isClusterOwner==true)` written
	// out, which an AND could never widen; dropping it is D4 / D12, and
	// tierDecidesTheRead in component/memql records each one.
	site := "tie-site-" + suffix
	seedTieSite(t, eng, site, owner, selfAccount)
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", site).Exec(context.Background())
	})

	for _, who := range []struct {
		name string
		ctx  context.Context
	}{
		{"a developer", tieActorCtx(developer, auth.RoleDeveloper)},
		{"a member of the self account's group", tieActorCtx(member, auth.RoleWriter)},
	} {
		got := idsOf(t, eng, who.ctx, `query packagesAll()`)
		if !got[tiedShort] {
			t.Fatalf("%s cannot read the tied package through packagesAll -- the Deployables list still narrows past the tier", who.name)
		}
		if got[untiedShort] {
			t.Fatalf("%s reads an UNTIED package through packagesAll", who.name)
		}
		if !idsOf(t, eng, who.ctx, fmt.Sprintf(`query packageDeployments(packageId: %s)`, langparser.QuoteString(tied)))[runShort] {
			t.Fatalf("%s cannot read the tied package's run through packageDeployments -- the timeline still narrows past the tier", who.name)
		}
		if !idsOf(t, eng, who.ctx, `query sitesAll()`)[site] {
			t.Fatalf("%s cannot read a site tied to the self account through sitesAll", who.name)
		}
		// The by-id gate every builtin runs first, under the caller's actor.
		if row, err := s.packageById(who.ctx, tied); err != nil || row == nil {
			t.Fatalf("%s is refused the tied package by packageById (%v, %v) -- every packages builtin gates on this read", who.name, row, err)
		}
	}

	strangerCtx := tieActorCtx(stranger, auth.RoleWriter)
	if got := idsOf(t, eng, strangerCtx, `query packagesAll()`); got[tiedShort] || got[untiedShort] {
		t.Fatal("somebody in no group reads a package through packagesAll, so the reads above prove nothing")
	}
	if idsOf(t, eng, strangerCtx, `query sitesAll()`)[site] {
		t.Fatal("somebody in no group reads the tied site through sitesAll")
	}
	// ...and cannot DEPLOY a package they are not admitted to: Deploy resolves
	// its package through packageById first and refuses by name.
	_, derr := Deploy(strangerCtx, &Deps{Store: s}, DeployRequest{PackageId: tied, Actor: Actor{UserId: stranger}})
	if RefusalCode(derr) != CodeSourceUnreadable {
		t.Fatalf("a stranger deploying a tied package: want %s, got %v", CodeSourceUnreadable, derr)
	}
}

// seedTieSite writes a site row owned by `owner` and tied to `accountId`. A
// raw insert rather than createSite, whose hostname policy would refuse the
// made-up name -- scaffolding, exactly as component/memql's seedSiteFor.
func seedTieSite(t *testing.T, eng *memql.MemQLEngine, id, owner, accountId string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"ownerUserId": owner,
		"title":       id,
		"hostname":    id + ".example.test",
		"status":      "live",
		"kind":        "spa",
		"bundleRef":   "blob://sites/" + id + "/v1/",
		"accountId":   accountId,
	})
	if err != nil {
		t.Fatalf("marshal site: %v", err)
	}
	q := fmt.Sprintf(`insert("v1:platform:site", id=%s, payload=%s)`, langparser.QuoteString(id), string(payload))
	if _, err := eng.Execute(tieSeederCtx(), q); err != nil {
		t.Fatalf("seed site %s: %v", id, err)
	}
}

// idsOf runs one named query as the caller and returns the bare ids it answers.
func idsOf(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, query string) map[string]bool {
	t.Helper()
	res, err := eng.Execute(ctx, query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	out := map[string]bool{}
	for _, n := range res.Bundle.GetNodes() {
		out[memql.BareShortId(n.GetId())] = true
	}
	return out
}
