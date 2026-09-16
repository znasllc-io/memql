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
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/dslfs"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// withLanguageLine adds the engine's own language line to a throwaway domain
// tree, as every real bundle domain must carry one (memql#5357), so each test
// sees only the problem its fixture is about.
func withLanguageLine(tree fstest.MapFS) fstest.MapFS {
	line := dslfs.Manifest{Language: langparser.LanguageVersion, Edition: langparser.Edition}
	tree[dslfs.ManifestFile] = &fstest.MapFile{Data: []byte(line.Render())}
	return tree
}

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
// yields. Measured when the strict gate landed (memql#2830); 31 -> 32 when
// campaigns gained ingestCampaignFeedback (memql#3461); 32 -> 31 when
// purgeExpiredPolicyTraces went with the rest of the dead policyTrace
// subsystem (memql#4114); 31 -> 33 when shopify gained applyShopifyInboundProduct + reconcileShopifyIndex (memql#4137);
// 33 -> 35 when library gained indexFileOnCreate + archiveFileOnArtifactArchive (memql#4340);
// 35 -> 37 when platform gained dispatchInboundToConnector + reconcileMirroredDomains, the
// data-origins sync runtime's two drivers (epic memql#4378);
// 37 -> 41 when deployment gained the instance lifecycle verbs -- provisionInstance,
// installInstance, repairInstance and the bringUpInstance composition over the first
// two (epic memql#4463); 41 -> 42 with deprovisionInstance, the lifecycle's
// destructive verb (memql#4469); 42 -> 43 with seedSelfAccount, the accounts
// domain's boot seed for the owner's own company (epic memql#4800); 43 -> 44
// with reconcileCustomDomains, the custom-domain verification + provisioning
// sweep (epic memql#4805); 44 -> 46 with the two D11 package update feeds --
// notePackageUpstreamFromWebhook and pollPackageUpstreams (epic memql#4794);
// 46 -> 47 with logsRetentionSweep, the log store's nightly archive-then-delete
// sweep (epic memql#4893); 47 -> 48 with sweepAbandonedPackageDeployments, the
// two-minutely close of a deploy whose node stopped answering (epic
// memql#4900); 48 -> 50 with the work spine's two sweeps --
// sweepWaitingWorkRuns (resume a due timer wait or re-claim an abandoned run)
// and workJournalRetentionSweep (fold the run summary, then archive-then-delete
// the journal) -- epic memql#4966.
//
// The constant read 47 when memql#5048 arrived, not the 50 the line above
// leaves you expecting: three automations went out between those changes
// without a line recorded here. Noted rather than quietly overwritten, because
// the history is the only thing that makes this number auditable, and a gap in
// it is the reason to distrust the next entry.
//
// 47 -> 50 with the three work-spine TEMPLATES: invokeAgent and
// produceArtifact (memql#5048), which `agent()` and the produceArtifact tool
// open goals against, and trainSpecialist (memql#5051), which replaced the
// Plan dispatcher. All three carry @template rather than a trigger -- they are
// invoked by the run that names them.
//
// 50 -> 49 with killSwitchSuspendsRunningPlans, deleted in memql#5053 -- it
// selected Plans on a field runs do not carry. The enforced computer-use kill
// switch is the pre-dispatch gate in integrations/agent/worker/dispatch.go,
// which was untouched throughout.
//
// 49 -> 50 with killSwitchCancelsComputerUseRuns, its successor on the work
// spine (memql#5066). Worth recording that the deletion's third reason -- "it
// had never fired (memql#2870)" -- was wrong: 8ef364cd7 fixed that on
// 2026-07-27, six weeks before the deletion, and the doc comment asserting it
// survived its own fix. The successor selects runs through
// v1:worker:invocation.runId instead of a Plan field, which is why it needs no
// new field on v1:work:run.
//
// 50 -> 51 with workerModelPullStaleSweep (epic memql#5103), the two-minutely
// close of a model pull whose agent replica has gone. It is the sibling of
// sweepAbandonedPackageDeployments both in cadence and in reason: a pull is
// claimed by exactly one replica, so nothing raises an event when the process
// holding it dies, and without a sweep the OS shows a progress bar that will
// never move again.
//
// 51 -> 53 with the scanner's two (epic memql#5146). workerModelProbeStaleSweep
// is workerModelPullStaleSweep's twin, for the same reason in the same words: a
// probe is claimed by exactly one replica, so nothing raises an event when the
// process holding it dies, and the OS shows a suite that will never finish.
// routingEvidenceFold is the nightly read of a week of router decisions that
// PROPOSES a demotion and applies none.
//
// The pair is the argument for keeping this a COUNT rather than a list: both
// arrived in one epic, and a list would have been updated by taking the diff,
// which is the operation that cannot notice a third automation silently
// dropped in the same change.
//
// 53 -> 56 in epic memql#5165 (groups and grants): ensureAccountGroup and
// archiveAccountGroup give an account its group and take it away with the
// account, and reconcileAccountDomains walks each client's own domain toward
// proof of ownership. Three added, none removed -- and the count is what
// SAYS none was removed, which is exactly the check a diff of a list could
// not have made.
// materializeFile adds the Materializer's known execution template.
// checkDeployableHealth adds the bounded website readiness sweep.
const shippedAutomationCount = 60

//
// 56 -> 57 in epic memql#5168 (the per-account front door):
// reconcileAccountFrontDoors, a SECOND sweep beside reconcileCustomDomains
// rather than a step inside it -- steps run in order and a failing one stops
// what follows, so folding them would make one client's DNS provider timing
// out the reason another client's front door stopped being reconciled.
//
// MEASURED, NOT ADDED. This rebase landed on top of #5165, which had taken the
// same constant 53 -> 56 while this branch took it 53 -> 54. Adding the two
// intentions gives 57 and so does measuring, but only one of those is evidence
// -- the loader was asked, the way #5165's own author asked it about the
// embedded-file count for the same reason.

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
			"" +
				"@description(\"bad\")\n" +
				"@trigger(event=\"node.created\", concept=\"v1:cluster:node\")\n" +
				"automation fixtureBadAutomation {\n" +
				"  steps {\n" +
				"    broken ==== &&&& not-a-step\n" +
				"  }\n" +
				"}\n")},
	}
	memqldsl.RegisterTree(domain, withLanguageLine(fixture))
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

// TestStrictAutomationBoot_TerseHeaderRefusesBoot: the retired terse header,
// `automation NAME @trigger(...) => logic L`, matches neither automation
// header the slicer looks for, so without a slicing of its own it would be
// absent from the load without a word (memql#2830). It is sliced as its line,
// and the parser refuses it by name (body_terse_retired, epic memql#5370).
func TestStrictAutomationBoot_TerseHeaderRefusesBoot(t *testing.T) {
	const domain = "s2830fixtureterseheader"
	fixture := fstest.MapFS{
		"automations.memql": {Data: []byte(
			// memqlmigrate:keep -- the retired terse header is the case.
			"automation fixtureTerse @trigger(event=\"system.startup\") => logic logicSomething\n")},
	}
	memqldsl.RegisterTree(domain, withLanguageLine(fixture))
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	t.Run("refuses without escape hatch", func(t *testing.T) {
		t.Setenv(memql.AllowSkipsEnvVar, "")
		loader := automations.NewLoader(automations.LoaderOptions{Registry: loadedRegistry(t)})

		_, err := loader.LoadAll()
		if err == nil {
			t.Fatal("a retired terse header must refuse boot, not vanish")
		}
		if !strings.Contains(err.Error(), "body_terse_retired") {
			t.Fatalf("the refusal must name the retired form, got: %v", err)
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
			memqldsl.RegisterTree(domain, withLanguageLine(fstest.MapFS{dir + "/automations.memql": broken}))
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
			"  persist := mutation createSpawnEvent(nodeId: \"a\", nodeType: \"b\", action: \"stopped\", reason: \"r\")\n",
		"brace on next line": "@trigger(event=\"system.startup\")\n" +
			"automation fixtureBraceNextLine\n" +
			"{\n" +
			"  persist := mutation createSpawnEvent(nodeId: \"a\", nodeType: \"b\", action: \"stopped\", reason: \"r\")\n" +
			"}\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			const domain = "s2830fixtureunextractable"
			memqldsl.RegisterTree(domain, withLanguageLine(fstest.MapFS{"automations.memql": {Data: []byte(src)}}))
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
