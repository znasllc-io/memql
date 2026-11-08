package work

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// The ROUTING REVIEW approval (epic memql#5146, design D5).
//
// ===========================================================================
// THE ONE KIND WITH NO RUN
// ===========================================================================
// Every other approval parks a run: a step reached a side effect, a ceiling, a
// question, and the run waits. This one is raised by the nightly evidence fold,
// which is a sweep. There is no run to park, and a synthetic one would be a run
// that never ran, sitting in every list of runs forever.
//
// The concept's `runId` therefore lost its `!`, and this file is what keeps the
// invariant that bang used to carry -- because a schema can say "required" and
// cannot say "required except for one kind", where a function can say exactly
// that and NAME the kind in its refusal.

// ApprovalKindRoutingReview is the fold's proposal.
const ApprovalKindRoutingReview = "routingReview"

// ErrApprovalNeedsRun is returned when an approval that parks a run carries no
// run to park.
var ErrApprovalNeedsRun = errors.New("work: this approval kind must name the run it parks")

// RoutingProposal is what the fold puts to a person.
//
// It carries the rule SOURCE rather than a description of it, because the
// artifact hash is over the source and the person is being asked to approve
// exactly that text.
type RoutingProposal struct {
	ModelId    string
	Level      string
	Week       string
	Direction  string
	RuleName   string
	RuleSource string
	Reason     string
	// The counts, so the approval can show its working. "This model is failing"
	// is not a decidable claim; "17 of 41 structured calls failed" is.
	Calls              int
	StructuredFailures int
}

// RoutingReviewApproval builds the proposal's approval row.
//
// THE ARTIFACT HASH IS OVER THE RULE SOURCE, which is what makes an approval
// un-carryable to a different rule: resume compares the hash, so a rule edited
// after a person approved it no longer matches the decision they made.
func RoutingReviewApproval(p RoutingProposal, now time.Time, ttl time.Duration) ApprovalRequest {
	subject := map[string]any{
		"modelId":            p.ModelId,
		"level":              p.Level,
		"week":               p.Week,
		"direction":          p.Direction,
		"ruleName":           p.RuleName,
		"ruleSource":         p.RuleSource,
		"calls":              p.Calls,
		"structuredFailures": p.StructuredFailures,
	}
	a := newApproval(ApprovalKindRoutingReview, "", "", subject, Evidence{
		Tier:   "evidence",
		Reason: p.Reason,
		Source: "routingEvidenceFold",
	}, now, ttl)
	a.Question = p.Reason
	// TWO OPTIONS AND NO THIRD. "Not now" would decide nothing and leave the
	// same evidence proposing again next week, which is the nag this design
	// exists to avoid: a decline is RECORDED, and recording it is what makes it
	// final for that week's evidence.
	a.Options = []map[string]any{
		{"label": "Approve the rule", "value": "approved"},
		{"label": "Leave routing as it is", "value": "rejected"},
	}
	return a
}

// ValidateApprovalKind refuses an approval that parks a run and names none.
//
// It is the enforcement the concept's schema gave up when `runId` lost its
// bang, moved to where it can NAME the exception. Every writer of an approval
// runs it, so "required except for routingReview" is a sentence the tree
// actually says rather than one a reader has to infer from a comment.
func ValidateApprovalKind(a ApprovalRequest) error {
	if strings.TrimSpace(a.Kind) == "" {
		return fmt.Errorf("work: an approval must name its kind")
	}
	if a.Kind == ApprovalKindRoutingReview {
		return nil
	}
	if strings.TrimSpace(a.RunId) == "" {
		return fmt.Errorf("%w: kind %q", ErrApprovalNeedsRun, a.Kind)
	}
	return nil
}
