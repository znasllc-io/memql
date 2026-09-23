package work

import (
	"errors"
	"testing"
	"time"
)

func TestOnlyRoutingReviewMayOmitTheRun(t *testing.T) {
	// The invariant the concept's schema gave up when `runId` lost its bang. A
	// schema can say "required" and cannot say "required except for one kind";
	// this can, and it names the kind in the refusal so the next reader is not
	// left inferring the exception from a comment.
	now := time.Date(2026, 9, 7, 3, 30, 0, 0, time.UTC)

	review := RoutingReviewApproval(RoutingProposal{
		ModelId: "qwen3.5:9b", Level: "strong", Week: "2026-W36",
		Direction: "demotion", RuleName: "r", RuleSource: "rule r { }",
	}, now, 0)
	if err := ValidateApprovalKind(review); err != nil {
		t.Fatalf("a routingReview raised by a sweep has no run to park: %v", err)
	}

	for _, kind := range []string{
		ApprovalKindSideEffect, ApprovalKindScopeElevation,
		ApprovalKindBudget, ApprovalKindSkillMint,
		// Raised by the ladder rather than parked on a run, and still
		// pointed at one: the shadow run whose comparison met the threshold.
		ApprovalKindProcedurePromotion,
	} {
		err := ValidateApprovalKind(ApprovalRequest{Kind: kind})
		if !errors.Is(err, ErrApprovalNeedsRun) {
			t.Fatalf("kind %q parks a run and must name one, got %v", kind, err)
		}
	}
	if err := ValidateApprovalKind(ApprovalRequest{}); err == nil {
		t.Fatal("an approval with no kind at all must be refused")
	}
}

func TestTheArtifactHashIsOverTheRuleSource(t *testing.T) {
	// An approval is a decision about one specific rule TEXT. Resume compares
	// the hash, so a rule edited after somebody approved it must no longer match
	// the decision they made -- which is the whole of the guarantee.
	now := time.Now()
	base := RoutingProposal{
		ModelId: "qwen3.5:9b", Level: "strong", Week: "2026-W36",
		Direction: "demotion", RuleName: "r", RuleSource: "rule r { @exclude(\"fleet:qwen3.5:9b\") }",
	}
	a := RoutingReviewApproval(base, now, 0)

	edited := base
	edited.RuleSource = base.RuleSource + " "
	b := RoutingReviewApproval(edited, now, 0)

	if a.ArtifactHash == b.ArtifactHash {
		t.Fatal("a rule that differs by a single space must not hash the same; an approval would carry to it")
	}
}

func TestTheProposalOffersTwoOptionsAndNoThird(t *testing.T) {
	// "Not now" decides nothing and leaves the same evidence proposing again
	// next week, which is the nag this design exists to avoid. A decline is
	// RECORDED, and recording it is what makes it final for that week.
	a := RoutingReviewApproval(RoutingProposal{
		ModelId: "m", Level: "fast", Week: "2026-W36", RuleSource: "rule r { }",
	}, time.Now(), 0)
	if len(a.Options) != 2 {
		t.Fatalf("expected two options, got %d: %+v", len(a.Options), a.Options)
	}
	values := map[string]bool{}
	for _, o := range a.Options {
		values[o["value"].(string)] = true
	}
	if !values["approved"] || !values["rejected"] {
		t.Fatalf("the two options must be approve and decline, got %+v", a.Options)
	}
}

func TestTheSubjectCarriesTheNumbersTheDecisionRestsOn(t *testing.T) {
	// The subject is what the approval SHOWS, and a person being asked to
	// demote a model is entitled to the counts rather than to an adjective.
	a := RoutingReviewApproval(RoutingProposal{
		ModelId: "m", Level: "fast", Week: "2026-W36", RuleSource: "rule r { }",
		Calls: 41, StructuredFailures: 17,
	}, time.Now(), 0)
	if a.Subject["calls"] != 41 || a.Subject["structuredFailures"] != 17 {
		t.Fatalf("subject: %+v", a.Subject)
	}
	if a.Subject["ruleSource"] != "rule r { }" {
		t.Fatal("the subject must carry the exact rule text that was hashed")
	}
}
