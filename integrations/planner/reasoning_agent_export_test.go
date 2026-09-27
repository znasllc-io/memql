package planner

import (
	"context"
	"strings"
	"testing"
)

// ResolveReasoningAgent is exported for a learned procedure's replay (epic
// memql#5408), which dispatches to the owner's machine and hands a diverged
// goal back to their app under this agent. These pin what the export adds to
// the resolution compile already used: the ROLE, which the replay acts under,
// and that the assistant still wins over the seeded planner.

func TestResolveReasoningAgentPrefersTheAssistantAndNamesItsRole(t *testing.T) {
	engine := &fakeEngine{execResponder: func(query string) (any, error) {
		if strings.Contains(query, "assistantAgentForUser") {
			return rowsEnvelope(map[string]any{"id": "v1:agents:agent:assistant-owner"}), nil
		}
		t.Fatalf("the seeded planner was read although an assistant exists: %s", query)
		return nil, nil
	}}
	agent, err := ResolveReasoningAgent(context.Background(), engine, "v1:identity:user:owner")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Id != "v1:agents:agent:assistant-owner" || agent.RoleSlug != "assistant" {
		t.Fatalf("agent = %+v, want the assistant with its role", agent)
	}
}

func TestResolveReasoningAgentFallsBackToTheSeededPlannerAndNamesItsRole(t *testing.T) {
	planner := map[string]any{"id": "v1:agents:agent:plannerAgent-owner", "ownerUserId": "v1:identity:user:owner",
		"active": true, "deleted": false, "roleSlug": "system-planner"}
	engine := &fakeEngine{execResponder: func(query string) (any, error) {
		if strings.Contains(query, "assistantAgentForUser") {
			return rowsEnvelope(), nil
		}
		return rowsEnvelope(planner), nil
	}}
	agent, err := ResolveReasoningAgent(context.Background(), engine, "v1:identity:user:owner")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Id != "v1:agents:agent:plannerAgent-owner" || agent.RoleSlug != "system-planner" {
		t.Fatalf("agent = %+v, want the seeded planner with its role", agent)
	}
	// And the method compile calls answers the same agent: one rule, two
	// callers.
	id, err := (&PlannerAgentLoop{engine: engine}).reasoningAgent(context.Background(), "v1:identity:user:owner")
	if err != nil || id != agent.Id {
		t.Fatalf("compile's reasoningAgent = %q, %v; want %q", id, err, agent.Id)
	}
}
