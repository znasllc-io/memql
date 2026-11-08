package app

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/pipelinerun"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

func (a *App) wirePipelineMaintenance(pipeline *pipelinerun.Integration) {
	if a.engine == nil || pipeline == nil {
		return
	}
	pipeline.Configure(func(d *pipelinerun.Deps) {
		d.OpenSecurityGoal = func(ctx context.Context, r pipelinerun.SecurityGoalRequest) (string, error) {
			work, ok := a.engine.IntegrationByName("work").(*workspine.Integration)
			if !ok || work == nil {
				return "", fmt.Errorf("pipeline security goal service is unavailable")
			}
			goal := workspine.DirectGoal{OwnerUserId: r.OwnerUserID, Statement: r.Statement, Input: r.Input, AccountIds: r.AccountIDs,
				BeforeRun: func(c context.Context, _, _ string) error { return r.Check(c) }}
			receipt, err := work.OpenPipelineSecurityGoal(ctx, r.Key, goal)
			return receipt.GoalID, err
		}
		d.OpenMaintenanceGoal = func(ctx context.Context, r pipelinerun.MaintenanceRequest) (string, error) {
			work, ok := a.engine.IntegrationByName("work").(*workspine.Integration)
			if !ok || work == nil {
				return "", fmt.Errorf("pipeline analysis goal service is unavailable")
			}
			receipt, err := work.OpenAnalysisGoal(ctx, r.Key, workspine.DirectGoal{
				OwnerUserId: r.OwnerUserID, Statement: r.Statement, AutomationName: r.Template, Input: r.Input,
				RequestedVia: "pipeline", TriggeredBy: "pipeline-maintenance",
			})
			return receipt.GoalID, err
		}
	})
}
