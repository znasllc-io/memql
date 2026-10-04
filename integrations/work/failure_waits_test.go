package work

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
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
	return i, eng
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
