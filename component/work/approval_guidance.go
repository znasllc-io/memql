package work

// ApprovalAcceptsGuidance identifies unfinished work that can take a correction.
// Retired budget requests can be closed, but cannot grant more resources.
func ApprovalAcceptsGuidance(kind string, subject map[string]any, options any) bool {
	if reviewType, _ := subject["reviewType"].(string); reviewType != "" {
		return false
	}
	if kind == ApprovalKindBudget || subject["continuationAllowed"] == false {
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
