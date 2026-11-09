package router

// THE EVIDENCE FOLD'S DECISIONS (epic memql#5146, design D5).
//
// ===========================================================================
// IT PROPOSES AND NEVER APPLIES
// ===========================================================================
// A week of real calls says more about a model than any probe can, and the
// obvious thing to do with that is demote automatically. This does not, for two
// reasons that are separate and both sufficient.
//
// The first is the never-a-silent-edit rule: routing is a thing a person is
// accountable for, and a rule that appeared overnight because a threshold was
// crossed is an edit nobody made. The second is smaller and more practical -- a
// week of bad luck on one schema is not a reason to route a whole level
// elsewhere, and the evidence cannot tell the two apart.
//
// So the fold opens an approval carrying a rule, hashed over its exact text,
// and a person decides. Declining is RECORDED, which is the difference between
// a proposal and a nag.
//
// ===========================================================================
// THERE IS EXACTLY ONE RENDERER OF THE RULE GRAMMAR, AND IT IS NOT HERE
// ===========================================================================
// The approval's guarantee is that an approved decision cannot carry to a
// different rule, and it rests on hashing the rendered SOURCE. Two renderers of
// one grammar drift, and with a hash the drift is silent: a person approves
// text A and text B is what gets applied.
//
// So the renderer is INJECTED. This package produces a Form and hashes whatever
// the renderer returns; the tree's one renderer fills it in at the call site.

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// The four levels, mirroring the closed set the catalog and the rule grammar
// share. A fifth here that the grammar does not know would render a rule that
// refuses to load.
var evidenceLevels = []string{"fast", "strong", "reasoning", "embeddings"}

// Window is one model's week at one level, as the fold counted it.
type Window struct {
	ModelId string
	Level   string
	// Week is the ISO week as YYYY-Www. It is a KEY: one row per model per
	// level per week, so two folds of the same week land on one row.
	Week string

	Calls              int
	StructuredFailures int
	Repairs            int
	Retries            int
	Parks              int
}

// FailureRate is the structured-failure rate, or zero when nothing was counted.
//
// ZERO FOR AN EMPTY WINDOW. Workflow policy applies its evidence floor: a rate over no calls is not a rate, and the floor is what stops
// this being read as "this model never fails".
func (w Window) FailureRate() float64 {
	if w.Calls <= 0 {
		return 0
	}
	return float64(w.StructuredFailures) / float64(w.Calls)
}

// Valid reports whether a window is complete enough to reason about.
//
// A window with no level is the one this returns false for most often, and it
// is the case that matters: a call that did not say which level it was serving
// cannot support a demotion AT a level, and demoting at every level instead
// would be a far heavier act than the evidence supports.
func (w Window) Valid() bool {
	return strings.TrimSpace(w.ModelId) != "" &&
		validEvidenceLevel(w.Level) &&
		strings.TrimSpace(w.Week) != ""
}

func validEvidenceLevel(level string) bool {
	for _, l := range evidenceLevels {
		if level == l {
			return true
		}
	}
	return false
}

// Form mirrors component/routingrules.Form field for field.
//
// It is a MIRROR rather than an import because this is a leaf module and that
// package lives in the root module, which depends on this one: the import
// would be a cycle. app/'s TestEvidenceFormMirrorsTheRoutingRulesForm, the one
// place both types are visible, fails the build if the two field lists
// diverge.
type Form struct {
	Name        string
	Description string
	// When carries ONLY the condition keys the proposal actually sets.
	//
	// A key present with an EMPTY value is a condition matching only an empty
	// value; a key ABSENT is no condition at all. Emitting every key with "" for
	// the blanks produces a rule that silently never fires -- which on this path
	// would be a demotion a person approved and that did nothing.
	When          map[string]string
	Policy        string
	Level         string
	Precedence    int
	OnUnavailable string
	Excludes      []string
}

// Renderer turns a Form into the rule source a person is asked to approve.
//
// Injected; see the file header. The tree's one renderer is
// component/routingrules.GenerateRule, which is stable across map iteration
// precisely so a re-render of an unchanged rule is byte-identical -- which is
// what makes hashing its output safe.
type Renderer func(Form) (string, error)

// Direction is what a proposal asks for.
const (
	// DirectionDemotion excludes a model at a level.
	DirectionDemotion = "demotion"
	// DirectionPromotion removes an exclusion.
	DirectionPromotion = "promotion"
)

// Proposal is one generated rule, ready to be put to a person.
type Proposal struct {
	ModelId   string
	Level     string
	Week      string
	Direction string
	RuleName  string
	// RuleSource is the exact text a person is asked to approve.
	RuleSource string
	// Hash is over RuleSource, not over the Form. An approval is a decision
	// about one specific rule TEXT, and hashing the inputs would let a renderer
	// change move the text under an approval that still verified.
	Hash string
	// Reason is the sentence the approval puts to the person. It carries the
	// numbers, because "this model is failing" is not a decidable claim and
	// "17 of 41 structured calls failed last week" is.
	Reason string
}

// ProposalHash is the artifact hash an approval carries.
//
// OVER THE RENDERED SOURCE, so it covers the exact text approved. Hashing
// the Form would let a renderer change move the text under an approval that
// still verified -- which is the one failure the hash exists to prevent, and
// the one nobody would notice.
func ProposalHash(ruleSource string) string {
	sum := sha256.Sum256([]byte(ruleSource))
	return hex.EncodeToString(sum[:])
}

// RuleName is the generated rule's name, derived from the direction, the model
// and the level.
//
// DERIVED rather than random, so a second fold of the same evidence produces
// the same name, and a promotion names the demotion it lifts by deriving it
// again (integrations/work's routing review decision).
//
// ITS SHAPE IS A SAVED RULE'S: lowerCamelCase letters and digits. An approved
// demotion is saved into the routing configuration through routingRuleSave's
// own checks (component/memql's ValidateRuleProposal), which refuse any other
// shape, and so do MemQL OS's rule forms. Until 2026-11-08 the name joined
// its parts with underscores, so no approved demotion could have been saved.
//
// The model part is the model id's letters and digits with each run
// capitalised, for a reader, then the first eight hex digits of the SHA-256 of
// the EXACT model id, for uniqueness: "qwen3.5:9b" and "qwen35:9b" share their
// letters and digits, and one name for both would let approving one replace
// the other's demotion. Nothing reads the model back out of a name; it travels
// on the evidence row and in the rule's @exclude, byte-identical to what the
// router selects on.
func RuleName(direction, modelId, level string) string {
	return "evidence" + upperFirst(direction) + upperFirst(level) + modelWords(modelId) + ProposalHash(modelId)[:8]
}

// maxModelWords bounds the readable part of a rule name; the digest after it
// is what keeps a truncated name unique.
const maxModelWords = 40

// modelWords is a model id's ASCII letters and digits, each run capitalised.
func modelWords(modelId string) string {
	var b strings.Builder
	startOfRun := true
	for _, r := range modelId {
		switch {
		case r >= 'a' && r <= 'z':
			if startOfRun {
				r -= 'a' - 'A'
			}
			b.WriteRune(r)
			startOfRun = false
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
			startOfRun = false
		default:
			startOfRun = true
		}
	}
	words := b.String()
	if len(words) > maxModelWords {
		words = words[:maxModelWords]
	}
	return words
}

// upperFirst capitalises a word from a closed ASCII set (a direction, a level)
// by hand rather than through strings.Title, which is deprecated and would
// apply Unicode word rules to a value that has none.
func upperFirst(word string) string {
	if word == "" {
		return ""
	}
	return strings.ToUpper(word[:1]) + word[1:]
}

// SortWindows orders windows so a fold writes them in a stable order.
//
// Two replicas will not both run the fold -- it is on the cron leader -- but a
// re-run after a failure should produce the same sequence of writes as the run
// it is replacing, or the rows carry an ordering that means nothing.
func SortWindows(ws []Window) {
	sort.SliceStable(ws, func(i, j int) bool {
		if ws[i].ModelId != ws[j].ModelId {
			return ws[i].ModelId < ws[j].ModelId
		}
		return levelRank(ws[i].Level) < levelRank(ws[j].Level)
	})
}

func levelRank(level string) int {
	for i, l := range evidenceLevels {
		if l == level {
			return i
		}
	}
	return len(evidenceLevels)
}
