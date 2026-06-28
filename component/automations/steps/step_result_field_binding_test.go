package steps

import (
	"log/slog"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

// memql#2235 -- pins the CORRECT authoring form for reading a prior step's
// OBJECT result field inside a (non-forEach) persist step.
//
// The decide->persist migration pattern is:
//
//	step decide  { logic <pure> { event: event } }   // returns an object
//	step persist { mutationName({ x: field(decide.result, "x"), ... }) }
//
// The form MUST be `field(decide.result, "x")` -- the BARE step name `decide`
// with `.result`, passed as field()'s first arg. The seemingly-equivalent
// `field(step("decide"), "x")` resolves to NIL at runtime (the engine drops
// it), which silently sends the mutation all-missing args -> "required argument
// X is missing". That exact bug shipped in the knowledge index automations
// (#2238) and blocked forge #2244 / cognition #2243 on the seeded-DB
// conformance gate. This test is the cheap (no-DB) guard so it can't recur.
func TestStepResultFieldBinding_CorrectFormResolves(t *testing.T) {
	const src = `@description("binding")
automation bindcheck {
  step decide { logic d { event: event } }
  step persist {
    mutate writeIt { good: field(decide.result, "foo"), broken: field(step("decide"), "foo") }
  }
}`
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	loader := automations.NewLoader(automations.LoaderOptions{Logger: logger})
	auto, err := loader.CompileSource(src, "test:bindcheck")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	var persist *automations.Step
	for _, s := range auto.Steps {
		if s != nil && s.Function != nil && s.Function.Name == "writeIt" {
			persist = s
		}
	}
	if persist == nil {
		t.Fatalf("no persist function step; steps=%+v", auto.Steps)
	}

	eval := automations.NewEvaluator()
	eval.SetStepResult("decide", &automations.StepResult{
		StepId: "decide",
		Status: "success",
		Result: map[string]any{"foo": "BAR"},
	})

	resolved, err := resolveArgsRefs(persist.Function.Args, eval)
	if err != nil {
		t.Fatalf("resolveArgsRefs: %v", err)
	}
	obj, ok := resolved["0"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected resolved shape: %#v", resolved)
	}
	// The correct form MUST resolve to the step result's field value.
	if obj["good"] != "BAR" {
		t.Errorf("field(decide.result, \"foo\") resolved to %#v, want \"BAR\" -- the decide->persist binding form is broken", obj["good"])
	}
	// The broken form is documented here as the trap: it resolves to nil. If a
	// future engine change makes field(step(...)) resolve too, that's fine --
	// loosen this; the load-bearing assertion is `good` above.
	if obj["broken"] == "BAR" {
		t.Logf("note: field(step(\"decide\"),\"foo\") now also resolves; the #2235 trap may be fixed engine-side")
	}
}
