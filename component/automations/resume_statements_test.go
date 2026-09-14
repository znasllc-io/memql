package automations

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// resume_statements_test.go -- a statement body resumed from its journal
// (epic memql#5370, task memql#5372), DB-free: the journal is built as
// LoadRunJournal would build it, the step registry is the probe, and the
// journal writes into a recorder. resume_v1_db_test.go is the round trip
// through Postgres.

// resumeProbe runs ResumeFrom over a built journal.
func resumeProbe(t *testing.T, src string, j *RunJournal, opts *ResumeOptions) (*stmtProbe, *journalRecorder, *AutomationExecution, error) {
	t.Helper()
	a := statementAutomation(t, src)
	j.RunId, j.AutomationName = "r1", a.Name
	probe := newStmtProbe()
	rec := &journalRecorder{}
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	e.journal = newWorkJournal(rec, nil)
	exec, err := e.ResumeFrom(context.Background(), j, a, opts)
	return probe, rec, exec, err
}

func states(pairs ...any) map[string]StepState {
	m := map[string]StepState{}
	for i := 0; i < len(pairs); i += 2 {
		m[pairs[i].(string)] = pairs[i+1].(StepState)
	}
	return m
}

func TestStatementResumeRebindsWhatFinishedAndPassesWhatWasContinued(t *testing.T) {
	// `flaky` failed under `on error continue` and its row came back first, so
	// the journal's FailedStep names it; the run stopped at `b`.
	j := &RunJournal{
		FailedStep: "flaky",
		Steps:      map[string]*MinimalStepResult{"a": {StepId: "a", Status: "success", Value: "A"}},
		StepStates: states(
			"a", StepState{Status: "done", Attempt: 1},
			"flaky", StepState{Status: "failed", Attempt: 1},
			"b", StepState{Status: "failed", Attempt: 1},
		),
	}
	probe, rec, exec, err := resumeProbe(t, `@trigger(event="probe.fired")
automation resumes {
  a := builtin one()
  builtin flaky() on error continue
  b := builtin two(x: a)
  builtin three(y: b)
}`, j, nil)
	if err != nil {
		t.Fatalf("resume: %v (%s)", err, exec.Error)
	}
	if got, want := probe.callees(), []string{"two", "three"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v, want %v: a finished and flaky was continued past; neither runs again", got, want)
	}
	if x := probe.argsOf(t, "two", 0)["x"]; x != "A" {
		t.Fatalf("two(x: %v), want the value a recorded", x)
	}
	if got := rec.keys(); !reflect.DeepEqual(got, []string{"b", "three"}) {
		t.Fatalf("step rows %v, want only the statements that ran", got)
	}
	joined := strings.Join(rec.all(), "\n")
	if !strings.Contains(joined, `idempotencyKey: "r1:b:2"`) {
		t.Fatalf("the resumed statement did not run as its second attempt:\n%s", joined)
	}
	if !reflect.DeepEqual(exec.StepOrder, []string{"a", "flaky", "b", "three"}) {
		t.Fatalf("step order %v, want the body's", exec.StepOrder)
	}
}

func TestStatementResumeReadsAnUnrecordedQueryAgain(t *testing.T) {
	j := &RunJournal{
		FailedStep: "consume",
		// Done, but with no value: its rows were more than the journal keeps.
		Steps:      map[string]*MinimalStepResult{"rows": {StepId: "rows", Status: "success"}},
		StepStates: states("rows", StepState{Status: "done", Attempt: 1}, "consume", StepState{Status: "failed", Attempt: 1}),
	}
	a := statementAutomation(t, `@trigger(event="probe.fired")
automation reads {
  rows := query items()
  builtin consume(n: rows.count())
}`)
	j.RunId, j.AutomationName = "r1", a.Name
	probe := newStmtProbe()
	probe.answers["items"] = rowsResult(map[string]any{"id": "v1:x:item:1", "payload": map[string]any{}}, map[string]any{"id": "v1:x:item:2", "payload": map[string]any{}})
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	if exec, err := e.ResumeFrom(context.Background(), j, a, nil); err != nil {
		t.Fatalf("resume: %v (%s)", err, exec.Error)
	}
	if got := probe.callees(); !reflect.DeepEqual(got, []string{"items", "consume"}) {
		t.Fatalf("calls %v, want the read again, then the resume point", got)
	}
	if n := probe.argsOf(t, "consume", 0)["n"]; n != int64(2) {
		t.Fatalf("consume(n: %v), want 2", n)
	}
}

func TestStatementResumeAtAMutationNeedsSideEffectsAllowed(t *testing.T) {
	src := `@trigger(event="probe.fired")
automation writes {
  a := builtin one()
  mutation save(v: a)
}`
	journal := func() *RunJournal {
		return &RunJournal{
			FailedStep: "save",
			Steps:      map[string]*MinimalStepResult{"a": {StepId: "a", Status: "success", Value: "A"}},
			StepStates: states("a", StepState{Status: "done", Attempt: 1}, "save", StepState{Status: "failed", Attempt: 1}),
		}
	}
	if _, _, _, err := resumeProbe(t, src, journal(), nil); !errors.Is(err, ErrNonRetryableStep) {
		t.Fatalf("resume at a mutation statement: %v, want ErrNonRetryableStep", err)
	}
	probe, _, exec, err := resumeProbe(t, src, journal(), &ResumeOptions{AllowSideEffects: true})
	if err != nil {
		t.Fatalf("resume: %v (%s)", err, exec.Error)
	}
	if got := probe.callees(); !reflect.DeepEqual(got, []string{"save"}) || probe.argsOf(t, "save", 0)["v"] != "A" {
		t.Fatalf("calls %v, want save(v: A)", got)
	}
}

func TestRunJournalIgnoresNestedKeys(t *testing.T) {
	// A logic's statements journal under the statement that called it; a
	// failed one of them is not where the run resumes.
	rows := []map[string]any{
		{"key": "first", "status": "done", "attempt": float64(1)},
		{"key": "verdict/a", "status": "done", "attempt": float64(1)},
		{"key": "verdict/write", "status": "failed", "attempt": float64(1)},
		{"key": "verdict", "status": "failed", "attempt": float64(2)},
	}
	j, err := runJournalFromRows(map[string]any{"id": "v1:work:run:r1", "automationName": "x"}, rows)
	if err != nil {
		t.Fatal(err)
	}
	if j.FailedStep != "verdict" {
		t.Fatalf("FailedStep = %q, want the calling statement", j.FailedStep)
	}
	if _, nested := j.Steps["verdict/a"]; nested || len(j.StepStates) != 2 {
		t.Fatalf("nested rows reached the journal: steps %v, states %v", j.Steps, j.StepStates)
	}
	if j.StepStates["verdict"] != (StepState{Status: "failed", Attempt: 2}) {
		t.Fatalf("verdict's state = %+v", j.StepStates["verdict"])
	}
}

func TestAStatementsReceiptRecordsTheValueItBound(t *testing.T) {
	probe := newStmtProbe()
	probe.answers["one"] = memql.NewResultWithOutput("A")
	many := make([]map[string]any, maxJournaledRows+1)
	for i := range many {
		many[i] = map[string]any{"id": "v1:x:item:" + string(rune('a'+i%26)), "payload": map[string]any{}}
	}
	probe.answers["many"] = rowsResult(many...)
	probe.answers["few"] = rowsResult(many[:2]...)
	rec := &journalRecorder{}
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	e.journal = newWorkJournal(rec, nil)
	a := statementAutomation(t, `@trigger(event="probe.fired")
automation records {
  a := builtin one()
  lots := query many()
  some := query few()
  builtin consume(a: a, n: lots.count(), m: some.count())
}`)
	if exec, err := e.ExecuteWithEvent(context.Background(), a, "test", nil); err != nil {
		t.Fatalf("run: %v (%s)", err, exec.Error)
	}
	receipt := func(key string) string {
		for _, c := range rec.all() {
			if strings.HasPrefix(c, "updateWorkStep(") && strings.Contains(c, `"stepId":"`+key+`"`) {
				return c
			}
		}
		t.Fatalf("no receipt for %s", key)
		return ""
	}
	if !strings.Contains(receipt("a"), `"value":"A"`) {
		t.Errorf("a's receipt does not record the value it bound: %s", receipt("a"))
	}
	if strings.Contains(receipt("lots"), `"value":`) {
		t.Errorf("a query's %d rows were recorded; past %d a resume reads them again", len(many), maxJournaledRows)
	}
	if !strings.Contains(receipt("some"), `"value":[`) {
		t.Errorf("a query's two rows were not recorded: %s", receipt("some"))
	}
	if strings.Contains(receipt("consume"), `"value":`) {
		t.Errorf("a statement that binds nothing recorded a value: %s", receipt("consume"))
	}
}
