package work

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// PauseReplanForBudget preserves the suspended remedy in the approval itself.
// A decision on another replica must resume planning, not the old failed step.
// The original cause is separate from the budget breach and all usage survives.
func (i *Integration) PauseReplanForBudget(ctx context.Context, ownerUserId, runId, stepKey string, ceiling *memql.RunCeilingError) error {
	if ceiling == nil || memql.BareShortId(ceiling.RunId) != memql.BareShortId(runId) {
		return fmt.Errorf("work: replan budget refusal must belong to this run")
	}
	run, err := i.remedyRun(ctx, ownerUserId, runId, waitKindReplan)
	if err != nil {
		return err
	}
	wait := rowMap(run, "waitingOn")
	act := actRun{row: run, id: runId, owner: ownerUserId, order: topLevelOrder(rowStringSlice(run, "stepOrder"))}
	if stepKey != rowString(wait, "subject") || act.requireExecutable() != nil || act.requireTopLevel(stepKey) != nil {
		return fmt.Errorf("work: replan budget refusal must name the pending executable step")
	}
	now := i.clock().UTC()
	breach := ceiling.Breach
	req := work.BudgetApproval(runId, stepKey, work.CeilingBreach{
		Ceiling: breach.Ceiling, Limit: breach.Limit, Actual: breach.Actual, Reason: breach.Reason,
	}, now, DefaultApprovalTTL)
	req.Subject["resumeKind"] = waitKindReplan
	req.Subject["resumeReason"] = rowString(wait, "reason")
	req.ArtifactHash = work.ArtifactHash(req.Subject)
	approvalId, err := i.RaiseApproval(ctx, ownerUserId, ApprovalSeed{
		RunId: runId, StepKey: stepKey, Kind: req.Kind, Subject: req.Subject, ArtifactHash: req.ArtifactHash,
		Question: "This run needs a higher resource limit to revise its plan. Raise the limit to continue, or stop this run?",
		Evidence: evidenceMap(req.Evidence), RequestedAt: now, ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		return err // Never park on an approval that was not saved.
	}
	spent := map[string]any{}
	for key, value := range rowMap(run, "spent") {
		spent[key] = value
	}
	spent["modelCalls"] = ceiling.Spent.ModelCalls
	spent["tokens"] = ceiling.Spent.Tokens
	spent["tokensSubscription"] = ceiling.Spent.TokensSubscription
	spent["tokensLocal"] = ceiling.Spent.TokensLocal
	spent["cost"] = ceiling.Spent.Cost
	spent["wallClockMs"] = ceiling.Spent.WallClockMs
	return i.store().updateRun(ownerActor(ctx, ownerUserId), runId, map[string]any{
		"spent": spent,
		"waitingOn": map[string]any{"kind": "approval", "subject": approvalId, "approvalKind": req.Kind,
			"ceiling": breach.Ceiling, "reason": breach.Reason, "since": rfc(now)},
	})
}
