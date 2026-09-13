package memql

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// GRANTS, END TO END AGAINST A REAL ENGINE AND DATABASE (app access grants
// record, section 6 "Enforcement through the engine"; epics memql#5294,
// memql#5295, memql#5296).
//
// The unit tests in component/auth prove the RULE over values. These prove
// the ROWS reach it: a grant written through the real @serverOnly mutation at
// the derived id, memberships written through the real group mutations, the
// requires-capability gates on the real direct-call and plan paths, and the
// graph-backed source reading it all back under the engine's own identity.
//
// THE NEGATIVE CONTROL IS THE PROOF THE CALL SITES MOVED. The record says it
// in one sentence: "a role-only resolver passes none of the grant cases,
// which is what proves the seven call sites moved". So the last test here
// clears the grant source -- leaving exactly the role-shaped answer the old
// auth.Capable gave -- and asserts the user-grant case is REFUSED. Without
// that, every green case above it would also be green against a gate that
// never consulted a grant, provided the fixture happened to agree with the
// role.
//
// Postgres-gated like its neighbours; CI's db-tests lane sets
// MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.

// grantTestResource is the resource the probe constructs require `execute`
// on. It is "seeded" on the catalog FAKE below rather than on a
// v1:rbac:capability row: the shared engine never runs the seed materializer,
// so the rows are not there to lean on, and the fake is what every other
// requires-capability test in this package uses (installCapabilityFake).
const grantTestResource = "app:grant-test"

// The two probe constructs, PARSED from struct-form source through the same
// entry point the tree load uses, so the annotation reaches
// Function.RequiresCapability by the real path. They are not authored into
// the tree because a construct gating on a resource no seed declares refuses
// BOOT (validateRequiresCapabilitySlugs), and that is the right answer for
// the shipped tree. Upserted into an engine's registry for one test and
// removed afterwards.
const (
	grantProbeLogic  = "grantGateProbeLogic"
	grantProbeQuery  = "grantGateProbeQuery"
	rankProbeLogic   = "rankGateProbeLogic"
	grantProbeOrigin = "memql#5296-test"
)

const grantProbeLogicSource = `@enabled
@description("memql#5296 grant gate probe -- the direct-call path")
@requiresCapability("execute", "app:grant-test")
logic grantGateProbeLogic {
  args {
    probe string @required
  }
  body {
    return cond(args.probe == "x", "yes", "no")
  }
}
`

// rankProbeLogicSource is the RANK twin of the logic probe, for the hole the
// capability probe exposed (see TestSingleStatementLogicClearsItsFloors).
const rankProbeLogicSource = `@enabled
@description("memql#5296 rank gate probe -- a single-statement logic")
@requiresRank("admin")
logic rankGateProbeLogic {
  args {
    probe string @required
  }
  body {
    return cond(args.probe == "x", "yes", "no")
  }
}
`

const grantProbeQuerySource = `use rbac.concepts.{ role }

@enabled
@description("memql#5296 grant gate probe -- the plan-expansion path")
@requiresCapability("execute", "app:grant-test")
query role grantGateProbeQuery {
  args {
    slug string!
  }
  filter  slug == args.slug
}
`

// installGrantTestCatalog installs the role level of every case: `writer`
// (the member tier's user-row spelling) holds nothing on the resource and
// `admin` holds execute on it. Both spellings of the member tier are carried
// because capabilityFake resolves no aliases.
func installGrantTestCatalog(t *testing.T) {
	t.Helper()
	exec := auth.VerbResource{Verb: auth.VerbExecute, Resource: grantTestResource}
	auth.SetCapabilityCatalog(&capabilityFake{
		ranks: map[string]int{"owner": 400, "developer": 300, "admin": 200, "user": 100, "writer": 100, "viewer": 50, "reader": 50},
		grants: map[string]map[auth.VerbResource]bool{
			"owner":     {exec: true},
			"admin":     {exec: true},
			"developer": {},
			"user":      {},
			"writer":    {},
			"viewer":    {},
			"reader":    {},
		},
	})
	t.Cleanup(func() { auth.SetCapabilityCatalog(nil) })
}

// installGrantProbes registers the two gated constructs on an engine and
// removes them when the test ends.
func installGrantProbes(t *testing.T, eng *MemQLEngine) {
	t.Helper()
	var probes []*Function
	for _, p := range []struct{ name, kind, src string }{
		{grantProbeLogic, "logic", grantProbeLogicSource},
		{grantProbeQuery, "query", grantProbeQuerySource},
		{rankProbeLogic, "logic", rankProbeLogicSource},
	} {
		fn, err := tryParseNewFunctionSyntax(p.name, p.kind, p.src, grantProbeOrigin, concept.DefaultRegistry())
		if err != nil {
			t.Fatalf("parse probe %s: %v", p.name, err)
		}
		if p.name != rankProbeLogic && fn.RequiresCapability != (CapabilityRequirement{Verb: auth.VerbExecute, Resource: grantTestResource}) {
			t.Fatalf("probe %s parsed without its @requiresCapability: %+v", p.name, fn.RequiresCapability)
		}
		if p.name == rankProbeLogic && fn.RequiresRank != "admin" {
			t.Fatalf("probe %s parsed without its @requiresRank: %q", p.name, fn.RequiresRank)
		}
		if err := eng.Functions().Upsert(fn); err != nil {
			t.Fatalf("upsert probe %s: %v", p.name, err)
		}
		probes = append(probes, fn)
	}
	t.Cleanup(func() {
		for _, fn := range probes {
			eng.Functions().Remove(QualifyConstruct(ConstructNamespaceForOrigin(fn.Origin), fn.Name))
		}
	})
}

// grantEngine is the shared engine with its grant resolution installed, and
// the sources cleared afterwards so no later test in the package reads them.
func grantEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	eng, _, _ := sharedReadMergeEngine(t)
	installGrantTestCatalog(t)
	eng.InstallGrantResolution()
	t.Cleanup(func() {
		auth.SetGrantSource(nil)
		auth.SetMembershipSource(nil)
	})
	installGrantProbes(t, eng)
	return eng
}

// writeGrant writes one grant through the real @serverOnly mutation at the
// derived id, under the scaffolding actor the group seeds use (internal
// origin, a cluster owner -- the rows are the deployment's).
func writeGrant(t *testing.T, eng *MemQLEngine, kind, subjectId, verb, resource, effect string) string {
	t.Helper()
	id := auth.GrantRowID(kind, subjectId, verb, resource)
	q := fmt.Sprintf(
		`mutation writeGrant(grantId: %s, subjectKind: %s, subjectId: %s, verb: %s, resourceType: %s, effect: %s, grantedBy: "grant-db-seeder")`,
		langparser.QuoteString(id), langparser.QuoteString(kind), langparser.QuoteString(subjectId),
		langparser.QuoteString(verb), langparser.QuoteString(resource), langparser.QuoteString(effect))
	if _, err := eng.Execute(groupSeedCtx(), q); err != nil {
		t.Fatalf("writeGrant %s/%s %s %s %s: %v", kind, subjectId, verb, resource, effect, err)
	}
	return id
}

func deactivateGrant(t *testing.T, eng *MemQLEngine, id string) {
	t.Helper()
	q := fmt.Sprintf(`mutation deactivateGrant(grantId: %s)`, langparser.QuoteString(id))
	if _, err := eng.Execute(groupSeedCtx(), q); err != nil {
		t.Fatalf("deactivateGrant %s: %v", id, err)
	}
}

// grantOutcome drives both gated paths for one caller and reports whether
// each ADMITTED. A refusal is the gate's own message; an admitted call runs
// to completion; any other error is a broken fixture rather than a verdict.
type grantOutcome struct{ direct, plan bool }

func gateOutcomes(t *testing.T, eng *MemQLEngine, ctx context.Context) grantOutcome {
	t.Helper()
	refused := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), "requires the "+auth.VerbExecute+" on "+grantTestResource+" capability")
	}
	var out grantOutcome
	for _, path := range []struct {
		name  string
		query string
		admit *bool
	}{
		{"direct", "logic " + grantProbeLogic + `(probe: "x")`, &out.direct},
		{"plan", "query " + grantProbeQuery + `(slug: "owner")`, &out.plan},
	} {
		_, err := eng.Execute(ctx, path.query)
		switch {
		case err == nil:
			*path.admit = true
		case refused(err):
			*path.admit = false
		default:
			t.Fatalf("the %s probe failed for a reason other than the gate: %v", path.name, err)
		}
	}
	return out
}

func TestGrantEnforcementUserGrantAdmitsAndRevocationRefusesAgain(t *testing.T) {
	eng := grantEngine(t)
	suffix := uniqueSuffix("grant-user")
	member := "member-" + suffix

	// A member-tier actor holds nothing on the resource: refused on both
	// paths, and the two paths agree.
	if got := gateOutcomes(t, eng, rankActorCtx(member, auth.RoleWriter)); got != (grantOutcome{}) {
		t.Fatalf("a member with no grant was admitted: %+v", got)
	}

	// A USER grant admits them -- on the direct call AND on the plan that
	// expands the construct, which is the hole the plan gate closes.
	id := writeGrant(t, eng, auth.SubjectKindUser, member, auth.VerbExecute, grantTestResource, auth.GrantAllow)
	if got := gateOutcomes(t, eng, rankActorCtx(member, auth.RoleWriter)); got != (grantOutcome{true, true}) {
		t.Fatalf("a member holding a user grant: %+v, want admitted on both paths", got)
	}

	// The grant is matched in BOTH spellings of the actor's id: an
	// AccessContext.UserId arrives canonical from a JWT and bare from a
	// scaffolding context alike.
	if got := gateOutcomes(t, eng, rankActorCtx(conceptIdentityUser+":"+member, auth.RoleWriter)); got != (grantOutcome{true, true}) {
		t.Fatalf("the same member under the canonical id spelling: %+v, want admitted", got)
	}

	// Revoking writes active:false at the SAME id (a new version, not a
	// deletion), and an inactive grant decides nothing.
	deactivateGrant(t, eng, id)
	if got := gateOutcomes(t, eng, rankActorCtx(member, auth.RoleWriter)); got != (grantOutcome{}) {
		t.Fatalf("a member whose grant was revoked was still admitted: %+v", got)
	}

	// And re-granting is the same row again, not a second one: the newest
	// version is the allow, so the answer flips back.
	if again := writeGrant(t, eng, auth.SubjectKindUser, member, auth.VerbExecute, grantTestResource, auth.GrantAllow); again != id {
		t.Fatalf("re-granting derived a different id %s than the first %s", again, id)
	}
	if got := gateOutcomes(t, eng, rankActorCtx(member, auth.RoleWriter)); got != (grantOutcome{true, true}) {
		t.Fatalf("a re-granted member: %+v, want admitted", got)
	}
}

func TestGrantEnforcementGroupGrantAdmitsMembersOnly(t *testing.T) {
	eng := grantEngine(t)
	suffix := uniqueSuffix("grant-group")
	member := "member-" + suffix
	outsider := "outsider-" + suffix
	group := "g-" + suffix

	// A CUSTOM group with no account: D13 says it can be a grant subject --
	// "it grants apps and still grants no rows".
	seedGroup(t, eng, group, "Grant test group", "custom", "")
	seedMembership(t, eng, group, member, "active")
	writeGrant(t, eng, auth.SubjectKindGroup, group, auth.VerbExecute, grantTestResource, auth.GrantAllow)

	if got := gateOutcomes(t, eng, rankActorCtx(member, auth.RoleWriter)); got != (grantOutcome{true, true}) {
		t.Fatalf("a member of a granted group: %+v, want admitted on both paths", got)
	}
	if got := gateOutcomes(t, eng, rankActorCtx(outsider, auth.RoleWriter)); got != (grantOutcome{}) {
		t.Fatalf("a non-member was admitted by somebody else's group grant: %+v", got)
	}

	// A REMOVED membership is history, and the resolver collapses to the
	// newest version per id before it reads anything.
	seedMembership(t, eng, group, member, "removed")
	if got := gateOutcomes(t, eng, rankActorCtx(member, auth.RoleWriter)); got != (grantOutcome{}) {
		t.Fatalf("a removed member was still admitted through the group: %+v", got)
	}
}

func TestGrantEnforcementGroupDenyOverRoleAllowRefuses(t *testing.T) {
	eng := grantEngine(t)
	suffix := uniqueSuffix("grant-deny")
	barred := "admin-barred-" + suffix
	free := "admin-free-" + suffix
	group := "g-deny-" + suffix

	// Precondition: the role level admits an admin.
	if got := gateOutcomes(t, eng, rankActorCtx(free, auth.RoleAdmin)); got != (grantOutcome{true, true}) {
		t.Fatalf("precondition: an admin holds execute on the resource by role, got %+v", got)
	}

	seedGroup(t, eng, group, "Barred admins", "custom", "")
	seedMembership(t, eng, group, barred, "active")
	writeGrant(t, eng, auth.SubjectKindGroup, group, auth.VerbExecute, grantTestResource, auth.GrantDeny)

	// The group deny NARROWS the role's allow for the member (D1: deny at
	// every level), and only for the member.
	if got := gateOutcomes(t, eng, rankActorCtx(barred, auth.RoleAdmin)); got != (grantOutcome{}) {
		t.Fatalf("an admin in a denied group was admitted: %+v", got)
	}
	if got := gateOutcomes(t, eng, rankActorCtx(free, auth.RoleAdmin)); got != (grantOutcome{true, true}) {
		t.Fatalf("an admin outside the denied group was refused: %+v", got)
	}

	// Most specific wins ACROSS levels (D2): a user allow over the group deny
	// admits the barred admin again.
	writeGrant(t, eng, auth.SubjectKindUser, barred, auth.VerbExecute, grantTestResource, auth.GrantAllow)
	if got := gateOutcomes(t, eng, rankActorCtx(barred, auth.RoleAdmin)); got != (grantOutcome{true, true}) {
		t.Fatalf("a user allow did not beat the group deny: %+v", got)
	}
}

// TestGrantEnforcementBorrowedAuthorityIsNotConsulted: a deny naming the
// person a worker borrows must not stop the worker's own write on that
// person's behalf (memql#4832 D4, carried to grants). Borrowed authority is
// Unranked and resolves as the catalog alone -- which for RoleWriter is
// "nothing", exactly as before this epic.
func TestGrantEnforcementBorrowedAuthorityIsNotConsulted(t *testing.T) {
	eng := grantEngine(t)
	suffix := uniqueSuffix("grant-borrowed")
	person := "person-" + suffix
	writeGrant(t, eng, auth.SubjectKindUser, person, auth.VerbExecute, grantTestResource, auth.GrantAllow)

	borrowed := auth.ContextWithUserActor(context.Background(), person)
	if got := gateOutcomes(t, eng, borrowed); got != (grantOutcome{}) {
		t.Fatalf("borrowed authority was widened by the person's own grant: %+v", got)
	}
}

// TestGrantEnforcementIsHonouredAcrossNodes: a grant written under one
// engine's context is honoured under another's at the next request. Two
// engine contexts over one database, no shared memory between them beyond
// the package-level source seam -- which is installed by the SECOND engine,
// so the first one's write reaches the gate through the rows alone.
func TestGrantEnforcementIsHonouredAcrossNodes(t *testing.T) {
	engA, db, _ := sharedReadMergeEngine(t)
	installGrantTestCatalog(t)

	engB, err := New(db)
	if err != nil {
		t.Fatalf("second engine: %v", err)
	}
	engB.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := engB.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("second engine Init: %v", err)
	}
	engB.InstallGrantResolution()
	t.Cleanup(func() {
		auth.SetGrantSource(nil)
		auth.SetMembershipSource(nil)
	})
	installGrantProbes(t, engB)

	suffix := uniqueSuffix("grant-xnode")
	member := "member-" + suffix
	group := "g-x-" + suffix

	// Everything is WRITTEN on A: the group, the membership, the grant.
	seedGroup(t, engA, group, "Cross-node group", "custom", "")
	seedMembership(t, engA, group, member, "active")
	writeGrant(t, engA, auth.SubjectKindGroup, group, auth.VerbExecute, grantTestResource, auth.GrantAllow)

	// And GATED on B, on a fresh context that carries no memo from anywhere.
	if got := gateOutcomes(t, engB, rankActorCtx(member, auth.RoleWriter)); got != (grantOutcome{true, true}) {
		t.Fatalf("a grant written on node A was not honoured on node B: %+v", got)
	}
}

// TestGrantReadsAnswerForAdminAndRefuseBelowTheFloor is the test
// tierDecidesTheRead (rowauthz_enforce_gate_test.go) names as the one that
// fails if its adjudication of grantsForSubject / grantsForResource is wrong:
// an admin is served the rows through the real tier, and a caller below the
// floor is refused rather than served nothing.
func TestGrantReadsAnswerForAdminAndRefuseBelowTheFloor(t *testing.T) {
	eng := grantEngine(t)
	suffix := uniqueSuffix("grant-reads")
	subject := "subject-" + suffix
	resource := grantTestResource + "/" + suffix
	id := writeGrant(t, eng, auth.SubjectKindUser, subject, auth.VerbRead, resource, auth.GrantAllow)

	for _, q := range []string{
		fmt.Sprintf(`query grantsForSubject(subjectKind: "user", subjectId: %s)`, langparser.QuoteString(subject)),
		fmt.Sprintf(`query grantsForResource(resourceType: %s)`, langparser.QuoteString(resource)),
	} {
		res, err := eng.Execute(rankActorCtx("admin-"+suffix, auth.RoleAdmin), q)
		if err != nil {
			t.Fatalf("%s as admin: %v", q, err)
		}
		found := false
		for _, n := range res.Bundle.GetNodes() {
			if BareShortId(n.GetId()) == id {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s as admin did not return the grant %s", q, id)
		}
		if _, err := eng.Execute(rankActorCtx("writer-"+suffix, auth.RoleWriter), q); err == nil {
			t.Fatalf("%s as writer was served rather than refused at the admin floor", q)
		}
	}
}

// TestGrantEnforcementNegativeControlRoleOnlyResolver is the record's own
// sentence made executable: with the grant source CLEARED -- the role-shaped
// answer and nothing else -- the user-grant case is refused. Every case
// above is only evidence because this one goes the other way.
func TestGrantEnforcementNegativeControlRoleOnlyResolver(t *testing.T) {
	eng := grantEngine(t)
	suffix := uniqueSuffix("grant-control")
	member := "member-" + suffix
	writeGrant(t, eng, auth.SubjectKindUser, member, auth.VerbExecute, grantTestResource, auth.GrantAllow)

	// Positive control first: with the source installed the grant admits.
	if got := gateOutcomes(t, eng, rankActorCtx(member, auth.RoleWriter)); got != (grantOutcome{true, true}) {
		t.Fatalf("positive control: a granted member was refused with the source installed: %+v", got)
	}

	auth.SetGrantSource(nil)
	if got := gateOutcomes(t, eng, rankActorCtx(member, auth.RoleWriter)); got != (grantOutcome{}) {
		t.Fatalf("with no grant source the gates still admitted a member by grant: %+v -- "+
			"something other than CapableFor is answering", got)
	}
}

// TestSingleStatementLogicClearsItsFloors pins the hole the capability probe
// exposed (epic memql#5296). A single-statement logic -- `return cond(...)`
// -- never hoists to plan.LogicCall (that needs LogicSteps); it expands to a
// literal at plan.Root and returned from one of executeWith's seven early
// branches BEFORE the plan-level rank and capability gates ran. So a
// `@requiresRank` or `@requiresCapability` on it was recorded on the plan and
// read by nothing, while the same annotation on a multi-step logic or a query
// was enforced. Both gates now run directly after the parse; this asserts
// both halves for both annotations.
func TestSingleStatementLogicClearsItsFloors(t *testing.T) {
	eng := grantEngine(t)
	suffix := uniqueSuffix("floors")

	// The capability half: gateOutcomes' "direct" path IS a single-statement
	// logic, so the member-refused / admin-admitted pair is the assertion.
	if got := gateOutcomes(t, eng, rankActorCtx("member-"+suffix, auth.RoleWriter)); got.direct {
		t.Fatal("a single-statement logic carrying @requiresCapability admitted a member holding nothing")
	}
	if got := gateOutcomes(t, eng, rankActorCtx("admin-"+suffix, auth.RoleAdmin)); !got.direct {
		t.Fatal("a single-statement logic carrying @requiresCapability refused an admin holding the grant")
	}

	// The rank half.
	call := "logic " + rankProbeLogic + `(probe: "x")`
	_, err := eng.Execute(rankActorCtx("member-"+suffix, auth.RoleWriter), call)
	if err == nil || !strings.Contains(err.Error(), `requires the "admin" role or above`) {
		t.Fatalf("a single-statement logic carrying @requiresRank(\"admin\") did not refuse a writer: %v", err)
	}
	if _, err := eng.Execute(rankActorCtx("admin-"+suffix, auth.RoleAdmin), call); err != nil {
		t.Fatalf("a single-statement logic carrying @requiresRank(\"admin\") refused an admin: %v", err)
	}
}

// TestEffectiveSetReportsAGrantWithItsProvenanceUntilRevoked (epic
// memql#5298): through the rows, a user grant appears in the caller's
// effective set with provenance `user`, and after the revoke it does not --
// the pair falls back to the role's answer.
func TestEffectiveSetReportsAGrantWithItsProvenanceUntilRevoked(t *testing.T) {
	eng := grantEngine(t)
	suffix := uniqueSuffix("grant-effective")
	member := "member-" + suffix

	find := func(ctx context.Context) (auth.Decision, bool) {
		subject, ok := eng.subjectFor(ctx)
		if !ok {
			t.Fatal("no subject for the member's context")
		}
		for _, d := range auth.EffectiveCapabilities(ctx, subject) {
			if d.Verb == auth.VerbExecute && d.Resource == grantTestResource {
				return d, true
			}
		}
		return auth.Decision{}, false
	}

	// decide asks the single-pair form, which answers whether or not the pair
	// is in the universe: the catalog fake cannot enumerate its slugs, so a
	// pair nothing grants is absent from the SET here (the graph catalog lists
	// them, and the pure test covers that row), while the DECISION for it is
	// always "not held, from the role".
	decide := func(ctx context.Context) auth.Decision {
		subject, _ := eng.subjectFor(ctx)
		return auth.DecideFor(ctx, subject, auth.VerbExecute, grantTestResource)
	}

	// Before: nothing names the pair for this member.
	if d := decide(rankActorCtx(member, auth.RoleWriter)); d.Held || d.Source != auth.SourceRole {
		t.Fatalf("before any grant: %+v, want not held from role", d)
	}
	if d, present := find(rankActorCtx(member, auth.RoleWriter)); present && d.Held {
		t.Fatalf("before any grant the set reported the pair held: %+v", d)
	}

	id := writeGrant(t, eng, auth.SubjectKindUser, member, auth.VerbExecute, grantTestResource, auth.GrantAllow)
	if d, present := find(rankActorCtx(member, auth.RoleWriter)); !present || !d.Held || d.Source != auth.SourceUser {
		t.Fatalf("with a user grant: %+v present=%v, want held from user", d, present)
	}

	deactivateGrant(t, eng, id)
	if d := decide(rankActorCtx(member, auth.RoleWriter)); d.Held || d.Source != auth.SourceRole {
		t.Fatalf("after the revoke: %+v, want not held from role again", d)
	}
	if d, present := find(rankActorCtx(member, auth.RoleWriter)); present && d.Held {
		t.Fatalf("after the revoke the set still reported the pair held: %+v", d)
	}

	// And grantById, the read grantRevoke acts on, answers the newest version
	// -- the revocation -- to an admin, and refuses a writer.
	q := fmt.Sprintf(`query grantById(grantId: %s)`, langparser.QuoteString(id))
	res, err := eng.Execute(rankActorCtx("admin-"+suffix, auth.RoleAdmin), q)
	if err != nil {
		t.Fatalf("grantById as admin: %v", err)
	}
	nodes := res.Bundle.GetNodes()
	if len(nodes) != 1 || BareShortId(nodes[0].GetId()) != id {
		t.Fatalf("grantById as admin returned %d nodes", len(nodes))
	}
	if active := nodes[0].GetPayload().GetFields()["active"]; active == nil || active.GetBoolValue() {
		t.Fatal("grantById answered the pre-revoke version rather than the newest")
	}
	if _, err := eng.Execute(rankActorCtx("writer-"+suffix, auth.RoleWriter), q); err == nil {
		t.Fatal("grantById as writer was served rather than refused at the admin floor")
	}
}
