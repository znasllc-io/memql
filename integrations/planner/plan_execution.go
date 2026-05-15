package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/visionarys-io/memql/component/auth"
	"github.com/visionarys-io/memql/component/events"
	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
	"github.com/visionarys-io/memql/component/node"
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
// Subscription: graph.node.updated.*.v1:planner:plan, filtered to
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
// a graph.node.updated.*.v1:planner:plan event payload to decide
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
}

// agentTurnResult mirrors cognition.agentReplyResult -- the
// planner's local copy so this package doesn't depend on the
// cognition package.
type agentTurnResult struct {
	text      string
	citations []*memqlv1.AgentTurnCitation
	retrieved []*memqlv1.AgentRetrievedChunk
}

// handlePlanApprovedForExecution is the event-bus subscriber.
// Filters to scope-elevation Plans transitioning to running, then
// kicks off the dispatch in a goroutine so the bus loop doesn't
// block on the multi-second tool-calling turn.
func (p *PlannerIntegration) handlePlanApprovedForExecution(event events.Event) {
	fields := extractPlanRoutingFields(event)
	if fields.Kind != "scopeElevation" || fields.Status != "running" || fields.ID == "" {
		return
	}

	if p.agentForwarder == nil {
		p.logger.Warn("plan execution: agent forwarder not configured; skipping dispatch",
			"plan_id", fields.ID,
		)
		return
	}

	// Run the dispatch in a fresh goroutine. The bus loop processes
	// events synchronously per-subscriber, and a tool-calling turn
	// takes seconds (sometimes minutes); blocking the bus would
	// stall every other subscriber's events behind us.
	bgCtx := contextWithSystemActor(context.Background())
	go p.executeApprovedPlan(bgCtx, fields.ID)
}

// executeApprovedPlan does the actual dispatch. Reads the Plan,
// resolves Sofia's SI participant, forwards the agent turn,
// consumes the response, posts the reply, and stamps the Plan
// terminal status.
func (p *PlannerIntegration) executeApprovedPlan(ctx context.Context, planId string) {
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

	// participantId stamped on AgentGenerateTurnMsg is left empty.
	// The agent uses it as a self-reference for downstream emits
	// (chat utterances) but the planner-driven dispatch posts the
	// reply via Plan.output.reply (rendered on the canvas card +
	// Tasks panel), not as a chat utterance. The participant lookup
	// would require the v1:cognition:participant concept which is
	// not loaded on the planner binary by design.
	requestId := uuid.NewString()

	// History carries one synthetic user-role message with the
	// Plan's goal. NOT written to chat -- it's prompt context the
	// agent reads. The chat-visible artifact from this turn is
	// the agent's reply utterance posted below via insertAIReply.
	history := []*memqlv1.AgentTurnMessage{{
		Role:    "user",
		Content: plan.Goal,
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
	if planSucceeded {
		// Reply text lands on Plan.output.reply via markPlanSucceeded.
		// The canvas card + Tasks panel render that field.
		p.markPlanSucceeded(ctx, planId, replyText)
	} else {
		// Worker tool never ran successfully. The agent's reply text
		// is its explanation of why; surface it as the Plan's
		// errorMessage so the Tasks panel + canvas card show the
		// actual reason instead of a generic "failed."
		errorMessage := replyText
		if errorMessage == "" {
			errorMessage = "agent finished the turn without dispatching a worker tool successfully"
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
