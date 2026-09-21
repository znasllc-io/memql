package work

import (
	"context"
	"errors"
	"testing"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// runceilings_test.go -- the guard that reads a goal's ceilings, folds a run's
// spend, and refuses a call past one (memql#5580).

const (
	ceilingRunId  = "v1:work:run:r-ceil"
	ceilingGoalId = "v1:work:goal:g-ceil"
	ceilingOwner  = "u-alice"
)

func newTestCeilings(t *testing.T) (*RunCeilings, *recordingEngine) {
	t.Helper()
	eng := newRecordingEngine()
	c := NewRunCeilings(eng, testLogger())
	c.now = func() time.Time { return testNow }
	return c, eng
}

// aGoalBackedRun is the context a dispatched work run carries.
func aGoalBackedRun() common.RunContext {
	return common.RunContext{
		RunId: ceilingRunId, GoalId: ceilingGoalId, OwnerUserId: ceilingOwner,
		StepKey: "step-a", Mode: common.RunModeLive,
	}
}

// answerRows wires the three reads a seed makes.
func answerRows(eng *recordingEngine, ceilings map[string]any, calls ...map[string]any) {
	eng.reply("workRunForOwner", map[string]any{
		"id": ceilingRunId, "ownerUserId": ceilingOwner, "goalId": ceilingGoalId,
		"status": runStatusRunning, "startedAt": testNow.Format(time.RFC3339Nano),
	})
	goal := map[string]any{"id": ceilingGoalId, "ownerUserId": ceilingOwner}
	if ceilings != nil {
		goal["ceilings"] = ceilings
	}
	eng.reply("workGoalForOwner", goal)
	eng.reply("workModelCallsForOwnerRun", calls...)
}

// -----------------------------------------------------------------------------
// the headline
// -----------------------------------------------------------------------------

// A run past its model-call cap is refused, and the breach names the ceiling
// and the numbers.
func TestAdmitRefusesARunPastItsModelCallCap(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, map[string]any{"maxModelCalls": 2.0})
	rc := aGoalBackedRun()
	ctx := context.Background()

	if b := c.Admit(ctx, rc, 10); b != nil {
		t.Fatalf("a run that has spent nothing must be admitted; got %+v", b)
	}
	c.Charge(ctx, rc, memqlengine.ModelSpend{Served: memqlengine.ServedLive, InputTokens: 5, OutputTokens: 5, Cost: 0.01})
	if b := c.Admit(ctx, rc, 10); b != nil {
		t.Fatalf("one call of two must still be admitted; got %+v", b)
	}
	c.Charge(ctx, rc, memqlengine.ModelSpend{Served: memqlengine.ServedLive, InputTokens: 5, OutputTokens: 5, Cost: 0.01})

	b := c.Admit(ctx, rc, 10)
	if b == nil || b.Ceiling != work.CeilingModelCalls {
		t.Fatalf("the third call must be refused by the model-call cap; got %+v", b)
	}
	if b.Limit != "2 calls" || b.Actual != "2 made" {
		t.Fatalf("the breach must name the numbers: %+v", b)
	}
}

// A run under its ceilings is admitted however many times it is asked.
func TestAdmitLeavesARunUnderItsCeilingsAlone(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, map[string]any{"maxModelCalls": 100.0, "tokenBudget": 100000.0, "costCeiling": 10.0})
	rc := aGoalBackedRun()
	for i := 0; i < 20; i++ {
		if b := c.Admit(context.Background(), rc, 100); b != nil {
			t.Fatalf("call %d was refused under a ceiling it is nowhere near: %+v", i, b)
		}
		c.Charge(context.Background(), rc, memqlengine.ModelSpend{Served: memqlengine.ServedLive, InputTokens: 50, OutputTokens: 50, Cost: 0.01})
	}
}

// ZERO IS UNSET. A goal declaring `maxModelCalls: 0` is unbounded by that
// ceiling, not forbidden from making a call -- the reading that keeps a goal
// with no ceilings runnable rather than dead on arrival.
func TestZeroIsUnsetAllTheWayThroughTheGuard(t *testing.T) {
	for _, name := range []string{"an explicit zero", "no ceilings key at all"} {
		c, eng := newTestCeilings(t)
		if name == "an explicit zero" {
			answerRows(eng, map[string]any{"maxModelCalls": 0.0, "tokenBudget": 0.0, "costCeiling": 0.0, "wallClockMs": 0.0})
		} else {
			answerRows(eng, nil)
		}
		rc := aGoalBackedRun()
		for i := 0; i < 50; i++ {
			if b := c.Admit(context.Background(), rc, 1<<20); b != nil {
				t.Fatalf("%s: an unset ceiling refused a call: %+v", name, b)
			}
			c.Charge(context.Background(), rc, memqlengine.ModelSpend{Served: memqlengine.ServedLive, InputTokens: 1 << 16, Cost: 100})
		}
	}
}

// -----------------------------------------------------------------------------
// the blast radius, and where it fails closed
// -----------------------------------------------------------------------------

// A RUN WITH NO GOAL READS NOTHING. Ceilings live on v1:work:goal, so an
// automation's run -- which has no goal -- inherits none. That is not a silent
// pass: there is nothing declared to evaluate, and this is what keeps the
// guard off every automation run in the cluster.
func TestARunWithNoGoalIsAdmittedWithoutAnyRead(t *testing.T) {
	c, eng := newTestCeilings(t)
	rc := common.RunContext{RunId: "v1:work:run:automation", StepKey: "s", Mode: common.RunModeLive}
	if b := c.Admit(context.Background(), rc, 1<<20); b != nil {
		t.Fatalf("a run with no goal has no ceilings to reach: %+v", b)
	}
	if got := len(eng.recorded()); got != 0 {
		t.Fatalf("%d reads were made for a run that names no goal; the guard must cost an automation run nothing", got)
	}
}

// A CEILING THAT COULD NOT BE READ REFUSES, naming itself `unevaluated`. A
// limit nobody could read and a limit nobody set are the same number, and only
// one of them means unbounded on purpose.
func TestAGoalThatCannotBeReadFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(eng *recordingEngine)
	}{
		{"the goal read fails", func(eng *recordingEngine) {
			answerRows(eng, map[string]any{"maxModelCalls": 5.0})
			eng.refuse("workGoalForOwner", errors.New("the database went away"))
		}},
		{"the goal row is not readable", func(eng *recordingEngine) {
			answerRows(eng, map[string]any{"maxModelCalls": 5.0})
			eng.reply("workGoalForOwner")
		}},
		{"the run row is not readable", func(eng *recordingEngine) {
			eng.reply("workRunForOwner")
		}},
		{"the journal read fails", func(eng *recordingEngine) {
			answerRows(eng, map[string]any{"maxModelCalls": 5.0})
			eng.refuse("workModelCallsForOwnerRun", errors.New("the database went away"))
		}},
	} {
		c, eng := newTestCeilings(t)
		tc.setup(eng)
		b := c.Admit(context.Background(), aGoalBackedRun(), 10)
		if b == nil {
			t.Fatalf("%s: the call was admitted; a ceiling that cannot be evaluated must not silently pass", tc.name)
		}
		if b.Ceiling != work.CeilingUnevaluated {
			t.Fatalf("%s: breach names %q; a refusal must not masquerade as a real ceiling", tc.name, b.Ceiling)
		}
		if b.Reason == "" {
			t.Fatalf("%s: the refusal says nothing a person can act on", tc.name)
		}
	}
}

// A BLIP COSTS ONE CALL, NOT THE RUN. The seed is retried on the next admit,
// so a run is not poisoned by one unreadable moment.
func TestTheSeedIsRetriedAfterATransientFailure(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, map[string]any{"maxModelCalls": 5.0})
	eng.refuse("workGoalForOwner", errors.New("the database went away"))
	rc := aGoalBackedRun()
	if b := c.Admit(context.Background(), rc, 10); b == nil || b.Ceiling != work.CeilingUnevaluated {
		t.Fatalf("the first call must be refused: %+v", b)
	}
	eng.refuse("workGoalForOwner", nil)
	if b := c.Admit(context.Background(), rc, 10); b != nil {
		t.Fatalf("the next call must re-read rather than stay refused: %+v", b)
	}
}

// A GOAL NAMED WITH NO OWNER IS AN ANOMALY AND IS REFUSED. Reading a goal
// under whatever actor happens to be in ctx answers zero rows and no error,
// which presents as "this goal has no ceilings" -- unbounded.
func TestAGoalBackedRunWithNoOwnerIsRefused(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, map[string]any{"maxModelCalls": 5.0})
	rc := aGoalBackedRun()
	rc.OwnerUserId = ""
	b := c.Admit(context.Background(), rc, 10)
	if b == nil || b.Ceiling != work.CeilingUnevaluated {
		t.Fatalf("a goal-backed run with no owner must fail closed: %+v", b)
	}
}

// -----------------------------------------------------------------------------
// re-entry, and what a cached or replayed answer costs
// -----------------------------------------------------------------------------

// RE-ENTRY ACCUMULATES. A resumed or re-dispatched run is the same run id, so
// the seed folds the calls it already made: park, approve, park again is not a
// way to buy another allowance.
func TestSpendAccumulatesAcrossAReEntry(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, map[string]any{"maxModelCalls": 3.0},
		map[string]any{"served": "live", "inputTokens": 10.0, "outputTokens": 10.0, "cost": 0.5},
		map[string]any{"served": "live", "inputTokens": 10.0, "outputTokens": 10.0, "cost": 0.5},
		map[string]any{"served": "live", "inputTokens": 10.0, "outputTokens": 10.0, "cost": 0.5},
	)
	// A fresh process, the same run: three calls are already on the record.
	if b := c.Admit(context.Background(), aGoalBackedRun(), 10); b == nil || b.Ceiling != work.CeilingModelCalls {
		t.Fatalf("a resumed run must continue its first attempt's budget, not start a new one; got %+v", b)
	}
}

// A JOURNAL-SERVED ROW COUNTS AS A CALL AND NOT AS MONEY, on the way back in
// as well as on the way out. Its token counts are the ORIGINAL call's, and the
// model seam already records cost: 0 for one.
func TestAJournaledReplayRowCountsAsACallAndNotAsSpend(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, map[string]any{"tokenBudget": 100.0},
		map[string]any{"served": "journal", "inputTokens": 5000.0, "outputTokens": 5000.0, "cost": 0.0},
	)
	rc := aGoalBackedRun()
	if b := c.Admit(context.Background(), rc, 10); b != nil {
		t.Fatalf("a replayed answer's tokens must not burn the token budget: %+v", b)
	}
	if got := c.Spent(context.Background(), rc); got.ModelCalls != 1 || got.Tokens != 0 {
		t.Fatalf("spent = %+v; a replayed answer is one call and no metered tokens", got)
	}
}

// A LOCAL ANSWER IS COUNTED, SEPARATELY, and does not burn the dollar
// ceilings -- the split this package's whole budget model rests on.
func TestLocalSpendIsCountedAndDoesNotBurnTheDollarCeilings(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, map[string]any{"tokenBudget": 100.0, "costCeiling": 1.0, "maxModelCalls": 10.0})
	rc := aGoalBackedRun()
	for i := 0; i < 5; i++ {
		c.Charge(context.Background(), rc, memqlengine.ModelSpend{Served: memqlengine.ServedLocal, InputTokens: 5000, OutputTokens: 5000})
	}
	if b := c.Admit(context.Background(), rc, 10); b != nil {
		t.Fatalf("MemQL was billed for none of that: %+v", b)
	}
	got := c.Spent(context.Background(), rc)
	if got.TokensLocal != 50000 || got.Tokens != 0 || got.ModelCalls != 5 {
		t.Fatalf("spent = %+v; local tokens are counted in their own bucket", got)
	}
}

// -----------------------------------------------------------------------------
// what this seam cannot evaluate
// -----------------------------------------------------------------------------

// maxRetries and maxEvents are the EXECUTOR's counters and reach no model
// call. Handing CheckCeilings a zero for them would clear them silently on
// every call, so they are cleared here and warned about instead.
func TestTheCeilingsThisSeamCannotEvaluateAreClearedNotSilentlyPassed(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, map[string]any{"maxRetries": 1.0, "maxEvents": 1.0})
	rc := aGoalBackedRun()
	if b := c.Admit(context.Background(), rc, 10); b != nil {
		t.Fatalf("a retry or event cap must not be evaluated against a spend this seam never sees: %+v", b)
	}
	c.mu.Lock()
	b := c.runs[memqlengine.BareShortId(ceilingRunId)]
	c.mu.Unlock()
	if b == nil {
		t.Fatal("no budget was seeded")
	}
	if b.ceilings.MaxRetries != 0 || b.ceilings.MaxEvents != 0 {
		t.Fatalf("the two ceilings this seam cannot see must be cleared rather than checked: %+v", b.ceilings)
	}
	if !b.unbounded {
		t.Fatal("with only those two declared there is nothing left for this seam to check, and it must say so rather than read every run's journal")
	}
}

// The wall clock is measured from the run's own startedAt, not from when this
// process first saw it -- otherwise a re-dispatch resets the clock.
func TestTheWallClockCeilingIsMeasuredFromTheRunsStart(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, map[string]any{"wallClockMs": 60000.0})
	c.now = func() time.Time { return testNow.Add(2 * time.Minute) }
	b := c.Admit(context.Background(), aGoalBackedRun(), 10)
	if b == nil || b.Ceiling != work.CeilingWallClock {
		t.Fatalf("a run two minutes into a one-minute ceiling must be refused: %+v", b)
	}
}

// The counter table is bounded: a long-lived process must not grow one entry
// per run forever. An evicted run re-seeds from its own rows, so eviction
// costs a read rather than an allowance.
func TestTheCounterTableIsBounded(t *testing.T) {
	c, _ := newTestCeilings(t)
	for i := 0; i < maxTrackedRuns+50; i++ {
		c.budgetFor(common.RunContext{RunId: "v1:work:run:r" + itoa(i), GoalId: ceilingGoalId})
	}
	c.mu.Lock()
	n := len(c.runs)
	c.mu.Unlock()
	if n > maxTrackedRuns {
		t.Fatalf("the table holds %d runs; it is bounded at %d", n, maxTrackedRuns)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A REFUSAL RE-READS THE GOAL BEFORE IT STANDS. The budget approval asks the
// person to raise the ceiling and carry on, so a run re-dispatched onto this
// process after they did must meet the NEW number. The ceilings are otherwise
// read once per run, and a stale one would make the approval's own words
// untrue.
func TestARaisedCeilingIsSeenOnTheNextCall(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, map[string]any{"maxModelCalls": 1.0})
	rc := aGoalBackedRun()
	ctx := context.Background()

	c.Charge(ctx, rc, memqlengine.ModelSpend{Served: memqlengine.ServedLive})
	if b := c.Admit(ctx, rc, 10); b == nil || b.Ceiling != work.CeilingModelCalls {
		t.Fatalf("one call against a one-call cap must be refused: %+v", b)
	}

	// The person raises it.
	eng.reply("workGoalForOwner", map[string]any{
		"id": ceilingGoalId, "ownerUserId": ceilingOwner,
		"ceilings": map[string]any{"maxModelCalls": 5.0},
	})
	if b := c.Admit(ctx, rc, 10); b != nil {
		t.Fatalf("a raised ceiling must be seen rather than answered from the seed: %+v", b)
	}
	// ...and the SPEND is not re-read, so the call already made still counts.
	if got := c.Spent(ctx, rc); got.ModelCalls != 1 {
		t.Fatalf("spent = %+v; re-reading the ceilings must not hand the run its allowance back", got)
	}
}

// A GOAL READ THAT FAILS ON THE REFUSAL PATH LEAVES THE REFUSAL STANDING. We
// already have a real answer; replacing it with `unevaluated` would lose which
// ceiling was reached.
func TestAFailedRefreshLeavesTheRealBreachStanding(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, map[string]any{"maxModelCalls": 1.0})
	rc := aGoalBackedRun()
	ctx := context.Background()
	if b := c.Admit(ctx, rc, 10); b != nil {
		t.Fatalf("the seed must succeed first, or this tests the seed's failure instead: %+v", b)
	}
	c.Charge(ctx, rc, memqlengine.ModelSpend{Served: memqlengine.ServedLive})
	eng.refuse("workGoalForOwner", errors.New("the database went away"))

	b := c.Admit(ctx, rc, 10)
	if b == nil || b.Ceiling != work.CeilingModelCalls {
		t.Fatalf("breach = %+v; the answer we already had must survive a failed refresh", b)
	}
}
