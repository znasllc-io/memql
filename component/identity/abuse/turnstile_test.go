package abuse

import (
	"context"
	"testing"
)

// TestTurnstileEmptySecretIsDevPassThrough confirms the dev-mode
// behavior: a verifier without a secret returns OK without making
// any network calls.
func TestTurnstileEmptySecretIsDevPassThrough(t *testing.T) {
	v := &TurnstileVerifier{} // Secret == ""
	got, err := v.Verify(context.Background(), "any-token", "203.0.113.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.OK {
		t.Errorf("dev-mode (empty secret) verifier should return OK=true, got %#v", got)
	}
	if got.Borderline {
		t.Errorf("dev-mode verifier should not flag Borderline, got %#v", got)
	}
}

// TestTurnstileMissingTokenReportedAsErrorCode confirms a configured
// verifier without a token short-circuits cleanly (no network call,
// non-OK result with the expected error code).
func TestTurnstileMissingTokenReportedAsErrorCode(t *testing.T) {
	v := &TurnstileVerifier{Secret: "fake-secret"}
	got, err := v.Verify(context.Background(), "", "203.0.113.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.OK {
		t.Errorf("missing token should produce OK=false, got %#v", got)
	}
	found := false
	for _, code := range got.ErrorCodes {
		if code == "missing-input-response" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected missing-input-response in error codes, got %v", got.ErrorCodes)
	}
}
