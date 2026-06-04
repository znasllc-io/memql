// agent_loop.go
//
// The Planner Agent dispatch loop (Phase 4 of the planner-redesign work).
//
// On each v1:planner:plan transition that needs the Planner Agent's next
// decision, the loop:
//
//  1. Loads the current Plan + all child Tasks.
//  2. Invokes the plannerAgent prompt (system prompt at
//     dsl/agents/prompts/plannerAgent.tmpl, paired with a strong-reasoning
//     provider) with the current state.
//  3. Parses the agent's structured-output decision.
//  4. Dispatches the decision against the engine (write Plan.phases on
//     'decompose', insert Task on 'dispatchTask', mark Plan terminal on
//     'markPlanSucceeded' / 'markPlanFailed', etc.).
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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
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
// graph.node.created.v1:planner:plan. Fires the first Planner Agent
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
	if kind == "trainSpecialist" {
		// trainSpecialist Plans are NOT decomposed by the Planner
		// Agent -- they're claimed by the TrainSpecialistDispatcher
		// (train_specialist_dispatch.go), which invokes the Trainer
		// Agent's web-search + chunk-write tool loop and completes the
		// Plan directly. Returning here keeps the Planner Agent loop
		// from racing the dispatcher on the same Plan.
		return
	}
	if kind == "embedDomainItems" {
		// embedDomainItems Plans are NOT decomposed by the Planner
		// Agent -- they're claimed by the EmbedDomainItemsDispatcher
		// (embed_domain_items_dispatch.go), which embeds a domain's
		// chunks via the knowledge integration's builtin and completes
		// the Plan directly. Returning here keeps the loop from racing
		// the dispatcher on the same Plan (#645).
		return
	}
	if kind == produceArtifactPlanKind {
		// produceArtifact (#788) is a SINGLE-TURN deliverable, NOT a
		// multi-step plan. It must NOT go through the planner-agent decompose
		// loop (invokeAndDispatch): that loop is for decomposing complex goals
		// + provisioning specialists via repeated large plannerAgent LLM calls,
		// and for a simple "make me a markdown file" it spun/re-invoked and
		// spammed the provider into a 429 rate-limit (memql#816). The requesting
		// assistant was stamped as ownerAgentId at creation, and the goal is
		// self-contained, so we just start the plan directly -> running;
		// handlePlanApprovedForExecution -> executeApprovedPlan then dispatches
		// it as ONE agent turn (the agent writes the file via the workbench).
		// No plannerAgent call, no loop.
		go l.startPlanDirect(context.Background(), planId)
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
	if status != "planning" && status != "queued" {
		// We only kick off on freshly-created Plans. The new userGoal
		// lifecycle stamps `planning` at mutationCreatePlan time and
		// transitions to `queued` once the planner agent has finished
		// decomposing + emitting tasks. Legacy callers / scope-
		// elevation flows still land in `queued` directly; accept
		// both so a freshly-inserted plan in either status triggers
		// the loop. Later transitions (running, awaitingFeedback,
		// terminal) are driven by HandlePlanUpdated, not this path.
		return
	}
	l.logger.Info("planner agent loop: handling new plan",
		"planId", planId, "kind", kind)
	// Run dispatch in a fresh goroutine: the bus loop processes events
	// synchronously per-subscriber, and a Planner Agent invocation is a
	// multi-second LLM call. Pattern mirrors handlePlanApprovedForExecution.
	//
	// Safety net: every leaky error path inside invokeAndDispatch (a
	// failing load, a rejected mutation, an unknown action, a panic
	// recovered by Go) must NOT leave a userGoal Plan stranded in
	// status="planning" forever. If the goroutine returns with an
	// error AND the plan is still in a non-terminal "live" status,
	// transition it to "failed" with the error as the errorMessage.
	// Explicit terminal transitions inside the loop
	// (markPlanFailed / markPlanSucceeded / escalateAwaitingFeedback)
	// fire before we get here, so the post-condition check is a
	// belt-and-suspenders backstop -- not the primary path.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				l.logger.Error("planner agent loop: PANIC in dispatch goroutine",
					"planId", planId, "recover", fmt.Sprintf("%v", r))
				l.ensurePlanLeftPlanning(context.Background(), planId,
					fmt.Errorf("panic: %v", r))
			}
		}()
		err := l.invokeAndDispatch(context.Background(), planId)
		if err == nil {
			return
		}
		l.logger.Warn("planner agent loop: invoke+dispatch failed",
			"planId", planId, "error", err)
		l.ensurePlanLeftPlanning(context.Background(), planId, err)
	}()
}

// ensurePlanLeftPlanning is the safety net that catches every leaky
// error path. Reads the Plan's current status; if it's still in a
// live working state (planning / routing / running), force a
// transition to failed so the user sees a clear terminal status with
// the underlying error message instead of a row stuck in planning
// for hours. Terminal-status plans (failed / succeeded / cancelled /
// awaitingFeedback / needsAgent / queued / paused) are left alone --
// they already represent a definite state the user can act on.
func (l *PlannerAgentLoop) ensurePlanLeftPlanning(ctx context.Context, planId string, loopErr error) {
	plan, lerr := l.loadPlan(ctx, planId)
	if lerr != nil {
		// Can't read the plan, can't safely classify -- best we can
		// do is unconditionally try to mark failed. The mutation is
		// idempotent on terminal states (an update on an already-
		// terminal plan is a no-op the user will not notice).
		if mfErr := l.markPlanFailed(ctx, planId,
			fmt.Sprintf("planner safety net: %v (status read failed: %v)", loopErr, lerr)); mfErr != nil {
			l.logger.Warn("planner agent loop: safety-net markPlanFailed failed",
				"planId", planId, "loopError", loopErr, "loadError", lerr, "markError", mfErr)
		}
		return
	}
	switch getString(plan, "status") {
	case "planning", "routing", "running":
		// Still in a live working state -- the loop bailed but
		// nothing inside it stamped a terminal status. Mark failed
		// so the row is no longer a UI mystery.
		if mfErr := l.markPlanFailed(ctx, planId,
			fmt.Sprintf("planner safety net: %v", loopErr)); mfErr != nil {
			l.logger.Warn("planner agent loop: safety-net markPlanFailed failed",
				"planId", planId, "loopError", loopErr, "markError", mfErr)
		}
	}
}

// HandlePlanUpdated is the event-bus subscriber for
// graph.node.updated.v1:planner:plan that complements
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
	if kind == "adHocAction" || kind == "scopeElevation" || kind == produceArtifactPlanKind {
		// produceArtifact is started directly at creation (HandlePlanCreated ->
		// startPlanDirect) and executed by handlePlanApprovedForExecution; it
		// never needs the decompose/auto-start dance, so updates are a no-op here.
		return
	}
	l.logger.Debug("planner agent loop: plan updated (re-invoke deferred)",
		"planId", planId, "kind", kind, "status", status)
}

// startPlanDirect flips a freshly-created Plan straight to "running" without
// the planner-agent decompose loop -- for single-turn kinds (produceArtifact)
// whose owning agent + self-contained goal are already set at creation. The
// status=running transition is what handlePlanApprovedForExecution /
// executeApprovedPlan key off to dispatch the single agent turn. Best-effort:
// on failure the plan stays in planning and the safety net never fires (no
// plannerAgent call was made), so there's nothing to spam. (memql#816)
func (l *PlannerAgentLoop) startPlanDirect(ctx context.Context, planId string) {
	q := fmt.Sprintf(`mutationStartPlan({planId:%q})`, planId)
	if _, err := l.engine.Execute(systemActorContext(ctx), q); err != nil {
		l.logger.Warn("planner agent loop: startPlanDirect failed",
			"planId", planId, "error", err)
	}
}

// produceArtifactPlanKind mirrors the agents integration's
// produceArtifactKind (integrations/agents/integration.go). Plans of this
// kind are spawned by the Assistant's produceArtifact tool and auto-run
// (skip the manual Run gate) so the deliverable is produced asynchronously.
const produceArtifactPlanKind = "produceArtifact"

// systemPlannerActor is the synthetic subject the planner integration
// stamps on its Execute calls so engine validators that gate writes
// on actor presence (mutationActor in component/memql/executor.go)
// don't reject the loop with "no actor found in context". Mirrors the
// seed materializer's systemActorContext pattern.
const systemPlannerActor = "system:planner"

// systemActorContext wraps ctx with a TokenInfo so engine.Execute
// calls from the planner agent loop carry a non-empty actor. Used
// uniformly by loadPlan / loadTasks / loadSpecialists / the mutation
// helpers so every engine roundtrip from this file sees the same
// system identity.
func systemActorContext(ctx context.Context) context.Context {
	return auth.ContextWithToken(ctx, &auth.TokenInfo{
		Subject: systemPlannerActor,
		Claims: map[string]any{
			"sub":  systemPlannerActor,
			"role": "system",
		},
	})
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
	// Token/cost approval gate runs BEFORE anything spends (epic #836 /
	// #839). Estimate the Plan's cost up front; if it exceeds the
	// approval threshold and the user hasn't already approved, PARK the
	// Plan as awaitingFeedback(budget_approval_required) and make NO
	// model call -- not even the cheap triage classifier. Below the
	// threshold (the common case) it auto-runs and we fall through.
	if handled, err := l.gatePlanApprovalIfNeeded(ctx, planId); err != nil || handled {
		return err
	}
	// Complexity triage runs FIRST (epic #836 / memql#837): a cheap
	// classifier decides trivial vs moderate/complex. A trivial,
	// self-contained deliverable (the "list of 10 birds" case) with an
	// owning agent is produced in ONE direct turn -- NO decompose loop,
	// no specialists, no training. Only moderate/complex goals (or
	// trivial goals with no owning agent yet) fall through to the
	// bounded, guarded decompose loop below. A classify failure never
	// strands the Plan -- triageAndMaybeShortcut returns handled=false
	// and we proceed to the loop.
	if handled, err := l.triageAndMaybeShortcut(ctx, planId, time.Now().UTC().Format(time.RFC3339)); err != nil || handled {
		return err
	}
	return l.invokeAndDispatchIter(ctx, planId, 0, newConvTracker())
}

func (l *PlannerAgentLoop) invokeAndDispatchIter(ctx context.Context, planId string, iter int, conv *convTracker) error {
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

	// The plannerAgent prompt's input JSON schema declares
	// `specialists` and `tasks` as arrays; a nil Go slice serializes
	// to `null`, which fails JSON-schema validation
	// ('expected array, but got null'). Default to empty slices so a
	// brand-new Plan with no tasks + no existing agents still passes
	// the schema gate.
	if tasks == nil {
		tasks = []map[string]any{}
	}
	if specialists == nil {
		specialists = []map[string]any{}
	}

	// Lean prompt inputs (memql#820): project plan / tasks / specialists
	// down to the decision-relevant fields before sending. This strips
	// the specialists' roleEmbedding vectors + lineage and the plan/task
	// input/output blobs -- the bloat that drove the 800k-tokens/min
	// spike -- so each plannerAgent call is cheap.
	data := map[string]any{
		"plan":        compactPlanForPrompt(plan),
		"tasks":       compactTasksForPrompt(tasks),
		"specialists": compactSpecialistsForPrompt(specialists),
		"partition":   getString(plan, "partition"),
		"now":         time.Now().UTC().Format(time.RFC3339),
	}

	// HARD per-plan LLM ceiling (memql#819). Before EVERY plannerAgent
	// call, check the cumulative invocation count + token budget
	// (persisted on the Plan, so they survive across cycles / retries).
	// On exceed we park the Plan to awaitingFeedback and NEVER make
	// another LLM call -- this is what makes unbounded spend
	// structurally impossible, independent of the per-cycle
	// maxPlannerIterations cap above.
	estTokens := estimatePlannerCallTokens(data)
	if gate := evaluatePlannerCallGate(plan, estTokens, maxPlannerInvocationsPerPlan(), plannerDefaultTokenBudget()); gate.Blocked {
		l.logger.Warn("planner agent loop: per-plan LLM ceiling reached; parking",
			"planId", planId, "llmCallCount", planLLMCallCount(plan),
			"tokenSpent", asInt(plan["tokenSpent"]), "iter", iter, "reason", gate.Reason)
		return l.escalateAwaitingFeedback(ctx, planId, gate.Reason, gate.Message)
	}

	// Model tiering (memql#838): the plannerAgent prompt's @defaultProvider
	// is the CHEAP tier (streamClaudeSonnet -- no extended thinking). We do
	// NOT default to Opus+thinking. Reasoning (reasoningClaudeOpus) is an
	// explicit, logged, bounded ESCALATION: only once the cheap tier has had
	// a few iterations in this cycle without converging do we override the
	// provider to the reasoning tier for the remaining iterations. A
	// trivial/moderate plan converges in 1-2 iterations and so makes ZERO
	// Opus+thinking calls.
	siCtx := systemActorContext(ctx)
	if provider, reasoning := selectPlannerProvider(iter); reasoning {
		l.logger.Info("planner agent loop: escalating to reasoning tier (cheap tier not converging)",
			"planId", planId, "iter", iter, "provider", provider)
		siCtx = memql.WithProviderOverride(siCtx, provider)
	}
	resp, err := l.engine.InvokeSI(siCtx, "plannerAgent", data)
	if err != nil {
		// Provider rate-limit (429) is RETRYABLE-LATER, not a plan
		// failure (memql#821). Re-attempting immediately would be a
		// retry storm -- the exact behavior that blew the rate limit.
		// Park the plan to awaitingFeedback (resumable) and make NO
		// further LLM call this cycle. This also catches the #825
		// circuit breaker's synthetic 429.
		if isRateLimitError(err) {
			l.logger.Warn("planner agent loop: provider rate-limited; parking (no retry storm)",
				"planId", planId, "iter", iter, "error", err)
			return l.escalateAwaitingFeedback(ctx, planId, "feedback_required",
				"The planner was rate-limited by the model provider (429). The plan is parked and can be resumed shortly -- it was NOT re-attempted, to avoid hammering the provider.")
		}
		return l.markPlanFailed(ctx, planId, fmt.Sprintf("plannerAgent invocation failed: %v", err))
	}
	// Count this invocation against the cumulative ceiling immediately
	// (before parsing / dispatch) and persist it, so a parse failure or
	// a later re-trigger still sees the spend. (memql#819)
	l.recordPlannerInvocation(ctx, plan, planId, estTokens)

	decision, err := parsePlannerDecision(resp)
	if err != nil {
		return l.markPlanFailed(ctx, planId, fmt.Sprintf("planner decision parse failed: %v", err))
	}

	l.logger.Info("planner agent loop: dispatching decision",
		"planId", planId, "action", decision.Action, "iter", iter)
	return l.dispatchDecision(ctx, planId, decision, iter, conv)
}

// dispatchDecision routes the Planner Agent's structured-output
// decision to its corresponding engine action. Loop-extending actions
// (decompose / createSpecialist / extendSpecialist) re-invoke the
// planner with iter+1 so the LLM picks up where it left off in the
// same event cycle. Terminal actions (markPlanSucceeded / Failed /
// escalate) and dispatchTask park the Plan for the next external
// trigger.
func (l *PlannerAgentLoop) dispatchDecision(ctx context.Context, planId string, d plannerDecision, iter int, conv *convTracker) error {
	// Convergence / no-progress guard (memql#822). If the planner has
	// emitted the IDENTICAL non-terminal decision past the threshold in
	// this cycle, it's oscillating without progressing -- park instead
	// of dispatching the repeat and spinning (terminal decisions are
	// never tracked, so this never blocks a real finish).
	if park, count := conv.recordAndCheck(d); park {
		l.logger.Warn("planner agent loop: no progress -- identical decision repeated; parking",
			"planId", planId, "action", d.Action, "repeats", count, "iter", iter)
		return l.escalateAwaitingFeedback(ctx, planId, "feedback_required",
			fmt.Sprintf("Planner repeated the same '%s' decision %d times without making progress. Resume to retry with fresh context.", d.Action, count))
	}
	switch d.Action {
	case "decompose":
		if err := l.stampPhases(ctx, planId, d.PlanOutline); err != nil {
			return err
		}
		// Re-invoke so the planner picks the first concrete action
		// (createSpecialist / dispatchTask) given the freshly-stamped
		// phases. Without this the Plan would sit at queued with
		// phases set but no Tasks created.
		return l.invokeAndDispatchIter(ctx, planId, iter+1, conv)
	case "dispatchTask":
		if err := l.insertDispatchedTask(ctx, planId, d.Task); err != nil {
			return err
		}
		// Re-invoke so the planner can emit the next task (or
		// markPlanSucceeded once it's done emitting). Previously
		// dispatchTask parked the plan after a single task, which
		// produced one-task-per-event-cycle plans -- fine when the
		// agent immediately consumed the task and triggered a new
		// cycle, but in planning-mode there's no auto-dispatch
		// pulling us forward. Continue iterating until the planner
		// signals it's done (markPlanSucceeded) or we hit the cap.
		return l.invokeAndDispatchIter(ctx, planId, iter+1, conv)
	case "markPlanSucceeded":
		// "Planning complete" semantics: while a userGoal plan is
		// still in status="planning", markPlanSucceeded from the
		// planner means "I've emitted every task I want to emit; the
		// plan is ready for the user to run." Transition to queued
		// (not succeeded) so the cockpit's Run button shows up.
		// markPlanSucceeded on a plan that's already running /
		// queued / etc. keeps its terminal-state semantics.
		plan, lerr := l.loadPlan(ctx, planId)
		if lerr == nil && getString(plan, "status") == "planning" {
			// Guard: a planning-stage Plan with ZERO tasks is the
			// "model shortcutted the workflow and tried to answer
			// the user directly" failure mode. The prompt forbids
			// it, but models are sometimes creative. When we detect
			// it, re-invoke so the model gets another shot rather
			// than promoting an empty Plan to queued. Iteration cap
			// will bail us out if it keeps refusing.
			existing, terr := l.loadTasks(ctx, planId)
			if terr == nil && len(existing) == 0 {
				l.logger.Warn("planner agent loop: markPlanSucceeded with no tasks; re-invoking",
					"planId", planId, "iter", iter)
				return l.invokeAndDispatchIter(ctx, planId, iter+1, conv)
			}
			return l.markPlanningComplete(ctx, planId)
		}
		return l.markPlanSucceeded(ctx, planId, d.outputAsMap())
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
		return l.ensureSpecialistForPlan(ctx, planId, iter, conv)
	case "mintSkill":
		// Phase 3 (memql#159): authority gate + catalog-search
		// heuristic + mutationMintSkill dispatch. In-envelope mints
		// execute immediately; out-of-envelope mints surface a canvas
		// approval card and park the Plan with
		// feedbackReason='mint_skill_approval_required'.
		return l.handleMintSkill(ctx, planId, d)
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
// "assistant" by design (it's a GA-only surface in chat),
// and (b) the planner integration has full engine access -- routing
// through the LLM's tool loop would add an unnecessary round trip
// when the planner has already decided this is the right action.
func (l *PlannerAgentLoop) ensureSpecialistForPlan(ctx context.Context, planId string, iter int, conv *convTracker) error {
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
	//
	// planId is threaded through so the factory can stamp
	// lineage.originatingPlanId + lineage.createdBy="planner" on the
	// new specialist (memql#399). The factory hard-stamps
	// kind="specialist" on any agent it creates regardless of caller.
	call := fmt.Sprintf(
		`ensureAgentForGoal({goal:%q, ownerUserId:%q, spaceId:%q, planId:%q})`,
		goal, ownerUserId, spaceId, planId,
	)
	res, err := l.engine.Execute(systemActorContext(ctx), call)
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
	return l.invokeAndDispatchIter(ctx, planId, iter+1, conv)
}

// extractAgentFactoryResult digs the {agentId, action} fields out of
// the ensureAgentForGoal builtin's return. The engine wraps results
// in {bundle: {nodes: [...]}} envelopes (same shape queryPlanById
// returns); we route through plannerExtractRows to normalize to a
// flat row slice, then pluck the factory-specific fields off the
// first row's top-level or payload.
func extractAgentFactoryResult(res any) (agentId, action string) {
	rows := memql.MaterializeRows(res)
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
	_, err := l.engine.Execute(systemActorContext(ctx), q)
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
	res, err := l.engine.Execute(systemActorContext(ctx), q)
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(res)
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
	res, err := l.engine.Execute(systemActorContext(ctx), q)
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(res), nil
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
	res, err := l.engine.Execute(systemActorContext(ctx), q)
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(res), nil
}

// (Row unwrapping moved to component/memql.MaterializeRows -- one
// canonical helper for every in-process or wire-format consumer.)

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
	// Keep the plan in "planning" while phases are being stamped --
	// under the new lifecycle the plan stays in planning until the
	// planner emits markPlanSucceeded (which we reinterpret as
	// "planning complete -> queued"). The previous code flipped
	// status to "running" here, which prematurely transitioned the
	// plan out of planning and broke the UI gating + the Run button.
	q := fmt.Sprintf(
		`mutationUpdatePlanStatus({planId:%q, status:"planning", phases:%s})`,
		planId, string(phasesJSON),
	)
	_, err = l.engine.Execute(systemActorContext(ctx), q)
	return err
}

func (l *PlannerAgentLoop) insertDispatchedTask(ctx context.Context, planId string, task plannerTask) error {
	taskId := id.NewShortId()
	logicalStepId := task.LogicalStepId
	if logicalStepId == "" {
		logicalStepId = taskId
	}
	input := task.Input
	if input == nil {
		input = map[string]any{}
	}
	// seq is the task's order within the parent Plan. The
	// plannerAgent prompt could be tightened to emit seq on each
	// dispatchTask, but it isn't part of the structured-output
	// schema today, so we compute it server-side: count existing
	// tasks under this Plan and assign the next index. The loop is
	// single-goroutine per Plan, so no race.
	existing, _ := l.loadTasks(ctx, planId)
	seq := len(existing)
	// Build the args block via json.Marshal -- the MemQL parser
	// accepts a JSON object literal as a function call's argument map
	// (quoted keys, properly-typed values incl. integers), which is
	// the same pattern the cockpit's mutationCreatePlan call uses.
	// The earlier handcrafted fmt.Sprintf produced bare-key /
	// bare-int syntax (`seq:0, attemptNumber:1, ...`) which the
	// parser rejected at the first integer-literal value.
	// task.AgentId is intentionally NOT passed to mutationCreateSemanticTask --
	// the task concept doesn't carry an agentId field (per the v1
	// design where the parent Plan's ownerAgentId is the single
	// authority on "who runs the tasks under this Plan"). We DO
	// still stamp ownerAgentId on the Plan below so the UI can show
	// the assigned agent next to the plan row.
	args := map[string]any{
		"taskId":        taskId,
		"planId":        planId,
		"kind":          task.Kind,
		"seq":           seq,
		"logicalStepId": logicalStepId,
		"attemptNumber": 1,
		"phase":         task.Phase,
		"input":         input,
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("marshal task args: %w", err)
	}
	q := fmt.Sprintf("mutationCreateSemanticTask(%s)", string(argsJSON))
	if _, err := l.engine.Execute(systemActorContext(ctx), q); err != nil {
		l.logger.Warn("planner agent loop: insertDispatchedTask query rejected",
			"planId", planId, "query", q, "error", err)
		return err
	}
	// During the planning phase we ONLY persist task definitions --
	// we do not flip the plan to running. The plan stays in
	// status="planning" until the planner emits markPlanSucceeded
	// (interpreted as "planning complete" -- the plan transitions to
	// "queued" and waits for the user to click Run). The agent /
	// dispatch surface that consumes status==running events is what
	// actually executes tasks; that path fires from
	// mutationStartPlan, not from here.
	//
	// We do stamp ownerAgentId so the queued plan's "who's going to
	// run this" is visible in the cockpit while the user reviews the
	// task list.
	if task.AgentId != "" {
		q2 := fmt.Sprintf(
			`mutationUpdatePlanStatus({planId:%q, status:"planning", ownerAgentId:%q})`,
			planId, task.AgentId,
		)
		if _, err := l.engine.Execute(systemActorContext(ctx), q2); err != nil {
			return err
		}
	}
	return nil
}

// markPlanningComplete flips a planning-phase Plan to queued.
// Triggered when the planner agent emits markPlanSucceeded while
// the plan is still in status="planning" -- it means "I've emitted
// every Task I'm going to emit; the user can review and click Run."
// No completedAt, no output -- the plan isn't actually done, just
// done planning.
func (l *PlannerAgentLoop) markPlanningComplete(ctx context.Context, planId string) error {
	q := fmt.Sprintf(
		`mutationUpdatePlanStatus({planId:%q, status:"queued"})`,
		planId,
	)
	_, err := l.engine.Execute(systemActorContext(ctx), q)
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
	_, err = l.engine.Execute(systemActorContext(ctx), q)
	return err
}

func (l *PlannerAgentLoop) markPlanFailed(ctx context.Context, planId, errorMessage string) error {
	q := fmt.Sprintf(
		`mutationUpdatePlanStatus({planId:%q, status:"failed", errorMessage:%q, completedAt:%q})`,
		planId, errorMessage, time.Now().UTC().Format(time.RFC3339),
	)
	_, err := l.engine.Execute(systemActorContext(ctx), q)
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
	_, err = l.engine.Execute(systemActorContext(ctx), q)
	return err
}

// --- decision shape + parser ----------------------------------------------

// plannerDecision is the structured-output envelope the plannerAgent
// prompt is contracted to emit. The valid action values live in
// dispatchDecision above. Phase 3 (memql#159) added the mintSkill
// payload fields at the bottom; unmarshaling tolerates their absence
// on non-mintSkill actions (omitempty + zero-value semantics).
type plannerDecision struct {
	Action      string         `json:"action"`
	PlanOutline []phaseOutline `json:"plan_outline,omitempty"`
	Task        plannerTask    `json:"task,omitempty"`
	// Output is the markPlanSucceeded payload. The contract is an
	// object, but models occasionally hand back a bare string when
	// they shortcut the workflow (treating the goal as "just answer
	// the user"). Use json.RawMessage so we can detect the shape at
	// dispatch time and either wrap a string in {response:"..."} or
	// reject the shortcut entirely. Parsing never fails on the shape
	// alone, so the planner doesn't lose the iteration to a parse
	// error on a recoverable mismatch.
	Output         json.RawMessage `json:"output,omitempty"`
	ErrorMessage   string          `json:"errorMessage,omitempty"`
	FeedbackReason string          `json:"feedbackReason,omitempty"`
	Question       string          `json:"question,omitempty"`

	// mintSkill payload (Phase 3 / memql#159). Populated only when
	// Action == "mintSkill"; the handler in mint_skill_handler.go
	// runs the authority gate + catalog-search heuristic on these
	// fields before issuing mutationMintSkill.
	Name          string   `json:"name,omitempty"`
	Slug          string   `json:"slug,omitempty"`
	Description   string   `json:"description,omitempty"`
	Category      string   `json:"category,omitempty"`
	Tier          string   `json:"tier,omitempty"`
	DomainIds     []string `json:"domainIds,omitempty"`
	ToolSlugs     []string `json:"toolSlugs,omitempty"`
	LiveSourceIds []string `json:"liveSourceIds,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Justification string   `json:"justification,omitempty"`
}

// outputAsMap decodes plannerDecision.Output into a map. When the
// model emitted a bare string under `output`, the result is wrapped
// as {response: "..."} so downstream serializers don't blow up. nil /
// empty / unparseable input returns an empty map.
func (d *plannerDecision) outputAsMap() map[string]any {
	out := map[string]any{}
	if len(d.Output) == 0 {
		return out
	}
	// Object first -- the canonical contract.
	if err := json.Unmarshal(d.Output, &out); err == nil {
		return out
	}
	// String -- the shortcut shape. Wrap so callers see a map.
	var s string
	if err := json.Unmarshal(d.Output, &s); err == nil {
		return map[string]any{"response": s}
	}
	// Anything else (array, number, etc.) -- store under "value" so
	// the data survives but the downstream type contract is still met.
	var raw any
	if err := json.Unmarshal(d.Output, &raw); err == nil {
		return map[string]any{"value": raw}
	}
	return out
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
	// Strip markdown code-fence wrappers if the model emitted one
	// despite the prompt's "no prose / first char {, last char }"
	// instruction. Both Claude and GPT-4-class models still wrap JSON
	// in ```json ... ``` fences on occasion, especially after a long
	// system prompt; refusing to handle that brittle-prompts the
	// planner. Strip leading ```[lang] and trailing ``` markers (with
	// surrounding whitespace) before unmarshalling.
	raw = stripJSONFence(raw)
	var d plannerDecision
	if err := json.Unmarshal(raw, &d); err != nil {
		return plannerDecision{}, fmt.Errorf("parse decision JSON: %w (raw=%s)", err, truncate(string(raw), 200))
	}
	if d.Action == "" {
		return plannerDecision{}, fmt.Errorf("planner decision missing 'action' field (raw=%s)", truncate(string(raw), 200))
	}
	return d, nil
}

// stripJSONFence removes a single leading ```[lang]\n ... ``` fence
// pair if present. Leaves non-fenced input untouched. Conservative:
// only strips when BOTH a leading and trailing fence are detected.
func stripJSONFence(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if !bytes.HasPrefix(trimmed, []byte("```")) {
		return raw
	}
	// Drop the opening fence line (```[lang]\n).
	rest := trimmed[3:]
	if nl := bytes.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	}
	rest = bytes.TrimRight(rest, " \t\n\r")
	if !bytes.HasSuffix(rest, []byte("```")) {
		return raw
	}
	return bytes.TrimSpace(rest[:len(rest)-3])
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
