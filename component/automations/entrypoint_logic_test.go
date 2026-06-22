package automations

// entrypoint_logic_test.go -- coverage for the entry-point-logic registration
// invariant (memql#1707): every logic marked `@entrypoint` MUST be invokable
// via run_automation, enforced by a registry audit that fails the build on any
// orphan.

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// TestEntryPointLogicsHaveInvocationPath is the build-fail gate: the registry
// audit must pass against the real DSL tree, proving every @entrypoint logic
// has a wrapping automation (authored or auto-generated). It FAILS THE BUILD if
// an @entrypoint logic is orphaned.
func TestEntryPointLogicsHaveInvocationPath(t *testing.T) {
	loader := newResolverLoader(t)
	if err := loader.ValidateEntryPointLogics(); err != nil {
		t.Fatalf("entry-point invariant violated: %v", err)
	}
}

// TestEntryPointLogicAuditCatchesOrphan proves the audit actually catches an
// orphan -- a hand-built @entrypoint logic with no invoking automation must be
// reported. Without this, the gate above could pass vacuously.
func TestEntryPointLogicAuditCatchesOrphan(t *testing.T) {
	wrappers := []memql.EntryPointLogicWrapper{
		{LogicName: "logicOrphanEntryPoint", BareName: "orphanEntryPoint"},
	}
	// No automation invokes logicOrphanEntryPoint.
	err := validateEntryPointLogics(wrappers, nil)
	if err == nil {
		t.Fatal("expected an orphan-entry-point error, got nil")
	}
	if !strings.Contains(err.Error(), "logicOrphanEntryPoint") {
		t.Fatalf("error should name the orphan logic, got: %v", err)
	}

	// With a wrapping automation that invokes it, the audit passes.
	wrapping := &Automation{
		Name:  "orphanEntryPointEntrypoint",
		Steps: []*Step{{ID: "run", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "logicOrphanEntryPoint"}}},
	}
	if err := validateEntryPointLogics(wrappers, []*Automation{wrapping}); err != nil {
		t.Fatalf("audit should pass once the logic is wrapped, got: %v", err)
	}
}

// TestEntryPointLogicInvokable proves the headline acceptance case:
// logicServiceVersionProbe (the engine-only @entrypoint example, memql#1707)
// is invokable via run_automation. It resolves through LoadByName (both
// spellings) to an auto-generated wrapper whose step invokes the logic, and
// the live + dry-run paths converge on the same source.
func TestEntryPointLogicInvokable(t *testing.T) {
	loader := newResolverLoader(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	const wrapperName = "serviceVersionProbeEntrypoint"

	for _, input := range []string{"logicServiceVersionProbe", "serviceVersionProbe"} {
		t.Run(input, func(t *testing.T) {
			auto, err := loader.LoadByName(input)
			if err != nil {
				t.Fatalf("LoadByName(%q): %v -- entry-point logic must be invokable", input, err)
			}
			if auto == nil || auto.Name != wrapperName {
				t.Fatalf("LoadByName(%q): resolved %v, want %q", input, auto, wrapperName)
			}
			// The wrapper must actually invoke the logic so the live path runs it.
			names := invokedLogicNames(auto)
			found := false
			for _, n := range names {
				if n == "logicServiceVersionProbe" {
					found = true
				}
			}
			if !found {
				t.Fatalf("wrapper %q does not invoke logicServiceVersionProbe (steps invoke %v)", wrapperName, names)
			}

			// Dry-run source fetch by the resolved canonical name -- exactly what
			// the runner's dry-run path does. Must return the generated wrapper.
			src, ok := memql.DSLConstructSource(logger, "automation", auto.Name)
			if !ok {
				t.Fatalf("dry-run DSLConstructSource(automation, %q): not found", auto.Name)
			}
			if !strings.Contains(src, "automation "+wrapperName) {
				t.Fatalf("dry-run source for %q does not contain its declaration:\n%s", wrapperName, src)
			}
		})
	}
}

// TestEntryPointWrapperGenerated confirms the generator surfaces
// logicServiceVersionProbe as an @entrypoint logic with a no-arg call
// shape (the logic declares no args block).
func TestEntryPointWrapperGenerated(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	wrappers := memql.EntryPointLogicWrappers(logger)

	var got *memql.EntryPointLogicWrapper
	for i := range wrappers {
		if wrappers[i].LogicName == "logicServiceVersionProbe" {
			got = &wrappers[i]
		}
	}
	if got == nil {
		t.Fatalf("logicServiceVersionProbe not surfaced as @entrypoint; got %d wrappers", len(wrappers))
	}
	if got.BareName != "serviceVersionProbe" {
		t.Errorf("BareName = %q, want serviceVersionProbe", got.BareName)
	}
	if got.WrapperName != "serviceVersionProbeEntrypoint" {
		t.Errorf("WrapperName = %q, want serviceVersionProbeEntrypoint", got.WrapperName)
	}
	// The logic takes no args, so the generated call is the empty-object form.
	if !strings.Contains(got.Source, "logic serviceVersionProbe {}") {
		t.Errorf("generated source should call the logic with no args:\n%s", got.Source)
	}
}
