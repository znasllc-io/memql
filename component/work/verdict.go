package work

// verdict.go -- a person's verdict on a step version, and the gate a lifted
// procedure passes to leave candidate (epic memql#5408, task memql#5410;
// design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// D15, D21, D23's certification half).
//
// D21: a verdict is like, dislike or neutral, and ABSENT MEANS UNSEEN. That is
// a fourth state, not a synonym for neutral: a version nobody looked at says
// nothing about the fault a dislike named, so it can never clear one.
//
// D23: a procedure whose corpus holds a disliked step version stays a
// candidate until a liked or neutral version of that step exists. Read with
// D15's "deny wins": the NEWEST verdict a person gave on the step is their
// current judgment, so an earlier like does not clear a later dislike, and
// unseen versions have no vote at all.
//
// A like on a replayed step is also a reinforcement (D23), but that is a
// reliability write the replay makes (reliability.go), not a gate.

import (
	"fmt"
	"strings"
)

// Verdict is one person's judgment of one step version.
type Verdict string

const (
	// VerdictUnseen is no verdict: nobody looked, or the value was not one
	// this build recognises.
	VerdictUnseen Verdict = ""
	// VerdictLike approves the version.
	VerdictLike Verdict = "like"
	// VerdictDislike objects to it.
	VerdictDislike Verdict = "dislike"
	// VerdictNeutral looked and did not object.
	VerdictNeutral Verdict = "neutral"
)

// ParseVerdict reads a stored verdict. Epic C wrote the past tense ("liked",
// "disliked") and D21 speaks in the present; both are the same verdict, in any
// case. Anything else is unseen rather than a guess -- a verdict this build
// cannot read is one it must not act on.
func ParseVerdict(s string) Verdict {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "like", "liked":
		return VerdictLike
	case "dislike", "disliked":
		return VerdictDislike
	case "neutral":
		return VerdictNeutral
	}
	return VerdictUnseen
}

// StepVersions is the verdicts on every version of ONE instance step, oldest
// first -- one entry per version, the newest verdict on that version.
type StepVersions struct{ Verdicts []Verdict }

// heldByDislike reports whether a step's newest verdict is a dislike. Each
// verdict is read through ParseVerdict, so an unparsed "disliked" handed over
// as a Verdict still holds -- a dislike the gate cannot recognise is a dislike
// it silently ignores.
func (v StepVersions) heldByDislike() bool {
	held := false
	for _, verdict := range v.Verdicts {
		switch ParseVerdict(string(verdict)) {
		case VerdictDislike:
			held = true
		case VerdictLike, VerdictNeutral:
			held = false
		}
	}
	return held
}

// CandidateEvidence is what the gate reads about one lifted procedure.
type CandidateEvidence struct {
	// Uses is how many recorded instances the template was generalized from.
	Uses int
	// UnexplainedHoles is the holes D13's order could not classify.
	UnexplainedHoles int
	// Instances is [instance][template step] -> that step's verdict history.
	Instances [][]StepVersions
}

// CandidateGate is D15's candidate rule with D23's certification half: at
// least two uses, every hole classified, and no instance step whose newest
// verdict is a dislike. The reason names the first thing holding the
// procedure, down to the instance and the step, because "something is holding
// this" is not a sentence anybody can act on.
func CandidateGate(e CandidateEvidence) (ready bool, reason string) {
	if e.Uses < 2 {
		return false, usedPhrase(e.Uses) + ", and a procedure needs at least two uses"
	}
	if e.UnexplainedHoles > 0 {
		return false, fmt.Sprintf("%d of its holes are unexplained, and every hole must be classified before it can be compared", e.UnexplainedHoles)
	}
	for i, instance := range e.Instances {
		for j, step := range instance {
			if step.heldByDislike() {
				// Counted from one, as a person counts: the sentence is read in
				// Nexus, where "instance 0, step 0" names nothing anybody can
				// find. Only the text moves; the indexes stay the gate's own.
				return false, fmt.Sprintf("instance %d, step %d was disliked and has no liked or neutral version since", i+1, j+1)
			}
		}
	}
	return true, fmt.Sprintf("used %d times with every hole classified and no step held by a dislike", e.Uses)
}

// EntryRung is the rung the lift writes: shadow when the gate passes,
// candidate otherwise, with the gate's reason as the ladder's reason either
// way.
func EntryRung(e CandidateEvidence) (Rung, string) {
	ready, reason := CandidateGate(e)
	if ready {
		return RungShadow, reason
	}
	return RungCandidate, reason
}

// usedPhrase spells a use count the way a person reads it: the reason is
// shown verbatim in Nexus, and "used 1 times" is the sentence a reader stops
// at.
func usedPhrase(n int) string {
	switch n {
	case 0:
		return "never used"
	case 1:
		return "used once"
	default:
		return fmt.Sprintf("used %d times", n)
	}
}
