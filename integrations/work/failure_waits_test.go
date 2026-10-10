package work

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/core/common"
)

// failure_waits_test.go -- serving the failure path's waits (memql#5664):
// the remedy hand-off, across replicas, and the writes a remedy makes back
// through this package.

// pkClaims is the claim table's primary key, shared by every replica handed
// it: the first claim of a (name, key) wins and every later one loses.
type pkClaims struct {
	mu   sync.Mutex
	held map[string]bool
}

func (c *pkClaims) ClaimWithTTL(_ context.Context, name, key string, _ time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held == nil {
		c.held = map[string]bool{}
	}
	k := name + "\x00" + key
	if c.held[k] {
		return false
	}
	c.held[k] = true
	return true
}

// remedyCall is one remedy a replica served, with what its context carried.
type remedyCall struct {
	kind, runId, owner, stepKey, reason string
	run                                 common.RunContext
	actor                               string
	authority                           auth.ForwardedAuthority
	hasAuthority                        bool
}

// signallingRemedy records every remedy it is handed. The hand-off is
// asynchronous, so a test waits on it rather than reading it at once.
type signallingRemedy struct {
	mu    sync.Mutex
	calls []remedyCall
}

func (r *signallingRemedy) record(ctx context.Context, kind, runId, owner, stepKey, reason string) bool {
	call := remedyCall{kind: kind, runId: runId, owner: owner, stepKey: stepKey, reason: reason}
	call.run, _ = common.RunFromContext(ctx)
	call.authority, call.hasAuthority = auth.ForwardedAuthorityFromContext(ctx)
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		call.actor = ac.UserId
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
	return true
}

func (r *signallingRemedy) Replan(ctx context.Context, runId, owner, stepKey, reason string) bool {
	return r.record(ctx, waitKindReplan, runId, owner, stepKey, reason)
}

func (r *signallingRemedy) Repair(ctx context.Context, runId, owner, stepKey, violation string) bool {
	return r.record(ctx, waitKindRepair, runId, owner, stepKey, violation)
}

// settled waits for at least want calls, then a little longer so a second
// replica's duplicate has time to arrive, and returns every call seen. With
// want 0 it only waits out that grace: the hand-off is in-process, so a
// remedy that is going to be served at all is served well inside it.
func (r *signallingRemedy) settled(t *testing.T, want int) []remedyCall {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for want > 0 && time.Now().Before(deadline) {
		r.mu.Lock()
		n := len(r.calls)
		r.mu.Unlock()
		if n >= want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]remedyCall(nil), r.calls...)
}

// signallingDispatcher records what an agent replica was asked to run. The
// event path dispatches on its own goroutine, so a test waits on it.
type signallingDispatcher struct {
	mu   sync.Mutex
	reqs []DispatchRequest
}

func (d *signallingDispatcher) Dispatch(_ context.Context, req DispatchRequest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reqs = append(d.reqs, req)
}

// settled is signallingRemedy.settled's wait, for dispatches.
func (d *signallingDispatcher) settled(t *testing.T, want int) []DispatchRequest {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for want > 0 && time.Now().Before(deadline) {
		d.mu.Lock()
		n := len(d.reqs)
		d.mu.Unlock()
		if n >= want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]DispatchRequest(nil), d.reqs...)
}

const remedyRunId = "v1:work:run:rm1"

// remedyWaitRow is a goal's run the failure path parked on a replan or repair.
func remedyWaitRow(kind string) map[string]any {
	return map[string]any{
		"id": remedyRunId, "ownerUserId": "u-alice", "goalId": "v1:work:goal:g1", "status": runStatusWaiting,
		"automationName": "summarise", "mode": modeLive, "triggeredBy": "api",
		"waitingOn": map[string]any{
			"kind": kind, "subject": "draft", "since": "2026-09-05T11:59:00Z",
			"reason": "the plan is wrong from here on", "ruleId": "",
		},
		"errorMessage": "the summary format the plan assumed does not exist",
	}
}

func remedyEvent(row map[string]any) events.Event {
	return events.Event{Topic: "graph.node.updated.v1:work:run", Payload: map[string]any{"id": row["id"], "payload": row}}
}

// plannerReplica is one planner replica: a remedy, the shared claim table, and
// an engine whose re-read of the run answers row.
func plannerReplica(t *testing.T, remedy Remedy, claims RunClaimer, row map[string]any) (*Integration, *recordingEngine) {
	t.Helper()
	i, eng := newTestIntegration(t)
	i.SetRemedy(remedy)
	i.SetRunClaimer(claims)
	eng.reply("workRunForOwner", row)
	eng.reply("userByIdSystem", map[string]any{"role": "writer"})
	return i, eng
}

func TestRemedyRestoresForwardableAuthorityOnAnotherReplica(t *testing.T) {
	for _, kind := range []string{waitKindReplan, waitKindRepair} {
		for _, tc := range []struct{ name, current, ceiling, want string }{
			{"captured reader ceiling", "owner", "reader", "reader"},
			{"current role was lowered", "reader", "owner", "reader"},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				row := remedyWaitRow(kind)
				row["ownerUserId"] = "v1:identity:user:u-alice"
				row["executionAuthority"] = map[string]any{"roleCeiling": tc.ceiling, "credentialClass": auth.ForwardedClassUser}
				claims, remedy := &pkClaims{}, &signallingRemedy{}
				a, ae := plannerReplica(t, remedy, claims, row)
				b, be := plannerReplica(t, remedy, claims, row)
				ae.reply("userByIdSystem", map[string]any{"role": tc.current})
				be.reply("userByIdSystem", map[string]any{"role": tc.current})
				a.HandleRunEvent(remedyEvent(row))
				b.HandleRunEvent(remedyEvent(row))
				calls := remedy.settled(t, 1)
				if len(calls) != 1 || !calls[0].hasAuthority {
					t.Fatalf("remedy cannot reach a remote worker: %+v", calls)
				}
				// Exercise the receiving node's actual assertion codec and verifier.
				authority := node.ForwardedAuthorityFromProto(node.ForwardedAuthorityToProto(calls[0].authority, "planner", "planner"))
				access, err := auth.VerifyForwardedAuthority(authority, time.Now())
				if err != nil || string(access.Role) != tc.want || access.UserId != row["ownerUserId"] || access.Synthetic || access.Unranked {
					t.Fatalf("received authority: %+v, %v", access, err)
				}
				if calls[0].run.GoalId != "v1:work:goal:g1" {
					t.Fatal("restoring authority lost budget attribution")
				}
			})
		}
	}
}

func TestRemedyRefusesInvalidPersistedAuthorityBeforeInference(t *testing.T) {
	for _, grant := range []map[string]any{
		{"roleCeiling": "owner", "credentialClass": "invented"},
		{"roleCeiling": "reader", "credentialClass": auth.ForwardedClassBadge, "expiresAt": rfc(time.Now().Add(-time.Minute))},
	} {
		row := remedyWaitRow(waitKindReplan)
		row["executionAuthority"] = grant
		remedy := &signallingRemedy{}
		i, eng := plannerReplica(t, remedy, &pkClaims{}, row)
		i.serveRemedy(remedy, waitKindReplan, remedyRunId, "u-alice", rowString(rowMap(row, "waitingOn"), "since"))
		if len(remedy.calls) != 0 || len(mutationsIn(eng)) != 0 {
			t.Fatal("invalid grant reached the remedy or changed the run")
		}
	}
}

// A REMEDY WAIT IS SERVED FROM THE RUN'S OWN EVENT, BY EXACTLY ONE REPLICA
// (memql#5664). SetRemedy had no caller, so no replan or repair wait was ever
// served; wired, the wait would be served only by the sweep -- which runs on
// the general cron leader, any node type -- so a remedy only a planner holds
// would wait for a planner to lead. The run's `waiting` event reaches every
// planner replica; the claim keeps the reasoning-level re-plan to one of them.
func TestARemedyWaitIsServedFromItsEventByExactlyOneReplica(t *testing.T) {
	for _, kind := range []string{waitKindReplan, waitKindRepair} {
		t.Run(kind, func(t *testing.T) {
			claims, remedy := &pkClaims{}, &signallingRemedy{}
			row := remedyWaitRow(kind)
			a, _ := plannerReplica(t, remedy, claims, row)
			b, _ := plannerReplica(t, remedy, claims, row)

			a.HandleRunEvent(remedyEvent(row))
			b.HandleRunEvent(remedyEvent(row))

			calls := remedy.settled(t, 1)
			if len(calls) != 1 {
				t.Fatalf("the %s was served %d times across two replicas, want exactly once: %+v", kind, len(calls), calls)
			}
			got := calls[0]
			if got.kind != kind || got.runId != remedyRunId || got.stepKey != "draft" || got.reason != "the plan is wrong from here on" {
				t.Fatalf("served %+v", got)
			}
			// The remedy's model call belongs to the run, made as its owner.
			if got.run.RunId != remedyRunId || got.run.GoalId != "v1:work:goal:g1" || got.run.OwnerUserId != "u-alice" {
				t.Errorf("the remedy ran under run context %+v; its model calls would be journaled and charged to nothing", got.run)
			}
			if got.actor != "u-alice" {
				t.Errorf("the remedy ran as %q, want the run's owner", got.actor)
			}
		})
	}
}

// THE SWEEP SERVES A REMEDY UNDER THE SAME CLAIM. Two passes that overlap --
// a scheduled automation with no @mode is not serialized, and the cron lease
// can move -- must not each re-plan the run.
func TestASweepServesARemedyOnlyUnderItsClaim(t *testing.T) {
	claims, remedy := &pkClaims{}, &signallingRemedy{}
	row := remedyWaitRow(waitKindReplan)
	a, _ := plannerReplica(t, remedy, claims, row)
	b, _ := plannerReplica(t, remedy, claims, row)

	resA := sweepRows(context.Background(), a, []map[string]any{row}, testNow, time.Minute)
	resB := sweepRows(context.Background(), b, []map[string]any{row}, testNow, time.Minute)

	if calls := remedy.settled(t, 1); len(calls) != 1 {
		t.Fatalf("two sweep passes served the replan %d times, want once: %+v", len(calls), calls)
	}
	if resA.Redispatched+resB.Redispatched != 1 {
		t.Errorf("the passes counted %d hand-offs, want 1", resA.Redispatched+resB.Redispatched)
	}
}

// A CLAIMED REMEDY RE-READS ITS RUN. The event, or the sweep's snapshot, was
// read before the claim; a run cancelled, decided or already remedied since --
// or parked on a LATER failure, whose wait has a different `since` -- is not
// served on the strength of the old read.
func TestAClaimedRemedyIsNotServedOnAWaitThatMovedOn(t *testing.T) {
	moved := []struct {
		name string
		edit func(map[string]any)
	}{
		{"running again", func(r map[string]any) { r["status"] = runStatusRunning }},
		{"cancelled", func(r map[string]any) { r["cancelRequested"] = true }},
		{"parked on a person", func(r map[string]any) {
			r["waitingOn"] = map[string]any{"kind": "approval", "subject": "v1:work:approval:a1"}
		}},
		{"parked on a later failure", func(r map[string]any) { r["waitingOn"].(map[string]any)["since"] = "2026-09-05T12:30:00Z" }},
	}
	for _, tc := range moved {
		t.Run(tc.name, func(t *testing.T) {
			stored := remedyWaitRow(waitKindReplan)
			tc.edit(stored)
			remedy := &signallingRemedy{}
			i, _ := plannerReplica(t, remedy, &pkClaims{}, stored)

			i.HandleRunEvent(remedyEvent(remedyWaitRow(waitKindReplan)))

			if calls := remedy.settled(t, 0); len(calls) != 0 {
				t.Fatalf("a remedy was served on a run that had moved on: %+v", calls)
			}
		})
	}
}

// No claim, no remedy: served unclaimed it would run once per replica.
func TestARemedyWithNoClaimIsNotServed(t *testing.T) {
	remedy := &signallingRemedy{}
	i, eng := newTestIntegration(t)
	i.SetRemedy(remedy)
	row := remedyWaitRow(waitKindReplan)
	eng.reply("workRunForOwner", row)

	i.HandleRunEvent(remedyEvent(row))
	sweepRows(context.Background(), i, []map[string]any{row}, testNow, time.Minute)

	if calls := remedy.settled(t, 0); len(calls) != 0 {
		t.Fatalf("a remedy was served with no claim to arbitrate it: %+v", calls)
	}
}

// ---------------------------------------------------------------------------
// The retry wait, served from its own event and backstopped by any leader
// ---------------------------------------------------------------------------

// retryWaitRow is a goal's run the failure path parked on a retry of its draft
// step, due at resumeAt, thirty seconds after the park.
func retryWaitRow(resumeAt time.Time) map[string]any {
	run := actRunRow(runStatusWaiting, "fetch", "draft")
	run["waitingOn"] = map[string]any{
		"kind": waitKindRetry, "subject": "draft", "since": resumeAt.Add(-30 * time.Second).Format(time.RFC3339),
		"resumeAt": resumeAt.Format(time.RFC3339), "reason": "the far side reported itself temporarily unavailable",
	}
	return run
}

// heldTimers stands in for time.AfterFunc: it keeps every retry timer a
// replica armed, with its delay, for the test to fire.
type heldTimers struct {
	mu     sync.Mutex
	delays []time.Duration
	fns    []func()
}

func (h *heldTimers) schedule(d time.Duration, f func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.delays = append(h.delays, d)
	h.fns = append(h.fns, f)
}

// armed is every delay armed and not yet fired.
func (h *heldTimers) armed() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.delays...)
}

// fire runs every timer armed so far, as though its delay had passed, and
// returns their delays.
func (h *heldTimers) fire() []time.Duration {
	h.mu.Lock()
	fns, delays := h.fns, h.delays
	h.fns, h.delays = nil, nil
	h.mu.Unlock()
	for _, f := range fns {
		f()
	}
	return delays
}

// retryAgent is one agent replica: a dispatcher, the shared claim table, held
// timers, and the run's two recorded versions -- fetch done, draft failed.
func retryAgent(t *testing.T, claims RunClaimer, run map[string]any) (*Integration, *recordingEngine, *signallingDispatcher, *heldTimers) {
	t.Helper()
	i, eng, store := newActsIntegration(t)
	addVersion(store, actRunId, "fetch", 0, 1, "done", nil, nil)
	addVersion(store, actRunId, "draft", 1, 1, "failed", nil, nil)
	d := &signallingDispatcher{}
	i.SetDispatcher(d)
	i.SetRunClaimer(claims)
	timers := &heldTimers{}
	i.schedule = timers.schedule
	eng.reply("workRunForOwner", run)
	return i, eng, d, timers
}

// heldByTheFailingExecution is a claim table in which the execution that
// failed still holds the run's claim on its bare id, as it does for four
// minutes from its dispatch.
func heldByTheFailingExecution(t *testing.T) *pkClaims {
	t.Helper()
	claims := &pkClaims{}
	if !claims.ClaimWithTTL(context.Background(), runClaimName, actRunId, runClaimTTL) {
		t.Fatal("could not stand in for the failing execution's claim")
	}
	return claims
}

// merged is a stored row after a write to it: what the write's event carries.
func merged(row, write map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range row {
		out[k] = v
	}
	for k, v := range write {
		if k != "runId" {
			out[k] = v
		}
	}
	return out
}

// sweepWithVersions is sweepRows for a replica whose version reads must keep
// working: an agent leader releases a goal's retry under a request, and the
// request's versions are read off the step rows.
func sweepWithVersions(t *testing.T, i *Integration, rows ...map[string]any) WaitSweepResult {
	t.Helper()
	i.rowsInFlight = (&stubRowSource{rows: rows}).read
	res, err := i.SweepWaiting(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:maintenance:sweepWaitingWorkRuns", Role: auth.RoleOwner, Unranked: true, Synthetic: true}), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// assertReleasedUnderARequest checks a retry's release: the run back to
// running, off its wait, under a re-run request on the failed step that nobody
// asked for, with the step's next version planned.
func assertReleasedUnderARequest(t *testing.T, release map[string]any) string {
	t.Helper()
	rerun := rowMap(release, "rerun")
	if release["status"] != runStatusRunning || rowString(rerun, "reason") != rerunReasonRerun || rowString(rerun, "stepKey") != "draft" || rowString(rerun, "requestId") == "" {
		t.Fatalf("release = %v, want the run back to running under a re-run request on the failed step", release)
	}
	if waiting := rowMap(release, "waitingOn"); len(waiting) != 0 {
		t.Errorf("the release left the wait %v", waiting)
	}
	if rowInt(rowMap(rerun, "versions"), "draft") != 2 || strings.Join(rowStringSlice(release, "staleSteps"), ",") != "draft" {
		t.Errorf("request versions %v, stale steps %v", rerun["versions"], release["staleSteps"])
	}
	if rowString(rerun, "requestedBy") != "" {
		t.Errorf("requestedBy = %v; nobody asked for a retry", rerun["requestedBy"])
	}
	return rowString(rerun, "requestId")
}

// A DUE RETRY IS SERVED FROM ITS OWN EVENT (memql#5664). The sweep alone
// served it, on the general cron leader: a leader that runs no steps never
// retried anything, and an agent leader retried only once the claim the failing
// execution took at its dispatch had lapsed -- four minutes, against a
// thirty-second backoff. Every agent now arms a timer from the wait's event,
// and the run is released under a re-run request of its own, which the agents
// claim under while the failing execution still holds the bare run id.
func TestADueRetryIsServedFromItsOwnEventDespiteTheFailingExecutionsClaim(t *testing.T) {
	run := retryWaitRow(testNow.Add(-time.Second))
	i, eng, d, timers := retryAgent(t, heldByTheFailingExecution(t), run)

	i.HandleRunEvent(remedyEvent(run))
	if delays := timers.fire(); len(delays) != 1 || delays[0] != 0 {
		t.Fatalf("the wait's event armed timers %v, want one, due now", delays)
	}
	release := argsOf(t, eng, "updateWorkRun")
	requestId := assertReleasedUnderARequest(t, release)

	i.HandleRunEvent(remedyEvent(merged(run, release)))
	if got := d.settled(t, 1); len(got) != 1 || got[0].RerunRequestId != requestId {
		t.Fatalf("the released run was dispatched %+v, want once under its request %s", got, requestId)
	}
}

// The timer is armed for the end of the backoff, once per wait however often
// the wait's event arrives, and the backoff is read again when it fires: a
// timer that fires early releases nothing.
func TestARetryTimerWaitsOutItsBackoff(t *testing.T) {
	run := retryWaitRow(testNow.Add(20 * time.Second))
	i, eng, _, timers := retryAgent(t, &pkClaims{}, run)

	i.HandleRunEvent(remedyEvent(run))
	i.HandleRunEvent(remedyEvent(run))
	if delays := timers.armed(); len(delays) != 1 || delays[0] != 20*time.Second {
		t.Fatalf("armed %v, want one timer for the twenty seconds left of the backoff", delays)
	}
	timers.fire()
	if n := len(eng.callsTo("updateWorkRun")); n != 0 {
		t.Fatalf("a timer that fired inside the backoff wrote %d times", n)
	}

	i.SetNow(func() time.Time { return testNow.Add(21 * time.Second) })
	i.HandleRunEvent(remedyEvent(run))
	timers.fire()
	assertReleasedUnderARequest(t, argsOf(t, eng, "updateWorkRun"))
}

// Every agent arms a timer and every one fires; the claim keeps the release to
// one of them. Two releases would be two requests, each claimable once: the run
// executed twice.
func TestTwoAgentsReleaseADueRetryOnce(t *testing.T) {
	claims := heldByTheFailingExecution(t)
	run := retryWaitRow(testNow.Add(-time.Second))
	a, engA, _, timersA := retryAgent(t, claims, run)
	b, engB, _, timersB := retryAgent(t, claims, run)

	a.HandleRunEvent(remedyEvent(run))
	b.HandleRunEvent(remedyEvent(run))
	timersA.fire()
	timersB.fire()
	if n := len(engA.callsTo("updateWorkRun")) + len(engB.callsTo("updateWorkRun")); n != 1 {
		t.Fatalf("the retry was released %d times across two agents, want once", n)
	}
}

// A run no event dispatches -- a scheduler's journal, with no goal -- is
// dispatched by its retry's serving itself, under a claim keyed on the wait:
// the claim an earlier execution holds on the bare run id does not hold it
// back, and the dispatch names the wait, so the seam admits nothing else.
func TestAGoallessRetryIsDispatchedUnderItsWait(t *testing.T) {
	run := retryWaitRow(testNow.Add(-time.Second))
	delete(run, "goalId")
	i, eng, d, timers := retryAgent(t, heldByTheFailingExecution(t), run)

	i.HandleRunEvent(remedyEvent(run))
	timers.fire()
	since := rowString(rowMap(run, "waitingOn"), "since")
	got := d.settled(t, 1)
	if len(got) != 1 || got[0].RetryWait != since || !got[0].Recovery || got[0].Status != runStatusWaiting || got[0].RerunRequestId != "" {
		t.Fatalf("dispatched %+v, want the waiting run once, for its wait %s", got, since)
	}
	if n := len(eng.callsTo("updateWorkRun")); n != 0 {
		t.Errorf("a goalless retry wrote the run %d times; nothing but its dispatch starts it", n)
	}
}

// An agent leading the sweep serves a due retry itself, as its timer would --
// while the failing execution still holds the bare run id.
func TestAnAgentSweepLeaderServesADueRetryDespiteTheFailingExecutionsClaim(t *testing.T) {
	run := retryWaitRow(testNow.Add(-time.Minute))
	i, eng, _, _ := retryAgent(t, heldByTheFailingExecution(t), run)

	res := sweepWithVersions(t, i, run)
	assertReleasedUnderARequest(t, argsOf(t, eng, "updateWorkRun"))
	if res.Redispatched != 1 {
		t.Errorf("sweep = %+v, want the retry counted as handed back", res)
	}
}

// A LEADER THAT RUNS NO STEPS ANNOUNCES A RETRY NOBODY SERVED (memql#5664).
// The agents subscribed when the wait was written armed timers; a pod started
// since begins at the stream's high watermark and never heard it, and a timer
// dies with its replica. So a sweep on a node that cannot serve the retry
// writes the run's heartbeat once the retry has gone unserved past
// retryNudgeAfter, and the agents serve that write's event as they serve the
// first -- here with the failing execution's claim still held.
func TestANonAgentSweepLeaderAnnouncesARetryNobodyServed(t *testing.T) {
	stranded := retryWaitRow(testNow.Add(-2 * time.Minute))
	leader, leaderEng := newTestIntegration(t)
	leaderEng.reply("workRunForOwner", stranded)

	res := sweepRows(context.Background(), leader, []map[string]any{stranded}, testNow, time.Minute)
	nudge := argsOf(t, leaderEng, "updateWorkRun")
	if len(nudge) != 2 || nudge["heartbeatAt"] != rfc(testNow) {
		t.Fatalf("the announcement wrote %v, want the run's heartbeat and nothing else", nudge)
	}
	if res.Redispatched != 1 {
		t.Errorf("sweep = %+v, want the announced retry counted as handed back", res)
	}

	announced := merged(stranded, nudge)
	agent, agentEng, d, timers := retryAgent(t, heldByTheFailingExecution(t), announced)
	agent.HandleRunEvent(remedyEvent(announced))
	timers.fire()
	release := argsOf(t, agentEng, "updateWorkRun")
	requestId := assertReleasedUnderARequest(t, release)
	agent.HandleRunEvent(remedyEvent(merged(announced, release)))
	if got := d.settled(t, 1); len(got) != 1 || got[0].RerunRequestId != requestId {
		t.Fatalf("the announced retry was dispatched %+v, want once under its request", got)
	}

	for name, row := range map[string]map[string]any{
		"a retry its agents may still be serving": retryWaitRow(testNow.Add(-10 * time.Second)),
		"a retry announced inside the window":     merged(stranded, map[string]any{"heartbeatAt": rfc(testNow.Add(-30 * time.Second))}),
	} {
		t.Run(name, func(t *testing.T) {
			leader, eng := newTestIntegration(t)
			eng.reply("workRunForOwner", row)
			sweepRows(context.Background(), leader, []map[string]any{row}, testNow, time.Minute)
			if n := len(eng.callsTo("updateWorkRun")); n != 0 {
				t.Fatalf("the sweep wrote the run %d times", n)
			}
		})
	}
}

// A LEADER WITHOUT THE REMEDY ANNOUNCES A REMEDY WAIT NOBODY SERVED
// (memql#5664). The planners subscribed when the wait was written heard it; a
// planner that claimed it and died mid-remedy leaves it parked with no second
// event, and only a planner leading the sweep took it up again. Past
// remedyNudgeAfter no planner can still hold the claim, so the sweep writes
// the run's heartbeat and a planner serves that write's event.
func TestANonPlannerSweepLeaderAnnouncesAStrandedRemedy(t *testing.T) {
	stranded := remedyWaitRow(waitKindReplan)
	stranded["waitingOn"].(map[string]any)["since"] = rfc(testNow.Add(-remedyNudgeAfter - time.Minute))
	leader, eng := newTestIntegration(t)
	eng.reply("workRunForOwner", stranded)

	res := sweepRows(context.Background(), leader, []map[string]any{stranded}, testNow, time.Minute)
	nudge := argsOf(t, eng, "updateWorkRun")
	if len(nudge) != 2 || nudge["heartbeatAt"] != rfc(testNow) {
		t.Fatalf("the announcement wrote %v, want the run's heartbeat and nothing else", nudge)
	}
	if res.Redispatched != 1 {
		t.Errorf("sweep = %+v, want the announced remedy counted as handed back", res)
	}

	announced := merged(stranded, nudge)
	remedy := &signallingRemedy{}
	planner, _ := plannerReplica(t, remedy, &pkClaims{}, announced)
	planner.HandleRunEvent(remedyEvent(announced))
	if calls := remedy.settled(t, 1); len(calls) != 1 || calls[0].kind != waitKindReplan {
		t.Fatalf("the announced remedy was served %+v, want once", calls)
	}

	t.Run("a remedy a planner may still be serving", func(t *testing.T) {
		recent := remedyWaitRow(waitKindReplan)
		recent["waitingOn"].(map[string]any)["since"] = rfc(testNow.Add(-5 * time.Minute))
		leader, eng := newTestIntegration(t)
		eng.reply("workRunForOwner", recent)
		sweepRows(context.Background(), leader, []map[string]any{recent}, testNow, time.Minute)
		if n := len(eng.callsTo("updateWorkRun")); n != 0 {
			t.Fatalf("the sweep wrote a remedy wait a live claimant may hold %d times", n)
		}
	})
}
