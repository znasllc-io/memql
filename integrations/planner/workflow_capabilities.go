package planner

import (
	"context"
	"fmt"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

func (p *PlannerIntegration) IntegrationName() string { return "planner" }
func (p *PlannerIntegration) Capabilities() []memql.IntegrationCapability {
	capabilities := append([]memql.IntegrationCapability{{Name: "plannerPursueResponsibilities", Handler: func(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		if !auth.OriginFromContext(ctx).IsInternal() {
			return nil, fmt.Errorf("responsibility sweep requires the trusted scheduler")
		}
		return nil, p.reactiveLoop.run(ctx)
	}}, {Name: "plannerRefreshKnowledge", Handler: func(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		if !auth.OriginFromContext(ctx).IsInternal() {
			return nil, fmt.Errorf("knowledge refresh requires the trusted scheduler")
		}
		return nil, p.refreshCron.run(ctx)
	}}}, workflowhost.ScopedCapabilities(map[string]workflowhost.Operation{
		"plannerResponsibilityOwners": nil, "plannerProcessOwner": nil, "plannerProcessResponsibility": nil, "plannerConverge": nil, "plannerHasLiveGoal": nil, "plannerClaimResponsibility": nil, "plannerResolveAssistant": nil, "plannerResolveSpecialist": nil, "plannerAssignResponsibility": nil, "plannerAppendDirective": nil, "plannerRecordEvaluation": nil, "plannerTryHonor": nil, "plannerOpenResponsibilityGoal": nil, "plannerWorkflowRefusal": nil, "plannerClaimConvergence": nil, "plannerSpaceGoals": nil, "plannerRecentMemory": nil, "plannerConvergenceTurn": nil, "plannerDispatchConvergence": nil, "plannerClaimAction": nil, "plannerConvergenceGoal": nil, "plannerConvergenceNudge": nil, "plannerLogNudge": nil, "plannerConvergenceSpecialist": nil,
		"knowledgeRefreshCandidates": nil, "knowledgeRefreshCandidate": nil, "knowledgeClaimRefresh": nil, "knowledgeOpenRefresh": nil, "knowledgeDispatchRefresh": nil,
	})...)
	ops := map[string]workflowhost.Operation{}
	for _, name := range work.SpineOperations() {
		ops[name] = nil
	}
	return append(capabilities, workflowhost.ScopedCapabilities(ops)...)
}
