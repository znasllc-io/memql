package pipelinerun

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/logger"
)

// recover.go -- runs nobody is driving are driven anyway (plan decision 10,
// design record D11).
//
// A queued run is normally claimed the moment its created event reaches an
// agent replica. Two things can leave a run undriven: no agent heard the
// event (none was up, or a forward was lost), and the agent driving it
// stopped (a rollout, an eviction, a crash). The every-minute poll calls
// RecoverRuns (Deps.Recover) on the agent replica its cron lease placed it on,
// which reads every unfinished run and claims the ones nobody is driving:
//
//   - a QUEUED run with no driver, queued at least unclaimedAfter (20 s) ago
//     -- younger ones still belong to their own event;
//   - a run whose driver has not renewed its lease for leaseStaleAfter (120
//     s): four missed heartbeats;
//   - a run whose lease names THIS replica while no drive of it runs here --
//     the same pod's previous process, whose lease no heartbeat will renew.
//
// Each claim is the same gated claim an event makes, so a recovery and an
// event, or two replicas recovering, still drive a run exactly once.
//
// A RUN WHOSE DRIVE STOPPED SHORT HERE WAITS before it is taken up again, and
// longer each time (abortBackoff): a write the run cannot go on without that
// fails every time -- a refusal, not an outage -- must not cost a tarball
// download every minute for ever. The wait is this node's memory; a restart
// forgets it, which costs one more try.

// abortRecord is a run whose drive on this node stopped short.
type abortRecord struct {
	count int
	until time.Time
}

// abortBackoff is how long recovery leaves a run alone after its n-th drive
// in a row stopped short on this node: a minute, doubling, at most half an
// hour.
func abortBackoff(n int) time.Duration {
	wait := time.Minute
	for ; n > 1 && wait < 30*time.Minute; n-- {
		wait *= 2
	}
	return min(wait, 30*time.Minute)
}

// noteAbort records a drive of runID that stopped short at now.
func (i *Integration) noteAbort(runID string, now time.Time) {
	r := &i.drives
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.aborts == nil {
		r.aborts = map[string]abortRecord{}
	}
	rec := r.aborts[runID]
	rec.count++
	rec.until = now.Add(abortBackoff(rec.count))
	r.aborts[runID] = rec
}

// forgetAborts clears runID's record once a drive of it concluded.
func (i *Integration) forgetAborts(runID string) {
	r := &i.drives
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.aborts, runID)
}

// backingOff reports whether recovery should leave runID alone at now.
func (i *Integration) backingOff(runID string, now time.Time) bool {
	r := &i.drives
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.aborts[bareID(runID)]
	return ok && now.Before(rec.until)
}

// RecoverRuns claims and drives, each in its own goroutine, every unfinished
// run nobody is driving. It answers once the claims are started; a drive
// takes as long as its run. On a node that does not drive it does nothing; on
// one set to drive that lacks a port it needs, it says which.
func (i *Integration) RecoverRuns(ctx context.Context) error {
	d := i.snapshot()
	if !d.Drive {
		return nil
	}
	if missing := d.cannotDrive(); missing != "" {
		return fmt.Errorf("pipelines: this node is set to drive runs and cannot recover any: %s", missing)
	}
	runs, err := d.Store.RunsUnfinished(memql.ContextWithFreshRead(ctx))
	if err != nil {
		return err
	}
	now := d.now()
	for _, r := range runs {
		if i.driving(r.ID) != nil || !recoverable(r, d.NodeID, now) || i.backingOff(r.ID, now) {
			continue
		}
		d.Logger.Info("pipelines: recovering a run nobody is driving",
			"component", "pipelinerun", logger.Subject(RunConcept, bareID(r.ID)),
			"status", r.Status, "holder", r.DriverNodeID)
		i.spawnDrive(r.ID)
	}
	if len(runs) >= pollPageLimit {
		d.Logger.Warn("pipelines: recovery read a full page of unfinished runs; any beyond it wait for the next pass",
			"component", "pipelinerun", "runs", len(runs))
	}
	return nil
}

// recoverable reports whether recovery on nodeID should claim r now.
func recoverable(r Run, nodeID string, now time.Time) bool {
	if r.Finished() {
		return false
	}
	switch holder := strings.TrimSpace(r.DriverNodeID); {
	case holder == "":
		// In progress with nobody holding it is stranded whatever its age; a
		// queued run gets its event's chance first.
		return r.Status != StatusQueued || r.QueuedAt.IsZero() || now.Sub(r.QueuedAt) >= unclaimedAfter
	case holder == nodeID:
		return true
	default:
		return leaseStale(r, now)
	}
}

// SignalCancel tells this node's drive of runID, if there is one, that the
// run was asked to stop: RequestCancel's hook (Deps.SignalCancel), so a run
// cancelled through this node stops now rather than at its next heartbeat.
func (i *Integration) SignalCancel(_ context.Context, runID string) {
	if l := i.driving(runID); l != nil {
		l.cancel()
	}
}
