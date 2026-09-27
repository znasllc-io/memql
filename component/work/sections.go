package work

// sections.go -- decomposition: long work cut into sections that ask the
// catalog before intelligence (epic memql#5414; design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// D24).
//
// The compile order's triage produces a decomposition: named sections, each
// with its inputs, its outputs, a one-line purpose and a REUSE INTENT. Before
// any section is planned live the catalog is asked for it -- an exact hit on
// the section's own signature, then a near match against REUSABLE automations
// only, then intelligence. A section with a catalogued automation spends no
// model at all; that is the whole premise, applied one level down.
//
// THE BOUNDARY RULE. A section boundary must coincide with a footprint
// boundary and a postcondition: a section's end has to be a point where
// everything it changed is done and checkable. A section that changes
// something and declares no postcondition ENDS MID-EFFECT, and is refused --
// the next section, or a later reuse of this one, would start from a state
// nobody checked. A section with neither outputs nor a postcondition has no end
// at all and is refused for the same reason. The rule is decided on what the
// section DECLARES, because at compile time nothing has run yet.
//
// THE NEAR TIER IS LEXICAL AND SPENDS NO MODEL. The goal-level near tier embeds
// the statement; a section's near tier compares the section's purpose with a
// reusable automation's own words, so asking the catalog costs nothing even
// when it misses -- the catalog is asked before intelligence, and an embedding
// call would be intelligence by the back door.

import (
	"fmt"
	"sort"
	"strings"
)

// Section is one piece of a decomposed goal, as triage proposed it.
type Section struct {
	Name        string
	Label       string
	Instruction string
	// Purpose is the one line a catalog entry is matched against.
	Purpose string
	Inputs  []string
	Outputs []string
	// ReuseIntent is the decomposer's PROPOSAL -- reusable, goalSpecific or
	// accountSpecific. The evidence decides the construct's label later.
	ReuseIntent string
	// Effects is what the section changes.
	Effects Footprint
	// Postcondition says how the section's end is checked; empty when none
	// was declared.
	Postcondition string
}

// SectionBoundaryError refuses a decomposition at the section that breaks the
// boundary rule.
type SectionBoundaryError struct {
	Section string
	Reason  string
}

func (e *SectionBoundaryError) Error() string {
	return fmt.Sprintf("decomposition_refused: section %q %s", e.Section, e.Reason)
}

// SectionSignature is a section's catalog key, the goal signature of its
// purpose and its input names -- so a section that asks for what an earlier
// goal asked for finds the construct that goal was served by.
func SectionSignature(s Section) string {
	return GoalSignature(s.Purpose, s.Inputs)
}

// CheckSectionBoundaries applies the boundary rule to every section, in
// order, and refuses at the first that breaks it.
func CheckSectionBoundaries(sections []Section) error {
	for _, s := range sections {
		name := s.Name
		if name == "" {
			name = s.Label
		}
		hasPost := strings.TrimSpace(s.Postcondition) != ""
		if s.Effects.IsSideEffect() && !hasPost {
			return &SectionBoundaryError{Section: name,
				Reason: "ends mid-effect: it changes " + describeEffects(s.Effects) + " and declares no postcondition that checks it"}
		}
		if !hasPost && len(nonEmpty(s.Outputs)) == 0 {
			return &SectionBoundaryError{Section: name,
				Reason: "has no end: it declares neither an output nor a postcondition"}
		}
	}
	return nil
}

func describeEffects(f Footprint) string {
	var parts []string
	if len(f.Concepts) > 0 {
		parts = append(parts, strings.Join(f.Concepts, ", "))
	}
	if f.Files {
		parts = append(parts, "files")
	}
	if f.Machine {
		parts = append(parts, "the machine")
	}
	if f.External {
		parts = append(parts, "an outside service")
	}
	if f.Spend {
		parts = append(parts, "money")
	}
	if len(parts) == 0 {
		return "something"
	}
	return strings.Join(parts, " and ")
}

// SectionCandidate is one construct a section may be served from.
type SectionCandidate struct {
	ConstructId string
	Name        string
	Signature   string
	// Reuse is the construct's EFFECTIVE label (EffectiveReuse).
	Reuse string
	// Text is the construct's own words -- its purpose or title -- that the
	// near tier compares with a section's purpose.
	Text string
	// Args are the argument names the construct declares.
	Args []string
	// Rung is RungNone for an authored construct.
	Rung Rung
}

// SectionRoute is where a section is served from.
type SectionRoute string

const (
	// SectionCatalogExact reuses a construct whose signature is the section's.
	SectionCatalogExact SectionRoute = "catalogExact"
	// SectionCatalogNear reuses a REUSABLE construct close enough to it.
	SectionCatalogNear SectionRoute = "catalogNear"
	// SectionIntelligence plans the section live.
	SectionIntelligence SectionRoute = "intelligence"
)

// SectionDecision is one section's route.
type SectionDecision struct {
	Section    Section
	Route      SectionRoute
	Candidate  *SectionCandidate
	Similarity float64
}

// SectionPlan is the decomposition's routing.
type SectionPlan struct {
	Sections []SectionDecision
	// NeedsModel is true when any section goes to intelligence. A plan whose
	// every section the catalog serves spends no model beyond the triage call
	// that produced it.
	NeedsModel bool
}

// DefaultSectionNearThreshold is the lexical similarity a reusable
// construct's words must reach against a section's purpose. Lexical overlap
// runs lower than an embedding's cosine for the same meaning, which is why it
// is not the goal tier's 0.82.
const DefaultSectionNearThreshold = 0.6

// DecideSections routes every section: an exact hit on its signature first --
// any construct, reusable or not, since an exact signature IS the same ask --
// then the most similar REUSABLE construct at or above the threshold whose
// arguments cover the section's inputs, then intelligence. A goal-specific or
// account-specific construct is never a near hit: it was cut for one goal, and
// serving it to a different one is the over-generalization the reuse label
// exists to prevent.
func DecideSections(sections []Section, exact map[string][]SectionCandidate, reusable []SectionCandidate, nearThreshold float64) SectionPlan {
	if nearThreshold <= 0 {
		nearThreshold = DefaultSectionNearThreshold
	}
	plan := SectionPlan{}
	for _, s := range sections {
		d := SectionDecision{Section: s, Route: SectionIntelligence}
		if hits := exact[SectionSignature(s)]; len(hits) > 0 {
			c := hits[0]
			d.Route, d.Candidate, d.Similarity = SectionCatalogExact, &c, 1
		} else if c, sim, ok := bestNear(s, reusable, nearThreshold); ok {
			d.Route, d.Candidate, d.Similarity = SectionCatalogNear, &c, sim
		}
		if d.Route == SectionIntelligence {
			plan.NeedsModel = true
		}
		plan.Sections = append(plan.Sections, d)
	}
	return plan
}

func bestNear(s Section, reusable []SectionCandidate, threshold float64) (SectionCandidate, float64, bool) {
	type scored struct {
		c   SectionCandidate
		sim float64
	}
	var hits []scored
	for _, c := range reusable {
		if c.Reuse != string(ReuseReusable) || !covers(c.Args, s.Inputs) {
			continue
		}
		if sim := SectionSimilarity(s.Purpose, c.Text); sim >= threshold {
			hits = append(hits, scored{c, sim})
		}
	}
	if len(hits) == 0 {
		return SectionCandidate{}, 0, false
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].sim != hits[j].sim {
			return hits[i].sim > hits[j].sim
		}
		return hits[i].c.Name < hits[j].c.Name
	})
	return hits[0].c, hits[0].sim, true
}

// covers reports whether a construct declares every input the section brings.
// A section input the construct has no argument for would be dropped, and a
// construct run without its input answers a different question.
func covers(args, inputs []string) bool {
	have := map[string]bool{}
	for _, a := range args {
		have[strings.TrimSpace(a)] = true
	}
	for _, in := range nonEmpty(inputs) {
		if !have[in] {
			return false
		}
	}
	return true
}

// sectionStopWords carry no meaning a purpose can be matched on.
var sectionStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "into": true, "that": true,
	"this": true, "each": true, "per": true, "all": true, "any": true, "its": true, "are": true,
	"was": true, "were": true, "will": true, "then": true, "than": true, "them": true, "they": true,
	"their": true, "have": true, "has": true, "not": true, "but": true, "our": true, "your": true,
}

// SectionSimilarity is the Dice coefficient over the two texts' content words
// (NormalizeStatement, then words of three letters or more that are not stop
// words): 1 for the same words, 0 for none in common. Symmetric, bounded, and
// spends nothing.
func SectionSimilarity(a, b string) float64 {
	wa, wb := contentWords(a), contentWords(b)
	if len(wa) == 0 || len(wb) == 0 {
		return 0
	}
	shared := 0
	for w := range wa {
		if wb[w] {
			shared++
		}
	}
	return 2 * float64(shared) / float64(len(wa)+len(wb))
}

func contentWords(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(NormalizeStatement(s)) {
		if len(w) >= 3 && !sectionStopWords[w] {
			out[w] = true
		}
	}
	return out
}

func nonEmpty(xs []string) []string {
	var out []string
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}
