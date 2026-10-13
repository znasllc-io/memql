package work

// ApprovalAcceptsGuidance identifies gates whose unfinished work can be
// reconsidered without granting the requested permission or increasing a
// budget. Artifact-specific reviews keep their own editing contracts.
func ApprovalAcceptsGuidance(kind string, subject map[string]any, options any) bool {
	if reviewType, _ := subject["reviewType"].(string); reviewType != "" {
		return false
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
