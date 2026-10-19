package memql

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
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

func guidanceConversation(run map[string]any) string {
	input, _ := run["input"].(map[string]any)
	conversation, _ := input["conversation"].(map[string]any)
	return stringArg(conversation, "id")
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

// Read all guidance for this run, including acknowledged messages: a restarted
// or later child loop, a retry, a repair, a re-plan or a re-run must recover the
// human's direction. The loop deduplicates within its own context; receipt
// state is never a delivery cursor.
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
	for n := range transcript.Turns {
		target := workGuidanceTarget(&transcript.Turns[n], run)
		if target == nil {
			continue
		}
		runes += utf8.RuneCountInString(target.Text)
		if len(out) >= maxWorkGuidanceMessages || runes > maxWorkGuidanceRunes {
			return nil, run, fmt.Errorf("saved guidance exceeds its bounded context")
		}
		out = append(out, WorkGuidance{ID: transcript.Turns[n].ID, Prompt: target.Text})
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
	expectedRun, expectedConversation := BareShortId(stringArg(run, "id")), BareShortId(conversation)
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
	if BareShortId(stringArg(run, "id")) != expectedRun || BareShortId(conversation) != expectedConversation {
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
		if !pending[transcript.Turns[n].ID] {
			continue
		}
		if target := workGuidanceTarget(&transcript.Turns[n], run); target != nil && target.Status != routeReceived {
			target.Status = routeReceived
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
