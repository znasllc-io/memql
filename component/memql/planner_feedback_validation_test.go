package memql

import "testing"

// TestIsFeedbackIntakeWrite covers the marker that distinguishes a generic
// feedback-intake resume (attachPlanFeedback) from every other
// v1:planner:plan write. Only a payload that stamps a feedbackResponse object
// carrying a non-empty respondedBy is an intake write.
func TestIsFeedbackIntakeWrite(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{
			name: "intake write: feedbackResponse with respondedBy",
			payload: map[string]any{
				"status": "running",
				"feedbackResponse": map[string]any{
					"response":    "use metric units",
					"respondedBy": "v1:identity:user:u1",
					"respondedAt": "2026-06-14T00:00:00Z",
				},
			},
			want: true,
		},
		{
			name:    "no feedbackResponse at all (normal status write)",
			payload: map[string]any{"status": "succeeded"},
			want:    false,
		},
		{
			name: "feedbackResponse present but no respondedBy (scope-elevation deny shape)",
			payload: map[string]any{
				"status":           "cancelled",
				"feedbackResponse": map[string]any{"response": "deny"},
			},
			want: false,
		},
		{
			name: "feedbackResponse present, respondedBy blank",
			payload: map[string]any{
				"feedbackResponse": map[string]any{"response": "x", "respondedBy": "  "},
			},
			want: false,
		},
		{
			name:    "nil payload",
			payload: nil,
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFeedbackIntakeWrite(tc.payload); got != tc.want {
				t.Fatalf("isFeedbackIntakeWrite = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFeedbackIntakeAllowed covers the pure prior-status x ownership matrix
// the engine guard enforces: a feedback resume is legal only from
// awaitingFeedback AND only for an owner/privileged caller.
func TestFeedbackIntakeAllowed(t *testing.T) {
	cases := []struct {
		name              string
		priorStatus       string
		ownerOrPrivileged bool
		want              bool
	}{
		{"awaitingFeedback + owner", "awaitingFeedback", true, true},
		{"awaitingFeedback + not owner", "awaitingFeedback", false, false},
		{"running + owner (already resumed / not parked)", "running", true, false},
		{"succeeded + owner (terminal)", "succeeded", true, false},
		{"queued + owner (never parked)", "queued", true, false},
		{"no prior row (plan does not exist)", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := feedbackIntakeAllowed(tc.priorStatus, tc.ownerOrPrivileged); got != tc.want {
				t.Fatalf("feedbackIntakeAllowed(%q, %v) = %v, want %v",
					tc.priorStatus, tc.ownerOrPrivileged, got, tc.want)
			}
		})
	}
}
