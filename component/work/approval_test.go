package work

import (
	"errors"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func TestBudgetApproval_CarriesTheNumbers(t *testing.T) {
	b := CheckCeilings(Ceilings{TokenBudget: 100}, Spent{Tokens: 90}, 20)
	a := BudgetApproval("run-1", "step-a", *b, t0, time.Hour)
	if a.Kind != "budget" || a.RunId != "run-1" || a.StepKey != "step-a" {
		t.Fatalf("%+v", a)
	}
	if a.ArtifactHash == "" {
		t.Fatal("every approval hashes what is being approved, or resume cannot tell it changed")
	}
	if a.Subject["limit"] == nil || a.Subject["actual"] == nil {
		t.Errorf("subject must carry the figures: %+v", a.Subject)
	}
	if !a.ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("expiresAt = %v", a.ExpiresAt)
	}
}

func TestApprovalKinds(t *testing.T) {
	ev := Evidence{Tier: "high", Reason: "r", RuleId: "x.y", Source: EvidenceSourceRules}
	if got := SideEffectApproval("r", "s", "hash", ev, map[string]any{"command": "rm -rf /"}, t0, time.Hour); got.Kind != "sideEffect" || got.ArtifactHash != "hash" {
		t.Errorf("%+v", got)
	}
	if got := PlanReviewApproval("r", "s", "hash", []map[string]any{{"kind": "relativize-literal"}}, ev, t0, time.Hour); got.Kind != ApprovalKindPlanReview {
		t.Errorf("%+v", got)
	}
	fb := FeedbackApproval("r", "s", "Which one?", []map[string]any{{"label": "A", "value": "a"}}, ev, t0, time.Hour)
	if fb.Kind != ApprovalKindFeedback || fb.Question != "Which one?" || len(fb.Options) != 1 {
		t.Errorf("%+v", fb)
	}
	if fb.ArtifactHash == "" {
		t.Error("even a question hashes: resume must refuse to answer a question that changed")
	}
}

// Spec section D: "An approval never carries to a modified artifact:
// resume compares the hash."
func TestResumeAllowed_RefusesAModifiedArtifact(t *testing.T) {
	if ok, err := ResumeAllowed("h1", "h1", "approved"); !ok || err != nil {
		t.Fatalf("an unchanged approved artifact resumes: ok=%v err=%v", ok, err)
	}
	ok, err := ResumeAllowed("h1", "h2", "approved")
	if ok {
		t.Fatal("a MODIFIED artifact must not resume on an old approval -- that is approving one thing and running another")
	}
	if !errors.Is(err, ErrArtifactChanged) {
		t.Fatalf("err = %v, want ErrArtifactChanged", err)
	}
}

func TestResumeAllowed_UndecidedAndRejected(t *testing.T) {
	if ok, err := ResumeAllowed("h1", "h1", ""); ok || !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("a pending approval does not resume: ok=%v err=%v", ok, err)
	}
	if ok, err := ResumeAllowed("h1", "h1", "rejected"); ok || !errors.Is(err, ErrApprovalRejected) {
		t.Fatalf("a rejected approval does not resume: ok=%v err=%v", ok, err)
	}
	if ok, _ := ResumeAllowed("h1", "h1", "answered"); !ok {
		t.Error("an answered feedback approval resumes the run")
	}
}

func TestArtifactHash_StableAndSensitive(t *testing.T) {
	a := ArtifactHash(map[string]any{"command": "ls", "cwd": "/tmp"})
	if a != ArtifactHash(map[string]any{"cwd": "/tmp", "command": "ls"}) {
		t.Fatal("the hash must not depend on map iteration order, or every resume would refuse")
	}
	if a == ArtifactHash(map[string]any{"command": "ls -la", "cwd": "/tmp"}) {
		t.Fatal("a changed command must change the hash; that is the whole guarantee")
	}
}
