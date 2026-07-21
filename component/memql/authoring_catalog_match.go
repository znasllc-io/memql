package memql

// authoring_catalog_match.go -- the deterministic "reuse-or-author" decision
// layer for the compose-first catalog (epic memql#954, issue #957).
//
// CatalogKey + FindCatalogMatch (authoring_catalog.go) give an EXACT structural
// match. This file adds the two pieces around them that the planner authoring
// loop (#960) and the embedding/similarTo retrieval (the remaining #957
// increment) call:
//
//   - CatalogMatchText: the text to EMBED for fuzzy (semantic) matching --
//     kind + the construct's @description (human intent) + its canonicalized,
//     name-independent body. Semantically-similar constructs land near each
//     other in embedding space even when named differently.
//   - DecideReuse: the orchestration -- an exact CatalogKey hit short-circuits
//     to direct reuse; otherwise it returns the MatchText to feed the
//     similarTo retrieval (and, when that also misses, the planner authors a
//     net-new construct).

import (
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"regexp"
	"strings"
)

// catalogDescRe extracts the body of the first `@description("...")` annotation
// (allowing escaped quotes inside).
var catalogDescRe = regexp.MustCompile(`@description\(\s*"((?:[^"\\]|\\.)*)"\s*\)`)

// CatalogMatchText builds the semantic text used to embed a construct for fuzzy
// catalog matching. It is name-independent (the construct's own name is
// neutralized via canonicalizeConstructSource) so two constructs that do the
// same thing under different names produce similar text. Returns an error if
// the source does not parse for the kind.
func CatalogMatchText(kind, source string) (string, error) {
	name, err := constructName(kind, source)
	if err != nil {
		return "", err
	}
	parts := []string{"kind:" + kind}
	// Ruling-3 precedence (memql#2634): the /// doc comment wins, extracted
	// by the parser's own lexer (LeadingDocComment) rather than a second
	// regex dialect; the @description regex remains the fallback for the
	// annotation-only corpus.
	if doc := languageParser.LeadingDocComment(source); doc != "" {
		parts = append(parts, "intent:"+doc)
	} else if m := catalogDescRe.FindStringSubmatch(source); m != nil && m[1] != "" {
		parts = append(parts, "intent:"+m[1])
	}
	parts = append(parts, "form:"+canonicalizeConstructSource(source, name))
	return strings.Join(parts, " "), nil
}

// ReuseDecision is the outcome of consulting the catalog for a construct the
// planner needs.
type ReuseDecision struct {
	// ExactMatch, when non-nil, is a cataloged construct with an identical
	// CatalogKey -- a safe DIRECT reuse; no embedding round-trip needed.
	ExactMatch *CatalogEntry `json:"exactMatch,omitempty"`
	// MatchText is set ONLY when there is no exact match: the text to embed +
	// run through similarTo over the catalog to find a close (but not
	// identical) reusable construct. If that also misses, the planner authors
	// a net-new construct.
	MatchText string `json:"matchText,omitempty"`
}

// DecideReuse implements the deterministic half of compose-first: an exact
// CatalogKey hit -> reuse directly; otherwise return the MatchText for the
// fuzzy similarTo layer. Returns an error if the candidate source does not
// parse.
func DecideReuse(kind, source string, catalog []CatalogEntry) (ReuseDecision, error) {
	m, err := FindCatalogMatch(kind, source, catalog)
	if err != nil {
		return ReuseDecision{}, err
	}
	if m != nil {
		return ReuseDecision{ExactMatch: m}, nil
	}
	text, err := CatalogMatchText(kind, source)
	if err != nil {
		return ReuseDecision{}, err
	}
	return ReuseDecision{MatchText: text}, nil
}
