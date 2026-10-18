package work

import (
	"testing"
	"time"
)

func TestOptionalFeedbackRejectsPermissionAndInvalidReceipts(t *testing.T) {
	now := time.Now().UTC()
	makeRow := func() map[string]any {
		subject := map[string]any{"kind": "text", "feedbackContinuation": map[string]any{"waitSeconds": 60, "templateFingerprint": "source", "rerunRequestId": "", "overrideKey": ""}}
		return map[string]any{"kind": "feedback", "subject": subject, "artifactHash": ArtifactHash(subject), "requestedAt": now.Add(-time.Minute).Format(time.RFC3339Nano), "continuedAt": now.Format(time.RFC3339Nano)}
	}
	if !OptionalFeedbackContinued(makeRow(), now) {
		t.Fatal("valid timeout refused")
	}
	for _, kind := range []string{"scopeElevation", "budget", "planReview", "procedurePromotion"} {
		row := makeRow()
		row["kind"] = kind
		if OptionalFeedbackContinued(row, now) {
			t.Fatalf("silence released %s", kind)
		}
	}
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"tampered", func(r map[string]any) { r["artifactHash"] = "altered" }},
		{"early", func(r map[string]any) { r["continuedAt"] = now.Add(-time.Second).Format(time.RFC3339Nano) }},
		{"future", func(r map[string]any) { r["continuedAt"] = now.Add(time.Second).Format(time.RFC3339Nano) }},
		{"unbounded wait", func(r map[string]any) {
			s := r["subject"].(map[string]any)
			s["feedbackContinuation"].(map[string]any)["waitSeconds"] = 301
			r["artifactHash"] = ArtifactHash(s)
		}},
		{"wrong epoch type", func(r map[string]any) {
			s := r["subject"].(map[string]any)
			s["feedbackContinuation"].(map[string]any)["templateFingerprint"] = 42
			r["artifactHash"] = ArtifactHash(s)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := makeRow()
			test.change(row)
			if OptionalFeedbackContinued(row, now) {
				t.Fatal("invalid continuation accepted")
			}
		})
	}
}
