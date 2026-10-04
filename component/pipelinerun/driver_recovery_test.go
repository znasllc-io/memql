package pipelinerun

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// driver_recovery_test.go -- a drive that stops short leaves its run claimed,
// and the run is taken up again: the case a crashed replica and a failed
// write share.

// flakyStore refuses the run writes fail picks, and passes everything else
// to the store underneath.
type flakyStore struct {
	Store
	fail func(RunPatch) error
}

func (s *flakyStore) UpdateRun(ctx context.Context, owner, runID string, patch RunPatch) error {
	if err := s.fail(patch); err != nil {
		return err
	}
	return s.Store.UpdateRun(ctx, owner, runID, patch)
}

// A write the run cannot go on without fails: the drive stops with nothing
// more written, the run stays claimed by this node, and the poll's recovery on
// this same node -- the lease names it, and no drive of it runs here -- takes
// it up again, without waiting for the lease to go stale. Not at once: a stop
// that recurs every time must not cost a tarball every minute, so recovery
// leaves the run alone for a while first, longer after each stop.
func TestADriveThatStopsShortIsTakenUpAgainByItsOwnNode(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	var refusing atomic.Bool
	refusing.Store(true)
	var clock atomic.Int64 // seconds past testNow
	dh.integ.Configure(func(d *Deps) {
		d.Now = func() time.Time { return testNow.Add(time.Duration(clock.Load()) * time.Second) }
		d.Store = &flakyStore{Store: dh.store, fail: func(p RunPatch) error {
			if refusing.Load() && p.Status != nil && *p.Status == StatusInProgress {
				return errors.New("the database went away")
			}
			return nil
		}}
	})
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)

	stopped, _ := dh.store.run(run.ID)
	if stopped.Status != StatusQueued || stopped.DriverNodeID != "agent-a" || stopped.WorkRunID != "" {
		t.Fatalf("the run stays claimed and unstarted: %s by %q, work %q", stopped.Status, stopped.DriverNodeID, stopped.WorkRunID)
	}
	if len(dh.exec.sent()) != 0 || len(dh.work.recorded()) != 0 {
		t.Errorf("nothing ran and nothing was journaled: %v, %d", dh.exec.sentKeys(), len(dh.work.recorded()))
	}
	if !strings.Contains(dh.logs.String(), "the drive stopped moving the run to in_progress") {
		t.Errorf("the stop is said, with what it stopped at:\n%s", dh.logs.String())
	}

	// Twice more, each stop pushing the next try further out.
	for _, wait := range []int64{60, 120} {
		if err := dh.integ.RecoverRuns(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitDrives(t, dh.integ)
		if n := len(dh.github.treeCalls); n != 0 {
			t.Fatalf("recovery inside the wait takes nothing up (%d tree reads)", n)
		}
		clock.Add(wait)
		if err := dh.integ.RecoverRuns(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitDrives(t, dh.integ)
		if got, _ := dh.store.run(run.ID); got.Status != StatusQueued || got.DriverNodeID != "agent-a" {
			t.Fatalf("the run stopped short again and stays claimed: %s by %q", got.Status, got.DriverNodeID)
		}
	}
	clock.Add(239)
	if err := dh.integ.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, dh.integ)
	if got, _ := dh.store.run(run.ID); got.Status != StatusQueued {
		t.Fatalf("after its third stop the run waits four minutes, not less: %s", got.Status)
	}

	refusing.Store(false)
	clock.Add(1)
	if _, err := dh.integ.Poll(automationCtx()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	waitDrives(t, dh.integ)
	if got, _ := dh.store.run(run.ID); got.Status != StatusCompleted || got.Conclusion != ConclusionSuccess || got.DriverNodeID != "agent-a" {
		t.Errorf("recovered by its own node and driven to the end: %s/%s by %q", got.Status, got.Conclusion, got.DriverNodeID)
	}
}

func TestAbortBackoffDoublesToHalfAnHour(t *testing.T) {
	for n, want := range map[int]time.Duration{
		1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 5: 16 * time.Minute, 6: 30 * time.Minute, 40: 30 * time.Minute,
	} {
		if got := abortBackoff(n); got != want {
			t.Errorf("abortBackoff(%d) = %v, want %v", n, got, want)
		}
	}
}

// A node set to drive that lacks a port it needs says so through the poll,
// rather than recovering nothing in silence.
func TestRecoveryOnANodeThatCannotDriveSaysWhy(t *testing.T) {
	h := newHarness(t)
	h.integ.EnableDriver() // no journal is wired
	if err := h.integ.RecoverRuns(context.Background()); err == nil || !strings.Contains(err.Error(), "no work journal") {
		t.Errorf("recover: %v", err)
	}
	res, err := h.integ.Poll(automationCtx())
	if err != nil || !strings.Contains(res.RecoverError, "no work journal") {
		t.Errorf("the poll reports it: %+v %v", res, err)
	}
}
