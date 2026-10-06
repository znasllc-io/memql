package automations

import (
	"errors"
	"testing"

	"github.com/znasllc-io/memql/core/id"
)

func TestResumeRejectsChangedExecutionDefinition(t *testing.T) {
	base := func() *Automation {
		return statementAutomation(t, `@template
automation guarded {
  args { destination string! }
  builtin send(destination: args.destination, content: "first") retry(1)
  for entry in [1, 2] { builtin record(value: entry) }
}`)
	}
	for _, tc := range []struct {
		name string
		edit func(*Automation)
	}{
		{"callee", func(a *Automation) { a.Steps[0].Function.Name = "delete" }},
		{"arguments", func(a *Automation) { a.Steps[0].Function.Args["content"] = "\"changed\"" }},
		{"retry", func(a *Automation) { a.Steps[0].RetryCount++ }},
		{"error-policy", func(a *Automation) { a.Steps[0].OnError = ErrorStrategyContinue }},
		{"journal-policy", func(a *Automation) { a.JournalRequired = true }},
		{"nested-body", func(a *Automation) { a.Steps[1].ForEach.Do[0].Function.Name = "changed" }},
		{"input-contract", func(a *Automation) { a.Args = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := base()
			engine := id.New()
			journal := &RunJournal{RunId: "r", AutomationName: a.Name, FailedStep: a.Steps[0].ID, TemplateFingerprint: a.DefinitionFingerprint(engine)}
			tc.edit(a)
			if err := ValidateRunJournal(journal, a, engine); !errors.Is(err, ErrAutomationChanged) {
				t.Fatalf("changed definition was resumable: %v", err)
			}
		})
	}
}

func TestDefinitionFingerprintIgnoresPreparationAndSourceLocation(t *testing.T) {
	a := &Automation{Name: "stable", Steps: []*Step{{ID: "one", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "query", Kind: "query", Args: map[string]any{"id": "\"one\""}}}}}
	engine := id.New()
	want := a.DefinitionFingerprint(engine)
	if err := ensurePrepared(a); err != nil {
		t.Fatal(err)
	}
	a.Origin, a.Description, a.Trusted = "/another/replica/definition.memql", "Changed prose", true
	if got := a.DefinitionFingerprint(engine); got != want {
		t.Fatalf("non-executable metadata changed the fingerprint: %s != %s", got, want)
	}
}
