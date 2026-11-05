package pipelinerun

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
)

// carry_test.go -- a push run carries a verified step, or one slice of a sharded
// one, forward from an earlier push's receipt when the change between the two
// commits left it untouched (design record
// docs/superpowers/specs/2026-11-05-owner-pushes-main-design.md, D5; carry.go).

// carryManifest has a step of each kind the decision separates:
//
//   - checks.vet does not declare verification, so it always runs;
//   - checks.whole declares it and selects no packages: it reads the whole tree,
//     so it carries only when the tree did not change at all;
//   - checks.history declares it and reads history beyond its tree, so it never
//     carries;
//   - tests.go is sharded over the import graph, so each slice is decided alone;
//   - tests.docs is gated on the docs bucket, so it carries when no change
//     touched the bucket.
var carryManifest = `formatVersion: 1
name: verify
pipeline:
  platform: linux/amd64
  image: registry/toolchain@sha256:` + strings.Repeat("a", 64) + `
  select:
    go: import-graph
    full: [scripts/runner.sh]
    buckets:
      docs: ["docs/**"]
  stages:
    - name: checks
      steps:
        - name: vet
          placement: cluster
          run: go vet ./...
        - name: whole
          verification: true
          placement: cluster
          run: make whole
        - name: history
          verification: true
          placement: cluster
          checkoutHistory: full
          run: make history
    - name: tests
      steps:
        - name: go
          verification: true
          placement: cluster
          run: go test $MEMQL_PACKAGES
          packages: affected
          shards: 2
        - name: docs
          verification: true
          placement: cluster
          when: { bucket: docs }
          run: make docs
`

// Three more commits of main, after shaA, shaB and shaC.
const (
	carrySHA4 = "4444444444444444444444444444444444444444"
	carrySHA5 = "5555555555555555555555555555555555555555"
	carrySHA6 = "6666666666666666666666666666666666666666"
)

// The packages of carryManifest's tree: the root, and a, b and c, where c
// imports a. Under carryTimings the go step splits into a alone and the rest.
const (
	pkgRoot = "acme.test/shop"
	pkgA    = "acme.test/shop/a"
	pkgB    = "acme.test/shop/b"
	pkgC    = "acme.test/shop/c"
)

const (
	carryVet      = "checks.vet"
	carryWhole    = "checks.whole"
	carryHistory  = "checks.history"
	carryShardOne = "tests.go#1" // [a]
	carryShardTwo = "tests.go#2" // [root b c]
	carryDocs     = "tests.docs"
)

// carryTimings splits tests.go into [a] and [root b c].
var carryTimings = map[string]float64{pkgRoot: 1, pkgA: 10, pkgB: 1, pkgC: 1}

// carryAncestors is an installed policy that opts into carrying forward: it
// declares relation, and admits every reuse it is asked about.
const carryAncestors = `@template
automation pipelineEvidenceReuse {
 args { event string! sourceEvent string! sourceMode string! ageSeconds int! relation string! }
 return true
}`

// newCarryHarness has every commit hold carryManifest and the four packages,
// with every runner answer artifact-free.
func newCarryHarness(t *testing.T, policy string) *driveHarness {
	t.Helper()
	dh := newDriveHarness(t, carryManifest)
	dh.exec.answer = artifactFreeAnswer
	a := workflowSource(t, policy)
	dh.integ.Configure(func(d *Deps) { bindTestWorkflow(d, a) })
	for _, sha := range []string{shaA, shaB, shaC, carrySHA4, carrySHA5, carrySHA6} {
		dh.setTree(sha, carryManifest)
	}
	dh.github.mu.Lock()
	for _, sha := range []string{shaA, shaB, shaC, carrySHA4, carrySHA5, carrySHA6} {
		tree := dh.github.trees[repoName+"@"+sha]
		tree["a/a.go"] = &fstest.MapFile{Data: []byte("package a\n")}
		tree["b/b.go"] = &fstest.MapFile{Data: []byte("package b\n")}
		tree["c/c.go"] = &fstest.MapFile{Data: []byte("package c\n\nimport _ \"acme.test/shop/a\"\n")}
	}
	dh.github.mu.Unlock()
	dh.setTimings(carryTimings)
	return dh
}

// at sets the harness's clock to testNow plus offset: when the next run opens
// and finishes, and the time a source's age is judged at.
func (dh *driveHarness) at(offset time.Duration) {
	now := testNow.Add(offset)
	dh.integ.Configure(func(d *Deps) { d.Now = func() time.Time { return now } })
}

// pushOf is a push of sha to the default branch, which moved it from base.
func pushOf(sha, base string) Opening {
	return Opening{Event: pipelines.EventPush, SHA: sha, BaseSHA: base, Branch: "main", Title: "Land " + shortSHA(sha), Trigger: TriggerWebhook}
}

// carriedReason is what a step carried from source says.
func carriedReason(source Run) string {
	return "Inputs unchanged since " + shortSHA(source.SHA) + "; verified in run " + source.ID + "."
}

// requireCarried holds that key was planned and settled as carried forward from
// the receipt source executed, over a change of exactly changed, and that its
// call names what it stands on.
func (dh *driveHarness) requireCarried(t *testing.T, run, source Run, key string, changed ...string) {
	t.Helper()
	row := dh.stepRowOf(t, run.WorkRunID, key)
	call := argObject(row, "call")
	if row["status"] != WorkStepSkipped || row["errorCode"] != pipelines.CodePassedEarlier || row["errorMessage"] != carriedReason(source) {
		t.Errorf("%s was not carried from %s: %v", key, source.ID, row)
	}
	if call["evidenceRunId"] != source.ID {
		t.Errorf("%s names evidence run %v, want the run that executed it, %s", key, call["evidenceRunId"], source.ID)
	}
	from := argObject(call, "carriedFrom")
	if from["pipelineRunId"] != source.ID || from["workRunId"] != bareID(source.WorkRunID) || from["sha"] != source.SHA || rowString(from, "treeHash") != source.TreeHash {
		t.Errorf("%s records it was carried from %v, want run %s at %s", key, from, source.ID, shortSHA(source.SHA))
	}
	if got := argStrings(from, "changedPaths"); !slices.Equal(got, changed) {
		t.Errorf("%s records the change %v, want %v", key, got, changed)
	}
	executed := dh.stepRowOf(t, source.WorkRunID, key)
	if executed["status"] != WorkStepDone {
		t.Errorf("%s stands on a receipt its source did not execute: %v", key, executed)
	}
	sourceCall := argObject(executed, "call")
	if fp, _ := call["carryFingerprint"].(string); fp == "" || fp != sourceCall["carryFingerprint"] {
		t.Errorf("%s declares carry fingerprint %v, the receipt it stands on %v", key, call["carryFingerprint"], sourceCall["carryFingerprint"])
	}
	if !slices.Equal(argStrings(call, "packages"), argStrings(sourceCall, "packages")) {
		t.Errorf("%s covers %v, the receipt it stands on %v", key, argStrings(call, "packages"), argStrings(sourceCall, "packages"))
	}
}

// requireExecuted holds that key ran in run.
func (dh *driveHarness) requireExecuted(t *testing.T, run Run, key string) {
	t.Helper()
	row := dh.stepRowOf(t, run.WorkRunID, key)
	if row["status"] != WorkStepDone || argObject(row, "call")["carriedFrom"] != nil || argObject(row, "call")["evidenceRunId"] != nil {
		t.Errorf("%s did not run: %v", key, row)
	}
}

// The case the owner asked for: a push runs what its change can have touched and
// carries the rest from the push before it, slice by slice.
func TestAPushCarriesForwardWhatItsChangeLeftUntouched(t *testing.T) {
	dh := newCarryHarness(t, carryAncestors)
	dh.at(0)
	source := dh.driveSource(t, pushOf(shaA, shaBase))
	dh.github.setAncestor(repoName, shaA, shaB, "b/b.go")
	dh.at(time.Hour)
	push, sent := dh.drive(t, pushOf(shaB, shaA))
	if push.Conclusion != ConclusionSuccess {
		t.Fatalf("the push concluded %q", push.Conclusion)
	}
	// b/b.go reaches the slice holding b and nothing that imports it; the whole
	// tree changed, so the whole-tree check runs; history always runs.
	if want := []string{carryHistory, carryVet, carryWhole, carryShardTwo}; !slices.Equal(sent, want) {
		t.Fatalf("the push executed %v, want %v", sent, want)
	}
	dh.requireCarried(t, push, source, carryShardOne, "b/b.go")
	dh.requireCarried(t, push, source, carryDocs, "b/b.go")
	if got := argStrings(argObject(dh.stepRowOf(t, push.WorkRunID, carryShardOne), "call"), "packages"); !slices.Equal(got, []string{pkgA}) {
		t.Fatalf("the carried slice covers %v, want [%s]", got, pkgA)
	}
}

// A change reaches every package that imports what it changed, and a slice with
// any of them runs. A change under the docs bucket runs the step gated on it, and
// a file in no package of its own belongs to the package above it.
func TestAPushRunsEverySliceItsChangeReaches(t *testing.T) {
	for name, tc := range map[string]struct {
		changed      []string
		ran, carried []string
	}{
		// c imports a, so changing a reaches both slices.
		"an imported package": {changed: []string{"a/a.go"}, ran: []string{carryShardOne, carryShardTwo}, carried: []string{carryDocs}},
		// docs/ holds no package, so the root package owns the file.
		"the docs bucket": {changed: []string{"docs/guide.md"}, ran: []string{carryShardTwo, carryDocs}, carried: []string{carryShardOne}},
	} {
		t.Run(name, func(t *testing.T) {
			dh := newCarryHarness(t, carryAncestors)
			dh.at(0)
			source := dh.driveSource(t, pushOf(shaA, shaBase))
			dh.github.setAncestor(repoName, shaA, shaB, tc.changed...)
			dh.at(time.Hour)
			push, _ := dh.drive(t, pushOf(shaB, shaA))
			if push.Conclusion != ConclusionSuccess {
				t.Fatalf("the push concluded %q", push.Conclusion)
			}
			for _, key := range tc.ran {
				dh.requireExecuted(t, push, key)
			}
			for _, key := range tc.carried {
				dh.requireCarried(t, push, source, key, tc.changed...)
			}
		})
	}
}

// When the selection cannot narrow -- a module file, the manifest, a path the
// manifest says selects everything -- nothing carries.
func TestAPushCarriesNothingWhenItsChangeSelectsEverything(t *testing.T) {
	every := sortedCopy([]string{carryHistory, carryVet, carryWhole, carryDocs, carryShardOne, carryShardTwo})
	for _, changed := range []string{"go.mod", pipelines.ManifestPath, "scripts/runner.sh"} {
		t.Run(changed, func(t *testing.T) {
			dh := newCarryHarness(t, carryAncestors)
			dh.at(0)
			dh.driveSource(t, pushOf(shaA, shaBase))
			dh.github.setAncestor(repoName, shaA, shaB, changed, "b/b.go")
			dh.at(time.Hour)
			push, sent := dh.drive(t, pushOf(shaB, shaA))
			if push.Conclusion != ConclusionSuccess {
				t.Fatalf("the push concluded %q", push.Conclusion)
			}
			if !slices.Equal(sent, every) {
				t.Fatalf("the push executed %v, want %v", sent, every)
			}
		})
	}
}

// A commit whose tree did not change carries every check of the tree, the
// whole-tree ones included, and still runs what reads more than its tree.
func TestAPushOfAnUnchangedTreeCarriesEveryCheckOfTheTree(t *testing.T) {
	dh := newCarryHarness(t, carryAncestors)
	dh.github.mu.Lock()
	dh.github.commitTrees[repoName+"@"+shaA] = treeOne
	dh.github.commitTrees[repoName+"@"+shaB] = treeOne
	dh.github.mu.Unlock()
	dh.at(0)
	source := dh.driveSource(t, pushOf(shaA, shaBase))
	dh.github.setAncestor(repoName, shaA, shaB)
	dh.at(time.Hour)
	push, sent := dh.drive(t, pushOf(shaB, shaA))
	if push.Conclusion != ConclusionSuccess {
		t.Fatalf("the push concluded %q", push.Conclusion)
	}
	if want := []string{carryHistory, carryVet}; !slices.Equal(sent, want) {
		t.Fatalf("the push executed %v, want %v", sent, want)
	}
	for _, key := range []string{carryWhole, carryDocs, carryShardOne, carryShardTwo} {
		dh.requireCarried(t, push, source, key)
	}
}

// An empty compare between two trees that differ is not an unchanged tree.
func TestAnEmptyCompareOfTwoTreesCarriesNothing(t *testing.T) {
	dh := newCarryHarness(t, carryAncestors)
	dh.at(0)
	dh.driveSource(t, pushOf(shaA, shaBase))
	dh.github.setAncestor(repoName, shaA, shaB)
	dh.at(time.Hour)
	_, sent := dh.drive(t, pushOf(shaB, shaA))
	if want := []string{carryHistory, carryVet, carryWhole, carryDocs, carryShardOne, carryShardTwo}; !slices.Equal(sent, sortedCopy(want)) {
		t.Fatalf("the push executed %v, want %v", sent, sortedCopy(want))
	}
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// With no proven source a push runs every step, as it did before carry-forward.
// GitHub failing to prove anything never fails the run.
func TestAPushCarriesNothingFromASourceItCannotProve(t *testing.T) {
	every := sortedCopy([]string{carryHistory, carryVet, carryWhole, carryDocs, carryShardOne, carryShardTwo})
	mutateSource := func(change func(*Run)) func(*driveHarness, Run) {
		return func(dh *driveHarness, source Run) {
			dh.store.mu.Lock()
			defer dh.store.mu.Unlock()
			r := dh.store.runs[source.ID]
			change(&r)
			dh.store.runs[source.ID] = r
		}
	}
	setAncestry := func(a githubapp.Ancestry) func(*driveHarness, Run) {
		return func(dh *driveHarness, _ Run) {
			dh.github.mu.Lock()
			defer dh.github.mu.Unlock()
			dh.github.ancestry[repoName+"@"+shaA+"..."+shaB] = a
		}
	}
	for name, tc := range map[string]struct {
		policy string
		after  func(dh *driveHarness, source Run) // after the source ran, before the push
	}{
		"GitHub does not answer": {after: func(dh *driveHarness, _ Run) {
			dh.github.mu.Lock()
			defer dh.github.mu.Unlock()
			delete(dh.github.ancestry, repoName+"@"+shaA+"..."+shaB)
		}},
		"the source is not an ancestor":        {after: setAncestry(githubapp.Ancestry{Files: []string{"b/b.go"}, Complete: true})},
		"GitHub stopped listing the change":    {after: setAncestry(githubapp.Ancestry{Ancestor: true, Files: []string{"b/b.go"}})},
		"the policy does not declare relation": {policy: admitEveryReuse},
		"the policy refuses ancestors": {policy: `@template
automation pipelineEvidenceReuse {
 args { event string! sourceEvent string! sourceMode string! ageSeconds int! relation string! }
 return args.relation != "ancestor"
}`},
		"the engine changed": {after: func(dh *driveHarness, _ Run) {
			dh.integ.Configure(func(d *Deps) { d.EngineRevision = func() string { return "another-engine" } })
		}},
		"the source is older than a day": {after: func(dh *driveHarness, _ Run) { dh.at(25 * time.Hour) }},
		"the source's policy differs": {after: mutateSource(func(r *Run) {
			r.PolicySnapshot = map[string]any{"version": "an older policy"}
		})},
		"the source has no policy":          {after: mutateSource(func(r *Run) { r.PolicySnapshot = nil })},
		"the source failed":                 {after: mutateSource(func(r *Run) { r.Conclusion = ConclusionFailure })},
		"the source was cancelled":          {after: mutateSource(func(r *Run) { r.Conclusion = ConclusionCancelled })},
		"the source was told to stop":       {after: mutateSource(func(r *Run) { r.CancelRequested = true })},
		"the source is unfinished":          {after: mutateSource(func(r *Run) { r.Status = StatusInProgress })},
		"the source ran affected":           {after: mutateSource(func(r *Run) { r.Mode = pipelines.ModeAffected })},
		"the source is someone else's":      {after: mutateSource(func(r *Run) { r.OwnerUserID = "v1:identity:user:stranger" })},
		"the source's repository differs":   {after: mutateSource(func(r *Run) { r.Repository = "acme/other" })},
		"the source is on another branch":   {after: mutateSource(func(r *Run) { r.HeadBranch = "release" })},
		"the source has no work run":        {after: mutateSource(func(r *Run) { r.WorkRunID = "" })},
		"the source finished in the future": {after: mutateSource(func(r *Run) { r.FinishedAt = testNow.Add(2 * time.Hour) })},
	} {
		t.Run(name, func(t *testing.T) {
			policy := tc.policy
			if policy == "" {
				policy = carryAncestors
			}
			dh := newCarryHarness(t, policy)
			dh.at(0)
			source := dh.driveSource(t, pushOf(shaA, shaBase))
			dh.github.setAncestor(repoName, shaA, shaB, "b/b.go")
			dh.at(time.Hour)
			if tc.after != nil {
				tc.after(dh, source)
			}
			push, sent := dh.drive(t, pushOf(shaB, shaA))
			if push.Conclusion != ConclusionSuccess {
				t.Fatalf("the push concluded %q", push.Conclusion)
			}
			if !slices.Equal(sent, every) {
				t.Fatalf("the push executed %v, want %v", sent, every)
			}
		})
	}
}

// Only a directly executed receipt counts. A slice the newest push itself carried
// is supplied by an older push that executed it, judged against that push's own
// change; and once every push that executed it is a day old, the slice runs
// again, so every check runs for real at least once a day while main moves.
func TestACarriedStepIsNeverASource(t *testing.T) {
	dh := newCarryHarness(t, carryAncestors)
	dh.at(0)
	first := dh.driveSource(t, pushOf(shaA, shaBase))

	dh.github.setAncestor(repoName, shaA, shaB, "b/b.go")
	dh.at(time.Hour)
	second := dh.driveSource(t, pushOf(shaB, shaA))
	dh.requireCarried(t, second, first, carryShardOne, "b/b.go")
	dh.requireCarried(t, second, first, carryDocs, "b/b.go")

	// The newest source carried tests.go#1 and docs, and its change reaches its
	// executed slice, so it supplies nothing; the oldest executed both.
	dh.github.setAncestor(repoName, shaB, shaC, "c/c.go")
	dh.github.setAncestor(repoName, shaA, shaC, "b/b.go", "c/c.go")
	dh.at(2 * time.Hour)
	third, sent := dh.drive(t, pushOf(shaC, shaB))
	if third.Conclusion != ConclusionSuccess {
		t.Fatalf("the third push concluded %q", third.Conclusion)
	}
	if want := []string{carryHistory, carryVet, carryWhole, carryShardTwo}; !slices.Equal(sent, want) {
		t.Fatalf("the third push executed %v, want %v", sent, want)
	}
	dh.requireCarried(t, third, first, carryShardOne, "b/b.go", "c/c.go")
	dh.requireCarried(t, third, first, carryDocs, "b/b.go", "c/c.go")

	// A day after the first push, only the third is young enough, and it executed
	// neither: both run again.
	dh.github.setAncestor(repoName, shaC, carrySHA4, "b/b.go")
	dh.github.setAncestor(repoName, shaB, carrySHA4, "b/b.go", "c/c.go")
	dh.github.setAncestor(repoName, shaA, carrySHA4, "b/b.go", "c/c.go")
	dh.at(25*time.Hour + 30*time.Minute)
	fourth, sent := dh.drive(t, pushOf(carrySHA4, shaC))
	if fourth.Conclusion != ConclusionSuccess {
		t.Fatalf("the fourth push concluded %q", fourth.Conclusion)
	}
	if want := []string{carryHistory, carryVet, carryWhole, carryDocs, carryShardOne, carryShardTwo}; !slices.Equal(sent, sortedCopy(want)) {
		t.Fatalf("the fourth push executed %v, want %v", sent, sortedCopy(want))
	}
}

// A shard's slice follows the timing table, which moves, so a push takes the split
// of the run it carries from when that run's shards hold exactly the packages the
// plan selected. Without it, a moved table would mean nothing ever carries.
func TestAPushTakesTheSplitOfTheRunItCarriesFrom(t *testing.T) {
	dh := newCarryHarness(t, carryAncestors)
	dh.at(0)
	source := dh.driveSource(t, pushOf(shaA, shaBase))
	// Now the table splits [b] from [root a c].
	dh.setTimings(map[string]float64{pkgRoot: 1, pkgA: 1, pkgB: 10, pkgC: 1})
	dh.github.setAncestor(repoName, shaA, shaB, "c/c.go")
	dh.at(time.Hour)
	before := len(dh.exec.sent())
	push := dh.driveSource(t, pushOf(shaB, shaA))
	dh.requireCarried(t, push, source, carryShardOne, "c/c.go")
	var shard []string
	for _, req := range dh.exec.sent()[before:] {
		if req.StepKey == carryShardOne {
			t.Fatalf("the carried slice ran")
		}
		if req.StepKey == carryShardTwo {
			shard = req.Step.Packages
		}
	}
	if want := []string{pkgRoot, pkgB, pkgC}; !slices.Equal(shard, want) {
		t.Fatalf("the slice the change reached ran over %v, want the source's %v", shard, want)
	}
}

// Five earlier pushes, and GitHub proves only the oldest: a push asks about four
// sources at most, one compare each, so it never reaches it.
func TestAPushAsksAboutFourSourcesAtMost(t *testing.T) {
	dh := newCarryHarness(t, carryAncestors)
	shas := []string{shaA, shaB, shaC, carrySHA4, carrySHA5}
	base := shaBase
	for i, sha := range shas {
		dh.at(time.Duration(i) * time.Minute)
		if i > 0 {
			// Nothing to carry between these: every step runs.
			dh.github.setAncestor(repoName, base, sha, "go.mod")
		}
		dh.driveSource(t, pushOf(sha, base))
		base = sha
	}
	// Only the oldest is proven an ancestor of the new head.
	dh.github.setAncestor(repoName, shaA, carrySHA6, "b/b.go")
	dh.at(time.Hour)
	_, sent := dh.drive(t, pushOf(carrySHA6, carrySHA5))
	if !slices.Contains(sent, carryShardOne) {
		t.Fatalf("the push carried from a fifth source: %v", sent)
	}

	// Within four, the same source carries.
	dh2 := newCarryHarness(t, carryAncestors)
	base = shaBase
	for i, sha := range shas[:4] {
		dh2.at(time.Duration(i) * time.Minute)
		if i > 0 {
			dh2.github.setAncestor(repoName, base, sha, "go.mod")
		}
		dh2.driveSource(t, pushOf(sha, base))
		base = sha
	}
	first, _ := dh2.store.run(dh2.store.runIDAt(t, shaA))
	dh2.github.setAncestor(repoName, shaA, carrySHA6, "b/b.go")
	dh2.at(time.Hour)
	push, _ := dh2.drive(t, pushOf(carrySHA6, carrySHA4))
	dh2.requireCarried(t, push, first, carryShardOne, "b/b.go")
}

// runIDAt is the id of the one run at sha.
func (s *memStore) runIDAt(t *testing.T, sha string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var found []string
	for id, r := range s.runs {
		if r.SHA == sha {
			found = append(found, id)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d runs at %s", len(found), shortSHA(sha))
	}
	return found[0]
}

// The installed policy is told which relation it is asked about, once it declares
// the argument: the same commit, the same tree, or an ancestor. Each answer is its
// own; the policy also sees the event, the source's event and mode, and its age.
func TestAnEvidencePolicyThatDeclaresRelationIsToldIt(t *testing.T) {
	only := func(clause string) string {
		return `@template
automation pipelineEvidenceReuse {
 args { event string! sourceEvent string! sourceMode string! ageSeconds int! relation string! }
 return ` + clause + `
}`
	}
	t.Run("the same commit", func(t *testing.T) {
		dh := newCarryHarness(t, only(`args.relation == "commit"`))
		dh.at(0)
		dh.driveSource(t, Opening{Event: pipelines.EventMergeGroup, SHA: shaA, Branch: "gh-readonly-queue/main/test", Trigger: TriggerWebhook})
		dh.at(time.Hour)
		_, sent := dh.drive(t, pushOf(shaA, shaBase))
		if want := []string{carryVet}; !slices.Equal(sent, want) {
			t.Fatalf("the push executed %v, want only %v", sent, want)
		}
	})
	t.Run("the same tree", func(t *testing.T) {
		dh := newCarryHarness(t, only(`args.relation == "tree"`))
		dh.github.mu.Lock()
		dh.github.commitTrees[repoName+"@"+shaA] = treeOne
		dh.github.commitTrees[repoName+"@"+shaB] = treeOne
		dh.github.mu.Unlock()
		dh.at(0)
		source := dh.driveSource(t, pushOf(shaA, shaBase))
		dh.at(time.Hour)
		release, sent := dh.drive(t, releaseOpening())
		if want := []string{carryHistory, carryVet}; !slices.Equal(sent, want) {
			t.Fatalf("the release executed %v, want %v", sent, want)
		}
		dh.requireReused(t, release, source, carryWhole)
	})
	t.Run("an ancestor", func(t *testing.T) {
		dh := newCarryHarness(t, only(`args.relation == "ancestor" && args.event == "push" && args.sourceEvent == "push" && args.sourceMode == "full" && args.ageSeconds == 3600`))
		dh.at(0)
		source := dh.driveSource(t, pushOf(shaA, shaBase))
		dh.github.setAncestor(repoName, shaA, shaB, "b/b.go")
		dh.at(time.Hour)
		push := dh.driveSource(t, pushOf(shaB, shaA))
		dh.requireCarried(t, push, source, carryShardOne, "b/b.go")
	})
}

// The opt-in pipelines.md tells an installation to write is a working policy: it
// carries forward from an earlier push, and still answers the same commit as the
// installation did before.
func TestTheDocumentedOptInCarriesForward(t *testing.T) {
	doc, err := os.ReadFile("../../docs/public/operate/pipelines.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(doc), "#### Carrying a verified step forward on the default branch")
	_, block, found := strings.Cut(section, "```memql\n")
	block, _, closed := strings.Cut(block, "\n```")
	if !ok || !found || !closed || !strings.Contains(block, "automation installationEvidenceReuse") {
		t.Fatal("pipelines.md no longer shows the opt-in this test runs")
	}
	// Bound to the role under the name the test harness binds it by.
	policy := strings.Replace(block, "automation installationEvidenceReuse", "automation pipelineEvidenceReuse", 1)

	t.Run("an earlier push", func(t *testing.T) {
		dh := newCarryHarness(t, policy)
		dh.at(0)
		source := dh.driveSource(t, pushOf(shaA, shaBase))
		dh.github.setAncestor(repoName, shaA, shaB, "b/b.go")
		dh.at(time.Hour)
		push := dh.driveSource(t, pushOf(shaB, shaA))
		dh.requireCarried(t, push, source, carryShardOne, "b/b.go")
	})
	t.Run("the same commit", func(t *testing.T) {
		dh := newCarryHarness(t, policy)
		dh.at(0)
		dh.driveSource(t, Opening{Event: pipelines.EventMergeGroup, SHA: shaA, Branch: "gh-readonly-queue/main/test", Trigger: TriggerWebhook})
		dh.at(time.Hour)
		if _, sent := dh.drive(t, pushOf(shaA, shaBase)); !slices.Equal(sent, []string{carryVet}) {
			t.Fatalf("the push executed %v, want only %s", sent, carryVet)
		}
	})
}

// A takeover replays each step's call from its declaration, and the run's
// definition fingerprint covers every call. So what carry-forward declared comes
// back from the rows, never from deciding again: a replica that took the run over
// after GitHub stopped proving the ancestry, with the timing table moved, still
// carries exactly what the run began with.
func TestATakeoverKeepsWhatCarryForwardDeclared(t *testing.T) {
	dh := newCarryHarness(t, carryAncestors)
	dh.at(0)
	source := dh.driveSource(t, pushOf(shaA, shaBase))
	dh.github.setAncestor(repoName, shaA, shaB, "b/b.go")
	dh.at(time.Hour)

	release, vetStarted := make(chan struct{}), make(chan struct{})
	var vetTries atomic.Int64
	dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.StepKey == carryVet {
			if vetTries.Add(1) == 1 {
				close(vetStarted)
			}
			select {
			case <-ctx.Done():
				return pipelines.StepResult{}, ctx.Err()
			case <-release:
			}
		}
		return artifactFreeAnswer(ctx, req)
	}
	run := dh.openRun(t, pushOf(shaB, shaA))
	dh.integ.HandleRunEvent(graphEvent("graph.node.created", run))
	select {
	case <-vetStarted:
	case <-time.After(20 * time.Second):
		t.Fatal("the push never reached its running step")
	}
	stored, _ := dh.store.run(run.ID)
	declared := map[string]map[string]any{}
	for _, key := range []string{carryShardOne, carryDocs} {
		declared[key] = argObject(dh.stepRowOf(t, stored.WorkRunID, key), "call")
		if declared[key]["evidenceRunId"] != source.ID || declared[key]["carriedFrom"] == nil {
			t.Fatalf("the plan did not carry %s: %v", key, declared[key])
		}
	}

	// What a replica deciding again would see: no proven ancestor, and a table
	// that splits the packages the other way.
	dh.github.mu.Lock()
	delete(dh.github.ancestry, repoName+"@"+shaA+"..."+shaB)
	dh.github.mu.Unlock()
	dh.setTimings(map[string]float64{pkgRoot: 1, pkgA: 1, pkgB: 10, pkgC: 1})

	dh.store.mu.Lock()
	r := dh.store.runs[run.ID]
	r.DriverHeartbeatAt = testNow.Add(time.Hour - 3*time.Minute)
	dh.store.runs[run.ID] = r
	dh.store.mu.Unlock()
	other := dh.peer("agent-b")
	a := workflowSource(t, carryAncestors)
	other.Configure(func(d *Deps) {
		bindTestWorkflow(d, a)
		d.Now = func() time.Time { return testNow.Add(time.Hour) }
	})
	if err := other.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitDrives(t, other)
	waitDrives(t, dh.integ)

	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionSuccess {
		t.Fatalf("the push concluded %q on %q", got.Conclusion, got.DriverNodeID)
	}
	for key, began := range declared {
		dh.requireCarried(t, got, source, key, "b/b.go")
		call := argObject(dh.stepRowOf(t, got.WorkRunID, key), "call")
		for _, field := range []string{"definitionFingerprint", "verificationFingerprint", "carryFingerprint", "evidenceRunId", "carriedFrom", "packages"} {
			if !reflect.DeepEqual(call[field], began[field]) {
				t.Errorf("%s: the takeover moved %s: %v, it began as %v", key, field, call[field], began[field])
			}
		}
	}
	for _, req := range dh.exec.sent() {
		if req.RunID == bareID(run.ID) && (req.StepKey == carryShardOne || req.StepKey == carryDocs) {
			t.Errorf("a carried step ran after the takeover: %s", req.StepKey)
		}
	}

	// What a release candidate, or any reader of the journal, recomputes: every
	// declared call, under the run's definition fingerprint.
	workRow := dh.work.run(got.WorkRunID)
	order := argStrings(workRow, "stepOrder")
	decls := make([]workjournal.StepDecl, 0, len(order))
	for _, key := range order {
		row := dh.stepRowOf(t, got.WorkRunID, key)
		decls = append(decls, workjournal.StepDecl{Key: key, Kind: workjournal.KindDeterministic, StepType: "exec", DependsOn: argStrings(row, "dependsOn"), Call: argObject(row, "call")})
	}
	fingerprint, err := workjournal.DefinitionFingerprint(rowString(workRow, "automationName"), decls)
	if err != nil || fingerprint != workRow["templateFingerprint"] {
		t.Fatalf("the takeover moved the declared execution: %s, want %v (%v)", fingerprint, workRow["templateFingerprint"], err)
	}
}

// A failed-only re-run carries what its original executed. What the original
// carried forward, or reused from another run, stands on that run's receipt and
// its age, so carrying it again into a re-run opened later would stretch a pass
// past the day it is good for: it runs again.
func TestAFailedOnlyRerunRunsWhatItsOriginalCarried(t *testing.T) {
	dh := newCarryHarness(t, carryAncestors)
	dh.at(0)
	source := dh.driveSource(t, pushOf(shaA, shaBase))
	dh.github.setAncestor(repoName, shaA, shaB, "b/b.go")
	dh.at(time.Hour)
	var failShard atomic.Bool
	failShard.Store(true)
	dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		result, err := artifactFreeAnswer(ctx, req)
		if req.StepKey == carryShardTwo && failShard.Load() {
			result.Status, result.ExitCode = pipelines.OutcomeFailed, 1
		}
		return result, err
	}
	first, _ := dh.drive(t, pushOf(shaB, shaA))
	if first.Conclusion != ConclusionFailure {
		t.Fatalf("the push concluded %q", first.Conclusion)
	}
	dh.requireCarried(t, first, source, carryShardOne, "b/b.go")

	failShard.Store(false)
	dh.at(49 * time.Hour)
	before := len(dh.exec.sent())
	next, err := dh.integ.Rerun(personCtx(ownerID), first.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	deliver(t, dh.integ, next)
	got, _ := dh.store.run(next.ID)
	if got.Conclusion != ConclusionSuccess {
		t.Fatalf("the re-run concluded %q", got.Conclusion)
	}
	if sent, want := dh.sentSince(before), []string{carryDocs, carryShardOne, carryShardTwo}; !slices.Equal(sent, want) {
		t.Fatalf("the re-run executed %v, want %v", sent, want)
	}
	for _, key := range []string{carryVet, carryWhole, carryHistory} {
		row := dh.stepRowOf(t, got.WorkRunID, key)
		if row["status"] != WorkStepSkipped || row["errorMessage"] != "Passed in attempt 1." {
			t.Errorf("%s, which the original executed, is not carried: %v", key, row)
		}
	}
}

// A release stands only on steps a run executed, so a push that carried any slice
// of a step leaves the release to execute the whole step: the release re-executes
// what the push carried and still reuses what it executed.
func TestAReleaseExecutesAStepThePushOfItsTreeCarried(t *testing.T) {
	dh := newCarryHarness(t, carryAncestors)
	dh.github.mu.Lock()
	dh.github.commitTrees[repoName+"@"+shaB] = treeOne
	dh.github.mu.Unlock()
	dh.at(0)
	source := dh.driveSource(t, pushOf(shaA, shaBase))
	dh.github.setAncestor(repoName, shaA, shaB, "b/b.go")
	dh.at(time.Hour)
	push := dh.driveSource(t, pushOf(shaB, shaA))
	dh.requireCarried(t, push, source, carryShardOne, "b/b.go")

	dh.at(2 * time.Hour)
	release, sent := dh.drive(t, releaseOpening())
	if release.Conclusion != ConclusionSuccess {
		t.Fatalf("the release concluded %q", release.Conclusion)
	}
	if want := []string{carryHistory, carryVet, carryDocs, carryShardOne, carryShardTwo}; !slices.Equal(sent, sortedCopy(want)) {
		t.Fatalf("the release executed %v, want %v", sent, sortedCopy(want))
	}
	dh.requireReused(t, release, push, carryWhole)
}

// A read the decision needs and did not get stops the drive, and recovery tries
// again: carrying nothing is not what the run planned, and neither is guessing.
// A policy that errs is not a policy that said no.
func TestCarryForwardStopsTheDriveOnAReadOrPolicyThatFails(t *testing.T) {
	for name, tc := range map[string]struct {
		policy string
		before func(*driveHarness)
	}{
		"the earlier runs cannot be read": {before: func(dh *driveHarness) {
			dh.store.mu.Lock()
			dh.store.failPreviousRuns = errors.New("the database did not answer")
			dh.store.mu.Unlock()
		}},
		// Asked about nothing until the second push asks about the first.
		"the policy answers no verdict": {policy: `@template
automation pipelineEvidenceReuse {
 args { event string! sourceEvent string! sourceMode string! ageSeconds int! relation string! }
 return "maybe"
}`},
	} {
		t.Run(name, func(t *testing.T) {
			policy := tc.policy
			if policy == "" {
				policy = carryAncestors
			}
			dh := newCarryHarness(t, policy)
			dh.at(0)
			dh.driveSource(t, pushOf(shaA, shaBase))
			dh.github.setAncestor(repoName, shaA, shaB, "b/b.go")
			if tc.before != nil {
				tc.before(dh)
			}
			dh.at(time.Hour)
			push, sent := dh.drive(t, pushOf(shaB, shaA))
			if push.Finished() || push.WorkRunID != "" || len(sent) != 0 {
				t.Fatalf("the drive went on: %s %q, work %q, executed %v", push.Status, push.Conclusion, push.WorkRunID, sent)
			}
		})
	}
}

// The fingerprint says "the same check at any commit of this pipeline": equal
// across commits, trees, events, modes and versions, and across what orders the
// plan, and bound to everything else the check is.
func TestCarryFingerprintIsTheSameCheckAtAnyCommit(t *testing.T) {
	push := Run{SHA: shaA, TreeHash: treeOne, Event: pipelines.EventPush, Mode: pipelines.ModeFull, HeadBranch: "main"}
	want := treeDriver(push).carryFingerprintOf(treeStep())
	if want == "" {
		t.Fatal("a verification step has no carry fingerprint")
	}
	later := Run{SHA: shaB, TreeHash: treeTwo, Event: pipelines.EventPush, Mode: pipelines.ModeFull, HeadBranch: "main", Version: shaB, InboundRequestID: "request"}
	if got := treeDriver(later).carryFingerprintOf(treeStep()); got != want {
		t.Fatalf("a later commit differs: %s, want %s", got, want)
	}
	unread := later
	unread.TreeHash = ""
	if got := treeDriver(unread).carryFingerprintOf(treeStep()); got != want {
		t.Fatal("an unread tree changed the fingerprint: the commit and tree take no part")
	}
	moved := treeStep()
	moved.DependsOn = []string{"secrets.gitleaks"}
	if got := treeDriver(later).carryFingerprintOf(moved); got != want {
		t.Fatal("what orders the plan changed the fingerprint")
	}
	skipped := treeStep()
	skipped.Skip = &pipelines.Skip{Code: pipelines.CodePassedEarlier, Reason: "Passed."}
	if got := treeDriver(later).carryFingerprintOf(skipped); got != want {
		t.Fatal("a planned skip changed the fingerprint")
	}
	if tree := treeDriver(push).treeFingerprintOf(treeStep()); tree == want {
		t.Fatal("the carry fingerprint is the tree fingerprint: two identities that must never compare equal")
	}

	for name, change := range map[string]func(*runDriver, *pipelines.Step){
		"engine":   func(dr *runDriver, _ *pipelines.Step) { dr.d.EngineRevision = func() string { return "other" } },
		"pipeline": func(dr *runDriver, _ *pipelines.Step) { dr.p.ID = "other" },
		"owner":    func(dr *runDriver, _ *pipelines.Step) { dr.p.OwnerUserID = "other" },
		"source":   func(dr *runDriver, _ *pipelines.Step) { dr.p.Repository = "acme/other" },
		"compute":  func(dr *runDriver, _ *pipelines.Step) { dr.p.Compute = pipelines.Compute("other") },
		"domain":   func(dr *runDriver, _ *pipelines.Step) { dr.d.Domain = func() string { return "other.test" } },
		"workflow": func(dr *runDriver, _ *pipelines.Step) { dr.workflow = &workflowProgram{fingerprint: "other"} },
		"command":  func(_ *runDriver, s *pipelines.Step) { s.Run = "go test ./other" },
		"image":    func(_ *runDriver, s *pipelines.Step) { s.Image = "registry/other@sha256:" + strings.Repeat("b", 64) },
		"packages": func(_ *runDriver, s *pipelines.Step) { s.Packages = []string{"b"} },
		"shard":    func(_ *runDriver, s *pipelines.Step) { s.Shard = pipelines.ShardRef{Index: 1, Count: 2} },
		"services": func(_ *runDriver, s *pipelines.Step) {
			s.Services = map[string]pipelines.Service{"postgres": {Image: "registry/pg@sha256:" + strings.Repeat("c", 64)}}
		},
	} {
		t.Run("binds "+name, func(t *testing.T) {
			dr, step := treeDriver(later), treeStep()
			change(dr, &step)
			if got := dr.carryFingerprintOf(step); got == "" || got == want {
				t.Fatalf("a different %s kept the fingerprint: %q", name, got)
			}
		})
	}
	for name, change := range map[string]func(*runDriver, *pipelines.Step){
		"not declared":   func(_ *runDriver, s *pipelines.Step) { s.Verification = false },
		"not a command":  func(_ *runDriver, s *pipelines.Step) { s.Kind = pipelines.StepNotify },
		"needs history":  func(_ *runDriver, s *pipelines.Step) { s.CheckoutHistory = "full" },
		"unknown engine": func(dr *runDriver, _ *pipelines.Step) { dr.d.EngineRevision = func() string { return "" } },
		"dirty engine":   func(dr *runDriver, _ *pipelines.Step) { dr.d.EngineRevision = func() string { return "abc-dirty" } },
	} {
		t.Run("none when "+name, func(t *testing.T) {
			dr, step := treeDriver(later), treeStep()
			change(dr, &step)
			if got := dr.carryFingerprintOf(step); got != "" {
				t.Fatalf("got fingerprint %q", got)
			}
		})
	}
}
