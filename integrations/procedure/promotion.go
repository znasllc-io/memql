package procedure

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/id"
)

// promotion.go -- the ONE human approval a learned procedure asks for (epic
// memql#5408, D3; plan Task 5 steps 2.10 and 6).
//
// The ladder proposes; a person promotes once; this file does the two halves
// that touch rows. RAISING: when a shadow comparison's transition proposes, a
// v1:work:approval of kind procedurePromotion is written under the owner's
// borrowed actor (component/work.ProcedurePromotionApproval builds it, its
// artifact hash the construct's procedureHash) and its id rides the SAME
// ladder write, so the ladder -- which never proposes while an approval is
// open -- cannot propose twice. APPLYING: when the approval is decided, the
// onProcedurePromotionDecided automation calls DecidePromotion, which moves
// shadow to canary on a yes and spends the streak on a no.
//
// RAISING WRITES createWorkApproval ITSELF, not through integrations/work's
// RaiseApproval, which the plan named. RaiseApproval is a method on the work
// plug-in's REGISTERED INSTANCE, and plug-ins are constructed independently:
// this one holds no handle to it, and app/ wiring for one mutation would couple
// the two plug-ins' construction for nothing it adds. The row is built by the
// same component/work.ProcedurePromotionApproval, checked by the same
// ValidateApprovalKind, and written under the owner's actor through this
// package's one internal-origin stamp (store.go) -- exactly the row
// RaiseApproval would write. Its id is this package's to derive
// (promotionApprovalId), so a proposal delivered twice writes one row.
//
// Deciding it is integrations/work's (handleDecideApproval): only the owner
// decides, a changed construct refuses with the artifact-changed error, and
// the shadow run the approval names is never touched. Nothing here decides
// anything a person did not.

// raisePromotion writes the procedurePromotion approval a transition
// proposed, under the owner's actor and the one stamp, and returns its id.
// dryEvidence says the comparison that proposed it held calls to the app's
// own rather than replaying them -- every comparison of a machine-local
// procedure, and any whose step may send something out of the sandbox -- so
// the person deciding can see what the streak did and did not exercise.
func (i *Integration) raisePromotion(ctx context.Context, owner string, c *loaded, shadowRunId string, t work.Transition, dryEvidence bool) (string, error) {
	distinct := make(map[string]int, len(t.State.DistinctBindings))
	for holeId, digests := range t.State.DistinctBindings {
		distinct[holeId] = len(digests)
	}
	rf := c.p.RecordedFrom
	recordedFrom := map[string]any{
		"sessionIds": append([]string{}, rf.SessionIds...),
		"runIds":     append([]string{}, rf.RunIds...),
	}
	for k, v := range map[string]string{"app": rf.App, "model": rf.Model, "effort": rf.Effort} {
		if strings.TrimSpace(v) != "" {
			recordedFrom[k] = v
		}
	}
	req := work.ProcedurePromotionApproval(work.PromotionProposal{
		OwnerUserId:      owner,
		ConstructId:      c.id,
		ConstructName:    c.name,
		ProcedureHash:    c.hash,
		ShadowRunId:      shadowRunId,
		ShadowMatches:    t.State.ShadowMatches,
		DistinctBindings: distinct,
		RecordedFrom:     recordedFrom,
		Title:            c.p.Title,
		// TODO(coordinator): component/work.PromotionProposal gains Target
		// and DryEvidence on the epic branch (F2). Once this branch sits on
		// it, name where the procedure runs and whether its evidence was dry:
		//	Target:      string(procedureTarget(c.p)),
		//	DryEvidence: dryEvidence,
	}, i.clock().UTC())
	if err := work.ValidateApprovalKind(req); err != nil {
		return "", err
	}
	approvalId := promotionApprovalId(c.id, c.hash, shadowRunId)
	if err := i.store.writeInternal(ownerActor(ctx, owner), "mutation "+call("createWorkApproval", map[string]any{
		"approvalId":   approvalId,
		"runId":        req.RunId,
		"kind":         req.Kind,
		"subject":      req.Subject,
		"artifactHash": req.ArtifactHash,
		"question":     req.Question,
		"options":      req.Options,
		"evidence": map[string]any{
			"tier": req.Evidence.Tier, "reason": req.Evidence.Reason,
			"ruleId": req.Evidence.RuleId, "source": req.Evidence.Source,
		},
		"requestedAt": req.RequestedAt.UTC().Format(timeLayout),
	})); err != nil {
		return "", fmt.Errorf("raise the promotion approval: %w", err)
	}
	i.log().Info("procedure: proposed a promotion to canary",
		"constructId", c.id, "approvalId", approvalId, "shadowRunId", shadowRunId, "shadowMatches", t.State.ShadowMatches)
	return approvalId, nil
}

// promotionApprovalId is a procedurePromotion approval's id: DERIVED from
// the construct, the version it pins and the shadow run whose comparison
// proposed it (review finding C1). A proposal delivered twice -- a comparison
// finished by two executions, a retry after a write whose reply was lost --
// writes the SAME row rather than a second card for one version; any of the
// three differing is a different proposal and a different row. Bare and
// canonical spellings of one construct or run derive one id.
func promotionApprovalId(constructId, procedureHash, shadowRunId string) string {
	return "v1:work:approval:" + string(id.New().MustFromMap(map[string]any{
		"kind":      work.ApprovalKindProcedurePromotion,
		"construct": memql.BareShortId(strings.TrimSpace(constructId)),
		"version":   strings.TrimSpace(procedureHash),
		"shadowRun": memql.BareShortId(strings.TrimSpace(shadowRunId)),
	}))
}

// DecidePromotion applies a decided procedurePromotion approval to the ladder
// -- the handler behind onProcedurePromotionDecided, run by the cluster's
// maintenance principal (the approval names its construct before anybody
// knows whose it is).
//
// IT APPLIES A DECISION, IT NEVER MAKES ONE, and it is IDEMPOTENT: it acts
// only when the construct is still in shadow and still waiting on THIS
// approval -- whose id rides in the ladder state Advance reads -- and, for a
// yes, only when the construct is still the version the person approved. A
// second delivery finds the approval id cleared and changes nothing; a
// decision on an approval a re-lift superseded changes nothing either.
func (i *Integration) DecidePromotion(ctx context.Context, approvalId string) (work.Transition, error) {
	approvalId = strings.TrimSpace(approvalId)
	if approvalId == "" {
		return work.Transition{}, fmt.Errorf("procedure.decidePromotion: approvalId is required")
	}
	if ac, _ := auth.AccessFromContext(ctx); !isClusterPrincipal(ac) {
		return work.Transition{}, fmt.Errorf("procedure.decidePromotion: a promotion decision is applied by the cluster when the approval is decided; " +
			"a person decides it in Approvals, and this call is the onProcedurePromotionDecided automation's")
	}
	approval, err := i.store.approvalByIdAsCluster(ctx, approvalId)
	if err != nil {
		return work.Transition{}, err
	}
	noop := func(reason string) (work.Transition, error) {
		i.log().Debug("procedure: a promotion decision changed nothing", "approvalId", approvalId, "reason", reason)
		return work.Transition{Reason: reason}, nil
	}
	if approval == nil {
		return noop("the approval is not readable")
	}
	if str(approval, "kind") != work.ApprovalKindProcedurePromotion {
		return noop("the approval is not a procedure promotion")
	}
	decision := str(approval, "decision")
	if decision != "approved" && decision != "rejected" {
		return noop("the approval has not been decided")
	}
	owner := strings.TrimSpace(str(approval, "ownerUserId"))
	constructId := strings.TrimSpace(str(obj(approval, "subject"), "constructId"))
	if owner == "" || constructId == "" {
		return noop("the approval names no owner or no construct")
	}
	// THE DECISION IS APPLIED TO THE CONSTRUCT AS IT IS NOW, under its ladder
	// lock (ladder.go): a comparison finishing while this runs must not write
	// its stale `shadow` back over the canary this moves to, and this must not
	// promote a streak a concurrent mismatch just spent.
	row, release, err := i.lockedConstruct(ctx, owner, constructId)
	if err != nil {
		if errors.Is(err, errConstructUnreadable) {
			return noop("the construct is not readable as its owner")
		}
		// Not a no-op: the person decided, and a decision dropped here would
		// leave the construct waiting on an approval nobody will decide
		// again. Failing lets the automation say so.
		return work.Transition{}, fmt.Errorf("procedure.decidePromotion: %w", err)
	}
	defer release()
	state := ladderStateOf(row)
	if memql.BareShortId(state.PromotionApprovalId) != memql.BareShortId(approvalId) {
		return noop("the construct is not waiting on this approval")
	}
	if state.Rung != work.RungShadow {
		return noop("the construct is no longer in shadow")
	}
	if decision == "approved" && str(approval, "artifactHash") != str(row, "procedureHash") {
		// The decide handler refuses an approval of a changed construct;
		// this is the same rule, held again where the ladder moves.
		return noop("the approval pinned a version the construct no longer is")
	}
	t := work.Advance(state, work.LadderEvent{
		Kind: work.EventPromotionDecided, At: i.clock().UTC(), Approved: decision == "approved",
	}, i.readPolicy(ctx, owner))
	if err := i.writeLadder(ctx, owner, str(row, "id"), t); err != nil {
		return t, err
	}
	i.log().Info("procedure: applied a promotion decision",
		"constructId", constructId, "approvalId", approvalId, "decision", decision, "from", string(t.From), "to", string(t.To))
	return t, nil
}

func (i *Integration) handleDecidePromotion(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	approvalId := strings.TrimSpace(argString(args, "approvalId"))
	t, err := i.DecidePromotion(ctx, approvalId)
	if err != nil {
		return nil, err
	}
	// A no-op answers the zero transition with its reason; an applied
	// decision always moves FROM shadow.
	return i.reply(map[string]any{
		"approvalId": approvalId,
		"applied":    t.From != work.RungNone,
		"from":       string(t.From),
		"to":         string(t.To),
		"reason":     t.Reason,
	}), nil
}
