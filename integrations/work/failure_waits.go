package work

// failure_waits.go -- the sweep's half of the work spine's failure path
// (epic memql#5127, design D12).
//
// The executor classifies a failed run's symptom and lands the ACT on the run
// as a `waiting` state; this is where the act is served. The split is not
// tidiness: the executor runs on an agent replica and compile, replan and the
// sweeps run on the planner (work-spine design record, section H), so an act
// performed where it was decided would be a call into machinery that node does
// not have, on a run another node owns.
//
// THREE KINDS ARRIVE HERE.
//
//   - `retry` is a due timer plus a re-execution: the symptom said the
//     failure was a blip, the run's retry budget had room, and the run runs
//     the failed step again with the completed prefix served. Every agent
//     arms a timer for it from the wait's own event, and the first to fire
//     serves it under a claim per wait (armRetry, serveRetry); a goal's run is
//     released under a re-run request of its own, so the claim the failing
//     execution still holds on the bare run id does not strand it.
//   - `replan` and `repair` need a REMEDY -- the machinery that invokes
//     replanGap or re-runs a step with the violation as guidance. That lives
//     in the planner, so it arrives here as a seam, served under a claim per
//     wait (handOffRemedy) from the run's own event and from the sweep.
//
// A NODE WITH NO REMEDY LEAVES THEM PARKED, and that is the honest default.
// The alternative -- treating "I cannot remedy this" as "somebody else will"
// -- is how a run reaches `abandoned` while the thing that could have fixed it
// was simply on another replica. A parked run is visible and waits on a state
// a person can read; a planner replica serves it from the run's event.
//
// THE SWEEP IS THE BACKSTOP, WHATEVER NODE LEADS ITS CRON (memql#5664). An
// event is heard only by the replicas that were subscribed when it was
// written -- a pod started since begins at the stream's high watermark -- and
// a timer or a remedy dies with its replica. So a pass serves a wait it can
// serve, and a wait it cannot it ANNOUNCES AGAIN once nobody has served it
// for long enough (nudgeWait): one heartbeat write, whose event reaches every
// agent and planner subscribed now. The general cron leader can be any node
// type; with only "serve it if you can" a wait whose event was missed waited
// for an agent or a planner to lead.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// The wait kinds the failure path writes. They must agree with
// component/automations' constants of the same names; the two sides are in
// different modules and a mismatch is a run waiting for something no sweep
// looks for, which presents as a run that simply never moves.
const (
	waitKindRetry  = "retry"
	waitKindReplan = "replan"
	waitKindRepair = "repair"
)

// RemedyReplan and RemedyRepair name the two remedies to the writes a remedy
// makes back through this package (AskAboutFailedRemedy): the wait kinds they
// serve, exported so the planner does not spell them a second time.
const (
	RemedyReplan = waitKindReplan
	RemedyRepair = waitKindRepair
)

// Remedy is the seam the two acts that need more than a row go through.
// Implemented by integrations/planner, wired from app/ on the planner node.
type Remedy interface {
	// Replan re-plans from the failed step on, KEEPING the completed prefix.
	// Never from the start: the steps that already succeeded are evidence
	// rather than a draft, and re-planning over them is the unguided rerun
	// the debugging literature measured as substantially worse than localized
	// repair. Returns whether it took the run.
	Replan(ctx context.Context, runId, ownerUserId, stepKey, reason string) bool

	// Repair re-runs ONE failed step with the violation as guidance, leaving
	// every other step alone. Returns whether it took the run.
	Repair(ctx context.Context, runId, ownerUserId, stepKey, violation string) bool
}

// SetRemedy installs the remedy seam. A nil remedy is a working state: replan
// and repair waits stay parked and are logged once per pass rather than
// silently dropped.
func (i *Integration) SetRemedy(r Remedy) {
	if i == nil {
		return
	}
	i.mu.Lock()
	i.remedy = r
	i.mu.Unlock()
}

func (i *Integration) remedyRef() Remedy {
	if i == nil {
		return nil
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.remedy
}

// failureWait reads the wait a classified failure wrote. ok is false for every
// other wait shape, so this function decides nothing about approvals, timers or
// the inference park.
func failureWait(run map[string]any) (kind, stepKey, reason, resumeAt string, ok bool) {
	waiting := rowMap(run, "waitingOn")
	if waiting == nil {
		return "", "", "", "", false
	}
	k, _ := waiting["kind"].(string)
	switch trim(k) {
	case waitKindRetry, waitKindReplan, waitKindRepair:
	default:
		return "", "", "", "", false
	}
	subject, _ := waiting["subject"].(string)
	why, _ := waiting["reason"].(string)
	at, _ := waiting["resumeAt"].(string)
	return trim(k), trim(subject), trim(why), trim(at), true
}

// failureRetryDue answers (due, isRetryWait) for a waiting run's waitingOn: a
// classified failure's `retry` wait whose backoff has passed. A retry with no
// readable resumeAt is not due, for timerDue's reason -- a run released on a
// time nobody could read would run early with no way to tell that it had.
// It is the dispatcher's admission (CanDispatchStoredRun) as well as the
// sweep's, so the two cannot disagree about whether a retry is due.
func failureRetryDue(waitingOn map[string]any, now time.Time) (due bool, isRetryWait bool) {
	kind, _, _, resumeAt, ok := failureWait(map[string]any{"waitingOn": waitingOn})
	if !ok || kind != waitKindRetry {
		return false, false
	}
	at, parsed := parseTime(resumeAt)
	if !parsed {
		return false, true
	}
	return !at.After(now), true
}

// serveFailureWait handles one classified-failure wait. It reports whether the
// run was moved, so the caller knows not to consider it for anything else.
// A wait announced again (nudgeWait) is counted as handed back: the pass gave
// it to the cluster rather than leaving it where it was.
func (i *Integration) serveFailureWait(ctx context.Context, run map[string]any, runId, owner string, now time.Time, res *WaitSweepResult) (handled bool) {
	kind, stepKey, reason, resumeAt, ok := failureWait(run)
	if !ok {
		return false
	}
	since := rowString(rowMap(run, "waitingOn"), "since")

	switch kind {
	case waitKindRetry:
		// A retry that carries no resumeAt is not due yet and never will be,
		// which is a bug on the writing side rather than a reason to retry
		// immediately -- retrying at once is what the backoff exists to
		// prevent. The agent's admission asks the same question of the
		// stored row (CanDispatchStoredRun), so the two cannot disagree.
		if due, _ := failureRetryDue(rowMap(run, "waitingOn"), now); !due {
			return true
		}
		if i.dispatcherRef() != nil {
			if i.serveRetry(ctx, runId, owner, since) {
				i.log().Info("work: a failure the rules called transient came due and was handed back to the cluster",
					"component", "work.sweep", "run", runId, "step", stepKey, "reason", reason)
				res.Redispatched++
			}
			return true
		}
		dueAt, _ := parseTime(resumeAt)
		if i.nudgeWait(ctx, run, runId, owner, dueAt, retryNudgeAfter) {
			i.log().Info("work: a retry that came due with no agent serving it was announced to the cluster again",
				"component", "work.sweep", "run", runId, "step", stepKey)
			res.Redispatched++
		}
		return true

	case waitKindReplan, waitKindRepair:
		if i.remedyRef() == nil {
			at, _ := parseTime(since)
			if i.nudgeWait(ctx, run, runId, owner, at, remedyNudgeAfter) {
				i.log().Info("work: a remedy wait no planner has served was announced to the cluster again",
					"component", "work.sweep", "run", runId, "kind", kind, "step", stepKey)
				res.Redispatched++
				return true
			}
			i.log().Info("work: a run is waiting on a remedy this node cannot serve, so it stays parked",
				"component", "work.sweep", "run", runId, "kind", kind, "step", stepKey)
			return true
		}
		if i.handOffRemedy(ctx, kind, runId, owner, rowString(rowMap(run, "waitingOn"), "since")) {
			res.Redispatched++
		}
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Serving a retry: the timer, the claim, and the release
// ---------------------------------------------------------------------------

// retryClaimName is the claim namespace a goal run's retry is released under,
// distinct from the run's dispatch claim so the failing execution's lease on
// the bare run id never holds the release back.
const retryClaimName = "work.run.retry"

// retryReleaseTimeout bounds one release: a fresh read, the versions read and
// one write. It is strictly inside the claim's lease, for remedyTimeout's
// reason: a stalled releaser must not wake after a peer has taken its expired
// claim and released the run under a request of its own.
const retryReleaseTimeout = time.Minute

// retryClaimTTL is the lease on one wait's release.
const retryClaimTTL = retryReleaseTimeout + time.Minute

// retryNudgeAfter is how long a due retry goes unserved before a node that
// cannot serve it announces it again. Every agent subscribed when the wait was
// written armed a timer for the moment it came due and serves it within a
// claim's round trip, so a retry still parked a minute later is one no agent
// holds a timer for; the minute keeps the announcement clear of a release in
// flight.
const retryNudgeAfter = time.Minute

// remedyNudgeAfter is how long a remedy wait goes untouched before a node
// without the remedy announces it again: past remedyClaimTTL, so no planner
// still holds its claim or is writing its remedy -- a claimant gives up at
// remedyTimeout -- and the planner that hears the announcement can take it.
const remedyNudgeAfter = remedyClaimTTL + time.Minute

// armRetry schedules a `retry` wait's serving for the moment its backoff ends,
// once per wait on this replica: the wait's event arrives again with every
// write to the run while it is parked, and each would otherwise arm another
// timer. Every agent subscribed when the wait was written arms its own, and
// serveRetry's claim keeps the retry to whichever fires first; a timer dies
// with its replica, which is the sweep's backstop to cover.
func (i *Integration) armRetry(runId, owner, since string, resumeAt time.Time) {
	key := runId + "#" + since
	if _, armed := i.armedRetries.LoadOrStore(key, struct{}{}); armed {
		return
	}
	delay := resumeAt.Sub(i.clock())
	if delay < 0 {
		delay = 0
	}
	i.after(delay, func() {
		defer i.armedRetries.Delete(key)
		i.serveRetry(context.Background(), runId, owner, since)
	})
}

// after runs f once d has passed: time.AfterFunc, or the scheduler a test
// installed so it can see the delay and choose the moment.
func (i *Integration) after(d time.Duration, f func()) {
	i.mu.RLock()
	schedule := i.schedule
	i.mu.RUnlock()
	if schedule != nil {
		schedule(d, f)
		return
	}
	time.AfterFunc(d, f)
}

// serveRetry serves one due `retry` wait on a replica that executes runs --
// from its timer, or from the sweep on an agent -- and reports whether this
// replica took it.
//
// A GOAL'S RUN IS RELEASED UNDER A REQUEST OF ITS OWN (releaseRetry): the
// failed step and every step after it, as the re-run nobody asked for, which
// the agents claim once under the request from the release's own event. The
// claim the failing execution took at its dispatch -- four minutes on the bare
// run id, against a thirty-second backoff -- does not hold it back, and the
// request rides the row, so the retried execution's own events claim under it
// and cannot start it a second time.
//
// ANY OTHER RUN IS DISPATCHED HERE, under a claim keyed on the wait
// (runClaimKey): a scheduler's journal, which no event dispatches, so nothing
// but this could start it and the claim keeps it to one replica.
func (i *Integration) serveRetry(ctx context.Context, runId, owner, since string) bool {
	if i.dispatcherRef() == nil || strings.TrimSpace(since) == "" {
		return false
	}
	run, err := i.store().runForOwner(ownerActor(memql.ContextWithFreshRead(ctx), owner), runId)
	if err != nil || run == nil || !onRetryWait(run, since, i.clock()) {
		return false
	}
	if retryAsRequest(run) {
		return i.releaseRetry(ctx, runId, owner, since)
	}
	if name := rowString(run, "automationName"); name == "" || name == compilingAutomationName {
		// Nothing to execute, for redispatchStale's reason: the claim would
		// be taken for a run the seam then refuses.
		return false
	}
	return i.dispatchRun(ctx, DispatchRequest{
		RunId: runId, OwnerUserId: owner, GoalId: rowString(run, "goalId"), Status: runStatusWaiting,
		TriggeredBy: rowString(run, "triggeredBy"), Recovery: true, RetryWait: since,
	})
}

// onRetryWait reports a run still parked on the due `retry` wait since names,
// and not asked to stop.
func onRetryWait(run map[string]any, since string, now time.Time) bool {
	if rowString(run, "status") != runStatusWaiting || argBool(run, "cancelRequested") {
		return false
	}
	waiting := rowMap(run, "waitingOn")
	if rowString(waiting, "since") != since {
		return false
	}
	due, retry := failureRetryDue(waiting, now)
	return retry && due
}

// retryAsRequest reports a run whose retry can be a re-run request: one the
// executor runs again on a request (actRun.requireExecutable -- a goal's run
// with a template) whose failed step is one of its own top-level steps.
func retryAsRequest(row map[string]any) bool {
	run := actRun{row: row, order: topLevelOrder(rowStringSlice(row, "stepOrder"))}
	key := topLevelStepKey(rowString(rowMap(row, "waitingOn"), "subject"))
	return key != "" && run.requireExecutable() == nil && run.requireTopLevel(key) == nil
}

// releaseRetry puts a goal's run back to `running` off its due retry, under a
// re-run request on the step it failed at (failureRetryRequest), and reports
// whether this replica wrote it.
//
// THE CLAIM IS PER WAIT AND COMES FIRST. Every agent's timer and the sweep can
// serve the same wait, and two releases are two requests, each claimable once:
// the run executed twice. The key is the run and the wait's `since`, so a later
// failure of the same run is a new claim; the lease outlives the release, so a
// live releaser is never stolen from, and a dead one's wait is released again
// once the lease lapses. After the claim the run is read again, fresh, and only
// a run still parked on THIS wait is released.
func (i *Integration) releaseRetry(ctx context.Context, runId, owner, since string) bool {
	claimer := i.runClaimerRef()
	if claimer == nil {
		i.log().Warn("work: a retry came due but this node has no cross-replica claim; refusing to release it rather than once per replica",
			"component", "work.retry", "run", runId)
		return false
	}
	if !claimer.ClaimWithTTL(ctx, retryClaimName, runId+"#"+since, retryClaimTTL) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retryReleaseTimeout)
	defer cancel()
	run, err := i.store().runForOwner(ownerActor(memql.ContextWithFreshRead(ctx), owner), runId)
	if err != nil || run == nil || !onRetryWait(run, since, i.clock()) {
		return false
	}
	now := i.clock().UTC()
	writeCtx := ownerActor(ctx, owner)
	request, stale, ok := i.failureRetryRequest(writeCtx, run, runId, failureRetry{stepKey: rowString(rowMap(run, "waitingOn"), "subject")}, now)
	if !ok {
		return false
	}
	if err := i.store().updateRun(writeCtx, runId, map[string]any{
		"status":      runStatusRunning,
		"waitingOn":   map[string]any{},
		"heartbeatAt": rfc(now),
		"rerun":       request,
		"staleSteps":  stale,
	}); err != nil {
		i.log().Warn("work: a due retry could not be released",
			"component", "work.retry", "run", runId, "err", err)
		return false
	}
	i.log().Info("work: released a due retry under a request of its own",
		"component", "work.retry", "run", runId, "step", rowString(request, "stepKey"), "request", rowString(request, "requestId"), "node", selfNodeId())
	return true
}

// nudgeWait announces a failure wait to the cluster again, on a node that
// cannot serve it itself, once nobody has served it for `after` since anchor
// (its resumeAt or its since) or since the last announcement: one write of the
// run's heartbeat, whose event reaches every agent and planner subscribed now,
// each serving it as it serves the wait's first event. It reports whether it
// wrote.
//
// The run is read again, fresh, immediately before the write, and announced
// only while it is the same wait the pass read and nobody has touched it since
// -- the write is the engine's read-merge, so the window it leaves to a
// concurrent writer is that one round trip, on a wait nothing has served for
// `after`.
func (i *Integration) nudgeWait(ctx context.Context, run map[string]any, runId, owner string, anchor time.Time, after time.Duration) bool {
	if anchor.IsZero() {
		return false
	}
	last := anchor
	if beat, ok := parseTime(rowString(run, "heartbeatAt")); ok && beat.After(last) {
		last = beat
	}
	now := i.clock().UTC()
	if now.Sub(last) < after {
		return false
	}
	current, err := i.store().runForOwner(ownerActor(memql.ContextWithFreshRead(ctx), owner), runId)
	if err != nil || current == nil || rowString(current, "status") != runStatusWaiting || argBool(current, "cancelRequested") {
		return false
	}
	read, fresh := rowMap(run, "waitingOn"), rowMap(current, "waitingOn")
	if rowString(fresh, "kind") != rowString(read, "kind") || rowString(fresh, "since") != rowString(read, "since") ||
		rowString(current, "heartbeatAt") != rowString(run, "heartbeatAt") {
		return false
	}
	if err := i.store().updateRun(ownerActor(ctx, owner), runId, map[string]any{"heartbeatAt": rfc(now)}); err != nil {
		i.log().Warn("work: could not announce a failure wait again",
			"component", "work.sweep", "run", runId, "err", err)
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Serving a remedy: the claim, the hand-off, and the writes the remedy makes
// ---------------------------------------------------------------------------
//
// A replan or repair wait reaches a remedy two ways, and both take the SAME
// claim. The run row's own `waiting` event is broadcast to every node, so each
// planner replica sees the wait the moment the failure path writes it
// (HandleRunEvent) -- which is what serves it whichever node holds the cron
// lease, since the sweep runs on the general cron leader and that can be any
// node type. The sweep's pass is the backstop for an event nobody heard, on a
// planner that happens to lead.
//
// THE CLAIM IS PER WAIT, AND IT IS WHAT KEEPS A REMEDY'S MODEL CALL TO ONE. A
// replan is a reasoning-level call; two replicas each hearing the event, or a
// sweep pass overlapping the last one (an automation with no @mode is not
// serialized), would each make it and each install a template. The key is the
// run and the wait's `since`, so a later failure of the same run is a new
// claim; the lease outlives remedyTimeout, so a live claimant is never stolen
// from, and a dead one's wait is served again once the lease lapses.

// remedyClaimName is the claim namespace a remedy is served under, distinct
// from the run's dispatch claim so the two never block each other.
const remedyClaimName = "work.run.remedy"

// remedyTimeout bounds one remedy: one replanGap call at the reasoning level,
// Gate 1 and the writes. It is strictly inside the claim's lease, for
// compile's reason: a stalled claimant must not wake after a peer has taken
// its expired claim.
const remedyTimeout = 10 * time.Minute

// remedyClaimTTL is the lease on one wait's remedy.
const remedyClaimTTL = remedyTimeout + 5*time.Minute

// handOffRemedy claims one replan or repair wait and serves it on a detached
// goroutine, reporting whether this replica took it. A replica with no remedy,
// or no claim to arbitrate with, takes nothing: a remedy served unclaimed is
// a model call made once per replica.
func (i *Integration) handOffRemedy(ctx context.Context, kind, runId, owner, since string) bool {
	remedy := i.remedyRef()
	if remedy == nil {
		return false
	}
	claimer := i.runClaimerRef()
	if claimer == nil {
		i.log().Warn("work: a run is waiting on a remedy but this node has no cross-replica claim; refusing to serve it rather than once per replica",
			"component", "work.remedy", "run", runId, "kind", kind)
		return false
	}
	if !claimer.ClaimWithTTL(ctx, remedyClaimName, runId+"#"+since, remedyClaimTTL) {
		return false
	}
	i.log().Info("work: claimed a classified failure for its remedy",
		"component", "work.remedy", "run", runId, "kind", kind, "node", selfNodeId())
	go i.serveRemedy(remedy, kind, runId, owner, since)
	return true
}

// serveRemedy runs one claimed remedy.
//
// IT RE-READS THE RUN AFTER THE CLAIM, as dispatchCompile does: the event or
// the sweep's snapshot was read before arbitration, and the run may since have
// been cancelled, decided or remedied by the claimant before this one. Only a
// run still parked on THIS wait is served.
//
// The model call a replan makes belongs to the RUN: it is journaled on it,
// charged to its ceilings and routed by its owner's choice, exactly as
// compile's calls are. So the context carries the run, under the owner's
// borrowed authority, and its budget scopes.
func (i *Integration) serveRemedy(remedy Remedy, kind, runId, owner, since string) {
	ctx, cancel := context.WithTimeout(context.Background(), remedyTimeout)
	defer cancel()
	run, err := i.remedyRun(ctx, owner, runId, kind)
	if err != nil {
		i.log().Info("work: a claimed remedy found its run no longer waiting on it; nothing to serve",
			"component", "work.remedy", "run", runId, "kind", kind, "reason", err)
		return
	}
	waiting := rowMap(run, "waitingOn")
	if rowString(waiting, "since") != since {
		return
	}
	stepKey, reason := rowString(waiting, "subject"), rowString(waiting, "reason")
	rc := common.RunContext{
		Spine: rowMap(run, "spine"),
		RunId: runId, GoalId: rowString(run, "goalId"), OwnerUserId: owner,
		Mode: rowString(run, "mode"), ReplayPolicy: rowString(run, "replayPolicy"),
		Routing: common.RouteChoiceFrom(run["routing"]),
	}
	if rc.Mode == "" {
		rc.Mode = common.RunModeLive
	}
	ctx = common.ContextWithRun(ownerActor(ctx, owner), rc)
	ctx = memql.ContextWithBudgetScope(ctx, memql.BudgetScopeId("run", runId), memql.BudgetScopeId("goal", rc.GoalId))

	took := false
	if kind == waitKindReplan {
		took = remedy.Replan(ctx, runId, owner, stepKey, reason)
	} else {
		took = remedy.Repair(ctx, runId, owner, stepKey, reason)
	}
	if took {
		i.log().Info("work: a classified failure was handed to its remedy",
			"component", "work.remedy", "run", runId, "kind", kind, "step", stepKey)
	}
}

// ErrRemedyNotWaiting is a remedy write refused because the run is no longer
// parked on the wait the remedy was serving: it was cancelled, decided, or
// moved on while the remedy worked. Its current state stands.
var ErrRemedyNotWaiting = errors.New("work: the run is no longer waiting on this remedy")

// remedyRun re-reads one run under its owner, skipping this node's result
// cache, and returns it only while it is still parked on a failure wait of
// this kind and nobody has asked it to stop. Every remedy write asks it
// before it moves the run.
func (i *Integration) remedyRun(ctx context.Context, owner, runId, kind string) (map[string]any, error) {
	if i == nil {
		return nil, errNoIntegration
	}
	run, err := i.store().runForOwner(ownerActor(memql.ContextWithFreshRead(ctx), owner), runId)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("work: run %s is not readable as %s", runId, owner)
	}
	waiting, _, _, _, ok := failureWait(run)
	if rowString(run, "status") != runStatusWaiting || !ok || waiting != kind || argBool(run, "cancelRequested") {
		return nil, fmt.Errorf("%w (run %s is %s)", ErrRemedyNotWaiting, runId, rowString(run, "status"))
	}
	return run, nil
}

// ReplanTemplate is a re-planned template that passed Gate 1 and was
// persisted the way compile persists a draft: what InstallReplan records on
// the run.
type ReplanTemplate struct {
	AutomationName      string
	TemplateConstructId string
	TemplateFingerprint string
	TemplateVersion     string
	// StepKeys are the new template's top-level step keys, in order.
	StepKeys []string
	// ResumeAt is the first of them the run has not given a finished version:
	// where the install's request starts the run.
	ResumeAt string
	// Outcome is what the re-plan did, for the run's outcome.
	Outcome map[string]any
}

// InstallReplan makes a re-planned template the run's own and hands the run
// back to the cluster (epic memql#5127, design D12).
//
// THE TEMPLATE FIELDS ARE COMPILE'S, written the way WorkCompiler.Compile
// writes them -- automationName, templateConstructId, templateFingerprint,
// templateVersion -- because the executing node loads a run's template from
// exactly those (app/work_template.go), and checks the fingerprint against the
// journal before it resumes.
//
// THE RUN STARTS FROM A REQUEST OF ITS OWN (memql#5664). The install writes a
// `replan` re-run request naming the first step of the new template the run
// has not finished, with the steps from there on stale, and the agents serve it
// exactly as they serve a re-run: under a claim keyed on the request, so the
// claim the failed execution still holds on the bare run id -- four minutes
// from its own dispatch, never released -- does not strand the run; the
// completed prefix served from the run's own rows; the new steps run as their
// first versions. With no request the run was claimed on the bare id, lost to
// that lease on every agent, and was closed as abandoned a minute later.
//
// THE FAILED STEP THE TEMPLATE DROPPED IS SUPERSEDED FIRST. Its row stays
// `failed` otherwise, a resume point for a step no template has; written
// `skipped` before the run moves -- the run's write is what starts it -- the
// journal says the step is behind the run, and a journal with no request and
// no failure in its template stays unresumable, as it must be for a second
// replica claiming from one of the executing replica's events.
func (i *Integration) InstallReplan(ctx context.Context, ownerUserId, runId string, t ReplanTemplate) error {
	if strings.TrimSpace(t.AutomationName) == "" || strings.TrimSpace(t.TemplateConstructId) == "" {
		return fmt.Errorf("work: a re-planned template names no automation or construct")
	}
	stale := staleFrom(t.StepKeys, t.ResumeAt)
	if len(stale) == 0 {
		return fmt.Errorf("work: the re-planned template does not name %q, the step its run resumes at", t.ResumeAt)
	}
	if _, err := i.remedyRun(ctx, ownerUserId, runId, waitKindReplan); err != nil {
		return err
	}
	if err := i.supersedeDroppedSteps(ctx, ownerUserId, runId, t.StepKeys); err != nil {
		return err
	}
	now := i.clock().UTC()
	fields := remedyReopenFields(now)
	fields["rerun"] = rerunRequest(rerunReasonReplan, t.ResumeAt, work.Override{}, map[string]int{}, nil, "", "", now)
	fields["staleSteps"] = stale
	fields["automationName"] = t.AutomationName
	fields["templateConstructId"] = t.TemplateConstructId
	if t.TemplateFingerprint != "" {
		fields["templateFingerprint"] = t.TemplateFingerprint
	}
	if t.TemplateVersion != "" {
		fields["templateVersion"] = t.TemplateVersion
	}
	if len(t.Outcome) > 0 {
		fields["outcome"] = t.Outcome
	}
	return i.store().updateRun(ownerActor(ctx, ownerUserId), runId, fields)
}

// staleFrom is keys from resumeAt on, or nil when keys does not name it.
func staleFrom(keys []string, resumeAt string) []string {
	for n, k := range keys {
		if k == resumeAt && strings.TrimSpace(k) != "" {
			return append([]string(nil), keys[n:]...)
		}
	}
	return nil
}

// supersedeDroppedSteps marks skipped every failed top-level step of the run
// that the new template does not have. The rows are read fresh under the
// owner, as every remedy read is; a nested key belongs to the step that called
// it and goes with that step.
func (i *Integration) supersedeDroppedSteps(ctx context.Context, ownerUserId, runId string, keep []string) error {
	kept := map[string]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	actorCtx := ownerActor(memql.ContextWithFreshRead(ctx), ownerUserId)
	rows, err := i.store().query(actorCtx, "query "+call("workStepsForOwnerRun", map[string]any{"runId": runId}))
	if err != nil {
		return err
	}
	for _, row := range rows {
		key := rowString(row, "key")
		if key == "" || strings.Contains(key, "/") || kept[key] || rowString(row, "status") != "failed" {
			continue
		}
		if err := i.store().writeInternal(ownerActor(ctx, ownerUserId), "mutation "+call("updateWorkStep", map[string]any{
			"stepId":       rowString(row, "id"),
			"status":       "skipped",
			"errorCode":    "replaced_by_replan",
			"errorMessage": "the run was re-planned from this step, and the new plan does not have it",
		})); err != nil {
			return fmt.Errorf("work: supersede step %s of run %s: %w", key, runId, err)
		}
	}
	return nil
}

// remedyReopenFields are reopenFields for a write a remedy makes: everything a
// reopened run must say, except the cancel. The remedy re-read the run and
// refuses one already asked to stop, so writing `cancelRequested: false` could
// only ever erase a cancel that landed AFTER that read -- a goal cancel racing
// the remedy across replicas. Left unwritten, a late cancel survives the
// read-merge and the executor stops the run at its first step boundary.
func remedyReopenFields(now time.Time) map[string]any {
	fields := reopenFields(now)
	delete(fields, "cancelRequested")
	delete(fields, "cancelledBy")
	return fields
}

// RequestRepair carries out a contract miss's repair (spec section E): the
// failed step re-runs with the violation as guidance, the completed prefix is
// served and never runs again, and the run never restarts from the start.
//
// IT IS A RE-RUN REQUEST, the one rerunStep writes (rerun.go), so the agent
// serves it on the path a person's re-run takes: the step runs as a new
// version, and the violation reaches its model calls as "what was wrong with
// the previous version" -- the override's guidance, read at the model seam.
// A deterministic step has no model call to read it, so for one the repair is
// a re-run, which is all such a step can act on. Nobody asked for it, so it
// names no requester and no author.
func (i *Integration) RequestRepair(ctx context.Context, ownerUserId, runId, stepKey, violation string) error {
	row, err := i.remedyRun(ctx, ownerUserId, runId, waitKindRepair)
	if err != nil {
		return err
	}
	run := actRun{row: row, id: runId, owner: ownerUserId, order: topLevelOrder(rowStringSlice(row, "stepOrder"))}
	if err := run.requireExecutable(); err != nil {
		return err
	}
	if err := run.requireTopLevel(stepKey); err != nil {
		return err
	}
	versions, err := i.readVersions(ownerActor(ctx, ownerUserId), runId, run.order)
	if err != nil {
		return err
	}
	plan, err := work.PlanRerun(run.order, versions.byKey, stepKey)
	if err != nil {
		return headRefusal(err)
	}
	override := work.Override{}
	if v := strings.TrimSpace(violation); v != "" {
		override.Guidance = &work.Guidance{Reason: v}
	}
	now := i.clock().UTC()
	fields := remedyReopenFields(now)
	fields["rerun"] = rerunRequest(rerunReasonRerun, stepKey, override, plan.Versions, nil, "", "", now)
	fields["staleSteps"] = plan.Stale
	return i.store().updateRun(ownerActor(ctx, ownerUserId), runId, fields)
}

// AskAboutFailedRemedy parks a run whose remedy could not be carried out on a
// person's question, and is what keeps a failing remedy from looping.
//
// Left on its wait, a replan that failed after its model call would be served
// again by the next pass -- another reasoning-level call, every lease, for
// ever. So once a remedy has spent its attempt, the run moves off the remedy
// wait onto a `feedback` approval naming why, the shape the failure path's own
// questions take (work.FailureApproval, so its approve and its answers can be
// decided): Retry resumes the run from the step it failed at, Abandon stops it.
func (i *Integration) AskAboutFailedRemedy(ctx context.Context, ownerUserId, runId, stepKey, kind, reason string) error {
	run, err := i.remedyRun(ctx, ownerUserId, runId, kind)
	if err != nil {
		return err
	}
	symptom := work.SymptomPlan
	if kind == waitKindRepair {
		symptom = work.SymptomContract
	}
	question := strings.TrimSpace(reason)
	if question == "" {
		question = "The remedy for this failure could not be carried out."
	}
	now := i.clock().UTC()
	req := work.FailureApproval(work.ApprovalKindFeedback, runId, stepKey, symptom, rowString(run, "errorMessage"), question,
		work.Evidence{Tier: "escalate", Reason: question, RuleId: "remedy." + kind, Source: work.EvidenceSourceRules}, now, DefaultApprovalTTL)
	approvalId, err := i.RaiseApproval(ctx, ownerUserId, ApprovalSeed{
		RunId: req.RunId, StepKey: req.StepKey, Kind: req.Kind, Subject: req.Subject, ArtifactHash: req.ArtifactHash,
		Question: req.Question, Options: req.Options, Evidence: evidenceMap(req.Evidence),
		RequestedAt: req.RequestedAt, ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		// No approval, no wait: a run parked on an approval id that does not
		// exist waits on nothing. It stays on its remedy wait, to be served
		// again once the claim lapses.
		return err
	}
	return i.store().updateRun(ownerActor(ctx, ownerUserId), runId, map[string]any{
		"waitingOn": map[string]any{
			"kind": "approval", "subject": approvalId, "approvalKind": work.ApprovalKindFeedback, "since": rfc(now),
		},
	})
}

// ReplanContext is what the replanGap prompt needs and only this package can
// read: the goal in the person's own words, the steps that already succeeded,
// and the step the plan broke at.
//
// THE COMPLETED PREFIX IS THE POINT. The steps that already ran are EVIDENCE,
// not a draft: re-planning from the start is the unguided rerun the debugging
// literature measured as substantially worse than localized repair, and it
// also re-executes side effects that already happened. The prompt is shown
// them as fixed and instructed never to re-emit them.
type ReplanContext struct {
	Statement      string
	CompletedSteps []map[string]any
	FailedStep     map[string]any
	// The sealed source is evidence for the repair, including real argument
	// names, agent ids and private dependencies on another replica.
	TemplateName string
	Template     []memql.SandboxConstruct
	// Recorded is every top-level step the run holds a row for, by key, with
	// its status. It is what a re-planned template is checked against before
	// it is installed: resume serves a step from its row only BEFORE the
	// resume point, so a completed step the new template puts after the first
	// step the run never reached would run a second time.
	Recorded map[string]string
	// Variables are what the run's resume binds into its template's args
	// before any step runs: the run's stored variables, or its triggering
	// event's payload when it has none -- component/automations' ResumeFrom
	// rule. A re-planned template is held to them before it is installed
	// (memql#5664).
	Variables map[string]any
}

// LoadReplanContext reads the context for one run's replan under the OWNER's
// borrowed authority, the way every read in this package does. A run whose
// steps cannot be read yields an error rather than an empty prefix: replanning
// against a prefix that is empty because the read failed would re-emit every
// completed step, which is precisely the failure the prefix exists to prevent.
func (i *Integration) LoadReplanContext(ctx context.Context, ownerUserId, runId, stepKey string) (ReplanContext, error) {
	if i == nil {
		return ReplanContext{}, errNoIntegration
	}
	actorCtx := ownerActor(ctx, ownerUserId)
	st := i.store()

	run, err := st.runForOwner(actorCtx, runId)
	if err != nil {
		return ReplanContext{}, err
	}
	out := ReplanContext{Variables: rowMap(run, "variables")}
	out.TemplateName = rowString(run, "automationName")
	out.Template, err = st.replanTemplate(actorCtx, run)
	if err != nil {
		return ReplanContext{}, err
	}
	if out.Variables == nil {
		out.Variables = rowMap(rowMap(run, "triggerEvent"), "payload")
	}
	if goalId := rowString(run, "goalId"); goalId != "" {
		if goal, gerr := st.goalForOwner(actorCtx, goalId); gerr == nil && goal != nil {
			out.Statement = rowString(goal, "statement")
		}
	}

	steps, err := st.query(actorCtx, "query "+call("workStepsForOwnerRun", map[string]any{"runId": runId}))
	if err != nil {
		return ReplanContext{}, err
	}
	out.Recorded = map[string]string{}
	for _, step := range steps {
		key := rowString(step, "key")
		if key == "" || strings.Contains(key, "/") {
			// A nested key is a statement of a logic a step called, journaled
			// under that step: it belongs to the step's version, is never a
			// step of the template, and is not part of the prefix.
			continue
		}
		out.Recorded[key] = rowString(step, "status")
		// The call is an object ({construct, name}); read as a string it was
		// always empty, and the prompt saw every completed step as a call to
		// nothing.
		entry := map[string]any{
			"key":  key,
			"call": step["call"],
		}
		switch rowString(step, "status") {
		case "done":
			entry["result"] = step["result"]
			out.CompletedSteps = append(out.CompletedSteps, entry)
		case "failed":
			// The named step wins when it is present, so a run with more than
			// one failure replans from the one the classifier judged rather
			// than from whichever the read returned last.
			if out.FailedStep == nil || key == stepKey {
				entry["errorMessage"] = rowString(step, "errorMessage")
				entry["symptom"] = rowString(step, "symptom")
				out.FailedStep = entry
			}
		}
	}
	return out, nil
}

// errNoIntegration is returned rather than a nil-pointer panic when a caller
// holds a nil integration. It is its own value so a test can tell it apart
// from a read failure.
var errNoIntegration = errors.New("work: the integration is nil")
