package work

import "time"

// These questions reuse the ordinary feedback surface. They carry an immutable
// checkpoint, not a user-editable time/call/token allowance.
func ProgressFeedback(runID, stepKey string, breach CeilingBreach, now time.Time, ttl time.Duration) ApprovalRequest {
	question := "I’ve saved the work so far, but need direction to finish. Should I keep working from here, or would you like me to change the approach?"
	options := []map[string]any{{"value": "retry", "label": "Keep working"}, {"value": "abandon", "label": "Stop here"}}
	subject := map[string]any{"kind": "choice", "question": question, "options": options, "progressLimit": true, "ceiling": breach.Ceiling, "limit": breach.Limit, "actual": breach.Actual, "reason": breach.Reason}
	result := newApproval(ApprovalKindFeedback, runID, stepKey, subject, Evidence{Tier: "work", Reason: breach.Reason, Source: EvidenceSourceRules}, now, ttl)
	result.Question, result.Options = question, options
	return result
}

// A hard stop has no misleading keep-working choice. Its saved results remain
// available to a new request with a narrower scope.
func StopProgressFeedback(request *ApprovalRequest) {
	request.Question = "I’ve saved the work so far, but can’t finish this request within its overall limits. You can use the saved results in a new request with a narrower scope."
	request.Options = []map[string]any{{"value": "abandon", "label": "Stop here"}}
	request.Subject["question"] = request.Question
	request.Subject["options"] = request.Options
	request.Subject["continuationAllowed"] = false
	request.ArtifactHash = ArtifactHash(request.Subject)
}

func IsProgressFeedback(subject map[string]any) bool { return subject["progressLimit"] == true }
func IsProgressCheckpoint(subject map[string]any) bool {
	id, _ := subject["continuationId"].(string)
	return IsProgressFeedback(subject) && subject["resumeKind"] == "checkpoint" && id != ""
}
func IsReasoningCheckpoint(subject map[string]any) bool {
	return IsModelCallCheckpoint(subject) || IsProgressCheckpoint(subject)
}
