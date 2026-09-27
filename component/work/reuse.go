package work

// reuse.go -- whether a construct serves many goals or one, decided by
// evidence (epic memql#5414; design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// D24).
//
// The decomposer PROPOSES a reuse intent, the EVIDENCE decides the label, and
// a person may OVERRIDE it. Two or more distinct goal signatures within the
// owner's work make a construct reusable; one goal keeps it goal-specific; a
// tie to exactly one account makes it account-specific. The override is a
// version, and the evidence keeps counting underneath it, so a surface can
// show both -- a person who called something reusable that nothing has reused
// is a disagreement worth seeing, not one to resolve silently.

import "strings"

// ReuseLabel is a construct's reuse label.
type ReuseLabel string

const (
	// ReuseReusable serves more than one goal.
	ReuseReusable ReuseLabel = "reusable"
	// ReuseGoalSpecific serves one goal.
	ReuseGoalSpecific ReuseLabel = "goalSpecific"
	// ReuseAccountSpecific serves one account's goals.
	ReuseAccountSpecific ReuseLabel = "accountSpecific"
)

// ParseReuseLabel reads a stored label; anything unrecognised is "" (no
// label), never a guess.
func ParseReuseLabel(s string) ReuseLabel {
	switch l := ReuseLabel(strings.TrimSpace(s)); l {
	case ReuseReusable, ReuseGoalSpecific, ReuseAccountSpecific:
		return l
	}
	return ""
}

// ReuseEvidence is what the reuse sweep counted for one construct.
type ReuseEvidence struct {
	// GoalSignatures are the distinct signatures of the goals that used it.
	GoalSignatures []string
	// AccountIds are the distinct accounts those goals were for.
	AccountIds []string
	// Uses counts the runs that used it.
	Uses int
}

// DecideReuse labels a construct from its evidence. reusableAfter is the
// feedbackPolicy's reusableAfterSignatures; a non-positive value is the
// default of 2.
func DecideReuse(e ReuseEvidence, reusableAfter int) ReuseLabel {
	if reusableAfter <= 0 {
		reusableAfter = DefaultFeedbackPolicy().ReusableAfterSignatures
	}
	if len(distinct(e.GoalSignatures)) >= reusableAfter {
		return ReuseReusable
	}
	if len(distinct(e.AccountIds)) == 1 {
		return ReuseAccountSpecific
	}
	return ReuseGoalSpecific
}

// EffectiveReuse is the label a person sees and the near tier reads: their
// override when there is one, the evidence's otherwise.
func EffectiveReuse(evidence, override ReuseLabel) ReuseLabel {
	if override != "" {
		return override
	}
	return evidence
}

func distinct(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
