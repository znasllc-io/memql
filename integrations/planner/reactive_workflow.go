package planner

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/core/num"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

// Every invocation binds one owner and one selected responsibility. DSL may
// choose the recipe, but cannot replace that authority with serialized args.
func (r *ReactiveLoop) runRow(ctx context.Context, owner string, row map[string]any, workflow string, now time.Time, extra map[string]any) (any, error) {
	normalized := map[string]any{}
	for k, v := range row {
		normalized[k] = v
	}
	for _, k := range []string{"id", "trigger", "targetKind", "assignedAgentId", "assignedRoleSlug", "statement", "successCriteria", "scopePartitionId"} {
		normalized[k] = getString(row, k)
	}
	args := map[string]any{"row": normalized, "recurringDue": r.recurringIsDue(row, now), "agentId": ""}
	for k, v := range extra {
		args[k] = v
	}
	return workflowhost.Run(ctx, workflow, args, workflowhost.Options{Operations: map[string]workflowhost.Operation{
		"plannerHasLiveGoal": func(ctx context.Context, _ map[string]any) (any, error) {
			return r.hasLiveGoal(ctx, owner, getString(row, "id")), nil
		},
		"plannerClaimResponsibility": func(ctx context.Context, a map[string]any) (any, error) {
			return r.claim(ctx, owner+":"+getString(row, "id")+":"+getString(a, "purpose"), a["minutes"])
		},
		"plannerResolveAssistant": func(ctx context.Context, _ map[string]any) (any, error) { return r.resolveAssistant(ctx, owner), nil },
		"plannerResolveSpecialist": func(ctx context.Context, _ map[string]any) (any, error) {
			return r.mintOrExtendSpecialist(ctx, owner, row)
		},
		"plannerAssignResponsibility": func(ctx context.Context, a map[string]any) (any, error) {
			r.assign(ctx, getString(row, "id"), getString(a, "kind"), getString(a, "agentId"), getString(a, "roleSlug"))
			return nil, nil
		},
		"plannerAppendDirective": func(ctx context.Context, a map[string]any) (any, error) {
			return nil, r.appendDirective(ctx, row, getString(a, "agentId"))
		},
		"plannerRecordEvaluation": func(ctx context.Context, a map[string]any) (any, error) {
			r.recordEvaluation(ctx, getString(row, "id"), getString(a, "result"))
			return nil, nil
		},
		"plannerTryHonor": func(ctx context.Context, a map[string]any) (any, error) {
			result, err := r.honorResponsibility(ctx, owner, row, getString(a, "agentId"))
			if err != nil {
				return map[string]any{"ok": false, "result": err.Error()}, nil
			}
			return map[string]any{"ok": true, "result": result}, nil
		},
		"plannerOpenResponsibilityGoal": func(ctx context.Context, a map[string]any) (any, error) {
			r.mu.Lock()
			goals := r.goals
			r.mu.Unlock()
			if goals == nil {
				return nil, fmt.Errorf("the work spine is not wired on this node")
			}
			input, _ := a["input"].(map[string]any)
			goalID, _, err := goals.OpenResponsibilityGoal(ctx, workintegration.ResponsibilityGoal{OwnerUserId: owner, ResponsibilityId: getString(row, "id"), Statement: getString(a, "statement"), Input: input})
			return goalID, err
		},
		"plannerWorkflowRefusal": func(_ context.Context, a map[string]any) (any, error) {
			return nil, fmt.Errorf("%s", getString(a, "message"))
		},
	}})
}

func (r *ReactiveLoop) claim(ctx context.Context, key string, minutes any) (bool, error) {
	if r.claims == nil {
		return false, fmt.Errorf("planner workflow requires a durable claim store")
	}
	n := numberField(minutes)
	if n <= 0 || n > 24*60 {
		return false, fmt.Errorf("invalid planner claim duration")
	}
	return r.claims.ClaimWithTTL(ctx, "planner.responsibility", key, time.Duration(n*float64(time.Minute))), nil
}

func (r *ReactiveLoop) converge(ctx context.Context, owner string, rows []map[string]any, now time.Time) error {
	selected := map[string]convergenceAction{}
	_, err := workflowhost.Run(ctx, "plannerConvergenceWorkflow", map[string]any{"owner": owner, "responsibilities": compactResponsibilities(rows), "observedAt": now.Format(time.RFC3339)}, workflowhost.Options{Operations: map[string]workflowhost.Operation{
		"plannerClaimConvergence": func(ctx context.Context, a map[string]any) (any, error) {
			return r.claim(ctx, "convergence:"+owner, a["minutes"])
		},
		"plannerSpaceGoals": func(ctx context.Context, _ map[string]any) (any, error) { return r.loadSpaceGoals(ctx, owner), nil },
		"plannerRecentMemory": func(ctx context.Context, a map[string]any) (any, error) {
			k := numberField(a["k"])
			if k < 1 || k > 100 {
				return nil, fmt.Errorf("recall bound must be between 1 and 100")
			}
			return r.loadRecentMemory(ctx, getString(a, "text"), num.Float64OrZero(k)), nil
		},
		"plannerConvergenceTurn": func(ctx context.Context, a map[string]any) (any, error) {
			data, _ := a["data"].(map[string]any)
			out, err := r.engine.InvokeAI(ctx, getString(a, "prompt"), data)
			if err != nil {
				return nil, err
			}
			decision, err := parseConvergence(out)
			if err != nil {
				return nil, err
			}
			ids := []string{}
			for i, action := range decision.Actions {
				id := fmt.Sprint(i)
				selected[id] = action
				ids = append(ids, id)
			}
			return ids, nil
		},
		"plannerDispatchConvergence": func(ctx context.Context, a map[string]any) (any, error) {
			action, ok := selected[getString(a, "id")]
			if !ok {
				return nil, fmt.Errorf("action outside model response")
			}
			return nil, r.dispatch(ctx, owner, action, now)
		},
	}})
	return err
}

func (r *ReactiveLoop) dispatch(ctx context.Context, owner string, a convergenceAction, now time.Time) error {
	// Canonical key encoding is mechanical; key selection and duration are DSL.
	args := map[string]any{"action": map[string]any{"kind": a.Kind, "responsibilityId": a.ResponsibilityId, "statement": a.Statement, "confidence": a.Confidence}, "statementKey": truncate(a.Statement, 64)}
	_, err := workflowhost.Run(ctx, "plannerConvergenceAction", args, workflowhost.Options{Operations: map[string]workflowhost.Operation{
		"plannerClaimAction": func(ctx context.Context, args map[string]any) (any, error) {
			return r.claim(ctx, "convergence-action:"+owner+":"+getString(args, "key"), args["minutes"])
		},
		"plannerConvergenceGoal": func(ctx context.Context, _ map[string]any) (any, error) {
			return r.openResponsibilityGoal(ctx, owner, map[string]any{"id": a.ResponsibilityId, "statement": a.Statement, "trigger": "reactive"}, "")
		},
		"plannerConvergenceNudge": func(ctx context.Context, args map[string]any) (any, error) {
			r.recordEvaluation(ctx, a.ResponsibilityId, getString(args, "result"))
			return nil, nil
		},
		"plannerLogNudge": func(_ context.Context, _ map[string]any) (any, error) {
			r.logger.Info("planner convergence nudge", "userId", owner, "statement", a.Statement)
			return nil, nil
		},
		"plannerConvergenceSpecialist": func(ctx context.Context, _ map[string]any) (any, error) {
			return r.routeResponsibility(ctx, owner, map[string]any{"id": a.ResponsibilityId, "statement": a.Statement, "targetKind": "unassigned", "trigger": "standing"})
		},
	}})
	return err
}
