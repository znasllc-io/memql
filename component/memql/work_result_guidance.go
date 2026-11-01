package memql

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/work"
)

// Stable across the intent, accepted run write, observation seal and Ask
// transcript delivery. It never changes to the later transcript's identity.
func WorkResultFeedbackID(runID, requestID string) string {
	return "v1:work:observation:result-feedback-" + work.ArtifactHash(map[string]any{"runId": BareShortId(runID), "requestId": requestID})
}

func (e *MemQLEngine) readResultWorkGuidance(ctx context.Context, targets []map[string]any, out []WorkGuidance, dates map[string]time.Time) ([]WorkGuidance, error) {
	for _, target := range targets {
		runID := stringArg(target, "id")
		rows, err := e.workGuidanceRows(ctx, "workResultFeedbackForOwnerRun", runID)
		if err != nil {
			return nil, err
		}
		receipts := map[string]map[string]any{}
		for _, row := range rows {
			if BareShortId(stringArg(row, "runId")) != BareShortId(runID) || BareShortId(stringArg(row, "ownerUserId")) != BareShortId(stringArg(target, "ownerUserId")) {
				return nil, fmt.Errorf("result feedback receipt attribution changed")
			}
			data, _ := row["data"].(map[string]any)
			receipt, _ := data["resultFeedback"].(map[string]any)
			if receipt["accepted"] != true || BareShortId(stringArg(receipt, "id")) != BareShortId(stringArg(row, "id")) {
				return nil, fmt.Errorf("result feedback receipt is not accepted")
			}
			receipts[stringArg(receipt, "id")] = receipt
		}
		// Dispatch can commit just before the immutable acceptance seal. This
		// exact run receipt is the proof; unrelated pending intents stay hidden.
		if latest, _ := target["feedbackContinuation"].(map[string]any); len(latest) > 0 {
			receipts[stringArg(latest, "id")] = latest
		}
		for _, receipt := range receipts {
			id, requestID, text := stringArg(receipt, "id"), stringArg(receipt, "requestId"), stringArg(receipt, "feedback")
			at, valid := askTimestamp(receipt["requestedAt"])
			schema, _ := typedNumber(receipt["schemaVersion"])
			if schema != 1 || !valid || requestID == "" || text == "" || id != WorkResultFeedbackID(runID, requestID) ||
				BareShortId(stringArg(receipt, "requestedBy")) != BareShortId(stringArg(target, "ownerUserId")) {
				return nil, fmt.Errorf("result feedback attribution is unavailable")
			}
			// The Ask target remains visible as a delivery receipt, but its
			// instruction uses the durable accepted-feedback identity and time.
			filtered := out[:0]
			for _, message := range out {
				if BareShortId(guidanceConversation(target))+":"+message.ID == requestID && strings.TrimSpace(message.Prompt) == text {
					continue
				}
				filtered = append(filtered, message)
			}
			out = append(filtered, WorkGuidance{ID: BareShortId(id), Prompt: text})
			dates[BareShortId(id)] = at
		}
	}
	return out, nil
}
