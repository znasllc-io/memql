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
	ID     string
	Kind   string
	Status string
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
	if _, alreadyExecuting := p.executing.LoadOrStore(fields.ID, true); alreadyExecuting {
		return
	}

	// Run the dispatch in a fresh goroutine. The bus loop processes
	// events synchronously per-subscriber, and a tool-calling turn
	// takes seconds (sometimes minutes); blocking the bus would
	// stall every other subscriber's events behind us.
	bgCtx := contextWithSystemActor(context.Background())
	go p.executeApprovedPlan(bgCtx, fields.ID)
}

// handleProduceArtifactCompletion is the event-bus subscriber that closes the
// async loop for the conversational produce-artifact path (memql#792). When a
// produceArtifact Plan (spawned by the Assistant's produceArtifact tool,
// memql#788) reaches terminal success, the deliverable has landed in the user's
// Library -- but the user is back in the conversation and doesn't know. This
// emits a notify canvas card (so the bell pings) telling them their file is
// ready and deep-linking the Library. Fires regardless of WHICH code path
// stamped the Plan succeeded.
func (p *PlannerIntegration) handleProduceArtifactCompletion(event events.Event) {
	fields := extractPlanRoutingFields(event)
	if fields.Kind != produceArtifactPlanKind || fields.Status != "succeeded" || fields.ID == "" {
		return
	}
	bgCtx := contextWithSystemActor(context.Background())
	go p.notifyProduceArtifactComplete(bgCtx, fields.ID)
}

// notifyProduceArtifactComplete loads the completed Plan and emits a
// private, notify-importance canvas card announcing the deliverable. Best-
// effort: a failure here must never affect the Plan's terminal state.
func (p *PlannerIntegration) notifyProduceArtifactComplete(ctx context.Context, planId string) {
	plan, err := p.fetchPlanForExecution(ctx, planId)
	if err != nil {
		p.logger.Warn("produceArtifact completion: fetch plan failed",
			"plan_id", planId, "error", err)
		return
	}
	requestedBy := strings.TrimSpace(plan.RequestedBy)
	spaceId := strings.TrimSpace(plan.SpaceId)
	if requestedBy == "" || spaceId == "" {
		p.logger.Warn("produceArtifact completion: missing requestedBy/spaceId; skipping notify",
			"plan_id", planId)
		return
	}

	goal := strings.TrimSpace(plan.Goal)
	body := "Your file is ready in your Library."
	if goal != "" {
		body = fmt.Sprintf("%s\n\nIt's ready in your Library.", goal)
	}
	// kind="document" notify card. The data shape MUST match the frontend
	// DocumentCard contract (copresent DocumentCardData): the markdown body
	// rides on `source` (NOT `body`), an optional `category` label renders
	// above the title, and navigation deep-links ride on
	// `links: [{label, href}]` (NOT a flat `link` string) -- the card pushes
	// each href via react-router navigate(). `/space?panel=library` opens the
	// Library panel in the user's active space. producedByPlanId is carried
	// for a future build to resolve + highlight the exact artifact
	// (memql#792 follow-up); the frontend ignores unknown fields. (memql#890)
	cardData := map[string]any{
		"format":   "markdown",
		"category": "Task Done",
		"title":    "Deliverable ready",
		"source":   body,
		"links": []map[string]any{
			{"label": "Open in Library", "href": "/space?panel=library"},
		},
		"producedByPlanId": planId,
	}
	args := map[string]any{
		"canvasStateId": fmt.Sprintf("produce-artifact-done-%s", planId),
		"space":         spaceId,
		"kind":          "document",
		"data":          cardData,
		"visibility":    "private",
		"forUserId":     requestedBy,
		"importance":    "notify",
		"actor": map[string]any{
			"kind": "system",
		},
	}
	payload, err := json.Marshal(args)
	if err != nil {
		p.logger.Warn("produceArtifact completion: marshal card failed", "plan_id", planId, "error", err)
		return
	}
	call := fmt.Sprintf(`mutationCreateCanvasState(%s)`, string(payload))
	if _, err := p.engine.Execute(contextWithSystemActor(ctx), call); err != nil {
		p.logger.Warn("produceArtifact completion: emit notify card failed",
			"plan_id", planId, "error", err)
		return
	}
	p.logger.Info("produceArtifact completion: notify card emitted",
		"plan_id", planId, "space_id", spaceId, "for_user", requestedBy)
}

// executeApprovedPlan does the actual dispatch. Reads the Plan,
// resolves Sofia's SI participant, forwards the agent turn,
// consumes the response, posts the reply, and stamps the Plan
// terminal status.
func (p *PlannerIntegration) executeApprovedPlan(ctx context.Context, planId string) {
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
	requestId := id.NewShortId()

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
				"automatically.\n\nDeliverable:\n%s",
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
	succeeded := planSucceeded || plan.Kind == produceArtifactPlanKind
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
		"plan_outcome", map[bool]string{true: "succeeded", false: "failed"}[planSucceeded],
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
func (p *PlannerIntegration) markPlanSucceeded(ctx context.Context, planId, replyText string) {
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
	}
	out.ID = strings.TrimSpace(out.ID)
	out.Kind = strings.TrimSpace(out.Kind)
	out.Status = strings.TrimSpace(out.Status)
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
