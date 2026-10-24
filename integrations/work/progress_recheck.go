package work

import (
	"context"
	"encoding/json"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
)

// A replica with the retired lifetime policy can save a hard call/time stop
// against a window another replica already renewed. The historical question
// remains immutable. A person's guidance may reconsider it only when the fresh
// owned run's EXISTING window covers its saved spend; this neither renews the
// window nor admits inference. The ordinary native admission path reconciles
// current receipts and still enforces explicit provider and monetary caps.
func (i *Integration) progressStopWithinCurrentWindow(ctx context.Context, approval, run map[string]any) bool {
	subject := rowMap(approval, "subject")
	ceiling := rowString(subject, "ceiling")
	if rowString(approval, "kind") != workstate.ApprovalKindFeedback || !workstate.IsProgressFeedback(subject) ||
		subject["continuationAllowed"] != false || rowString(subject, "reviewType") != "" ||
		rowString(subject, "resumeKind") != waitKindReplan || rowString(subject, "resumeReason") == "" ||
		(ceiling != workstate.CeilingModelCalls && ceiling != workstate.CeilingWallClock) ||
		workstate.ArtifactHash(subject) != rowString(approval, "artifactHash") {
		return false
	}
	state := rowMap(rowMap(run, "classification"), "progressContinuation")
	if rowInt(state, "policyVersion") < 2 || rowInt(state, "extensions") < 1 {
		return false // Never infer an existing renewal from an unversioned estimate.
	}
	goal, err := i.store().goalForOwner(memql.ContextWithFreshRead(ctx), rowString(run, "goalId"))
	if err != nil || goal == nil || rowString(goal, "status") == "closed" {
		return false
	}
	declared, err := ceilingsOf(goal)
	if err != nil {
		return false
	}
	limits, err := progressCeilings(goal, run, declared)
	if err != nil {
		return false
	}
	saved := rowMap(run, "spent")
	if declared.CostCeiling > 0 {
		if value, ok := saved["cost"]; !ok || value == nil {
			return false
		}
	}
	for _, field := range []string{"modelCalls", "tokens", "wallClockMs"} {
		if value, ok := saved[field]; !ok || value == nil {
			return false // An absent accounting snapshot is not zero usage.
		}
	}
	if cost, present := saved["cost"]; present && cost == nil {
		return false // JSON null otherwise decodes as an unspent numeric field.
	}
	raw, err := json.Marshal(saved)
	var spent workstate.Spent
	if err != nil || json.Unmarshal(raw, &spent) != nil || spent.ModelCalls < 0 || spent.Tokens < 0 || spent.Cost < 0 || spent.WallClockMs < 0 {
		return false
	}
	return workstate.CheckCeilings(enforceableHere(limits), spent, 0) == nil
}
