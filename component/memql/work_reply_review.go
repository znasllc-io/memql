package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
)

const replyReviewMaxBytes = 24000

// Re-read receipts under the current run's owner on whichever replica finishes
// the answer. Draft prose and prior generated answers never become evidence.
// This is a bounded model pre-filter, not a factuality certification.
func (e *MemQLEngine) workReviewReplyBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	run, ok := common.RunFromContext(ctx)
	actor, _ := auth.AccessFromContext(ctx)
	if !ok || !run.IsRun() || run.GoalId == "" || actor == nil || BareShortId(actor.UserId) != BareShortId(run.OwnerUserId) {
		return nil, fmt.Errorf("reply review requires the current owned work run")
	}
	draft, err := reviewDraftText(args["draft"])
	if err != nil {
		return nil, err
	}
	ctx = ContextWithFreshRead(ctx)
	runs, err := e.workRows(ctx, "workRunForOwner", run.RunId)
	if err != nil || len(runs) != 1 || BareShortId(stringArg(runs[0], "goalId")) != BareShortId(run.GoalId) {
		return nil, fmt.Errorf("reply review run is unavailable")
	}
	remaining := replyReviewMaxBytes
	checkpoints, archiveOmitted, err := e.workReplySourceReceipts(ctx, run.RunId)
	if err != nil {
		return nil, fmt.Errorf("reply review sources: %w", err)
	}
	recovered, recoveryOmitted := workRecoveredSources(checkpoints, &remaining)
	current, omitted := e.workCheckpointSources(ctx, checkpoints, run.RunId, &remaining)
	sources := make([]any, 0, len(current))
	for _, source := range current {
		sources = append(sources, source)
	}
	limitations := []string{}
	if omitted || archiveOmitted || recoveryOmitted {
		limitations = append(limitations, "Some current tool receipts are unavailable or exceed the preview budget; their omitted contents are not verified.")
	}
	input, _ := runs[0]["input"].(map[string]any)
	conversation, _ := input["conversation"].(map[string]any)
	if conversationID := stringArg(conversation, "id"); conversationID != "" {
		prior, readErr := e.workSourceContextBuiltin(ctx, map[string]any{"conversationId": conversationID, "turnId": conversation["turnId"]}, 0)
		if readErr != nil || len(prior) != 1 {
			return nil, fmt.Errorf("reply review conversation sources are unavailable")
		}
		var evidence map[string]any
		if err := json.Unmarshal(prior[0].Payload, &evidence); err != nil {
			return nil, err
		}
		if notes, ok := evidence["limitations"].([]any); ok {
			for _, note := range notes {
				if text, ok := note.(string); ok {
					limitations = append(limitations, text)
				}
			}
		}
		priorSources, _ := evidence["recordedSources"].([]any)
		for _, source := range priorSources {
			encoded, err := json.Marshal(source)
			if err != nil || len(encoded) > remaining {
				limitations = append(limitations, "A previous receipt exceeded the preview budget; omitted content is not verified.")
				continue
			}
			sources = append(sources, source)
			remaining -= len(encoded)
		}
	}
	// A lookup that produced no receipt still needs review: absence of evidence
	// must not silently grant the draft the self-contained-reply bypass.
	classification, _ := runs[0]["classification"].(map[string]any)
	if len(sources) == 0 && (classification["workload"] == "lookup" || classification["workload"] == "research") {
		limitations = append(limitations, "This request required a lookup, but no successful source receipt was recorded. Current facts, model availability and causal claims have not been verified.")
	}
	// A greeting or self-contained composition has no retrieved evidence to
	// audit and spends no extra model call. Failed reads above never skip.
	if len(sources) == 0 && len(recovered) == 0 && len(limitations) == 0 {
		return reviewedReplyRows(draft, false)
	}
	call, err := parser.RenderCall("workGoalForOwner", map[string]any{"goalId": run.GoalId})
	if err != nil {
		return nil, err
	}
	result, err := e.Execute(ctx, "query "+call)
	if err != nil {
		return nil, err
	}
	goals := MaterializeRows(result.OutputPayload())
	if len(goals) != 1 {
		return nil, fmt.Errorf("reply review goal is unavailable")
	}
	statement := stringArg(goals[0], "statement")
	if statement == "" || len(statement) > replyReviewMaxBytes {
		return nil, fmt.Errorf("reply review requires a bounded recorded request")
	}
	now := time.Now()
	evidence, err := json.Marshal(map[string]any{"currentRunId": BareShortId(run.RunId), "recordedSources": sources, "recoveredSources": recovered, "limitations": limitations, "clock": workSourceClock(sources, now), "priorUserRequests": reviewPriorRequests(conversation["messages"])})
	if err != nil {
		return nil, err
	}
	draftRows := MaterializeRows(args["draft"])
	canContinue, _ := draftRows[0]["continueLookup"].(bool)
	canContinue = canContinue && (classification["workload"] == "lookup" || classification["workload"] == "research")
	review, err := reviewReplyOnce(ctx, statement, draft, string(evidence), now, canContinue, e.InvokeAIStructured)
	if err != nil {
		return nil, err // Never publish the unchecked draft after a failed review.
	}
	decision, err := json.Marshal(map[string]any{"continuationAvailable": canContinue, "missingEvidence": review.MissingEvidence})
	if err != nil {
		return nil, err
	}
	// Keep the completion decision separate from source receipts. It records
	// why this run continued or stopped; it never becomes evidence of a fact.
	call, err = parser.RenderCall("createWorkObservation", map[string]any{
		"observationId": "reply-review-" + BareShortId(run.RunId) + "-" + workContextHash(run.StepKey, decision),
		"runId":         run.RunId, "stepKey": run.StepKey, "kind": "decision",
		"content": "Reply evidence completion decision",
		"data":    map[string]any{"replyReview": json.RawMessage(decision)},
	})
	if err != nil {
		return nil, err
	}
	if _, err = e.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call); err != nil {
		return nil, fmt.Errorf("reply review decision receipt: %w", err)
	}
	raw, err := json.Marshal(map[string]any{"reply": review.Reply, "missingEvidence": review.MissingEvidence, "evidenceReviewed": true})
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "work-reviewed-reply", Payload: raw}}, nil
}

// Page enough current receipts to retain source reads that precede file checks.
// Both the archive scan and the eventual model preview remain bounded. All
// reads use owner-authorized queries on the finishing replica.
func (e *MemQLEngine) workReplySourceReceipts(ctx context.Context, runID string) ([]map[string]any, bool, error) {
	call, err := parser.RenderCall("workDelegatedSourcesForOwnerRun", map[string]any{"runId": runID})
	if err != nil {
		return nil, false, err
	}
	rows := []map[string]any{}
	cursor := ""
	for range 4 {
		result, err := e.Execute(ContextWithCursor(ctx, cursor), "query "+call)
		if err != nil {
			return nil, false, err
		}
		rows = append(rows, MaterializeRows(result.OutputPayload())...)
		cursor = ""
		if meta := result.GetMeta(); meta != nil {
			cursor = meta.Cursor
		}
		if cursor == "" {
			break
		}
	}
	checkpoints, err := e.workRows(ctx, "workSourceCheckpointsForOwnerRun", runID)
	return append(rows, checkpoints...), cursor != "", err
}

// Prior requests resolve follow-up references, never establish external facts.
// Read only the recorded run input; omit assistant assertions and oversized text.
func reviewPriorRequests(value any) []string {
	rows := MaterializeRows(value)
	requests := []string{}
	remaining := 6000
	for i := len(rows) - 1; i >= 0 && len(requests) < 4; i-- {
		content := stringArg(rows[i], "content")
		if stringArg(rows[i], "role") != "user" || content == "" || len(content) > remaining {
			continue
		}
		requests = append([]string{content}, requests...)
		remaining -= len(content)
	}
	return requests
}

func reviewDraftText(value any) (string, error) {
	rows := MaterializeRows(value)
	if len(rows) != 1 {
		return "", fmt.Errorf("reply review requires one draft")
	}
	draft, ok := rows[0]["reply"].(string)
	if !ok || strings.TrimSpace(draft) == "" || len(draft) > replyReviewMaxBytes {
		return "", fmt.Errorf("reply review requires a nonempty bounded draft")
	}
	return draft, nil
}

type replyReviewInvoker func(context.Context, string, map[string]any, string, json.RawMessage, bool) (string, error)

type replyReviewDecision struct {
	Reply           string `json:"reply"`
	MissingEvidence string `json:"missingEvidence"`
}

// Exactly one routed check under the original run's budget and cancellation.
// A repair finding is data for the existing task, never authority for an effect.
func reviewReplyOnce(ctx context.Context, statement, draft, sources string, now time.Time, canContinue bool, invoke replyReviewInvoker) (replyReviewDecision, error) {
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"required":["missingEvidence","reply"],"properties":{"missingEvidence":{"type":"string","maxLength":1200},"reply":{"type":"string","minLength":1,"maxLength":24000}}}`)
	raw, err := invoke(ctx, "workReplyReview", map[string]any{"statement": statement, "draft": draft, "sources": sources, "now": now.UTC().Format(time.RFC3339), "canContinue": canContinue}, "work_reply_review", schema, true)
	if err != nil {
		return replyReviewDecision{}, fmt.Errorf("reply evidence review: %w", err)
	}
	var answer replyReviewDecision
	if json.Unmarshal([]byte(raw), &answer) != nil || strings.TrimSpace(answer.Reply) == "" || len(answer.Reply) > replyReviewMaxBytes || len(answer.MissingEvidence) > 1200 {
		return replyReviewDecision{}, fmt.Errorf("reply evidence review returned an invalid answer")
	}
	if !canContinue {
		answer.MissingEvidence = ""
	}
	return answer, nil
}

func reviewedReplyRows(reply string, checked bool) ([]memorynodes.MemoryNode, error) {
	raw, err := json.Marshal(map[string]any{"reply": reply, "evidenceReviewed": checked})
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "work-reviewed-reply", Payload: raw}}, nil
}
