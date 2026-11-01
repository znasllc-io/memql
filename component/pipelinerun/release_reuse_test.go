package pipelinerun

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
)

// release_reuse_test.go -- a release reuses the checks of the run that tested its
// tree, and never anything that would stop it becoming a candidate
// (docs/superpowers/specs/2026-10-29-release-same-tree-reuse-design.md).

// sameTreeManifest has one step of each kind the decision separates:
//
//   - checks.unit and tests.integration declare verification, so they are reused;
//   - checks.vet does not, so it always runs;
//   - scans.advisories and release-only.image declare artifacts, so they always
//     run, the first as a vulnerability scan does and the second as a release
//     build does;
//   - pull-only sits right before the tests on a pull request and not on a
//     release, so tests.integration's dependsOn differs between the two plans.
var sameTreeManifest = `formatVersion: 1
name: verify
pipeline:
  platform: linux/amd64
  image: registry/toolchain@sha256:` + strings.Repeat("a", 64) + `
  stages:
    - name: checks
      steps:
        - name: unit
          verification: true
          placement: cluster
          run: go test ./unit
        - name: vet
          placement: cluster
          run: go vet ./...
    - name: scans
      steps:
        - name: advisories
          placement: cluster
          artifacts: [report]
          run: scan
    - name: pull-only
      on: [pull_request]
      steps:
        - name: gate
          placement: cluster
          run: gate
    - name: tests
      steps:
        - name: integration
          verification: true
          placement: cluster
          run: go test ./integration
    - name: release-only
      on: [release]
      steps:
        - name: image
          placement: cluster
          artifacts: [dist]
          run: build
`

// admitEveryReuse is an installed policy that admits every reuse it is asked about.
const admitEveryReuse = `@template
automation pipelineEvidenceReuse {
 args { event string! sourceEvent string! sourceMode string! ageSeconds int! }
 return true
}`

const (
	releasedUnit        = "checks.unit"
	releasedVet         = "checks.vet"
	releasedAdvisories  = "scans.advisories"
	releasedIntegration = "tests.integration"
	releasedImage       = "release-only.image"
)

// artifactFreeAnswer reports artifact files only for a step that declares them,
// as a real runner does.
func artifactFreeAnswer(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
	result := passed(req)
	if len(req.Step.Artifacts) == 0 {
		result.ArtifactFileIDs = nil
	}
	return result, nil
}

// newSameTreeHarness has shaA and shaB hold one tree under policy, with every
// runner answer artifact-free unless the step declares artifacts.
func newSameTreeHarness(t *testing.T, manifest, policy string) *driveHarness {
	t.Helper()
	dh := newDriveHarness(t, manifest)
	dh.exec.answer = artifactFreeAnswer
	a := workflowSource(t, policy)
	dh.integ.Configure(func(d *Deps) { bindTestWorkflow(d, a) })
	dh.setTree(shaB, manifest)
	dh.github.commitTrees[repoName+"@"+shaA] = treeOne
	dh.github.commitTrees[repoName+"@"+shaB] = treeOne
	return dh
}

func releaseOpening() Opening {
	return Opening{Event: pipelines.EventRelease, Mode: pipelines.ModeFull, SHA: shaB, ReleaseTag: "v1.4.0", Trigger: TriggerManual}
}

// drive opens and delivers o and answers the run as it ended, with the keys its
// steps sent to the runner, sorted.
func (dh *driveHarness) drive(t *testing.T, o Opening) (Run, []string) {
	t.Helper()
	before := len(dh.exec.sent())
	run := dh.openRun(t, o)
	deliver(t, dh.integ, run)
	got, _ := dh.store.run(run.ID)
	return got, dh.sentSince(before)
}

func (dh *driveHarness) sentSince(before int) []string {
	var keys []string
	for _, req := range dh.exec.sent()[before:] {
		keys = append(keys, req.StepKey)
	}
	slices.Sort(keys)
	return keys
}

// driveSource runs o and requires it to succeed.
func (dh *driveHarness) driveSource(t *testing.T, o Opening) Run {
	t.Helper()
	source, _ := dh.drive(t, o)
	if source.Conclusion != ConclusionSuccess {
		t.Fatalf("the source did not succeed: %+v", source)
	}
	return source
}

// stepRowOf is the merged row of key in one work run.
func (dh *driveHarness) stepRowOf(t *testing.T, workRunID, key string) map[string]any {
	t.Helper()
	for _, row := range dh.work.stepRows(bareID(workRunID)) {
		if row["key"] == key {
			return row
		}
	}
	t.Fatalf("work run %s has no step %s", workRunID, key)
	return nil
}

// requireReused holds that key was skipped on source's receipt and says so.
func (dh *driveHarness) requireReused(t *testing.T, release, source Run, key string) {
	t.Helper()
	row := dh.stepRowOf(t, release.WorkRunID, key)
	call := argObject(row, "call")
	if row["status"] != WorkStepSkipped || row["errorCode"] != pipelines.CodePassedEarlier {
		t.Errorf("%s was not skipped as passed earlier: %v", key, row)
	}
	if call["evidenceRunId"] != source.ID {
		t.Errorf("%s names evidence run %v, want %s", key, call["evidenceRunId"], source.ID)
	}
	if reason, _ := row["errorMessage"].(string); !strings.Contains(reason, source.ID) {
		t.Errorf("%s does not say which run it stands on: %q", key, reason)
	}
	fromSource := argObject(dh.stepRowOf(t, source.WorkRunID, key), "call")["treeFingerprint"]
	if fromSource == nil || fromSource == "" || call["treeFingerprint"] != fromSource {
		t.Errorf("%s fingerprint %v, the source's %v", key, call["treeFingerprint"], fromSource)
	}
}

// The case the owner asked for, and the sources like it: a release skips what the
// run that tested its tree executed, whatever commit that run had, and still runs
// everything that is not a pure check of the tree.
func TestReleaseReusesTheRunThatTestedItsTree(t *testing.T) {
	for name, source := range map[string]Opening{
		// Different commits with one tree: the up-to-date pull request's head and
		// the merge commit that landed it.
		"its pull request": prOpening(),
		"a merge group":    {Event: pipelines.EventMergeGroup, SHA: shaA, Branch: "gh-readonly-queue/main/test", Trigger: TriggerWebhook},
		"another push":     pushOpening(),
		// The same commit: a tree comparison needs no separate rule for it.
		"the push of its commit": {Event: pipelines.EventPush, SHA: shaB, BaseSHA: shaBase, Branch: "main", Title: "Land", Trigger: TriggerWebhook},
	} {
		t.Run(name, func(t *testing.T) {
			dh := newSameTreeHarness(t, sameTreeManifest, admitEveryReuse)
			from := dh.driveSource(t, source)
			release, sent := dh.drive(t, releaseOpening())
			if release.Conclusion != ConclusionSuccess {
				t.Fatalf("the release did not succeed: %+v", release)
			}
			if want := []string{releasedVet, releasedImage, releasedAdvisories}; !slices.Equal(sent, want) {
				t.Fatalf("the release executed %v, want %v", sent, want)
			}
			dh.requireReused(t, release, from, releasedUnit)
			dh.requireReused(t, release, from, releasedIntegration)
		})
	}
}

// A vulnerability scan is never reused. It declares its report as an artifact,
// and it branches on the event: a release records findings and a pull request
// fails on them (#6381). The rule is what the step declares, so a scan that
// passed on the same tree an hour ago still runs, and leaves its own report.
func TestReleaseRunsItsOwnVulnerabilityScans(t *testing.T) {
	dh := newSameTreeHarness(t, sameTreeManifest, admitEveryReuse)
	dh.driveSource(t, prOpening())
	release, sent := dh.drive(t, releaseOpening())
	if !slices.Contains(sent, releasedAdvisories) {
		t.Fatalf("the release did not scan: %v", sent)
	}
	row := dh.stepRowOf(t, release.WorkRunID, releasedAdvisories)
	if row["status"] != WorkStepDone || argObject(row, "call")["treeFingerprint"] != nil {
		t.Fatalf("a scan offers reuse: %v", row)
	}
}

// With no proven source the release runs every step, as it does today.
func TestReleaseExecutesEveryStepWithoutAProvenSource(t *testing.T) {
	every := []string{releasedUnit, releasedVet, releasedImage, releasedAdvisories, releasedIntegration}
	mutateSource := func(change func(*Run)) func(*driveHarness, Run) {
		return func(dh *driveHarness, source Run) {
			dh.store.mu.Lock()
			defer dh.store.mu.Unlock()
			r := dh.store.runs[source.ID]
			change(&r)
			dh.store.runs[source.ID] = r
		}
	}
	for name, tc := range map[string]struct {
		policy string
		before func(*driveHarness)                // before the source runs
		after  func(dh *driveHarness, source Run) // after it, before the release
	}{
		"another tree": {after: func(dh *driveHarness, _ Run) {
			dh.github.mu.Lock()
			defer dh.github.mu.Unlock()
			dh.github.commitTrees[repoName+"@"+shaB] = treeTwo
		}},
		"the release's tree is unread": {after: func(dh *driveHarness, _ Run) {
			dh.github.mu.Lock()
			defer dh.github.mu.Unlock()
			dh.github.commitTreeErr = context.DeadlineExceeded
		}},
		"the source's tree is unread": {
			before: func(dh *driveHarness) { dh.github.commitTreeErr = context.DeadlineExceeded },
			after: func(dh *driveHarness, _ Run) {
				dh.github.mu.Lock()
				defer dh.github.mu.Unlock()
				dh.github.commitTreeErr = nil
			},
		},
		"the policy refuses": {policy: `@template
automation pipelineEvidenceReuse {
 args { event string! sourceEvent string! sourceMode string! ageSeconds int! }
 return false
}`},
		"the engine changed": {after: func(dh *driveHarness, _ Run) {
			dh.integ.Configure(func(d *Deps) { d.EngineRevision = func() string { return "another-engine" } })
		}},
		"the source is older than a day": {after: func(dh *driveHarness, source Run) {
			dh.integ.Configure(func(d *Deps) { d.Now = func() time.Time { return source.FinishedAt.Add(25 * time.Hour) } })
		}},
		"the source's policy differs": {after: mutateSource(func(r *Run) {
			r.PolicySnapshot = map[string]any{"version": "an older policy"}
		})},
		"the source has no policy":        {after: mutateSource(func(r *Run) { r.PolicySnapshot = nil })},
		"the source failed":               {after: mutateSource(func(r *Run) { r.Conclusion = ConclusionFailure })},
		"the source was cancelled":        {after: mutateSource(func(r *Run) { r.Conclusion = ConclusionCancelled })},
		"the source was told to stop":     {after: mutateSource(func(r *Run) { r.CancelRequested = true })},
		"the source is unfinished":        {after: mutateSource(func(r *Run) { r.Status = StatusInProgress })},
		"the source is a scheduled scan":  {after: mutateSource(func(r *Run) { r.Event = pipelines.EventSchedule })},
		"the source is someone else's":    {after: mutateSource(func(r *Run) { r.OwnerUserID = "v1:identity:user:stranger" })},
		"the source's repository differs": {after: mutateSource(func(r *Run) { r.Repository = "acme/other" })},
		"the source has no work run":      {after: mutateSource(func(r *Run) { r.WorkRunID = "" })},
		// These two read as a success on the run row and not in the journal.
		// Candidate preparation inspects the journal, so a skip planned from the
		// row alone is the reuse that costs a release its version.
		"the source's work run did not succeed": {after: func(dh *driveHarness, source Run) {
			dh.work.mu.Lock()
			defer dh.work.mu.Unlock()
			dh.work.runs[bareID(source.WorkRunID)]["status"] = "failed"
		}},
		"the source's work input names no tree": {after: func(dh *driveHarness, source Run) {
			dh.work.mu.Lock()
			defer dh.work.mu.Unlock()
			delete(dh.work.runs[bareID(source.WorkRunID)]["input"].(map[string]any), "treeHash")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			policy := tc.policy
			if policy == "" {
				policy = admitEveryReuse
			}
			dh := newSameTreeHarness(t, sameTreeManifest, policy)
			if tc.before != nil {
				tc.before(dh)
			}
			source := dh.driveSource(t, prOpening())
			if tc.after != nil {
				tc.after(dh, source)
			}
			release, sent := dh.drive(t, releaseOpening())
			if release.Conclusion != ConclusionSuccess {
				t.Fatalf("the release did not succeed: %+v", release)
			}
			if !slices.Equal(sent, every) {
				t.Fatalf("the release executed %v, want %v", sent, every)
			}
		})
	}
}

// A source row is judged by what it reported too. An artifact recorded against a
// step, in files or intents, stands in no release, whatever the step declares.
func TestReleaseDoesNotReuseAStepThatReportedAnArtifact(t *testing.T) {
	for name, report := range map[string]func(*pipelines.StepResult){
		"files":   func(r *pipelines.StepResult) { r.ArtifactFileIDs = []string{"file-1"} },
		"intents": func(r *pipelines.StepResult) { r.ArtifactIntentIDs = []string{strings.Repeat("c", 64)} },
	} {
		t.Run(name, func(t *testing.T) {
			dh := newSameTreeHarness(t, sameTreeManifest, admitEveryReuse)
			dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
				result, err := artifactFreeAnswer(ctx, req)
				if req.StepKey == releasedUnit {
					report(&result)
				}
				return result, err
			}
			source := dh.driveSource(t, prOpening())
			dh.exec.answer = artifactFreeAnswer
			release, sent := dh.drive(t, releaseOpening())
			if !slices.Contains(sent, releasedUnit) {
				t.Fatalf("a step that reported an artifact was reused: %v", sent)
			}
			dh.requireReused(t, release, source, releasedIntegration)
		})
	}
}

// Only a directly executed receipt counts. A pull request that passed across a
// failed-only re-run offers what its last attempt executed; what that attempt
// carried stands on an attempt that failed, and a chain of skips must not renew
// its own freshness.
func TestReleaseDoesNotStandOnACarriedPass(t *testing.T) {
	dh := newSameTreeHarness(t, sameTreeManifest, admitEveryReuse)
	var failIntegration atomic.Bool
	failIntegration.Store(true)
	dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		result, err := artifactFreeAnswer(ctx, req)
		if req.StepKey == releasedIntegration && failIntegration.Load() {
			result.Status, result.ExitCode = pipelines.OutcomeFailed, 1
		}
		return result, err
	}
	first, _ := dh.drive(t, prOpening())
	if first.Conclusion != ConclusionFailure {
		t.Fatalf("the first attempt concluded %q", first.Conclusion)
	}
	failIntegration.Store(false)
	second, err := dh.integ.Rerun(personCtx(ownerID), first.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	deliver(t, dh.integ, second)
	source, _ := dh.store.run(second.ID)
	if source.Conclusion != ConclusionSuccess {
		t.Fatalf("the re-run concluded %q", source.Conclusion)
	}

	release, sent := dh.drive(t, releaseOpening())
	if !slices.Contains(sent, releasedUnit) {
		t.Fatalf("the release stood on a carried pass: %v", sent)
	}
	dh.requireReused(t, release, source, releasedIntegration)
}

// The policy still decides, one source at a time, and it is asked with the same
// arguments every other event uses. A pull request runs affected, and that is the
// policy's to weigh: the fingerprint already binds what the step executed.
func TestReleaseAsksThePolicyAboutEachSource(t *testing.T) {
	t.Run("it may refuse the newest and admit an older one", func(t *testing.T) {
		dh := newSameTreeHarness(t, sameTreeManifest, `@template
automation pipelineEvidenceReuse {
 args { event string! sourceEvent string! sourceMode string! ageSeconds int! }
 return args.sourceEvent == "push"
}`)
		dh.setTree(shaC, sameTreeManifest)
		dh.github.commitTrees[repoName+"@"+shaC] = treeOne
		push := dh.driveSource(t, pushOpening())
		pull := prOpening()
		pull.SHA = shaC
		dh.driveSource(t, pull)
		release, _ := dh.drive(t, releaseOpening())
		dh.requireReused(t, release, push, releasedUnit)
	})
	t.Run("it sees the event, the source's event and mode, and the age", func(t *testing.T) {
		dh := newSameTreeHarness(t, sameTreeManifest, `@template
automation pipelineEvidenceReuse {
 args { event string! sourceEvent string! sourceMode string! ageSeconds int! }
 return args.event == "release" && args.sourceEvent == "pull_request" && args.sourceMode == "affected" && args.ageSeconds >= 0
}`)
		source := dh.driveSource(t, prOpening())
		release, _ := dh.drive(t, releaseOpening())
		dh.requireReused(t, release, source, releasedUnit)
	})
}

// An explicit retry executes: a whole re-run of a release is someone asking for
// the checks to run again, and it reuses nothing.
func TestAWholeReleaseRerunExecutesEveryStep(t *testing.T) {
	dh := newSameTreeHarness(t, sameTreeManifest, admitEveryReuse)
	dh.driveSource(t, prOpening())
	release, _ := dh.drive(t, releaseOpening())
	before := len(dh.exec.sent())
	next, err := dh.integ.Rerun(personCtx(ownerID), release.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	deliver(t, dh.integ, next)
	want := []string{releasedUnit, releasedVet, releasedImage, releasedAdvisories, releasedIntegration}
	if got := dh.sentSince(before); !slices.Equal(got, want) {
		t.Fatalf("the whole re-run executed %v, want %v", got, want)
	}
}

// Reuse across commits is a release's alone. Every other event still compares
// commits, so a push does not stand on a pull request with its tree.
func TestOtherEventsStillCompareCommits(t *testing.T) {
	dh := newSameTreeHarness(t, sameTreeManifest, admitEveryReuse)
	dh.driveSource(t, prOpening())
	push := pushOpening()
	push.SHA = shaB
	got, sent := dh.drive(t, push)
	if got.Conclusion != ConclusionSuccess || !slices.Contains(sent, releasedUnit) {
		t.Fatalf("a push reused across commits: %s, %v", got.Conclusion, sent)
	}
}

// A failed-only re-run of a release keeps #6369's rule for what it executes: what
// failed, what was blocked and every artifact producer. What the first attempt
// reused it carries, naming the attempt's own skip, and the proof walks back to the
// source (integrations/release/candidate_reused.go).
func TestAFailedOnlyReleaseRerunCarriesWhatItReused(t *testing.T) {
	dh := newSameTreeHarness(t, sameTreeManifest, admitEveryReuse)
	source := dh.driveSource(t, prOpening())
	var failVet atomic.Bool
	failVet.Store(true)
	dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		result, err := artifactFreeAnswer(ctx, req)
		if req.StepKey == releasedVet && failVet.Load() {
			result.Status, result.ExitCode = pipelines.OutcomeFailed, 1
		}
		return result, err
	}
	first, _ := dh.drive(t, releaseOpening())
	if first.Conclusion != ConclusionFailure {
		t.Fatalf("the first attempt concluded %q", first.Conclusion)
	}
	failVet.Store(false)
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
	if sent, want := dh.sentSince(before), []string{releasedVet, releasedImage, releasedAdvisories}; !slices.Equal(sent, want) {
		t.Fatalf("the re-run executed %v, want %v", sent, want)
	}
	for _, key := range []string{releasedUnit, releasedIntegration} {
		row := dh.stepRowOf(t, got.WorkRunID, key)
		if row["status"] != WorkStepSkipped || row["errorCode"] != pipelines.CodePassedEarlier {
			t.Errorf("%s is not carried: %v", key, row)
		}
		// The attempt that reused it names the source; this one only carries it.
		if argObject(row, "call")["evidenceRunId"] != nil {
			t.Errorf("%s: a carry claims to have reused: %v", key, row)
		}
		if reason, _ := row["errorMessage"].(string); !strings.Contains(reason, source.ID) {
			t.Errorf("%s lost the run it stands on: %q", key, reason)
		}
	}
}

// shardedManifest is one verification step over the repository's packages, split
// over two shards by the timing table, between a stage before it and one after it.
// A step that fails in the last stage fails a release that has already executed or
// reused the shards; one that fails in the first blocks them.
var shardedManifest = `formatVersion: 1
name: verify
pipeline:
  platform: linux/amd64
  image: registry/toolchain@sha256:` + strings.Repeat("a", 64) + `
  select:
    go: import-graph
  stages:
    - name: checks
      steps:
        - name: vet
          placement: cluster
          run: go vet ./...
    - name: tests
      steps:
        - name: go
          verification: true
          placement: cluster
          run: go test $MEMQL_PACKAGES
          packages: affected
          shards: 2
    - name: final
      steps:
        - name: gate
          placement: cluster
          run: gate
`

// newShardedHarness has shaA and shaB hold one tree of three packages.
func newShardedHarness(t *testing.T) *driveHarness {
	t.Helper()
	dh := newSameTreeHarness(t, shardedManifest, admitEveryReuse)
	dh.github.mu.Lock()
	for _, sha := range []string{shaA, shaB} {
		tree := dh.github.trees[repoName+"@"+sha]
		for _, pkg := range []string{"a", "b", "c"} {
			tree[pkg+"/"+pkg+".go"] = &fstest.MapFile{Data: []byte("package " + pkg + "\n")}
		}
	}
	dh.github.mu.Unlock()
	dh.setTimings(map[string]float64{"acme.test/shop/a": 10, "acme.test/shop/b": 1, "acme.test/shop/c": 1})
	return dh
}

func (dh *driveHarness) setTimings(timings map[string]float64) {
	p, _ := dh.store.pipeline(dh.p.ID)
	p.Timings = timings
	dh.store.addPipeline(p)
}

// moveTimings rewrites the table as any successful full run does, so a plan
// compiled now splits the packages the other way.
func (dh *driveHarness) moveTimings() {
	dh.setTimings(map[string]float64{"acme.test/shop/a": 1, "acme.test/shop/b": 10, "acme.test/shop/c": 1})
}

// A shard's slice comes from the timing table, which every successful full run
// moves, so requiring equal slices would mean go-tests and db-tests are almost
// never reused. A step is judged whole: when the source's shards hold exactly the
// packages the plan selected, the release takes the source's slices.
func TestReleaseReusesAShardedStepWholeWhenItsSlicesMoved(t *testing.T) {
	dh := newShardedHarness(t)
	source := dh.driveSource(t, pushOpening())
	dh.moveTimings()
	release, sent := dh.drive(t, releaseOpening())
	if release.Conclusion != ConclusionSuccess {
		t.Fatalf("the release concluded %q", release.Conclusion)
	}
	if want := []string{"checks.vet", "final.gate"}; !slices.Equal(sent, want) {
		t.Fatalf("the release executed %v, want %v", sent, want)
	}
	for _, key := range []string{"tests.go#1", "tests.go#2"} {
		dh.requireReused(t, release, source, key)
		held := argStrings(argObject(dh.stepRowOf(t, source.WorkRunID, key), "call"), "packages")
		if got := argStrings(argObject(dh.stepRowOf(t, release.WorkRunID, key), "call"), "packages"); !slices.Equal(got, held) {
			t.Errorf("%s names %v, the receipt it stands on covers %v", key, got, held)
		}
	}
}

// Fewer packages are not the same check: an affected pull request that reached two
// packages says nothing about the third. Its two shards share the plan's keys and
// its fingerprints match under its own slices, so the union is all that tells it
// apart from a full run, and a false reuse here would release an untested package.
func TestReleaseDoesNotReuseAStepThatCoveredOtherPackages(t *testing.T) {
	dh := newShardedHarness(t)
	dh.github.mu.Lock()
	dh.github.compare[repoName+"@"+shaBase+"..."+shaA] = compareAnswer{Files: []string{"a/a.go", "b/b.go"}, Complete: true}
	dh.github.mu.Unlock()
	source := dh.driveSource(t, prOpening())
	if rows := dh.work.stepRows(bareID(source.WorkRunID)); !slices.ContainsFunc(rows, func(row map[string]any) bool { return row["key"] == "tests.go#2" }) {
		t.Fatalf("the pull request did not split over the plan's keys: %v", rows)
	}
	_, sent := dh.drive(t, releaseOpening())
	if want := []string{"checks.vet", "final.gate", "tests.go#1", "tests.go#2"}; !slices.Equal(sent, want) {
		t.Fatalf("the release executed %v, want %v", sent, want)
	}
}

// Every shard or none. Reusing half a step and running the rest under a new split
// could leave a package no receipt covers.
func TestReleaseReusesAShardedStepAllOrNothing(t *testing.T) {
	dh := newShardedHarness(t)
	dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		result, err := artifactFreeAnswer(ctx, req)
		if req.StepKey == "tests.go#2" {
			result.ArtifactFileIDs = []string{"file-1"}
		}
		return result, err
	}
	dh.driveSource(t, pushOpening())
	dh.exec.answer = artifactFreeAnswer
	dh.moveTimings()
	_, sent := dh.drive(t, releaseOpening())
	if want := []string{"checks.vet", "final.gate", "tests.go#1", "tests.go#2"}; !slices.Equal(sent, want) {
		t.Fatalf("the release executed %v, want %v", sent, want)
	}
}

// A failed-only re-run of a release takes the attempt's slices. Otherwise moved
// timings would make every shard run again, in a release whose shards passed or
// were reused, and the re-run would cost what #6369 exists to save.
func TestAFailedOnlyReleaseRerunKeepsItsSlicesWhenTheTableMoved(t *testing.T) {
	for name, reused := range map[string]bool{"shards it reused": true, "shards it executed": false} {
		t.Run(name, func(t *testing.T) {
			dh := newShardedHarness(t)
			if reused {
				dh.driveSource(t, pushOpening())
				dh.moveTimings()
			}
			var failGate atomic.Bool
			failGate.Store(true)
			dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
				result, err := artifactFreeAnswer(ctx, req)
				// In the last stage, so the shards have settled by then.
				if req.StepKey == "final.gate" && failGate.Load() {
					result.Status, result.ExitCode = pipelines.OutcomeFailed, 1
				}
				return result, err
			}
			first, _ := dh.drive(t, releaseOpening())
			if first.Conclusion != ConclusionFailure {
				t.Fatalf("the first attempt concluded %q", first.Conclusion)
			}
			before := map[string][]string{}
			for _, key := range []string{"tests.go#1", "tests.go#2"} {
				row := dh.stepRowOf(t, first.WorkRunID, key)
				if want := map[bool]string{true: WorkStepSkipped, false: WorkStepDone}[reused]; row["status"] != want {
					t.Fatalf("%s is %v in the first attempt, want %s", key, row["status"], want)
				}
				before[key] = argStrings(argObject(row, "call"), "packages")
			}
			failGate.Store(false)
			// A plan compiled now splits the packages a third way.
			dh.setTimings(map[string]float64{"acme.test/shop/c": 10, "acme.test/shop/a": 1, "acme.test/shop/b": 1})
			mark := len(dh.exec.sent())
			next, err := dh.integ.Rerun(personCtx(ownerID), first.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			deliver(t, dh.integ, next)
			got, _ := dh.store.run(next.ID)
			if got.Conclusion != ConclusionSuccess {
				t.Fatalf("the re-run concluded %q", got.Conclusion)
			}
			if sent := dh.sentSince(mark); !slices.Equal(sent, []string{"final.gate"}) {
				t.Fatalf("the re-run executed %v, want only final.gate", sent)
			}
			for key, packages := range before {
				row := dh.stepRowOf(t, got.WorkRunID, key)
				if row["status"] != WorkStepSkipped || row["errorCode"] != pipelines.CodePassedEarlier {
					t.Errorf("%s is not carried: %v", key, row)
				}
				if slices.Equal(packages, nil) || !slices.Equal(argStrings(argObject(row, "call"), "packages"), packages) {
					t.Errorf("%s names other packages than the attempt it carries: %v, want %v", key, argStrings(argObject(row, "call"), "packages"), packages)
				}
			}
		})
	}
}

// A shard that failed runs again over the slice it failed on, not a new split that
// could hand a package to a shard that already passed.
func TestAFailedOnlyReleaseRerunRunsAFailedShardOverItsOwnSlice(t *testing.T) {
	dh := newShardedHarness(t)
	var failShard atomic.Bool
	failShard.Store(true)
	dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		result, err := artifactFreeAnswer(ctx, req)
		if req.StepKey == "tests.go#2" && failShard.Load() {
			result.Status, result.ExitCode = pipelines.OutcomeFailed, 1
		}
		return result, err
	}
	first, _ := dh.drive(t, releaseOpening())
	if first.Conclusion != ConclusionFailure {
		t.Fatalf("the first attempt concluded %q", first.Conclusion)
	}
	failed := argStrings(argObject(dh.stepRowOf(t, first.WorkRunID, "tests.go#2"), "call"), "packages")
	failShard.Store(false)
	dh.moveTimings()
	mark := len(dh.exec.sent())
	next, err := dh.integ.Rerun(personCtx(ownerID), first.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	deliver(t, dh.integ, next)
	if got, _ := dh.store.run(next.ID); got.Conclusion != ConclusionSuccess {
		t.Fatalf("the re-run concluded %q", got.Conclusion)
	}
	var shard []string
	for _, req := range dh.exec.sent()[mark:] {
		if req.StepKey == "tests.go#1" {
			t.Fatalf("the shard that passed ran again")
		}
		if req.StepKey == "tests.go#2" {
			shard = req.Step.Packages
		}
	}
	if !slices.Equal(shard, failed) {
		t.Fatalf("the failed shard ran over %v, it failed on %v", shard, failed)
	}
}

// A takeover replays each step's call from its declaration, and the run's
// definition fingerprint covers every call. A replica that dropped what reuse wrote
// would leave a release candidate coverage refuses as no longer matching its
// declared execution, so the rows have to come back as they were. That includes a
// plan compiled after the timing table moved, whose fingerprints differ.
func TestATakeoverKeepsWhatReuseDeclared(t *testing.T) {
	for name, tc := range map[string]struct {
		harness func(*testing.T) *driveHarness
		source  Opening
		reused  []string
		moved   bool
	}{
		"the plan reads the same again": {
			harness: func(t *testing.T) *driveHarness { return newSameTreeHarness(t, sameTreeManifest, admitEveryReuse) },
			source:  prOpening(), reused: []string{releasedUnit, releasedIntegration},
		},
		"the table moved while it was stranded": {
			harness: newShardedHarness,
			source:  pushOpening(), reused: []string{"tests.go#1", "tests.go#2"}, moved: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dh := tc.harness(t)
			source := dh.driveSource(t, tc.source)

			release, vetStarted := make(chan struct{}), make(chan struct{})
			var vetTries atomic.Int64
			dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
				if req.StepKey == releasedVet {
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
			run := dh.openRun(t, releaseOpening())
			dh.integ.HandleRunEvent(graphEvent("graph.node.created", run))
			select {
			case <-vetStarted:
			case <-time.After(20 * time.Second):
				t.Fatal("the release never reached its running step")
			}
			stored, _ := dh.store.run(run.ID)
			declared := map[string]map[string]any{}
			for _, key := range tc.reused {
				declared[key] = argObject(dh.stepRowOf(t, stored.WorkRunID, key), "call")
				if declared[key]["evidenceRunId"] != source.ID {
					t.Fatalf("the plan did not reuse %s: %v", key, declared[key])
				}
			}

			dh.store.mu.Lock()
			r := dh.store.runs[run.ID]
			r.DriverHeartbeatAt = testNow.Add(-3 * time.Minute)
			dh.store.runs[run.ID] = r
			dh.store.mu.Unlock()
			if tc.moved {
				dh.moveTimings()
			}
			other := dh.peer("agent-b")
			a := workflowSource(t, admitEveryReuse)
			other.Configure(func(d *Deps) { bindTestWorkflow(d, a) })
			if err := other.RecoverRuns(context.Background()); err != nil {
				t.Fatal(err)
			}
			close(release)
			waitDrives(t, other)
			waitDrives(t, dh.integ)

			got, _ := dh.store.run(run.ID)
			if got.Conclusion != ConclusionSuccess {
				t.Fatalf("the release concluded %q on %q", got.Conclusion, got.DriverNodeID)
			}
			for _, key := range tc.reused {
				dh.requireReused(t, got, source, key)
				call := argObject(dh.stepRowOf(t, got.WorkRunID, key), "call")
				for _, field := range []string{"definitionFingerprint", "verificationFingerprint", "treeFingerprint"} {
					if call[field] != declared[key][field] {
						t.Errorf("%s: the takeover moved %s: %v, it began as %v", key, field, call[field], declared[key][field])
					}
				}
			}

			// What candidate coverage recomputes: every declared call, under the run's
			// definition fingerprint.
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
		})
	}
}
