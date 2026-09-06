// Package agents implements the `agent(name, prompt, partitionId)`
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
// AgentRegistry, opens a v1:work:goal naming the `invokeAgent` template
// with the agent and the prompt as its input, and returns {goalId, runId}
// immediately. The run opens at `running`, its graph event reaches every
// agent replica, exactly one claims it, and that one executes the turn
// (memql#5048 / memql#5054).
//
// It used to mint a v1:planner:plan in `queued` and rely on the planner's
// orchestration loop to notice. The comment that stood here recorded, in its
// own words, that the dispatch side would land "in a follow-up commit" and
// that "for now the Plan sits in queued state until that wiring lands".
//
// DSL callers consume the ids by:
//
//   - Fire-and-forget (automation case): ignore the return.
//   - Subscribe to graph.node.updated.v1:work:run filtered by
//     id==<runId> for lifecycle progression.
//   - Direct query via workRunsForGoal to read current state + outcome.
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
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// Integration owns the AgentRegistry + Engine pointers and implements
// memql.IntegrationProvider. Constructed by the plug-in factory in
// plugin.go from PluginContext.Agents + .Engine.
//
// The engine handle lets the handler call createPlan via the
// engine's Execute path -- the canonical write path. No AI / LLM
// providers are needed at this layer anymore: dispatch (the actual
// agent-tool-loop work) is owned by the planner integration's
// agent-loop subscriber, which is the only side that needs provider
// access.
type Integration struct {
	agents *memql.AgentRegistry
	engine memql.IntegrationEngineAccess
	// turnSeam carries the agent runtime, which exists only on an
	// agent-tagged build (memql#5048). See agent_turn.go.
	turnSeam

	// workGoals opens a work goal. See goals.go -- this package cannot write
	// the @serverOnly work rows itself.
	mu        sync.RWMutex
	workGoals WorkGoals
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
			Description: "Async-invoke a DSL-registered agent. Opens a v1:work:goal naming the invokeAgent template and returns {goalId, runId}. The run dispatcher claims the run on an agent node and runs the turn.",
			Handler:     i.handleInvoke,
			ArgsSchema: map[string]string{
				"name":        "string",
				"prompt":      "string",
				"partitionId": "string",
			},
		},
		{
			// memql#5048: the work spine's way to invoke an agent, with no
			// Plan in the path. See agent_turn.go for why it dispatches
			// locally rather than forwarding.
			Name:        "runAgentTurn",
			Description: "Run one agent turn and return its reply. SYNCHRONOUS: the asynchrony belongs to the work run that calls it, which is already detached. Requires an agent runtime, so it answers only on an agent node. Returns {agentId, requestId, reply}.",
			Handler:     i.handleRunAgentTurn,
			ArgsSchema: map[string]string{
				"agentId": "string (required) -- v1:agents:agent.id to run",
				"prompt":  "string (required) -- the user-role turn to send",
				"scopeId": "string -- optional scope the turn runs in",
			},
		},
		{
			Name:        ensureForGoalCapName,
			Description: "Match-extend-or-create an agent that can handle a goal. Reads the role + agent catalogs, runs the agentFactoryAnalyze structured-output prompt, issues the appropriate write, and returns {agentId, action, reasoning}. Restricted to the Assistant via the wrapping ensureAgent tool's @allowedRoles.",
			Handler:     i.handleEnsureForGoal,
			ArgsSchema: map[string]string{
				"goal":        "string",
				"ownerUserId": "string",
				"partitionId": "string",
			},
		},
		{
			Name:        "askSpecialist",
			Description: "Synchronously query a specialist agent by role. Resolves the specialist by roleSlug, invokes the askSpecialist structured-output prompt with the specialist's persona + the assistant's query, and returns ONE JSON object {response, rationale?, confidence, needsMore?}. Specialists never write utterances; the assistant paraphrases the response into the human-facing reply.",
			Handler:     i.handleAskSpecialist,
			ArgsSchema: map[string]string{
				"role":        "string -- specialist roleSlug (e.g. accounting-finance, human-resources)",
				"query":       "string -- what the assistant wants the specialist to answer",
				"context":     "object (optional) -- conversation context attached by the assistant",
				"ownerUserId": "string (required) -- whose specialists to resolve against (auto-stamped; @autoInjected, so an LLM-supplied value is dropped)",
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
				"partitionId": "string (optional) -- target space (auto-stamped)",
			},
		},
		{
			Name:        "produceArtifact",
			Description: "Delegate a produce-an-artifact request. Opens a v1:work:goal naming the produceArtifact template, carrying the goal + desired format/filename, and returns {status:\"delegated\", goalId, runId, ack}. The run executes on an agent node; the deliverable lands in the Library asynchronously.",
			Handler:     i.handleProduceArtifact,
			ArgsSchema: map[string]string{
				"goal":        "string (required) -- the deliverable to produce, phrased as a concrete artifact",
				"format":      "string (optional) -- output format; defaults to markdown",
				"filename":    "string (optional) -- preferred filename",
				"agentId":     "string (optional) -- calling agent id (auto-stamped)",
				"ownerUserId": "string (required) -- session-owner user id (auto-stamped); becomes the plan's requestedBy",
				"partitionId": "string (required) -- space the plan lives in (auto-stamped)",
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
//	args["name"]    string  required -- agent registry key (typically the roleSlug)
//	args["prompt"]  string  required -- what you want the agent to do
//	args["partitionId"] string  required -- the space the Plan lives in
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
	partitionId, _ := args["partitionId"].(string)
	if partitionId == "" {
		return nil, fmt.Errorf("agent(%q): 'partitionId' argument is required (use the system placeholder if no space context)", name)
	}

	// Same owner dimension as askSpecialist (memql#3216). `agent` already
	// declared ownerUserId in its arg schema; it just was not part of the
	// lookup. Unlike askSpecialist this one TOLERATES an empty owner: the
	// planner dispatches platform agents with no user behind the call, and an
	// empty owner resolves the shared catalog and nothing else -- never
	// another user's bucket.
	ownerUserId, _ := args["ownerUserId"].(string)
	def, ok := i.agents.Get(strings.TrimSpace(ownerUserId), name)
	if !ok || def == nil {
		return nil, fmt.Errorf("agent(%q): no agent registered with that name (loaded names: %v)", name, i.agents.NamesFor(ownerUserId))
	}

	goals := i.workGoalsRef()
	if goals == nil {
		// REFUSED rather than acked. This handler's whole job is to get work
		// STARTED, and the Plan path it replaces spent its life doing the
		// opposite -- returning a plan id for work nothing executed. An ack
		// for a goal that was never opened is that failure with a new name.
		return nil, fmt.Errorf("agent(%q): no work-goal surface on this node, so the invocation cannot be started", name)
	}

	owner := resolveGoalOwner(ownerUserId, def.OwnerUserId)
	goalId, runId, err := goals.OpenDirectGoal(ctx, DirectGoal{
		OwnerUserId:    owner,
		Statement:      prompt,
		AutomationName: templateInvokeAgent,
		Input: map[string]any{
			"agentId": def.Id,
			"prompt":  prompt,
		},
		RequestedVia: "agent",
		TriggeredBy:  "agent.dsl",
	})
	if err != nil {
		return nil, fmt.Errorf("agent(%q): open goal: %w", name, err)
	}

	payload := map[string]any{
		"goalId": goalId,
		"runId":  runId,
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

	// The owner dimension (memql#3216). Server-stamped and @autoInjected, so
	// the model cannot supply it: the tool schema marks it auto-injected,
	// ExecuteTool's applyToolDefaults deletes whatever arrived under that name,
	// and the agent runtime's per-call defaults put the real value back
	// (memql#3237 -- that delivery is why this issue was blocked on it).
	//
	// FAIL CLOSED on an empty owner rather than resolving the shared catalog.
	// An empty value here does not mean "a platform call with no user"; it
	// means the runtime could not establish who is asking, and the thing being
	// handed back is another agent's Description and SystemPrompt verbatim.
	// Refusing is a visible configuration failure; falling through would be an
	// invisible cross-tenant one.
	ownerUserId, _ := args["ownerUserId"].(string)
	ownerUserId = strings.TrimSpace(ownerUserId)
	if ownerUserId == "" {
		return nil, fmt.Errorf("askSpecialist(%q): no owner in the call context -- specialists resolve per owner and this call cannot say whose", role)
	}

	def, ok := i.agents.Get(ownerUserId, role)
	if !ok || def == nil {
		// NamesFor, not a global list: this message reaches the model, and a
		// global one would answer "which specialists do other users have" to
		// anyone who can provoke a miss.
		return nil, fmt.Errorf("askSpecialist(%q): no agent registered with that role (loaded: %v)", role, i.agents.NamesFor(ownerUserId))
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
		`mutation requestPlanFeedback(planId:%s, feedbackRequest:%s)`,
		langparser.QuoteString(planId), string(fbReqJSON),
	)
	if _, err := i.engine.Execute(mutationCtx, q); err != nil {
		return nil, fmt.Errorf("requestUserFeedback: requestPlanFeedback failed: %w", err)
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

// handleProduceArtifact delegates a "produce an artifact" request to the work
// spine. It opens a v1:work:goal naming the produceArtifact template, carrying
// the goal + desired format/filename as its input, and returns a small ack
// {status:"delegated", goalId, runId, message} so the Assistant acks and ends
// its turn while the deliverable is produced and landed in the Library
// asynchronously. Mirrors the agent() handler + the requestUserFeedback ack
// shape.
//
// The ack SHAPE is deliberately unchanged apart from the ids -- memql#5048's
// acceptance names it, because the Assistant's prompt reads `message` and a
// reworded ack changes what a model says to a person.
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
	partitionId := strings.TrimSpace(asString(args["partitionId"]))
	if partitionId == "" {
		return nil, fmt.Errorf("produceArtifact: 'partitionId' required (auto-injection failed -- no space in the turn context)")
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

	goals := i.workGoalsRef()
	if goals == nil {
		return nil, fmt.Errorf("produceArtifact: no work-goal surface on this node, so the deliverable cannot be started")
	}
	if ownerUserId == "" {
		// The Plan path defaulted this and the deliverable still landed --
		// under nobody. Refused here: a goal, its run and every observation of
		// it are owner-scoped, so an unowned deliverable is one the requester
		// cannot see in their own Library.
		return nil, fmt.Errorf("produceArtifact: no owning user resolved; the deliverable would be produced for nobody")
	}

	input := map[string]any{
		"agentId": agentId,
		"goal":    goal,
		"format":  format,
	}
	if filename != "" {
		input["filename"] = filename
	}

	goalId, runId, err := goals.OpenDirectGoal(ctx, DirectGoal{
		OwnerUserId:    ownerUserId,
		Statement:      goal,
		AutomationName: templateProduceArtifact,
		Input:          input,
		RequestedVia:   "agent",
		TriggeredBy:    "user.implicit",
	})
	if err != nil {
		return nil, fmt.Errorf("produceArtifact: open goal: %w", err)
	}

	payload, err := json.Marshal(map[string]any{
		"status":  "delegated",
		"goalId":  goalId,
		"runId":   runId,
		"format":  format,
		"message": "Delegated; the deliverable will be produced asynchronously and land in the Library. Reply with a SHORT acknowledgement (e.g. naming what you're making and that it'll be in their Library) and END YOUR TURN -- do NOT author the deliverable yourself.",
	})
	if err != nil {
		return nil, fmt.Errorf("produceArtifact: marshal ack: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("produceArtifact-envelope:%s:%d", runId, time.Now().UnixNano()),
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
	return auth.ContextWithUserActor(ctx, ownerUserId)
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
