package cognition

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// plan_feedback.go is the cognition (assistant) side of the generic
// plan-feedback loop (epic memql#1404 / child #1406). It complements the
// planner-side intake foundation (#1405, attachPlanFeedback) with the
// conversational surface: when a Plan in a space enters awaitingFeedback, the
// space assistant posts a chat message stating what feedback is needed; a user
// chat reply answering it is classified and routed into the intake mutation so
// the Plan resumes -- a conversational alternative to the feedback card.
//
// Two halves:
//
//   1. ANNOUNCE (handlePlanUpdatedForFeedback): event-driven on
//      graph.node.updated.v1:planner:plan. When a Plan transitions to
//      awaitingFeedback carrying a feedbackRequest.question, the assistant
//      posts the question into the Plan's space chat. The plan-updated event
//      is broadcast to BOTH cognition replicas, so a cross-replica advisory-
//      lock gate (dispatchGate.tryAnnounceFeedback, mirroring the #1386 greet
//      gate) plus a durable read-before-write dedup
//      (feedbackAnnouncementForPlan) make the post exactly-once.
//
//   2. ROUTE (tryRouteUtteranceAsPlanFeedback): called from
//      handleUtteranceForCognition BEFORE the normal dispatch. When the space
//      has an awaitingFeedback Plan, an LLM classifier
//      (cognitionFeedbackRoute) decides whether the user's message answers the
//      pending question; on a confident hit it calls attachPlanFeedback
//      under the cognition system actor (the #1405 guard permits cluster-owner
//      / system actors acting on the user's behalf) and posts a brief
//      confirmation. The Plan resumes via #1405's cross-replica exactly-once
//      path -- cognition adds NO second resume path.

// feedbackRouteConfidenceThreshold is the minimum classifier confidence for
// treating a user message as the answer to an awaitingFeedback Plan's
// question. A false positive routes the wrong text into the Plan and resumes
// it incorrectly, so the floor is deliberately conservative; below it the
// utterance falls through to normal dispatch and the user can use the card
// instead (or rephrase).
const feedbackRouteConfidenceThreshold = 0.6

// planStatusAwaitingFeedback is the Plan status that opens the assistant-
// mediated feedback surface. Mirrors the engine guard's constant
// (component/memql/planner_feedback_validation.go) without importing it.
const planStatusAwaitingFeedback = "awaitingFeedback"

// awaitingFeedbackPlan is the projection of a Plan parked in awaitingFeedback
// that both halves need: the id to attach feedback to, the space it lives in,
// and the pending question + options + reason to classify against / announce.
type awaitingFeedbackPlan struct {
	ID             string
	PartitionId    string
	Status         string
	FeedbackReason string
	Question       string
	Options        []map[string]any
	RequestedBy    string
}

// feedbackRouteResult is the structured-output shape returned by the
// cognitionFeedbackRoute prompt. Adding fields is safe; removing is breaking.
type feedbackRouteResult struct {
	IsFeedback bool    `json:"isFeedback"`
	Feedback   string  `json:"feedback"`
	Confidence float64 `json:"confidence"`
	Reasoning  string  `json:"reasoning"`
}

// cognitionFeedbackRouteSchemaJSON constrains the classifier output. strict=true
// is enforced at the InvokeAIStructured boundary.
const cognitionFeedbackRouteSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "isFeedback": {
      "type": "boolean",
      "description": "True when the user's message answers the pending feedback question."
    },
    "feedback": {
      "type": "string",
      "description": "The free-text answer to fold into the Plan when isFeedback is true; empty otherwise."
    },
    "confidence": {
      "type": "number",
      "description": "Self-assessed certainty 0.0-1.0."
    },
    "reasoning": {
      "type": "string",
      "description": "One short sentence; human debugging only."
    }
  },
  "required": ["isFeedback", "feedback", "confidence", "reasoning"]
}`

// handlePlanUpdatedForFeedback is the announce-half event handler, subscribed
// to graph.node.updated.v1:planner:plan in Start. It is a no-op for any plan
// update that is not a fresh awaitingFeedback transition carrying a question.
func (c *CognitionIntegration) handlePlanUpdatedForFeedback(event events.Event) {
	if c == nil || c.engine == nil {
		return
	}

	plan := planFromUpdateEvent(event)
	if plan == nil {
		return
	}
	if plan.Status != planStatusAwaitingFeedback {
		return
	}
	// No question -> nothing for the assistant to announce conversationally.
	// (Scope-elevation / approval parks always carry a question; a bare park
	// with no question is left to the card surface.)
	if strings.TrimSpace(plan.Question) == "" {
		return
	}
	if strings.TrimSpace(plan.PartitionId) == "" {
		return
	}

	// The plan-updated event is broadcast to BOTH cognition replicas. Announce
	// in a background goroutine under the cross-replica gate so the handler
	// returns promptly and exactly one replica posts.
	go c.announceFeedbackRequest(contextWithSystemActor(context.Background()), plan)
}

// announceFeedbackRequest posts the Plan's pending feedback question into its
// space chat as an assistant utterance, gated cross-replica + deduped so it
// fires exactly once per (space, plan).
func (c *CognitionIntegration) announceFeedbackRequest(ctx context.Context, plan *awaitingFeedbackPlan) {
	if c == nil || plan == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Cross-replica gate: exactly one replica wins the (space, plan) lock and
	// proceeds; the loser bails. Fails SAFE (proceeds) on any infra error,
	// falling through to the read-before-write dedup below.
	proceed, release, gateErr := c.dispatchGate.tryAnnounceFeedback(ctx, plan.PartitionId, plan.ID)
	if gateErr != nil {
		c.Logger.Warn("cognition: feedback announce gate errored; failing safe and proceeding",
			"error", gateErr, "partitionId", plan.PartitionId, "planId", plan.ID)
	}
	if !proceed {
		c.Logger.Debug("cognition: another replica owns this plan-feedback announcement, skipping",
			"partitionId", plan.PartitionId, "planId", plan.ID)
		return
	}
	defer release()

	// Durable read-before-write dedup: if the announcement is already in the
	// chat (e.g. a prior plan-updated event for the same still-parked Plan),
	// do not post it again. This is the fail-safe backstop behind the gate.
	if c.queryHasFeedbackAnnouncement(ctx, plan.ID) {
		c.Logger.Debug("cognition: feedback announcement already posted, skipping",
			"partitionId", plan.PartitionId, "planId", plan.ID)
		return
	}

	aiParticipant, err := c.findAIParticipant(ctx, plan.PartitionId)
	if err != nil || aiParticipant == nil || strings.TrimSpace(aiParticipant.ID) == "" {
		c.Logger.Warn("cognition: no AI participant to announce plan feedback; skipping",
			"error", err, "partitionId", plan.PartitionId, "planId", plan.ID)
		return
	}

	message := buildFeedbackAnnouncement(plan)
	// Stamp the plan id onto the source so feedbackAnnouncementForPlan can
	// dedup, and mark the utterance kind so the frontend can render an inline
	// reply affordance alongside the conversational prompt.
	source := map[string]string{
		"outputMethod":           "text",
		"kind":                   "planFeedbackRequest",
		"feedbackAnnouncePlanId": plan.ID,
		"feedbackReason":         plan.FeedbackReason,
	}
	if err := c.insertAIResponse(ctx, plan.PartitionId, aiParticipant, "", "", message, source, nil, nil); err != nil {
		c.Logger.Warn("cognition: failed to post plan-feedback announcement",
			"error", err, "partitionId", plan.PartitionId, "planId", plan.ID)
		return
	}
	c.Logger.Info("cognition: posted assistant plan-feedback announcement",
		"partitionId", plan.PartitionId, "planId", plan.ID, "feedbackReason", plan.FeedbackReason)
}

// tryRouteUtteranceAsPlanFeedback is the route-half entrypoint, called from
// handleUtteranceForCognition before the normal turn dispatch. It returns
// handled=true when the user's message was recognized as feedback for an
// awaitingFeedback Plan in the space and routed into attachPlanFeedback
// (so the caller must NOT run the normal agent turn for this utterance).
//
// Returns handled=false (and the caller proceeds normally) whenever there is no
// awaitingFeedback Plan, the classifier is unsure, or anything errors -- a
// false negative just means the user answers via the card or rephrases, which
// is far cheaper than a false positive resuming the Plan with the wrong text.
func (c *CognitionIntegration) tryRouteUtteranceAsPlanFeedback(ctx context.Context, partitionId, userText, lastAgentText string) (handled bool) {
	if c == nil || c.engine == nil {
		return false
	}
	userText = strings.TrimSpace(userText)
	if partitionId == "" || userText == "" {
		return false
	}

	plan := c.findAwaitingFeedbackPlanForSpace(ctx, partitionId)
	if plan == nil {
		return false
	}

	cls, err := c.classifyFeedbackRoute(ctx, plan, userText, lastAgentText)
	if err != nil {
		c.Logger.Warn("cognition: plan-feedback route classification failed; falling through to normal dispatch",
			"error", err, "partitionId", partitionId, "planId", plan.ID)
		return false
	}
	if !cls.IsFeedback || cls.Confidence < feedbackRouteConfidenceThreshold {
		c.Logger.Debug("cognition: message not routed as plan feedback",
			"partitionId", partitionId, "planId", plan.ID,
			"isFeedback", cls.IsFeedback, "confidence", cls.Confidence,
			"reasoning", cls.Reasoning)
		return false
	}

	feedback := strings.TrimSpace(cls.Feedback)
	if feedback == "" {
		// Classifier said "this is feedback" but extracted nothing usable; use
		// the raw user text rather than attaching an empty answer.
		feedback = userText
	}

	if err := c.attachPlanFeedback(ctx, plan.ID, feedback); err != nil {
		c.Logger.Warn("cognition: failed to attach routed plan feedback; falling through",
			"error", err, "partitionId", partitionId, "planId", plan.ID)
		return false
	}

	c.Logger.Info("cognition: routed chat message into plan feedback intake",
		"partitionId", partitionId, "planId", plan.ID,
		"confidence", cls.Confidence, "feedbackLen", len(feedback))

	// Best-effort confirmation so the user sees the loop closed. The Plan's
	// resume (awaitingFeedback -> running) is driven by #1405 server-side;
	// cognition only acknowledges here.
	c.confirmPlanFeedbackRouted(ctx, partitionId)
	return true
}

// findAwaitingFeedbackPlanForSpace returns the most relevant Plan parked in
// awaitingFeedback (carrying a question) for the space, or nil when none. Reads
// via the existing plansForSpace + scans client-side for the status (the
// query has no status arg; the per-space plan count is small).
func (c *CognitionIntegration) findAwaitingFeedbackPlanForSpace(ctx context.Context, partitionId string) *awaitingFeedbackPlan {
	if c == nil || c.engine == nil || strings.TrimSpace(partitionId) == "" {
		return nil
	}
	query := fmt.Sprintf(`query plansForSpace(partitionId: "%s")`, partitionId)
	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		c.Logger.Debug("cognition: plansForSpace failed for feedback routing",
			"error", err, "partitionId", partitionId)
		return nil
	}

	nodes := nodesFromResult(result)
	var best *awaitingFeedbackPlan
	for _, node := range nodes {
		payload := payloadFromNode(node)
		if payload == nil {
			continue
		}
		if strings.TrimSpace(stringFromAny(payload["status"])) != planStatusAwaitingFeedback {
			continue
		}
		plan := awaitingFeedbackPlanFromPayload(node, payload)
		if plan == nil || strings.TrimSpace(plan.Question) == "" {
			continue
		}
		// plansForSpace returns newest-first (concept default), so the
		// first awaitingFeedback row we hit is the most recent. Keep it.
		best = plan
		break
	}
	return best
}

// classifyFeedbackRoute runs the cognitionFeedbackRoute structured-output
// prompt to decide whether userText answers the Plan's pending question.
func (c *CognitionIntegration) classifyFeedbackRoute(ctx context.Context, plan *awaitingFeedbackPlan, userText, lastAgentText string) (feedbackRouteResult, error) {
	var out feedbackRouteResult
	optionsJSON := "[]"
	if len(plan.Options) > 0 {
		if b, err := json.Marshal(plan.Options); err == nil {
			optionsJSON = string(b)
		}
	}
	data := map[string]any{
		"question":       plan.Question,
		"userText":       userText,
		"feedbackReason": plan.FeedbackReason,
		"options":        optionsJSON,
		"lastAgentText":  lastAgentText,
	}
	raw, err := c.engine.InvokeAIStructured(
		ctx,
		"cognitionFeedbackRoute",
		data,
		"cognitionFeedbackRoute",
		json.RawMessage(cognitionFeedbackRouteSchemaJSON),
		true,
	)
	if err != nil {
		return out, fmt.Errorf("invoke cognitionFeedbackRoute: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return out, fmt.Errorf("parse cognitionFeedbackRoute result: %w", err)
	}
	return out, nil
}

// attachPlanFeedback calls the #1405 intake mutation under the cognition system
// actor (acting on the user's behalf; the engine guard permits system /
// cluster-owner actors). The mutation stamps feedbackResponse, transitions
// awaitingFeedback -> running with a fresh startedAt (cross-replica exactly-once
// resume), and clears the request -- so cognition does NOT add a resume path.
func (c *CognitionIntegration) attachPlanFeedback(ctx context.Context, planId, feedback string) error {
	if c == nil || c.engine == nil {
		return fmt.Errorf("engine not configured")
	}
	if strings.TrimSpace(planId) == "" {
		return fmt.Errorf("planId is empty")
	}
	query := fmt.Sprintf(`mutation attachPlanFeedback(planId: %s, feedback: %s)`,
		escapeJSONString(planId), escapeJSONString(feedback))
	if _, err := c.engine.Execute(ctx, query); err != nil {
		return fmt.Errorf("execute attachPlanFeedback: %w", err)
	}
	return nil
}

// confirmPlanFeedbackRouted posts a brief assistant confirmation that the
// feedback was received and the work is resuming. Best-effort.
func (c *CognitionIntegration) confirmPlanFeedbackRouted(ctx context.Context, partitionId string) {
	aiParticipant, err := c.findAIParticipant(ctx, partitionId)
	if err != nil || aiParticipant == nil || strings.TrimSpace(aiParticipant.ID) == "" {
		return
	}
	source := map[string]string{
		"outputMethod": "text",
		"kind":         "planFeedbackAck",
	}
	if err := c.insertAIResponse(ctx, partitionId, aiParticipant, "", "",
		"Got it -- I've passed that along and the task is picking back up.", source, nil, nil); err != nil {
		c.Logger.Debug("cognition: failed to post plan-feedback confirmation", "error", err, "partitionId", partitionId)
	}
}

// queryHasFeedbackAnnouncement reports whether the assistant already posted the
// awaitingFeedback announcement for planId (read-before-write dedup behind the
// cross-replica announce gate).
func (c *CognitionIntegration) queryHasFeedbackAnnouncement(ctx context.Context, planId string) bool {
	if c == nil || c.engine == nil || strings.TrimSpace(planId) == "" {
		return false
	}
	query := fmt.Sprintf(`query feedbackAnnouncementForPlan(planId: %s)`, escapeJSONString(planId))
	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		return false
	}
	return len(nodesFromResult(result)) > 0
}

// ---- payload / event helpers ----

// planFromUpdateEvent extracts the awaitingFeedbackPlan projection from a
// graph.node.updated.v1:planner:plan event payload. Returns nil when the event
// is not a plan row or lacks the fields. Tolerates both the flattened
// (top-level) and nested-payload event shapes, matching extractUtteranceFromEvent.
func planFromUpdateEvent(event events.Event) *awaitingFeedbackPlan {
	if event.Payload == nil {
		return nil
	}
	id, _ := event.Payload["nodeId"].(string)
	if id == "" {
		id, _ = event.Payload["id"].(string)
	}

	// Prefer a nested payload object when present; else read the flattened
	// top-level fields the bridge stamps.
	var payload map[string]any
	if nested, ok := event.Payload["payload"].(map[string]any); ok {
		payload = nested
	} else {
		payload = event.Payload
	}
	if payload == nil {
		return nil
	}
	if strings.TrimSpace(id) == "" {
		id = strings.TrimSpace(stringFromAny(payload["id"]))
	}
	if strings.TrimSpace(id) == "" {
		return nil
	}
	plan := awaitingFeedbackPlanFromPayload(map[string]any{"id": id}, payload)
	return plan
}

// awaitingFeedbackPlanFromPayload builds the projection from a node + its
// payload map. node supplies the id intrinsic (top-level), payload the fields.
func awaitingFeedbackPlanFromPayload(node, payload map[string]any) *awaitingFeedbackPlan {
	if payload == nil {
		return nil
	}
	id := strings.TrimSpace(stringFromAny(node["id"]))
	if id == "" {
		id = strings.TrimSpace(stringFromAny(payload["id"]))
	}
	plan := &awaitingFeedbackPlan{
		ID:             id,
		PartitionId:    strings.TrimSpace(stringFromAny(payload["partitionId"])),
		Status:         strings.TrimSpace(stringFromAny(payload["status"])),
		FeedbackReason: strings.TrimSpace(stringFromAny(payload["feedbackReason"])),
		RequestedBy:    strings.TrimSpace(stringFromAny(payload["requestedBy"])),
	}
	if fr, ok := payload["feedbackRequest"].(map[string]any); ok {
		plan.Question = strings.TrimSpace(stringFromAny(fr["question"]))
		plan.Options = mapSliceFromAny(fr["options"])
	}
	return plan
}

// buildFeedbackAnnouncement renders the chat message the assistant posts when a
// Plan enters awaitingFeedback. The pending question is the load-bearing line;
// it is surfaced verbatim so the user can answer it conversationally.
func buildFeedbackAnnouncement(plan *awaitingFeedbackPlan) string {
	q := strings.TrimSpace(plan.Question)
	var b strings.Builder
	b.WriteString("I need your input to continue: ")
	b.WriteString(q)
	if len(plan.Options) > 0 {
		labels := make([]string, 0, len(plan.Options))
		for _, opt := range plan.Options {
			label := strings.TrimSpace(stringFromAny(opt["label"]))
			if label == "" {
				label = strings.TrimSpace(stringFromAny(opt["value"]))
			}
			if label != "" {
				labels = append(labels, label)
			}
		}
		if len(labels) > 0 {
			b.WriteString(" (")
			b.WriteString(strings.Join(labels, " / "))
			b.WriteString(")")
		}
	}
	b.WriteString(" -- just reply here and I'll pass it along.")
	return b.String()
}

// nodesFromResult pulls the bundle.nodes slice (every node, not just [0]) out
// of an engine query result, tolerating the Go-struct capitalized variants.
func nodesFromResult(result any) []map[string]any {
	resultMap, ok := result.(map[string]any)
	if !ok {
		return nil
	}
	bundle, ok := resultMap["bundle"].(map[string]any)
	if !ok {
		bundle, ok = resultMap["Bundle"].(map[string]any)
		if !ok {
			return nil
		}
	}
	raw, ok := bundle["nodes"].([]any)
	if !ok {
		raw, ok = bundle["Nodes"].([]any)
		if !ok {
			return nil
		}
	}
	out := make([]map[string]any, 0, len(raw))
	for _, n := range raw {
		if m, ok := n.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// payloadFromNode pulls the payload map out of a bundle node (tolerating the
// capitalized Go-struct variant), and stamps the node's id intrinsic into it
// so callers can read it uniformly.
func payloadFromNode(node map[string]any) map[string]any {
	if node == nil {
		return nil
	}
	payload, ok := node["payload"].(map[string]any)
	if !ok {
		payload, ok = node["Payload"].(map[string]any)
		if !ok {
			return nil
		}
	}
	if _, has := payload["id"]; !has {
		if nid := stringFromAny(node["id"]); nid != "" {
			payload["id"] = nid
		}
	}
	return payload
}

// mapSliceFromAny coerces an any (typically a JSON array of objects) to a
// []map[string]any, dropping non-object entries.
func mapSliceFromAny(v any) []map[string]any {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
