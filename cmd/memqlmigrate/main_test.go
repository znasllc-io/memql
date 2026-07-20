package main

import (
	"strings"
	"testing"
)

func TestRewriteResultNavigation(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "trailing .empty becomes .Empty()",
			in:   "getAgent.empty",
			want: "getAgent.Empty()",
		},
		{
			name: "trailing .count becomes .Len()",
			in:   "step.count",
			want: "step.Len()",
		},
		{
			// Chained access is now migrated to the method form with
			// the post-call chain intact, because the parser accepts
			// `.X()` followed by `.Y.Z` after Phase 6's chained-
			// accessor landing.
			name: "chained .first.payload.id migrates to .First().payload.id",
			in:   "getAgent.first.payload.id",
			want: "getAgent.First().payload.id",
		},
		{
			name: "chained .first.payload.name in return",
			in:   "return getAgent.first.payload.name",
			want: "return getAgent.First().payload.name",
		},
		{
			// Multi-segment paths like foo.result.Bundle.nodes are
			// NOT rewritten -- `result` is a raw record field, not a
			// navigation accessor. The runtime evaluator's step-
			// accessor switch only matches our six names
			// (first/last/empty/count/nodes/ran); other tokens like
			// `result` pass through.
			name: "deep path .result.Bundle.nodes is preserved",
			in:   "source: getAgent.result.Bundle.nodes",
			want: "source: getAgent.result.Bundle.nodes",
		},
		{
			name: "already-migrated stays the same",
			in:   "getAgent.Empty()",
			want: "getAgent.Empty()",
		},
		{
			name: "string literals are untouched",
			in:   `"getAgent.empty is a string"`,
			want: `"getAgent.empty is a string"`,
		},
		{
			name: "multiple rewrites on same line",
			in:   "if getAgent.empty && other.count > 0 { x() }",
			want: "if getAgent.Empty() && other.Len() > 0 { x() }",
		},
		{
			name: "ran suffix rewrites",
			in:   "step.ran",
			want: "step.Ran()",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := rewriteResultNavigation([]byte(tc.in))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("rewriteResultNavigation(%q):\n  got:  %q\n  want: %q", tc.in, string(got), tc.want)
			}
		})
	}
}

func TestRewriteSliceSyntax(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "simple array(string)", in: "scores array(string)", want: "scores []string"},
		{name: "array(int)", in: "x array(int)", want: "x []int"},
		{name: "array of concept", in: "refs array(v1:cognition:space)", want: "refs []v1:cognition:space"},
		{
			name: "multiple on one line",
			in:   "names array(string), ids array(int)",
			want: "names []string, ids []int",
		},
		{
			name: "already migrated stays the same",
			in:   "scores []int",
			want: "scores []int",
		},
		{
			name: "string literals untouched",
			in:   `description "the array(T) syntax" scores array(int)`,
			want: `description "the array(T) syntax" scores []int`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := rewriteSliceSyntax([]byte(tc.in))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("rewriteSliceSyntax(%q):\n  got:  %q\n  want: %q", tc.in, string(got), tc.want)
			}
		})
	}
}

// TestRewritePipelineCombinesRewrites verifies --rewrite=a,b chains
// multiple passes in the order specified. `.first.payload.id` now
// migrates to `.First().payload.id` because the parser accepts
// post-call chaining (Phase 6 chained-accessor landing).
func TestRewritePipelineCombinesRewrites(t *testing.T) {
	src := "scores array(int); x := getAgent.first.payload.id; check := if getAgent.empty { f() }"
	want := "scores []int; x := getAgent.First().payload.id; check := if getAgent.Empty() { f() }"

	pipeline := []rewriter{rewriteResultNavigation, rewriteSliceSyntax}
	got, err := applyPipeline([]byte(src), pipeline)
	if err != nil {
		t.Fatalf("applyPipeline: %v", err)
	}
	if string(got) != want {
		t.Errorf("pipeline:\n  got:  %q\n  want: %q", string(got), want)
	}
}

// TestRewriteDoesNotAlterUnaffectedSource is a no-op guarantee: if a
// file contains nothing matching the rewrite patterns, the output
// equals the input byte-for-byte.
func TestRewriteDoesNotAlterUnaffectedSource(t *testing.T) {
	src := "// unchanged\nfunc (Query) foo(args any) (any, error) {\n  return concept==v1:foo:bar, nil\n}\n"
	for name, fn := range rewriters {
		name, fn := name, fn
		t.Run(name, func(t *testing.T) {
			got, err := fn([]byte(src))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if string(got) != src {
				t.Errorf("%s altered unaffected source:\n  got:  %q\n  want: %q", name, string(got), src)
			}
		})
	}
}

// TestSplitCSVTrimsAndSkipsEmpty guards the command-line multi-rewrite
// parser against trailing commas and whitespace in --rewrite=.
func TestSplitCSVTrimsAndSkipsEmpty(t *testing.T) {
	got := splitCSV(" result-navigation , slice-syntax ,,,")
	want := []string{"result-navigation", "slice-syntax"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("splitCSV: got %v, want %v", got, want)
	}
}

func TestRewriteArgsDescription(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "inline annotation stripped, rest of the field line intact",
			in:   "query q {\n  args {\n    workdir string @required @description(\"path\")\n    ref string\n  }\n}\n",
			want: "query q {\n  args {\n    workdir string @required\n    ref string\n  }\n}\n",
		},
		{
			name: "standalone annotation line removed entirely",
			in:   "query q {\n  args {\n    workdir string\n    @description(\"orphan\")\n  }\n}\n",
			want: "query q {\n  args {\n    workdir string\n  }\n}\n",
		},
		{
			name: "declaration-level annotation untouched",
			in:   "@description(\"load-bearing\")\nquery q {\n  args {\n    x string\n  }\n}\n",
			want: "@description(\"load-bearing\")\nquery q {\n  args {\n    x string\n  }\n}\n",
		},
		{
			name: "concept-field annotation untouched",
			in:   "concept widget {\n  label string @description(\"load-bearing\")\n}\n",
			want: "concept widget {\n  label string @description(\"load-bearing\")\n}\n",
		},
		{
			name: "annotation after the args block close untouched",
			in:   "mutation m {\n  args {\n    x string @description(\"dead\")\n  }\n  shape {\n    y string @description(\"outside args\")\n  }\n}\n",
			want: "mutation m {\n  args {\n    x string\n  }\n  shape {\n    y string @description(\"outside args\")\n  }\n}\n",
		},
		{
			name: "args in a comment or string never opens a block",
			in:   "// args { commentary\nconcept widget {\n  label string @description(\"keep\")\n}\n",
			want: "// args { commentary\nconcept widget {\n  label string @description(\"keep\")\n}\n",
		},
		{
			name: "multiple fields stripped in one block",
			in:   "action a {\n  args {\n    x string @required @description(\"one\")\n    y string @description(\"two\")\n  }\n}\n",
			want: "action a {\n  args {\n    x string @required\n    y string\n  }\n}\n",
		},
	}
	for _, tc := range cases {
		got, err := rewriteArgsDescription([]byte(tc.in))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, string(got), tc.want)
		}
	}
}

// TestRewritesAreRuneOffsetCorrect pins the #2615 discovery: token
// positions are RUNE offsets (the lexer scans []rune), so a multibyte
// character upstream of the rewrite site must not skew the edit range.
// The section-sign prose mirrors dsl/deployment/logic.memql, where
// byte-offset slicing left a dangling paren.
func TestRewritesAreRuneOffsetCorrect(t *testing.T) {
	argsIn := "// clause § 4\nquery q {\n  args {\n    x string @description(\"dead\")\n  }\n}\n"
	argsWant := "// clause § 4\nquery q {\n  args {\n    x string\n  }\n}\n"
	got, err := rewriteArgsDescription([]byte(argsIn))
	if err != nil {
		t.Fatalf("args-description: %v", err)
	}
	if string(got) != argsWant {
		t.Errorf("args-description after multibyte:\n got %q\nwant %q", string(got), argsWant)
	}

	sliceIn := "// clause § 4\nx array(string)\n"
	sliceWant := "// clause § 4\nx []string\n"
	got, err = rewriteSliceSyntax([]byte(sliceIn))
	if err != nil {
		t.Fatalf("slice-syntax: %v", err)
	}
	if string(got) != sliceWant {
		t.Errorf("slice-syntax after multibyte:\n got %q\nwant %q", string(got), sliceWant)
	}

	navIn := "// clause § 4\nreturn getAgent.first.payload.id\n"
	navWant := "// clause § 4\nreturn getAgent.First().payload.id\n"
	got, err = rewriteResultNavigation([]byte(navIn))
	if err != nil {
		t.Fatalf("result-navigation: %v", err)
	}
	if string(got) != navWant {
		t.Errorf("result-navigation after multibyte:\n got %q\nwant %q", string(got), navWant)
	}
}

// TestLexicalTokenRewriteUsesEndPos pins the EndPos half of the rune-
// offset fix (#2658 review: the mutant `prev = tok.Pos + len(tok.Literal)`
// survived the original fixtures). A non-ASCII IDENTIFIER makes byte-len
// overshoot the rune offset, eating the bytes after the token.
func TestLexicalTokenRewriteUsesEndPos(t *testing.T) {
	in := "logic l {\n  body {\n    return résumé.first.payload.x }\n}\n"
	want := "logic l {\n  body {\n    return résumé.First().payload.x }\n}\n"
	got, err := rewriteResultNavigation([]byte(in))
	if err != nil {
		t.Fatalf("result-navigation: %v", err)
	}
	if string(got) != want {
		t.Errorf("non-ASCII identifier rewrite:\n got %q\nwant %q", string(got), want)
	}
}
