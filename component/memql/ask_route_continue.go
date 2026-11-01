package memql

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/language/parser"
)

// Finished executions are not activeWorkRun: that predicate also projects
// queued messages and must not pretend that finished work is still running.
// Routing separately admits the latest result of an open, unaccepted goal.
func (e *MemQLEngine) routableAskGoals(ctx context.Context, transcript askTranscript) ([]askRouteGoal, error) {
	return e.askGoals(ctx, transcript, true)
}

func (e *MemQLEngine) askContinuationVersion(ctx context.Context, run map[string]any) (string, error) {
	if status := stringArg(run, "status"); status != "succeeded" && status != "failed" {
		return "", nil
	}
	if run["cancelRequested"] == true || stringArg(run, "parentRunId") != "" || stringArg(run, "goalId") == "" {
		return "", nil
	}
	if mode := stringArg(run, "mode"); mode != "" && mode != "live" {
		return "", nil
	}
	goalID := stringArg(run, "goalId")
	read := func(name string, args map[string]any) ([]map[string]any, error) {
		call, err := parser.RenderCall(name, args)
		if err != nil {
			return nil, err
		}
		result, err := e.Execute(ContextWithFreshRead(ctx), "query "+call)
		if err != nil {
			return nil, err
		}
		return MaterializeRows(result.OutputPayload()), nil
	}
	goals, err := read("workGoalForOwner", map[string]any{"goalId": goalID})
	if err != nil {
		return "", err
	}
	if len(goals) != 1 || stringArg(goals[0], "status") != "open" {
		return "", nil
	}
	if accepted, _ := goals[0]["acceptance"].(map[string]any); len(accepted) != 0 {
		return "", nil
	}
	version := askRouteRunVersion(run)
	_, err = time.Parse(time.RFC3339Nano, version)
	if err != nil {
		return "", nil
	}
	conflicts, err := read("workContinuationConflictsForOwnerGoal", map[string]any{"goalId": goalID, "runVersion": version})
	if err != nil {
		return "", err
	}
	if len(conflicts) != 0 {
		return "", nil
	}
	return version, nil
}

func askRouteRunVersion(run map[string]any) string {
	if at, ok := run["createdAt"].(time.Time); ok {
		return at.UTC().Format(time.RFC3339Nano)
	}
	return stringArg(run, "createdAt")
}

// This is a native lifecycle action, outside the conversation lock. The work
// handler rechecks ownership, exact result and acceptance under the goal gate.
// Its durable request receipt makes a transport retry idempotent. A refusal
// stays on this goal; it must not silently become a second intake request.
func (e *MemQLEngine) deliverContinuationTargets(ctx context.Context, conversationID, turnID string, goals []askRouteGoal, text string) (delivered []AskRouteTarget, rest []askRouteGoal) {
	for _, goal := range goals {
		if goal.ContinueVersion == "" {
			rest = append(rest, goal)
			continue
		}
		target := AskRouteTarget{GoalID: goal.GoalID, RunID: goal.RunID, Title: goal.Title, Text: text, Status: routeNotDelivered, Reason: routeReasonRefused}
		if guidanceReaches(goal.Run, conversationID) && guidanceFits(text) {
			call, err := parser.RenderCall("work.continueRun", map[string]any{"runId": goal.RunID, "runVersion": goal.ContinueVersion, "requestId": BareShortId(conversationID) + ":" + turnID, "feedback": text})
			if err == nil {
				result, err := e.Execute(ctx, "builtin "+call)
				if err == nil {
					rows := MaterializeRows(result.OutputPayload())
					if len(rows) == 1 && rows[0]["continuationRequested"] == true && BareShortId(stringArg(rows[0], "runId")) == BareShortId(goal.RunID) {
						target.Status, target.Reason = routeReceived, routeReasonContinued
					}
				}
			}
		}
		delivered = append(delivered, target)
	}
	return delivered, rest
}
