//go:build agent

package agent

// The default fallback recipe lives in dsl/agents/automations.memql.
// Native code owns pre-start evidence, authority, call ceilings and tool dispatch.

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"slices"
	"strings"

	agentworker "github.com/znasllc-io/memql/integrations/agent/worker"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// rerouteToolNames are the two the reroute drives.
const (
	rerouteFleetTool = "workerHost"
	rerouteScopeTool = "requestComputerUseScope"
)

// rerouteWorkbenchMismatch lends the tool invocation to an installed DSL recipe.
// The host retains the original call and the verified pre-start mismatch;
// workflow arguments can neither invent that evidence nor change the call.
func (r *Replier) rerouteWorkbenchMismatch(ctx context.Context, turnCtx turnContext, toolName, result string, args map[string]any) (string, bool) {
	if toolName != "workbenchHost" || r == nil || r.stamper == nil {
		return "", false
	}
	plan, eligible := planWorkbenchReroute(result, args, turnCtx)
	if !eligible {
		return "", false
	}
	s := &toolRecoveryScope{plan: plan, turn: turnCtx, original: result,
		execute: r.stamper.ExecuteToolByName}
	value, err := s.run(ctx, nil)
	if err != nil {
		r.logger.Warn("agent: tool recovery stopped", "error", err, "run_id", turnCtx.RunId)
		// A transport error after dispatch is an UNKNOWN outcome. Never hide it
		// behind the earlier workbench refusal, which proved only that first call
		// did not start, and could invite a duplicate effect on the next turn.
		code := "recovery_unavailable"
		if s.attempted {
			code = "tool_outcome_unknown"
		}
		b, _ := json.Marshal(map[string]any{"ok": false, "errorCode": code, "errorMessage": err.Error()})
		return string(b), true
	}
	return value, value != result
}

type toolRecoveryScope struct {
	plan                 workbenchReroute
	turn                 turnContext
	original, last       string
	execute              func(context.Context, string, map[string]any) (string, error)
	attempted, requested bool
	lastErr              error
}

func (s *toolRecoveryScope) operations() map[string]workflowhost.Operation {
	return map[string]workflowhost.Operation{
		"agentRecoveryContext": func(context.Context, map[string]any) (any, error) {
			needs := make([]any, len(s.plan.Mismatch.UnmetNeeds))
			for i, need := range s.plan.Mismatch.UnmetNeeds {
				needs[i] = need
			}
			return map[string]any{"action": s.plan.Action, "needs": needs,
				"requestedScope": s.plan.RequestedScope, "original": s.original}, nil
		},
		"integration.agents.spineRetryHost": func(ctx context.Context, _ map[string]any) (any, error) {
			return s.attempt(ctx, rerouteFleetTool, s.plan.FleetArgs)
		},
		"integration.agents.spineObserveComputer": func(ctx context.Context, args map[string]any) (any, error) {
			if !slices.Contains(s.plan.Mismatch.UnmetNeeds, workbench.NeedDisplay) {
				return nil, fmt.Errorf("computer observation requires a verified display mismatch")
			}
			// This observes the UI for the next model turn. It never translates a
			// refused shell command into invented clicks or repeats a completed effect.
			observation := getRecoveryObservation(args)
			if observation == "" {
				return nil, fmt.Errorf("computer recovery observation must be window_list or display_info")
			}
			s.plan.RequestedScope = "observe"
			s.plan.CardArgs["requestedScope"] = "observe"
			s.plan.CardArgs["intent"] = observation
			return s.attempt(ctx, "workerComputer", map[string]any{
				"action": observation, "args": map[string]any{}, "requireLabels": s.plan.RequireLabels,
				"agentId": s.turn.AgentId, "ownerUserId": s.turn.OwnerUserId, "runId": s.turn.RunId,
			})
		},
		"integration.agents.spineRequestScope": func(ctx context.Context, args map[string]any) (any, error) {
			code := rerouteErrorCode(s.last)
			if s.lastErr != nil || !s.attempted || s.requested || (code != "denied_no_per_task_approval" && code != "denied_by_scope") {
				return nil, fmt.Errorf("scope request requires this invocation's verified consent refusal and may run once")
			}
			summary := strings.TrimSpace(stringFromArgs(args, "summary"))
			if summary == "" {
				return nil, fmt.Errorf("scope request requires a summary")
			}
			s.requested = true
			s.plan.CardArgs["summary"] = summary
			return s.call(ctx, rerouteScopeTool, s.plan.CardArgs)
		},
	}
}

func (s *toolRecoveryScope) attempt(ctx context.Context, tool string, args map[string]any) (any, error) {
	if s.attempted || s.lastErr != nil {
		return nil, fmt.Errorf("tool recovery permits one attempt after a verified pre-start refusal")
	}
	s.attempted = true
	return s.call(ctx, tool, args)
}

func (s *toolRecoveryScope) call(ctx context.Context, tool string, args map[string]any) (any, error) {
	result, err := s.execute(agentToolCallContext(ctx, tool, s.turn), tool, args)
	if err == nil && tool == "workerComputer" {
		// The original workbench call did not execute. An observation is evidence
		// for a subsequent decision, never a receipt for the refused command.
		var receipt map[string]any
		if json.Unmarshal([]byte(result), &receipt) == nil && receipt != nil {
			receipt["recovery"] = map[string]any{"kind": "computer_observation", "originalActionExecuted": false, "originalAction": s.plan.Action}
			encoded, encodeErr := json.Marshal(receipt)
			if encodeErr != nil {
				return nil, encodeErr
			}
			result = string(encoded)
		}
	}
	s.last, s.lastErr = result, err
	if err != nil {
		return nil, err
	}
	return map[string]any{"content": result, "errorCode": rerouteErrorCode(result)}, nil
}

func (s *toolRecoveryScope) run(ctx context.Context, source workflowhost.SourceLoader) (string, error) {
	ops := s.operations()
	var snapshot *workflowhost.Snapshot
	var err error
	contract := "agent.tool-recovery/1"
	if run, ok := common.RunFromContext(ctx); ok && run.Spine != nil {
		snapshot, err = workflowhost.SnapshotFromMap(run.Spine)
		if err != nil {
			return "", err
		}
		if len(snapshot.Entries) > 0 {
			contract = work.SpineContract
		} else {
			snapshot = nil
		}
	}
	if snapshot == nil {
		snapshot, err = workflowhost.Capture("agentWorkbenchRecovery", contract, source, ops)
	}
	if err != nil {
		return "", err
	}
	out, err := workflowhost.RunSnapshotEntry(ctx, snapshot, contract, snapshot.PhaseEntry("agentWorkbenchRecovery"), nil, workflowhost.Options{Operations: ops})
	if s.lastErr != nil {
		return "", s.lastErr
	}
	if err != nil {
		return "", err
	}
	value, ok := out.(string)
	// A recipe chooses actions, not fabricated tool evidence. Once a dispatch
	// happened its actual result is what the model must see.
	expected := s.original
	if s.attempted {
		expected = s.last
	}
	if !ok || value != expected {
		return "", fmt.Errorf("tool recovery must return its actual final tool result")
	}
	return value, nil
}

// rerouteErrorCode pulls errorCode out of a worker tool result. An unparseable
// result yields "", which the caller reads as "not a refusal" -- the safe
// direction, because the alternative is raising a consent card for a call that
// already ran.
func rerouteErrorCode(result string) string {
	var envelope struct {
		OK        bool   `json:"ok"`
		ErrorCode string `json:"errorCode"`
	}
	if err := json.Unmarshal([]byte(result), &envelope); err != nil {
		return ""
	}
	if envelope.OK {
		return ""
	}
	return envelope.ErrorCode
}

// stringFromArgs reads a string argument, tolerating absence.
func stringFromArgs(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	s, _ := args[key].(string)
	return s
}

// workbenchReroute is the decision, separated from its execution so it can be
// tested without a tool executor. Everything that reads the mismatch, derives
// the routing requirement and builds both argument maps is here; the method
// above only runs it and reads the answers.
type workbenchReroute struct {
	Mismatch       workbench.EnvironmentMismatch
	Action         string
	RequireLabels  map[string]string
	RequestedScope string
	FleetArgs      map[string]any
	CardArgs       map[string]any
}

// planWorkbenchReroute reads a workbenchHost result. The second return is
// false for anything that is not an environment mismatch -- a success, any
// other failure, or an unparseable body -- which is the caller's signal to
// leave the model's answer alone.
func planWorkbenchReroute(result string, args map[string]any, turnCtx turnContext) (workbenchReroute, bool) {
	mismatch, ok := workbench.EnvironmentMismatchFromPayload([]byte(result))
	if !ok {
		return workbenchReroute{}, false
	}
	action := strings.TrimSpace(stringFromArgs(args, "action"))
	inner, _ := args["args"].(map[string]any)
	requireLabels := agentworker.EnvironmentNeedsLabels(mismatch.UnmetNeeds, mismatch.RequestedOS)
	scope := agentworker.EnvironmentNeedsScope(mismatch.UnmetNeeds)

	return workbenchReroute{
		Mismatch:       mismatch,
		Action:         action,
		RequireLabels:  requireLabels,
		RequestedScope: scope,
		FleetArgs: map[string]any{
			// The SAME action and the SAME arguments. workerHost and
			// workbenchHost carry the identical six verbs, so a reroute is a
			// change of destination and nothing else -- rewriting the call on
			// the way across would make the machine run something other than
			// what the workbench refused.
			"action":        action,
			"args":          inner,
			"agentId":       turnCtx.AgentId,
			"ownerUserId":   turnCtx.OwnerUserId,
			"runId":         turnCtx.RunId,
			"requireLabels": requireLabels,
			// The routing record's answer to "why did this run on the laptop".
			"reroutedFrom": agentworker.ReroutedFromWorkbench,
		},
		CardArgs: map[string]any{
			"intent":         action,
			"requestedScope": scope,

			"agentId":     turnCtx.AgentId,
			"ownerUserId": turnCtx.OwnerUserId,
			"partitionId": turnCtx.PartitionId,
			// The card names the requirement, so the user's Allow visibly
			// covers a SET of machines rather than appearing to name one.
			"requireLabels": requireLabels,
		},
	}, true
}

// The current agent tool conversation is text-based. Structured desktop facts
// are usable observations; passing screenshot bytes as text would not be vision.
func getRecoveryObservation(args map[string]any) string {
	action, _ := args["action"].(string)
	if action == "window_list" || action == "display_info" {
		return action
	}
	return ""
}
