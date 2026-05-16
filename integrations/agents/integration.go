// Package agents implements the `agent(name, prompt, spaceId)`
// builtin's executor -- the runtime side of the agents-as-DSL-primitive
// feature. Pairs with:
//
//   - dsl/_reference/_agent.memql      syntax reference
//   - component/memql/agents.go        AgentDefinition + AgentRegistry
//   - dsl/agents/builtins.memql        builtin declaration that points
//                                      at this integration via @executor
//
// Contract (async):
//
// The handler resolves the requested agent name against the
// AgentRegistry, mints a v1:planner:plan row with kind="agentInvocation"
// in `queued` status, and returns the planId immediately. The
// dispatch side -- actually running the agent's tool loop -- is owned
// by the planner integration's agent loop (which subscribes to
// graph.node.created.*.v1:planner:plan and handles the agentInvocation
// kind in a follow-up commit). For now the Plan sits in queued state
// until that wiring lands; the contract this builtin exposes is
// already correct so callers can write against it.
//
// DSL callers consume the planId by:
//
//   - Fire-and-forget (automation case): ignore the return.
//   - Subscribe to graph.node.updated.*.v1:planner:plan filtered by
//     id==<planId> for lifecycle progression.
//   - Direct query via queryPlanById to read current state + output.
//
// For blocking AI work from DSL, use `si("promptName", args)` -- the
// synchronous structured-output path. `agent()` is for agent-
// orchestrated, planner-tracked work; `si()` is for one-shot LLM calls.
package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	memorynodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	"github.com/visionarys-io/memql/component/memql"
)

// Integration owns the AgentRegistry + Engine pointers and implements
// memql.IntegrationProvider. Constructed by the plug-in factory in
// plugin.go from PluginContext.Agents + .Engine.
//
// The engine handle lets the handler call mutationCreatePlan via the
// engine's Execute path -- the canonical write path. No SI / LLM
// providers are needed at this layer anymore: dispatch (the actual
// agent-tool-loop work) is owned by the planner integration's
// agent-loop subscriber, which is the only side that needs provider
// access.
type Integration struct {
	agents *memql.AgentRegistry
	engine memql.IntegrationEngineAccess
}

// New constructs the agents integration. Returns nil if the
// AgentRegistry is nil so the plug-in factory can skip registration
// cleanly when the engine hasn't built one yet (test contexts).
// engine may be nil -- the handler returns a plain error in that
// case rather than crashing.
func New(agents *memql.AgentRegistry, engine memql.IntegrationEngineAccess) *Integration {
	if agents == nil {
		return nil
	}
	return &Integration{agents: agents, engine: engine}
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "agents" }

// Capabilities implements memql.IntegrationProvider. Exposes two
// capabilities:
//
//   - `invoke`        -- the agent() builtin's async dispatch path.
//   - `ensureForGoal` -- the General Assistant's agent-factory tool
//                        backing builtin (see factory.go).
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "invoke",
			Description: "Async-invoke a DSL-registered agent. Mints a v1:planner:plan row with kind=agentInvocation in queued status and returns the planId. The planner agent loop picks the Plan up and drives the agent's tool loop.",
			Handler:     i.handleInvoke,
			ArgsSchema: map[string]string{
				"name":    "string",
				"prompt":  "string",
				"spaceId": "string",
			},
		},
		{
			Name:        ensureForGoalCapName,
			Description: "Match-extend-or-create an agent that can handle a goal. Reads the role + agent catalogs, runs the agentFactoryAnalyze structured-output prompt, issues the appropriate write, and returns {agentId, action, reasoning}. Restricted to the General Assistant via the wrapping ensureAgent tool's @allowedRoles.",
			Handler:     i.handleEnsureForGoal,
			ArgsSchema: map[string]string{
				"goal":        "string",
				"ownerUserId": "string",
				"spaceId":     "string",
			},
		},
	}
}

// envelopeConcept is the namespace used for the MemoryNode this
// handler returns. Distinct from any concept-row concept -- this is
// an in-flight integration result, never persisted to a row.
const envelopeConcept = "integration:agents:envelope"

// systemActorId stamps every envelope MemoryNode this handler emits.
// Mirrors the cognition integration's pattern for capability-emitted
// nodes -- the concrete value is informational; downstream consumers
// don't permission-check on it.
const systemActorId = "system:integration:agents"

// agentInvocationKind is the v1:planner:plan.kind the handler stamps.
// The planner integration's agent loop dispatches on this value.
const agentInvocationKind = "agentInvocation"

// handleInvoke is the DSL-callable executor.
//
//	args["name"]    string  required -- agent registry key (typically the roleSlug)
//	args["prompt"]  string  required -- what you want the agent to do
//	args["spaceId"] string  required -- the space the Plan lives in
//
// Returns ONE MemoryNode whose payload JSON is
//
//	{planId, agent: {name, role, roleSlug}}
//
// The caller subscribes / queries to observe the Plan's progress.
func (i *Integration) handleInvoke(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.agents == nil {
		return nil, fmt.Errorf("agents integration not initialized -- AgentRegistry is nil")
	}
	if i.engine == nil {
		return nil, fmt.Errorf("agents integration not initialized -- engine handle missing")
	}

	name, _ := args["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("agent: 'name' argument is required")
	}
	prompt, _ := args["prompt"].(string)
	if prompt == "" {
		return nil, fmt.Errorf("agent(%q): 'prompt' argument is required", name)
	}
	spaceId, _ := args["spaceId"].(string)
	if spaceId == "" {
		return nil, fmt.Errorf("agent(%q): 'spaceId' argument is required (use the system placeholder if no space context)", name)
	}

	def, ok := i.agents.Get(name)
	if !ok || def == nil {
		return nil, fmt.Errorf("agent(%q): no agent registered with that name (loaded names: %v)", name, i.agents.Names())
	}

	planId, err := i.createInvocationPlan(ctx, def, prompt, spaceId)
	if err != nil {
		return nil, fmt.Errorf("agent(%q): create plan: %w", name, err)
	}

	payload := map[string]any{
		"planId": planId,
		"agent": map[string]any{
			"name":     def.Name,
			"role":     def.Role,
			"roleSlug": def.RoleSlug,
		},
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("agent(%q): marshal envelope: %w", name, err)
	}

	node := memorynodes.MemoryNode{
		ID:        fmt.Sprintf("agents-envelope:%s:%d", def.Name, time.Now().UnixNano()),
		Concept:   envelopeConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: systemActorId,
		Payload:   payloadBytes,
	}
	return []memorynodes.MemoryNode{node}, nil
}

// createInvocationPlan mints the v1:planner:plan row. The planner
// integration's agent loop will pick this Plan up on its
// graph.node.created subscription, dispatch the agent (using the
// agent's systemPrompt + the prompt arg as the user turn), and
// transition status queued -> running -> succeeded with output set
// to the agent's reply. Owner attribution lives on ownerAgentId.
//
// requestedBy/authorizedBy default to the system actor today; a
// future iteration will plumb the calling actor through so audit
// tracks the user-on-whose-behalf the agent ran.
func (i *Integration) createInvocationPlan(ctx context.Context, def *memql.AgentDefinition, prompt, spaceId string) (string, error) {
	planId := uuid.New().String()
	// Build the input object the planner agent loop reads when
	// dispatching: agentName + prompt are the essential signal;
	// agentRoleSlug is included so the dispatcher doesn't have to
	// re-resolve the registry entry.
	input := map[string]any{
		"kind":          agentInvocationKind,
		"agentName":     def.Name,
		"agentRoleSlug": def.RoleSlug,
		"prompt":        prompt,
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("marshal Plan.input: %w", err)
	}

	query := fmt.Sprintf(
		`mutationCreatePlan({planId: %q, spaceId: %q, kind: %q, goal: %q, requestedBy: %q, triggerSource: "agent.dsl", authorizedBy: %q, input: %s})`,
		planId, spaceId, agentInvocationKind, prompt, systemActorId, systemActorId, string(inputJSON),
	)
	if _, err := i.engine.Execute(ctx, query); err != nil {
		return "", fmt.Errorf("execute mutationCreatePlan: %w", err)
	}
	return planId, nil
}
