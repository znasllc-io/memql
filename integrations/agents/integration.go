// Package agents implements the `agent(name, args)` builtin's
// executor -- the runtime side of the agents-as-DSL-primitive
// feature. Pairs with:
//
//   - dsl/_reference/_agent.memql      syntax reference
//   - component/memql/agent_parser.go  parser (Phase 1)
//   - component/memql/agents.go        AgentDefinition + AgentRegistry (Phase 2)
//   - component/memql/unified_kinds_loader.go
//                                      LoadUnifiedAgents (Phase 3)
//   - dsl/agents/builtins.memql        builtin declaration that points at
//                                      this integration via @executor
//
// The handler resolves the requested agent name against the
// AgentRegistry the engine populated at startup. Phase 4 lands the
// resolution + envelope SHAPE (so DSL callers can wire against the
// builtin and tests can fixture it); the SI dispatch -- actually
// invoking the agent's LLM with its systemPrompt + the caller's
// utterance -- is a follow-up commit that swaps the placeholder
// envelope with the real cognition / agent-node round-trip.
package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	memorynodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	"github.com/visionarys-io/memql/component/memql"
)

// Integration owns the AgentRegistry pointer + the engine handle and
// implements memql.IntegrationProvider. Constructed by the plug-in
// factory in plugin.go from PluginContext.Agents + PluginContext.Engine.
type Integration struct {
	agents *memql.AgentRegistry
	engine memql.IntegrationEngineAccess
}

// New constructs the agents integration. Returns nil if either input
// is nil so the plug-in factory can skip registration cleanly when
// the engine hasn't built an AgentRegistry yet (test contexts, etc.).
func New(agents *memql.AgentRegistry, engine memql.IntegrationEngineAccess) *Integration {
	if agents == nil {
		return nil
	}
	return &Integration{agents: agents, engine: engine}
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "agents" }

// Capabilities implements memql.IntegrationProvider. Exposes one
// capability today: `invoke` -- callable from the DSL as
// `agent(name=..., utterance=..., spaceContext=..., history=...)`.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "invoke",
			Description: "Invoke a DSL-registered agent. Resolves the agent name in the AgentRegistry, runs the agent's reply turn (Phase 5+: tool-loop + respondToUser envelope; Phase 4 stub: returns a placeholder envelope), and returns {response, citations}.",
			Handler:     i.handleInvoke,
			ArgsSchema: map[string]string{
				"name":         "string",
				"utterance":    "string",
				"spaceContext": "object",
				"history":      "array",
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

// handleInvoke is the DSL-callable executor.
//
//	args["name"]         string   required -- agent registry key
//	args["utterance"]    string   required -- user's text
//	args["spaceContext"] object   optional -- forwarded to the agent
//	args["history"]      []object optional -- prior turns for context
//
// Returns ONE MemoryNode whose payload JSON is
// {response, citations[], agent: {name, role, roleSlug, scope}}.
//
// Phase 4 placeholder: the response text is a deterministic string
// that confirms the call resolved + the registry lookup succeeded.
// Phase 5 swaps the body with the real SI dispatch (agent's
// systemPrompt + LLM provider call + tool loop interception of
// respondToUser).
func (i *Integration) handleInvoke(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.agents == nil {
		return nil, fmt.Errorf("agents integration not initialized -- AgentRegistry is nil")
	}

	name, _ := args["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("agent: 'name' argument is required")
	}

	utterance, _ := args["utterance"].(string)
	if utterance == "" {
		return nil, fmt.Errorf("agent(%q): 'utterance' argument is required", name)
	}

	def, ok := i.agents.Get(name)
	if !ok || def == nil {
		return nil, fmt.Errorf("agent(%q): no agent registered with that name (loaded names: %v)", name, i.agents.Names())
	}

	// Phase 4 placeholder envelope. The actual SI dispatch lands in
	// the next commit; the contract below is what callers will
	// consume either way (response: string, citations: []object,
	// agent: {...metadata}).
	payload := map[string]any{
		"response":  fmt.Sprintf("[agents/invoke stub] agent %q (role=%q) acknowledged utterance: %s", def.Name, def.Role, utterance),
		"citations": []any{},
		"agent": map[string]any{
			"name":     def.Name,
			"role":     def.Role,
			"roleSlug": def.RoleSlug,
			"scope":    string(def.Scope),
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
