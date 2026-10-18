package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// Late information is read at the same model/tool boundaries as human guidance.
// Its receipt remains in the shared journal; no timer or mailbox lives on a pod.
func (e *MemQLEngine) ReadWorkGuidance(ctx context.Context) ([]WorkGuidance, error) {
	out, root, err := e.readAskWorkGuidance(ctx)
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
	runes := 0
	for _, item := range out {
		runes += utf8.RuneCountInString(item.Prompt)
	}
	for _, target := range targets {
		rows, err := e.workRows(ctx, "workQuestionsForOwnerRun", stringArg(target, "id"))
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
			runes += utf8.RuneCountInString(prompt)
			if len(out) >= maxWorkGuidanceMessages || runes > maxWorkGuidanceRunes {
				return nil, fmt.Errorf("saved guidance exceeds its bounded context")
			}
			out = append(out, WorkGuidance{ID: "optional-answer-" + BareShortId(stringArg(row, "id")), Prompt: prompt})
		}
	}
	return out, nil
}
