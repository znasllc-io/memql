package pipelinerun

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
)

type MaintenanceRequest struct {
	Key, OwnerUserID, Template, Statement string
	Input                                 map[string]any
}

// reconcileMaintenance is an internal post-terminal consumer. The installed
// policy may request analysis, never write/merge/install. The callback's stable
// identity survives duplicate events, lost responses and replica takeover.
func (i *Integration) reconcileMaintenance(ctx context.Context, d Deps, runID string) error {
	r, err := d.Store.RunByID(memql.ContextWithFreshRead(ctx), runID)
	if err != nil || r == nil {
		return err
	}
	if !r.Finished() || r.Conclusion != ConclusionFailure || (r.MaintenanceState != "" && r.MaintenanceState != "pending") {
		return nil
	}
	if r.PolicySnapshot == nil {
		return i.recordMaintenance(ctx, d, *r, "none", "", "This older run has no captured maintenance policy.")
	}
	policy, err := readPipelinePolicy(r.PolicySnapshot)
	if err != nil {
		return err
	}
	value, err := runPolicyPhase(ctx, policy, "pipelineMaintenancePolicy", map[string]any{
		"event": string(r.Event), "conclusion": r.Conclusion, "failureCode": r.RefusalCode,
		"attempt": r.Attempt, "repository": r.Repository, "sha": r.SHA,
	}, workflowhost.Options{})
	if err != nil {
		return err
	}
	answer, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("maintenance policy must return a disposition")
	}
	action, _ := answer["action"].(string)
	template, _ := answer["template"].(string)
	reason, _ := answer["reason"].(string)
	if len(reason) > 2000 || (action != "none" && action != "human" && action != "analysis") {
		return fmt.Errorf("unsupported maintenance disposition")
	}
	if action == "analysis" && (r.Attempt != 1 || r.RerunOf != "") {
		action, reason = "human", "This failed retry needs review of the existing diagnosis before more repair work."
	}
	// The owner's AI diagnosis switch (Pipelines > Settings > AI) is off unless
	// turned on. Off, the failure still reports itself and no model is asked;
	// a policy's own non-AI decision (none, human) is left as it chose.
	if action == "analysis" && !d.aiSettings(ctx).Diagnosis {
		action, reason = "none", aiDiagnosisOffMessage
	}
	goalID := ""
	if action == "analysis" {
		if template == "" || len(template) > 256 || strings.TrimSpace(template) != template || reason == "" || d.OpenMaintenanceGoal == nil || d.GitHub == nil {
			return fmt.Errorf("maintenance analysis requires an installed template, explanation and goal service")
		}
		p, err := d.Store.PipelineByID(memql.ContextWithFreshRead(ctx), r.PipelineID)
		if err != nil {
			return err
		}
		if p == nil || !p.Active() || !sameID(p.OwnerUserID, r.OwnerUserID) || p.Repository != r.Repository {
			return fmt.Errorf("maintenance source ownership changed")
		}
		// Analysis does not require push authority, but still needs the owner's
		// current grant to read the source. No token is handed to the goal.
		_, installation, err := d.GitHub.InstallationToken(ctx, p.CredentialID, p.OwnerUserID, p.Repository)
		if err != nil {
			return err
		}
		if installation != p.InstallationID {
			return fmt.Errorf("maintenance source installation changed")
		}
		if err := i.recordMaintenance(ctx, d, *r, "pending", "", reason); err != nil {
			return err
		}
		goalID, err = d.OpenMaintenanceGoal(auth.ContextWithUserActor(ctx, r.OwnerUserID), MaintenanceRequest{
			Key: "pipeline-maintenance:" + bareID(r.ID), OwnerUserID: r.OwnerUserID, Template: template,
			Statement: "Diagnose the failed delivery for " + r.Repository + ": " + reason,
			Input:     map[string]any{"pipelineId": r.PipelineID, "repository": r.Repository, "sha": r.SHA, "runId": r.ID, "failureCode": r.RefusalCode, "reason": reason},
		})
		if err != nil {
			return err
		}
		if goalID == "" {
			return fmt.Errorf("maintenance goal service returned no durable identity")
		}
	}
	return i.recordMaintenance(ctx, d, *r, action, goalID, reason)
}

func (i *Integration) recordMaintenance(ctx context.Context, d Deps, original Run, state, goalID, message string) error {
	return d.gate(ctx, RunGateKey(original.ID), func(gctx context.Context) error {
		current, err := d.Store.RunByID(memql.ContextWithFreshRead(gctx), original.ID)
		if err != nil {
			return err
		}
		if current == nil || !current.Finished() || current.Conclusion != ConclusionFailure || !sameID(current.OwnerUserID, original.OwnerUserID) {
			return fmt.Errorf("maintenance source changed")
		}
		if current.MaintenanceState != "" && current.MaintenanceState != "pending" {
			return nil
		}
		return d.Store.UpdateRun(gctx, current.OwnerUserID, current.ID, RunPatch{MaintenanceState: &state, MaintenanceGoalID: &goalID, MaintenanceMessage: &message})
	})
}

func (i *Integration) recoverMaintenance(ctx context.Context, d Deps) error {
	runs, err := d.Store.RunsAwaitingMaintenance(memql.ContextWithFreshRead(ctx))
	if err != nil {
		return err
	}
	for _, r := range runs {
		if err := i.reconcileMaintenance(ctx, d, r.ID); err != nil {
			d.Logger.Warn("pipelines: failed-run diagnosis awaits recovery", "run", r.ID, "error", err)
		}
	}
	return nil
}
