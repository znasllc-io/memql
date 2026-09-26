package app

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// Execution starts on a different replica from intake and compilation. The
// persisted journal supplies identity and lineage; no context value can be
// assumed to have followed the graph event here.
func workExecutionContext(ctx context.Context, j, source *automations.RunJournal, resolver *auth.IdentityResolver) (context.Context, error) {
	run := common.RunContext{RunId: j.RunId, GoalId: j.GoalId, OwnerUserId: j.OwnerUserId, Mode: j.Mode, ReplayPolicy: j.ReplayPolicy, ForkAtStepKey: j.ForkAtStepKey}
	if run.Mode == "" {
		run.Mode = common.RunModeLive
	}
	if j.ForkedFromRunId != "" {
		if source == nil || memql.BareShortId(source.RunId) != memql.BareShortId(j.ForkedFromRunId) || memql.BareShortId(source.GoalId) != memql.BareShortId(j.GoalId) || source.OwnerUserId != j.OwnerUserId {
			return ctx, fmt.Errorf("the source journal does not belong to this goal and owner")
		}
		run.SourceRunId = j.ForkedFromRunId
		run.SourceGoalId = source.GoalId
		run.StepOrder = source.StepOrder
	}
	// Ownerless scheduler journals keep their system context. A goal-backed
	// run must name its persisted owner before any owned work can execute.
	if j.OwnerUserId != "" || j.GoalId != "" {
		var err error
		ctx, err = auth.ContextWithPersistedOwner(ctx, j.OwnerUserId, j.ExecutionAuthority, resolver)
		if err != nil {
			return nil, err
		}
	}
	ctx = common.ContextWithRun(ctx, run)
	return memql.ContextWithBudgetScope(ctx, memql.BudgetScopeId("run", j.RunId), memql.BudgetScopeId("goal", j.GoalId)), nil
}
