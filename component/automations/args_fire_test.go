package automations

// G1 (event-payload-binding ADR Decision 2, memql#2363) fire-time tests: the
// refusal path through the executor (universal gate), the scheduler
// (validation BEFORE the @filter), and invoke-by-reference.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/events"
)

// deployArgsSchema is the contract used across the fire tests: one required
// enum string plus an optional int.
func deployArgsSchema() *ArgsSchema {
	return &ArgsSchema{Fields: []*ArgsField{
		{Name: "environment", Type: "string", Optional: false, Enum: []any{"development", "staging", "production"}},
		{Name: "replicas", Type: "int", Optional: true},
	}}
}

func loggedExecutor(buf *bytes.Buffer, bus *events.Bus) *Executor {
	return NewExecutor(ExecutorOptions{Logger: capturingLogger(buf), EventBus: bus})
}

// A missing @required field refuses the run: status "skipped", zero steps,
// the skip counter increments, and the loud Warn names the field.
func TestExecuteWithEvent_RefusesMissingRequired(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	var buf bytes.Buffer
	exec := loggedExecutor(&buf, bus)

	auto := &Automation{
		Name:  "deployWithArgs",
		Args:  deployArgsSchema(),
		Steps: []*Step{{ID: "gate", Type: StepTypeFunction}},
	}
	ev := events.NewEvent("deploy.requested", events.KindNodeCreated, map[string]any{"replicas": 2})

	before := refusedFires.CountFor(auto.Name)
	result, err := exec.ExecuteWithEvent(context.Background(), auto, "test", &ev)
	if err != nil {
		t.Fatalf("ExecuteWithEvent error: %v", err)
	}
	if result.Status != "skipped" {
		t.Fatalf("status = %q, want skipped (missing @required must refuse before any step)", result.Status)
	}
	if len(result.Steps) != 0 {
		t.Errorf("expected 0 step results on refusal, got %d", len(result.Steps))
	}
	if got := refusedFires.CountFor(auto.Name) - before; got != 1 {
		t.Errorf("skip counter delta = %d, want 1", got)
	}
	logs := buf.String()
	if !strings.Contains(logs, "refused to fire") || !strings.Contains(logs, "environment") {
		t.Errorf("expected a loud Warn naming the failing field 'environment'; logs:\n%s", logs)
	}
}

func TestExecuteWithEvent_RefusesWrongType(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	var buf bytes.Buffer
	exec := loggedExecutor(&buf, bus)

	auto := &Automation{Name: "wrongType", Args: deployArgsSchema(), Steps: []*Step{{ID: "gate", Type: StepTypeFunction}}}
	ev := events.NewEvent("deploy.requested", events.KindNodeCreated,
		map[string]any{"environment": "staging", "replicas": "two"})

	before := refusedFires.CountFor(auto.Name)
	result, err := exec.ExecuteWithEvent(context.Background(), auto, "test", &ev)
	if err != nil {
		t.Fatalf("ExecuteWithEvent error: %v", err)
	}
	if result.Status != "skipped" {
		t.Fatalf("status = %q, want skipped (type mismatch must refuse)", result.Status)
	}
	if got := refusedFires.CountFor(auto.Name) - before; got != 1 {
		t.Errorf("skip counter delta = %d, want 1", got)
	}
	if !strings.Contains(buf.String(), "replicas") {
		t.Errorf("Warn must name the mistyped field 'replicas'; logs:\n%s", buf.String())
	}
}

func TestExecuteWithEvent_RefusesEnumViolation(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	var buf bytes.Buffer
	exec := loggedExecutor(&buf, bus)

	auto := &Automation{Name: "enumViol", Args: deployArgsSchema(), Steps: []*Step{{ID: "gate", Type: StepTypeFunction}}}
	ev := events.NewEvent("deploy.requested", events.KindNodeCreated, map[string]any{"environment": "qa"})

	before := refusedFires.CountFor(auto.Name)
	result, err := exec.ExecuteWithEvent(context.Background(), auto, "test", &ev)
	if err != nil {
		t.Fatalf("ExecuteWithEvent error: %v", err)
	}
	if result.Status != "skipped" {
		t.Fatalf("status = %q, want skipped (enum violation must refuse)", result.Status)
	}
	if got := refusedFires.CountFor(auto.Name) - before; got != 1 {
		t.Errorf("skip counter delta = %d, want 1", got)
	}
}

// A valid payload carrying UNDECLARED extra fields is NOT refused
// (tolerant-reader). A zero-step automation completes cleanly so the assertion
// does not depend on a wired step registry.
func TestExecuteWithEvent_ToleratesExtraFields(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	var buf bytes.Buffer
	exec := loggedExecutor(&buf, bus)

	auto := &Automation{Name: "tolerant", Args: deployArgsSchema()} // zero steps
	ev := events.NewEvent("deploy.requested", events.KindNodeCreated, map[string]any{
		"environment": "staging",
		"triggeredBy": "alice", // undeclared -> tolerated
	})

	before := refusedFires.CountFor(auto.Name)
	result, err := exec.ExecuteWithEvent(context.Background(), auto, "test", &ev)
	if err != nil {
		t.Fatalf("ExecuteWithEvent error: %v", err)
	}
	if result.Status == "skipped" {
		t.Fatalf("status = skipped, but extra fields must be tolerated (not refused)")
	}
	if got := refusedFires.CountFor(auto.Name) - before; got != 0 {
		t.Errorf("skip counter delta = %d, want 0 (tolerant extras are not a refusal)", got)
	}
	if !strings.Contains(buf.String(), "undeclared payload fields ignored") {
		t.Errorf("expected a Debug note about ignored extra fields; logs:\n%s", buf.String())
	}
}

// newMinimalScheduler builds just enough Scheduler to exercise
// subscribeToEventTrigger + TriggerAutomationWithArgs without a real
// engine/loader/step-registry.
func newMinimalScheduler(buf *bytes.Buffer, bus *events.Bus) *Scheduler {
	s := &Scheduler{
		logger:      capturingLogger(buf),
		eventBus:    bus,
		automations: map[string]*Automation{},
		eventUnsubs: []func(){},
	}
	s.eventExecutor = NewExecutor(ExecutorOptions{Logger: s.logger, EventBus: bus})
	return s
}

// Validation runs BEFORE the @filter: a contract violation refuses the fire
// (skip counter++) without the filter ever being consulted. A valid payload
// is NOT refused. The filter itself reads the validated `args` binding
// (proven by TestEvaluatorSeesArgs, which uses the same EvaluateCondition
// path this scheduler filter uses).
func TestScheduler_ValidationRefusesBeforeFilter(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	var buf bytes.Buffer
	s := newMinimalScheduler(&buf, bus)

	auto := &Automation{
		Name:    "deployGated",
		Args:    deployArgsSchema(),
		Trigger: &TriggerConfig{Event: "deploy.requested", Filter: `args.environment == "staging"`},
		Steps:   []*Step{{ID: "gate", Type: StepTypeFunction}},
	}
	s.automations[auto.Name] = auto
	if err := s.subscribeToEventTrigger(auto); err != nil {
		t.Fatalf("subscribeToEventTrigger: %v", err)
	}

	// Violation (missing required environment): refuses before the filter.
	before := refusedFires.CountFor(auto.Name)
	bus.PublishSync(events.NewEvent("deploy.requested", events.KindNodeCreated, map[string]any{"replicas": 1}))
	if got := refusedFires.CountFor(auto.Name) - before; got != 1 {
		t.Errorf("violating payload: skip counter delta = %d, want 1 (validation must refuse before the filter)", got)
	}

	// Valid contract, filter false (environment=development): NOT refused --
	// the filter gates it, not the args contract.
	before = refusedFires.CountFor(auto.Name)
	bus.PublishSync(events.NewEvent("deploy.requested", events.KindNodeCreated, map[string]any{"environment": "development"}))
	if got := refusedFires.CountFor(auto.Name) - before; got != 0 {
		t.Errorf("valid-but-filtered payload: skip counter delta = %d, want 0 (a filter miss is not a refusal)", got)
	}

	// Valid contract, filter true (environment=staging): NOT refused.
	before = refusedFires.CountFor(auto.Name)
	bus.PublishSync(events.NewEvent("deploy.requested", events.KindNodeCreated, map[string]any{"environment": "staging"}))
	if got := refusedFires.CountFor(auto.Name) - before; got != 0 {
		t.Errorf("valid payload: skip counter delta = %d, want 0", got)
	}
}

// Invoke-by-reference (sub-automation) validates the passed params against the
// same args contract, with the same refusal semantics.
func TestTriggerAutomationWithArgs_ValidatesContract(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	var buf bytes.Buffer
	s := newMinimalScheduler(&buf, bus)

	// Zero steps so the valid case completes cleanly without a step registry;
	// the args gate runs before any step regardless.
	auto := &Automation{Name: "subDeploy", Args: deployArgsSchema()}
	s.automations[auto.Name] = auto

	// Violating params -> refused (status skipped + counter++).
	before := refusedFires.CountFor(auto.Name)
	exec, err := s.TriggerAutomationWithArgs(context.Background(), auto.Name, map[string]any{"environment": "qa"})
	if err != nil {
		t.Fatalf("TriggerAutomationWithArgs error: %v", err)
	}
	if exec.Status != "skipped" {
		t.Fatalf("status = %q, want skipped (invoke-by-reference must validate the same contract)", exec.Status)
	}
	if got := refusedFires.CountFor(auto.Name) - before; got != 1 {
		t.Errorf("skip counter delta = %d, want 1", got)
	}

	// Valid params -> NOT refused; passes the args gate.
	before = refusedFires.CountFor(auto.Name)
	exec2, err := s.TriggerAutomationWithArgs(context.Background(), auto.Name, map[string]any{"environment": "staging", "replicas": 3})
	if err != nil {
		t.Fatalf("TriggerAutomationWithArgs error: %v", err)
	}
	if exec2.Status == "skipped" {
		t.Fatalf("valid params must pass the args gate, got status skipped")
	}
	if got := refusedFires.CountFor(auto.Name) - before; got != 0 {
		t.Errorf("valid params: skip counter delta = %d, want 0", got)
	}
}
