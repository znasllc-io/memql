package automations_test

// strict_automation_boot_test.go -- coverage for the strict automation boot
// gate (memql#2830). Before it, LoadFromUnifiedTree swallowed EVERY compile
// error behind a `continue`: a malformed automation was dropped with a WARN,
// the node booted green, and the workflow was silently missing. Forcing every
// automation to fail compile yielded `LoadAll -> 0 automations, err=<nil>`.
//
// The gate refuses that. NO load problem is exempt -- the old
// "not found in registry" exemption was dead code that could only ever have
// swallowed genuine defects, so it is gone (see the note at the compile-error
// branch in unified_loader.go). Two properties must NOT regress:
//   - MEMQL_DSL_ALLOW_SKIPS remains the operator break-glass;
//   - `_`/`.`-prefixed directories remain soft-disabled, so parking a WIP
//     automation in `<domain>/_disabled/` does not refuse boot.

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// loadedRegistry mirrors app/database.go's concept load so these tests run
// against the same registry a booting node has.
//
// The registry is NOT what makes these tests work: automation slice
// compilation on this path does not resolve concepts, so a nil or empty
// registry yields the same 31 automations. It is here for boot fidelity, not
// coverage -- do not read it as exercising concept resolution.
func loadedRegistry(t *testing.T) concept.Registry {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	return concept.DefaultRegistry()
}

// TestStrictAutomationBoot_EmbeddedTreeIsClean is the healthy-path guard: the
// shipped tree loads every automation cleanly, so the gate never false-fails a
// healthy fleet. This is also the measurement that made the flip safe -- if a
// future edit lands a malformed automation, this test catches it here rather
// than as a refused boot in staging.
func TestStrictAutomationBoot_EmbeddedTreeIsClean(t *testing.T) {
	t.Setenv(memql.AllowSkipsEnvVar, "") // strict regardless of ambient env
	loader := automations.NewLoader(automations.LoaderOptions{Registry: loadedRegistry(t)})

	loaded, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("the shipped automation tree must load clean under strict boot, got: %v", err)
	}
	if len(loaded) == 0 {
		t.Fatal("expected the shipped tree to yield automations; zero means the loader silently blanked the tree -- the exact #2830 failure mode")
	}
	// Pin the COUNT, not just non-emptiness. "err == nil && len > 0" would
	// still pass if a future edit dropped an automation, which is the very
	// failure mode this guard exists to catch. Bump deliberately when adding
	// or removing an automation.
	if len(loaded) != shippedAutomationCount {
		t.Fatalf("shipped automation count = %d, want %d. If you added or removed an automation, update shippedAutomationCount; if you did not, an automation is being silently dropped (#2830).", len(loaded), shippedAutomationCount)
	}
}

// shippedAutomationCount is the number of automations the embedded tree
// yields. Measured when the strict gate landed (memql#2830).
const shippedAutomationCount = 31

// TestStrictAutomationBoot_MalformedAutomationRefusesBoot is the core
// acceptance test: a malformed automation injected as a throwaway domain (the
// same RegisterTree path a product DSL bundle mounts through) makes LoadAll
// return an error instead of dropping it.
//
// Failing-first check: with the `continue` restored in LoadFromUnifiedTree,
// LoadAll returns (automations, nil) and this test fails on the nil error.
func TestStrictAutomationBoot_MalformedAutomationRefusesBoot(t *testing.T) {
	const domain = "s2830fixturebadautomation"
	// Balanced braces so slice extraction finds the block, but a body the
	// automation compiler rejects. Precisely: the garbage inside `steps { }`
	// produces no diagnostic of its own -- `steps` is not the step keyword, so
	// the rewriter sees an automation with no steps and fails with "at least
	// one `step` is required". Either way it is a compile error that must not
	// be swallowed, which is what this pins.
	fixture := fstest.MapFS{
		"automations.memql": {Data: []byte(
			"@enabled\n" +
				"@description(\"bad\")\n" +
				"@trigger(event=\"node.created\", concept=\"v1:cluster:node\")\n" +
				"automation fixtureBadAutomation {\n" +
				"  steps {\n" +
				"    broken ==== &&&& not-a-step\n" +
				"  }\n" +
				"}\n")},
	}
	memqldsl.RegisterTree(domain, fixture)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	t.Run("refuses without escape hatch", func(t *testing.T) {
		t.Setenv(memql.AllowSkipsEnvVar, "")
		loader := automations.NewLoader(automations.LoaderOptions{Registry: loadedRegistry(t)})

		_, err := loader.LoadAll()
		if err == nil {
			t.Fatal("LoadAll must FAIL when an automation cannot compile and the break-glass is unset -- a silently-dropped automation is the #2830 defect")
		}
		if !strings.Contains(err.Error(), "strict automation load refused") {
			t.Fatalf("expected a strict-boot refusal, got: %v", err)
		}
		if !strings.Contains(err.Error(), "fixtureBadAutomation") {
			t.Fatalf("the refusal must NAME the offending automation so the operator can fix it, got: %v", err)
		}
		if !strings.Contains(err.Error(), memql.AllowSkipsEnvVar) {
			t.Fatalf("the refusal must point at the %s break-glass, got: %v", memql.AllowSkipsEnvVar, err)
		}
	})

	t.Run("boots with escape hatch, healthy automations still load", func(t *testing.T) {
		t.Setenv(memql.AllowSkipsEnvVar, "1")
		loader := automations.NewLoader(automations.LoaderOptions{Registry: loadedRegistry(t)})

		loaded, err := loader.LoadAll()
		if err != nil {
			t.Fatalf("LoadAll must SUCCEED with %s=1 (break-glass): %v", memql.AllowSkipsEnvVar, err)
		}
		if len(loaded) == 0 {
			t.Fatal("the break-glass must still load the healthy automations, not blank the tree")
		}
		for _, a := range loaded {
			if a.Name == "fixtureBadAutomation" {
				t.Fatal("the malformed automation must not be returned as runnable")
			}
		}
	})
}

// TestStrictAutomationBoot_TerseLoweringRejectionRefusesBoot covers the OTHER
// newly-gated phase. A terse-lowering rejection drops the WHOLE FILE, not one
// automation, so it is the most destructive silent drop of the three -- and it
// was equally silent before the gate.
//
// The trigger is a real authoring violation (a file-top `args { }` block ahead
// of a terse `=> logic` automation, forbidden by the event-payload-binding
// ADR): the terse form forwards the event payload and declares no args.
func TestStrictAutomationBoot_TerseLoweringRejectionRefusesBoot(t *testing.T) {
	const domain = "s2830fixturetersereject"
	fixture := fstest.MapFS{
		"automations.memql": {Data: []byte(
			"args {\n" +
				"  node any\n" +
				"}\n\n" +
				"automation fixtureTerseRejected @trigger(event=\"system.startup\") => logic logicSomething\n")},
	}
	memqldsl.RegisterTree(domain, fixture)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	t.Run("refuses without escape hatch", func(t *testing.T) {
		t.Setenv(memql.AllowSkipsEnvVar, "")
		loader := automations.NewLoader(automations.LoaderOptions{Registry: loadedRegistry(t)})

		_, err := loader.LoadAll()
		if err == nil {
			t.Fatal("a terse-lowering rejection drops the whole file; it must refuse boot, not vanish with a WARN")
		}
		if !strings.Contains(err.Error(), "terseLowering") {
			t.Fatalf("the refusal must identify the failing phase, got: %v", err)
		}
		if !strings.Contains(err.Error(), domain) {
			t.Fatalf("the refusal must name the offending file, got: %v", err)
		}
	})

	t.Run("boots with escape hatch", func(t *testing.T) {
		t.Setenv(memql.AllowSkipsEnvVar, "1")
		loader := automations.NewLoader(automations.LoaderOptions{Registry: loadedRegistry(t)})

		loaded, err := loader.LoadAll()
		if err != nil {
			t.Fatalf("LoadAll must SUCCEED with %s=1 (break-glass): %v", memql.AllowSkipsEnvVar, err)
		}
		if len(loaded) != shippedAutomationCount {
			t.Fatalf("break-glass must still load the healthy tree: got %d, want %d", len(loaded), shippedAutomationCount)
		}
	})
}

// TestStrictAutomationBoot_SoftDisabledDirsAreSkipped pins the walker
// convention. Every other DSL walker (dslfs.WalkMemqlFiles, the actions and
// capability loaders) skips `_`/`.`-prefixed directories as soft-disabled or
// hidden, and dsl/runtime_mount.go documents that for MEMQL_DSL_PATH bundles.
//
// This walker had no such filter. That was harmless while a drop was a silent
// WARN, but once a load problem REFUSES the load, a product bundle parking a
// WIP automation in `<domain>/_disabled/automations.memql` would take the
// whole fleet down over a file the rest of the DSL system considers disabled.
// Found in review round 2 of memql#2830.
func TestStrictAutomationBoot_SoftDisabledDirsAreSkipped(t *testing.T) {
	// Deliberately unparseable: if the directory is walked at all, this
	// refuses. Its silence is the assertion.
	broken := &fstest.MapFile{Data: []byte(
		"@trigger(event=\"system.startup\")\n" +
			"automation fixtureParkedAutomation {\n" +
			"  ==== garbage &&&& not-a-step\n" +
			"}\n")}

	for _, dir := range []string{"_disabled", ".attic"} {
		t.Run(dir, func(t *testing.T) {
			const domain = "s2830fixturesoftdisabled"
			memqldsl.RegisterTree(domain, fstest.MapFS{dir + "/automations.memql": broken})
			t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

			t.Setenv(memql.AllowSkipsEnvVar, "")
			loader := automations.NewLoader(automations.LoaderOptions{Registry: loadedRegistry(t)})

			loaded, err := loader.LoadAll()
			if err != nil {
				t.Fatalf("a %s/ directory is soft-disabled and must not be walked, got: %v", dir, err)
			}
			if len(loaded) != shippedAutomationCount {
				t.Fatalf("soft-disabled dir must contribute nothing: got %d, want %d", len(loaded), shippedAutomationCount)
			}
		})
	}
}

// TestStrictAutomationBoot_UnextractableHeaderRefusesLoad covers the drop
// phase that had NO signal at all -- not even a WARN. An automation with
// unbalanced braces, or whose opening `{` sits on the next line, was never
// turned into a slice and simply vanished: `LoadAll -> 31, err=<nil>` with a
// workflow missing. Unbalanced braces is the most common malformed shape, so
// before this it was the loudest bug with the quietest failure. Found in
// review round 2 of memql#2830.
func TestStrictAutomationBoot_UnextractableHeaderRefusesLoad(t *testing.T) {
	cases := map[string]string{
		"unbalanced braces": "@trigger(event=\"system.startup\")\n" +
			"automation fixtureUnbalanced {\n" +
			"  step persist {\n" +
			"    mutation createSpawnEvent (nodeId: \"a\", nodeType: \"b\", action: \"stopped\", reason: \"r\")\n" +
			"  }\n",
		"brace on next line": "@trigger(event=\"system.startup\")\n" +
			"automation fixtureBraceNextLine\n" +
			"{\n" +
			"  step persist {\n" +
			"    mutation createSpawnEvent (nodeId: \"a\", nodeType: \"b\", action: \"stopped\", reason: \"r\")\n" +
			"  }\n" +
			"}\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			const domain = "s2830fixtureunextractable"
			memqldsl.RegisterTree(domain, fstest.MapFS{"automations.memql": {Data: []byte(src)}})
			t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

			t.Setenv(memql.AllowSkipsEnvVar, "")
			loader := automations.NewLoader(automations.LoaderOptions{Registry: loadedRegistry(t)})

			_, err := loader.LoadAll()
			if err == nil {
				t.Fatal("an automation that cannot be extracted must refuse the load -- vanishing with zero signal is the #2830 defect")
			}
			if !strings.Contains(err.Error(), "extract") {
				t.Fatalf("the refusal must identify the extract phase, got: %v", err)
			}
		})
	}
}
