package automations

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

func TestReplaySafetyInspectsActualCalleeAndNestedBody(t *testing.T) {
	fns := newTestFunctionRegistry()
	for _, fn := range []*memql.Function{
		{Name: "read", FunctionKind: "query", Expr: &memql.ComparisonExpression{Value: true}},
		{Name: "write", FunctionKind: "mutation"},
		{Name: "opaque", Type: memql.FunctionTypeBuiltin, Executor: "integration.future.publish"},
		{Name: "readLogic", FunctionKind: "logic", LogicBody: []map[string]any{{"id": "r", "type": "function", "function": map[string]any{"name": "read", "kind": "query"}}}},
		{Name: "writeLogic", FunctionKind: "logic", LogicBody: []map[string]any{{"id": "w", "type": "function", "function": map[string]any{"name": "write", "kind": "query"}}}},
	} {
		if err := fns.Upsert(fn); err != nil {
			t.Fatal(err)
		}
	}
	call := func(name string) *Step {
		return &Step{ID: "s", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: name, Kind: "query"}}
	}
	for _, tc := range []struct {
		name string
		safe bool
	}{{"read", true}, {"write", false}, {"opaque", false}, {"missing", false}, {"readLogic", true}, {"writeLogic", false}} {
		t.Run(tc.name, func(t *testing.T) {
			step := call(tc.name)
			if got := replaySafeStep(step, fns, nil, map[string]bool{}, 0); got != tc.safe {
				t.Fatalf("safe=%v want %v", got, tc.safe)
			}
			// Repeat through a loop nested in a parallel block. The outer
			// composer must not erase the inner operation's classification.
			a := statementAutomation(t, "@template\nautomation nested { parallel { branch a { for x in [1] { query "+tc.name+"() } } } }")
			if got := replaySafeStep(a.Steps[0], fns, nil, map[string]bool{}, 0); got != tc.safe {
				t.Fatalf("nested safe=%v want %v", got, tc.safe)
			}
		})
	}
}

func TestResumePreservesCompletedEffectAndSkippedDecision(t *testing.T) {
	a := statementAutomation(t, "@template\nautomation recovery { mutation send()\nmutation skipped()\nbuiltin finish() }")
	j := &RunJournal{RunId: "r", AutomationName: a.Name, FailedStep: "finish", StepStates: states("send", StepState{Status: "done"}, "skipped", StepState{Status: "skipped"}, "finish", StepState{Status: "failed"})}
	probe := newStmtProbe()
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	if _, err := e.ResumeFrom(context.Background(), j, a, &ResumeOptions{AllowSideEffects: true}); err != nil {
		t.Fatal(err)
	}
	if got := probe.callees(); !reflect.DeepEqual(got, []string{"finish"}) {
		t.Fatalf("recovery repeated confirmed/skipped work: %v", got)
	}
}

func TestResumeMissingBoundReceiptRefusesBeforeAnyWrite(t *testing.T) {
	a := statementAutomation(t, "@template\nautomation recovery { saved := mutation send()\nbuiltin finish(x: saved) }")
	for _, m := range []*MinimalStepResult{nil, {StepId: "saved", Status: "completed"}} {
		j := &RunJournal{RunId: "r", AutomationName: a.Name, FailedStep: "finish", StepStates: states("saved", StepState{Status: "done"}, "finish", StepState{Status: "failed"}), Steps: map[string]*MinimalStepResult{"saved": m}}
		probe, rec := newStmtProbe(), &journalRecorder{}
		e := NewExecutor(ExecutorOptions{StepRegistry: probe})
		e.journal = newWorkJournal(rec, nil)
		if _, err := e.ResumeFrom(context.Background(), j, a, &ResumeOptions{AllowSideEffects: true}); !errors.Is(err, ErrRunJournalInvalid) {
			t.Fatalf("missing bound result admitted: %v", err)
		}
		if len(probe.callees()) != 0 || len(rec.all()) != 0 {
			t.Fatal("refused recovery executed or reopened the run")
		}
	}
}

func TestResumeRecordedNilDoesNotRepeatItsProducer(t *testing.T) {
	a := statementAutomation(t, "@template\nautomation recovery { saved := mutation send()\nbuiltin finish(x: saved) }")
	m := ToMinimalStepResults(map[string]*StepResult{"saved": {StepId: "saved", Status: "completed", BoundRecorded: true}})["saved"]
	j, err := runJournalFromRows(map[string]any{"id": "r", "automationName": a.Name}, []map[string]any{
		{"key": "saved", "status": "done", "result": jsonShape(t, m)}, {"key": "finish", "status": "failed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	probe := newStmtProbe()
	if _, err := NewExecutor(ExecutorOptions{StepRegistry: probe}).ResumeFrom(context.Background(), j, a, &ResumeOptions{AllowSideEffects: true}); err != nil {
		t.Fatal(err)
	}
	if got := probe.callees(); !reflect.DeepEqual(got, []string{"finish"}) {
		t.Fatalf("nil producer repeated: %v", got)
	}
}

func TestMalformedReceiptIsNotPartiallyRehydrated(t *testing.T) {
	_, err := runJournalFromRows(map[string]any{"id": "r", "automationName": "a"}, []map[string]any{
		{"key": "send", "status": "done", "result": map[string]any{"value": "partial", "valueRecorded": "not-a-boolean"}},
	})
	if !errors.Is(err, ErrResumeResultMissing) {
		t.Fatalf("malformed receipt admitted: %v", err)
	}
}
