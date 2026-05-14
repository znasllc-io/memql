package compiler

import (
	"testing"
)

// TestTranslateCondition_TokenBasedRewrite locks in the exact strings
// the compiler emits for condition expressions after the regex-based
// translateCondition was replaced by a shared-lexer token walk
// (Phase 2, Step 14). Every case below is a condition shape that
// appears in production .memql files; any drift in the rewriter
// surfaces here before the migration harness has to load it.
func TestTranslateCondition_TokenBasedRewrite(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bare truthy step accessor is untouched",
			in:   "matchingConfirmed.empty",
			want: "matchingConfirmed.empty",
		},
		{
			// parseConditionExpression emits `! getAgent.empty` with
			// a space after the bang (bang is a general-purpose
			// operator, not colocated with its operand), so the
			// rewriter round-trips that spelling.
			name: "bang+empty stays verbatim",
			in:   "! getAgent.empty",
			want: "! getAgent.empty",
		},
		{
			name: "bare .metadata is rewritten to $steps",
			in:   "checkAutoProvision.metadata.itemCount > 0",
			want: "$steps.checkAutoProvision.metadata.itemCount > 0",
		},
		{
			name: "first(name).path expands to Bundle.nodes.0.path",
			in:   "first(checkAutoProvision).payload.value == true",
			want: "$steps.checkAutoProvision.result.Bundle.nodes.0.payload.value == true",
		},
		{
			name: "last(name).path expands to Bundle.nodes.-1.path",
			in:   "last(getHistory).payload.timestamp > 0",
			want: "$steps.getHistory.result.Bundle.nodes.-1.payload.timestamp > 0",
		},
		{
			name: "step(\"x\").metadata.y uses legacy sugar",
			in:   "step(\"checkUser\").metadata.itemCount > 0",
			want: "$steps.checkUser.metadata.itemCount > 0",
		},
		{
			name: "step(\"x\") alone implies .result",
			in:   "step(\"checkUser\")",
			want: "$steps.checkUser.result",
		},
		{
			name: "compound && chains preserve && separator",
			in:   "! getAgent.empty && event.payload.status != \"left\" && checkGreeting.empty",
			want: "! getAgent.empty && event.payload.status != \"left\" && checkGreeting.empty",
		},
	}

	c := New(Config{})
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := c.translateCondition(tc.in)
			if got != tc.want {
				t.Fatalf("translateCondition(%q):\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestExtractStepReferences_TokenWalker mirrors the regex-based
// extractor's observable behaviour after the switch to a token-based
// implementation. Keep in lockstep with translateCondition -- a step
// name that translateCondition rewrites must also be detected here.
func TestExtractStepReferences_TokenWalker(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{name: "bare .metadata", in: "foo.metadata.x == 1", want: []string{"foo"}},
		{name: "first call", in: "first(checkAuto).payload.v == true", want: []string{"checkAuto"}},
		{name: "last call", in: "last(history).payload.t > 0", want: []string{"history"}},
		{name: "reserved names skipped", in: "event.payload.x == \"y\"", want: []string{"event"}},
		{name: "compound", in: "!g.empty && h.metadata.n > 0", want: []string{"g", "h"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := extractStepReferences(tc.in)
			for _, w := range tc.want {
				if _, ok := got[w]; !ok {
					t.Errorf("extractStepReferences(%q) missing %q; got %v", tc.in, w, got)
				}
			}
		})
	}
}
