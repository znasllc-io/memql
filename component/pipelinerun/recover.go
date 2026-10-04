package pipelinerun

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
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
//
// A CONCLUDED RUN WHOSE FINAL REPORT DID NOT LAND is recovery's too
// (republishFinalCheckRuns): its row says checkRunState `unavailable`, and its
// check run on GitHub still shows it unfinished, which holds a merge on a
// required check. Recovery republishes the final report of every such run
// concluded within republishWindow, under the same backoff when it fails
// again.

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
	return i.republishFinalCheckRuns(ctx, d)
}

const (
	// republishWindow is how far back recovery republishes a concluded run's
	// final check run: a day. A required check stuck longer than that has
	// been noticed by a person, and the read must stay bounded.
	republishWindow = 24 * time.Hour
	// republishPageLimit is pipelineRunsFinalCheckRunUnavailable's page.
	republishPageLimit = 100
)

// republishFinalCheckRuns writes, again, the final check run of every run
// concluded within republishWindow whose final report did not land, and
// records `written` -- or, on a 403, `refused` and its note -- once GitHub
// answers. Everything GitHub is asked is asked with NO GATE HELD (one token
// per pipeline, minted once a pass, then each run's write); the answer is
// recorded under the run's gate as a fresh read and one write. A republish
// that fails again leaves the row `unavailable` and the run alone for a
// while, longer each time (abortBackoff). It answers an error only when the
// runs could not be read; one run's failure is said and does not stop the
// others.
func (i *Integration) republishFinalCheckRuns(ctx context.Context, d Deps) error {
	now := d.now()
	runs, err := d.Store.RunsFinalCheckRunUnavailable(memql.ContextWithFreshRead(ctx), now.Add(-republishWindow))
	if err != nil {
		return err
	}
	if len(runs) >= republishPageLimit {
		d.Logger.Warn("pipelines: recovery read a full page of runs whose final check run did not land; any beyond it wait for the next pass",
			"component", "pipelinerun", "runs", len(runs))
	}
	type grant struct {
		p     *Pipeline
		token string
		err   error
	}
	grants := map[string]grant{}
	gate := func(ctx context.Context, key string, fn func(context.Context) error) error {
		return i.driverGate(ctx, d, key, fn)
	}
	for _, r := range runs {
		if !r.Finished() || r.CheckRunState != CheckRunUnavailable || i.driving(r.ID) != nil || i.backingOff(r.ID, now) {
			continue
		}
		log := d.Logger.With("component", "pipelinerun", logger.Subject(RunConcept, r.ID))
		g, seen := grants[bareID(r.PipelineID)]
		if !seen {
			p, err := d.Store.PipelineByID(ctx, r.PipelineID)
			switch {
			case err != nil:
				g.err = err
			case p != nil:
				callCtx, done := context.WithTimeout(ctx, githubCallTimeout)
				g.p = p
				g.token, g.err = checkRunToken(callCtx, d, *p)
				done()
			}
			grants[bareID(r.PipelineID)] = g
		}
		if g.p == nil && g.err == nil {
			continue // the pipeline is gone: there is no repository to report to
		}
		if g.p == nil {
			log.Warn("pipelines: a final check run could not be republished: its pipeline could not be read", "error", g.err)
			i.noteAbort(bareID(r.ID), now)
			continue
		}
		steps, err := finalStepReports(memql.ContextWithFreshRead(ctx), d, r)
		if err != nil {
			log.Warn("pipelines: a final check run could not be republished: its steps could not be read", "error", err)
			i.noteAbort(bareID(r.ID), now)
			continue
		}
		callCtx, done := context.WithTimeout(ctx, githubCallTimeout)
		w := writeCheckRun(callCtx, d, *g.p, r, ReportFor(*g.p, r, steps), g.token, g.err)
		done()
		if w.Err != nil {
			log.Warn("pipelines: a concluded run's final check run was not republished; recovery tries again later",
				"checkRunState", w.State, "error", w.Err)
			i.noteAbort(bareID(r.ID), now)
		} else {
			i.forgetAborts(bareID(r.ID))
		}
		if _, changed := finalCheckRunPatch(w, r); !changed {
			continue
		}
		if err := recordFinalCheckRun(ctx, d, gate, r.ID, w); err != nil {
			log.Warn("pipelines: a republished check run's state was not recorded", "error", err)
		}
	}
	return nil
}

// finalStepReports is a concluded run's steps as its final check run reports
// them, rebuilt from its work run's step rows in plan order: their receipts
// are what its drive wrote, already masked. A step's log tail lived only in
// that drive's memory, so a republished report carries each step's outcome,
// code and message, and no output excerpt. A run that never opened work -- a
// refusal, a cancel before any agent claimed it, nothing to run -- has none.
func finalStepReports(ctx context.Context, d Deps, r Run) ([]pipelines.StepReport, error) {
	if strings.TrimSpace(r.WorkRunID) == "" {
		return nil, nil
	}
	rows, err := d.Store.WorkSteps(ctx, r.WorkRunID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].Seq < rows[b].Seq })
	out := make([]pipelines.StepReport, 0, len(rows))
	for _, row := range rows {
		stage := row.Stage
		if stage == "" {
			stage, _, _ = strings.Cut(row.Key, ".")
		}
		s := StepState{Key: row.Key, Stage: stage, Name: row.Name, Status: StepPending}
		if row.Finished() {
			s = stateFromRow(s, row)
		}
		out = append(out, s.Report())
	}
	return out, nil
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
