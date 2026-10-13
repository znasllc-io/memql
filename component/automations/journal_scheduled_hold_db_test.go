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

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
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

// lateWriteProbe adds the verb "lateWrite" to holdDBProbe: it takes its time,
// makes a real engine write, and reads the run back the moment that write
// returns. By then the write observer has opened the run and replayed what it
// held, so what the read finds is what the work sweep would find of the run.
type lateWriteProbe struct {
	holdDBProbe
	delay      time.Duration
	releasedAt time.Time
	seen       *RunJournal
	seenErr    error
}

func (p *lateWriteProbe) Execute(ctx context.Context, step *Step, stepCtx *StepContext) (*StepResult, error) {
	if step.Function == nil || step.Function.Name != "lateWrite" {
		return p.holdDBProbe.Execute(ctx, step, stepCtx)
	}
	time.Sleep(p.delay)
	write := *step
	fn := *step.Function
	fn.Name = "write"
	write.Function = &fn
	p.releasedAt = time.Now()
	res, err := p.holdDBProbe.Execute(ctx, &write, stepCtx)
	if run, ok := common.RunFromContext(ctx); ok {
		p.seen, p.seenErr = LoadRunJournal(context.Background(), p.engine, run.RunId)
	}
	return res, err
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

	// The sweep judges a running run by its newest heartbeat and hands one it finds
	// silent for a minute to another replica. This run's first step finished long
	// before its write opened it, so its held receipt carries a beat that old;
	// replayed as it was made, the row would read as silent from the moment it
	// existed. Read back the instant the opening write returns -- which is what the
	// sweep would read -- the run must be live.
	t.Run("ALateRunIsLiveTheMomentItOpens", func(t *testing.T) {
		probe := &lateWriteProbe{holdDBProbe: holdDBProbe{&dbLogicProbe{engine: engine}}, delay: 300 * time.Millisecond}
		e := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: probe})
		t.Cleanup(e.Close)
		exec, err := e.Execute(context.Background(), dbScheduledTick("lateScheduledTick", "readA", "lateWrite"), "schedule")
		if err != nil || exec.Status != "completed" {
			t.Fatalf("status %q, err %v", exec.Status, err)
		}
		if probe.seenErr != nil || probe.seen == nil {
			t.Fatalf("the run was not on file when its opening write returned: %v", probe.seenErr)
		}
		// The engine may store the stamp at a coarser precision than the clock reads
		// it; the beat that must not survive is the first step's, a full delay older.
		if probe.seen.HeartbeatAt.Before(probe.releasedAt.Add(-probe.delay / 3)) {
			t.Fatalf("the run opened with heartbeatAt %s, %s before it opened: the sweep would read it as silent and hand it to a second replica",
				probe.seen.HeartbeatAt.Format(time.RFC3339Nano), probe.releasedAt.Sub(probe.seen.HeartbeatAt))
		}
	})

	// The first version of a late run carries its heartbeat, which needs the
	// mutation to take one: createWorkRun is the only writer that can put it on
	// the version the sweep may read before any update lands.
	t.Run("CreateWorkRunStoresTheHeartbeatItIsGiven", func(t *testing.T) {
		auto := &Automation{Name: "heartbeatOnCreate", Steps: scheduledSteps("a")}
		exec := NewExecution(auto.Name, "schedule")
		exec.ID = fmt.Sprintf("heartbeatOnCreate%d", time.Now().UnixNano())
		beat := time.Date(2026, 10, 11, 12, 30, 15, 0, time.UTC)

		j := newWorkJournal(engine, nil)
		args := j.openRunArgs(auto, exec, nil, events.Cause{})
		args["heartbeatAt"] = rfc3339(beat)
		if err := j.write(context.Background(), "createWorkRun", args); err != nil {
			t.Fatalf("createWorkRun refused a heartbeatAt: %v", err)
		}
		journal, err := LoadRunJournal(context.Background(), engine, exec.ID)
		if err != nil {
			t.Fatalf("the created run: %v", err)
		}
		if !journal.HeartbeatAt.Equal(beat) {
			t.Fatalf("the run was created with heartbeatAt %s, want the %s it was given", journal.HeartbeatAt.Format(time.RFC3339Nano), beat.Format(time.RFC3339Nano))
		}
	})
}

func TestScheduledChildJournal_DB(t *testing.T) {
	engine := sharedJournalEngine(t)
	for _, mode := range []string{"idle", "child-writes", "parent-writes-later"} {
		t.Run(mode, func(t *testing.T) {
			e, rec, reg, parent, child := subAutomationHarness(t)
			parent.Trigger = nil
			parent.Schedule = "0 */10 * * * *"
			parent.Steps = append(parent.Steps, scheduledSteps("after")...)
			probe := &heldChildDBProbe{heldChildProbe: heldChildProbe{base: reg, rec: rec, mode: mode, child: child.Name}, engine: engine}
			e.stepRegistry = probe
			e.journal = newWorkJournal(engine, nil)
			exec, err := e.Execute(context.Background(), parent, "schedule")
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{exec.ID, probe.childID} {
				j, err := LoadRunJournal(context.Background(), engine, id)
				if mode == "idle" {
					if !errors.Is(err, ErrRunNotFound) {
						t.Fatalf("idle run %s persisted: %v", id, err)
					}
				} else if err != nil || j.Status != "succeeded" {
					t.Fatalf("run %s did not persist a completed journal: %+v %v", id, j, err)
				}
			}
		})
	}
}

type heldChildDBProbe struct {
	heldChildProbe
	engine  *memql.MemQLEngine
	childID string
}

func (p *heldChildDBProbe) Execute(ctx context.Context, step *Step, sc *StepContext) (*StepResult, error) {
	child := sc.Execution.AutomationName == p.child
	if child {
		p.childID = sc.Execution.ID
	}
	if (child && p.mode == "child-writes") || (step.ID == "after" && p.mode == "parent-writes-later") {
		writer := &dbLogicProbe{engine: p.engine}
		if _, err := writer.Execute(ctx, &Step{ID: step.ID, Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "write"}}, sc); err != nil {
			return nil, err
		}
	}
	return p.base.Execute(ctx, step, sc)
}
