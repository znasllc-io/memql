package pipelinerun

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/workjournal"
)

func claimedTestDriver(t *testing.T, integ *Integration, runID string) *runDriver {
	t.Helper()
	d := integ.snapshot()
	run, ok, err := integ.claim(context.Background(), d, runID)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	dr := &runDriver{i: integ, d: d, lease: newLease(runID), runID: runID, run: run, claimID: run.DriverLeaseID, log: d.Logger}
	dr.guardJournal()
	return dr
}

// The old process has already checked its lease and received a result when
// it pauses. A replacement with the SAME node name claims and saves its own
// receipt. Letting the old process wake must not overwrite that receipt.
func TestAClaimTokenFencesAPausedReceiptFromTheSameNode(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	old := claimedTestDriver(t, dh.integ, run.ID)
	wr := old.d.Journal.Reopen(dh.p.OwnerUserID, "goal", "work", []workjournal.StepDecl{{Key: "build"}}, testNow)
	step, err := wr.Step(context.Background(), "build")
	if err != nil {
		t.Fatal(err)
	}
	if !old.stillHolds(context.Background()) {
		t.Fatal("initial lease not held")
	}
	paused, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	gate := old.d.Gate
	old.d.Gate = func(ctx context.Context, key string, write func(context.Context) error) error {
		close(paused)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return gate(ctx, key, write)
	}
	done := make(chan error, 1)
	go func() {
		done <- step.Finish(context.Background(), workjournal.Receipt{Status: "failed", Code: "obsolete"})
	}()
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("receipt never reached guard")
	}
	next := claimedTestDriver(t, dh.peer(old.d.NodeID), run.ID)
	if old.claimID == "" || old.claimID == next.claimID {
		t.Fatal("same-node claim reused ownership")
	}
	replacement := next.d.Journal.Reopen(dh.p.OwnerUserID, "goal", "work", []workjournal.StepDecl{{Key: "build"}}, testNow)
	current, err := replacement.Step(context.Background(), "build")
	if err != nil {
		t.Fatal(err)
	}
	if err := current.Finish(context.Background(), workjournal.Receipt{Status: "done"}); err != nil {
		t.Fatal(err)
	}
	before := len(dh.work.recorded())
	release <- struct{}{}
	select {
	case err := <-done:
		if !errors.Is(err, errLeaseLost) {
			t.Fatalf("stale receipt: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale receipt did not return")
	}
	if len(dh.work.recorded()) != before || !old.lease.isLost() {
		t.Fatal("old process wrote after takeover")
	}
	wr.Heartbeat(context.Background())
	if err := wr.Succeeded(context.Background(), nil); !errors.Is(err, errLeaseLost) {
		t.Fatalf("stale terminal: %v", err)
	}
	if len(dh.work.recorded()) != before {
		t.Fatal("old heartbeat or terminal wrote after takeover")
	}
}

// A heartbeat whose reply is lost still lands: the client gives up on the
// read while the server commits. The next journal write then waits on the
// run's advisory lock and finds the row one version further on than its read
// -- a version THIS replica wrote. That is not another holder, and treating it
// as one stopped a healthy drive during a database stall (run 9892b7ef,
// 2026-10-11). The guard reads again, finds its own claim, and decides again.
func TestAVersionThisReplicaWroteIsNotALostLease(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	dr := claimedTestDriver(t, dh.integ, run.ID)
	wr := dr.d.Journal.Reopen(dh.p.OwnerUserID, "goal", "work", []workjournal.StepDecl{{Key: "build"}}, testNow)
	step, err := wr.Step(context.Background(), "build")
	if err != nil {
		t.Fatal(err)
	}
	dh.work.refuseCall, dh.work.refuseErr, dh.work.refuseTimes = "updateWorkStep", memql.ErrRowVersionChanged, 1
	dh.work.onRefuse = func() {
		// The late heartbeat: the row moves, its holder does not.
		if err := dh.store.UpdateRun(context.Background(), dh.p.OwnerUserID, run.ID, RunPatch{DriverHeartbeatAt: ptr(testNow.Add(30 * time.Second))}); err != nil {
			t.Error(err)
		}
	}
	before := len(dh.work.callsNamed("updateWorkStep"))
	if err := step.Finish(context.Background(), workjournal.Receipt{Status: "done"}); err != nil {
		t.Fatalf("the receipt did not land: %v", err)
	}
	if dr.lease.isLost() {
		t.Fatal("a version this replica wrote was read as another holder")
	}
	if got := len(dh.work.callsNamed("updateWorkStep")); got != before+1 {
		t.Fatalf("the receipt landed %d times, want once", got-before)
	}
}

// The other half stays: when the version moved because another process
// claimed the run -- a claim whose own reply was lost lands the same late way,
// past the gate -- the write is refused and this drive writes nothing more.
func TestAVersionAnotherClaimWroteStillLosesTheLease(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	dr := claimedTestDriver(t, dh.integ, run.ID)
	wr := dr.d.Journal.Reopen(dh.p.OwnerUserID, "goal", "work", []workjournal.StepDecl{{Key: "build"}}, testNow)
	step, err := wr.Step(context.Background(), "build")
	if err != nil {
		t.Fatal(err)
	}
	dh.work.refuseCall, dh.work.refuseErr, dh.work.refuseTimes = "updateWorkStep", memql.ErrRowVersionChanged, 1
	dh.work.onRefuse = func() {
		if err := dh.store.UpdateRun(context.Background(), dh.p.OwnerUserID, run.ID, RunPatch{
			DriverNodeID: ptr("agent-b"), DriverLeaseID: ptr("claim-b"), DriverHeartbeatAt: ptr(testNow.Add(time.Minute)),
		}); err != nil {
			t.Error(err)
		}
	}
	before := len(dh.work.recorded())
	if err := step.Finish(context.Background(), workjournal.Receipt{Status: "done"}); !errors.Is(err, errLeaseLost) {
		t.Fatalf("a receipt fenced by another claim: %v", err)
	}
	if !dr.lease.isLost() || len(dh.work.recorded()) != before {
		t.Fatal("the old drive kept writing after another claim")
	}
}

// A row that cannot be read proves neither: the write did not land, and the
// lease is not given up on a guess.
func TestAnUnreadableRowAfterAMovedVersionIsNotALostLease(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	dr := claimedTestDriver(t, dh.integ, run.ID)
	wr := dr.d.Journal.Reopen(dh.p.OwnerUserID, "goal", "work", []workjournal.StepDecl{{Key: "build"}}, testNow)
	step, err := wr.Step(context.Background(), "build")
	if err != nil {
		t.Fatal(err)
	}
	dh.work.refuseCall, dh.work.refuseErr, dh.work.refuseTimes = "updateWorkStep", memql.ErrRowVersionChanged, 1
	dh.work.onRefuse = func() { dr.d.Store = unavailableLeaseStore{dr.d.Store} }
	if err := step.Finish(context.Background(), workjournal.Receipt{Status: "done"}); !errors.Is(err, memql.ErrRowVersionChanged) {
		t.Fatalf("the write = %v, want the refusal it met", err)
	}
	if dr.lease.isLost() {
		t.Fatal("an unreadable row was read as another holder")
	}
}

type unavailableLeaseStore struct{ Store }

func (s unavailableLeaseStore) RunByID(context.Context, string) (*Run, error) {
	return nil, errors.New("database disconnected")
}

func TestAnUnreadableLeaseCannotAuthorizeAnExternalCall(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	dr := claimedTestDriver(t, dh.integ, run.ID)
	dr.d.Store = unavailableLeaseStore{dr.d.Store}
	if dr.stillHolds(context.Background()) {
		t.Fatal("failed lease read authorized an external call")
	}
	called := false
	if err := dr.underLease(context.Background(), func(context.Context, Run) error { called = true; return nil }); err == nil || called {
		t.Fatal("unreadable lease authorized a write")
	}
}
