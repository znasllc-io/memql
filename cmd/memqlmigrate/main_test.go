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
