package automations

import (
	"errors"
	"testing"

	"github.com/znasllc-io/memql/core/id"
)

func TestValidateRunJournal_RefusesAChangedAutomation(t *testing.T) {
	auto := &Automation{Name: "demo", Steps: []*Step{{ID: "a", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}}}}
	j := &RunJournal{RunId: "run-1", AutomationName: "demo", TemplateFingerprint: "stale", FailedStep: "a"}
	if err := ValidateRunJournal(j, auto, id.New()); !errors.Is(err, ErrAutomationChanged) {
		t.Fatalf("err = %v, want ErrAutomationChanged", err)
	}
	j.TemplateFingerprint = auto.DefinitionFingerprint(id.New())
	if err := ValidateRunJournal(j, auto, id.New()); err != nil {
		t.Fatalf("matching fingerprint refused: %v", err)
	}
}

func TestValidateRunJournal_NeedsARunAndAFailedStep(t *testing.T) {
	if err := ValidateRunJournal(nil, &Automation{}, id.New()); !errors.Is(err, ErrRunJournalInvalid) {
		t.Fatalf("nil journal: %v", err)
	}
	if err := ValidateRunJournal(&RunJournal{RunId: "r", AutomationName: "demo"}, &Automation{Name: "demo"}, id.New()); !errors.Is(err, ErrRunJournalInvalid) {
		t.Fatalf("no failed step: %v", err)
	}
}

func TestRunJournalFromRows_TakesTheLatestVersionOfEachStep(t *testing.T) {
	run := map[string]any{
		"id": "v1:work:run:run-1", "automationName": "demo", "templateFingerprint": "fp",
		"triggeredBy": "event", "callerSuppliedPayload": true, "input": map[string]any{"k": "v"},
		"chainHead": "h2", "initialChainHead": "h0", "stepOrder": []any{"a", "b"},
	}
	steps := []map[string]any{
		{"id": "v1:work:step:run-1-a", "key": "a", "status": "done", "result": map[string]any{"stepId": "a", "status": "completed", "result": 1}},
		{"id": "v1:work:step:run-1-b", "key": "b", "status": "failed", "errorMessage": "boom"},
	}
	j, err := runJournalFromRows(run, steps)
	if err != nil {
		t.Fatalf("runJournalFromRows: %v", err)
	}
	if j.RunId != "run-1" || j.AutomationName != "demo" || !j.CallerSuppliedPayload || j.ChainHead != "h2" {
		t.Errorf("run fields: %+v", j)
	}
	if j.FailedStep != "b" {
		t.Errorf("failed step = %q, want b", j.FailedStep)
	}
	if got := j.Steps["a"]; got == nil || got.Status != "completed" {
		t.Errorf("done step a not rehydrated: %+v", got)
	}
	if _, present := j.Steps["b"]; present {
		t.Error("a failed step is not a completed result")
	}
	if len(j.StepOrder) != 2 {
		t.Errorf("stepOrder = %v", j.StepOrder)
	}
}

// A step at `running` with no receipt is a crash mid-step, and it resumes from
// exactly where a `failed` one does. Without this arm a run whose node died
// between the intent and the receipt reports no resume point at all, and
// ValidateRunJournal refuses it as invalid rather than resuming it.
func TestRunJournalFromRows_AnUnfinishedRunningStepIsAResumePoint(t *testing.T) {
	run := map[string]any{"id": "v1:work:run:run-9", "automationName": "demo"}
	steps := []map[string]any{
		{"id": "v1:work:step:run-9-a", "key": "a", "status": "done"},
		{"id": "v1:work:step:run-9-b", "key": "b", "status": "running"},
	}
	j, err := runJournalFromRows(run, steps)
	if err != nil {
		t.Fatalf("runJournalFromRows: %v", err)
	}
	if j.FailedStep != "b" {
		t.Fatalf("FailedStep = %q, want b -- a step written at `running` and never finished is a crash mid-step", j.FailedStep)
	}
}

// The short id is what the executor minted and what a later write addresses.
// A read returns the canonical form, so a resume that kept it would open a
// SECOND run at v1:work:run:v1:work:run:<id>.
func TestRunJournalFromRows_StripsTheCanonicalPrefix(t *testing.T) {
	j, err := runJournalFromRows(map[string]any{"id": "v1:work:run:abc123", "automationName": "d"}, nil)
	if err != nil {
		t.Fatalf("runJournalFromRows: %v", err)
	}
	if j.RunId != "abc123" {
		t.Fatalf("RunId = %q, want abc123", j.RunId)
	}
}

func TestResumeRetryableRule(t *testing.T) {
	if !IsStepRetryable(StepTypeQuery) || IsStepRetryable(StepTypeMutation) || IsStepRetryable(StepTypeWebhook) {
		t.Fatal("read-only steps retry freely; mutation and webhook need AllowSideEffects")
	}
	if IsStepRetryable(StepTypeEvent) || IsStepRetryable(StepTypeAction) {
		t.Fatal("event and action steps have external effects too")
	}
	if !IsStepRetryable(StepTypeFunction) || !IsStepRetryable(StepTypeForEach) {
		t.Fatal("function, forEach and the other composers are re-run")
	}
}
