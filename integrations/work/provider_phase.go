package work

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// Non-execution inference belongs to the native operation that admitted it,
// not to every caller that happens to find the run in the same status. These
// private stamps are created after the compiler/remedy claim or answer-check
// entry gates. They are neither DSL arguments nor wire authority. The provider
// seam runs before Fleet transport; a receiving planner reconstructs its own
// stamp after claiming the persisted run, without intake-node memory.
type providerPhaseKey struct{}

type providerPhase struct {
	runID, goalID, ownerID string
	status, nodeID         string
	heartbeat              time.Time
	waitKind, since        string
	stepKey                string
	stepVersion            int
	runVersion             time.Time
	expiresAt              time.Time
}

// Return only the phase binding. Its caller owns the context and keeps the
// package's no-returned-authority-context invariant.
func bindProviderPhase(rc common.RunContext, phase providerPhase) providerPhase {
	phase.runID, phase.goalID, phase.ownerID = rc.RunId, rc.GoalId, rc.OwnerUserId
	return phase
}

// Called under the workload gate after an owner-scoped fresh run read. A phase
// context stays bound to that phase even after the run resumes execution.
func (i *Integration) admitProviderPhase(ctx context.Context, rc common.RunContext, run map[string]any) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if run == nil || run["cancelRequested"] == true || memql.BareShortId(rowString(run, "goalId")) != memql.BareShortId(rc.GoalId) {
		return fmt.Errorf("work is cancelled or no longer belongs to this goal")
	}
	phase, scoped := ctx.Value(providerPhaseKey{}).(providerPhase)
	status := rowString(run, "status")
	if !scoped {
		if status == runStatusRunning {
			return nil
		}
		return fmt.Errorf("work is not running and this call has no admitted work phase")
	}
	if memql.BareShortId(phase.runID) != memql.BareShortId(rc.RunId) || memql.BareShortId(phase.goalID) != memql.BareShortId(rc.GoalId) || memql.BareShortId(phase.ownerID) != memql.BareShortId(rc.OwnerUserId) || phase.status != status {
		return fmt.Errorf("the admitted work phase no longer matches this run")
	}
	if !phase.expiresAt.IsZero() && !i.clock().Before(phase.expiresAt) {
		return fmt.Errorf("the admitted work phase expired")
	}
	switch status {
	case runStatusCompiling:
		heartbeat, ok := rowTime(run, "heartbeatAt")
		if phase.nodeID != "" && phase.nodeID == rowString(run, "nodeId") && !phase.heartbeat.IsZero() && ok && !heartbeat.Before(phase.heartbeat) {
			return nil
		}
	case runStatusWaiting:
		waiting := rowMap(run, "waitingOn")
		if (phase.waitKind == waitKindReplan || phase.waitKind == waitKindRepair) && phase.since != "" && phase.stepKey != "" &&
			rowString(waiting, "kind") == phase.waitKind && rowString(waiting, "since") == phase.since && rowString(waiting, "subject") == phase.stepKey {
			return nil
		}
	case runStatusSucceeded:
		version, ok := rowTime(run, "createdAt")
		if !ok || phase.runVersion.IsZero() || !version.Equal(phase.runVersion) || phase.stepKey == "" || phase.stepKey != rc.StepKey || phase.stepVersion < 1 {
			break
		}
		rows, err := i.store().query(ctx, "query "+call("workStepForOwnerRun", map[string]any{"runId": rc.RunId, "stepKey": phase.stepKey}))
		if err != nil {
			return err
		}
		if len(rows) == 1 && rowString(rows[0], "status") == "done" && rowVersion(rows[0]) == phase.stepVersion {
			return nil
		}
	}
	return fmt.Errorf("the admitted compiler, remedy or answer version is no longer current")
}
