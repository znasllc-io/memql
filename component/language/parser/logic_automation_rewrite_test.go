package parser

import (
	"strings"
	"testing"
)

// Logic struct form rewrites to `func (Logic) NAME(...) (any, error)`
// with the body's statements inline. The `args { ... }` block lifts
// to the file top so the parser's existing args attachment handles
// it; `args.X` references in the body translate to `ctx.X` to match
// the runtime envelope.
func TestNormaliseLogicSource_BasicRewrite(t *testing.T) {
	src := `@useQuery(queryFoo)
@description("test")
logic doFoo {
  args {
    x string @required
  }
  body {
    result := queryFoo({ y: args.x })
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
	if strings.Contains(out, "args.x") {
		t.Fatalf("expected args.x to translate to ctx.x; got %q", out)
	}
	if !strings.Contains(out, "queryFoo({ y: ctx.x })") {
		t.Fatalf("expected body to carry the translated call; got %q", out)
	}
}

// Automation struct form rewrites to `func (Automation) NAME(ctx any)`
// with one `:=` assignment per step. Step bodies of the form
// `name { args }` translate to `name({ args })`, and `event.X`
// references translate to `ctx.input.X`.
func TestNormaliseAutomationSource_StepRewrite(t *testing.T) {
	src := `@enabled
@trigger(event="graph.node.created.*.v1:cognition:space")
@useLogic(joinAgents)
@description("Demo automation.")
automation autoJoinSI {
  step joinAgents {
    logicJoinAgents { spaceId: event.node.id }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "func (Automation) autoJoinSI(ctx any)") {
		t.Fatalf("expected procedural rewrite; got %q", out)
	}
	if !strings.Contains(out, "joinAgents := logicJoinAgents({ spaceId: ctx.input.node.id })") {
		t.Fatalf("step did not rewrite to assignment + translated call; got %q", out)
	}
	if !strings.Contains(out, "return ctx, nil") {
		t.Fatalf("expected trailing return; got %q", out)
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
			source: `mutation foo { insert thing { id: "x" } }`,
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
@trigger(event="graph.node.created.*.v1:cognition:privateUtterance")
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
	// First step appears before second in the rewritten body.
	classifyIdx := strings.Index(out, "classify := ")
	persistIdx := strings.Index(out, "persist := ")
	if classifyIdx >= persistIdx {
		t.Fatalf("step order not preserved (classify=%d, persist=%d)", classifyIdx, persistIdx)
	}
}
