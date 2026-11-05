package pipelinerun

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/logger"
)

// push_lane.go -- a push to the default branch waits for the push run ahead of
// it, and only the newest waiting push survives (design record 2026-11-05 D4).
//
// WHY. The owner pushes straight to the default branch, and every push opens a
// full run. A run is claimed and started the moment it opens -- queueing
// happens per step, inside the cluster's queue -- so three pushes in ten
// minutes started three full runs at once, and the newest commit's verdict,
// the one anybody waits on, arrived last, hours later. The lane gives each
// pipeline's pushes ONE runner at a time and drops the commits a newer push
// already covers: the default branch only moves forward, so a newer commit's
// run tests every change an older one carried.
//
// WHO IS IN THE LANE (inPushLane). A `push` run in full mode that a webhook or
// the poll opened -- never a re-run, a qualification, a manual run, a release,
// a schedule or a pull request's run -- AND whose OWN saved policy snapshot
// opts in: its supersession phase (pipelineSupersessionPolicy, the optional
// `supersession` role) answers yes for {event: push, mode: full, previousMode:
// full, trigger}. The portable policy answers no, so nothing changes until an
// installation opts in; and because the answer is the run's own snapshot's, a
// run keeps the rule it was opened under on every replica and across every
// retry, whatever the installation decides later.
//
// WAITING AND EXECUTING. A lane run WAITS while it is queued, never started,
// claimed by nobody and not asked to stop (waitingRun) -- the one state in which
// nothing of it is in flight. A push run of the pipeline HOLDS the lane while
// it is in progress, whatever its lease says (a stranded run is recovery's to
// take over, and holds the lane until it ends), or while it is queued under a
// fresh lease (claimed a moment ago and starting) -- occupiesPushLane. Holding
// is judged on the run's SHAPE, not its opt-in: a push run opened before the
// installation opted in still holds the lane until it finishes, so opting in
// never starts a run beside one already going.
//
// THE FOUR PLACES THE LANE ACTS, all of them through the run's own gate for
// any write:
//
//   - The CLAIM (lease.go). A lane run that has not started is not claimed
//     while another push run of the pipeline holds the lane: no write at all,
//     so it stays queued with no driver and its time limit, counted from its
//     start, has not begun. A run that has STARTED is never held -- a takeover
//     after a lost replica is never blocked -- and neither is a run asked to
//     stop, whose cancel must not wait on anything.
//   - A new PUSH (supersede.go, coalescePush). It supersedes every waiting lane
//     run of the pipeline whose commit GitHub proves a proper ancestor of the
//     new commit, when the new commit is the default branch's head.
//   - RECOVERY, every minute (reconcilePushLane). It supersedes the waiting
//     runs a run at the head covers -- the out-of-order case: an older
//     commit's delivery opened its run after the head's -- and, when the lane
//     is free, starts ONE waiting run per pipeline: the head's, else the
//     newest queued. It never starts one while the lane is held, and logs
//     nothing about it then.
//   - A DRIVE that concludes a lane-shaped run (driver.go, advancePushLane).
//     The replica that finished the run ahead reconciles the lane and starts
//     the next waiting run at once, rather than leaving it a minute for
//     recovery.
//
// WHAT A SUPERSEDE REQUIRES, and what it never does. The default branch's head,
// read from GitHub; a run AT that head that was not asked to stop (the run that
// will test what the superseded commits carried -- a head whose delivery has
// not opened a run yet covers nothing, because that delivery may never come);
// and GitHub's proof (CompareAncestry) that the waiting run's commit is a
// PROPER ancestor of the head. A stale or failed head read, a missing head
// run, or an ancestry that is not proven supersedes nothing, and the log says
// why: a run left waiting costs a runner, a run superseded wrongly loses a
// verdict. GitHub is never asked under a gate: the head and the compares are
// read first, and each waiting run is then concluded by cancelIfWaiting under
// its own gate on a fresh read, only if it is STILL waiting -- a run an agent
// claimed meanwhile is already executing, and finishes. The claim takes the
// same gate, so of a claim and a supersede of one run exactly one wins.
// Force-pushes to the default branch are refused by the installation's
// ruleset, but nothing here relies on it: an ancestry that is not proven
// supersedes nothing, and a superseded commit that becomes the head again runs
// again (R37, supersede.go).
//
// THE LANE IS SOFT, by the stance supersede.go takes for pull requests. Each
// claim takes ITS OWN run's gate, so two waiting runs claimed at the same
// instant on two replicas -- or a waiting run claimed while the run ahead is
// being claimed -- can both read a free lane and both start. That costs a
// runner and nothing else: every run still answers for its own commit. A
// pipeline-wide gate around every claim would make that impossible, and would
// put every claim of the pipeline behind one advisory lock for the length of a
// fresh read; the claim stays on the run's gate.
//
// WHAT SURVIVES A RETRY, A PAUSE OR A REPLICA CHANGE: everything, because the
// lane is the rows. Membership is the run's saved snapshot, waiting is its
// row, the head and the ancestry are asked again every time; nothing about the
// lane lives in one process. A replica that dies holding a lane run leaves a
// run in progress -- which holds the lane until recovery takes it over and
// finishes it -- or a claimed run that never started, whose lease goes stale
// and then holds nothing.
//
// Regression tests: push_lane_test.go (claim, recovery, the hop between
// replicas, the claim-and-supersede race), push_lane_db_test.go (the hold, the
// supersede and the fenced conclusion over real rows and the production gate),
// supersede_test.go (a push superseding, R34 and R37 for pushes, the gate
// order), cancel_test.go (cancelIfWaiting), notify_test.go (a superseded run is
// no previous answer), gate_discipline_test.go (every lane path, walked once).

// pushLaneShaped reports whether r has a lane run's shape: a push to the
// default branch in full mode that a webhook or the poll opened -- not a
// re-run, a qualification, a manual run, a release, a schedule or a pull
// request's run. Shape alone decides whether a run HOLDS the lane; joining it
// takes the opt-in too (inPushLane).
func pushLaneShaped(r Run) bool {
	return r.Event == pipelines.EventPush && r.Mode == pipelines.ModeFull && r.PullRequest <= 0 &&
		(r.Trigger == TriggerWebhook || r.Trigger == TriggerPoll) &&
		r.Qualification == nil && strings.TrimSpace(r.RerunOf) == ""
}

// pushLaneAdmits asks a policy snapshot's supersession phase whether a full
// push opened by trigger joins the lane: the installation's opt-in. A phase
// that errs or answers anything but true is a no.
func pushLaneAdmits(ctx context.Context, policy *workflowhost.Snapshot, trigger string) bool {
	if policy == nil {
		return false
	}
	value, err := runPolicyPhase(ctx, policy, "pipelineSupersessionPolicy", map[string]any{
		"event": string(pipelines.EventPush), "mode": string(pipelines.ModeFull),
		"previousMode": string(pipelines.ModeFull), "trigger": trigger,
	}, workflowhost.Options{})
	allowed, ok := value.(bool)
	return err == nil && ok && allowed
}

// inPushLane reports whether r is a lane member: shaped like one, and opted in
// by its OWN saved policy snapshot. A run with no snapshot, or one that does
// not read, is not: the lane is opt-in, and fails closed to today's behaviour.
// It evaluates the snapshot -- local work, but more than a gated section
// should do -- so callers decide it before taking any gate.
func inPushLane(ctx context.Context, r Run) bool {
	if !pushLaneShaped(r) || r.PolicySnapshot == nil {
		return false
	}
	policy, err := readPipelinePolicy(r.PolicySnapshot)
	if err != nil {
		return false
	}
	return pushLaneAdmits(ctx, policy, r.Trigger)
}

// waitingRun reports whether nobody drives r yet: queued, never started,
// claimed by no agent and not asked to stop. Nothing of it is in flight and
// nobody else would conclude it, so it is the one state a supersede concludes
// on the spot (cancelIfWaiting).
func waitingRun(r Run) bool {
	return r.Status == StatusQueued && r.StartedAt.IsZero() && strings.TrimSpace(r.DriverNodeID) == "" && !r.CancelRequested
}

// notStarted reports whether r is subject to the lane at a claim: queued,
// never started, not asked to stop -- claimed by nobody, or under a lease whose
// holder never started it. A started run is taken over whatever holds the lane,
// and a run asked to stop is claimed only to be concluded, as with admission.
func notStarted(r Run) bool {
	return r.Status == StatusQueued && r.StartedAt.IsZero() && !r.CancelRequested
}

// occupiesPushLane reports whether r holds its pipeline's push lane at now:
// lane-shaped, unfinished, and executing -- in progress, whatever its lease
// says, or queued under a driver whose lease is fresh, because that replica
// claimed it and is starting it.
func occupiesPushLane(r Run, now time.Time) bool {
	if !pushLaneShaped(r) || r.Finished() {
		return false
	}
	switch r.Status {
	case StatusInProgress:
		return true
	case StatusQueued:
		return strings.TrimSpace(r.DriverNodeID) != "" && !leaseStale(r, now)
	}
	return false
}

// pushLaneHolder is the run holding r's push lane at now, read FRESH under the
// gate its caller holds (r's own), or "" when the lane is free. r itself never
// holds its own lane.
func pushLaneHolder(gctx context.Context, d Deps, r Run, now time.Time) (string, error) {
	runs, err := d.Store.RunsUnfinishedForPipelineEvent(memql.ContextWithFreshRead(gctx), r.PipelineID, pipelines.EventPush)
	if err != nil {
		return "", err
	}
	for _, other := range runs {
		if !sameID(other.ID, r.ID) && sameID(other.PipelineID, r.PipelineID) && occupiesPushLane(other, now) {
			return bareID(other.ID), nil
		}
	}
	return "", nil
}

// pushLaneMember reads runID FRESH, with no gate held, and reports whether it
// is a lane member (inPushLane): what a claim needs to know before it takes
// the run's gate, decided there because it evaluates the run's snapshot. A run
// that cannot be read, or is finished, is no member, and the claim's own gated
// read then decides as it always has. Membership rests on fields a run never
// changes after it is opened, so the answer cannot go stale between this read
// and the gate.
func (i *Integration) pushLaneMember(ctx context.Context, d Deps, runID string) bool {
	if d.Store == nil {
		return false
	}
	r, err := d.Store.RunByID(memql.ContextWithFreshRead(ctx), runID)
	if err != nil || r == nil || r.Finished() {
		return false
	}
	return inPushLane(ctx, *r)
}

// ---------------------------------------------------------------------------
// Reconciling a pipeline's lane: recovery and a drive that concluded
// ---------------------------------------------------------------------------

// errNoPushHeadRun is reconcilePushLane's reason to supersede nothing when the
// default branch's head has no run yet.
var errNoPushHeadRun = errors.New("the default branch's head has no push run yet")

// reconcilePushLane supersedes the waiting lane runs of pipelineID that a run
// at the default branch's head covers, and answers the one lane run that may
// start now -- the head's, else the newest queued -- or nil when the lane is
// held or nothing waits. It is called with NO GATE HELD: the lane is read
// fresh, GitHub is asked for the head and the ancestries with no gate held,
// and each supersede takes only its own run's gate (cancelIfWaiting). It
// starts nothing itself; the caller does, through the claim, which decides
// again on the run's own row.
//
// A failure that keeps the head or the head's run from being known supersedes
// nothing and is logged; the run to start is still answered, because whether
// the lane is free is the rows' alone.
func (i *Integration) reconcilePushLane(ctx context.Context, d Deps, pipelineID string, now time.Time) (*Run, error) {
	runs, err := d.Store.RunsUnfinishedForPipelineEvent(memql.ContextWithFreshRead(ctx), pipelineID, pipelines.EventPush)
	if err != nil {
		return nil, err
	}
	held := false
	var candidates, waiting []Run
	for _, r := range runs {
		if !sameID(r.PipelineID, pipelineID) {
			continue
		}
		if occupiesPushLane(r, now) {
			held = true
		}
		if notStarted(r) && inPushLane(ctx, r) {
			candidates = append(candidates, r)
			if waitingRun(r) {
				waiting = append(waiting, r)
			}
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	head, superseded := i.supersedeTowardHead(ctx, d, pipelineID, waiting)
	var survivors []Run
	for _, r := range candidates {
		if !superseded[bareID(r.ID)] {
			survivors = append(survivors, r)
		}
	}
	if held || len(survivors) == 0 {
		return nil, nil
	}
	next := newestLaneRun(survivors, head)
	return &next, nil
}

// supersedeTowardHead reads pipelineID's default branch head and, when a run
// at it covers them, supersedes each of waiting whose commit GitHub proves a
// proper ancestor of it. It answers the head ("" when it could not be read)
// and the bare ids it superseded. Asked only when something waits: with
// nothing waiting there is nothing to supersede and GitHub is asked nothing.
func (i *Integration) supersedeTowardHead(ctx context.Context, d Deps, pipelineID string, waiting []Run) (string, map[string]bool) {
	superseded := map[string]bool{}
	if len(waiting) == 0 {
		return "", superseded
	}
	attrs := []any{"component", "pipelinerun", logger.Subject(PipelineConcept, bareID(pipelineID))}
	p, err := d.Store.PipelineByID(memql.ContextWithFreshRead(ctx), pipelineID)
	switch {
	case err != nil:
		d.Logger.Warn("pipelines: the pipeline of waiting push runs could not be read, so none is superseded", append(attrs, "error", err)...)
		return "", superseded
	case p == nil || !p.Active():
		// A disconnected pipeline's waiting runs are refused by their own drive
		// (driver.go); there is no installation to ask GitHub through.
		return "", superseded
	}
	attrs = append(attrs, "repository", p.Repository)
	token := ""
	if d.GitHub != nil {
		callCtx, done := context.WithTimeout(ctx, githubCallTimeout)
		token, _, err = d.GitHub.InstallationToken(callCtx, p.CredentialID, p.OwnerUserID, p.Repository)
		done()
		if err != nil {
			token = ""
			attrs = append(attrs, "error", err)
		}
	}
	head, err := defaultBranchHead(ctx, d, *p, token)
	if err != nil {
		switch {
		case errors.Is(err, errNoHeadToken):
			d.Logger.Warn("pipelines: there is no installation token to ask GitHub for the default branch's head, so no waiting push run is superseded", attrs...)
		case errors.Is(err, errNoHeadNamed):
			d.Logger.Warn("pipelines: GitHub named no head for the default branch, so no waiting push run is superseded", attrs...)
		default:
			d.Logger.Warn("pipelines: the default branch's head could not be read, so no waiting push run is superseded", append(attrs, "error", err)...)
		}
		return "", superseded
	}
	cover, err := pushRunAtHead(ctx, d, *p, head)
	if err != nil {
		if !errors.Is(err, errNoPushHeadRun) {
			d.Logger.Warn("pipelines: the runs of the default branch's head could not be read, so no waiting push run is superseded",
				append(attrs, "head", head, "error", err)...)
		}
		return head, superseded
	}
	for _, w := range waiting {
		if normalizeSHA(w.SHA) == head || sameID(w.ID, cover.ID) {
			continue
		}
		if i.supersedeIfAncestor(ctx, d, *p, w, cover, head, token) {
			superseded[bareID(w.ID)] = true
		}
	}
	return head, superseded
}

// pushRunAtHead is the run that tests the default branch's head: a push run of
// the pipeline at head that nobody asked to stop -- running, waiting or
// concluded with an answer. A run asked to stop, by a person or a newer push,
// tests nothing, so it covers nothing. errNoPushHeadRun when there is none.
func pushRunAtHead(ctx context.Context, d Deps, p Pipeline, head string) (Run, error) {
	runs, err := d.Store.RunsForPipelineSHA(memql.ContextWithFreshRead(ctx), p.ID, head)
	if err != nil {
		return Run{}, err
	}
	for _, r := range runs { // newest first
		if r.Event == pipelines.EventPush && sameID(r.PipelineID, p.ID) && normalizeSHA(r.SHA) == head &&
			r.Qualification == nil && !r.CancelRequested && r.Conclusion != ConclusionCancelled {
			return r, nil
		}
	}
	return Run{}, errNoPushHeadRun
}

// newestLaneRun is the lane run to start of runs: the one at the default
// branch's head when there is one -- its verdict is the one anybody waits on
// -- else the most recently queued (an older commit whose ancestry was not
// proven, or a head GitHub would not name). Ties fall to the id, so every
// replica answers the same.
func newestLaneRun(runs []Run, head string) Run {
	sorted := slices.Clone(runs)
	slices.SortFunc(sorted, func(a, b Run) int {
		aHead, bHead := head != "" && normalizeSHA(a.SHA) == head, head != "" && normalizeSHA(b.SHA) == head
		switch {
		case aHead != bHead:
			if aHead {
				return -1
			}
			return 1
		case !a.QueuedAt.Equal(b.QueuedAt):
			return b.QueuedAt.Compare(a.QueuedAt)
		}
		return strings.Compare(bareID(a.ID), bareID(b.ID))
	})
	return sorted[0]
}

// recoverPushLanes is recovery's half of the lane: for every pipeline with an
// unstarted lane run among runs, it reconciles the lane and starts the run the
// lane answers, if any and if recovery may claim it now. It answers the bare
// ids of every unstarted lane run, which recovery's ordinary pass must leave
// to the lane: claiming one while the lane is held writes nothing and would be
// retried -- and logged -- every minute.
func (i *Integration) recoverPushLanes(ctx context.Context, d Deps, runs []Run, now time.Time) map[string]bool {
	lane := map[string]bool{}
	var pipelineIDs []string
	for _, r := range runs {
		if !notStarted(r) || !inPushLane(ctx, r) {
			continue
		}
		lane[bareID(r.ID)] = true
		if id := bareID(r.PipelineID); !slices.Contains(pipelineIDs, id) {
			pipelineIDs = append(pipelineIDs, id)
		}
	}
	slices.Sort(pipelineIDs)
	for _, pipelineID := range pipelineIDs {
		next, err := i.reconcilePushLane(ctx, d, pipelineID, now)
		if err != nil {
			d.Logger.Warn("pipelines: the push lane could not be read; recovery tries again next pass",
				"component", "pipelinerun", logger.Subject(PipelineConcept, pipelineID), "error", err)
			continue
		}
		if next == nil || i.driving(next.ID) != nil || !recoverable(*next, d.NodeID, now) || i.backingOff(next.ID, now) {
			continue
		}
		d.Logger.Info("pipelines: the push lane is free; starting its newest waiting run",
			"component", "pipelinerun", logger.Subject(RunConcept, bareID(next.ID)), "sha", next.SHA)
		i.spawnDrive(next.ID)
	}
	return lane
}

// advancePushLane starts the next waiting run of the lane a drive just freed:
// called by the replica that concluded a lane-shaped run (claimAndDrive), with
// no gate held, so the next push starts now rather than at recovery's next
// pass. A lane that cannot be reconciled is left to recovery, which does the
// same within a minute.
func (i *Integration) advancePushLane(ctx context.Context, d Deps, concluded Run) {
	now := d.now()
	next, err := i.reconcilePushLane(ctx, d, concluded.PipelineID, now)
	if err != nil {
		d.Logger.Warn("pipelines: the push lane could not be read after a push run concluded; recovery starts the next one",
			"component", "pipelinerun", logger.Subject(RunConcept, bareID(concluded.ID)), "error", err)
		return
	}
	if next == nil || i.driving(next.ID) != nil || i.backingOff(next.ID, now) {
		return
	}
	d.Logger.Info("pipelines: the push lane is free; starting its newest waiting run",
		"component", "pipelinerun", logger.Subject(RunConcept, bareID(next.ID)), "sha", next.SHA, "after", bareID(concluded.ID))
	i.spawnDrive(next.ID)
}
