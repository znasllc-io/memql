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
