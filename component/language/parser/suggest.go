package parser

import (
	"fmt"
	"sort"
	"strings"
)

// suggest.go -- shared "did you mean" machinery for the parser's fail-loud
// error surface (memql#2358, epic #2351). Before this, the parser had exactly
// three ad-hoc did-you-mean hints (`&`->`&&` and `|`->`||` in the lexer,
// `not in` in parseIdentifierExpression). These helpers centralise the
// Levenshtein-nearest-keyword computation so the unknown-invocation-kind and
// unknown-top-level-keyword rejections can point the author at the construct
// they almost certainly meant.

// keywordSuggestionThreshold bounds how far a mistyped token may sit from a
// known keyword before we stop offering a "did you mean" hint. The canonical
// footgun this hardening targets -- `mutate` (the mutation *declaration*
// verb) written in *call* position where the *invocation* noun `mutation`
// belongs -- is edit distance 3, so the threshold must be at least 3. It is
// held EXACTLY at 3 so genuinely-unrelated garbage (`foobar` -> `tool` is 4)
// does not draw a misleading suggestion.
const keywordSuggestionThreshold = 3

// levenshtein returns the Levenshtein edit distance between a and b using a
// single-row dynamic-programming buffer (O(len(a)*len(b)) time, O(len(b))
// space). Distances are computed over bytes; every keyword in play is ASCII,
// so byte distance equals rune distance.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prevRow := make([]int, len(b)+1)
	for j := 0; j <= len(b); j++ {
		prevRow[j] = j
	}
	for i := 1; i <= len(a); i++ {
		prev := prevRow[0] // prevRow[j-1] before this row overwrites it
		prevRow[0] = i
		for j := 1; j <= len(b); j++ {
			cur := prevRow[j]
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			// min(delete, insert, substitute)
			best := prevRow[j] + 1
			if v := prevRow[j-1] + 1; v < best {
				best = v
			}
			if v := prev + cost; v < best {
				best = v
			}
			prevRow[j] = best
			prev = cur
		}
	}
	return prevRow[len(b)]
}

// nearestKeyword returns the candidate closest to name by edit distance and
// whether a candidate within keywordSuggestionThreshold was found. An exact
// match is skipped: a valid-but-misused keyword (e.g. `mutate` in call
// position) suggests its nearest COUSIN rather than itself. Ties break on the
// lexically-smaller candidate so the result is deterministic regardless of
// the candidate slice's order.
func nearestKeyword(name string, candidates []string) (string, bool) {
	best := ""
	bestDist := -1
	for _, c := range candidates {
		if c == name {
			continue // never suggest the exact token the author already wrote
		}
		d := levenshtein(name, c)
		if bestDist == -1 || d < bestDist || (d == bestDist && c < best) {
			bestDist = d
			best = c
		}
	}
	if bestDist >= 0 && bestDist <= keywordSuggestionThreshold {
		return best, true
	}
	return "", false
}

// didYouMean renders a trailing " (did you mean 'X'?)" fragment, or "" when no
// candidate lands within the suggestion threshold. Callers concatenate it onto
// a base error message so the hint is additive and never stands alone.
func didYouMean(name string, candidates []string) string {
	if s, ok := nearestKeyword(name, candidates); ok {
		return fmt.Sprintf(" (did you mean '%s'?)", s)
	}
	return ""
}

// rewriterHandledDeclKeywords are the author-facing top-level *declaration*
// keywords that NormaliseAll rewrites to the internal `func (Receiver)` form
// BEFORE parseDefinition's contextual-keyword dispatch runs, so they are
// deliberately absent from topLevelDeclParsers / TopLevelDeclKeywords: the
// query / mutate / logic / automation family. They are still valid words an
// author types, so the fail-loud error surface must recognise them (in the
// expected-keyword hint list) and offer them as did-you-mean candidates -- a
// typo'd `quer` should suggest `query` even though the parser never dispatches
// `query` itself. Kept as an explicit list so TestDeclarationKeywordNamesInSync
// can prove declarationKeywordNames = TopLevelDeclKeywords + this family.
var rewriterHandledDeclKeywords = []string{"automation", "logic", "mutate", "query"}

// declarationKeywordNames is the flat, sorted, hand-maintained literal of every
// author-facing top-level DECLARATION keyword: the contextual constructs
// dispatched by topLevelDeclParsers PLUS the rewriter-handled query / mutate /
// logic / automation family (rewriterHandledDeclKeywords).
//
// It is deliberately a LITERAL rather than derived from topLevelDeclParsers /
// TopLevelDeclKeywords. The did-you-mean pools below are consulted from
// parseIdentifierExpression, which sits transitively inside topLevelDeclParsers'
// own package-var initialization graph; deriving these pools from
// TopLevelDeclKeywords (which is derived FROM topLevelDeclParsers) would form a
// Go initialization cycle. TestDeclarationKeywordNamesInSync guards this literal
// against drift from the real dispatch table + the rewriter-handled family, so
// the single-source-of-truth property is preserved by a test instead of a
// compile-time derivation.
var declarationKeywordNames = []string{
	"action", "automation", "builtin", "capability", "concept", "logic",
	"mutate", "policy", "prompt", "provider", "query", "seed", "shape",
	"spec", "tool", "trait",
}

// invocationKindKeywordList returns the invocation-kind prefixes
// (invocationKindKeywords) as a sorted slice, for suggestion candidate pools
// and error-message rendering. invocationKindKeywords is a leaf literal map, so
// reading it introduces no initialization cycle.
func invocationKindKeywordList() []string {
	out := make([]string, 0, len(invocationKindKeywords))
	for kw := range invocationKindKeywords {
		out = append(out, kw)
	}
	sort.Strings(out)
	return out
}

// renderKeywordList quotes and comma-joins a keyword slice for embedding in an
// error message: {"a","b"} -> "'a', 'b'".
func renderKeywordList(kws []string) string {
	quoted := make([]string, len(kws))
	for i, kw := range kws {
		quoted[i] = "'" + kw + "'"
	}
	return strings.Join(quoted, ", ")
}

// kindSuggestionCandidates is the did-you-mean pool for an unknown
// invocation-kind prefix in call position: every invocation kind UNION every
// declaration keyword. Including the declaration keywords is what lets `mutate`
// -- itself a valid declaration verb -- resolve to its invocation cousin
// `mutation` (nearestKeyword skips the exact `mutate` match, leaving `mutation`
// at distance 3 as the nearest), and lets a near-miss of a declaration keyword
// typed in call position (`shappe foo(`) still draw a useful hint.
func kindSuggestionCandidates() []string {
	set := map[string]bool{}
	for kw := range invocationKindKeywords {
		set[kw] = true
	}
	for _, kw := range declarationKeywordNames {
		set[kw] = true
	}
	return sortedKeys(set)
}

// topLevelKeywordHintKeywords is the full author-facing set of tokens that can
// legally open a top-level construct: `func` (the internal procedural form) +
// every declaration keyword (declarationKeywordNames, which already folds in
// the rewriter-handled query / mutate / logic / automation family). It is BOTH
// the expected-keyword hint list AND the did-you-mean candidate pool for
// parseDefinition's unknown-token error, so the error names every keyword an
// author might have meant -- including the ones the rewriter consumes upstream,
// which the old hint list omitted (#2358).
func topLevelKeywordHintKeywords() []string {
	set := map[string]bool{"func": true}
	for _, kw := range declarationKeywordNames {
		set[kw] = true
	}
	return sortedKeys(set)
}

// sortedKeys returns the map's keys as a sorted slice.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
