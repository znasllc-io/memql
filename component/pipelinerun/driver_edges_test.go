package pipelinerun

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
)

// driver_edges_test.go -- the edges an independent review of the driver
// found, each reproduced here first: a GitHub read that never answers, a step
// a resume would have skipped behind the runner's back, a lease lost in the
// middle of a conclusion, a check run created twice, a final report that did
// not land, work opened before it was named, and runner text that was not
// masked.

// hangingTree is a GitHub whose tarball read never answers until its context
// ends -- the tarball client has no timeout of its own.
type hangingTree struct {
	GitHub
	entered chan struct{}
}

func (g *hangingTree) Tree(ctx context.Context, _, _, _ string, _ func(string) bool, _ int64) (fs.FS, error) {
	g.entered <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestACancelStopsATreeReadThatHangs(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	hang := &hangingTree{GitHub: dh.github, entered: make(chan struct{}, 1)}
	dh.integ.Configure(func(d *Deps) { d.GitHub = hang })
	run := dh.openRun(t, prOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	select {
	case <-hang.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("the tree was never read")
	}

	if err := dh.integ.RequestCancel(context.Background(), run.ID, "v1:identity:user:"+ownerID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitDrives(t, dh.integ)
	got, _ := dh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionCancelled || got.RefusalCode != "" {
		t.Errorf("a cancel ends a run whose tree read hangs, as cancelled: %s/%s %q", got.Status, got.Conclusion, got.RefusalCode)
	}
	if last := dh.lastUpdate(t).Run; last.Conclusion != "cancelled" {
		t.Errorf("check run: %s", last.Conclusion)
	}
}

// The first driver could not read the change, so the bucket-gated step ran;
// by the time another replica takes the run over the change reads whole and
// misses the bucket. The step was HANDED TO THE RUNNER, so it is re-sent and
// its outcome is the run's -- here a failure, which a skip would have hidden
// behind a green check.
func TestAResumedStepThatWasSentIsNotSkippedBehindTheRunnersBack(t *testing.T) {
	dh := newDriveHarness(t, `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  select:
    buckets:
      os: ["web/**"]
  stages:
    - name: tests
      steps:
        - name: unit
          run: go test ./...
        - name: os
          run: make os
          when: { bucket: os }
`)
	release := make(chan struct{})
	var tries atomic.Int64
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if tries.Add(1) <= 2 {
			<-release // agent-a's two steps, in flight when it stops
			return passed(req), nil
		}
		res := passed(req)
		if req.StepKey == "tests.os" {
			res.Status, res.ExitCode = pipelines.OutcomeFailed, 2
			res.Failure = &pipelines.Failure{Code: pipelines.CodeJobRejected, Message: "the os step failed"}
		}
		return res, nil
	}
	run := dh.openRun(t, prOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	dh.awaitEntered(t, "tests.unit", "tests.os")

	dh.store.mu.Lock()
	r := dh.store.runs[run.ID]
	r.DriverHeartbeatAt = testNow.Add(-3 * time.Minute)
	dh.store.runs[run.ID] = r
	dh.store.mu.Unlock()
	dh.github.mu.Lock()
	dh.github.compare[repoName+"@"+shaBase+"..."+shaA] = compareAnswer{Files: []string{"main.go"}, Complete: true}
	dh.github.mu.Unlock()

	other := dh.peer("agent-b")
	if err := other.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, other)
	close(release)
	waitDrives(t, dh.integ)

	counts := map[string]int{}
	for _, k := range dh.exec.sentKeys() {
		counts[k]++
	}
	if counts["tests.os"] != 2 {
		t.Errorf("the os step was sent, so it is re-sent: %v", counts)
	}
	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionFailure {
		t.Errorf("its failure is the run's: %s", got.Conclusion)
	}
	if os := dh.work.receiptsOf("tests.os"); len(os) != 1 || argString(os[0].Args, "status") != WorkStepFailed {
		t.Errorf("the os step's receipt is its runner's answer, not a skip: %+v", os)
	}
}

// flipOnSteps is a store over which another replica takes the run over the
// moment this one reads the work steps to close them.
type flipOnSteps struct {
	*memStore
	flip func()
}

func (s *flipOnSteps) WorkSteps(ctx context.Context, workRunID string) ([]WorkStep, error) {
	s.flip()
	return s.memStore.WorkSteps(ctx, workRunID)
}

// A lease found lost WHILE concluding -- by the fence of the first step it
// settles -- stops the conclusion there: no work run closed, no check run
// written, no row, and no cancel of work the new holder now owns.
func TestALeaseLostMidConclusionWritesNothingMore(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	finish := dh.strand(t, run)
	dh.store.mu.Lock()
	r := dh.store.runs[run.ID]
	r.CancelRequested = true
	dh.store.runs[run.ID] = r
	dh.store.mu.Unlock()

	var journalAt, checksAt int
	other := dh.peer("agent-b")
	other.Configure(func(d *Deps) {
		d.Store = &flipOnSteps{memStore: dh.store, flip: func() {
			dh.store.mu.Lock()
			r := dh.store.runs[run.ID]
			r.DriverNodeID, r.DriverHeartbeatAt = "agent-c", testNow
			dh.store.runs[run.ID] = r
			dh.store.mu.Unlock()
			journalAt = len(dh.work.recorded())
			checksAt = len(dh.github.createdRuns()) + len(dh.github.updatedRuns())
		}}
	})
	if err := other.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, other)
	finish()

	if n := len(dh.work.recorded()); n != journalAt {
		t.Errorf("the journal was written after the loss: %d -> %d", journalAt, n)
	}
	if n := len(dh.github.createdRuns()) + len(dh.github.updatedRuns()); n != checksAt {
		t.Errorf("the check run was written after the loss: %d -> %d", checksAt, n)
	}
	if got, _ := dh.store.run(run.ID); got.Status == StatusCompleted || got.DriverNodeID != "agent-c" {
		t.Errorf("the run is agent-c's, unconcluded: %s by %q", got.Status, got.DriverNodeID)
	}
	if cancels := dh.exec.cancelled(); len(cancels) != 0 {
		t.Errorf("work the new holder owns is not cancelled: %v", cancels)
	}
}

// The opening's check run never landed, and the row write recording the one
// the driver then created fails: every later write still moves THAT check run
// -- never a second beside it -- and the conclusion records its id.
func TestACheckRunTheRowDidNotLearnIsNotCreatedTwice(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	dh.github.mu.Lock()
	dh.github.createErr = errStatus(502)
	dh.github.mu.Unlock()
	run := dh.openRun(t, prOpening())
	if run.CheckRunID != 0 || run.CheckRunState != CheckRunUnavailable {
		t.Fatalf("the opening's check run did not land: %d %q", run.CheckRunID, run.CheckRunState)
	}
	dh.github.mu.Lock()
	dh.github.createErr = nil
	dh.github.mu.Unlock()
	var refused atomic.Bool
	dh.integ.Configure(func(d *Deps) {
		d.Store = &flakyStore{Store: dh.store, fail: func(p RunPatch) error {
			if p.CheckRunID != nil && refused.CompareAndSwap(false, true) {
				return errors.New("the gate timed out")
			}
			return nil
		}}
	})
	deliver(t, dh.integ, run)

	created := dh.github.createdRuns()
	if len(created) != 1 {
		t.Fatalf("one check run is created: %d", len(created))
	}
	for _, w := range dh.github.updatedRuns() {
		if w.ID != created[0].ID {
			t.Errorf("a write moved check run %d; the run's is %d", w.ID, created[0].ID)
		}
	}
	got, _ := dh.store.run(run.ID)
	if got.CheckRunID != created[0].ID || got.CheckRunState != CheckRunWritten || got.Conclusion != ConclusionSuccess {
		t.Errorf("the row ends naming the check run: id %d state %q, %s", got.CheckRunID, got.CheckRunState, got.Conclusion)
	}
}

// flakyFinal is a GitHub whose completing check-run writes fail with a 502 a
// number of times first.
type flakyFinal struct {
	GitHub
	fails atomic.Int32
}

func (g *flakyFinal) UpdateCheckRun(ctx context.Context, token, repository string, id int64, run githubapp.CheckRun) error {
	if run.Status == StatusCompleted && g.fails.Add(-1) >= 0 {
		return &githubapp.StatusError{Status: 502, Endpoint: "/repos/" + repository + "/check-runs"}
	}
	return g.GitHub.UpdateCheckRun(ctx, token, repository, id, run)
}

// A required check left showing a finished run unfinished holds a merge: the
// final write is tried again while its failure may pass, and when it never
// lands the row says the check run is not current rather than "written".
func TestTheFinalCheckRunWriteIsRetriedAndItsFailureRecorded(t *testing.T) {
	for _, c := range []struct {
		name  string
		fails int32
		state string
	}{
		{"lands on the third try", 2, CheckRunWritten},
		{"never lands", 99, CheckRunUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			dh := newDriveHarness(t, driveManifest)
			flaky := &flakyFinal{GitHub: dh.github}
			flaky.fails.Store(c.fails)
			dh.integ.Configure(func(d *Deps) {
				d.GitHub = flaky
				d.publishBackoff = []time.Duration{time.Millisecond, time.Millisecond}
			})
			run := dh.openRun(t, prOpening())
			deliver(t, dh.integ, run)

			got, _ := dh.store.run(run.ID)
			if got.Status != StatusCompleted || got.Conclusion != ConclusionSuccess || got.CheckRunState != c.state {
				t.Errorf("run %s/%s, checkRunState %q, want %q", got.Status, got.Conclusion, got.CheckRunState, c.state)
			}
			completed := 0
			for _, w := range dh.github.updatedRuns() {
				if w.Run.Status == StatusCompleted {
					completed++
				}
			}
			if want := map[bool]int{true: 1, false: 0}[c.state == CheckRunWritten]; completed != want {
				t.Errorf("completed writes that landed: %d, want %d", completed, want)
			}
		})
	}
}

// The work is named on the run row BEFORE it is opened: a Begin that fails
// leaves a row naming work that does not exist yet -- which a resume opens,
// under the same ids -- and never work that nothing names.
func TestTheWorkIsNamedBeforeItIsOpened(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	var clock atomic.Int64
	dh.integ.Configure(func(d *Deps) {
		d.Now = func() time.Time { return testNow.Add(time.Duration(clock.Load()) * time.Second) }
	})
	dh.work.mu.Lock()
	dh.work.refuseCall, dh.work.refuseErr = "createWorkGoal", errors.New("the work spine refused the goal")
	dh.work.mu.Unlock()
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)

	named, _ := dh.store.run(run.ID)
	goalID, runID, err := workjournal.IDs(workjournal.Work{
		Template: "pipeline:shop", GoalKey: bareID(dh.p.ID) + "|" + run.RunKey, RunKey: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if named.WorkRunID != runID || named.WorkGoalID != goalID || named.Status != StatusInProgress {
		t.Fatalf("the row names the work before it is opened: run %q goal %q (%s); want %q %q",
			named.WorkRunID, named.WorkGoalID, named.Status, runID, goalID)
	}
	if n := len(dh.work.callsNamed("createWorkRun")); n != 0 || len(dh.exec.sent()) != 0 {
		t.Fatalf("no work was opened and nothing ran: %d work runs, %v", n, dh.exec.sentKeys())
	}

	dh.work.mu.Lock()
	dh.work.refuseErr = nil
	dh.work.mu.Unlock()
	clock.Add(61)
	if err := dh.integ.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, dh.integ)
	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionSuccess || got.WorkRunID != runID {
		t.Errorf("the resume opens the work it named: %s, work %q", got.Conclusion, got.WorkRunID)
	}
	if wr := dh.work.run(runID); argString(wr, "status") != "succeeded" {
		t.Errorf("the named work run is opened and closed: %v", wr)
	}
}

// Every string the runner reports is masked before it is kept: a code, a
// status, a job name, a machine label, a file id and a package path as much as
// a message (Review Focus 5).
func TestEveryRunnerStringIsMasked(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		res := passed(req)
		if req.StepKey != "tests.unit" {
			return res, nil
		}
		res.Status = pipelines.Outcome("odd-" + shopSecret)
		res.Failure = &pipelines.Failure{Code: "code-" + shopSecret, Message: "message"}
		res.Notes = []pipelines.Failure{{Code: "note-" + shopSecret, Message: "note"}}
		res.Where = pipelines.Where{
			Surface: "cluster", JobName: "job-" + shopSecret, WorkerID: "w-" + shopSecret,
			MachineLabels: map[string]string{"k-" + shopSecret: "v-" + shopSecret},
		}
		res.LogFileID = "file-" + shopSecret
		res.ArtifactFileIDs = []string{"art-" + shopSecret}
		res.Timings = map[string]float64{"acme.test/" + shopSecret: 1, "acme.test/shop": 2}
		return res, nil
	}
	run := dh.openRun(t, pushOpening())
	deliver(t, dh.integ, run)

	for _, c := range dh.work.recorded() {
		if strings.Contains(c.Raw, shopSecret) {
			t.Errorf("a journal write carries the value: %s", c.Raw)
		}
	}
	for _, w := range append(dh.github.createdRuns(), dh.github.updatedRuns()...) {
		if o := w.Run.Output; o != nil && strings.Contains(o.Title+o.Summary+o.Text, shopSecret) {
			t.Errorf("a check-run write carries the value: %s", o.Summary)
		}
	}
	dh.store.mu.Lock()
	rows, _ := json.Marshal(struct {
		Runs      []runUpdate
		Pipelines []pipelineUpdate
	}{dh.store.runUpdates, dh.store.pipelineUpdates})
	dh.store.mu.Unlock()
	if strings.Contains(string(rows), shopSecret) {
		t.Errorf("a row write carries the value")
	}
	if strings.Contains(dh.logs.String(), shopSecret) {
		t.Errorf("a log line carries the value")
	}
	unit := dh.work.receiptsOf("tests.unit")
	if len(unit) != 1 || argString(unit[0].Args, "errorCode") != "code-***" {
		t.Errorf("the code is kept, masked: %+v", unit)
	}
	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionFailure {
		t.Errorf("an outcome the driver does not know fails the step: %s", got.Conclusion)
	}
}

// A predecessor's steps that a run ends without re-sending are stopped: a
// stranded run refused on resume tells the runner to stop what it left in
// flight.
func TestARunEndedOnResumeStopsWhatItsPredecessorLeftInFlight(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	finish := dh.strand(t, run)
	dh.github.mu.Lock()
	dh.github.tokenErr = errors.New("GitHub is down")
	dh.github.mu.Unlock()

	other := dh.peer("agent-b")
	if err := other.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, other)
	finish()
	if cancels := dh.exec.cancelled(); len(cancels) != 1 || cancels[0] != run.ID {
		t.Errorf("the runner is told to stop the abandoned steps: %v", cancels)
	}
	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionFailure {
		t.Errorf("run = %s", got.Conclusion)
	}
}

// A cancel that arrives after every step has ended has nothing to stop: the
// run reports what its steps did. One that cut a step short is a cancel.
func TestACancelWithNothingLeftToStopReportsTheSteps(t *testing.T) {
	passedTrack := &stepTrack{state: StepState{Key: "checks.vet", Status: StepSucceeded}}
	dr := &runDriver{lease: newLease("r"), tracks: []*stepTrack{passedTrack}}
	dr.lease.cancel()
	if v := dr.verdictOfSteps(); v.conclusion != ConclusionSuccess {
		t.Errorf("every step passed before the cancel: %s", v.conclusion)
	}
	dr.tracks = append(dr.tracks, &stepTrack{state: StepState{Key: "tests.unit", Status: StepCancelled}})
	if v := dr.verdictOfSteps(); v.conclusion != ConclusionCancelled {
		t.Errorf("a step the cancel cut short: %s", v.conclusion)
	}
}

// A cancel is answered before anything else a run is: a stranded run asked to
// stop, on a pipeline disconnected since, is cancelled -- not failed
// "disconnected".
func TestACancelIsAnsweredBeforeADisconnect(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	dh.store.mu.Lock()
	r := dh.store.runs[run.ID]
	r.DriverNodeID, r.DriverHeartbeatAt, r.CancelRequested = "agent-z", testNow.Add(-3*time.Minute), true
	dh.store.runs[run.ID] = r
	p := dh.store.pipelines[dh.p.ID]
	p.Status = PipelineDisconnected
	dh.store.pipelines[dh.p.ID] = p
	dh.store.mu.Unlock()

	if err := dh.integ.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, dh.integ)
	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionCancelled || got.RefusalCode != "" {
		t.Errorf("run = %s %q", got.Conclusion, got.RefusalCode)
	}
}
