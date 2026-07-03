package parser

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// parser_hardening_test.go -- S3 of the fail-loud syntax epic (memql#2358 /
// #2351). These tests pin the three parser holes the 2026-07-03 audit found,
// each of which previously turned a typo into SILENCE:
//
//  1. an unknown invocation-kind prefix (`mutate createNode(...)` -- the
//     mutation *declaration* verb in *call* position) lowered the leading word
//     to a bare SpecReferenceExpr and dropped the entire call;
//  2. the Parser.Parse() expression fall-through had no expect-EOF, so trailing
//     garbage after a valid expression was silently discarded;
//  3. parseDefinition's expected-keyword hint omitted query/mutate/logic/
//     automation, so a typo'd top-level keyword got a hint list that didn't
//     even contain the keyword it was a typo of.
//
// The quality bar (matching kind_prefixed_invocation_test.go's #2335 rejection
// tests) is actionable, migration-pointing errors -- so these assert on the
// human-facing message text, not just err != nil.

// parseViaMethod runs the raw NewParser(tokens).Parse() METHOD path (as
// distinct from the ParseExpression() package function, which has always had an
// EOF tail check). This is the path that used to silently drop content.
func parseViaMethod(t *testing.T, src string) (Node, error) {
	t.Helper()
	lex := NewLexer(src)
	toks, err := lex.Tokenize()
	if err != nil {
		t.Fatalf("Tokenize(%q) error: %v", src, err)
	}
	return NewParser(toks).Parse()
}

// ---------------------------------------------------------------------------
// Fix 1: unknown invocation-kind prefix is rejected with a nearest-kind hint.
// ---------------------------------------------------------------------------

func TestReject_UnknownInvocationKind_MutateVsMutation(t *testing.T) {
	// The audit's exact probe: `mutate` (declaration verb) used where the
	// invocation noun `mutation` belongs.
	_, err := ParseExpression(`mutate createNode(id:"x")`)
	if err == nil {
		t.Fatal("expected an error for `mutate createNode(...)`, got nil (the call was silently dropped)")
	}
	msg := err.Error()
	for _, want := range []string{
		"mutate",                          // names the offending word
		"not a construct-invocation kind", // explains WHY
		"silently dropped",                // names the failure mode being prevented
		"did you mean 'mutation'?",        // the actionable, migration-pointing fix
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q\n  full: %s", want, msg)
		}
	}
}

func TestReject_UnknownInvocationKind_NotSilentlyDropped(t *testing.T) {
	// Via the Parser.Parse() METHOD -- historically this returned
	// SpecReferenceExpr{Name:"mutate"} with err=nil and dropped `createNode(...)`.
	node, err := parseViaMethod(t, `mutate createNode(id:"x")`)
	if err == nil {
		t.Fatalf("Parse() method silently accepted `mutate createNode(...)`, returned %#v", node)
	}
	if _, ok := err.(*ParseError); !ok {
		t.Errorf("want *ParseError, got %T: %v", err, err)
	}
}

func TestReject_UnknownInvocationKind_InLogicBody(t *testing.T) {
	// Faithful reproduction of the audit probe "in a logic body": the full
	// struct-form logic construct, rewritten by NormaliseAll then parsed. The
	// body's `return <expr>` carries the malformed call.
	src := `logic doThing {
  body {
    return mutate createNode(id: "x")
  }
}`
	rewritten, err := NormaliseAll(src)
	if err != nil {
		t.Fatalf("NormaliseAll error: %v", err)
	}
	_, err = parseViaMethod(t, rewritten)
	if err == nil {
		t.Fatal("expected the logic body to reject `mutate createNode(...)`, got nil")
	}
	if !strings.Contains(err.Error(), "did you mean 'mutation'?") {
		t.Errorf("logic-body error should carry the mutation hint; got: %v", err)
	}
}

func TestReject_UnknownInvocationKind_ArbitraryWord(t *testing.T) {
	// Any `<ident> <ident>(` whose leading word is not an invocation kind is
	// rejected. A word with no close keyword still errors (fail-loud) but
	// carries no misleading suggestion.
	_, err := ParseExpression(`frobnicate doThing(x: 1)`)
	if err == nil {
		t.Fatal("expected an error for `frobnicate doThing(...)`, got nil")
	}
	if !strings.Contains(err.Error(), "not a construct-invocation kind") {
		t.Errorf("unexpected message: %v", err)
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Errorf("`frobnicate` is far from every keyword; no suggestion should be offered: %v", err)
	}
}

func TestReject_UnknownInvocationKind_Regressions(t *testing.T) {
	// The rejection must NOT disturb any valid form. Each of these must parse
	// clean.
	cases := []string{
		`mutation createNode(id:"x")`,               // real invocation kind
		`query allNodes()`,                          // real invocation kind, empty args
		`logic decideThing()`,                       // real invocation kind
		`createNode(id: args.id)`,                   // bare call, `(` right after the name
		`spec requiresOwner`,                        // spec predicate form (no paren)
		`trait isActiveRecord`,                      // trait predicate form (no paren)
		`active && spec requiresOwner`,              // predicate inside a boolean expr
		`args.members.where(m => m.active).count()`, // collection-method chain
		`coalesce(a, b)`,                            // bare primitive
	}
	for _, src := range cases {
		if _, err := ParseExpression(src); err != nil {
			t.Errorf("valid form must still parse: %q\n  got error: %v", src, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Fix 2: EOF is required after the Parser.Parse() expression fall-through.
// ---------------------------------------------------------------------------

func TestRequireEOF_TrailingTokensRejected(t *testing.T) {
	// The audit's exact probe: a valid comparison followed by garbage. The
	// Parse() method used to return the ComparisonExpr and drop the tail.
	node, err := parseViaMethod(t, `active == true bogus trailing tokens`)
	if err == nil {
		t.Fatalf("Parse() method silently dropped trailing tokens, returned %#v", node)
	}
	if !strings.Contains(err.Error(), "after expression") {
		t.Errorf("error should flag the trailing content; got: %v", err)
	}
}

func TestRequireEOF_CleanExpressionStillParses(t *testing.T) {
	node, err := parseViaMethod(t, `active == true`)
	if err != nil {
		t.Fatalf("a clean single expression must parse via the Parse() method: %v", err)
	}
	if _, ok := node.(*ComparisonExpr); !ok {
		t.Fatalf("want *ComparisonExpr, got %T", node)
	}
}

func TestRequireEOF_TrailingSemicolonsTolerated(t *testing.T) {
	// Trailing semicolons are permitted (mirrors ParseExpression); only real
	// tokens after the expression are rejected.
	if _, err := parseViaMethod(t, `active == true;`); err != nil {
		t.Errorf("a trailing semicolon must be tolerated: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Fix 3: parseDefinition's expected-keyword hint is the full set + did-you-mean.
// ---------------------------------------------------------------------------

func TestParseDefinition_UnknownKeyword_ListsRewriterHandledKinds(t *testing.T) {
	// A file that reaches parseDefinition (leading annotation) with a typo'd
	// construct keyword. The previously-omitted rewriter-handled keywords
	// (query/mutate/logic/automation) must now appear in the hint.
	_, err := ParseFile("@enabled\nconept foo { }")
	if err == nil {
		t.Fatal("expected an error for the typo'd `conept` keyword, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"'query'", "'mutate'", "'logic'", "'automation'"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected-keyword hint should list %s (it omitted these before #2358)\n  full: %s", want, msg)
		}
	}
	if !strings.Contains(msg, "did you mean 'concept'?") {
		t.Errorf("expected a `concept` suggestion for `conept`; got: %s", msg)
	}
}

func TestParseDefinition_UnknownKeyword_SuggestsNearest(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{"@description(\"x\")\nquer participant qFoo { }", "did you mean 'query'?"},
		{"use cognition.concepts.{ space }\n\nshaep space s { row.id }", "did you mean 'shape'?"},
		{"@enabled\nmutaton space createSpace { }", "did you mean 'mutate'?"},
	}
	for _, tc := range cases {
		_, err := ParseFile(tc.src)
		if err == nil {
			t.Errorf("expected an error for %q, got nil", tc.src)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("src %q: want suggestion %q\n  got: %v", tc.src, tc.want, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Suggestion helpers.
// ---------------------------------------------------------------------------

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"", "abc", 3},
		{"abc", "", 3},
		{"quer", "query", 1},
		{"conept", "concept", 1},
		{"mutate", "mutation", 3}, // the canonical footgun distance
		{"kitten", "sitting", 3},  // textbook case
	}
	for _, tc := range cases {
		if got := levenshtein(tc.a, tc.b); got != tc.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := levenshtein(tc.b, tc.a); got != tc.want {
			t.Errorf("levenshtein is asymmetric: (%q,%q)=%d want %d", tc.b, tc.a, got, tc.want)
		}
	}
}

func TestDidYouMean_ThresholdBoundary(t *testing.T) {
	pool := kindSuggestionCandidates()
	// mutate -> mutation is distance 3 == threshold: suggested.
	if got := didYouMean("mutate", pool); !strings.Contains(got, "'mutation'") {
		t.Errorf("mutate should suggest mutation; got %q", got)
	}
	// nearestKeyword skips the exact match, so a keyword typed verbatim never
	// suggests itself.
	if got := didYouMean("mutate", []string{"mutate"}); got != "" {
		t.Errorf("an exact-only candidate set must yield no suggestion; got %q", got)
	}
	// foobar -> nearest is distance 4 (> threshold): no suggestion.
	if got := didYouMean("foobar", pool); got != "" {
		t.Errorf("foobar is beyond the threshold; want no suggestion, got %q", got)
	}
}

// TestDeclarationKeywordNamesInSync is the drift guard for the hand-maintained
// declarationKeywordNames literal (kept literal to avoid a package
// initialization cycle -- see the comment on the var). It must equal the real
// dispatch set (TopLevelDeclKeywords) unioned with the rewriter-handled family.
func TestDeclarationKeywordNamesInSync(t *testing.T) {
	set := map[string]bool{}
	for _, kw := range TopLevelDeclKeywords {
		set[kw] = true
	}
	for _, kw := range rewriterHandledDeclKeywords {
		set[kw] = true
	}
	want := make([]string, 0, len(set))
	for kw := range set {
		want = append(want, kw)
	}
	sort.Strings(want)

	got := append([]string(nil), declarationKeywordNames...)
	if !sort.StringsAreSorted(got) {
		t.Errorf("declarationKeywordNames must be kept sorted; got %v", got)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("declarationKeywordNames drifted from the dispatch table.\n  literal: %v\n  want:    %v\n(update declarationKeywordNames or rewriterHandledDeclKeywords)", got, want)
	}
}
