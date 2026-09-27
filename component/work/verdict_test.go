package work

// The candidate gate and D23's certification half (epic memql#5408, task
// memql#5410).

import (
	"strings"
	"testing"
)

// evidence builds two uses, every hole explained, and one instance-step
// history per instance: the smallest evidence that clears every other half of
// the gate, so each test below exercises the half it names.
func evidence(instances ...[]StepVersions) CandidateEvidence {
	return CandidateEvidence{Uses: 2, UnexplainedHoles: 0, Instances: instances}
}

func versions(vs ...Verdict) StepVersions { return StepVersions{Verdicts: vs} }

// The #5410 acceptance test (D23): a procedure whose corpus holds a disliked
// step version stays a candidate until a liked version of that step exists.
// A procedure certified from a step a person said was wrong would replay the
// wrong thing without a model -- the one outcome certification exists to
// prevent. The reason names WHERE, because "a dislike is holding this" sends a
// person nowhere.
func TestADislikedInstanceStepHoldsACandidateUntilALikedVersionExists(t *testing.T) {
	held := evidence(
		[]StepVersions{versions(), versions(VerdictDislike)},
		[]StepVersions{versions(), versions()},
	)
	ready, reason := CandidateGate(held)
	if ready {
		t.Fatal("a disliked instance step must hold the candidate")
	}
	// The first instance's second step, counted the way a person counts.
	if !strings.HasPrefix(reason, "instance 1, step 2 was disliked") {
		t.Fatalf("the reason must name the instance and the step; got %q", reason)
	}

	cleared := evidence(
		[]StepVersions{versions(), versions(VerdictDislike, VerdictLike)},
		[]StepVersions{versions(), versions()},
	)
	if ready, reason := CandidateGate(cleared); !ready {
		t.Fatalf("a liked version after the dislike clears it; still held: %q", reason)
	}
}

// A person counts from one. The reason is read in Nexus, where "instance 0,
// step 0" names nothing anybody can find; the gate's own indexes stay
// zero-based, and only the sentence counts from one.
func TestTheHeldReasonCountsInstancesAndStepsFromOne(t *testing.T) {
	_, reason := CandidateGate(evidence([]StepVersions{versions(VerdictDislike)}))
	if !strings.HasPrefix(reason, "instance 1, step 1 was disliked") {
		t.Fatalf("the first step of the first instance must read as instance 1, step 1; got %q", reason)
	}
	_, reason = CandidateGate(evidence(
		[]StepVersions{versions(), versions()},
		[]StepVersions{versions(), versions(), versions(VerdictDislike)},
	))
	if !strings.HasPrefix(reason, "instance 2, step 3 was disliked") {
		t.Fatalf("the third step of the second instance must read as instance 2, step 3; got %q", reason)
	}
}

// Neutral is a verdict too (D21): a person looked at the new version and did
// not object, which is what the dislike was waiting for.
func TestANeutralVersionClearsADislike(t *testing.T) {
	if ready, reason := CandidateGate(evidence([]StepVersions{versions(VerdictDislike, VerdictNeutral)})); !ready {
		t.Fatalf("a neutral version after a dislike clears it; still held: %q", reason)
	}
}

// Unseen is not a verdict (D21: "absent meaning unseen"). A version nobody
// looked at says nothing about whether the fault the dislike named is gone,
// and treating silence as approval would certify on nobody's word.
func TestAnUnseenVersionDoesNotClearADislike(t *testing.T) {
	if ready, _ := CandidateGate(evidence([]StepVersions{versions(VerdictDislike, VerdictUnseen)})); ready {
		t.Fatal("an unseen version after a dislike must not clear it")
	}
}

// Deny wins (D15). The newest verdict a person gave on a step is their
// current judgment of it; an earlier like does not outvote a later dislike of
// a version re-run since.
func TestALaterDislikeIsNotClearedByAnEarlierLike(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps StepVersions
		ready bool
	}{
		{"liked, then disliked", versions(VerdictLike, VerdictDislike), false},
		{"disliked, liked, disliked again", versions(VerdictDislike, VerdictLike, VerdictDislike), false},
		{"liked, then unseen", versions(VerdictLike, VerdictUnseen), true},
		{"never judged", versions(), true},
	} {
		if ready, reason := CandidateGate(evidence([]StepVersions{tc.steps})); ready != tc.ready {
			t.Errorf("%s: ready = %v (%q), want %v", tc.name, ready, reason, tc.ready)
		}
	}
}

// A verdict stored in epic C's past tense and handed over unparsed must still
// hold the candidate. The gate reads the verdict it is given the way
// ParseVerdict would, because a dislike the gate cannot recognise is a dislike
// it silently ignores.
func TestAnUnparsedPastTenseDislikeStillHoldsACandidate(t *testing.T) {
	if ready, _ := CandidateGate(evidence([]StepVersions{versions(Verdict("disliked"))})); ready {
		t.Fatal("a stored \"disliked\" must hold the candidate as a dislike")
	}
}

// D14's floor and D15's candidate rule: two uses. One recording is an
// anecdote; a template generalized from it has never been shown to hold on a
// second instance.
func TestFewerThanTwoUsesIsNotACandidate(t *testing.T) {
	for _, uses := range []int{-1, 0, 1} {
		e := evidence()
		e.Uses = uses
		ready, reason := CandidateGate(e)
		if ready {
			t.Fatalf("uses=%d passed the gate", uses)
		}
		if reason == "" {
			t.Fatalf("uses=%d: a held candidate must say why", uses)
		}
	}
}

// D15: every hole classified. An unexplained hole is a position nothing in the
// recordings accounts for, so a replay would have to invent its value -- the
// one thing a model-free replay cannot do.
func TestAnUnexplainedHoleIsNotACandidate(t *testing.T) {
	e := evidence()
	e.UnexplainedHoles = 1
	if ready, reason := CandidateGate(e); ready || reason == "" {
		t.Fatalf("an unexplained hole passed the gate (ready=%v, reason=%q)", ready, reason)
	}
}

// The lift writes the entry rung from the gate: shadow when it passes, and
// candidate -- with the gate's reason as the ladder's reason -- when it does
// not.
func TestEntryRungIsShadowOnlyWhenTheGatePasses(t *testing.T) {
	if rung, reason := EntryRung(evidence()); rung != RungShadow || reason == "" {
		t.Fatalf("EntryRung(ready) = (%q, %q), want shadow with a reason", rung, reason)
	}
	held := evidence([]StepVersions{versions(VerdictDislike)})
	_, gateReason := CandidateGate(held)
	rung, reason := EntryRung(held)
	if rung != RungCandidate {
		t.Fatalf("EntryRung(held) = %q, want candidate", rung)
	}
	if reason != gateReason {
		t.Fatalf("the entry reason must be the gate's (%q), got %q", gateReason, reason)
	}
}

// Epic C wrote its feedback in the past tense ("liked" / "disliked"), D21
// speaks in the present, and a person's client may shout. All of them are the
// same verdict; anything else is unseen rather than a guess.
func TestParseVerdictAcceptsThePastTenseEpicCWrote(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Verdict
	}{
		{"like", VerdictLike},
		{"liked", VerdictLike},
		{"dislike", VerdictDislike},
		{"disliked", VerdictDislike},
		{"DISLIKE", VerdictDislike},
		{" Liked ", VerdictLike},
		{"neutral", VerdictNeutral},
		{"meh", VerdictUnseen},
		{"", VerdictUnseen},
	} {
		if got := ParseVerdict(tc.in); got != tc.want {
			t.Errorf("ParseVerdict(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTheUsesReasonReadsAsASentence: the gate's reason is shown verbatim in
// Nexus under "why it last moved", so its counts must read as English.
func TestTheUsesReasonReadsAsASentence(t *testing.T) {
	for uses, want := range map[int]string{0: "never used", 1: "used once", 3: "used 3 times"} {
		_, reason := CandidateGate(CandidateEvidence{Uses: uses})
		if !strings.HasPrefix(reason, want+" ") && !strings.HasPrefix(reason, want+",") {
			t.Errorf("uses %d: reason %q, want it to open %q", uses, reason, want)
		}
	}
}
