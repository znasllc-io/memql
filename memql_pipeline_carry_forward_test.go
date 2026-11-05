package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

// A push to main carries a verification step's slice forward from an earlier push
// when the import graph says the change between them left the slice untouched
// (component/pipelinerun/carry.go; design record
// docs/superpowers/specs/2026-11-05-owner-pushes-main-design.md, D5). The graph sees
// Go source and nothing a step's command runs or reads beside `go test`, so this
// repository bounds the rest in its manifest, and these tests hold the bounds: a
// manifest edit that loosened one would otherwise turn a broken step runner into a
// carried pass, silently.

// Every repository file a step that can carry across a change names in its command
// is under select.full, and so is what those files read in turn. A change to any
// of them then selects every package, and nothing carries over it.
//
// Only a step that declares verification carries, and over a changed tree only one
// that narrows: by packages or by a bucket. Every other step is checked, at most,
// on an unchanged tree, where nothing it runs can have changed.
func TestEngineCarriedStepRunnersSelectEveryPackage(t *testing.T) {
	spec := engineManifest(t).Pipeline
	_, tracked := engineTrackedGoTree(t)
	if spec.Select == nil {
		t.Fatal("the pipeline declares no select block")
	}
	inFull, err := pipelines.PathSet(spec.Select.Full)
	if err != nil {
		t.Fatal(err)
	}
	var narrowing []string
	for _, stage := range spec.Stages {
		for _, step := range stage.Steps {
			if !step.Verification || step.CheckoutHistory != "" || (step.Packages == "" && step.When == nil) {
				continue
			}
			narrowing = append(narrowing, stage.Name+"."+step.Name)
			for _, field := range strings.Fields(step.Run) {
				path := strings.Trim(field, `"';`)
				if !tracked[path] {
					continue
				}
				if !inFull(path) {
					t.Errorf("%s.%s runs %s, which the import graph cannot see; add it to select.full, or a push carries the step forward over a change to it",
						stage.Name, step.Name, path)
				}
			}
		}
	}
	slices.Sort(narrowing)
	if want := []string{"tests.db-tests", "tests.go-tests"}; !slices.Equal(narrowing, want) {
		t.Errorf("the steps that can carry across a change are %v, want %v: check what each one's command runs and reads, then update this list", narrowing, want)
	}
	// What those runners read in turn: build-css.sh sources lib.sh and compiles
	// brand/ into component/identity/web's embedded stylesheet; the db-tests runner
	// reads its per-package budgets.
	for _, path := range []string{
		"scripts/identity/build-css.sh", "scripts/identity/lib.sh", "scripts/ci/pipeline-db-tests.py", "scripts/ci/db-test-budgets.yaml",
		"brand/tokens.css", "brand/theme.css", "brand/fonts.css", "brand/favicon.svg",
	} {
		if !tracked[path] {
			t.Errorf("%s is not a tracked file, so this list no longer says what it means", path)
		}
		if !inFull(path) {
			t.Errorf("%s is read by a step that can carry across a change, and is not under select.full", path)
		}
	}
}

// The repository-reading gates run on every push. A carried go-tests slice may hold
// one of their packages, whose tests read docs, scripts and `git ls-files`, so the
// gates must execute on every commit of main whatever was carried. gate-inputs
// declares no verification, so it is never carried itself.
func TestEngineGatesRunOnEveryPush(t *testing.T) {
	spec := engineManifest(t).Pipeline
	for _, stage := range spec.Stages {
		if stage.Name != manifestStageGates {
			continue
		}
		if !slices.Contains(stage.On, string(pipelines.EventPush)) {
			t.Errorf("the %s stage runs on %v; a push must run it", stage.Name, stage.On)
		}
		for _, step := range stage.Steps {
			if step.Verification {
				t.Errorf("%s.%s declares verification, so a push could carry the gate itself forward", stage.Name, step.Name)
			}
		}
		return
	}
	t.Fatalf("the pipeline has no %s stage", manifestStageGates)
}
