package pipelinerun

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/logger"
)

// lease.go -- which replica drives a run (plan decision 10, design record
// D11).
//
// A run is driven by exactly one agent replica, and the run ROW says which:
// driverNodeId is the replica's MEMQL_NODE_ID and driverHeartbeatAt is when it
// last said it was still there. Nothing else decides it -- not an event, not
// memory -- because the replica that opened a run, the replicas that hear its
// events and the replica that recovers it are different processes.
//
// THE THREE AUTHORITIES, from strongest to cheapest:
//
//   - The gate plus a fresh read (underLease): the claim, the heartbeat and
//     every write to the run row are a read-modify-write under RunGateKey, so
//     a claim and a renewal, or two claims, can never both decide from one
//     stale row. Each is ONE short section -- a fresh read and a write, one
//     gate, nothing nested, no call to GitHub or the runner inside -- because
//     the production gate holds a connection of a small direct pool and gives
//     up acquiring after five seconds (githubconnect.WithGate, driverGate).
//     For the same reason nothing hot takes it: a stage's sixteen steps
//     writing their receipts at once would time each other out.
//   - A fresh read with no gate (stillHolds): before a step's intent or
//     receipt, and before a progress write to GitHub. Another replica can
//     claim only a lease stale for 120 seconds, and this one renews every 30,
//     so the gap between that read and the write is not a window anybody can
//     use.
//   - Memory (lease.isLost): once any of the above has seen another holder,
//     nothing of this drive writes again.
//
// A LOST LEASE STOPS EVERY WRITE AND CANCELS NOTHING. The replica that took
// the run over re-sends each step that has no receipt with the SAME attempt,
// and the runner re-attaches it by (run, step key, attempt); cancelling what is
// in flight here would kill the work it is re-attaching to.

const (
	// leaseRenewEvery is how often a driver renews its lease.
	leaseRenewEvery = 30 * time.Second
	// leaseStaleAfter is how old a lease must be before another replica may
	// take the run over: four missed renewals.
	leaseStaleAfter = 120 * time.Second
	// unclaimedAfter is how long a queued run waits for its created event to
	// be claimed before recovery claims it instead.
	unclaimedAfter = 20 * time.Second
)

// errLeaseLost is a write refused because another replica holds the run now,
// or the run has ended under somebody else's hand.
var errLeaseLost = errors.New("pipelines: this replica no longer holds the run's lease")

// errRunFinished is a write refused because the run has ended under THIS
// replica's own hand: the heartbeat that ticks once more after the conclusion
// has nothing to renew, and nothing was lost.
var errRunFinished = errors.New("pipelines: the run has concluded")

// driveStopped reports an error that is the drive's own end rather than a
// failure to report: the lease lost, or the run concluded.
func driveStopped(err error) bool {
	return errors.Is(err, errLeaseLost) || errors.Is(err, errRunFinished)
}

// driveRegistry is the runs this node drives now. A run is reserved here
// BEFORE it is claimed, so two of this node's own events for one run can
// never start two drives of it, and released only when its drive returns.
type driveRegistry struct {
	mu     sync.Mutex
	active map[string]*lease
	// started counts the drives this node has started and not finished,
	// claimed or not (spawnDrive).
	started sync.WaitGroup
	// aborts are the runs whose drive on this node stopped short, and when
	// recovery may take each up again (recover.go).
	aborts map[string]abortRecord

	// gateMu admits ONE of this node's drives to a gate at a time
	// (driverGate).
	gateMu sync.Mutex
}

// driverGate runs fn under key's gate, for one of this node's drives at a
// time.
//
// The production gate holds a connection of the DIRECT database pool for its
// whole section -- a pool of four, one or two of which an agent's cron
// leaders already hold, whose waiters give up after five seconds -- so a node
// driving many runs takes the gate for one of them at a time, and every
// section the driver runs under it is a fresh read and a write: never a call
// to GitHub or to the runner, and never a second gate.
func (i *Integration) driverGate(ctx context.Context, d Deps, key string, fn func(context.Context) error) error {
	i.drives.gateMu.Lock()
	defer i.drives.gateMu.Unlock()
	return d.gate(ctx, key, fn)
}

// lease is one run this node drives: what stops the drive, and why.
type lease struct {
	runID     string
	stop      chan struct{}
	stopOnce  sync.Once
	lost      atomic.Bool
	cancelled atomic.Bool
}

func newLease(runID string) *lease { return &lease{runID: runID, stop: make(chan struct{})} }

// cancel asks the drive to cancel the run: the runner told to stop, what is
// unfinished recorded cancelled, the run concluded cancelled. A lease already
// lost is not this node's to cancel.
func (l *lease) cancel() {
	if l.lost.Load() {
		return
	}
	l.cancelled.Store(true)
	l.stopOnce.Do(func() { close(l.stop) })
}

// lose stops the drive without one more write: another replica holds the run.
func (l *lease) lose() {
	l.lost.Store(true)
	l.stopOnce.Do(func() { close(l.stop) })
}

func (l *lease) isLost() bool { return l.lost.Load() }

// stoppable is ctx bounded by d AND by the drive's stop -- a cancel or a lost
// lease -- whichever comes first: a call to GitHub never outlives the drive
// that made it, and never waits without end (a tarball client has no timeout
// of its own; its caller's context bounds it).
func (dr *runDriver) stoppable(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	bounded, cancel := context.WithTimeout(ctx, d)
	go func() {
		select {
		case <-dr.lease.stop:
			cancel()
		case <-bounded.Done():
		}
	}()
	return bounded, cancel
}

// isCancelled reports a cancel this drive must carry out: asked, and the
// lease still this node's.
func (l *lease) isCancelled() bool { return l.cancelled.Load() && !l.lost.Load() }

// reserve marks runID as driven by this node, or answers nil when it already
// is.
func (i *Integration) reserve(runID string) *lease {
	r := &i.drives
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		r.active = map[string]*lease{}
	}
	if _, busy := r.active[runID]; busy {
		return nil
	}
	l := newLease(runID)
	r.active[runID] = l
	return l
}

// release ends a reservation.
func (i *Integration) release(l *lease) {
	r := &i.drives
	r.mu.Lock()
	if r.active[l.runID] == l {
		delete(r.active, l.runID)
	}
	r.mu.Unlock()
}

// spawnDrive claims and drives runID in a goroutine of its own, counted, so
// waitForDrives sees it from the moment it is started rather than from the
// moment it is claimed.
func (i *Integration) spawnDrive(runID string) {
	i.drives.started.Add(1)
	go func() {
		defer i.drives.started.Done()
		i.claimAndDrive(runID)
	}()
}

// driving is the lease of a run this node drives now, or nil.
func (i *Integration) driving(runID string) *lease {
	r := &i.drives
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[bareID(runID)]
}

// waitForDrives blocks until every drive this node started has returned, or
// ctx ends. A test's way to know a drive is over; production never waits.
func (i *Integration) waitForDrives(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		i.drives.started.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// claimable reports whether nodeID may take r's lease now: r is not finished,
// and nobody holds it, or nodeID itself does (this replica's previous process:
// the node id is the pod's name, which a restarted container keeps, and a
// run in this process's memory is reserved before it ever gets here), or its
// holder has not renewed it for leaseStaleAfter.
func claimable(r Run, nodeID string, now time.Time) bool {
	if r.Finished() {
		return false
	}
	holder := strings.TrimSpace(r.DriverNodeID)
	if holder == "" || holder == nodeID {
		return true
	}
	return leaseStale(r, now)
}

// leaseStale reports whether r's holder stopped renewing its lease. A holder
// that never wrote a heartbeat holds nothing.
func leaseStale(r Run, now time.Time) bool {
	return r.DriverHeartbeatAt.IsZero() || now.Sub(r.DriverHeartbeatAt) >= leaseStaleAfter
}

// claim takes runID's lease for this node: under the run's gate, a FRESH read
// (this node's result cache may hold a row another replica has since
// claimed), and when claimable the write of driverNodeId and
// driverHeartbeatAt. It answers the claimed row, or false when the run is
// finished, gone, or another replica's.
func (i *Integration) claim(ctx context.Context, d Deps, runID string) (Run, bool, error) {
	var (
		claimed Run
		ok      bool
	)
	err := i.driverGate(ctx, d, RunGateKey(runID), func(gctx context.Context) error {
		r, err := d.Store.RunByID(memql.ContextWithFreshRead(gctx), runID)
		if err != nil || r == nil {
			return err
		}
		now := d.now()
		if !claimable(*r, d.NodeID, now) {
			return nil
		}
		if err := d.Store.UpdateRun(gctx, r.OwnerUserID, r.ID, RunPatch{
			DriverNodeID: ptr(d.NodeID), DriverHeartbeatAt: ptr(now),
		}); err != nil {
			return err
		}
		r.DriverNodeID, r.DriverHeartbeatAt = d.NodeID, now
		claimed, ok = *r, true
		return nil
	})
	return claimed, ok, err
}

// underLease runs fn under the run's gate after a fresh read proves this node
// still holds the lease, handing fn the row as it stands. It answers
// errLeaseLost, and runs nothing, when another replica holds the run or the
// run has ended -- and from then on the lease is lost in memory too.
func (dr *runDriver) underLease(ctx context.Context, fn func(gctx context.Context, current Run) error) error {
	if dr.lease.isLost() {
		return errLeaseLost
	}
	return dr.i.driverGate(ctx, dr.d, RunGateKey(dr.runID), func(gctx context.Context) error {
		r, err := dr.d.Store.RunByID(memql.ContextWithFreshRead(gctx), dr.runID)
		if err != nil {
			return err
		}
		if r == nil || r.DriverNodeID != dr.d.NodeID {
			dr.loseLease(r)
			return errLeaseLost
		}
		if r.Finished() {
			return errRunFinished
		}
		if dr.lease.isLost() {
			return errLeaseLost
		}
		return fn(gctx, *r)
	})
}

// stillHolds is the cheap fence before a write that does not take the gate:
// a fresh read of the run, and false -- with the lease lost from then on --
// when it is no longer this node's. A read that FAILS proves nothing about the
// lease, so it answers true and leaves the verdict to the heartbeat; the write
// it guards most likely fails the same way.
func (dr *runDriver) stillHolds(ctx context.Context) bool {
	if dr.lease.isLost() {
		return false
	}
	r, err := dr.d.Store.RunByID(memql.ContextWithFreshRead(ctx), dr.runID)
	if err != nil {
		return true
	}
	if r == nil || r.DriverNodeID != dr.d.NodeID {
		dr.loseLease(r)
		return false
	}
	// A run this replica concluded is held and over: nothing more to write.
	return !r.Finished()
}

// loseLease records, once, that another replica holds the run.
func (dr *runDriver) loseLease(current *Run) {
	if dr.lease.isLost() {
		return
	}
	holder, status := "", ""
	if current != nil {
		holder, status = current.DriverNodeID, current.Status
	}
	dr.d.Logger.Warn("pipelines: this replica no longer holds the run's lease; it stops driving it and writes nothing more",
		"component", "pipelinerun", logger.Subject(RunConcept, dr.runID), "node", dr.d.NodeID,
		"holder", holder, "status", status)
	dr.lease.lose()
}

// heartbeat renews the lease every HeartbeatEvery until ctx ends, the lease is
// lost, or the run has concluded. It keeps renewing after a cancel and through
// the conclusion: concluding takes a moment -- calls to GitHub among it -- and
// a lease left to go stale there would let another replica take the run over
// mid-conclusion.
func (dr *runDriver) heartbeat(ctx context.Context) {
	t := time.NewTicker(dr.d.HeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if dr.lease.isLost() || !dr.renew(ctx) {
			return
		}
	}
}

// renew is one heartbeat: the gated read-modify-write of driverHeartbeatAt,
// the cancel request read off the same row, and the work run's own beat. It
// answers false when there is nothing left to renew: the lease is lost, or the
// run concluded.
func (dr *runDriver) renew(ctx context.Context) bool {
	cancelAsked := false
	err := dr.underLease(ctx, func(gctx context.Context, current Run) error {
		cancelAsked = current.CancelRequested
		return dr.d.Store.UpdateRun(gctx, current.OwnerUserID, current.ID, RunPatch{DriverHeartbeatAt: ptr(dr.d.now())})
	})
	switch {
	case errors.Is(err, errLeaseLost), errors.Is(err, errRunFinished):
		return false
	case err != nil:
		// Not a lost lease: a gate or a write that failed. The next beat
		// tries again; four in a row and another replica may take over,
		// which the beat after that will see.
		dr.d.Logger.Warn("pipelines: a lease renewal failed; the next one retries",
			"component", "pipelinerun", logger.Subject(RunConcept, dr.runID), "error", err)
		return true
	}
	if cancelAsked {
		dr.lease.cancel()
	}
	dr.work().Heartbeat(ctx)
	return true
}
