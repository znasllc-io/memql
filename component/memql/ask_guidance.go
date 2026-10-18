package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/common"
)

// Guidance is a durable human message, not a new goal, permission or rerun.
// Bound the lifetime transcript contribution instead of silently truncating it.
const maxWorkGuidanceMessages = 16
const maxWorkGuidanceRunes = 16000

type WorkGuidance struct {
	ID     string
	Prompt string
}

func runningGuidanceTarget(run map[string]any) bool {
	return run["status"] == "running" && run["cancelRequested"] != true && stringArg(run, "goalId") != "" && stringArg(run, "parentRunId") == ""
}

func workGuidanceEpoch(run map[string]any) string {
	// Permission answers and ordinary step progress retain the target. A replan
	// or explicit rerun moves it. Never carry old queued advice
	// silently into a different execution attempt.
	raw, _ := json.Marshal([]any{run["templateFingerprint"], run["rerun"]})
	return workContextHash("guidance-epoch", raw)
}

func guidanceConversation(run map[string]any) string {
	input, _ := run["input"].(map[string]any)
	conversation, _ := input["conversation"].(map[string]any)
	return stringArg(conversation, "id")
}

func (e *MemQLEngine) guidanceTarget(ctx context.Context, runID, conversationID string) (map[string]any, error) {
	subject, ok := auth.SubjectFromContext(ctx)
	if !ok || !auth.CapableFor(ctx, subject, "read", "app:ask") {
		return nil, fmt.Errorf("sign in to guide work")
	}
	runs, err := e.workRows(ContextWithFreshRead(ctx), "workRunForOwner", runID)
	if err != nil || len(runs) != 1 {
		return nil, fmt.Errorf("work is unavailable")
	}
	run := runs[0]
	if !runningGuidanceTarget(run) {
		return nil, fmt.Errorf("this work is no longer running; open its current request or result")
	}
	if conversationID == "" || BareShortId(guidanceConversation(run)) != BareShortId(conversationID) {
		return nil, fmt.Errorf("this work belongs to another conversation")
	}
	return run, nil
}

func projectWorkGuidance(turn *AskTurn, run map[string]any) {
	if turn.GuidanceStatus != "queued" {
		return
	}
	status := stringArg(run, "status")
	if run == nil || run["cancelRequested"] == true || turn.GuidanceEpoch != workGuidanceEpoch(run) || (status != "running" && status != "waiting") {
		turn.GuidanceStatus = "not-received"
	}
}

func (e *MemQLEngine) askGuideWorkBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if rc, ok := common.RunFromContext(ctx); ok && rc.RunId != "" {
		return nil, fmt.Errorf("guiding work needs a person's message")
	}
	targetEpoch := stringArg(args, "targetEpoch")
	runID, conversationID, requestID, prompt := stringArg(args, "runId"), stringArg(args, "conversationId"), stringArg(args, "requestId"), stringArg(args, "prompt")
	if targetEpoch == "" || len(targetEpoch) > 128 || len(requestID) == 0 || utf8.RuneCountInString(requestID) > 120 || strings.TrimSpace(requestID) != requestID || strings.TrimSpace(prompt) == "" || utf8.RuneCountInString(prompt) > 4000 {
		return nil, fmt.Errorf("guidance needs a request id and a message of 1 to 4000 characters")
	}
	// Resolve ownership before acquiring a caller-selected lock.
	if _, err := e.askRead(ContextWithFreshRead(ctx), conversationID); err != nil {
		return nil, err
	}
	hold, err := e.holdAskConversation(ctx, conversationID, "write")
	if err != nil {
		return nil, err
	}
	defer hold.release()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	row, err := e.askRead(ContextWithFreshRead(ctx), conversationID)
	if err != nil {
		return nil, err
	}
	transcript, err := approvalTranscript(row)
	if err != nil {
		return nil, err
	}
	turnID := workContextHash("active-work-guidance", []byte(requestID))
	result := func(turn AskTurn) ([]memorynodes.MemoryNode, error) {
		return askValue(map[string]any{"conversationId": BareShortId(conversationID), "runId": BareShortId(runID), "requestId": requestID, "turn": turn, "status": turn.GuidanceStatus})
	}
	count, runes := 0, 0
	for _, turn := range transcript.Turns {
		if turn.ID == turnID {
			if BareShortId(turn.GuidanceTargetRunID) != BareShortId(runID) || turn.Prompt != prompt || turn.GuidanceEpoch != targetEpoch {
				return nil, fmt.Errorf("this request id already belongs to another message")
			}
			// A retry acknowledges the original receipt even after work finishes. It
			// cannot create another message or apply it to a new attempt.
			runs, err := e.workRows(ContextWithFreshRead(ctx), "workRunForOwner", runID)
			if err != nil {
				return nil, err
			}
			var saved map[string]any
			if len(runs) == 1 {
				saved = runs[0]
			}
			projectWorkGuidance(&turn, saved)
			return result(turn)
		}
		if BareShortId(turn.GuidanceTargetRunID) == BareShortId(runID) {
			count++
			runes += utf8.RuneCountInString(turn.Prompt)
		}
	}
	run, err := e.guidanceTarget(ctx, runID, conversationID)
	if err != nil {
		return nil, err
	}
	if targetEpoch != workGuidanceEpoch(run) {
		return nil, fmt.Errorf("this work changed; refresh and select it again")
	}
	if count >= maxWorkGuidanceMessages || runes+utf8.RuneCountInString(prompt) > maxWorkGuidanceRunes {
		return nil, fmt.Errorf("this work has reached its guidance limit")
	}
	turn := AskTurn{ID: turnID, Prompt: prompt, GuidanceTargetRunID: BareShortId(runID), GuidanceEpoch: workGuidanceEpoch(run), GuidanceStatus: "queued", State: "done", StartedAt: time.Now().UTC(), AnswerOnly: true, Activity: []WorkEvent{}}
	transcript.Turns = append(transcript.Turns, turn)
	if err := hold.verify(ctx); err != nil {
		return nil, err
	}
	if err := e.askSave(ctx, conversationID, stringArg(row, "title"), transcript); err != nil {
		return nil, err
	}
	return result(turn)
}

// Work run context is retained across synchronous child automations. Fresh
// owner-bound ancestry also covers delegated children that carry their own ID.
func (e *MemQLEngine) workGuidanceScope(ctx context.Context) (map[string]any, string, error) {
	rc, ok := common.RunFromContext(ctx)
	if !ok || rc.RunId == "" || rc.GoalId == "" || rc.OwnerUserId == "" || rc.StepKey == "" {
		return nil, "", nil
	}
	access, _ := auth.AccessFromContext(ctx)
	if access == nil || BareShortId(access.UserId) != BareShortId(rc.OwnerUserId) {
		return nil, "", fmt.Errorf("guidance requires owned work execution")
	}
	runs, err := e.workRows(ContextWithFreshRead(ctx), "workRunForOwner", rc.RunId)
	if err != nil {
		return nil, "", err
	}
	if len(runs) != 1 {
		return nil, "", fmt.Errorf("guidance work is unavailable")
	}
	if BareShortId(stringArg(runs[0], "ownerUserId")) != BareShortId(rc.OwnerUserId) || BareShortId(stringArg(runs[0], "goalId")) != BareShortId(rc.GoalId) {
		return nil, "", fmt.Errorf("guidance execution lost its owner or goal")
	}
	// Guidance targets the root execution even when a delegated child copied
	// its conversation input. Conversation discovery may stop at that child;
	// execution guidance must continue through its fresh owner-bound lineage.
	run := runs[0]
	seen := map[string]bool{}
	for depth := 0; ; depth++ {
		id := BareShortId(stringArg(run, "id"))
		if id == "" || seen[id] || depth >= 32 {
			return nil, "", fmt.Errorf("guidance execution ancestry is unavailable")
		}
		seen[id] = true
		parent := stringArg(run, "parentRunId")
		if parent == "" {
			break
		}
		parents, err := e.workRows(ContextWithFreshRead(ctx), "workRunForOwner", parent)
		if err != nil || len(parents) != 1 {
			return nil, "", fmt.Errorf("guidance execution ancestry is unavailable")
		}
		run = parents[0]
	}
	if !runningGuidanceTarget(run) {
		return nil, "", nil
	}
	return run, guidanceConversation(run), nil
}

// Read all guidance for this execution attempt, including acknowledged messages:
// a restarted or later child loop must recover the human's direction. The loop
// deduplicates within its own context; receipt state is never a delivery cursor.
func (e *MemQLEngine) readAskWorkGuidance(ctx context.Context) ([]WorkGuidance, map[string]any, error) {
	run, conversation, err := e.workGuidanceScope(ctx)
	if err != nil || conversation == "" {
		return nil, run, err
	}
	row, err := e.askRead(ContextWithFreshRead(ctx), conversation)
	if err != nil {
		return nil, run, err
	}
	transcript, err := approvalTranscript(row)
	if err != nil {
		return nil, run, err
	}
	var out []WorkGuidance
	runes := 0
	for _, turn := range transcript.Turns {
		if BareShortId(turn.GuidanceTargetRunID) != BareShortId(stringArg(run, "id")) || turn.GuidanceEpoch != workGuidanceEpoch(run) {
			continue
		}
		runes += utf8.RuneCountInString(turn.Prompt)
		if len(out) >= maxWorkGuidanceMessages || runes > maxWorkGuidanceRunes {
			return nil, run, fmt.Errorf("saved guidance exceeds its bounded context")
		}
		out = append(out, WorkGuidance{ID: turn.ID, Prompt: turn.Prompt})
	}
	return out, run, nil
}

// Acknowledgement means a successful model response used this human message;
// it makes no claim that the model followed it or that the task completed.
func (e *MemQLEngine) AcknowledgeWorkGuidance(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > maxWorkGuidanceMessages {
		return fmt.Errorf("too many guidance receipts")
	}
	run, conversation, err := e.workGuidanceScope(ctx)
	if err != nil || conversation == "" {
		return err
	}
	expectedRun, expectedConversation, expectedEpoch := BareShortId(stringArg(run, "id")), BareShortId(conversation), workGuidanceEpoch(run)
	hold, err := e.holdAskConversation(ctx, conversation, "write")
	if err != nil {
		return err
	}
	defer hold.release()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	// Reread the run after acquiring the transcript lock, as admission does.
	run, conversation, err = e.workGuidanceScope(ctx)
	if err != nil || conversation == "" {
		return err
	}
	if BareShortId(stringArg(run, "id")) != expectedRun || BareShortId(conversation) != expectedConversation || workGuidanceEpoch(run) != expectedEpoch {
		return nil
	}
	row, err := e.askRead(ContextWithFreshRead(ctx), conversation)
	if err != nil {
		return err
	}
	transcript, err := approvalTranscript(row)
	if err != nil {
		return err
	}
	pending := map[string]bool{}
	for _, id := range ids {
		pending[id] = true
	}
	changed := false
	for n := range transcript.Turns {
		turn := &transcript.Turns[n]
		if !pending[turn.ID] || BareShortId(turn.GuidanceTargetRunID) != BareShortId(stringArg(run, "id")) || turn.GuidanceEpoch != workGuidanceEpoch(run) {
			continue
		}
		if turn.GuidanceStatus != "applied" {
			turn.GuidanceStatus = "applied"
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := hold.verify(ctx); err != nil {
		return err
	}
	return e.askSave(ctx, conversation, stringArg(row, "title"), transcript)
}
