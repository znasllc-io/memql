package memql

import (
	"io"
	"log/slog"
	"testing"
	"testing/fstest"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// #2607: @disabled must actually disable specs, traits, and capabilities --
// the last kinds where it was silently swallowed. The live spec path
// registered unconditionally (specDeclToSpec's lifecycle case was a no-op and
// the engine Spec struct has no lifecycle field), and the capability loader
// extracted only dotted names, never annotations. Same contract as #2606:
// skipped at load, deliberately NOT a LoadReport skip (strict boot must not
// trip on an intentional disable).

func lifecycleDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestLoadUnifiedSpecs_DisabledSpecSkipped(t *testing.T) {
	overlay := fstest.MapFS{"specs.memql": {Data: []byte(`@disabled
spec actorEnvelope retiredProbeSpec {
  return role == "admin"
}

@enabled
spec actorEnvelope liveProbeSpec {
  return role == "admin"
}
`)}}
	const domain = "lifecycledisabledspecs"
	memqldsl.RegisterTree(domain, overlay)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	registry := newSpecRegistry()
	rep := newLoadReport()
	if _, err := LoadUnifiedSpecs(lifecycleDiscardLogger(), registry, rep); err != nil {
		t.Fatalf("LoadUnifiedSpecs: %v", err)
	}
	if rep.HasProblems() {
		t.Errorf("@disabled spec must not produce a LoadReport skip (strict boot); skips=%+v", rep.Skipped)
	}
	if registry.Has("retiredProbeSpec") {
		t.Error("@disabled spec registered; @disabled must skip spec registration")
	}
	if !registry.Has("liveProbeSpec") {
		t.Error("enabled peer spec did not register")
	}
	// The disabled name stays reserved: promotion guards refuse it and
	// diagnostics can distinguish disabled from never-declared.
	if !registry.IsDisabled("retiredProbeSpec") {
		t.Error("@disabled spec name not reserved; an authored spec could be promoted over the retired core name")
	}
	if registry.IsDisabled("liveProbeSpec") {
		t.Error("enabled spec wrongly marked disabled")
	}
}

// The gate must sit AFTER body validation: a @disabled spec with a broken
// body must still be rejected, or disabling ships the breakage green and
// re-enabling bricks boot.
func TestLoadUnifiedSpecs_DisabledSpecStillValidated(t *testing.T) {
	overlay := fstest.MapFS{"specs.memql": {Data: []byte(`@disabled
spec actorEnvelope brokenRetiredSpec {
  return 42
}
`)}}
	const domain = "lifecycledisabledspecinvalid"
	memqldsl.RegisterTree(domain, overlay)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	registry := newSpecRegistry()
	rep := newLoadReport()
	if _, err := LoadUnifiedSpecs(lifecycleDiscardLogger(), registry, rep); err != nil {
		t.Fatalf("LoadUnifiedSpecs: %v", err)
	}
	if !rep.HasProblems() {
		t.Error("a @disabled spec with a non-boolean body must still fail validation (parse-phase skip), or re-enabling it later bricks boot")
	}
}

func TestLoadUnifiedSpecs_DisabledTraitSkipped(t *testing.T) {
	overlay := fstest.MapFS{"traits.memql": {Data: []byte(`@disabled
trait retiredProbeTrait {
  return effect == "allow"
}

@enabled
trait liveProbeTrait {
  return effect == "allow"
}
`)}}
	const domain = "lifecycledisabledtraits"
	memqldsl.RegisterTree(domain, overlay)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	registry := newSpecRegistry()
	rep := newLoadReport()
	if _, err := LoadUnifiedSpecs(lifecycleDiscardLogger(), registry, rep); err != nil {
		t.Fatalf("LoadUnifiedSpecs: %v", err)
	}
	if rep.HasProblems() {
		t.Errorf("@disabled trait must not produce a LoadReport skip (strict boot); skips=%+v", rep.Skipped)
	}
	if registry.Has("retiredProbeTrait") {
		t.Error("@disabled trait registered; @disabled must skip trait registration (the _reference sheets promise exactly this)")
	}
	if !registry.Has("liveProbeTrait") {
		t.Error("enabled peer trait did not register")
	}
}

func TestLoadCapabilityNames_DisabledSkipped(t *testing.T) {
	overlay := fstest.MapFS{"capabilities.memql": {Data: []byte(`@disabled
capability integration.probe.retiredVerb {
  args {
    subject string @required
  }
}

@enabled
capability integration.probe.liveVerb {
  args {
    subject string @required
  }
}
`)}}

	names, err := loadCapabilityNamesFromFS(overlay)
	if err != nil {
		t.Fatalf("loadCapabilityNamesFromFS: %v", err)
	}
	if names["integration.probe.retiredVerb"] {
		t.Error("@disabled capability present in the declared set; @disabled must skip it at load")
	}
	if !names["integration.probe.liveVerb"] {
		t.Error("enabled peer capability missing from the declared set")
	}
}

// G3 from the #2642 review: a query referencing a @disabled spec must say
// so, not report "function not found" -- the reservation makes the
// distinction possible; this pins that the validator consumes it.
func TestExpandFunctionCall_DisabledSpecSaysDisabled(t *testing.T) {
	specs := newSpecRegistry()
	specs.MarkDisabled("retiredGate")

	plan := &QueryPlan{
		Root: &FunctionCallExpression{Name: "retiredGate", Args: map[string]any{}},
	}
	err := resolvePlanFunctions(plan, newFunctionRegistry(), specs)
	if err == nil {
		t.Fatal("referencing a disabled spec must error")
	}
	if got := err.Error(); got != `spec "retiredGate" is disabled` {
		t.Errorf("want the disabled-spec diagnostic, got %q", got)
	}
}
