// agent_loop.go
//
// The Planner Agent dispatch loop (Phase 4 of the planner-redesign work).
//
// On each v1:planner:plan transition that needs the Planner Agent's next
// decision, the loop:
//
//   1. Loads the current Plan + all child Tasks.
//   2. Invokes the plannerAgent prompt (system prompt at
//      dsl/agents/prompts/plannerAgent.tmpl, paired with a strong-reasoning
//      provider) with the current state.
//   3. Parses the agent's structured-output decision.
//   4. Dispatches the decision against the engine (write Plan.phases on
//      'decompose', insert Task on 'dispatchTask', mark Plan terminal on
//      'markPlanSucceeded' / 'markPlanFailed', etc.).
//
// Phase 4 scope: scaffolding + the simplest decisions (decompose, succeed,
// fail) wired end-to-end. The richer actions (createSpecialist /
// extendSpecialist / spawnTrainingPlan / escalate-with-feedback) land in
// the follow-up phases that ship the supporting plumbing -- the dedupe
// pipeline (Phase 7), the Trainer's web-search tools (Phase 6), the
// approval-card UX. Until those phases land, the Planner Agent's
// structured output may emit those actions; the loop logs them and
// transitions the Plan to awaitingFeedback so the user is in the loop.
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/znasllc-io/memql/component/events"
)

// PlannerAgentLoop owns the structured-output cycle that drives every
// non-trivial Plan. The loop is event-driven -- subscribed in Start()
// alongside the existing scope-elevation path.
type PlannerAgentLoop struct {
	engine Engine
	logger *slog.Logger
}

// NewPlannerAgentLoop constructs a loop pinned to the planner integration's
// engine adapter and logger.
func NewPlannerAgentLoop(engine Engine, logger *slog.Logger) *PlannerAgentLoop {
	if logger == nil {
		logger = slog.Default()
	}
	return &PlannerAgentLoop{engine: engine, logger: logger}
}

// HandlePlanCreated is the event-bus subscriber for
// graph.node.created.*.v1:planner:plan. Fires the first Planner Agent
// invocation for a brand-new Plan: typically returns a 'decompose'
// decision that stamps the Plan.phases outline.
//
// Ad-hoc Plans (kind='adHocAction', created by the taskstamp Stamper
// from chat-driven tool calls) are skipped -- they don't need
// decomposition because the wrapping semantic Task and child tool-call
// Tasks are already materialized by the stamper at first call.
//
// Plans created with kind='scopeElevation' continue to flow through the
// existing handlePlanApprovedForExecution path; the agent loop doesn't
// re-claim them.
func (l *PlannerAgentLoop) HandlePlanCreated(ev events.Event) {
	planId, kind, status, ok := extractPlanFields(ev)
	if !ok {
		return
	}
	if kind == "adHocAction" || kind == "scopeElevation" || kind == "agentInvocation" {
		// adHocAction: stamper handles end-to-end.
		// scopeElevation: existing handlePlanApprovedForExecution covers it.
		// agentInvocation: minted by `agent("name", prompt, spaceId)`
		//   builtin (integrations/agents). The agents integration owns
		//   dispatch for this kind; the Planner Agent does NOT try to
		//   decompose -- the caller named a specific agent.
		//   END-TO-END WIRING IS A FOLLOW-UP: today the Plan sits in
		//   queued until the agents integration subscribes to plan-
		//   created events (requires widening PluginContext to expose
		//   the EventBus). Documented in
		//   docs/planning/agent-role-catalog.md.
		return
	}
	if status != "queued" {
		// We only kick off on freshly-queued Plans. The loop's own
		// state transitions (queued -> routing -> running -> ...) are
		// driven by HandlePlanUpdated, not this path.
		return
	}
	l.logger.Info("planner agent loop: handling new plan",
		"planId", planId, "kind", kind)
	// Run dispatch in a fresh goroutine: the bus loop processes events
	// synchronously per-subscriber, and a Planner Agent invocation is a
	// multi-second LLM call. Pattern mirrors handlePlanApprovedForExecution.
	go func() {
		if err := l.invokeAndDispatch(context.Background(), planId); err != nil {
			l.logger.Warn("planner agent loop: invoke+dispatch failed",
				"planId", planId, "error", err)
		}
	}()
}

// HandlePlanUpdated is the event-bus subscriber for
// graph.node.updated.*.v1:planner:plan that complements
// handlePlanApprovedForExecution. Re-invokes the Planner Agent when
// the Plan transitions in a way that asks for the next decision (e.g.
// a Task under the Plan transitioned to succeeded/failed and the
// Planner needs to react).
//
// Phase 4 scope: log-only for now. The Task-completion -> Planner-
// re-invoke wiring lands once the Planner Agent's tool-call surface
// is in place (Phase 4b).
func (l *PlannerAgentLoop) HandlePlanUpdated(ev events.Event) {
	planId, kind, status, ok := extractPlanFields(ev)
	if !ok {
		return
	}
	if kind == "adHocAction" || kind == "scopeElevation" {
		return
	}
	l.logger.Debug("planner agent loop: plan updated (re-invoke deferred)",
		"planId", planId, "kind", kind, "status", status)
}

// maxPlannerIterations caps how many times we'll re-invoke the
// plannerAgent for a single Plan inside one event-driven cycle.
// Loop-style actions (decompose, createSpecialist, extendSpecialist)
// transition the Plan to a new state and then re-enter the loop so
// the LLM sees the updated context and emits the next decision.
// The cap stops a misbehaving model from spinning forever; in
// practice an end-to-end run is "decompose -> createSpecialist (maybe)
// -> dispatchTask" which is 2-3 iterations.
const maxPlannerIterations = 5

// invokeAndDispatch performs one or more loop iterations: load Plan
// state, call the plannerAgent prompt, parse the decision, dispatch.
// Loop-extending actions (decompose / createSpecialist) recurse into
// invokeAndDispatchIter so the same Plan gets the next decision in
// the same event-cycle instead of parking until a future trigger.
func (l *PlannerAgentLoop) invokeAndDispatch(ctx context.Context, planId string) error {
	return l.invokeAndDispatchIter(ctx, planId, 0)
}

func (l *PlannerAgentLoop) invokeAndDispatchIter(ctx context.Context, planId string, iter int) error {
	if iter >= maxPlannerIterations {
		// Park the plan rather than fail it -- the user can resume on
		// the next trigger. Keeps a stuck LLM from killing a Plan
		// outright when it might just need a fresh context.
		l.logger.Warn("planner agent loop: iteration cap reached; escalating",
			"planId", planId, "iter", iter)
		return l.escalateAwaitingFeedback(ctx, planId, "feedback_required",
			fmt.Sprintf("Planner reached the per-cycle iteration cap (%d) without converging. Resume to retry.", maxPlannerIterations))
	}

	plan, err := l.loadPlan(ctx, planId)
	if err != nil {
		return fmt.Errorf("load plan %s: %w", planId, err)
	}
	tasks, err := l.loadTasks(ctx, planId)
	if err != nil {
		l.logger.Warn("planner agent loop: load tasks failed; proceeding with empty list",
			"planId", planId, "error", err)
		tasks = nil
	}
	specialists, err := l.loadSpecialists(ctx, plan)
	if err != nil {
		l.logger.Warn("planner agent loop: load specialists failed; proceeding with empty list",
			"planId", planId, "error", err)
		specialists = nil
	}

	resp, err := l.engine.InvokeSI(ctx, "plannerAgent", map[string]any{
		"plan":        plan,
		"tasks":       tasks,
		"specialists": specialists,
		"partition":   getString(plan, "partition"),
		"now":         time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return l.markPlanFailed(ctx, planId, fmt.Sprintf("plannerAgent invocation failed: %v", err))
	}

	decision, err := parsePlannerDecision(resp)
	if err != nil {
		return l.markPlanFailed(ctx, planId, fmt.Sprintf("planner decision parse failed: %v", err))
	}

	l.logger.Info("planner agent loop: dispatching decision",
		"planId", planId, "action", decision.Action, "iter", iter)
	return l.dispatchDecision(ctx, planId, decision, iter)
}

// dispatchDecision routes the Planner Agent's structured-output
// decision to its corresponding engine action. Loop-extending actions
// (decompose / createSpecialist / extendSpecialist) re-invoke the
// planner with iter+1 so the LLM picks up where it left off in the
// same event cycle. Terminal actions (markPlanSucceeded / Failed /
// escalate) and dispatchTask park the Plan for the next external
// trigger.
func (l *PlannerAgentLoop) dispatchDecision(ctx context.Context, planId string, d plannerDecision, iter int) error {
	switch d.Action {
	case "decompose":
		if err := l.stampPhases(ctx, planId, d.PlanOutline); err != nil {
			return err
		}
		// Re-invoke so the planner picks the first concrete action
		// (createSpecialist / dispatchTask) given the freshly-stamped
		// phases. Without this the Plan would sit at queued with
		// phases set but no Tasks created.
		return l.invokeAndDispatchIter(ctx, planId, iter+1)
	case "dispatchTask":
		return l.insertDispatchedTask(ctx, planId, d.Task)
	case "markPlanSucceeded":
		return l.markPlanSucceeded(ctx, planId, d.Output)
	case "markPlanFailed":
		return l.markPlanFailed(ctx, planId, d.ErrorMessage)
	case "escalate":
		return l.escalateAwaitingFeedback(ctx, planId, d.FeedbackReason, d.Question)
	case "createSpecialist", "extendSpecialist":
		// The plannerAgent prompt distinguishes createSpecialist from
		// extendSpecialist (the latter prefers Q10's layered dedupe),
		// but both resolve to the same handler from our perspective:
		// the ensureAgentForGoal factory builtin reads the role + agent
		// catalogs and picks match-vs-extend-vs-create on its own. We
		// pass the Plan's goal verbatim; the factory does the matching.
		return l.ensureSpecialistForPlan(ctx, planId, iter)
	case "spawnTrainingPlan", "retry":
		// Still parked. spawnTrainingPlan minted a kind=trainSpecialist
		// child Plan; the Trainer Agent picks that up via its own loop
		// once it ships. retry handling needs the failure-context
		// surface that the agent worker emits, which isn't wired yet.
		l.logger.Info("planner agent loop: decision deferred; escalating",
			"planId", planId, "action", d.Action)
		return l.escalateAwaitingFeedback(ctx, planId, "feedback_required",
			fmt.Sprintf("Planner emitted '%s' which is not yet supported end-to-end. Manual intervention required.", d.Action))
	default:
		return l.markPlanFailed(ctx, planId,
			fmt.Sprintf("planner emitted unknown action %q", d.Action))
	}
}

// ensureSpecialistForPlan resolves the Plan's goal into an agent
// (match / extend / create) via the ensureAgentForGoal factory
// builtin, then re-invokes the planner so it can dispatch a Task to
// the resulting specialist on the next iteration.
//
// The factory itself is the integration.agents.ensureForGoal handler
// in integrations/agents/factory.go -- it reads the role catalog +
// the user's existing agents, runs the agentFactoryAnalyze
// structured-output prompt to pick a role / domains / tools, and
// writes via mutationCreateAgent or mutationUpdateAgent. Returns
// {agentId, action, reasoning} on a synthetic in-flight MemoryNode.
//
// We call it directly from Go rather than through the GA's
// ensureAgent tool because (a) the tool's @allowedRoles is
// "general_assistant" by design (it's a GA-only surface in chat),
// and (b) the planner integration has full engine access -- routing
// through the LLM's tool loop would add an unnecessary round trip
// when the planner has already decided this is the right action.
func (l *PlannerAgentLoop) ensureSpecialistForPlan(ctx context.Context, planId string, iter int) error {
	plan, err := l.loadPlan(ctx, planId)
	if err != nil {
		return fmt.Errorf("load plan %s: %w", planId, err)
	}
	goal := getString(plan, "goal")
	ownerUserId := getString(plan, "requestedBy")
	spaceId := getString(plan, "spaceId")
	if goal == "" || ownerUserId == "" {
		return l.markPlanFailed(ctx, planId,
			"createSpecialist: plan is missing goal or requestedBy; cannot run factory")
	}

	// MemQL function-call args take bare-identifier keys, not JSON
	// quoted keys. json.Marshal would produce quoted keys and the
	// parser would silently drop the args, so we build the call
	// literal by hand. %q quotes the string values per MemQL string
	// syntax.
	call := fmt.Sprintf(
		`ensureAgentForGoal({goal:%q, ownerUserId:%q, spaceId:%q})`,
		goal, ownerUserId, spaceId,
	)
	res, err := l.engine.Execute(ctx, call)
	if err != nil {
		return l.markPlanFailed(ctx, planId,
			fmt.Sprintf("ensureAgentForGoal failed: %v", err))
	}

	agentId, factoryAction := extractAgentFactoryResult(res)
	if agentId == "" {
		return l.markPlanFailed(ctx, planId,
			"ensureAgentForGoal returned no agentId; cannot proceed")
	}
	l.logger.Info("planner agent loop: factory resolved specialist",
		"planId", planId, "agentId", agentId, "factoryAction", factoryAction, "iter", iter)

	// Stamp the agent onto the Plan so the next loadSpecialists call
	// includes it in the LLM's candidate set, and so consumers querying
	// the Plan see the assignment.
	if err := l.assignOwnerAgent(ctx, planId, agentId); err != nil {
		l.logger.Warn("planner agent loop: ownerAgentId update failed; continuing",
			"planId", planId, "agentId", agentId, "error", err)
	}

	// Re-invoke so the planner picks dispatchTask now that the
	// specialist exists.
	return l.invokeAndDispatchIter(ctx, planId, iter+1)
}

// extractAgentFactoryResult digs the {agentId, action} fields out of
// the ensureAgentForGoal builtin's return. The engine wraps results
// in {bundle: {nodes: [...]}} envelopes (same shape queryPlanById
// returns); we route through plannerExtractRows to normalize to a
// flat row slice, then pluck the factory-specific fields off the
// first row's top-level or payload.
func extractAgentFactoryResult(res any) (agentId, action string) {
	rows := plannerExtractRows(res)
	if len(rows) == 0 {
		return "", ""
	}
	row := rows[0]
	if id, ok := row["agentId"].(string); ok {
		agentId = id
	}
	if a, ok := row["action"].(string); ok {
		action = a
	}
	if p, ok := row["payload"].(map[string]any); ok {
		if id, ok := p["agentId"].(string); ok && agentId == "" {
			agentId = id
		}
		if a, ok := p["action"].(string); ok && action == "" {
			action = a
		}
	}
	return agentId, action
}

// assignOwnerAgent writes a Plan-status update setting ownerAgentId
// and transitioning the Plan to routing. Mutation re-validates against
// the Plan concept; we just want the in-place merge of ownerAgentId.
// Bare-identifier keys per MemQL function-call syntax.
func (l *PlannerAgentLoop) assignOwnerAgent(ctx context.Context, planId, agentId string) error {
	q := fmt.Sprintf(
		`mutationUpdatePlanStatus({planId:%q, status:"routing", ownerAgentId:%q})`,
		planId, agentId,
	)
	_, err := l.engine.Execute(ctx, q)
	return err
}

// --- engine helpers -------------------------------------------------------

// loadPlan resolves a Plan by id via the queryPlanById DSL function.
// The function lives in core's dsl/planner/queries.memql (moved there
// from copresent on 2026-05-17). Earlier versions of this method used
// a `from(v1:planner:plan) ?.id==X` syntax that the engine doesn't
// support -- the engine's only entry point for reading a concept is
// a named query function, not an inline `from()` clause.
func (l *PlannerAgentLoop) loadPlan(ctx context.Context, planId string) (map[string]any, error) {
	q := fmt.Sprintf(`queryPlanById({planId:%q})`, planId)
	res, err := l.engine.Execute(ctx, q)
	if err != nil {
		return nil, err
	}
	rows := plannerExtractRows(res)
	if len(rows) == 0 {
		// Dump the raw response shape so we can see whether the
		// engine returned an empty result (filter mismatch / partition
		// scope issue) vs. a non-empty result in an unexpected shape
		// our extractor doesn't recognize.
		rawJSON, _ := json.Marshal(res)
		raw := string(rawJSON)
		if len(raw) > 500 {
			raw = raw[:500] + "...(truncated)"
		}
		l.logger.Warn("planner agent loop: queryPlanById returned no rows",
			"planId", planId, "query", q, "rawResponse", raw)
		return nil, fmt.Errorf("plan %s not found", planId)
	}
	return rows[0], nil
}

func (l *PlannerAgentLoop) loadTasks(ctx context.Context, planId string) ([]map[string]any, error) {
	q := fmt.Sprintf(`queryTasksForPlan({planId:%q})`, planId)
	res, err := l.engine.Execute(ctx, q)
	if err != nil {
		return nil, err
	}
	return plannerExtractRows(res), nil
}

func (l *PlannerAgentLoop) loadSpecialists(ctx context.Context, plan map[string]any) ([]map[string]any, error) {
	// Load every active agent owned by the user that requested this
	// Plan. Same DSL function the agent factory uses for its dedupe
	// lookup, so the candidate set the planner sees matches what the
	// factory's analysis would consider for match-or-extend.
	ownerUserId := getString(plan, "requestedBy")
	if ownerUserId == "" {
		return nil, nil
	}
	q := fmt.Sprintf(`queryActiveAgentsForUser({ownerUserId:%q})`, ownerUserId)
	res, err := l.engine.Execute(ctx, q)
	if err != nil {
		return nil, err
	}
	return plannerExtractRows(res), nil
}

// plannerExtractRows pulls a slice of row-maps out of an engine.Execute
// return value. The engine wraps query results in one of several
// shapes (a bare slice, a {rows: []} envelope, a {result: {rows: []}}
// envelope, a {nodes: []} envelope); the JSON-roundtrip walk here
// tolerates all of them. Same approach the agents integration uses
// in factory.go's extractRowsFromExecuteResult -- duplicated here so
// the planner doesn't take a dependency on the agents package's
// internal helpers.
func plannerExtractRows(raw any) []map[string]any {
	if raw == nil {
		return nil
	}
	rawJSON, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var loose any
	if err := json.Unmarshal(rawJSON, &loose); err != nil {
		return nil
	}
	return plannerRowsFromLoose(loose)
}

func plannerRowsFromLoose(v any) []map[string]any {
	switch x := v.(type) {
	case map[string]any:
		// Engine-emitted shape today: {bundle: {nodes: [...]}}. The
		// diagnostic we logged on the failing reproducer surfaced this:
		// the rawResponse was a non-empty bundle that the extractor
		// silently dropped because it only knew about top-level keys.
		if bundle, ok := x["bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok {
				return plannerCastRows(nodes)
			}
		}
		// Other shapes the engine produces under different builders.
		// Keep them so this extractor works against every working path
		// for future-proofing.
		for _, key := range []string{"nodes", "rows", "items", "results", "data"} {
			if arr, ok := x[key].([]any); ok {
				return plannerCastRows(arr)
			}
		}
		if result, ok := x["result"].(map[string]any); ok {
			for _, key := range []string{"rows", "nodes"} {
				if arr, ok := result[key].([]any); ok {
					return plannerCastRows(arr)
				}
			}
			if bundle, ok := result["bundle"].(map[string]any); ok {
				if nodes, ok := bundle["nodes"].([]any); ok {
					return plannerCastRows(nodes)
				}
			}
		}
	case []any:
		return plannerCastRows(x)
	}
	return nil
}

func plannerCastRows(items []any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// stampPhases, insertDispatchedTask, markPlanSucceeded, markPlanFailed,
// and escalateAwaitingFeedback all call DSL mutation functions via
// engine.Execute. MemQL's call syntax requires BARE-identifier object
// keys, not JSON-style quoted keys -- a quoted key silently drops the
// argument from the parse without erroring, which is exactly the
// failure mode that left an earlier set of Plans stuck in queued. See
// plan_execution.go for the canonical call shape and worker/store.go
// for the same pattern against queryPlanById.

func (l *PlannerAgentLoop) stampPhases(ctx context.Context, planId string, outline []phaseOutline) error {
	if len(outline) == 0 {
		return l.markPlanFailed(ctx, planId, "planner emitted empty decompose outline")
	}
	phasesJSON, err := json.Marshal(outline)
	if err != nil {
		return err
	}
	// Values may be JSON literals (objects/arrays/numbers) -- those
	// stay JSON-quoted because MemQL's value syntax accepts JSON
	// literals; only the KEY needs to be a bare identifier.
	q := fmt.Sprintf(
		`mutationUpdatePlanStatus({planId:%q, status:"running", phases:%s})`,
		planId, string(phasesJSON),
	)
	_, err = l.engine.Execute(ctx, q)
	return err
}

func (l *PlannerAgentLoop) insertDispatchedTask(ctx context.Context, planId string, task plannerTask) error {
	taskId := uuid.New().String()
	logicalStepId := task.LogicalStepId
	if logicalStepId == "" {
		logicalStepId = taskId
	}
	inputJSON, err := json.Marshal(task.Input)
	if err != nil || string(inputJSON) == "null" {
		inputJSON = []byte(`{}`)
	}
	q := fmt.Sprintf(
		`mutationCreateSemanticTask({taskId:%q, planId:%q, kind:%q, seq:0, logicalStepId:%q, attemptNumber:1, agentId:%q, phase:%q, input:%s})`,
		taskId, planId, task.Kind, logicalStepId, task.AgentId, task.Phase, string(inputJSON),
	)
	if _, err := l.engine.Execute(ctx, q); err != nil {
		return err
	}
	// Stamp the Plan to running with the task's agent. The existing
	// scope-elevation re-dispatch path picks it up from here. Phase
	// 4b stretch goal: fire AgentForwarder.ForwardContinuation
	// directly so the specialist starts immediately rather than on
	// the next event tick.
	q2 := fmt.Sprintf(
		`mutationUpdatePlanStatus({planId:%q, status:"running", ownerAgentId:%q})`,
		planId, task.AgentId,
	)
	_, err = l.engine.Execute(ctx, q2)
	return err
}

func (l *PlannerAgentLoop) markPlanSucceeded(ctx context.Context, planId string, output map[string]any) error {
	outputJSON, err := json.Marshal(output)
	if err != nil || string(outputJSON) == "null" {
		outputJSON = []byte(`{}`)
	}
	q := fmt.Sprintf(
		`mutationUpdatePlanStatus({planId:%q, status:"succeeded", output:%s, completedAt:%q})`,
		planId, string(outputJSON), time.Now().UTC().Format(time.RFC3339),
	)
	_, err = l.engine.Execute(ctx, q)
	return err
}

func (l *PlannerAgentLoop) markPlanFailed(ctx context.Context, planId, errorMessage string) error {
	q := fmt.Sprintf(
		`mutationUpdatePlanStatus({planId:%q, status:"failed", errorMessage:%q, completedAt:%q})`,
		planId, errorMessage, time.Now().UTC().Format(time.RFC3339),
	)
	_, err := l.engine.Execute(ctx, q)
	return err
}

func (l *PlannerAgentLoop) escalateAwaitingFeedback(ctx context.Context, planId, reason, question string) error {
	if reason == "" {
		reason = "feedback_required"
	}
	fbReq := map[string]any{
		"question": question,
		"kind":     "text",
		"askedAt":  time.Now().UTC().Format(time.RFC3339),
	}
	fbReqJSON, err := json.Marshal(fbReq)
	if err != nil {
		fbReqJSON = []byte(`{}`)
	}
	q := fmt.Sprintf(
		`mutationUpdatePlanStatus({planId:%q, status:"awaitingFeedback", feedbackReason:%q, feedbackRequest:%s})`,
		planId, reason, string(fbReqJSON),
	)
	_, err = l.engine.Execute(ctx, q)
	return err
}

// --- decision shape + parser ----------------------------------------------

// plannerDecision is the structured-output envelope the plannerAgent
// prompt is contracted to emit. The eight valid action values live in
// dispatchDecision above.
type plannerDecision struct {
	Action         string         `json:"action"`
	PlanOutline    []phaseOutline `json:"plan_outline,omitempty"`
	Task           plannerTask    `json:"task,omitempty"`
	Output         map[string]any `json:"output,omitempty"`
	ErrorMessage   string         `json:"errorMessage,omitempty"`
	FeedbackReason string         `json:"feedbackReason,omitempty"`
	Question       string         `json:"question,omitempty"`
}

type phaseOutline struct {
	Kind              string `json:"kind"`
	Label             string `json:"label"`
	ExpectedTaskCount int    `json:"expectedTaskCount,omitempty"`
	Status            string `json:"status,omitempty"`
}

type plannerTask struct {
	Kind          string         `json:"kind"`
	AgentId       string         `json:"agentId"`
	LogicalStepId string         `json:"logicalStepId,omitempty"`
	Phase         string         `json:"phase,omitempty"`
	Input         map[string]any `json:"input,omitempty"`
}

func parsePlannerDecision(resp any) (plannerDecision, error) {
	if resp == nil {
		return plannerDecision{}, fmt.Errorf("planner returned nil")
	}
	// engine.InvokeSI may return a string (the raw model output) or a
	// map (when the prompt's structured-output schema is enforced).
	// Handle both.
	var raw []byte
	switch r := resp.(type) {
	case string:
		raw = []byte(r)
	case map[string]any:
		b, err := json.Marshal(r)
		if err != nil {
			return plannerDecision{}, err
		}
		raw = b
	default:
		b, err := json.Marshal(resp)
		if err != nil {
			return plannerDecision{}, err
		}
		raw = b
	}
	var d plannerDecision
	if err := json.Unmarshal(raw, &d); err != nil {
		return plannerDecision{}, fmt.Errorf("parse decision JSON: %w (raw=%s)", err, truncate(string(raw), 200))
	}
	if d.Action == "" {
		return plannerDecision{}, fmt.Errorf("planner decision missing 'action' field (raw=%s)", truncate(string(raw), 200))
	}
	return d, nil
}

// --- small helpers --------------------------------------------------------

func extractPlanFields(ev events.Event) (planId, kind, status string, ok bool) {
	if ev.Payload == nil {
		return "", "", "", false
	}
	planId = getString(ev.Payload, "id")
	payload, _ := ev.Payload["payload"].(map[string]any)
	if payload == nil {
		return planId, "", "", planId != ""
	}
	kind = getString(payload, "kind")
	status = getString(payload, "status")
	return planId, kind, status, planId != ""
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
