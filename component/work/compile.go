package work

// compile.go -- the compile ORDER (design record
// docs/superpowers/specs/2026-09-05-work-spine-design.md, section B
// "Compile").
//
// Catalog exact match on the normalized statement and input shape, then
// near-match at or above the threshold with a gap list, then the cheap
// triage classifier. Trivial becomes a one-step run; sectionable goes to
// the existing deterministic parallel generator; everything else runs one
// reasoning step (compileGoal) that emits an automation draft, which must
// pass Gate 1 before the run proceeds.
//
// THE ORDER IS THE DESIGN, AND IT IS WHY THIS FILE IS PURE. The spec's
// headline claim -- "a goal that fully matches the catalog makes zero
// provider calls" -- is a property of the DECISION, not of a mock. Decide
// takes values and returns a value, so a test proves the claim with no
// engine, no provider and no database; the wiring in
// integrations/planner/work_compile.go is then only responsible for
// obeying it.
//
// A NOTE ON THE SIGNATURE. The existing catalog key (CatalogKey in
// component/memql) hashes the construct SOURCE, per kind, and refuses
// `automation` outright -- which is exactly the kind a compiled goal is.
// So a goal cannot be looked up by it. GoalSignature is the new key: the
// normalized statement plus the SORTED input arg names, which is the pair
// that decides whether two goals want the same template. Sorting matters:
// argument order is a spelling, not a difference, and an order-sensitive
// key would miss its own entries roughly half the time.

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"unicode"
)

// Route is compile's answer.
type Route string

const (
	// RouteUnknown means the caller must run the cheap triage classifier.
	RouteUnknown Route = ""
	// RouteCatalogExact reuses a catalogued template verbatim.
	RouteCatalogExact Route = "catalogExact"
	// RouteCatalogNear reuses one with a gap list to close.
	RouteCatalogNear Route = "catalogNear"
	// RouteTrivial is a one-step run with one reasoning step.
	RouteTrivial Route = "trivial"
	// RouteSectionable is the deterministic parallel generator.
	RouteSectionable Route = "sectionable"
	// RouteAuthor runs compileGoal to emit a fresh draft.
	RouteAuthor Route = "author"
)

// CatalogCandidate is one catalogued template compile may reuse.
type CatalogCandidate struct {
	// ConstructId is the v1:authoring:construct row.
	ConstructId string
	// Name is the automation's name in the registry.
	Name string
	// Signature is the GoalSignature recorded when it was catalogued.
	Signature string
	// Similarity is set by the near-match tier only.
	Similarity float64
	// MissingArgs are the arguments this goal supplies that the candidate
	// does not declare -- the gap list a near match must close.
	MissingArgs []string
}

// CompileInput is everything the decision reads.
type CompileInput struct {
	// Statement is the goal in the person's words.
	Statement string
	// InputKeys are the argument names the goal supplies.
	InputKeys []string
	// Exact are candidates whose signature equals this goal's.
	Exact []CatalogCandidate
	// Near are similarity-ranked candidates, highest first.
	Near []CatalogCandidate
	// NearThreshold is the similarity floor for a near match. The
	// authoring pipeline's existing floor is 0.82; passing 0 means
	// "no near tier", not "everything matches".
	NearThreshold float64
	// Complexity is the triage classifier's verdict, empty until it runs.
	Complexity string
	// Sectionable is the same classifier's second answer, read off the
	// same response -- so consulting it costs no extra call.
	Sectionable bool
}

// Decision is what compile decided and what it will cost.
type Decision struct {
	// Route is the branch taken.
	Route Route
	// Candidate is the template reused, for the two catalog routes.
	Candidate *CatalogCandidate
	// Gaps are the arguments a near match must close.
	Gaps []string
	// NeedsTriage is true when the caller must run the cheap classifier
	// before it can decide. It is the ONLY way to reach triage.
	NeedsTriage bool
	// NeedsModel is true when following this route reaches a provider.
	// An exact catalog hit is the case where it is false, which is the
	// whole point of the catalog.
	NeedsModel bool
}

// Decide runs the compile order. It never calls anything.
func Decide(in CompileInput) Decision {
	// Tier 1: exact. Free, and it outranks everything -- a near match
	// that could beat an exact one would make the cheap path unreachable
	// exactly when it is most valuable.
	if len(in.Exact) > 0 {
		c := in.Exact[0]
		return Decision{Route: RouteCatalogExact, Candidate: &c}
	}

	// Tier 2: near, at or above the threshold. Reaching a model to close
	// a gap list is still far cheaper than authoring from scratch, and it
	// keeps the catalogued template's shape.
	if in.NearThreshold > 0 {
		for i := range in.Near {
			c := in.Near[i]
			if c.Similarity >= in.NearThreshold {
				return Decision{
					Route:      RouteCatalogNear,
					Candidate:  &c,
					Gaps:       append([]string(nil), c.MissingArgs...),
					NeedsModel: true,
				}
			}
			// The list is similarity-descending, so the first miss ends it.
			break
		}
	}

	// Tier 3: the cheap triage classifier. Until it has run there is
	// nothing more to decide.
	switch in.Complexity {
	case "":
		return Decision{Route: RouteUnknown, NeedsTriage: true, NeedsModel: true}
	case "trivial":
		return Decision{Route: RouteTrivial, NeedsModel: true}
	}
	if in.Sectionable {
		// Deterministic after the one shared triage call: the generator
		// synthesizes the bundle from the classifier's own section list
		// and reaches no provider of its own.
		return Decision{Route: RouteSectionable}
	}
	return Decision{Route: RouteAuthor, NeedsModel: true}
}

// GoalSignature is the catalog key for a goal: the normalized statement
// and the sorted input arg names. Always computable -- an empty goal
// hashes like any other, because a missing key and a key for nothing are
// different states and only one of them is a bug.
func GoalSignature(statement string, inputKeys []string) string {
	keys := make([]string, 0, len(inputKeys))
	for _, k := range inputKeys {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(NormalizeStatement(statement)))
	h.Write([]byte("\x00args\x00"))
	h.Write([]byte(strings.Join(keys, ",")))
	return hex.EncodeToString(h.Sum(nil))
}

// NormalizeStatement folds a person's wording down to what it asks for:
// lower case, punctuation dropped, runs of whitespace collapsed. It is
// deliberately crude. Anything cleverer (stemming, stop words) would make
// two goals collide that a person would not call the same, and the near
// tier already exists for everything this misses.
func NormalizeStatement(s string) string {
	var b strings.Builder
	lastSpace := true
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastSpace = false
		case unicode.IsSpace(r):
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		default:
			// Punctuation is dropped rather than replaced with a space,
			// so "yesterday's" and "yesterdays" are one goal.
		}
	}
	return strings.TrimSpace(b.String())
}
