package work

import "testing"

// "Two goal signatures make a construct reusable" (#5418 acceptance).
func TestTwoGoalSignaturesMakeAConstructReusable(t *testing.T) {
	if got := DecideReuse(ReuseEvidence{GoalSignatures: []string{"s1", "s2"}, Uses: 2}, 2); got != ReuseReusable {
		t.Errorf("got %q, want reusable", got)
	}
	if got := DecideReuse(ReuseEvidence{GoalSignatures: []string{"s1", "s1", " s1 "}, Uses: 3}, 2); got == ReuseReusable {
		t.Error("one signature used three times is still one goal")
	}
}

func TestOneGoalKeepsItGoalSpecific(t *testing.T) {
	for _, e := range []ReuseEvidence{{}, {GoalSignatures: []string{"s1"}, Uses: 5}} {
		if got := DecideReuse(e, 2); got != ReuseGoalSpecific {
			t.Errorf("DecideReuse(%+v) = %q, want goalSpecific", e, got)
		}
	}
}

func TestOneAccountTieMakesItAccountSpecific(t *testing.T) {
	if got := DecideReuse(ReuseEvidence{GoalSignatures: []string{"s1"}, AccountIds: []string{"acme", "acme"}}, 2); got != ReuseAccountSpecific {
		t.Errorf("got %q, want accountSpecific", got)
	}
	if got := DecideReuse(ReuseEvidence{GoalSignatures: []string{"s1"}, AccountIds: []string{"acme", "globex"}}, 2); got != ReuseGoalSpecific {
		t.Errorf("two accounts are not one account's; got %q", got)
	}
	if got := DecideReuse(ReuseEvidence{GoalSignatures: []string{"s1", "s2"}, AccountIds: []string{"acme"}}, 2); got != ReuseReusable {
		t.Errorf("reuse across goals outranks the account tie; got %q", got)
	}
}

// "An override is a version and the evidence keeps counting" (#5418
// acceptance, the pure half): the label a person sees is theirs, and the
// evidence's label is still decided underneath it.
func TestAnOverrideWinsAndTheEvidenceStillDecides(t *testing.T) {
	evidence := DecideReuse(ReuseEvidence{GoalSignatures: []string{"s1", "s2", "s3"}}, 2)
	if got := EffectiveReuse(evidence, ReuseGoalSpecific); got != ReuseGoalSpecific {
		t.Errorf("the person's label wins: got %q", got)
	}
	if evidence != ReuseReusable {
		t.Errorf("the evidence keeps counting: got %q", evidence)
	}
	if got := EffectiveReuse(evidence, ""); got != ReuseReusable {
		t.Errorf("with no override the evidence decides: got %q", got)
	}
}

func TestAThresholdOfZeroIsTheDefault(t *testing.T) {
	if got := DecideReuse(ReuseEvidence{GoalSignatures: []string{"s1"}}, 0); got != ReuseGoalSpecific {
		t.Errorf("a zero threshold must not call one goal reusable; got %q", got)
	}
	if ParseReuseLabel("Reusable") != "" || ParseReuseLabel("reusable") != ReuseReusable {
		t.Error("labels are exact")
	}
}

func TestTheFeedbackPolicyReadsTheRowAndDefaultsTheRest(t *testing.T) {
	if p := FeedbackPolicyFrom(nil); p != DefaultFeedbackPolicy() {
		t.Errorf("an absent row is the defaults, got %+v", p)
	}
	p := FeedbackPolicyFrom(map[string]any{"validateAnswers": false, "reusableAfterSignatures": float64(3)})
	if p.ValidateAnswers || p.ReusableAfterSignatures != 3 {
		t.Errorf("got %+v", p)
	}
	if p := FeedbackPolicyFrom(map[string]any{"reusableAfterSignatures": float64(0)}); p.ReusableAfterSignatures != 2 || !p.ValidateAnswers {
		t.Errorf("a zero threshold normalizes and a missing switch reads as on; got %+v", p)
	}
}
