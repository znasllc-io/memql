package memql

import (
	"io"
	"log/slog"
	"strings"
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
spec actorEnvelope retiredProbeSpec = actor => actor.role == "admin"

@enabled
spec actorEnvelope liveProbeSpec = actor => actor.role == "admin"
`)}}
	const domain = "lifecycledisabledspecs"
	memqldsl.RegisterTree(domain, withLanguageLine(overlay))
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
// re-enabling bricks boot. An edition-2026 body is validated by Lower, which
// runs at Init (memql#5366) -- the loader cannot, since what the body may read
// depends on its binding -- so the loader keeps a @disabled body for the Init
// pass, which lowers it exactly as an enabled one and registers nothing. A
// sound @disabled body beside it stays quiet: an intentional disable is still
// not a strict-boot problem.
func TestLoadUnifiedSpecs_DisabledSpecStillValidated(t *testing.T) {
	eng, err := bootLowerTree(t, map[string]string{
		"specs.memql": `use common.shapes.{ actorEnvelope }

/// Retired, and its body is a number, not a condition.
@disabled
spec actorEnvelope brokenRetiredSpec = actor => 42

/// Retired, and sound.
@disabled
spec actorEnvelope soundRetiredSpec = actor => actor.role == "admin"
`,
	})
	if err == nil {
		t.Fatal("a @disabled spec with a non-boolean body booted clean; re-enabling it would brick boot")
	}
	skips := lowerInitSkips(eng)
	if len(skips) != 1 {
		t.Fatalf("want exactly the broken body refused, got %d skip(s):\n%s", len(skips), strings.Join(skips, "\n"))
	}
	for _, want := range []string{
		"spec brokenRetiredSpec",
		"@disabled, and its body does not lower -- re-enabling it would refuse boot",
		"`42` is a number, and a condition must be boolean",
	} {
		if !strings.Contains(skips[0], want) {
			t.Errorf("the refusal does not say %q:\n%s", want, skips[0])
		}
	}
	for _, name := range []string{"brokenRetiredSpec", "soundRetiredSpec"} {
		if eng.specs.Has(name) {
			t.Errorf("@disabled %s was registered; validating a disabled body must not make it callable", name)
		}
		if !eng.specs.IsDisabled(name) {
			t.Errorf("@disabled %s lost its name reservation", name)
		}
	}
}

func TestLoadUnifiedSpecs_DisabledTraitSkipped(t *testing.T) {
	overlay := fstest.MapFS{"traits.memql": {Data: []byte(`@disabled
trait retiredProbeTrait = row => row.effect == "allow"

@enabled
trait liveProbeTrait = row => row.effect == "allow"
`)}}
	const domain = "lifecycledisabledtraits"
	memqldsl.RegisterTree(domain, withLanguageLine(overlay))
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
