package work

import (
	"encoding/json"
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

// THE RAISE SIDE AND THE DECIDE SIDE MUST AGREE ON EVERY KIND WHOSE HASH IS
// DERIVED (memql#5664). The failure path stored one map as its subject and
// hashed another, and the decide side's recompute over the stored subject
// refused every approve and every answer as "artifact changed". This asserts,
// for every builder that derives its hash, that the subject AS STORED -- after
// the JSON round trip the graph gives it -- recomputes to the hash it was
// raised with, and that a genuinely edited subject does not.
func TestEveryDerivedApprovalHashSurvivesTheDecideRecompute(t *testing.T) {
	ev := Evidence{Tier: "t", Reason: "r", RuleId: "x.y", Source: EvidenceSourceRules}
	breach := CheckCeilings(Ceilings{MaxModelCalls: 2}, Spent{ModelCalls: 2}, 0)
	for _, req := range []ApprovalRequest{
		BudgetApproval("run-1", "s", *breach, t0, time.Hour),
		FeedbackApproval("run-1", "s", "Which one?", []map[string]any{{"label": "A", "value": "a"}}, ev, t0, time.Hour),
		PlanReviewApproval("run-1", "s", "", []map[string]any{{"op": "relativize", "path": "/tmp/x"}}, ev, t0, time.Hour),
		InferenceUnavailableApproval("run-1", "s", RefusalEveryDoorShut, nil, t0, time.Hour),
		FailureApproval(ApprovalKindFeedback, "run-1", "s", SymptomHuman, "prompt is too long", "q", ev, t0, time.Hour),
		FailureApproval(ApprovalKindPlanReview, "run-1", "s", SymptomEnvironment, "permission denied", "q", ev, t0, time.Hour),
		FailureApproval(ApprovalKindBudget, "run-1", "s", SymptomHuman, "insufficient_quota", "q", ev, t0, time.Hour),
	} {
		t.Run(req.Kind+"/"+req.Question, func(t *testing.T) {
			stored := roundTrip(t, req.Subject)
			if got := CurrentArtifactHash(req.Kind, req.ArtifactHash, stored); got != req.ArtifactHash {
				t.Fatalf("the stored subject recomputes to %s, the approval was raised with %s: every approve and answer would be refused as changed", short(got), short(req.ArtifactHash))
			}
			if ok, err := ResumeAllowed(req.ArtifactHash, CurrentArtifactHash(req.Kind, req.ArtifactHash, stored), "approved"); !ok {
				t.Fatalf("an untouched approval did not resume: %v", err)
			}
			edited := roundTrip(t, req.Subject)
			edited["editedSince"] = "the artifact changed"
			if ok, err := ResumeAllowed(req.ArtifactHash, CurrentArtifactHash(req.Kind, req.ArtifactHash, edited), "approved"); ok || !errors.Is(err, ErrArtifactChanged) {
				t.Fatalf("an edited subject resumed (ok=%v err=%v); that is approving one thing and running another", ok, err)
			}
		})
	}
}

// A failure-path question is a decision about the FAILURE: the subject names
// the symptom, the step and the words, and two different failures of the
// same step are two different decisions.
func TestFailureApprovalIsAboutTheFailure(t *testing.T) {
	ev := Evidence{Reason: "r", Source: EvidenceSourceRules}
	a := FailureApproval(ApprovalKindFeedback, "run-1", "fetch", SymptomHuman, "first failure", "q", ev, t0, time.Hour)
	b := FailureApproval(ApprovalKindFeedback, "run-1", "fetch", SymptomHuman, "second failure", "q", ev, t0, time.Hour)
	if a.ArtifactHash == b.ArtifactHash {
		t.Fatal("two different failures hash the same, so a decision about one carries to the other")
	}
	if a.Subject["symptom"] != string(SymptomHuman) || a.Subject["stepKey"] != "fetch" || a.Subject["errorMessage"] != "first failure" {
		t.Fatalf("subject = %+v", a.Subject)
	}
	if a.Question != "q" || len(a.Options) != 2 {
		t.Fatalf("question/options = %q %+v", a.Question, a.Options)
	}
}

// Answering a failure question with Abandon is a stop; answering it with
// Retry, or answering some other question with the same word, is not.
func TestAnswerAbandonsReadsTheOfferedOptionOnly(t *testing.T) {
	asked := FailureApproval(ApprovalKindFeedback, "run-1", "s", SymptomHuman, "e", "q", Evidence{}, t0, time.Hour)
	stored := roundTrip(t, map[string]any{"options": asked.Options})["options"]
	if !AnswerAbandons(stored, map[string]any{"value": FailureAnswerAbandon, "label": "Abandon"}) {
		t.Fatal("choosing Abandon on a failure question did not read as a stop")
	}
	if AnswerAbandons(stored, map[string]any{"value": FailureAnswerRetry}) {
		t.Fatal("choosing Retry read as a stop")
	}
	other := []any{map[string]any{"label": "Keep it", "value": "keep"}}
	if AnswerAbandons(other, map[string]any{"value": FailureAnswerAbandon}) {
		t.Fatal("a question that never offered Abandon was stopped by an answer carrying the word")
	}
}

// roundTrip is the subject as the graph returns it: JSON in, JSON out, so a
// []map[string]any comes back as []any and an int as a float64.
func roundTrip(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
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
