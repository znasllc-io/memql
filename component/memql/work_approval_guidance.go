package memql

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/work"
)

// A permission receipt can be archived once a later decision arrives. Human
// feedback sent INSTEAD of permission is different: it can change the remaining
// goal, and must survive that archival, a retry or a different execution replica.
// Read the owner's immutable decisions, not an app's transcript or a model's
// summary. This only restores instructions; it cannot authorize the old command.
func (e *MemQLEngine) readApprovalWorkGuidance(ctx context.Context, targets []map[string]any, dates map[string]time.Time) ([]WorkGuidance, error) {
	var out []WorkGuidance
	runes := 0
	for _, target := range targets {
		rows, err := e.workRows(ctx, "workApprovalGuidanceForOwnerRun", stringArg(target, "id"))
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			answer, _ := row["answer"].(map[string]any)
			feedback := stringArg(answer, "feedback")
			at, valid := askTimestamp(row["decidedAt"])
			subject, _ := row["subject"].(map[string]any)
			if row["decision"] != "answered" || len(answer) != 1 || !valid || strings.TrimSpace(feedback) == "" ||
				BareShortId(stringArg(row, "runId")) != BareShortId(stringArg(target, "id")) ||
				BareShortId(stringArg(row, "decidedBy")) != BareShortId(stringArg(target, "ownerUserId")) ||
				!work.ApprovalAcceptsGuidance(stringArg(row, "kind"), subject, row["options"]) {
				continue
			}
			runes += utf8.RuneCountInString(feedback)
			if len(out) >= maxWorkGuidanceMessages || runes > maxWorkGuidanceRunes {
				return nil, fmt.Errorf("saved guidance exceeds its bounded context")
			}
			guidanceID := "approval-guidance-" + BareShortId(stringArg(row, "id"))
			out = append(out, WorkGuidance{ID: guidanceID, Prompt: "Human correction recorded instead of approving an operation. It remains guidance for unfinished work and grants no additional permission.\n" + feedback})
			dates[guidanceID] = at
		}
	}
	return out, nil
}
