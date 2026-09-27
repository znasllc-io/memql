package work

// approval_procedure.go -- the one human approval a learned procedure asks for
// (epic memql#5408, task memql#5410; design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// D3, D9, D15).
//
// A procedure certifies itself in shadow and asks a person exactly once, to
// move from shadow to canary; after that yes it climbs and falls on its own
// evidence (ladder.go). So this approval is the whole of the human's part in
// certification, and it has to carry everything the decision rests on: what
// the procedure is, which version, how it matched, and where it was recorded
// from.
//
// THE ARTIFACT HASH IS THE CONSTRUCT'S procedureHash -- the digest over its
// rendered source, the template a replay executes, and the preconditions it
// was learned with. Resume compares the hash before it acts, so a yes never
// carries to a procedure whose source, steps or preconditions changed after
// the person saw it: that procedure is a new candidate and earns its own
// proposal.

import (
	"fmt"
	"strings"
	"time"
)

// ApprovalKindProcedurePromotion is the ladder's proposal to move a learned
// procedure from shadow to canary.
const ApprovalKindProcedurePromotion = "procedurePromotion"

// PromotionProposal is what the ladder puts to a person.
type PromotionProposal struct {
	// OwnerUserId is the person whose decision it is. The approval row's
	// owner is stamped server-side from the run, so the builder writes no
	// owner; the caller raises the approval under this person's actor.
	OwnerUserId string
	// ConstructId and ConstructName name the procedure.
	ConstructId   string
	ConstructName string
	// ProcedureHash is the construct version being approved.
	ProcedureHash string
	// ShadowRunId is the shadow run whose comparison met the threshold --
	// the run the approval names, since every approval but the routing
	// review's names one.
	ShadowRunId string
	// ShadowMatches is the consecutive matches in the streak that proposed.
	ShadowMatches int
	// DistinctBindings is, per free parameter, how many distinct bindings
	// the streak saw.
	DistinctBindings map[string]int
	// RecordedFrom is the provenance stamp (D9): {app, model, effort,
	// sessionIds, runIds}.
	RecordedFrom map[string]any
	// Title is the goal statement the procedure serves.
	Title string
}

// ProcedurePromotionApproval builds the approval row. The subject always
// carries the same seven keys, as copies, so the card reads alike on every
// promotion and the ladder can keep working on its own maps after raising it.
func ProcedurePromotionApproval(p PromotionProposal, requestedAt time.Time) ApprovalRequest {
	bindings := make(map[string]any, len(p.DistinctBindings))
	for id, n := range p.DistinctBindings {
		bindings[id] = n
	}
	recordedFrom := make(map[string]any, len(p.RecordedFrom))
	for k, v := range p.RecordedFrom {
		recordedFrom[k] = v
	}
	subject := map[string]any{
		"constructId":      p.ConstructId,
		"constructName":    p.ConstructName,
		"procedureHash":    p.ProcedureHash,
		"title":            p.Title,
		"shadowMatches":    p.ShadowMatches,
		"distinctBindings": bindings,
		"recordedFrom":     recordedFrom,
	}
	// No expiry: a pending promotion costs nothing while it waits, because
	// the procedure stays in shadow and the app keeps serving.
	a := newApproval(ApprovalKindProcedurePromotion, p.ShadowRunId, "", subject, Evidence{
		Tier:   "evidence",
		Reason: promotionReason(p),
		RuleId: "ladder.promotion",
		Source: EvidenceSourceRules,
	}, requestedAt, 0)
	a.ArtifactHash = p.ProcedureHash
	a.Question = fmt.Sprintf("Promote %s to canary? It matched the app %d times beside it, and would now run for real with the app standing by.", promotionSubjectName(p), p.ShadowMatches)
	// TWO OPTIONS AND NO THIRD, as for the routing review: "not now" would
	// decide nothing and leave the same evidence proposing again. A decline
	// is recorded and spends the streak, which is what makes it final.
	a.Options = []map[string]any{
		{"label": "Promote to canary", "value": "approved"},
		{"label": "Keep it in shadow", "value": "rejected"},
	}
	return a
}

// promotionSubjectName is what the question calls the procedure. The GOAL
// it serves, when the lift recorded one, because that is the only name a
// person recognises: the construct name is derived from the goal signature
// (learnedProcedure_<digest>_l1) and reads as noise in an inbox. The
// construct name is the fallback, and a generic phrase the last resort --
// never an empty quote.
//
// The goal is quoted the way a person writes a quotation, in plain double
// quotes: Go's quoting would show a statement that carries quotation marks of
// its own with backslashes in it, and one that ran over several lines with a
// literal \n. Its whitespace is collapsed instead, so it is one line in the
// question.
func promotionSubjectName(p PromotionProposal) string {
	if t := strings.Join(strings.Fields(p.Title), " "); t != "" {
		return `"` + t + `"`
	}
	if n := strings.TrimSpace(p.ConstructName); n != "" {
		return n
	}
	return "this learned procedure"
}

// promotionReason states the evidence in a sentence: the matches, and the
// fewest distinct bindings any parameter reached, since that is the half of
// the rule a single value could not have satisfied.
func promotionReason(p PromotionProposal) string {
	if len(p.DistinctBindings) == 0 {
		return fmt.Sprintf("matched the app %d consecutive times in shadow, with no parameter to vary", p.ShadowMatches)
	}
	fewest := -1
	for _, n := range p.DistinctBindings {
		if fewest < 0 || n < fewest {
			fewest = n
		}
	}
	return fmt.Sprintf("matched the app %d consecutive times in shadow, across at least %d distinct bindings of every parameter", p.ShadowMatches, fewest)
}
