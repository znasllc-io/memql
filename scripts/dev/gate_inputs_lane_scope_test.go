// Repository-reading gates must run for every input, including mixed Go/non-Go PRs.
package dev

import (
	"os"
	"path/filepath"
	"testing"
)

var gateInputs = []struct {
	path string // repo-relative, must exist
	pkg  string // the package whose tests read it
	gate string // the test that reads it, for the failure message
}{
	{"CLAUDE.md", ".", "TestDocsDoNotTeachRetiredDeclarationKeywords"},
	{"integrations/arch.md", ".", "TestDocsDoNotTeachDeletedGoExtensionPoints"},
	{"component/node/CLAUDE.md", ".", "TestDocsDoNotTeachDeletedGoExtensionPoints"},
	{"docs/public/concepts/events.md", ".", "TestDocsDoNotReferencePrefixedConstructNames"},
	{"scripts/secrets/manifest.yaml", "component/envregistry", "TestEmbeddedManifestInSync"},
	{"component/envregistry/manifest.yaml", "component/envregistry", "TestEmbeddedManifestInSync"},
	{"component/architecture/embedded/topology.model.json", "component/architecture", "TestArchitectureModelIsNotStale"},
	{"dsl/forge/automations.memql", "component/architecture", "TestArchitectureModelIsNotStale"},
	{"dsl/forge/mutations.memql", "component/architecture", "TestArchitectureModelIsNotStale"},
	{"dsl/forge/logic.memql", "component/architecture", "TestArchitectureModelIsNotStale"},
	{"scripts/make/help.sh", "scripts/make", "TestHelpScript"},
	{"scripts/lib/capability.sh", "scripts/lib", "the capability-script contract test"},
	// The `**/*.memql` glob had NO row at all until the memql#3060 review,
	// so the guard asserted nothing in either direction for it -- it could
	// neither notice the glob missing from the bucket nor notice the step
	// lacking the package that reads it. Both of these are outside dsl/
	// (which has its own bucket) and outside the root package.
	{"examples/referencepack/dsl/concepts.memql", "examples/referencepack", "TestReferencePackLoadsAndExtends"},
	{"docs/public/operate/auth/per-row-authz-audit.md", "test/dslconformance", "TestPublicExamplesAreAnnotated"},
	// The embedded-asset inventory (memql#3165). embed_inventory_test.go pins
	// per-package `//go:embed` file COUNTS and lives in the root package, so
	// EVERY file it pins is a gate input -- and 68 of the 270 were in no
	// bucket this lane reads. That is a fail-open with DELAYED BLAME: the
	// PR's `ci-required` went green and the guard then fired on the next
	// author's push to main.
	//
	// One row per glob added to the bucket, and each names a file that ONLY
	// the new glob covers -- a row already matched by `**/*.md` or
	// `**/*.memql` would stay green with the new glob deleted and would
	// therefore assert nothing. `examples/deploypack/dsl/**` deliberately has
	// no row for that reason: all three of its embedded files are `.memql`
	// today, so the glob is future-proofing with nothing yet to pin.
	{"VERSION", ".", "TestEmbeddedFileCountsAreStable"},
	{"component/database/memory-nodes/migrations/20260324000000_initial_setup.up.sql", ".", "TestEmbeddedFileCountsAreStable"},
	{"component/identity/web/static/app.js", ".", "TestEmbeddedFileCountsAreStable"},
	{"component/mcp/icon.svg", ".", "TestEmbeddedFileCountsAreStable"},
	{"dsl/agents/prompts/plannerAgent.tmpl", ".", "TestEmbeddedFileCountsAreStable"},
	{"examples/referencepack/dsl/namespace.pin", ".", "TestEmbeddedFileCountsAreStable"},
	{"packs/reviewspack/dsl/namespace.pin", ".", "TestEmbeddedFileCountsAreStable"},
	{"integrations/integrations.json", ".", "TestEmbeddedFileCountsAreStable"},
	// The error pages (after the 2026-10-11 outage): component/errorpages embeds
	// its two templates, the brand mark and the wordmark face, and the inventory
	// pins all four. Named by a template, which no other `gates` glob covers.
	{"component/errorpages/maintenance.html.tmpl", ".", "TestEmbeddedFileCountsAreStable"},
	// The operator page's upgrade backfill for the error pages. Its gate reads
	// the page and holds the documented command to what the custom-domain and
	// door builders label and annotate, so it lives beside the builders, in
	// integrations/customdomain -- a package no other row put in this step. A
	// `.md`-only change reaches this lane and nothing else.
	{"docs/public/operate/front-door.md", "integrations/customdomain", "TestTheUpgradeBackfillInTheDocsSelectsWhatTheBuildersLabel"},
	// Not embedded either, and the newest of this shape (memql#3326).
	// clients_allowlist_test.go sweeps `git ls-files clients` from the root
	// package, so every file under clients/ is a gate input. A new client is
	// TypeScript, which matches the `portal` bucket -- and portal-checks runs
	// npm, not Go -- so `clients/**` in `gates` is the ONLY thing that puts the
	// guard's own trigger case (a product SPA landing under clients/) in front
	// of a Go lane. `package.json` is named rather than the README on purpose:
	// `**/*.md` already covers clients/README.md, so a row for that file would
	// stay green with `clients/**` deleted and would assert nothing.
	{"clients/os/package.json", ".", "TestClientsDirectoryIsAllowlisted"},
	// The Materializer app copies some of the engine's rules (which files the composer
	// reads, what can start blank, the name a file is filed under) because a person is told
	// before the engine is asked, and os_copies_test.go in integrations/compose reads the
	// app's source to hold each copy to the engine. The app is TypeScript under `clients/**`,
	// so a PR touching only it reaches this lane and nothing else: the package has to be one
	// the step runs, or the guard fires only on the next Go change (the app once said an
	// email template is filed as .json where the engine files .email.json).
	{"clients/os/src/apps/materializer/make.ts", "integrations/compose", "TestTheAppOffersExactlyTheFilesTheComposerReads"},
	{"clients/os/src/items/materialize.ts", "integrations/compose", "TestTheAppOffersExactlyTheFormatsAndPackagesTheEngineWrites"},
	// Not embedded, so embed_inventory_test.go does not pin it -- but a gate
	// input all the same, and in no bucket this lane read until the same
	// sweep found it. Renaming a `service:` reddens the model staleness check,
	// so a green PR could break `main` for the next author.
	//
	// The sweep found a SECOND file of this shape,
	// `component/memql/testdata/spaceid_core_baseline.txt`
	// (TestNoNewSpaceIdInCore). It gets NO row here on purpose: that test
	// lives in `component/memql`, which this step deliberately does not run.
	// It is routed via the `go` bucket to db-tests instead -- the lane that
	// actually runs it. A row here would assert the wrong lane.
	{"arch.yaml", "component/architecture", "TestArchitectureModelIsNotStale"},
	// The platform graph (memql#5727). Its gate lives in the ROOT package and
	// regenerates through cmd/platformgraph, which reads the deploy base, the
	// engine-bff component, the generated cloud front door, the OS navigation
	// contract, arch.yaml's roles and ENGINE_NODE_TYPES -- all non-Go, so a PR
	// touching only one of them reaches this lane and nothing else. One row
	// per kind of input, each naming the root package that must stay in the
	// step for the gate to run.
	{"component/architecture/embedded/platform.graph.json", ".", "TestPlatformGraphIsNotStale"},
	{"deploy/k8s/base/agent.yaml", ".", "TestPlatformGraphIsNotStale"},
	{"deploy/k8s/components/engine-bff/bff.yaml", ".", "TestPlatformGraphIsNotStale"},
	{"deploy/k8s/overlays/cloud/front-door.generated.yaml", ".", "TestPlatformGraphIsNotStale"},
	{"component/memql/os_navigation.json", ".", "TestPlatformGraphIsNotStale"},
	{"arch.yaml", ".", "TestPlatformGraphIsNotStale"},
	{"scripts/lib/engine_build_args.sh", ".", "TestPlatformGraphIsNotStale"},
	// The three paths that were EXEMPTED as "no gate reads it" until
	// memql#3451 made an exemption's reason checkable against the tree
	// (scripts/ci/gate_corpus_test.go). Each is read by a Go test under
	// scripts/, and each therefore needs its own `gates` glob: `scripts/**`
	// covers the package the test lives in, not the file the test opens.
	//
	// A row here is what stops the glob being deleted again on the "it is only
	// a Dockerfile / only .gitignore" reasoning that put them on the allow
	// list in the first place.
	{".gitignore", "scripts/dev", "TestLockfileNegationsHaveATrackedFileBehindThem"},
	{"Dockerfile", "scripts/ci", "TestDockerfileDeclaresTheSpaStageSelector"},
	{"cmd/deploy-gate-check/Dockerfile", "scripts/ci", "TestDockerfilesCopyEveryNestedModuleManifest"},
}

func TestGatesBucketCoversEveryKnownGateInput(t *testing.T) {
	_, step := nativeStep(t, "gate-inputs")
	for _, in := range gateInputs {
		if _, err := os.Stat(filepath.Join(repoRoot(t), in.path)); err != nil {
			t.Errorf("gate input %s: %v", in.path, err)
		}
		nativeStepCoversPaths(t, step, in.path)
	}
}
func TestGateInputsStepCoversEveryGatePackage(t *testing.T) {
	patterns := nativeSuite(t, "gate-inputs")
	for _, in := range gateInputs {
		info, err := os.Stat(filepath.Join(repoRoot(t), in.pkg))
		if err != nil || !info.IsDir() {
			t.Errorf("gate package %s does not exist", in.pkg)
			continue
		}
		dir := in.pkg
		if dir == "." {
			dir = ""
		}
		covered := false
		for _, pat := range patterns {
			if coveredBy(dir, pat) {
				covered = true
			}
		}
		if !covered {
			t.Errorf("%s is read by %s, but its package %s is absent from %v", in.path, in.gate, in.pkg, patterns)
		}
	}
}
func TestGoChecksRunsOnGateInputOnlyChanges(t *testing.T) {
	_, step := nativeStep(t, "go-checks")
	for _, in := range gateInputs {
		nativeStepCoversPaths(t, step, in.path)
	}
}
