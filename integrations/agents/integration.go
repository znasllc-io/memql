// Package agents implements the `agent(name, prompt, spaceId)`
// builtin's executor -- the runtime side of the agents-as-DSL-primitive
// feature. Pairs with:
//
//   - dsl/_reference/_agent.memql      syntax reference
//   - component/memql/agents.go        AgentDefinition + AgentRegistry
//   - dsl/agents/builtins.memql        builtin declaration that points
//     at this integration via @executor
//
// Contract (async):
//
// The handler resolves the requested agent name against the
// AgentRegistry, mints a v1:planner:plan row with kind="agentInvocation"
// in `queued` status, and returns the planId immediately. The
// dispatch side -- actually running the agent's tool loop -- is owned
// by the planner integration's agent loop (which subscribes to
// graph.node.created.v1:planner:plan and handles the agentInvocation
// kind in a follow-up commit). For now the Plan sits in queued state
// until that wiring lands; the contract this builtin exposes is
// already correct so callers can write against it.
//
// DSL callers consume the planId by:
//
//   - Fire-and-forget (automation case): ignore the return.
//   - Subscribe to graph.node.updated.v1:planner:plan filtered by
//     id==<planId> for lifecycle progression.
//   - Direct query via queryPlanById to read current state + output.
//
// For blocking AI work from DSL, use `ai("promptName", args)` -- the
// synchronous structured-output path. `agent()` is for agent-
// orchestrated, planner-tracked work; `ai()` is for one-shot LLM calls.
package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// Integration owns the AgentRegistry + Engine pointers and implements
// memql.IntegrationProvider. Constructed by the plug-in factory in
// plugin.go from PluginContext.Agents + .Engine.
//
// The engine handle lets the handler call mutationCreatePlan via the
// engine's Execute path -- the canonical write path. No AI / LLM
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
//   - `ensureForGoal` -- the Assistant's agent-factory tool
//     backing builtin (see factory.go).
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
			Description: "Match-extend-or-create an agent that can handle a goal. Reads the role + agent catalogs, runs the agentFactoryAnalyze structured-output prompt, issues the appropriate write, and returns {agentId, action, reasoning}. Restricted to the Assistant via the wrapping ensureAgent tool's @allowedRoles.",
			Handler:     i.handleEnsureForGoal,
			ArgsSchema: map[string]string{
				"goal":        "string",
				"ownerUserId": "string",
				"spaceId":     "string",
			},
		},
		{
			Name:        "askSpecialist",
			Description: "Synchronously query a specialist agent by role. Resolves the specialist by roleSlug, invokes the askSpecialist structured-output prompt with the specialist's persona + the assistant's query, and returns ONE JSON object {response, rationale?, confidence, needsMore?}. Specialists never write utterances; the assistant paraphrases the response into the human-facing reply.",
			Handler:     i.handleAskSpecialist,
			ArgsSchema: map[string]string{
				"role":    "string -- specialist roleSlug (e.g. accounting-finance, human-resources)",
				"query":   "string -- what the assistant wants the specialist to answer",
				"context": "object (optional) -- conversation context attached by the assistant",
			},
		},
		{
			Name:        "requestUserFeedback",
			Description: "Transition the active Plan to awaitingFeedback / feedback_required with a feedbackRequest{question, kind, options?, timeoutAt}. The agent calls this BEFORE guessing when it (or a specialist it fronts for) needs missing detail from the user. The user's answer (Plan.feedbackResponse + status->running) resumes the Plan through the existing planner re-invocation path.",
			Handler:     i.handleRequestUserFeedback,
			ArgsSchema: map[string]string{
				"question":    "string (required) -- the question to put to the user",
				"kind":        "string (required) -- choice / text / multi response widget",
				"options":     "array (optional) -- [{label, value}] for choice / multi",
				"timeoutAt":   "string (optional) -- RFC3339 auto-pause deadline",
				"agentId":     "string (required) -- calling agent id (auto-stamped)",
				"ownerUserId": "string (required) -- session-owner user id (auto-stamped)",
				"planId":      "string (required) -- the active Plan to park (auto-stamped)",
				"spaceId":     "string (optional) -- target space (auto-stamped)",
			},
		},
		{
			Name:        "produceArtifact",
			Description: "Delegate a produce-an-artifact request to the planner. Mints a v1:planner:plan (kind=produceArtifact, triggerSource=user.implicit) carrying the goal + desired format/filename, and returns {status:\"delegated\", planId, ack}. The plan auto-runs; the deliverable lands in the Library asynchronously.",
			Handler:     i.handleProduceArtifact,
			ArgsSchema: map[string]string{
				"goal":        "string (required) -- the deliverable to produce, phrased as a concrete artifact",
				"format":      "string (optional) -- output format; defaults to markdown",
				"filename":    "string (optional) -- preferred filename",
				"agentId":     "string (optional) -- calling agent id (auto-stamped)",
				"ownerUserId": "string (required) -- session-owner user id (auto-stamped); becomes the plan's requestedBy",
				"spaceId":     "string (required) -- space the plan lives in (auto-stamped)",
			},
		},
	}
}

// produceArtifactKind is the v1:planner:plan.kind the produceArtifact
// handler stamps. The planner agent loop does NOT decompose this kind: it
// starts the plan directly and dispatches a single agent turn to the plan's
// ownerAgentId (the requesting assistant), avoiding the heavyweight
// plannerAgent decompose loop that spammed the LLM into a 429 (memql#816).
const produceArtifactKind = "produceArtifact"

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

// askSpecialistResponseSchema enforces the specialist's JSON output
// contract. Specialists answer the assistant in this shape; the
// assistant paraphrases `response` into the human-facing reply.
var askSpecialistResponseSchema = json.RawMessage(`{
  "type": "object",
  "required": ["response", "confidence"],
  "additionalProperties": false,
  "properties": {
    "response":   {"type": "string"},
    "rationale":  {"type": "string"},
    "confidence": {"type": "number", "minimum": 0, "maximum": 1},
    "needsMore":  {"type": "array", "items": {"type": "string"}}
  }
}`)

// handleAskSpecialist is the synchronous specialist-query executor.
//
//	args["role"]    string  required -- specialist roleSlug
//	args["query"]   string  required -- the assistant's question
//	args["context"] object  optional -- conversation context
//
// Returns ONE MemoryNode whose payload is the specialist's structured
// JSON response. The assistant's tool loop reads `response` and
// paraphrases it into the human reply.
func (i *Integration) handleAskSpecialist(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.agents == nil {
		return nil, fmt.Errorf("askSpecialist: agents integration not initialized")
	}
	if i.engine == nil {
		return nil, fmt.Errorf("askSpecialist: engine handle missing")
	}

	role, _ := args["role"].(string)
	if role == "" {
		return nil, fmt.Errorf("askSpecialist: 'role' argument is required (specialist roleSlug)")
	}
	query, _ := args["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("askSpecialist(%q): 'query' argument is required", role)
	}
	contextArg, _ := args["context"].(map[string]any)

	def, ok := i.agents.Get(role)
	if !ok || def == nil {
		return nil, fmt.Errorf("askSpecialist(%q): no agent registered with that role (loaded: %v)", role, i.agents.Names())
	}
	if def.Role != "specialist" {
		return nil, fmt.Errorf("askSpecialist(%q): target agent has role=%q, not specialist", role, def.Role)
	}
	// Defense in depth: system-kind agents (MemQL Planner, MemQL
	// Trainer) carry role="specialist" but are platform infrastructure
	// invoked by the planner service, not by the assistant's tool loop.
	// They must never be reachable via askSpecialist even if the
	// assistant hallucinates a system roleSlug into its tool call.
	if def.Kind == "system" {
		return nil, fmt.Errorf("askSpecialist(%q): target is a platform-infrastructure agent (kind=system); askSpecialist is for user-facing specialists only", role)
	}

	data := map[string]any{
		"specialistName":         def.Name,
		"specialistDescription":  def.Description,
		"specialistSystemPrompt": def.SystemPrompt,
		"query":                  query,
	}
	if contextArg != nil {
		data["context"] = contextArg
	}

	raw, err := i.engine.InvokeAIStructured(
		ctx,
		"askSpecialist",
		data,
		"askSpecialistResponse",
		askSpecialistResponseSchema,
		true,
	)
	if err != nil {
		return nil, fmt.Errorf("askSpecialist(%q): invoke prompt: %w", role, err)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("askSpecialist(%q): parse response: %w (raw: %s)", role, err, raw)
	}

	envelope := map[string]any{
		"role":     role,
		"name":     def.Name,
		"response": parsed,
	}
	payloadBytes, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("askSpecialist(%q): marshal envelope: %w", role, err)
	}

	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("askSpecialist-envelope:%s:%d", role, time.Now().UnixNano()),
		Concept:   envelopeConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: systemActorId,
		Payload:   payloadBytes,
	}}, nil
}

// handleRequestUserFeedback transitions the active Plan to
// awaitingFeedback / feedback_required so the user is asked for the
// detail the agent is missing. The generic counterpart to the worker
// integration's handleRequestScope (scope_elevation_required); this is
// the feedback_required variant, agent-callable mid-turn.
//
//	args["question"]    string  required -- what to ask the user
//	args["kind"]        string  required -- choice / text / multi
//	args["options"]     array   optional -- [{label, value}] for choice / multi
//	args["timeoutAt"]   string  optional -- RFC3339 auto-pause deadline
//	args["agentId"]     string  required -- auto-stamped by the streaming loop
//	args["ownerUserId"] string  required -- auto-stamped by the streaming loop
//	args["planId"]      string  required -- the active Plan (auto-stamped)
//
// Returns ONE MemoryNode whose payload is a small ack
// {status:"awaiting_user", planId, kind}. The agent reads it, emits a
// short respondToUser acknowledgement, and ends its turn; the user's
// answer is the gate.
func (i *Integration) handleRequestUserFeedback(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil {
		return nil, fmt.Errorf("requestUserFeedback: agents integration not initialized")
	}

	question := strings.TrimSpace(asString(args["question"]))
	if question == "" {
		return nil, fmt.Errorf("requestUserFeedback: 'question' is required")
	}
	kind := strings.TrimSpace(asString(args["kind"]))
	switch kind {
	case "choice", "text", "multi":
		// valid
	case "":
		return nil, fmt.Errorf("requestUserFeedback: 'kind' is required (choice / text / multi)")
	default:
		return nil, fmt.Errorf("requestUserFeedback: 'kind' must be choice / text / multi (got %q)", kind)
	}

	planId := strings.TrimSpace(asString(args["planId"]))
	if planId == "" {
		return nil, fmt.Errorf("requestUserFeedback: 'planId' required (auto-injection failed -- no active Plan in the turn context)")
	}
	ownerUserId := strings.TrimSpace(asString(args["ownerUserId"]))

	// Build the feedbackRequest object the canvas card renders. Mirrors
	// the planner agent loop's escalateAwaitingFeedback shape + the
	// concept doc on plan.feedbackRequest.
	fbReq := map[string]any{
		"question": question,
		"kind":     kind,
		"askedAt":  time.Now().UTC().Format(time.RFC3339),
	}
	if opts, ok := args["options"].([]any); ok && len(opts) > 0 {
		fbReq["options"] = opts
	}
	if timeoutAt := strings.TrimSpace(asString(args["timeoutAt"])); timeoutAt != "" {
		fbReq["timeoutAt"] = timeoutAt
	}
	fbReqJSON, err := json.Marshal(fbReq)
	if err != nil {
		return nil, fmt.Errorf("requestUserFeedback: marshal feedbackRequest: %w", err)
	}

	if i.engine == nil {
		return nil, fmt.Errorf("requestUserFeedback: engine handle missing")
	}

	// The mutation runs as the OWNING USER so the parked Plan's row
	// createdBy lands as the user (correct for ownership / audit). The
	// agent-tool dispatch path doesn't carry the user's JWT into the
	// per-tool context, so without this the update path would fail with
	// "no actor found in context". Same pattern the worker integration's
	// handleRequestScope uses for the scope-elevation mutation.
	mutationCtx := withUserActor(ctx, ownerUserId)

	q := fmt.Sprintf(
		`mutationRequestPlanFeedback({planId:%q, feedbackRequest:%s})`,
		planId, string(fbReqJSON),
	)
	if _, err := i.engine.Execute(mutationCtx, q); err != nil {
		return nil, fmt.Errorf("requestUserFeedback: mutationRequestPlanFeedback failed: %w", err)
	}

	payload, err := json.Marshal(map[string]any{
		"status":  "awaiting_user",
		"planId":  planId,
		"kind":    kind,
		"message": "Feedback request emitted; the Plan is now awaitingFeedback. Reply with a short acknowledgement and end your turn -- the user's answer resumes the Plan.",
	})
	if err != nil {
		return nil, fmt.Errorf("requestUserFeedback: marshal ack: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("requestUserFeedback-envelope:%s:%d", planId, time.Now().UnixNano()),
		Concept:   envelopeConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: systemActorId,
		Payload:   payload,
	}}, nil
}

// handleProduceArtifact delegates a "produce an artifact" request to the
// planner. It mints a v1:planner:plan (kind=produceArtifact,
// triggerSource=user.implicit) via the single mutationCreatePlan write path,
// carrying the goal + desired format/filename in Plan.input, and returns a
// small ack {status:"delegated", planId, ack}. The plan auto-runs (the planner
// agent loop auto-starts produceArtifact plans), so the deliverable is
// produced + landed in the Library asynchronously while the Assistant just
// acks and ends its turn. Mirrors createInvocationPlan + the
// requestUserFeedback ack shape.
func (i *Integration) handleProduceArtifact(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil {
		return nil, fmt.Errorf("produceArtifact: agents integration not initialized")
	}

	goal := strings.TrimSpace(asString(args["goal"]))
	if goal == "" {
		return nil, fmt.Errorf("produceArtifact: 'goal' is required -- describe the deliverable to produce")
	}
	ownerUserId := strings.TrimSpace(asString(args["ownerUserId"]))
	if ownerUserId == "" {
		return nil, fmt.Errorf("produceArtifact: 'ownerUserId' required (auto-injection failed -- no session owner in the turn context)")
	}
	spaceId := strings.TrimSpace(asString(args["spaceId"]))
	if spaceId == "" {
		return nil, fmt.Errorf("produceArtifact: 'spaceId' required (auto-injection failed -- no space in the turn context)")
	}
	// Default the output format to markdown unless the caller named one.
	format := strings.TrimSpace(asString(args["format"]))
	if format == "" {
		format = "markdown"
	}
	filename := strings.TrimSpace(asString(args["filename"]))
	// The calling assistant (auto-stamped agentId) becomes the plan's
	// ownerAgentId so the planner dispatches the production turn straight to it
	// -- NO planner-agent decompose loop (which spammed the LLM, memql#816).
	agentId := strings.TrimSpace(asString(args["agentId"]))

	planId := id.NewShortId()
	// Plan.input is the per-kind input shape the executing agent reads to
	// know WHAT to write + in what format. goal is also the Plan.goal, but
	// keep it in input too so a downstream consumer that only reads input
	// has the full picture.
	input := map[string]any{
		"kind":   produceArtifactKind,
		"goal":   goal,
		"format": format,
	}
	if filename != "" {
		input["filename"] = filename
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("produceArtifact: marshal Plan.input: %w", err)
	}

	if i.engine == nil {
		return nil, fmt.Errorf("produceArtifact: engine handle missing")
	}

	// requestedBy is the OWNING USER so the planner's specialist lookup
	// (queryActiveAgentsForUser{ownerUserId: requestedBy}) sees the user's
	// agents, and the produced artifact is owned by the user. The mutation
	// runs under a synthetic user actor for the same reason
	// requestUserFeedback does -- the per-tool context doesn't carry the
	// user's JWT.
	mutationCtx := withUserActor(ctx, ownerUserId)
	var qb strings.Builder
	fmt.Fprintf(&qb,
		`mutationCreatePlan({planId: %q, spaceId: %q, kind: %q, goal: %q, requestedBy: %q, triggerSource: "user.implicit", authorizedBy: %q, input: %s`,
		planId, spaceId, produceArtifactKind, goal, ownerUserId, ownerUserId, string(inputJSON),
	)
	if agentId != "" {
		// Dispatch the production turn straight to the requesting assistant
		// (no planner-agent decompose loop -- memql#816).
		fmt.Fprintf(&qb, `, ownerAgentId: %q`, agentId)
	}
	qb.WriteString(`})`)
	if _, err := i.engine.Execute(mutationCtx, qb.String()); err != nil {
		return nil, fmt.Errorf("produceArtifact: mutationCreatePlan failed: %w", err)
	}

	payload, err := json.Marshal(map[string]any{
		"status":  "delegated",
		"planId":  planId,
		"format":  format,
		"message": "Delegated to the planner; the deliverable will be produced asynchronously and land in the Library. Reply with a SHORT acknowledgement (e.g. naming what you're making and that it'll be in their Library) and END YOUR TURN -- do NOT author the deliverable yourself.",
	})
	if err != nil {
		return nil, fmt.Errorf("produceArtifact: marshal ack: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("produceArtifact-envelope:%s:%d", planId, time.Now().UnixNano()),
		Concept:   envelopeConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: systemActorId,
		Payload:   payload,
	}}, nil
}

// withUserActor stamps a synthetic user actor on the context so a
// mutation invoked from the agent tool-loop (which doesn't carry the
// user's JWT into the per-tool context) writes rows attributed to the
// owning user. Mirrors integrations/agent/worker/integration.go's
// withUserActor; no-op when ownerUserId is empty (the mutation then
// runs under whatever actor the inbound context already carries).
func withUserActor(ctx context.Context, ownerUserId string) context.Context {
	if strings.TrimSpace(ownerUserId) == "" {
		return ctx
	}
	claims := map[string]any{
		"sub":   ownerUserId,
		"email": ownerUserId,
		"role":  "user",
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	return auth.ContextWithToken(ctx, token)
}

// asString coerces an arg value to a trimmed-friendly string, returning
// "" for nil / non-string values. Callers TrimSpace as needed.
func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
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
	planId := id.NewShortId()
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
