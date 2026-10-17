package automations

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestEventJournalOnWrite(t *testing.T) {
	for _, tc := range []struct {
		name, trigger, write, fail string
		optIn, wantJournal         bool
	}{
		{"noop", "event:delivery", "", "", true, false},
		{"authored-event", "authored", "", "", true, false},
		{"write", "event:delivery", "a", "", true, true},
		{"failure", "event:delivery", "", "a", true, true},
		{"manual", "run:client", "", "", true, true},
		{"default", "event:delivery", "", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingJournalExecutor{}
			probe := &scheduledProbe{rec: rec, writeAt: tc.write, failAt: tc.fail}
			e := scheduledHoldExecutor(rec, probe)
			defer e.Close()
			a := &Automation{Name: "inboundProbe", JournalOnWrite: tc.optIn, Steps: scheduledSteps("a")}
			a.Trigger = &TriggerConfig{Event: "graph.node.created.v1:platform:inboundRequest"}
			exec, err := e.Execute(context.Background(), a, tc.trigger)
			if tc.fail == "" && (err != nil || exec.Status != "completed") {
				t.Fatalf("execution: %v, %v", exec, err)
			}
			if tc.fail != "" && exec.Status != "failed" {
				t.Fatalf("failure was lost: %v, %v", exec, err)
			}
			if got := journalWriteNames(rec); (len(got) > 0) != tc.wantJournal {
				t.Fatalf("journal writes = %v, want journal %v", got, tc.wantJournal)
			}
			if tc.write != "" && len(probe.afterWrite[tc.write]) == 0 {
				t.Fatal("first write did not release the held journal")
			}
		})
	}
}

func TestEventJournalOnWritePreservesHumanWait(t *testing.T) {
	rec := &recordingJournalExecutor{}
	e := NewExecutor(ExecutorOptions{StepRegistry: &waitingProbe{waitAt: "a"}})
	defer e.Close()
	e.journal = newWorkJournal(rec, nil)
	exec, err := e.Execute(context.Background(), &Automation{Name: "askingEvent", JournalOnWrite: true, Steps: scheduledSteps("a")}, "event:delivery")
	if !isHumanWait(err) || exec.Status != "waiting" {
		t.Fatalf("human wait lost: %v, %v", exec, err)
	}
	names, args := journalWrites(t, rec)
	if len(names) != 3 || names[0] != "createWorkRun" || args[2]["status"] != "waiting" {
		t.Fatalf("waiting run not persisted: %v %v", names, args)
	}
}

func TestEventJournalOnWriteHonorsInheritedRequirement(t *testing.T) {
	rec := &recordingJournalExecutor{}
	probe := &scheduledProbe{rec: rec}
	e := scheduledHoldExecutor(rec, probe)
	defer e.Close()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ctx = context.WithValue(ctx, journalRequirementKey{}, &journalRequirement{cancel: cancel})
	_, err := e.Execute(ctx, &Automation{Name: "requiredChild", JournalOnWrite: true, Steps: scheduledSteps("a")}, "event:delivery")
	if err != nil {
		t.Fatal(err)
	}
	if len(probe.atStart["a"]) != 2 {
		t.Fatalf("inherited required journal was held: %v", probe.atStart["a"])
	}
}

func TestJournalOnWriteContract(t *testing.T) {
	a := statementAutomation(t, "@template\n@journalOnWrite\nautomation probe { return true }")
	if !a.JournalOnWrite {
		t.Fatal("annotation lost during compilation")
	}
	if holdsBackgroundJournal(a, "event:delivery", &RunAdoption{}) {
		t.Fatal("adopted run must keep its journal")
	}
	a.JournalRequired = true
	if err := prepareJournalContract(a); err == nil {
		t.Fatal("incompatible journal policies accepted")
	}
}

func TestEventJournalOnWrite_DB(t *testing.T) {
	engine := sharedJournalEngine(t)
	for _, effect := range []string{"readA", "write", "fail"} {
		t.Run(effect, func(t *testing.T) {
			e := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: holdDBProbe{&dbLogicProbe{engine: engine}}})
			defer e.Close()
			a := dbScheduledTick("eventJournal", "readA", effect)
			a.Schedule, a.JournalOnWrite = "", true
			exec, err := e.Execute(context.Background(), a, "event:delivery")
			if effect != "fail" && err != nil {
				t.Fatal(err)
			}
			journal, err := LoadRunJournal(context.Background(), engine, exec.ID)
			if effect == "readA" {
				if !errors.Is(err, ErrRunNotFound) {
					t.Fatalf("no-op journal: %v", err)
				}
				if rows := stepStatuses(t, engine, exec.ID); len(rows) != 0 {
					t.Fatalf("no-op steps: %v", rows)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if journal.TriggeredBy != "event:delivery" || len(stepStatuses(t, engine, exec.ID)) != 2 {
				t.Fatalf("incomplete event journal: %+v", journal)
			}
		})
	}
}

func TestStormWarningOncePerAdmittedWindow(t *testing.T) {
	var logs bytes.Buffer
	e := NewExecutor(ExecutorOptions{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	defer e.Close()
	a := &Automation{Name: "burstProbe"}
	burst := func() {
		for range 50 {
			if _, err := e.Execute(context.Background(), a, "event:delivery"); err != nil {
				t.Fatal(err)
			}
		}
	}
	burst()
	if got := strings.Count(logs.String(), "automation storm detected"); got != 1 {
		t.Fatalf("warnings = %d, want 1", got)
	}
	e.executionTracker.counts[a.Name].windowStart = time.Now().Add(-2 * time.Minute)
	burst()
	if got := strings.Count(logs.String(), "automation storm detected"); got != 2 {
		t.Fatalf("warnings = %d, want 2 after next window", got)
	}
}
