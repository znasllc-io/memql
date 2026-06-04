package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/core/id"
)

// Concept-visibility note: v1:cognition:participant is annotated
// `@visibility("cognition", "bff")` and is intentionally NOT loaded
// on the planner binary. The planner deliberately does not look up
// the SI participant id when dispatching a Plan -- it would be a
// cross-domain leak (planner reaching into cognition's data axis to
// learn which Participant row Sofia is in this space). Instead the
// planner stamps the agent's reply onto the Plan via
// mutationUpdatePlanStatus(output={reply:...}); the canvas card
// + Tasks panel both render that field directly. The chat
// transcript stays cognition's responsibility; if a future flow
// wants the planner-driven reply to ALSO show up as a chat
// utterance, that emit is cognition's job (subscribe to plan
// transitions and write the utterance from the cognition node,
// which DOES have the participant concept loaded).

const systemActorId = "system:planner-integration"

// plan_execution.go -- post-Allow agent dispatch on the planner node.
//
// Lives here (not in the cognition integration) because Plan and
// Task lifecycle is the planner service's responsibility. Cognition
// owns routing + turn-taking for chat utterances; the planner owns
// "a Plan transitioned to running, dispatch the agent that will
// execute the work and stamp the terminal status when it's done."
//
// Subscription: graph.node.updated.v1:planner:plan, filtered to
// kind=scopeElevation && status=running. The frontend's
// PlanScopeElevationCard.allow path bumps the Plan to running on
// the user's Allow click; the engine's executeUpdate path emits the
// updated event after the underlying append-only insert succeeds.
//
// Dispatch: build a synthetic AgentGenerateTurnMsg with the Plan's
// goal as a single user-role history message (NOT written to chat
// as a fake utterance -- prompt context only); forward to the agent
// peer via this integration's own AgentForwarder; consume the
// response stream; mark the Plan terminal (succeeded with
// output={reply:...} or failed with errorMessage). The reply text
// is rendered on the canvas card + Tasks panel via Plan.output.reply
// -- the planner does NOT write a chat utterance (cognition's data
// axis -- the v1:cognition:participant concept isn't loaded here by
// design).
//
// hints["trigger"]="plan_approved" rides on the dispatched
// AgentGenerateTurnMsg so the agent prompt's planApprovedTrigger
// branch fires (skip elevation re-request, dispatch the worker tool
// directly, publish the task-done card).

// planRoutingFields is the minimal set of identifiers extracted off
// a graph.node.updated.v1:planner:plan event payload to decide
// whether this handler should fire.
type planRoutingFields struct {
	ID          string
	Kind        string
	Status      string
	RequestedBy string
}

// planExecutionRow is the projection fetchPlanForExecution returns.
// Holds everything executeApprovedPlan needs to construct the
// dispatch.
type planExecutionRow struct {
	ID           string
	Partition    string
	Kind         string
	Status       string
	Goal         string
	SpaceId      string
	OwnerAgentId string
	RequestedBy  string
	// WatchExecution is the opt-in "watch agent work" flag (memql#900),
	// read off Plan.input.watchExecution. When true the execution turn is
	// routed through the interactive streaming lane instead of the
	// background default so live progress can be surfaced. Defaults false.
	WatchExecution bool
}

// agentTurnResult mirrors cognition.agentReplyResult -- the
// planner's local copy so this package doesn't depend on the
// cognition package.
type agentTurnResult struct {
	text      string
	citations []*memqlv1.AgentTurnCitation
	retrieved []*memqlv1.AgentRetrievedChunk
	// paused is true when the turn ended by cooperative preemption ("pass",
	// epic memql#902 / #906). The caller leaves the Plan in its paused state
	// instead of stamping it succeeded/failed.
	paused bool
}

// executableViaApprovedPath reports whether a plan of this kind is executed by
// forwarding its goal to the assigned agent's tool loop when it reaches
// "running" (executeApprovedPlan). Two kinds use this path:
//   - scopeElevation: the user clicked Allow on a computer-use scope card.
//   - produceArtifact: the conversational "make me a file" delegation (#788),
//     auto-started to running (#788) -- this is the executor that was missing,
//     so the deliverable was never actually produced (memql#800).
//
// Other kinds have their own dispatchers (trainSpecialist / embedDomainItems)
// or are handled elsewhere (adHocAction / agentInvocation), so they MUST NOT
// flow through here.
func executableViaApprovedPath(kind string) bool {
	return kind == "scopeElevation" || kind == produceArtifactPlanKind
}

// handlePlanApprovedForExecution is the event-bus subscriber.
// Filters to executable Plans transitioning to running, then kicks off the
// dispatch in a goroutine so the bus loop doesn't block on the multi-second
// tool-calling turn.
func (p *PlannerIntegration) handlePlanApprovedForExecution(event events.Event) {
	fields := extractPlanRoutingFields(event)
	if fields.Status != "running" || fields.ID == "" || !executableViaApprovedPath(fields.Kind) {
		return
	}

	if p.agentForwarder == nil {
		p.logger.Warn("plan execution: agent forwarder not configured; skipping dispatch",
			"plan_id", fields.ID,
		)
		return
	}

	// Dedup: a plan can receive more than one graph.node.updated event while
	// in "running"; without this guard it would be dispatched to its agent
	// twice (double file write, double token spend). LoadOrStore claims the
	// plan; executeApprovedPlan clears it on return. (memql#800)
	//
	// The claim value is the turn's requestId (minted here so it's known
	// before dispatch): handlePlanPreempt (#906) reads it to address the
	// cross-node "pass" signal at the in-flight turn. Forwarding it as
	// AgentGenerateTurnMsg.RequestId means the agent loop's pause check keys
	// on the same id.
	requestId := id.NewShortId()
	if _, alreadyExecuting := p.executing.LoadOrStore(fields.ID, requestId); alreadyExecuting {
		return
	}

	// Run the dispatch in a fresh goroutine. The bus loop processes
	// events synchronously per-subscriber, and a tool-calling turn
	// takes seconds (sometimes minutes); blocking the bus would
	// stall every other subscriber's events behind us.
	bgCtx := contextWithSystemActor(context.Background())
	go p.executeApprovedPlan(bgCtx, fields.ID, requestId)
}

// isSlotFreeingStatus reports whether a Plan transition INTO this status
// vacates an account concurrency slot -- i.e. the Plan is no longer counted as
// running (epic memql#902 / #905). The count gate only counts status=running,
// so every one of these states means a slot may now be free for a waiting
// Plan. (We deliberately exclude waitingForSlot -- that transition is the
// admission demote itself, which happens precisely when the account is AT cap,
// so there is never a slot to hand out at that moment.)
func isSlotFreeingStatus(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "paused", "awaitingFeedback", "needsAgent":
		return true
	default:
		return false
	}
}

// handleSlotFreed is the event-bus subscriber that drives the FIFO waiting
// queue (epic memql#902 / #905). When a Plan leaves the running state, a
// concurrency slot may have opened for the owning account, so it admits the
// longest-waiting Plan(s). Admission is idempotent and re-counts under the
// per-account lock, so a spurious wakeup (the freed Plan wasn't actually
// occupying a slot) is a harmless no-op.
func (p *PlannerIntegration) handleSlotFreed(event events.Event) {
	fields := extractPlanRoutingFields(event)
	if fields.ID == "" || !isSlotFreeingStatus(fields.Status) {
		return
	}
	if p.admission == nil || p.entitlements == nil {
		return
	}
	bgCtx := contextWithSystemActor(context.Background())
	account := fields.RequestedBy
	if account == "" {
		// The event didn't carry requestedBy; fall back to a by-id read.
		if row, err := p.fetchPlanForExecution(bgCtx, fields.ID); err == nil {
			account = strings.TrimSpace(row.RequestedBy)
		}
	}
	if account == "" {
		return
	}
	// Admit in a goroutine: the sweep takes a per-account advisory lock and
	// issues engine mutations, which must not block the synchronous bus loop.
	go p.admitNextForAccount(bgCtx, account)
}

// isPreemptTriggerStatus reports whether a Plan transition INTO this status
// should preempt the in-flight agent turn (epic memql#902 / #906). A "pass"
// (or a cancel) of a Plan that is mid-execution must cooperatively stop the
// turn on the agent node so it actually relinquishes its slot, not just flip
// the row's status. paused = the user/scheduler passed it (resumable, #907);
// cancelled = terminal stop. Both want the in-flight turn to halt.
func isPreemptTriggerStatus(status string) bool {
	return status == "paused" || status == "cancelled"
}

// handlePlanPreempt is the event-bus subscriber that turns a "pass" (a Plan
// going to paused/cancelled while a turn is in flight) into the cross-node
// signal that actually stops the agent turn (epic memql#902 / #906). It looks
// up the in-flight turn's requestId (stored in the executing map by
// handlePlanApprovedForExecution) and forwards an AgentPreemptTurn on that
// turn's open stream; the agent node calls RequestPause(requestId) and the
// loop halts at its next checkpoint. The slot-free + admit-next is already
// handled by handleSlotFreed on the same paused transition.
//
// No-op when the Plan has no in-flight turn here (not mid-execution, or the
// turn already finished) -- the status change alone is sufficient then.
func (p *PlannerIntegration) handlePlanPreempt(event events.Event) {
	fields := extractPlanRoutingFields(event)
	if fields.ID == "" || !isPreemptTriggerStatus(fields.Status) {
		return
	}
	if p.agentForwarder == nil {
		return
	}
	v, ok := p.executing.Load(fields.ID)
	if !ok {
		return // no in-flight turn for this Plan; nothing to preempt
	}
	requestId, _ := v.(string)
	if strings.TrimSpace(requestId) == "" {
		return
	}
	envelope := &memqlv1.MemqlClientMessage{
		MessageId: id.NewShortId(),
		Payload: &memqlv1.MemqlClientMessage_AgentPreemptTurn{
			AgentPreemptTurn: &memqlv1.AgentPreemptTurnMsg{RequestId: requestId},
		},
	}
	// Fire on the in-flight forwarded stream (keyed by requestId). authClaims
	// + partition are unused by the agent-side preempt handler (it just sets
	// the pause flag), so empty is fine.
	if err := p.agentForwarder.ForwardContinuation(requestId, nil, "", envelope); err != nil {
		p.logger.Warn("plan preempt: forward continuation failed",
			"plan_id", fields.ID,
			"request_id", requestId,
			"status", fields.Status,
			"error", err,
		)
		return
	}
	p.logger.Info("plan preempt: pass signal sent to agent; turn will stop at next checkpoint",
		"plan_id", fields.ID,
		"request_id", requestId,
		"status", fields.Status,
	)
}

// admitNextForAccount pulls as many of the account's waiting Plans into running
// as there are free slots under its tier cap (#905). Unlimited accounts have
// no live queue under normal operation, but a cap that was lowered-then-raised
// can leave stale waitingForSlot Plans -- passing cap=0 (unlimited) drains them
// all. Best-effort: errors are logged, never fatal.
func (p *PlannerIntegration) admitNextForAccount(ctx context.Context, account string) {
	ent := p.entitlements.Resolve(ctx, account)
	cap := 0 // unlimited -> drain the whole queue
	if !ent.Unlimited {
		cap = ent.MaxConcurrentTasks
	}
	promote := func(c context.Context, planId string) error {
		return p.markPlanRunningFromQueue(c, planId)
	}
	admitted, err := p.admission.AdmitNext(ctx, account, cap, promote)
	if err != nil {
		p.logger.Warn("plan execution: admit-next sweep errored",
			"account_id", account,
			"admitted", admitted,
			"error", err,
		)
		return
	}
	if admitted > 0 {
		p.logger.Info("plan execution: admitted waiting Plans into freed slots",
			"account_id", account,
			"admitted", admitted,
			"cap", cap,
		)
	}
}

// The produceArtifact completion card (the "Deliverable ready -> Open in
// Library" notify card) is emitted from the CoPresent carrier side, NOT
// here. mutationCreateCanvasState + v1:copresent:canvasState are
// copresent-only constructs the planner core build doesn't load, so the
// old planner-side notifier failed at runtime with `function
// "mutationCreateCanvasState" not found`. The card now lands via the
// emitProduceArtifactDoneCanvasCard automation in the copresent DSL,
// which fires on the same v1:planner:plan succeeded transition on a
// carrier node (memql#940).

// executeApprovedPlan does the actual dispatch. Reads the Plan,
// resolves Sofia's SI participant, forwards the agent turn,
// consumes the response, posts the reply, and stamps the Plan
// terminal status.
func (p *PlannerIntegration) executeApprovedPlan(ctx context.Context, planId, requestId string) {
	// Release the in-flight claim taken in handlePlanApprovedForExecution when
	// this dispatch finishes (success or failure), so a legitimate later
	// re-run of the same plan id isn't permanently blocked. (memql#800)
	defer p.executing.Delete(planId)

	start := time.Now()
	p.logger.Info("plan execution: starting", "plan_id", planId)

	plan, err := p.fetchPlanForExecution(ctx, planId)
	if err != nil {
		p.logger.Warn("plan execution: lookup failed",
			"plan_id", planId,
			"error", err,
		)
		return
	}
	if plan.SpaceId == "" || plan.OwnerAgentId == "" || plan.Goal == "" {
		p.logger.Warn("plan execution: missing required fields on Plan; skipping",
			"plan_id", planId,
			"space_id", plan.SpaceId,
			"agent_id", plan.OwnerAgentId,
			"goal_present", plan.Goal != "",
		)
		p.markPlanFailed(ctx, planId, "plan missing spaceId / ownerAgentId / goal")
		return
	}

	// Admission control (epic memql#902). Bound the number of Plans an
	// account may have running concurrently across all its spaces by its
	// billing tier. DEFAULT = UNLIMITED: an unconfigured / enterprise account
	// resolves to unlimited and we skip the gate entirely -- a true no-op, no
	// extra query, no regression. For a finite cap we ask the controller to
	// atomically count the account's running Plans and either admit this one
	// or park it (waitingForSlot) for a free slot (#905). The gate FAILS OPEN:
	// any resolver / controller error proceeds to dispatch rather than wedge
	// the Plan.
	if p.entitlements != nil {
		ent := p.entitlements.Resolve(ctx, plan.RequestedBy)
		if !ent.Unlimited && p.admission != nil {
			demote := func(c context.Context) error {
				return p.markPlanWaitingForSlot(c, planId)
			}
			admitted, admitErr := p.admission.TryAdmit(ctx, plan.RequestedBy, planId, ent.MaxConcurrentTasks, demote)
			switch {
			case admitErr != nil:
				p.logger.Warn("plan execution: admission check errored; admitting (fail-open)",
					"plan_id", planId,
					"account_id", plan.RequestedBy,
					"cap", ent.MaxConcurrentTasks,
					"error", admitErr,
				)
			case !admitted:
				p.logger.Info("plan execution: account at concurrency cap; parked waiting for slot",
					"plan_id", planId,
					"account_id", plan.RequestedBy,
					"cap", ent.MaxConcurrentTasks,
				)
				return
			}
		}
	}

	// participantId stamped on AgentGenerateTurnMsg is left empty.
	// The agent uses it as a self-reference for downstream emits
	// (chat utterances) but the planner-driven dispatch posts the
	// reply via Plan.output.reply (rendered on the canvas card +
	// Tasks panel), not as a chat utterance. The participant lookup
	// would require the v1:cognition:participant concept which is
	// not loaded on the planner binary by design.
	//
	// requestId is passed in from handlePlanApprovedForExecution (stored in
	// the executing map) so handlePlanPreempt can address the in-flight turn
	// for a cross-node "pass" (#906).

	// History carries one synthetic user-role message with the
	// Plan's goal. NOT written to chat -- it's prompt context the
	// agent reads. The chat-visible artifact from this turn is
	// the agent's reply utterance posted below via insertAIReply.
	//
	// For produceArtifact, the owning agent is the SAME assistant whose
	// persona says "for a PRODUCE request, call produceArtifact and end
	// your turn -- do NOT author the deliverable yourself." On THIS turn
	// that instinct is exactly wrong: the assistant IS the executor now,
	// and re-calling produceArtifact just spawns another delegation loop
	// that produces no file (memql#889 -- observed in the wild: the plan
	// reached succeeded but wrote zero generatedOutput rows because the
	// agent re-delegated + published a card instead of writing the file).
	// Reframe the goal as an explicit production directive that overrides
	// the delegation persona and steers the agent to write the deliverable
	// with the workbench tool. scopeElevation plans keep the raw goal.
	turnContent := plan.Goal
	if plan.Kind == produceArtifactPlanKind {
		turnContent = fmt.Sprintf(
			"PRODUCE THIS DELIVERABLE NOW. You are the executor for this task -- "+
				"do NOT call produceArtifact (that would re-delegate and produce nothing). "+
				"Write the file to your workbench with the workbenchHost tool "+
				"(action \"fs_write\"), defaulting to a markdown (.md) file unless the "+
				"deliverable names another format, then end your turn with a short "+
				"acknowledgement. The promoted file lands in the user's Library "+
				"automatically. Do NOT publish the deliverable's content to the canvas "+
				"(no canvasPublish) -- the content belongs ONLY in the file; the canvas "+
				"gets a short 'ready in your Library' notification automatically.\n\n"+
				"Deliverable:\n%s",
			plan.Goal,
		)
	}
	history := []*memqlv1.AgentTurnMessage{{
		Role:    "user",
		Content: turnContent,
	}}

	// Hints carry the post-approval signal + IDs the agent's tool
	// loop needs but can't read from ctx on a system-initiated
	// dispatch. The agent uses these to:
	//   - set turnContext.OwnerUserId when AccessContext.UserId is
	//     missing (system actor on planner-driven turns)
	//   - stamp planId on worker tool args so v1:worker:invocation
	//     rows file under the right Plan
	hints := map[string]string{
		"plan_id": planId,
		"trigger": "plan_approved",
		// Route this turn onto the agent node's NON-STREAMING background
		// executor (memql#896). Plan/task execution is batch work -- nobody
		// is watching tokens arrive, the user gets a completion card when
		// it's done -- so it runs through a request/response tool loop with
		// one overall timeout instead of the interactive streaming path +
		// per-chunk idle watchdog. Running background work through the
		// interactive watchdog is exactly what false-killed slow
		// produceArtifact turns (memql#893); the dedicated lane retires that
		// failure mode. The hint key/value are the agent package's
		// ExecutionLaneHintKey / ExecutionLaneBackground; the planner binary
		// doesn't import the agent package (separate build tags), so the
		// literal strings are duplicated here intentionally.
		"execution_lane": "background",
	}
	// produceArtifact delivers to the workbench/Library ONLY -- its content
	// must never be dumped onto the canvas as a content card (the canvas is
	// for events/notifications; the file is the deliverable). This hint tells
	// the agent to scope canvasPublish OUT of this turn's tool set so the
	// model physically can't publish the file body to the canvas, even as a
	// fallback when something else fails (memql#950). The key/value mirror the
	// agent package's DeliverableSurfaceHintKey / DeliverableSurfaceWorkbench;
	// the planner binary doesn't import the agent package (separate build
	// tags), so the literal strings are duplicated here intentionally (same as
	// execution_lane above).
	if plan.Kind == produceArtifactPlanKind {
		hints["deliverable_surface"] = "workbench"
	}
	// Opt-in "watch agent work" (memql#900). A plan can request live,
	// token-by-token streaming of its execution -- e.g. the user clicked
	// "watch this run" -- by setting input.watchExecution=true at create
	// time. That routes the turn through the INTERACTIVE streaming lane
	// instead of the background default, so a CoPresent "watch agent work"
	// view can render progress as it streams. The default stays background;
	// this only flips the lane for plans that explicitly asked. "interactive"
	// is anything the agent's IsBackgroundLane treats as non-background.
	if plan.WatchExecution {
		hints["execution_lane"] = "interactive"
		p.logger.Info("plan execution: streaming opt-in (watch agent work)",
			"plan_id", planId)
	}
	// Forward the Plan's requestedBy as the owner-user hint when it
	// looks like a canonical user id. The agent's resolveOwnerForAgent
	// fallback can't be trusted here -- the agent record on the
	// agent node may be in a corrupted state with createdBy holding
	// an email rather than the canonical id, which would silently
	// propagate into Plan.requestedBy / canvasState.forUserId on
	// future writes and break the private-canvas filter again.
	// Using the Plan's requestedBy keeps the post-approval turn
	// faithful to the user who originally asked.
	if rb := strings.TrimSpace(plan.RequestedBy); rb != "" {
		hints["owner_user_id"] = rb
	}

	turnMsg := &memqlv1.AgentGenerateTurnMsg{
		RequestId: requestId,
		AgentId:   plan.OwnerAgentId,
		SpaceId:   plan.SpaceId,
		// participantId left empty: the planner doesn't look up the
		// SI participant (cross-domain concept leak). The agent's
		// reply lives on Plan.output.reply and the canvas card; no
		// chat utterance is written from this dispatch.
		History: history,
		Hints:   hints,
	}
	envelope := &memqlv1.MemqlClientMessage{
		MessageId: requestId,
		Payload: &memqlv1.MemqlClientMessage_AgentGenerateTurn{
			AgentGenerateTurn: turnMsg,
		},
	}

	partition := plan.Partition
	if partition == "" {
		partition = "default"
	}

	// Pass the planner's system-actor claims through the forwarder so
	// they ride alongside the AgentGenerateTurnMsg into the agent
	// node's gRPC context. The agent's downstream tool dispatch
	// persists v1:worker:invocation rows via mutationCreateWorkerInvocation,
	// and the engine's pre-insert path requires an actor in context to
	// stamp createdBy. An earlier `nil` here ("system-initiated
	// dispatch; no end-user principal") meant invocation persistence
	// silently failed with "no actor found in context" -- the row
	// never landed, so the planner's queryInvocationsForPlan came
	// back empty, and the Plan was stamped failed even when the
	// worker tool succeeded on the user's machine.
	//
	// systemActorId is unique per integration (see contextWithSystemActor
	// at the bottom of this file) so the audit trail can tell apart
	// "the planner stamped this row" from "cognition stamped this row".
	authClaims := map[string]string{
		"sub":   systemActorId,
		"email": systemActorId,
		"role":  "system",
	}
	respCh, err := p.agentForwarder.Forward(
		ctx,
		requestId,
		node.NodeTypeAgent,
		authClaims,
		partition,
		envelope,
	)
	if err != nil {
		p.logger.Warn("plan execution: forward to agent failed",
			"plan_id", planId,
			"error", err,
		)
		p.markPlanFailed(ctx, planId, fmt.Sprintf("forward to agent: %s", err.Error()))
		return
	}

	result, err := p.consumeAgentTurn(ctx, requestId, respCh)
	if err != nil {
		p.logger.Warn("plan execution: agent turn errored",
			"plan_id", planId,
			"error", err,
		)
		p.markPlanFailed(ctx, planId, fmt.Sprintf("agent turn: %s", err.Error()))
		return
	}

	// Preempted ("passed", epic memql#902 / #906): the turn stopped at a
	// checkpoint and persisted its taskState for resume (#907). The pass
	// action already stamped the Plan paused (which freed its slot + admitted
	// the next via #905), so DON'T mark it succeeded/failed -- that would
	// clobber the paused state. Leave it for resume.
	if result.paused {
		p.logger.Info("plan execution: turn preempted at checkpoint; left paused for resume",
			"plan_id", planId,
			"request_id", requestId,
		)
		return
	}

	replyText := strings.TrimSpace(result.text)

	// "Turn finished without a stream-error" is NOT the same as "the
	// task succeeded." The agent can complete its turn gracefully
	// while admitting it couldn't do the work -- e.g. when the
	// granted scope is below what its planned tool actually needs,
	// or when its mental model picks the wrong tool surface and
	// gives up.
	//
	// The authoritative "did anything actually happen?" signal is
	// v1:worker:invocation: every workerHost / workerComputer call
	// writes a row at completion, and outcome=success only fires
	// when the call returned ok=true. Query for invocations on this
	// Plan id: at least one success means the agent did the work
	// (Plan succeeded with the reply text); zero successes means
	// the agent talked about the task but didn't dispatch the tool
	// (Plan failed with the agent's text as the error message so
	// the user sees WHY it didn't run).
	planSucceeded, invocationCount, successCount := p.workerInvocationOutcomeForPlan(ctx, planId)
	// The worker-invocation gate is the right success signal for COMPUTER-USE
	// work (scopeElevation): a remote workerHost/workerComputer call can return
	// ok=false, so "turn finished" != "task done". But produceArtifact (#788)
	// produces its deliverable via the WORKBENCH (in-process fs_write, promoted
	// to the Library) and typically dispatches NO worker tool, so it would
	// always have zero worker invocations and be wrongly failed. For
	// produceArtifact the authoritative signal is a clean agent turn -- a turn
	// error already returned early above with markPlanFailed, so reaching here
	// means the agent completed; the workbench write either succeeded (and was
	// promoted) or the agent surfaced the problem in its reply. (memql#800)
	succeeded := planSucceeded
	if plan.Kind == produceArtifactPlanKind {
		// produceArtifact produces its deliverable via the WORKBENCH
		// (in-process fs_write, promoted to the Library), not a worker tool,
		// so the worker-invocation gate is always zero here and cannot be the
		// success signal. The authoritative signal is whether a
		// v1:library:generatedOutput row actually landed for this plan --
		// promoteWorkbenchOutput stamps producedByPlanId on it ONLY after a
		// successful fs_write promotion. A turn that completed gracefully but
		// produced NOTHING -- e.g. the workbench write was rejected and the
		// agent merely apologised in prose (the exact memql#938 signature) --
		// must NOT be reported as success. (memql#939)
		produced, queryOK := p.artifactProducedForPlan(ctx, planId, plan.RequestedBy)
		if queryOK {
			succeeded = produced
		} else {
			// Inconclusive lookup (engine misconfigured / query error /
			// unresolved owner): fall back to the prior clean-turn heuristic
			// rather than false-failing a real deliverable on an infra problem.
			// The empty-reply guard below still catches the stalled-turn case.
			succeeded = true
		}
	}
	// A produceArtifact turn that comes back EMPTY produced nothing. The
	// classic signature is the agent's SI stream stalling and the idle
	// watchdog killing it after exhausting retries -- the failure is swallowed
	// upstream and surfaces here as a graceful-but-empty turn (no reply text,
	// no tool output). Stamping that "succeeded" lies to the user: the
	// completion card announces "your file is ready in the Library" when no
	// file was ever written. A real production turn always ends with a
	// respondToUser ack, so a non-empty reply is the floor for success. Treat
	// an empty produceArtifact turn as an honest failure the user can see and
	// retry instead of a silent fake-success. (memql#893)
	if plan.Kind == produceArtifactPlanKind && replyText == "" {
		succeeded = false
	}
	if succeeded {
		// Reply text lands on Plan.output.reply via markPlanSucceeded.
		// The canvas card + Tasks panel render that field.
		p.markPlanSucceeded(ctx, planId, replyText)
	} else {
		// The work didn't complete. The agent's reply text (when present) is
		// its explanation of why; surface it as the Plan's errorMessage so the
		// Tasks panel + canvas card show the actual reason instead of a
		// generic "failed."
		errorMessage := replyText
		if errorMessage == "" {
			if plan.Kind == produceArtifactPlanKind {
				errorMessage = "The deliverable couldn't be produced -- the agent didn't finish the turn (it may have timed out before writing the file). Please try again."
			} else {
				errorMessage = "agent finished the turn without dispatching a worker tool successfully"
			}
		}
		p.markPlanFailed(ctx, planId, errorMessage)
	}

	p.logger.Info("plan execution: completed",
		"plan_id", planId,
		"elapsed_ms", time.Since(start).Milliseconds(),
		"reply_chars", len(replyText),
		"worker_invocations", invocationCount,
		"worker_successes", successCount,
		// worker_outcome is the worker-invocation signal alone; plan_outcome
		// is the FINAL decision the user sees (for produceArtifact it reflects
		// the generatedOutput-evidence gate, not the always-zero worker
		// signal). Logging only planSucceeded here used to read "failed" while
		// the user was told "done" -- they now agree. (memql#939)
		"worker_outcome", map[bool]string{true: "succeeded", false: "failed"}[planSucceeded],
		"plan_outcome", map[bool]string{true: "succeeded", false: "failed"}[succeeded],
	)
}

// workerInvocationOutcomeForPlan returns whether the Plan should be
// considered successful. The v1:worker:invocation rows are the
// authoritative record of "did the agent actually call the worker
// tool" -- a row with outcome=success means a workerHost or
// workerComputer call returned ok=true and the work landed on the
// user's machine. Zero rows or all-failed rows means the agent
// talked but never executed.
//
// Returns (succeeded, totalInvocations, successInvocations).
//
// The query is best-effort: if it errors (concept not loaded,
// engine misconfigured), we DEFAULT TO FAILED so a stuck path is
// surfaced in the UI rather than silently marked succeeded. The
// counts on the WARN log give operators something to grep when
// the fail rate looks off.
func (p *PlannerIntegration) workerInvocationOutcomeForPlan(ctx context.Context, planId string) (succeeded bool, total, successes int) {
	if p.engine == nil {
		p.logger.Warn("plan execution: engine not configured for invocation lookup; defaulting to failed",
			"plan_id", planId,
		)
		return false, 0, 0
	}
	q := fmt.Sprintf(`queryInvocationsForPlan({planId:%q})`, planId)
	res, err := p.engine.Execute(ctx, q)
	if err != nil {
		p.logger.Warn("plan execution: invocation lookup failed; defaulting to failed",
			"plan_id", planId,
			"error", err,
		)
		return false, 0, 0
	}
	rows := invocationRowsFromExecuteResult(res)
	for _, row := range rows {
		total++
		if row.Outcome == "success" {
			successes++
		}
	}
	return successes > 0, total, successes
}

// artifactProducedForPlan reports whether a produceArtifact turn
// actually wrote a deliverable: at least one v1:library:generatedOutput
// row stamped with this planId. promoteWorkbenchOutput writes that row
// ONLY after a successful workbench fs_write promotion, so its presence
// is the authoritative "did anything get produced?" signal -- the
// produceArtifact analogue of workerInvocationOutcomeForPlan.
//
// The generatedOutput concept is owner-scoped (reads gate on
// ownerUserId==actor.userId), so the lookup stamps the plan's owner as
// the acting user via ownerActorContext and filters on producedByPlanId.
//
// Returns (produced, queryOK):
//   - queryOK=true,  produced=true  -> a real deliverable landed.
//   - queryOK=true,  produced=false -> the turn produced nothing (honest
//     failure the user can see + retry).
//   - queryOK=false                 -> the lookup could not run (engine
//     misconfigured / query error / no resolvable owner); INCONCLUSIVE,
//     so the caller does NOT fail an otherwise-clean turn on it.
func (p *PlannerIntegration) artifactProducedForPlan(ctx context.Context, planId, ownerUserId string) (produced, queryOK bool) {
	if p.engine == nil || strings.TrimSpace(planId) == "" {
		return false, false
	}
	owner := strings.TrimSpace(ownerUserId)
	if owner == "" {
		// No owner to impersonate -> the owned read can't run. Inconclusive.
		return false, false
	}
	q := fmt.Sprintf(`queryGeneratedOutputsForPlan({planId:%q})`, planId)
	res, err := p.engine.Execute(ownerActorContext(ctx, owner), q)
	if err != nil {
		p.logger.Warn("plan execution: generatedOutput lookup failed; treating as inconclusive",
			"plan_id", planId,
			"error", err,
		)
		return false, false
	}
	return generatedOutputCountFromExecuteResult(res) > 0, true
}

// generatedOutputCountFromExecuteResult counts the rows queryGeneratedOutputsForPlan
// returned, unpacking shape() output the same way invocationRowsFromExecuteResult
// does: single-row -> bare map, multi-row -> []any, empty -> []any{}.
func generatedOutputCountFromExecuteResult(res any) int {
	resultMap, ok := res.(map[string]any)
	if !ok {
		return 0
	}
	switch data := resultMap["data"].(type) {
	case map[string]any:
		return 1
	case []any:
		return len(data)
	default:
		return 0
	}
}

// invocationRow is the slim projection workerInvocationOutcomeForPlan
// reads off queryInvocationsForPlan. We only need outcome for the
// success-count check; the rest of workerInvocationFull (tool,
// action, durationMs, etc.) is irrelevant here.
type invocationRow struct {
	Outcome string
}

// invocationRowsFromExecuteResult unpacks the shape() output the
// same way planRowsFromExecuteResult does -- single-row -> bare
// map, multi-row -> []any, empty -> []any{}. The plan's invocation
// list is typically 1-3 rows so all three shapes show up in
// practice.
func invocationRowsFromExecuteResult(res any) []invocationRow {
	resultMap, ok := res.(map[string]any)
	if !ok {
		return nil
	}
	var raw []map[string]any
	switch data := resultMap["data"].(type) {
	case map[string]any:
		raw = append(raw, data)
	case []any:
		for _, item := range data {
			if m, ok := item.(map[string]any); ok {
				raw = append(raw, m)
			}
		}
	case []map[string]any:
		raw = data
	}
	out := make([]invocationRow, 0, len(raw))
	for _, m := range raw {
		row := invocationRow{}
		if v, ok := m["outcome"].(string); ok {
			row.Outcome = v
		}
		out = append(out, row)
	}
	return out
}

// fetchPlanForExecution reads the Plan via queryPlanById and
// projects the fields executeApprovedPlan needs. queryPlanById is
// shape()-based so the result lives on the data axis (not bundle).
func (p *PlannerIntegration) fetchPlanForExecution(ctx context.Context, planId string) (planExecutionRow, error) {
	if p.engine == nil {
		return planExecutionRow{}, fmt.Errorf("engine not configured")
	}
	q := fmt.Sprintf(`queryPlanById({planId:%q})`, planId)
	res, err := p.engine.Execute(ctx, q)
	if err != nil {
		return planExecutionRow{}, err
	}
	if res == nil {
		return planExecutionRow{}, fmt.Errorf("plan not found")
	}
	rows := planRowsFromExecuteResult(res)
	if len(rows) == 0 {
		return planExecutionRow{}, fmt.Errorf("plan %q not found", planId)
	}
	return rows[0], nil
}

// consumeAgentTurn reads deltas + complete from the forwarder
// response channel. Returns the assembled reply text + structured
// citations / retrieved chunks.
//
// Slimmer than cognition.consumeAgentTurnStream:
//   - No mutationEmitTextChunk relay -- the planner-driven dispatch
//     doesn't have a paired chat-streaming bubble on the frontend
//     (the plan is system-initiated, not user-initiated). The
//     reply lands as a single committed utterance via
//     insertAIReply once the turn completes.
//   - No client-tool-call relay -- the scope-elevation flow only
//     uses workerHost / workerComputer (server-executed). If a
//     future kind=scopeElevation triggered an agent that calls a
//     client-executed tool, that ClientToolCall would arrive here
//     and we'd warn-and-skip; for now the use case doesn't exist.
func (p *PlannerIntegration) consumeAgentTurn(
	ctx context.Context,
	requestId string,
	respCh <-chan *memqlv1.MemqlServerMessage,
) (agentTurnResult, error) {
	var fullText strings.Builder
	for {
		select {
		case <-ctx.Done():
			return agentTurnResult{text: fullText.String()}, ctx.Err()
		case serverMsg, ok := <-respCh:
			if !ok {
				return agentTurnResult{text: strings.TrimSpace(fullText.String())}, nil
			}
			switch payload := serverMsg.GetPayload().(type) {
			case *memqlv1.MemqlServerMessage_AgentGenerateTurnDelta:
				if t := payload.AgentGenerateTurnDelta.GetText(); t != nil {
					fullText.WriteString(t.GetText())
				}
			case *memqlv1.MemqlServerMessage_ClientToolCall:
				p.logger.Warn("plan execution: client-tool call ignored (not supported on planner-driven turns)",
					"request_id", requestId,
				)
			case *memqlv1.MemqlServerMessage_AgentGenerateTurnComplete:
				complete := payload.AgentGenerateTurnComplete
				if complete.GetError() != nil {
					return agentTurnResult{}, fmt.Errorf("agent turn error: %s (%s)",
						complete.GetError().GetMessage(),
						complete.GetError().GetCode())
				}
				final := strings.TrimSpace(complete.GetFinalText())
				if final == "" {
					final = strings.TrimSpace(fullText.String())
				}
				return agentTurnResult{
					text:      final,
					citations: complete.GetCitations(),
					retrieved: complete.GetRetrieved(),
					paused:    complete.GetPaused(),
				}, nil
			case *memqlv1.MemqlServerMessage_QueryError:
				e := payload.QueryError.GetError()
				return agentTurnResult{}, fmt.Errorf("forward error: %s (%s)", e.GetMessage(), e.GetCode())
			}
		}
	}
}

// markPlanSucceeded stamps status=succeeded + completedAt=now,
// rolling the agent's reply text into output.reply for the Tasks
// panel's expand-drawer view.
// planExternallyHalted reports whether the Plan's CURRENT status was set
// out-of-band while a dispatch turn was in flight -- paused (incl. a
// fairness pass, memql#908), re-queued for a slot (waitingForSlot, #905), or
// cancelled. The background dispatch must NOT stamp a terminal status over
// any of these: a passed plan whose turn happens to finish should stay
// paused for resume (memql#907), not flip to succeeded/failed. Best-effort:
// a lookup miss returns false so a normal turn still stamps its result.
func (p *PlannerIntegration) planExternallyHalted(ctx context.Context, planId string) bool {
	row, err := p.fetchPlanForExecution(ctx, planId)
	if err != nil {
		return false
	}
	switch row.Status {
	case "paused", "waitingForSlot", "cancelled":
		return true
	}
	return false
}

func (p *PlannerIntegration) markPlanSucceeded(ctx context.Context, planId, replyText string) {
	if p.planExternallyHalted(ctx, planId) {
		p.logger.Info("plan execution: skipping succeeded stamp; plan halted out-of-band (paused/passed/cancelled)",
			"plan_id", planId)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	output := map[string]any{}
	if replyText != "" {
		output["reply"] = replyText
	}
	outputJSON, _ := json.Marshal(output)
	q := fmt.Sprintf(
		`mutationUpdatePlanStatus({planId:%q, status:"succeeded", completedAt:%q, output:%s})`,
		planId, now, string(outputJSON),
	)
	if _, err := p.engine.Execute(ctx, q); err != nil {
		p.logger.Warn("plan execution: markPlanSucceeded failed",
			"plan_id", planId,
			"error", err,
		)
	}
}

// markPlanFailed stamps status=failed + completedAt=now with the
// supplied error message.
func (p *PlannerIntegration) markPlanFailed(ctx context.Context, planId, errorMessage string) {
	if p.planExternallyHalted(ctx, planId) {
		p.logger.Info("plan execution: skipping failed stamp; plan halted out-of-band (paused/passed/cancelled)",
			"plan_id", planId)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	q := fmt.Sprintf(
		`mutationUpdatePlanStatus({planId:%q, status:"failed", completedAt:%q, errorMessage:%q})`,
		planId, now, errorMessage,
	)
	if _, err := p.engine.Execute(ctx, q); err != nil {
		p.logger.Warn("plan execution: markPlanFailed failed",
			"plan_id", planId,
			"error", err,
		)
	}
}

// markPlanWaitingForSlot parks a Plan in the per-account waiting queue
// (epic memql#902 / #904). The user clicked Run -- the Plan is already
// running -- but the account is at its tier's concurrency cap, so the Plan
// holds at waitingForSlot until a slot frees and #905 admits the next one.
// Distinct from queued (awaiting the user's Run) and paused (the user paused
// it): the system is throttling concurrency. Returns the engine error so the
// admission demote callback can fail open on a write failure.
func (p *PlannerIntegration) markPlanWaitingForSlot(ctx context.Context, planId string) error {
	q := fmt.Sprintf(`mutationUpdatePlanStatus({planId:%q, status:"waitingForSlot"})`, planId)
	if _, err := p.engine.Execute(ctx, q); err != nil {
		p.logger.Warn("plan execution: markPlanWaitingForSlot failed",
			"plan_id", planId,
			"error", err,
		)
		return err
	}
	return nil
}

// markPlanRunningFromQueue promotes a parked Plan back to running when a slot
// frees (epic memql#902 / #905). The status=running transition re-emits the
// graph.node.updated event that handlePlanApprovedForExecution consumes, so the
// Plan re-enters executeApprovedPlan and dispatches its turn (which it never
// did -- a Plan is parked BEFORE the turn is forwarded). Re-admission there
// re-checks the cap, but AdmitNext only promotes within the cap so it passes.
// Returns the engine error so the AdmitNext promote loop can stop on failure.
func (p *PlannerIntegration) markPlanRunningFromQueue(ctx context.Context, planId string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	q := fmt.Sprintf(`mutationUpdatePlanStatus({planId:%q, status:"running", startedAt:%q})`, planId, now)
	if _, err := p.engine.Execute(ctx, q); err != nil {
		p.logger.Warn("plan execution: markPlanRunningFromQueue failed",
			"plan_id", planId,
			"error", err,
		)
		return err
	}
	return nil
}

// extractPlanRoutingFields pulls planId / kind / status off the
// event payload. Tolerates both the flattened-fields shape and the
// nested-payload shape events.Event can carry depending on emitter.
func extractPlanRoutingFields(event events.Event) planRoutingFields {
	out := planRoutingFields{}
	out.ID, _ = event.Payload["nodeId"].(string)
	if out.ID == "" {
		out.ID, _ = event.Payload["id"].(string)
	}
	out.Kind, _ = event.Payload["kind"].(string)
	out.Status, _ = event.Payload["status"].(string)
	out.RequestedBy, _ = event.Payload["requestedBy"].(string)
	if nested, ok := event.Payload["payload"].(map[string]any); ok {
		if out.ID == "" {
			out.ID, _ = nested["id"].(string)
		}
		if out.Kind == "" {
			out.Kind, _ = nested["kind"].(string)
		}
		if out.Status == "" {
			out.Status, _ = nested["status"].(string)
		}
		if out.RequestedBy == "" {
			out.RequestedBy, _ = nested["requestedBy"].(string)
		}
	}
	out.ID = strings.TrimSpace(out.ID)
	out.Kind = strings.TrimSpace(out.Kind)
	out.Status = strings.TrimSpace(out.Status)
	out.RequestedBy = strings.TrimSpace(out.RequestedBy)
	return out
}

// planRowsFromExecuteResult unpacks queryPlanById's shape() result
// into planExecutionRow values.
//
// shape() output is shape-shifted by applyShapeTemplate based on
// match count: a multi-row match returns []any, a single-row match
// returns the lone map[string]any UNWRAPPED, and an empty match
// returns []any{}. queryPlanById is by-id (one match expected), so
// the single-object path is the hot path -- treat the array path as
// a defensive fallback for callers that filter on something that
// could legitimately produce >1 hit.
func planRowsFromExecuteResult(res any) []planExecutionRow {
	resultMap, ok := res.(map[string]any)
	if !ok {
		return nil
	}
	var raw []map[string]any
	switch data := resultMap["data"].(type) {
	case map[string]any:
		// Single-row shape() result -- unwrapped object.
		raw = append(raw, data)
	case []any:
		for _, item := range data {
			if m, ok := item.(map[string]any); ok {
				raw = append(raw, m)
			}
		}
	case []map[string]any:
		raw = data
	}
	out := make([]planExecutionRow, 0, len(raw))
	for _, m := range raw {
		row := planExecutionRow{}
		if id, ok := m["id"].(string); ok {
			row.ID = id
		}
		if v, ok := m["partition"].(string); ok {
			row.Partition = v
		}
		if v, ok := m["kind"].(string); ok {
			row.Kind = v
		}
		if v, ok := m["status"].(string); ok {
			row.Status = v
		}
		if v, ok := m["goal"].(string); ok {
			row.Goal = v
		}
		if v, ok := m["spaceId"].(string); ok {
			row.SpaceId = v
		}
		if v, ok := m["ownerAgentId"].(string); ok {
			row.OwnerAgentId = v
		}
		if v, ok := m["requestedBy"].(string); ok {
			row.RequestedBy = v
		}
		// Opt-in "watch agent work" (memql#900). Read input.watchExecution
		// off the projected Plan.input object. The planFull shape flattens
		// payload.input to the top-level "input" key. Tolerate the field
		// being absent (the common case) or a non-bool; default false.
		if input, ok := m["input"].(map[string]any); ok {
			if w, ok := input["watchExecution"].(bool); ok {
				row.WatchExecution = w
			}
		}
		out = append(out, row)
	}
	return out
}

// contextWithSystemActor stamps a system-actor token + claims on
// ctx so engine writes (markPlanSucceeded, markPlanFailed,
// insertAIReply) attribute their createdBy column to a stable
// system principal. Mirrors the cognition integration's helper of
// the same name; the systemActorId is unique per integration so
// audit logs can tell apart "the planner stamped this row" from
// "cognition stamped this row".
func contextWithSystemActor(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	claims := map[string]any{
		"sub":   systemActorId,
		"email": systemActorId,
		"role":  "system",
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	return auth.ContextWithToken(ctx, token)
}
