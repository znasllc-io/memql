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

	args, err := json.Marshal(map[string]any{
		"goal":        goal,
		"ownerUserId": ownerUserId,
		"spaceId":     spaceId,
	})
	if err != nil {
		return l.markPlanFailed(ctx, planId, fmt.Sprintf("createSpecialist: marshal args: %v", err))
	}
	call := fmt.Sprintf("ensureAgentForGoal(%s)", string(args))
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
// the ensureAgentForGoal builtin's return. The engine wraps integration
// results as a MemoryNode-shaped value; the factory's payload is
// {agentId, agentName, roleSlug, action, reasoning} per
// dsl/agents/builtins.memql.
func extractAgentFactoryResult(res any) (agentId, action string) {
	switch v := res.(type) {
	case map[string]any:
		// Direct payload map.
		if id, ok := v["agentId"].(string); ok {
			agentId = id
		}
		if a, ok := v["action"].(string); ok {
			action = a
		}
		// Or wrapped inside `payload`.
		if p, ok := v["payload"].(map[string]any); ok {
			if id, ok := p["agentId"].(string); ok && agentId == "" {
				agentId = id
			}
			if a, ok := p["action"].(string); ok && action == "" {
				action = a
			}
		}
	case []map[string]any:
		if len(v) > 0 {
			return extractAgentFactoryResult(v[0])
		}
	case []any:
		if len(v) > 0 {
			return extractAgentFactoryResult(v[0])
		}
	}
	return agentId, action
}

// assignOwnerAgent writes a Plan-status update setting ownerAgentId
// without changing the status. Mutation re-validates against the Plan
// concept; we just want the in-place merge of ownerAgentId.
func (l *PlannerAgentLoop) assignOwnerAgent(ctx context.Context, planId, agentId string) error {
	args, err := json.Marshal(map[string]any{
		"planId":       planId,
		"status":       "routing",
		"ownerAgentId": agentId,
	})
	if err != nil {
		return err
	}
	_, err = l.engine.Execute(ctx, fmt.Sprintf("mutationUpdatePlanStatus(%s)", string(args)))
	return err
}

// --- engine helpers -------------------------------------------------------

func (l *PlannerAgentLoop) loadPlan(ctx context.Context, planId string) (map[string]any, error) {
	q := fmt.Sprintf(`from(v1:planner:plan) ?.id==%q limit 1`, planId)
	res, err := l.engine.Execute(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, ok := res.([]map[string]any)
	if !ok || len(rows) == 0 {
		return nil, fmt.Errorf("plan %s not found", planId)
	}
	return rows[0], nil
}

func (l *PlannerAgentLoop) loadTasks(ctx context.Context, planId string) ([]map[string]any, error) {
	q := fmt.Sprintf(`from(v1:planner:task) ?.payload.planId==%q select *`, planId)
	res, err := l.engine.Execute(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, ok := res.([]map[string]any)
	if !ok {
		return nil, nil
	}
	return rows, nil
}

func (l *PlannerAgentLoop) loadSpecialists(ctx context.Context, plan map[string]any) ([]map[string]any, error) {
	// v1: load every active agent owned by the user that requested
	// this Plan. Later phases narrow to "agents visible in this
	// space," exclude system_ roleSlugs from candidates, etc.
	ownerUserId := getString(plan, "requestedBy")
	if ownerUserId == "" {
		return nil, nil
	}
	q := fmt.Sprintf(
		`from(v1:agents:agent) ?.payload.ownerUserId==%q;payload.active==true select *`,
		ownerUserId,
	)
	res, err := l.engine.Execute(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, ok := res.([]map[string]any)
	if !ok {
		return nil, nil
	}
	return rows, nil
}

func (l *PlannerAgentLoop) stampPhases(ctx context.Context, planId string, outline []phaseOutline) error {
	if len(outline) == 0 {
		return l.markPlanFailed(ctx, planId, "planner emitted empty decompose outline")
	}
	phasesJSON, err := json.Marshal(outline)
	if err != nil {
		return err
	}
	q := fmt.Sprintf(
		`mutationUpdatePlanStatus({"planId": %q, "status": "running", "phases": %s})`,
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
		`mutationCreateSemanticTask({"taskId": %q, "planId": %q, "kind": %q, "seq": 0, "logicalStepId": %q, "attemptNumber": 1, "agentId": %q, "phase": %q, "input": %s})`,
		taskId, planId, task.Kind, logicalStepId, task.AgentId, task.Phase, string(inputJSON),
	)
	if _, err := l.engine.Execute(ctx, q); err != nil {
		return err
	}
	// Stamp the Plan to routing -> running so the existing scope-
	// elevation re-dispatch path picks it up. Stretch goal in Phase 4b:
	// fire a direct AgentForwarder.ForwardContinuation here so the
	// specialist starts work immediately, not on the next event cycle.
	q2 := fmt.Sprintf(
		`mutationUpdatePlanStatus({"planId": %q, "status": "running", "ownerAgentId": %q})`,
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
		`mutationUpdatePlanStatus({"planId": %q, "status": "succeeded", "output": %s, "completedAt": %q})`,
		planId, string(outputJSON), time.Now().UTC().Format(time.RFC3339),
	)
	_, err = l.engine.Execute(ctx, q)
	return err
}

func (l *PlannerAgentLoop) markPlanFailed(ctx context.Context, planId, errorMessage string) error {
	q := fmt.Sprintf(
		`mutationUpdatePlanStatus({"planId": %q, "status": "failed", "errorMessage": %q, "completedAt": %q})`,
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
		`mutationUpdatePlanStatus({"planId": %q, "status": "awaitingFeedback", "feedbackReason": %q, "feedbackRequest": %s})`,
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
