package automations

// journal_scheduled_hold_db_test.go -- a scheduled firing's held journal against
// a real Postgres (journal.go header; storage growth stability release,
// 2026-10-11).
//
// journal_scheduled_hold_test.go pins WHEN the executor writes, through a
// recorder, with a step that reports its own write to the observer by hand. A
// recorder cannot say whether the engine ever makes that report to the observer
// the executor installed, or whether it accepts the run and the steps held
// behind it once they are written late. This asserts both: a step makes a REAL
// graph write through the engine, and the run it opens reads back, through the
// same read resume uses, as one run with every one of its steps; a firing that
// writes nothing leaves nothing to read; a firing that fails leaves its failure.
//
// Postgres-gated: skips cleanly when no DB is reachable, FAILS under
// MEMQL_REQUIRE_DB=1.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// holdDBProbe is dbLogicProbe (logic_journal_db_test.go) with one more verb: a
// step named "fail" fails. Its "write" is a real engine write made at the step's
// own context, which is the write the executor's observer has to hear.
type holdDBProbe struct{ *dbLogicProbe }

func (p holdDBProbe) Execute(ctx context.Context, step *Step, stepCtx *StepContext) (*StepResult, error) {
	if step.Function != nil && step.Function.Name == "fail" {
		now := time.Now()
		return &StepResult{StepId: step.ID, Status: "failed", Error: errBoom.Error(), StartedAt: now, CompletedAt: now}, errBoom
	}
	return p.dbLogicProbe.Execute(ctx, step, stepCtx)
}

// dbScheduledTick is a cron-shaped automation whose steps s0, s1, ... call the
// named builtins of the probe. The name is stamped so a rerun against the same
// database never reads an earlier run's rows.
func dbScheduledTick(name string, calls ...string) *Automation {
	a := &Automation{Name: fmt.Sprintf("%s%d", name, time.Now().UnixNano()), Schedule: "0 */10 * * * *"}
	for i, call := range calls {
		a.Steps = append(a.Steps, &Step{ID: fmt.Sprintf("s%d", i), Type: StepTypeFunction, Function: &FunctionStepConfig{Name: call, Kind: "query"}})
	}
	return a
}

// stepStatuses reads a run's step rows by key, newest version of each.
func stepStatuses(t *testing.T, engine *memql.MemQLEngine, runId string) map[string]string {
	t.Helper()
	call, err := journalArgs("workStepsForRun", map[string]any{"runId": runId})
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Execute(journalContext(context.Background()), "query "+call)
	if err != nil {
		t.Fatalf("read the step rows of %s: %v", runId, err)
	}
	status := map[string]string{}
	for _, row := range memql.MaterializeRows(res) {
		status[stringField(row, "key")] = stringField(row, "status")
	}
	return status
}

// TestScheduledJournal_DB runs this file's cases on the package's shared engine
// (memql#5668): none installs a logic runner, registers a function or mounts a
// fixture, which is the borrowing rule (journal_db_test.go).
func TestScheduledJournal_DB(t *testing.T) {
	engine := sharedJournalEngine(t)
	newExecutor := func(t *testing.T) *Executor {
		e := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: holdDBProbe{&dbLogicProbe{engine: engine}}})
		t.Cleanup(e.Close)
		return e
	}

	// The 93%: a tick that only reads. No run, no step, no heartbeat.
	t.Run("AFiringThatOnlyReadsLeavesNoRun", func(t *testing.T) {
		exec, err := newExecutor(t).Execute(context.Background(), dbScheduledTick("idleScheduledTick", "readA", "readB"), "schedule")
		if err != nil || exec.Status != "completed" {
			t.Fatalf("status %q, err %v", exec.Status, err)
		}
		if _, err := LoadRunJournal(context.Background(), engine, exec.ID); !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("an idle scheduled firing's run: %v, want none", err)
		}
		if rows := stepStatuses(t, engine, exec.ID); len(rows) != 0 {
			t.Fatalf("an idle scheduled firing left step rows: %v", rows)
		}
	})

	// The rest: a tick that changes something is journaled whole, including the
	// step that read before the write and the one that read after it.
	t.Run("AFiringThatWritesLeavesItsWholeRun", func(t *testing.T) {
		tick := dbScheduledTick("writingScheduledTick", "readA", "write", "readB")
		exec, err := newExecutor(t).Execute(context.Background(), tick, "schedule")
		if err != nil || exec.Status != "completed" {
			t.Fatalf("status %q, err %v", exec.Status, err)
		}
		journal, err := LoadRunJournal(context.Background(), engine, exec.ID)
		if err != nil {
			t.Fatalf("the firing's run: %v -- a real write beneath the executor did not open it", err)
		}
		if journal.AutomationName != tick.Name || journal.Status != "succeeded" || journal.TriggeredBy != "schedule" {
			t.Fatalf("run = %q %q triggered by %q, want %q succeeded by schedule", journal.AutomationName, journal.Status, journal.TriggeredBy, tick.Name)
		}
		if len(journal.StepOrder) != 3 {
			t.Errorf("step order %v, want all three steps", journal.StepOrder)
		}
		statuses := stepStatuses(t, engine, exec.ID)
		for _, key := range []string{"s0", "s1", "s2"} {
			if statuses[key] != "done" {
				t.Errorf("step %s is %q, want a done row (rows: %v)", key, statuses[key], statuses)
			}
		}
	})

	// A failure with no write is still a record: the run, the step that failed
	// and the failed close.
	t.Run("AFiringThatFailsLeavesItsFailure", func(t *testing.T) {
		tick := dbScheduledTick("failingScheduledTick", "readA", "fail")
		exec, _ := newExecutor(t).Execute(context.Background(), tick, "schedule")
		if exec.Status != "failed" {
			t.Fatalf("status = %q, want failed", exec.Status)
		}
		journal, err := LoadRunJournal(context.Background(), engine, exec.ID)
		if err != nil {
			t.Fatalf("the failed firing's run: %v -- a failure must open a held run", err)
		}
		if journal.Status != "failed" || journal.FailedStep != "s1" {
			t.Fatalf("run = %q failed at %q, want failed at s1", journal.Status, journal.FailedStep)
		}
		statuses := stepStatuses(t, engine, exec.ID)
		if statuses["s0"] != "done" || statuses["s1"] != "failed" {
			t.Errorf("step rows %v, want s0 done and s1 failed", statuses)
		}
	})
}
