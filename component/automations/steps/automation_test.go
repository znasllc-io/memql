package steps

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
)

// automation_test.go -- what a statement calling another automation binds
// (epic memql#5414, D24). A decomposed goal calls each section it planned
// live as an automation of its own, and a later section and the assembly
// read that section's answer by the name its statement binds -- so the name
// must hold what the sub-automation RETURNED, not a note that it ran.

// memberRunner resolves a sub-automation among the automations it holds and
// runs it on the real executor, over the same registry, the way the node a
// work run executes on resolves one among its draft's members.
type memberRunner struct {
	members map[string]*automations.Automation
	reg     *Registry
}

func (m *memberRunner) TriggerAutomation(ctx context.Context, name string) (*automations.AutomationExecution, error) {
	return m.TriggerAutomationWithArgs(ctx, name, nil)
}

func (m *memberRunner) TriggerAutomationWithArgs(ctx context.Context, name string, args map[string]any) (*automations.AutomationExecution, error) {
	auto := m.members[name]
	if auto == nil {
		return nil, fmt.Errorf("%s is not a member", name)
	}
	exec, err := automations.NewExecutor(automations.ExecutorOptions{StepRegistry: m.reg, AutomationTrigger: m}).
		ExecuteWithClientEvent(ctx, auto, "work.subtemplate", &events.Event{Payload: args})
	if err == nil && exec.Status != "completed" {
		err = fmt.Errorf("member %s ended %s: %s", name, exec.Status, exec.Error)
	}
	return exec, err
}

// runWithMembers runs the parent automation with the members it calls, every
// construct call answered by the probe.
func runWithMembers(t *testing.T, probe *callProbe, parent string, members ...string) *automations.AutomationExecution {
	t.Helper()
	loader := automations.NewLoader(automations.LoaderOptions{})
	reg := NewRegistry()
	reg.Register(automations.StepTypeFunction, probe)
	runner := &memberRunner{members: map[string]*automations.Automation{}, reg: reg}
	for _, src := range members {
		a, err := loader.CompileSource(src, "member.memql")
		if err != nil {
			t.Fatalf("compile member: %v\n%s", err, src)
		}
		runner.members[a.Name] = a
	}
	a, err := loader.CompileSource(parent, "parent.memql")
	if err != nil {
		t.Fatalf("compile parent: %v\n%s", err, parent)
	}
	ev := events.NewEvent("probe.fired", events.KindMessage, nil)
	exec, err := automations.NewExecutor(automations.ExecutorOptions{StepRegistry: reg, AutomationTrigger: runner}).
		ExecuteWithEvent(context.Background(), a, "test", &ev)
	if err != nil || exec.Status != "completed" {
		t.Fatalf("run the parent: %v (%+v)", err, exec)
	}
	return exec
}

// TestAnAutomationStepBindsTheSubAutomationsReturnedValue: the statement's
// name holds what the sub-automation returned -- a later statement reads it,
// and the parent's own return is it -- and a sub-automation whose body
// returns nothing still binds the run summary it always bound.
func TestAnAutomationStepBindsTheSubAutomationsReturnedValue(t *testing.T) {
	const child = `@template
automation summarise {
  args {
    month any
  }
  answer := builtin draft(month: args.month)
  return {text: answer, month: args.month}
}`
	const quiet = `@template
automation touchOnly {
  args {
    month any
  }
  builtin touch(month: args.month)
}`
	const parent = `@trigger(event="probe.fired")
automation assemble {
  summary := automation summarise(month: "2026-08")
  touched := automation touchOnly(month: "2026-08")
  builtin record(summary: summary, touched: touched)
  return summary
}`
	probe := &callProbe{answers: map[string]any{"draft": "the August summary"}}
	exec := runWithMembers(t, probe, parent, child, quiet)

	want := map[string]any{"text": "the August summary", "month": "2026-08"}
	if !reflect.DeepEqual(exec.Output, want) {
		t.Fatalf("the parent returned %#v, want the sub-automation's returned value %#v", exec.Output, want)
	}
	recorded := probe.argsOf("record")
	if len(recorded) != 1 {
		t.Fatalf("record was called %d times, want once", len(recorded))
	}
	if got := recorded[0]["summary"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("a later statement read %#v by the name, want the returned value %#v", got, want)
	}
	touched, _ := recorded[0]["touched"].(map[string]any)
	if touched == nil || touched["automation"] != "touchOnly" || touched["status"] != "completed" {
		t.Fatalf("a sub-automation that returns nothing must still bind its run summary, got %#v", recorded[0]["touched"])
	}
}
