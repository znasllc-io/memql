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
	return writeRoutingReview(t, eng, p, auth.MaintenanceActor("routingEvidenceFold"))
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
