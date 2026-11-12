package work

// routing_review_db_test.go -- the nightly routing review, seen and decided
// by a person, against a REAL engine and Postgres (design record
// docs/superpowers/specs/2026-11-08-routing-review-apply-design.md).
//
// A fake engine cannot show any of what matters here: that the fold's write
// under the maintenance principal lands with no owner, that the cluster read
// admits it under the system actor, that the builtin result survives the
// engine's row gate for a developer, and that an approved rule reaches the
// routing configuration another replica reads.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	routerlib "github.com/znasllc-io/memql/component/router"
	work "github.com/znasllc-io/memql/component/work"
)

// uniqueModel is a model id no other run of these tests has used. The
// database is shared, and a review's id derives from its rule text.
func uniqueModel(t *testing.T) string {
	t.Helper()
	return "dbtest-" + strings.NewReplacer(".", "", ":", "").Replace(time.Now().UTC().Format("20060102150405.000000000")) + ":9b"
}

// evidenceProposal renders a proposal the way the fold does: the name from
// component/router.RuleName, the text in component/routingrules.GenerateRule's
// exact shape (this module cannot import that package), the hash over it.
func evidenceProposal(direction, model, level string) routerlib.Proposal {
	name := routerlib.RuleName(direction, model, level)
	source := "/// Generated from 2026-W45 evidence: 17 of 41 structured calls to " + model + " at level " + level + " failed.\n" +
		"@when(level=\"" + level + "\")\n" +
		"@level(\"" + level + "\")\n" +
		"@policy(\"localFirst\")\n" +
		"@precedence(50)\n" +
		"@onUnavailable(\"degrade\")\n"
	if direction == routerlib.DirectionDemotion {
		source += "@exclude(\"fleet:" + model + "\")\n"
	}
	source += "rule " + name + " { }\n"
	return routerlib.Proposal{ModelId: model, Level: level, Week: "2026-W45", Direction: direction,
		RuleName: name, RuleSource: source, Hash: routerlib.ProposalHash(source), Reason: "17 of 41 structured calls failed."}
}

// raiseRoutingReview writes the approval exactly as the fold does
// (integrations/router's openRoutingReview): createWorkApproval as the fold's
// maintenance principal, with the claims and token the automation executor
// stamps, at internal origin. Returns the approval id.
func raiseRoutingReview(t *testing.T, eng *memqlengine.MemQLEngine, p routerlib.Proposal) string {
	t.Helper()
	return writeRoutingReview(t, eng, p, auth.MaintenanceActor(auth.AutomationOriginEngine, "routingEvidenceFold"))
}

// raiseLegacyRoutingReview writes the review as the fold did before memql#4817:
// the same identity, but not marked synthetic, so the row keeps the
// `system:maintenance:` owner the engine now blanks.
func raiseLegacyRoutingReview(t *testing.T, eng *memqlengine.MemQLEngine, p routerlib.Proposal) string {
	t.Helper()
	return writeRoutingReview(t, eng, p, &auth.AccessContext{UserId: "system:maintenance:routingEvidenceFold", Role: auth.RoleOwner, Unranked: true})
}

func writeRoutingReview(t *testing.T, eng *memqlengine.MemQLEngine, p routerlib.Proposal, actor *auth.AccessContext) string {
	t.Helper()
	approval := work.RoutingReviewApproval(work.RoutingProposal{
		ModelId: p.ModelId, Level: p.Level, Week: p.Week, Direction: p.Direction, RuleName: p.RuleName,
		RuleSource: p.RuleSource, Reason: p.Reason, Calls: 41, StructuredFailures: 17,
	}, time.Now().UTC(), 14*24*time.Hour)
	require.NoError(t, work.ValidateApprovalKind(approval))
	id := "v1:work:approval:" + p.Hash[:32]
	claims := map[string]any{"sub": actor.UserId, "email": actor.UserId, "role": string(actor.Role)}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	ctx = auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, actor))
	_, err := eng.Execute(ctx, "mutation "+call("createWorkApproval", map[string]any{
		"approvalId": id, "runId": "", "stepKey": "", "kind": approval.Kind, "subject": approval.Subject,
		"artifactHash": approval.ArtifactHash, "question": approval.Question, "options": approval.Options,
		"evidence":    map[string]any{"tier": approval.Evidence.Tier, "reason": approval.Evidence.Reason, "source": approval.Evidence.Source},
		"requestedAt": rfc(approval.RequestedAt), "expiresAt": rfc(approval.ExpiresAt),
	}))
	require.NoError(t, err)
	return id
}

// personCtx is a signed-in person holding role.
func personCtx(userId string, role auth.Role) context.Context {
	return auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), userId), &auth.AccessContext{UserId: userId, Role: role})
}

// routingReviewIntegration is the work integration over a real engine, wired
// the way the plug-in factory wires it.
func routingReviewIntegration(t *testing.T, eng *memqlengine.MemQLEngine) *Integration {
	t.Helper()
	i := New(eng, testLogger())
	i.SetGateDB(testGates(t))
	i.admitRow = memqlengine.AdmitSourceRow
	return i
}

// listedReviews runs workRoutingReviews through the ENGINE, so the engine's
// row gate on builtin results applies exactly as it does for MemQL OS.
func listedReviews(t *testing.T, eng *memqlengine.MemQLEngine, ctx context.Context, approvalId string) map[string]map[string]any {
	t.Helper()
	args := map[string]any{}
	if approvalId != "" {
		args["approvalId"] = approvalId
	}
	res, err := eng.Execute(memqlengine.ContextWithFreshRead(ctx), "builtin "+call("workRoutingReviews", args))
	require.NoError(t, err)
	out := map[string]map[string]any{}
	for _, row := range memqlengine.MaterializeRows(res) {
		out[memqlengine.BareShortId(rowString(row, "id"))] = row
	}
	return out
}

func TestRoutingReviewsDBAreTheirDecidersOnly(t *testing.T) {
	eng := openWorkTestEngine(t)
	require.NoError(t, eng.RegisterIntegration(routingReviewIntegration(t, eng)))
	model := uniqueModel(t)
	current := raiseRoutingReview(t, eng, evidenceProposal(routerlib.DirectionDemotion, model, "strong"))
	legacy := raiseLegacyRoutingReview(t, eng, evidenceProposal(routerlib.DirectionDemotion, model+"-legacy", "strong"))

	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper} {
		listed := listedReviews(t, eng, personCtx("dbtest-"+string(role), role), "")
		require.Contains(t, listed, memqlengine.BareShortId(current), "%s must see the review the fold raised", role)
		require.Contains(t, listed, memqlengine.BareShortId(legacy), "%s must see a review raised before the owner undo", role)
		require.Equal(t, "routingReview", rowString(listed[memqlengine.BareShortId(current)], "kind"))
	}
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleAdmin} {
		listed := listedReviews(t, eng, personCtx("dbtest-"+string(role), role), "")
		require.Empty(t, listed, "%s may not change routing, so it has no routing review to decide", role)
	}

	one := listedReviews(t, eng, personCtx("dbtest-developer", auth.RoleDeveloper), current)
	require.Len(t, one, 1)
	require.Contains(t, one, memqlengine.BareShortId(current))
}

// dbtestRulePrefixes are the saved rules these tests make: every demotion
// they approve is named for a dbtest- model, and the one rule of a person's
// own is named here. The database is shared, so each test sweeps them before
// it starts and when it ends, including what an earlier failed run left.
var dbtestRulePrefixes = []string{"evidenceDemotionStrongDbtest", "dbtestOwnRule"}

func cleanRoutingReviewRules(t *testing.T, eng *memqlengine.MemQLEngine) {
	t.Helper()
	sweep := func() {
		ctx := personCtx("dbtest-cleanup", auth.RoleOwner)
		_, rules, err := eng.Policies().SnapshotRouting(ctx, eng.Rules())
		require.NoError(t, err)
		for _, rule := range rules.All() {
			for _, prefix := range dbtestRulePrefixes {
				if !rule.Locked && strings.HasPrefix(rule.Name, prefix) {
					_, err := eng.ApplyRoutingReview(ctx, memqlengine.RoutingReviewChange{RetireName: rule.Name})
					require.NoError(t, err)
				}
			}
		}
	}
	sweep()
	t.Cleanup(sweep)
}

// routingRevision is the routing configuration's current revision, as the
// routingRules builtin reports it to anyone signed in.
func routingRevision(t *testing.T, eng *memqlengine.MemQLEngine) int64 {
	t.Helper()
	res, err := eng.Execute(memqlengine.ContextWithFreshRead(personCtx("dbtest-reader", auth.RoleWriter)), "builtin routingRules()")
	require.NoError(t, err)
	rows := memqlengine.MaterializeRows(res)
	require.NotEmpty(t, rows, "the rule catalog always holds the shipped rules")
	revision, ok := rows[0]["revision"].(float64)
	if !ok {
		n, isInt := rows[0]["revision"].(int64)
		require.True(t, isInt, "revision %T", rows[0]["revision"])
		return n
	}
	return int64(revision)
}

// savedRule is the rule another replica would route by, if any.
func savedRule(t *testing.T, eng *memqlengine.MemQLEngine, name string) (*memqlengine.RuleConfig, bool) {
	t.Helper()
	_, rules, err := eng.Policies().SnapshotRouting(personCtx("dbtest-reader", auth.RoleWriter), eng.Rules())
	require.NoError(t, err)
	return rules.Lookup(name)
}

func decide(t *testing.T, i *Integration, ctx context.Context, approvalId, decision string, answer map[string]any) (map[string]any, error) {
	t.Helper()
	args := map[string]any{"approvalId": approvalId, "decision": decision}
	if answer != nil {
		args["answer"] = answer
	}
	nodes, err := i.handleDecideApproval(ctx, args, 0)
	if err != nil {
		return nil, err
	}
	return decodeReply(t, nodes), nil
}

// routingReviewFixture is one engine with the work integration registered,
// so workRoutingReviews runs through the engine as MemQL OS calls it.
func routingReviewFixture(t *testing.T) (*memqlengine.MemQLEngine, *Integration) {
	t.Helper()
	eng := openWorkTestEngine(t)
	i := routingReviewIntegration(t, eng)
	require.NoError(t, eng.RegisterIntegration(i))
	cleanRoutingReviewRules(t, eng)
	return eng, i
}

func TestRoutingReviewDBDemotionIsSavedAndSeenByAPeer(t *testing.T) {
	eng, i := routingReviewFixture(t)
	peer := openWorkTestEngine(t)
	model := uniqueModel(t)
	p := evidenceProposal(routerlib.DirectionDemotion, model, "strong")
	id := raiseRoutingReview(t, eng, p)
	before := routingRevision(t, eng)

	developer := personCtx("dbtest-developer-"+model, auth.RoleDeveloper)
	reply, err := decide(t, i, developer, id, "approved", nil)
	require.NoError(t, err)
	require.Equal(t, "approved", reply["decision"])
	require.Equal(t, false, reply["runResumed"])
	require.Equal(t, p.RuleName, reply["routingRule"])
	require.Equal(t, true, reply["routingChanged"])
	require.EqualValues(t, before+1, reply["routingRevision"])

	// Another replica, with its own registries, reads the same document on
	// its next call -- which is what "every replica routes by it" means.
	saved, ok := savedRule(t, peer, p.RuleName)
	require.True(t, ok, "the peer's SnapshotRouting must hold the approved rule")
	require.Equal(t, []string{"fleet:" + model}, saved.Excludes)
	require.Equal(t, 50, saved.Precedence)
	require.Equal(t, "strong", saved.When.Level)

	row := listedReviews(t, eng, developer, id)[memqlengine.BareShortId(id)]
	require.Equal(t, "approved", rowString(row, "decision"))
	require.Contains(t, rowString(row, "decidedBy"), "dbtest-developer-")

	// A second person deciding the same review is answered with the recorded
	// decision, and nothing is applied twice.
	again, err := decide(t, i, personCtx("dbtest-owner-"+model, auth.RoleOwner), id, "approved", nil)
	require.NoError(t, err)
	require.Equal(t, "approved", again["decision"])
	require.NotContains(t, again, "routingRevision")
	require.Equal(t, before+1, routingRevision(t, eng))
}

func TestRoutingReviewDBSecondDemotionAtTheLevelIsRefused(t *testing.T) {
	eng, i := routingReviewFixture(t)
	model := uniqueModel(t)
	developer := personCtx("dbtest-developer-"+model, auth.RoleDeveloper)
	first := evidenceProposal(routerlib.DirectionDemotion, model+"-a", "strong")
	_, err := decide(t, i, developer, raiseRoutingReview(t, eng, first), "approved", nil)
	require.NoError(t, err)

	second := raiseRoutingReview(t, eng, evidenceProposal(routerlib.DirectionDemotion, model+"-b", "strong"))
	before := routingRevision(t, eng)
	_, err = decide(t, i, developer, second, "approved", nil)
	require.ErrorContains(t, err, first.RuleName, "the refusal must name the saved demotion in the way")
	require.ErrorContains(t, err, "Fleet > Routing > Rules")
	require.Contains(t, listedReviews(t, eng, developer, ""), memqlengine.BareShortId(second), "a refused approval records nothing")
	require.Equal(t, before, routingRevision(t, eng))

	// Declining it still works, and changes no routing.
	reply, err := decide(t, i, developer, second, "rejected", nil)
	require.NoError(t, err)
	require.Equal(t, "rejected", reply["decision"])
	require.Equal(t, before, routingRevision(t, eng))

	// The same refusal for a rule a person saved by hand at precedence 50.
	_, err = eng.ApplyRoutingReview(developer, memqlengine.RoutingReviewChange{RetireName: first.RuleName})
	require.NoError(t, err)
	_, err = eng.Execute(developer, "builtin "+call("routingRuleSave", map[string]any{
		"name": "dbtestOwnRule50", "policy": "localFirst", "precedence": 50,
		"expectedRevision": routingRevision(t, eng), "conditions": map[string]any{"tag": "dbtest"},
	}))
	require.NoError(t, err)
	third := raiseRoutingReview(t, eng, evidenceProposal(routerlib.DirectionDemotion, model+"-c", "strong"))
	_, err = decide(t, i, developer, third, "approved", nil)
	require.ErrorContains(t, err, "dbtestOwnRule50")
}

func TestRoutingReviewDBPromotionRetiresTheDemotion(t *testing.T) {
	eng, i := routingReviewFixture(t)
	peer := openWorkTestEngine(t)
	model := uniqueModel(t)
	developer := personCtx("dbtest-developer-"+model, auth.RoleDeveloper)
	demotion := evidenceProposal(routerlib.DirectionDemotion, model, "strong")
	_, err := decide(t, i, developer, raiseRoutingReview(t, eng, demotion), "approved", nil)
	require.NoError(t, err)
	_, ok := savedRule(t, peer, demotion.RuleName)
	require.True(t, ok)

	before := routingRevision(t, eng)
	promotion := raiseRoutingReview(t, eng, evidenceProposal(routerlib.DirectionPromotion, model, "strong"))
	reply, err := decide(t, i, developer, promotion, "approved", nil)
	require.NoError(t, err)
	require.Equal(t, demotion.RuleName, reply["routingRule"], "a promotion retires the demotion, not a rule of its own")
	require.Equal(t, true, reply["routingChanged"])
	require.EqualValues(t, before+1, reply["routingRevision"])
	_, ok = savedRule(t, peer, demotion.RuleName)
	require.False(t, ok, "the peer must no longer route by the retired demotion")
}

func TestRoutingReviewDBWriterIsRefused(t *testing.T) {
	eng, i := routingReviewFixture(t)
	model := uniqueModel(t)
	id := raiseRoutingReview(t, eng, evidenceProposal(routerlib.DirectionDemotion, model, "strong"))
	before := routingRevision(t, eng)
	_, err := decide(t, i, personCtx("dbtest-writer-"+model, auth.RoleWriter), id, "approved", nil)
	require.ErrorContains(t, err, "no pending approval", "a writer is told what it is told about any approval that is not theirs")
	require.Equal(t, before, routingRevision(t, eng))
	require.Contains(t, listedReviews(t, eng, personCtx("dbtest-owner-"+model, auth.RoleOwner), ""), memqlengine.BareShortId(id))
}

func TestRoutingReviewDBRejectChangesNoRouting(t *testing.T) {
	eng, i := routingReviewFixture(t)
	model := uniqueModel(t)
	p := evidenceProposal(routerlib.DirectionDemotion, model, "strong")
	id := raiseRoutingReview(t, eng, p)
	before := routingRevision(t, eng)
	owner := personCtx("dbtest-owner-"+model, auth.RoleOwner)
	reply, err := decide(t, i, owner, id, "rejected", nil)
	require.NoError(t, err)
	require.Equal(t, "rejected", reply["decision"])
	require.NotContains(t, reply, "routingRevision")
	require.Equal(t, before, routingRevision(t, eng))
	_, ok := savedRule(t, eng, p.RuleName)
	require.False(t, ok)
	require.Equal(t, "rejected", rowString(listedReviews(t, eng, owner, id)[memqlengine.BareShortId(id)], "decision"))
}

func TestRoutingReviewDBAnsweredIsRefused(t *testing.T) {
	eng, i := routingReviewFixture(t)
	model := uniqueModel(t)
	id := raiseRoutingReview(t, eng, evidenceProposal(routerlib.DirectionDemotion, model, "strong"))
	developer := personCtx("dbtest-developer-"+model, auth.RoleDeveloper)
	_, err := decide(t, i, developer, id, "answered", map[string]any{"value": "approved"})
	require.ErrorContains(t, err, "approved or rejected", "a routing review takes a decision, not an answer")
	require.Contains(t, listedReviews(t, eng, developer, ""), memqlengine.BareShortId(id), "a refused answer records nothing")
}
