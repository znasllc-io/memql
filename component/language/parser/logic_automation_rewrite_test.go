package parser

import (
	"strings"
	"testing"
)

// Logic struct form rewrites to `func (Logic) NAME(...) (any, error)`
// with the body's statements inline. The `args { ... }` block lifts
// to the file top so the parser's existing args attachment handles
// it. `args.X` references in the body now pass through verbatim --
// the engine parser and mutation-template parser learned `args.X`
// natively as part of F.3 of the ctx-envelope purge, so the rewriter
// no longer translates them to `ctx.X`.
func TestNormaliseLogicSource_BasicRewrite(t *testing.T) {
	src := `@useQuery(queryFoo)
@description("test")
logic doFoo {
  args {
    x string @required
  }
  body {
    result := queryFoo(y: args.x)
    return result
  }
}`
	out, err := NormaliseLogicSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "func (Logic) doFoo") {
		t.Fatalf("expected procedural rewrite; got %q", out)
	}
	if !strings.Contains(out, "args {") {
		t.Fatalf("expected file-top args block in rewrite; got %q", out)
	}
	if !strings.Contains(out, "queryFoo(y: args.x)") {
		t.Fatalf("expected body to carry args.x untranslated; got %q", out)
	}
	if strings.Contains(out, "ctx.x") {
		t.Fatalf("expected no ctx.X translation in body; got %q", out)
	}
}

// Automation struct form rewrites to `func (Automation) NAME(_ any)`
// with one `:=` assignment per step. Step bodies of the form
// `name { args }` translate to `name(args)`. `event` and
// `event.X` references in step args pass through verbatim -- the
// runtime function-step args resolver recognises them as runtime
// references and resolves them via the evaluator's `event` binding.
// (Historically these were translated to `ctx.input.X`, but
// `ctx.input` is not a recognised runtime reference and the
// translated value fell through as a literal string; see
// memql-cockpit#49.) Per issue #93 the parameter is `_` and there
// is no `return ctx, nil` trailing line.
func TestNormaliseAutomationSource_StepRewrite(t *testing.T) {
	src := `@enabled
@trigger(event="graph.node.created.v1:cognition:space")
@useLogic(joinAgents)
@description("Demo automation.")
automation autoJoinAI {
  step joinAgents {
    logicJoinAgents { partitionId: event.node.id }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "func (Automation) autoJoinAI(_ any)") {
		t.Fatalf("expected procedural rewrite with `_` placeholder param; got %q", out)
	}
	if strings.Contains(out, "(ctx any)") {
		t.Fatalf("rewriter must not emit `ctx any` parameter; got %q", out)
	}
	if !strings.Contains(out, "joinAgents := logicJoinAgents(partitionId: event.node.id)") {
		t.Fatalf("step did not rewrite to assignment + verbatim event ref; got %q", out)
	}
	if strings.Contains(out, "ctx.input") {
		t.Fatalf("rewriter must not emit ctx.input references; got %q", out)
	}
	if strings.Contains(out, "return ctx, nil") {
		t.Fatalf("rewriter must not emit trailing `return ctx, nil`; got %q", out)
	}
}

// Legacy `func (Automation) NAME(...)` source is recognisable so
// upstream loaders can reject it. The rewriter itself only acts on
// struct-form headers; LooksLikeLegacyAutomation is the gate the
// automation loader uses to refuse legacy author input.
func TestLooksLikeLegacyAutomation(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   bool
	}{
		{
			name: "legacy func form",
			source: `@enabled
func (Automation) foo(_ any) {
  return ctx, nil
}`,
			want: true,
		},
		{
			name: "struct form",
			source: `@enabled
automation foo {
  step run { logic doThing { event: event } }
}`,
			want: false,
		},
		{
			name:   "no automation in source",
			source: `mutate foo { insert { id: "x" } }`,
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LooksLikeLegacyAutomation(tc.source); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Multi-step automations preserve source order in the rewritten body
// and let later steps reference prior step outputs via bare
// `<priorStep>.<field>` -- the dotted suffix rides through verbatim.
func TestNormaliseAutomationSource_MultipleStepsAndOutputRefs(t *testing.T) {
	src := `@enabled
@trigger(event="graph.node.created.v1:cognition:privateUtterance")
@useLogic(classifyLeadIntent, recordLeadIfNeeded)
automation leadClassification {
  step classify {
    logicClassifyLeadIntent { text: event.node.payload.text }
  }
  step persist {
    logicRecordLeadIfNeeded { utteranceId: event.node.id, score: classify.score, label: classify.label }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "classify := logicClassifyLeadIntent") {
		t.Fatalf("first step missing; got %q", out)
	}
	if !strings.Contains(out, "persist := logicRecordLeadIfNeeded") {
		t.Fatalf("second step missing; got %q", out)
	}
	if !strings.Contains(out, "classify.score") || !strings.Contains(out, "classify.label") {
		t.Fatalf("step output references should ride through verbatim; got %q", out)
	}
	if !strings.Contains(out, "event.node.payload.text") || !strings.Contains(out, "event.node.id") {
		t.Fatalf("event.X references should ride through verbatim; got %q", out)
	}
	if strings.Contains(out, "ctx.input") {
		t.Fatalf("rewriter must not emit ctx.input references; got %q", out)
	}
	// First step appears before second in the rewritten body.
	classifyIdx := strings.Index(out, "classify := ")
	persistIdx := strings.Index(out, "persist := ")
	if classifyIdx >= persistIdx {
		t.Fatalf("step order not preserved (classify=%d, persist=%d)", classifyIdx, persistIdx)
	}
}

// The `{ event: event }` step body is the conventional plumb-through
// every event-triggered automation uses to forward the trigger event
// to its receiving logic. Both occurrences -- the KEY (LHS of `:`)
// and the VALUE (RHS of `:`) -- must pass through verbatim.
// Translation to `ctx.input` was a half-purge artifact that broke
// runtime resolution two different ways:
//  1. The previous regex was over-eager and rewrote the KEY too,
//     producing `{ ctx.input: ctx.input }` and clobbering the arg
//     name.
//  2. Even after preserving the key, `ctx.input` is NOT a recognised
//     runtime reference in `automations/steps/function.go::
//     isRuntimeReference`, so the translated value fell through as
//     a literal string `"ctx.input"` and the receiving Logic's
//     validator rejected it with `argument "event": expected
//     object, got string`.
//
// Both failure modes surfaced as the "daily space never shows up"
// symptom in memql-cockpit#49.
func TestNormaliseAutomationSource_EventRefsPassThroughVerbatim(t *testing.T) {
	src := `@enabled
@trigger(event="graph.node.created.v1:identity:authSession")
@useLogic(logicEnsureDailySpaceOnAuthSession)
automation ensureDailySpaceOnAuthSession {
  step run {
    logic ensureDailySpaceOnAuthSession { event: event }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Post-C6 (memql#2036) a `logic <name>` step resolves by bare name -- no
	// prefix promotion -- so the rewritten call uses the bare construct name.
	if !strings.Contains(out, "run := ensureDailySpaceOnAuthSession(event: event)") {
		t.Fatalf("expected `{ event: event }` verbatim; got %q", out)
	}
	if strings.Contains(out, "ctx.input") {
		t.Fatalf("rewriter must not emit ctx.input references; got %q", out)
	}
}
