package automations_test

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// TestDryRunCompileResolvesLogicConstructUseImports is the regression guard for
// memql#1681.
//
// Background: the MCP run_automation dry-run path fetches an automation's source
// via memql.DSLConstructSource (memql#1663), which -- like ExtractFunctionSlices
// -- prepends the file-top `use <ns>.<construct>.{ ... }` imports to the
// automation slice. The unified-tree load path instead STRIPS those imports, so
// only the dry-run / inline-compile path exercised the use-resolution branch.
//
// The bug: the automation loader's resolveFileUseDeclarations treated every
// `use` declaration's dotted PATH as a concept id (legacy Form A), so a Form-B
// logic import `use library.logic.{ logicIndexDocument, ... }` synthesized the
// concept id `v1:library:logic`, which is not registered, and the compile failed
// with `concept "v1:library:logic" not found in registry`. Every Logic-B
// automation (19 constructs across 7 namespaces) failed to compile over the
// connector.
//
// The fix: resolve Form-B imports by trailing-segment NAME match (tolerating
// non-concept construct imports such as logic), mirroring
// memql.ConceptResolver. This test compiles every `automation` slice (with its
// use preamble, exactly as the dry-run path feeds it) and asserts none fails
// with a "not found in registry" error.
func TestDryRunCompileResolvesLogicConstructUseImports(t *testing.T) {
	// Load the full embedded concept registry, mirroring engine bootstrap
	// (and TestEngineInitLoadsFullDSL). The dry-run compiles against the live
	// registry read-only.
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := memoryNodes.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry is empty after LoadUnifiedConcepts")
	}

	loader := automations.NewLoader(automations.LoaderOptions{Registry: registry})
	tree := memqldsl.Tree()

	// The Logic-B constructs reported in memql#1681, by wrapping automation.
	// Compiling the wrapping automation slice exercises the use-resolution
	// branch the bug lived in.
	wantAutomations := map[string]bool{
		// v1:library:logic family (Index*)
		"indexDocumentOnCreate": false, "indexGeneratedOutputOnCreate": false,
		"indexNoteOnCreate": false, "indexTodoOnCreate": false,
		"indexCalendarEventOnCreate": false, "indexMemoryOnCreate": false,
		"indexLiveSourceOnCreate": false,
		// v1:cognition:logic family (daily-space / session)
		"autoJoinSI": false, "bootstrapSession": false,
		"ensureDailySpaceOnAuthSession": false, "generateResponse": false,
		"provisionDailySpaceOnUserCreate": false, "rolloverDailySpace": false,
		"voiceMigrationOnSecondHuman": false,
		// the remaining per-namespace logic concepts
		"consolidateMemory": false, "conflictDetection": false,
		"killSwitchSuspendsRunningPlans": false, "onDelegationCreated": false,
	}

	sawLogicUseImport := false

	err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, "/automations.memql") {
			return nil
		}
		data, readErr := fs.ReadFile(tree, path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		source := string(data)
		if strings.Contains(source, ".logic.{") {
			sawLogicUseImport = true
		}

		// ExtractAutomationSlices prepends the file-top `use` preamble to each
		// slice -- the exact text the dry-run path compiles (memql#1663).
		for _, slice := range memql.ExtractAutomationSlices(source) {
			origin := "dryrun:" + path + ":" + slice.Name
			if _, err := loader.CompileSource(slice.Source, origin); err != nil {
				if strings.Contains(err.Error(), "not found in registry") {
					t.Errorf("%s: dry-run compile failed on a use-import concept resolution (memql#1681 regression): %v", origin, err)
				} else {
					t.Errorf("%s: dry-run compile failed: %v", origin, err)
				}
				continue
			}
			if _, tracked := wantAutomations[slice.Name]; tracked {
				wantAutomations[slice.Name] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk dsl tree: %v", err)
	}

	if !sawLogicUseImport {
		t.Fatal("no automation file imported a `.logic.{ ... }` construct -- the regression fixture is stale")
	}

	for name, compiled := range wantAutomations {
		if !compiled {
			t.Errorf("expected memql#1681 automation %q to compile via the dry-run source path, but it was never compiled", name)
		}
	}
}
