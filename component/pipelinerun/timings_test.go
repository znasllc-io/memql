package pipelinerun

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/pipelines"
)

const (
	timedRoot = "acme.test/shop"
	timedA    = "acme.test/shop/a"
	timedB    = "acme.test/shop/b"
	timedC    = "acme.test/shop/c"
)

// timingManifest is a sharded step between steps that run the same packages.
// Its numbers below are this repository's own, measured on 2026-10-31: fuzzing
// component/memql takes 40 seconds and a smoke test 5, against 800 for its suite
// in db-tests, and the tagged lane spends 430 on app, which takes 80 in db-tests.
const timingManifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  select:
    go: import-graph
  stages:
    - name: checks
      steps:
        - name: smoke
          run: go test -run Smoke ./a/
    - name: tests
      needs: [checks]
      steps:
        - name: go
          run: go test $MEMQL_PACKAGES
          packages: affected
          shards: 2
    - name: lanes
      needs: [tests]
      steps:
        - name: fuzz
          run: go test -fuzz Fuzz ./a/
        - name: tagged
          run: go test -tags slow ./b/
`

// timesOfTimingManifest is what each of its steps reports. Every stage after
// tests runs later than the shards, so the figures a table must not take are
// the last ones a drive reads.
func timesOfTimingManifest() map[string]map[string]float64 {
	return map[string]map[string]float64{
		"checks.smoke": {timedA: 5},
		"tests.go":     {timedA: 800, timedB: 80, timedC: 3},
		"lanes.fuzz":   {timedA: 40},
		"lanes.tagged": {timedB: 430},
	}
}

// addTimedPackages gives shaA's tree a, b and c beside its root package.
func (dh *driveHarness) addTimedPackages() {
	dh.github.mu.Lock()
	defer dh.github.mu.Unlock()
	tree := dh.github.trees[repoName+"@"+shaA]
	for _, pkg := range []string{"a", "b", "c"} {
		tree[pkg+"/"+pkg+".go"] = &fstest.MapFile{Data: []byte("package " + pkg + "\n")}
	}
}

// newTimingHarness has shaA hold the root package and a, b and c, with rows as
// the pipeline's table.
func newTimingHarness(t *testing.T, manifest string, rows map[string]float64) *driveHarness {
	t.Helper()
	dh := newDriveHarness(t, manifest)
	dh.addTimedPackages()
	dh.setTimings(rows)
	return dh
}

// reportTimes answers every step as passed, reporting the times byStep gives it:
// a step that selects packages, only those of its slice, as `go test` would.
func reportTimes(byStep map[string]map[string]float64) func(context.Context, pipelines.StepRequest) (pipelines.StepResult, error) {
	return func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		res := passed(req)
		step, _, _ := strings.Cut(req.StepKey, "#")
		res.Timings = map[string]float64{}
		for pkg, seconds := range byStep[step] {
			if len(req.Step.Packages) == 0 || slices.Contains(req.Step.Packages, pkg) {
				res.Timings[pkg] = seconds
			}
		}
		return res, nil
	}
}

// failing makes answer fail the step keyed key for as long as fails holds.
func failing(answer func(context.Context, pipelines.StepRequest) (pipelines.StepResult, error), key string, fails *atomic.Bool) func(context.Context, pipelines.StepRequest) (pipelines.StepResult, error) {
	return func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		res, err := answer(ctx, req)
		if req.StepKey == key && fails.Load() {
			res.Status, res.ExitCode = pipelines.OutcomeFailed, 1
		}
		return res, err
	}
}

func (dh *driveHarness) expectTimings(t *testing.T, want map[string]float64) {
	t.Helper()
	after, _ := dh.store.pipeline(dh.p.ID)
	if !maps.Equal(after.Timings, want) {
		t.Errorf("the table is %v, want %v", after.Timings, want)
	}
}

func (dh *driveHarness) expectConclusion(t *testing.T, run Run, want string) {
	t.Helper()
	if got, _ := dh.store.run(run.ID); got.Conclusion != want {
		t.Fatalf("the run concludes %q, want %q:\n%s", got.Conclusion, want, dh.logs.String())
	}
}

// The table's one reader is Partition, placing the packages of a step that
// declares shards, so what it needs is a package's time IN THAT STEP. Another
// step's time for the package answers a different question, whichever way it
// differs: fuzzing a package whose suite takes 800 seconds would read 40 (the
// last read was the rule, and the package then shared a shard with 24 others), and
// keeping the largest time instead would read 430 for a package a tagged lane
// spends that long on and its shard spends 80 on, leaving that shard short.
func TestOnlyAStepThatDeclaresShardsTeachesTheTimingTable(t *testing.T) {
	dh := newTimingHarness(t, timingManifest, map[string]float64{timedA: 10, timedB: 1, timedC: 1})
	dh.exec.answer = reportTimes(timesOfTimingManifest())

	run := dh.openRun(t, pushOpening())
	deliver(t, dh.integ, run)

	dh.expectConclusion(t, run, ConclusionSuccess)
	dh.expectTimings(t, map[string]float64{timedA: 800, timedB: 80, timedC: 3})
	if after, _ := dh.store.pipeline(dh.p.ID); after.TimingsRunID != bareID(run.ID) {
		t.Errorf("the table names the run that taught it: %q, want %q", after.TimingsRunID, bareID(run.ID))
	}
}

// A failed-only re-run reports only what it ran, so comparing a run's steps with
// each other would not protect a row here: the shards that measured the package
// are carried, not run, and the short step is all the drive ever reads. It would
// write 40 seconds over 800 with no other step in sight.
func TestAFailedOnlyRerunOfAnotherStepLeavesTheTimingTableAlone(t *testing.T) {
	taught := map[string]float64{timedA: 800, timedB: 80, timedC: 3}
	dh := newTimingHarness(t, timingManifest, taught)
	var fails atomic.Bool
	fails.Store(true)
	dh.exec.answer = failing(reportTimes(timesOfTimingManifest()), "lanes.fuzz", &fails)

	first := dh.openRun(t, pushOpening())
	deliver(t, dh.integ, first)
	dh.expectConclusion(t, first, ConclusionFailure)
	sentBefore := len(dh.exec.sent())

	fails.Store(false)
	next, err := dh.integ.Rerun(personCtx(ownerID), first.ID, true)
	if err != nil {
		t.Fatalf("rerun failed steps: %v", err)
	}
	deliver(t, dh.integ, next)

	dh.expectConclusion(t, next, ConclusionSuccess)
	var keys []string
	for _, req := range dh.exec.sent()[sentBefore:] {
		keys = append(keys, req.StepKey)
	}
	if !slices.Equal(keys, []string{"lanes.fuzz"}) {
		t.Fatalf("only the step that failed runs again: %v", keys)
	}
	dh.expectTimings(t, taught)
	if after, _ := dh.store.pipeline(dh.p.ID); after.TimingsRunID != "" {
		t.Errorf("the re-run taught nothing, so it is not named as the teacher: %q", after.TimingsRunID)
	}
}

// A shard the re-run runs again measures what the table wants, and teaches it;
// the rows of the shards it carries keep what they had, and the steps after it
// still teach nothing.
func TestAFailedOnlyRerunOfAShardTeachesWhatItRan(t *testing.T) {
	// a is heaviest, so it takes the first shard alone: b, c and the root package
	// are the second.
	dh := newTimingHarness(t, timingManifest, map[string]float64{timedA: 10, timedB: 1, timedC: 1})
	var fails atomic.Bool
	fails.Store(true)
	dh.exec.answer = failing(reportTimes(timesOfTimingManifest()), "tests.go#2", &fails)

	first := dh.openRun(t, pushOpening())
	deliver(t, dh.integ, first)
	dh.expectConclusion(t, first, ConclusionFailure)
	dh.expectTimings(t, map[string]float64{timedA: 10, timedB: 1, timedC: 1})
	sentBefore := len(dh.exec.sent())

	fails.Store(false)
	next, err := dh.integ.Rerun(personCtx(ownerID), first.ID, true)
	if err != nil {
		t.Fatalf("rerun failed steps: %v", err)
	}
	deliver(t, dh.integ, next)

	dh.expectConclusion(t, next, ConclusionSuccess)
	var keys []string
	for _, req := range dh.exec.sent()[sentBefore:] {
		keys = append(keys, req.StepKey)
	}
	if slices.Contains(keys, "tests.go#1") || !slices.Contains(keys, "tests.go#2") {
		t.Fatalf("the failed shard alone runs again: %v", keys)
	}
	dh.expectTimings(t, map[string]float64{timedA: 10, timedB: 80, timedC: 3})
}

// A replica that takes a run over compiles the plan again and reads what it sees
// finish, so the shards it carries on with still teach. What the replica before
// it read died with that drive, which costs a stale row at worst. Here the first
// replica stops with both shards in flight and never reads them.
func TestAReplicaThatTakesARunOverTeachesWhatItSawFinish(t *testing.T) {
	dh := newTimingHarness(t, timingManifest, map[string]float64{timedA: 10, timedB: 1, timedC: 1})
	release := make(chan struct{})
	var tries atomic.Int64
	report := reportTimes(timesOfTimingManifest())
	dh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if strings.HasPrefix(req.StepKey, "tests.go") && tries.Add(1) <= 2 {
			<-release // agent-a's two shards, in flight when it stops
		}
		return report(ctx, req)
	}
	run := dh.openRun(t, pushOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	dh.awaitEntered(t, "tests.go#1", "tests.go#2")

	dh.store.mu.Lock()
	stranded := dh.store.runs[run.ID]
	stranded.DriverHeartbeatAt = testNow.Add(-3 * time.Minute)
	dh.store.runs[run.ID] = stranded
	dh.store.mu.Unlock()

	other := dh.peer("agent-b")
	if err := other.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, other)
	close(release)
	waitDrives(t, dh.integ)

	dh.expectConclusion(t, run, ConclusionSuccess)
	if got, _ := dh.store.run(run.ID); got.DriverNodeID != "agent-b" {
		t.Fatalf("the run was taken over: it ended with %q", got.DriverNodeID)
	}
	dh.expectTimings(t, map[string]float64{timedA: 800, timedB: 80, timedC: 3})
}

// A step that declares shards can still come out whole: when every package is
// measured at zero they all land in one shard, and it runs under its own key with
// no MEMQL_SHARD. It must teach all the same, or the table could never learn its
// way out and the step would stay whole however long it grew.
func TestAStepThatDeclaresShardsTeachesWhenItCouldNotSplit(t *testing.T) {
	dh := newTimingHarness(t, timingManifest, map[string]float64{timedRoot: 0, timedA: 0, timedB: 0, timedC: 0})
	dh.exec.answer = reportTimes(timesOfTimingManifest())

	run := dh.openRun(t, pushOpening())
	deliver(t, dh.integ, run)

	dh.expectConclusion(t, run, ConclusionSuccess)
	if keys := dh.exec.sentKeys(); !slices.Contains(keys, "tests.go") {
		t.Fatalf("the step comes out whole: %v", keys)
	}
	dh.expectTimings(t, map[string]float64{timedRoot: 0, timedA: 800, timedB: 80, timedC: 3})
}

// Two steps that declare shards can run one package, and the table has one row
// for it. The larger time takes it, whichever step finished last: a package placed
// too heavy leaves a shard short, one placed too light overloads a shard. It is the
// largest of THIS run, though, and a later run's figure replaces it, so a package
// that gets faster gets lighter.
func TestTwoShardedStepsOverOnePackageTeachTheLargerTime(t *testing.T) {
	dh := newTimingHarness(t, `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  select:
    go: import-graph
  stages:
    - name: tests
      steps:
        - name: go
          run: go test $MEMQL_PACKAGES
          packages: affected
          shards: 2
    - name: race
      needs: [tests]
      steps:
        - name: go
          run: go test -race $MEMQL_PACKAGES
          packages: affected
          shards: 2
`, map[string]float64{timedA: 10, timedB: 1, timedC: 1})
	dh.exec.answer = reportTimes(map[string]map[string]float64{
		"tests.go": {timedA: 300, timedB: 2, timedC: 7},
		"race.go":  {timedA: 100, timedB: 9, timedC: 7},
	})

	first := dh.openRun(t, pushOpening())
	deliver(t, dh.integ, first)
	dh.expectConclusion(t, first, ConclusionSuccess)
	dh.expectTimings(t, map[string]float64{timedA: 300, timedB: 9, timedC: 7})

	dh.exec.mu.Lock()
	dh.exec.answer = reportTimes(map[string]map[string]float64{
		"tests.go": {timedA: 50, timedB: 2, timedC: 7},
		"race.go":  {timedA: 20, timedB: 9, timedC: 7},
	})
	dh.exec.mu.Unlock()
	next, err := dh.integ.Rerun(personCtx(ownerID), first.ID, false)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	deliver(t, dh.integ, next)
	dh.expectConclusion(t, next, ConclusionSuccess)
	dh.expectTimings(t, map[string]float64{timedA: 50, timedB: 9, timedC: 7})
}

// A package path reaches the pipeline's row, so one that would need masking is no
// package path and is not kept.
func TestATimingPathCarryingASecretIsNotTaught(t *testing.T) {
	dh := newTimingHarness(t, strings.Replace(timingManifest, "          shards: 2\n",
		"          shards: 2\n          secrets: [SHOP_TOKEN]\n", 1), map[string]float64{timedA: 10})
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		res := passed(req)
		if strings.HasPrefix(req.StepKey, "tests.go") {
			res.Timings = map[string]float64{"acme.test/" + shopSecret: 1, timedA: 2}
		}
		return res, nil
	}

	run := dh.openRun(t, pushOpening())
	deliver(t, dh.integ, run)

	dh.expectConclusion(t, run, ConclusionSuccess)
	dh.expectTimings(t, map[string]float64{timedA: 2})
}
