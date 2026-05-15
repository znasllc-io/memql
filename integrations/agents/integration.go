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
	"github.com/visionarys-io/memql/core/common"
)

// Integration owns the AgentRegistry + ProviderRegistry pointers and
// implements memql.IntegrationProvider. Constructed by the plug-in
// factory in plugin.go from PluginContext.Agents + .Providers.
//
// The handler dispatches an LLM call directly via ProviderRegistry's
// ChatProvider lookup -- one-shot, non-streaming, non-tool-calling.
// Future iterations swap this for the full streaming tool loop
// (the same path cognition's ForwardTurn uses), but the one-shot path
// is enough to make `agent(...)` produce real LLM-generated text
// from the agent's systemPrompt + the caller's utterance.
type Integration struct {
	agents    *memql.AgentRegistry
	providers *memql.ProviderRegistry
}

// New constructs the agents integration. Returns nil if the
// AgentRegistry is nil so the plug-in factory can skip registration
// cleanly when the engine hasn't built one yet (test contexts).
// providers may be nil -- the handler falls back to a stub envelope
// in that case so unit tests / startup smoke don't need a live
// provider registry to exercise the registry-lookup path.
func New(agents *memql.AgentRegistry, providers *memql.ProviderRegistry) *Integration {
	if agents == nil {
		return nil
	}
	return &Integration{agents: agents, providers: providers}
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

	response, err := i.dispatch(ctx, def, utterance)
	if err != nil {
		return nil, fmt.Errorf("agent(%q): dispatch: %w", name, err)
	}

	// Envelope contract: { response: string, citations: []object,
	// agent: {name, role, roleSlug, scope} }. The tool-loop +
	// respondToUser-interception path the cognition dispatcher uses
	// produces citations; this one-shot path doesn't have a tool
	// loop yet, so citations stays empty here. Future commit adds
	// the tool loop (mirroring the streaming dispatch in
	// integrations/agent/streaming.go).
	payload := map[string]any{
		"response":  response,
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

// dispatch turns an agent definition + a caller's utterance into the
// agent's reply text. Today: one-shot synchronous chat completion
// (system=agent.SystemPrompt, user=utterance) via the SI provider
// registry. Tomorrow: full streaming tool loop with respondToUser
// envelope interception, matching the cognition ForwardTurn path.
//
// When no ProviderRegistry was supplied to the integration (typical
// for unit-test fixtures that construct a bare engine without
// providers loaded), dispatch falls back to a deterministic stub
// string so the rest of the envelope pipeline can still be exercised.
func (i *Integration) dispatch(ctx context.Context, def *memql.AgentDefinition, utterance string) (string, error) {
	if i.providers == nil {
		return fmt.Sprintf("[agents/invoke stub: no provider registry] agent %q (role=%q) acknowledged utterance: %s", def.Name, def.Role, utterance), nil
	}

	// Provider selection precedence matches the agent concept's:
	//   providerConfig.llm.provider (explicit)
	//   providerConfig.llm.policyName (router policy resolution; today
	//                                  we treat policyName as a provider
	//                                  name fallback -- the router-aware
	//                                  resolution is a follow-up.)
	//   else -- registry default.
	providerName := def.LLMProvider
	if providerName == "" {
		providerName = def.LLMPolicyName
	}
	chat := i.providers.ChatProvider(providerName)
	if chat == nil {
		return "", fmt.Errorf("no chat provider available (tried %q, then default)", providerName)
	}

	systemPrompt := def.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = fmt.Sprintf("You are %s. %s", def.DisplayName, def.Personality)
	}

	messages := []common.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: utterance},
	}
	return chat.CallChat(ctx, messages)
}
