package pipelinerun

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

// Only operator configuration selects a resolver. Repository manifests cannot
// replace it, and its return value is workflow configuration, never authority.
const PolicyResolverVariable = "MEMQL_PIPELINES_POLICY_RESOLVER"
const pipelinePolicyContract = "pipelines.delivery.v1"

var policyRoles = map[string]string{
	"workflow":             "runPipelineStages",
	"stageIncluded":        "pipelineStageIncluded",
	"packageSelection":     "pipelinePackageSelection",
	"packageStepSelection": "pipelinePackageStepSelection",
	"bucketIncluded":       "pipelineBucketIncluded",
	"eventMode":            "pipelineModeForEvent",
	"versionForEvent":      "pipelineVersionForEvent",
	"notificationCopy":     "pipelineNotificationCopy",
	"notificationOutcome":  "pipelineNotificationOutcome",
	"artifactRetention":    "pipelineArtifactRetentionPolicy",
}

var optionalPolicyRoles = map[string]string{
	"supersession":      "pipelineSupersessionPolicy",
	"reconciliation":    "pipelineRunReconciliation",
	"maintenance":       "pipelineMaintenancePolicy",
	"evidenceReuse":     "pipelineEvidenceReuse",
	"independentReview": "pipelineIndependentReviewPolicy",
	"computePlacement":  "pipelineComputePlacementPolicy",
}

// Capture admits the union; each invocation below receives only its own native
// operations and preflights again. An effect in a selector is therefore refused
// before selection, even when that effect is legal in another captured phase.
func pipelinePolicyOperations() map[string]workflowhost.Operation {
	out := map[string]workflowhost.Operation{}
	for _, name := range []string{
		"pipelineReviewPullRequest", "pipelineReportMergeGroupReview", "pipelineWorkflowFacts", "pipelineSkipStep", "pipelineReportPublish", "pipelineReportObserve",
		"pipelineRetentionSweepPage", "integration.pipelines.executeStep", "integration.pipelines.reportProgress",
	} {
		out[name] = func(context.Context, map[string]any) (any, error) {
			return nil, fmt.Errorf("capture does not grant execution authority")
		}
	}
	return out
}

func capturePipelinePolicy(ctx context.Context, d Deps, p Pipeline, o Opening) (*workflowhost.Snapshot, error) {
	if o.RerunOf != "" {
		prior, err := d.Store.RunByID(memql.ContextWithFreshRead(ctx), bareID(o.RerunOf))
		if err != nil {
			return nil, err
		}
		if prior == nil || prior.PipelineID != p.ID || prior.Repository != p.Repository || prior.OwnerUserID != p.OwnerUserID {
			return nil, fmt.Errorf("pipeline retry has no matching source run")
		}
		if prior.PolicySnapshot != nil {
			return readPipelinePolicy(prior.PolicySnapshot)
		}
	}
	roles := maps.Clone(policyRoles)
	maps.Copy(roles, optionalPolicyRoles)
	resolver := ""
	if d.SystemVariable != nil {
		var err error
		resolver, err = d.SystemVariable(memql.ContextWithFreshRead(ctx), PolicyResolverVariable)
		if err != nil {
			if !memql.IsVariableNotFound(err) {
				return nil, err
			}
			// This optional setting has no seeded row. Absence selects the
			// same portable policy as an explicitly empty value; failed reads
			// and configured resolver errors must still refuse the opening.
			resolver = ""
		}
		resolver = strings.TrimSpace(resolver)
	}
	entries := []string{"pipelineSARIFDelivery"}
	if resolver != "" {
		selection, err := workflowhost.Capture(resolver, pipelinePolicyContract, d.WorkflowSource, nil)
		if err != nil {
			return nil, fmt.Errorf("pipeline policy resolver: %w", err)
		}
		value, err := workflowhost.RunSnapshot(ctx, selection, pipelinePolicyContract, map[string]any{
			"repository": p.Repository, "ownerUserId": p.OwnerUserID, "pipelineId": p.ID,
		}, workflowhost.Options{})
		if err != nil {
			return nil, fmt.Errorf("pipeline policy resolver: %w", err)
		}
		chosen, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("pipeline policy resolver must return an object")
		}
		if len(chosen) != 0 {
			for role := range chosen {
				if roles[role] == "" {
					return nil, fmt.Errorf("unknown pipeline policy role %q", role)
				}
			}
			for role := range policyRoles {
				name, ok := chosen[role].(string)
				if !ok || strings.TrimSpace(name) != name || name == "" {
					return nil, fmt.Errorf("pipeline policy resolver omitted role %q", role)
				}
				roles[role] = name
			}
			for role := range optionalPolicyRoles {
				if value, present := chosen[role]; present {
					name, ok := value.(string)
					if !ok || name == "" || strings.TrimSpace(name) != name {
						return nil, fmt.Errorf("invalid pipeline policy role %q", role)
					}
					roles[role] = name
				}
			}
		}
		entries = append(entries, resolver)
	}
	phases := make(map[string]string, len(roles))
	for role, name := range roles {
		canonical := policyRoles[role]
		if canonical == "" {
			canonical = optionalPolicyRoles[role]
		}
		phases[canonical] = name
	}
	return workflowhost.CapturePhases(roles["workflow"], pipelinePolicyContract, phases, entries, d.WorkflowSource, pipelinePolicyOperations())
}

func readPipelinePolicy(value map[string]any) (*workflowhost.Snapshot, error) {
	s, err := workflowhost.SnapshotFromMap(value)
	if err != nil {
		return nil, err
	}
	if s.Contract != pipelinePolicyContract {
		return nil, fmt.Errorf("incompatible pipeline policy contract")
	}
	for _, canonical := range policyRoles {
		if s.Phases[canonical] == "" {
			return nil, fmt.Errorf("pipeline policy snapshot is missing %s", canonical)
		}
	}
	return s, nil
}

func runPolicyPhase(ctx context.Context, snapshot *workflowhost.Snapshot, name string, args map[string]any, opts workflowhost.Options) (any, error) {
	return workflowhost.RunSnapshotEntry(ctx, snapshot, pipelinePolicyContract, snapshot.PhaseEntry(name), args, opts)
}

func policyModeForEvent(ctx context.Context, snapshot *workflowhost.Snapshot, p Pipeline, o Opening) (pipelines.Mode, error) {
	args := map[string]any{"event": string(o.Event)}
	if snapshot.PhaseEntry("pipelineModeForEvent") != "pipelineModeForEvent" {
		args["trigger"], args["repository"], args["sha"], args["pullRequest"] = o.Trigger, p.Repository, o.SHA, o.PullRequest
	}
	value, err := runPolicyPhase(ctx, snapshot, "pipelineModeForEvent", args, workflowhost.Options{})
	if err != nil {
		return "", err
	}
	mode, ok := value.(string)
	if !ok || (mode != "" && mode != string(pipelines.ModeAffected) && mode != string(pipelines.ModeFull)) {
		return "", fmt.Errorf("pipeline policy selected unsupported mode %v", value)
	}
	// Queue checks must continue to represent the complete integration tree.
	if o.Event == pipelines.EventMergeGroup && mode != string(pipelines.ModeFull) {
		return "", fmt.Errorf("merge-group validation requires full coverage")
	}
	return pipelines.Mode(mode), nil
}

func policyVersionForEvent(ctx context.Context, snapshot *workflowhost.Snapshot, o Opening, sha string) (string, error) {
	value, err := runPolicyPhase(ctx, snapshot, "pipelineVersionForEvent", map[string]any{
		"eventName": string(o.Event), "sha": sha, "tag": o.ReleaseTag,
	}, workflowhost.Options{})
	if err != nil {
		return "", err
	}
	version, ok := value.(string)
	if !ok || version == "" {
		return "", fmt.Errorf("pipeline policy returned no version")
	}
	return version, nil
}
