package work

import (
	"encoding/json"
	"time"
)

// OptionalFeedback is an immutable information-wait contract. It cannot be
// attached to a permission, budget, failure or review gate by the model.
func OptionalFeedback(row map[string]any) (map[string]any, time.Time, bool) {
	subject, _ := row["subject"].(map[string]any)
	contract, _ := subject["feedbackContinuation"].(map[string]any)
	kind, _ := subject["kind"].(string)
	var seconds int
	raw, _ := json.Marshal(contract["waitSeconds"])
	_ = json.Unmarshal(raw, &seconds)
	fingerprint, _ := contract["templateFingerprint"].(string)
	_, rerunOK := contract["rerunRequestId"].(string)
	_, overrideOK := contract["overrideKey"].(string)
	if row["kind"] != "feedback" || (kind != "text" && kind != "choice" && kind != "multi") ||
		seconds < 1 || seconds > 300 || fingerprint == "" || !rerunOK || !overrideOK ||
		row["artifactHash"] != ArtifactHash(subject) || IsModelCallCheckpoint(subject) || IsFailureQuestion(row["options"]) {
		return nil, time.Time{}, false
	}
	requested, _ := row["requestedAt"].(string)
	at, err := time.Parse(time.RFC3339Nano, requested)
	if err != nil {
		return nil, time.Time{}, false
	}
	return contract, at.Add(time.Duration(seconds) * time.Second), true
}

// A timestamp is a continuation receipt, never a human decision. The model
// cannot write it, and a receiver revalidates both its deadline and the hash.
func OptionalFeedbackContinued(row map[string]any, now time.Time) bool {
	_, due, ok := OptionalFeedback(row)
	stamp, _ := row["continuedAt"].(string)
	at, err := time.Parse(time.RFC3339Nano, stamp)
	return ok && err == nil && !at.Before(due) && !at.After(now)
}
