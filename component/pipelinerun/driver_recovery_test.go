package pipelinerun

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
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
// it up again, without waiting for the lease to go stale.
func TestADriveThatStopsShortIsTakenUpAgainByItsOwnNode(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	var refusing atomic.Bool
	refusing.Store(true)
	dh.integ.Configure(func(d *Deps) {
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

	refusing.Store(false)
	if _, err := dh.integ.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	waitDrives(t, dh.integ)
	if got, _ := dh.store.run(run.ID); got.Status != StatusCompleted || got.Conclusion != ConclusionSuccess || got.DriverNodeID != "agent-a" {
		t.Errorf("recovered by its own node and driven to the end: %s/%s by %q", got.Status, got.Conclusion, got.DriverNodeID)
	}
}
