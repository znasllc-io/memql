package pipelinerun

import (
	"context"
	"errors"
	"testing"
	"time"

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
