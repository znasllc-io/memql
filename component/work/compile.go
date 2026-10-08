package work

// Shared compile values and stable goal signatures. Planning order lives in
// the installed defaultWorkSpine automation, not in a second native policy.

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
	// Rung is RungNone for an authored construct. The planner filters learned
	// procedures through DecideServe, ranks trusted before canary, and binds
	// the rung into the scoped handle so selection uses the replay template.
	Rung Rung
}

// Decision carries a decomposition validation result back to the Spine.
type Decision struct{ Route Route }

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
