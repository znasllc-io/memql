package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// Late information is read at the same model/tool boundaries as human guidance.
// Its receipt remains in the shared journal; no timer or mailbox lives on a pod.
// Delivery pages share a chronological cursor across all instruction sources.
func (e *MemQLEngine) ReadWorkGuidance(ctx context.Context) ([]WorkGuidance, error) {
	return e.readWorkGuidance(ctx, false)
}

// Final review has no restored instruction message. Include its whole-prompt
// override in the same dated sequence, without changing ordinary resume input.
func (e *MemQLEngine) readWorkGuidance(ctx context.Context, includeWholePrompt bool) ([]WorkGuidance, error) {
	dates := map[string]time.Time{}
	out, root, err := e.readAskWorkGuidance(ctx, dates)
	if err != nil {
		return nil, err
	}
	rc, ok := common.RunFromContext(ctx)
	if !ok || rc.RunId == "" || rc.GoalId == "" || rc.StepKey == "" {
		return out, nil
	}
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || BareShortId(ac.UserId) != BareShortId(rc.OwnerUserId) {
		return nil, fmt.Errorf("feedback guidance requires owned work")
	}
	ctx = ContextWithFreshRead(ctx)
	run := root
	if BareShortId(stringArg(root, "id")) != BareShortId(rc.RunId) {
		runs, err := e.workRows(ctx, "workRunForOwner", rc.RunId)
		if err != nil {
			return nil, err
		}
		if len(runs) != 1 {
			return nil, fmt.Errorf("feedback guidance run is unavailable")
		}
		run = runs[0]
	}
	if BareShortId(stringArg(run, "goalId")) != BareShortId(rc.GoalId) {
		return nil, fmt.Errorf("feedback guidance goal changed")
	}
	if run["status"] != "running" || run["cancelRequested"] == true {
		return out, nil
	}
	rerun, _ := run["rerun"].(map[string]any)
	if stringArg(rerun, "requestId") != rc.RerunRequestId {
		return nil, fmt.Errorf("feedback guidance execution changed")
	}
	targets := []map[string]any{run}
	if root != nil && BareShortId(stringArg(root, "id")) != BareShortId(rc.RunId) {
		targets = append(targets, root)
	}
	for _, target := range targets {
		rows, err := e.workGuidanceRows(ctx, "workQuestionsForOwnerRun", stringArg(target, "id"))
		if err != nil {
			return nil, err
		}
		targetRerun, _ := target["rerun"].(map[string]any)
		for _, row := range rows {
			contract, _, valid := work.OptionalFeedback(row)
			if !valid || row["decision"] != "answered" || !work.OptionalFeedbackContinued(row, time.Now()) ||
				contract["templateFingerprint"] != target["templateFingerprint"] || stringArg(contract, "rerunRequestId") != stringArg(targetRerun, "requestId") {
				continue
			}
			raw, err := json.Marshal(map[string]any{"question": row["question"], "answer": row["answer"], "decidedAt": row["decidedAt"]})
			if err != nil {
				return nil, err
			}
			prompt := "Late answer to an optional question for this execution. Use it for unfinished work; preserve completed results. It grants no additional permission.\n" + string(raw)
			guidanceID := "optional-answer-" + BareShortId(stringArg(row, "id"))
			out = append(out, WorkGuidance{ID: guidanceID, Prompt: prompt})
			dates[guidanceID], _ = askTimestamp(row["decidedAt"])
		}
	}
	corrections, err := e.readApprovalWorkGuidance(ctx, targets, dates)
	if err != nil {
		return nil, err
	}
	out = append(out, corrections...)
	// The rerun's owner instructions are older than corrections made during
	// that attempt. Compaction can have archived their original message; the
	// Apps seam would then append them again AFTER newer guidance. Keep their
	// exact rendering in this bounded chronological batch so that seam finds
	// them already present and cannot accidentally renew their priority.
	// WholePrompt stays separate for execution, which already has its active
	// instruction. Final review includes it here because it has no such message.
	if rc.Override != nil && (!rc.Override.WholePrompt || includeWholePrompt) {
		extra := StepOverrideMessages(ctx)
		if len(extra) > 0 {
			instructionAt := rerun["requestedAt"]
			if inherited, exists := rerun["overrideRequestedAt"]; exists {
				instructionAt = inherited
			}
			at, valid := askTimestamp(instructionAt)
			if !valid {
				return nil, fmt.Errorf("step override guidance timestamp is unavailable")
			}
			for n, message := range extra {
				guidanceID := fmt.Sprintf("step-override-%s-%d", workContextHash("guidance", []byte(rc.RerunRequestId+"\x00"+rc.StepKey)), n)
				out = append(out, WorkGuidance{ID: guidanceID, Prompt: message.Content})
				dates[guidanceID] = at
			}
		}
	}
	// Sources are queried independently. Restore human chronology before
	// presentation so an older approval correction cannot override later Ask
	// guidance merely because its journal reader ran last.
	sort.SliceStable(out, func(i, j int) bool {
		if dates[out[i].ID].Equal(dates[out[j].ID]) {
			return out[i].ID < out[j].ID
		}
		return dates[out[i].ID].Before(dates[out[j].ID])
	})
	return out, nil
}
