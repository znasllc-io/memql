package work

// ApprovalAcceptsGuidance identifies gates whose unfinished work can be
// reconsidered without granting the requested permission. A budget checkpoint
// can save guidance, but stays paused until a separate budget decision.
// Artifact-specific reviews keep their own editing contracts.
func ApprovalAcceptsGuidance(kind string, subject map[string]any, options any) bool {
	if reviewType, _ := subject["reviewType"].(string); reviewType != "" {
		return false
	}
	if kind == ApprovalKindBudget {
		// Only a native checkpoint has a durable answer-delivery path. Legacy
		// failures and planner budget gates retain their own review process.
		resumeKind, _ := subject["resumeKind"].(string)
		continuationID, _ := subject["continuationId"].(string)
		return resumeKind == "checkpoint" && continuationID != ""
	}
	if IsFailureQuestion(options) {
		return true
	}
	switch kind {
	case ApprovalKindFeedback, ApprovalKindScopeElevation, ApprovalKindSideEffect:
		return true
	default:
		return false
	}
}
