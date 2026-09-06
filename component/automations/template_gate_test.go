package automations

import (
	"strings"
	"testing"
)

// TestTemplateIsAThirdWayToBeReachable pins @template's whole reason for
// existing (memql#5048).
//
// validateTriggerWiring refuses an automation with no event trigger and no
// schedule, because one that silently never runs is indistinguishable from
// one that works until the day you need it. A work-spine template is reachable
// -- a v1:work:run names it and the dispatcher executes it -- so the gate has
// to know about the third way in, and this is what says so.
func TestTemplateIsAThirdWayToBeReachable(t *testing.T) {
	l := NewLoader(LoaderOptions{})

	if err := l.validateTriggerWiring(&Automation{Name: "t", Template: true}); err != nil {
		t.Fatalf("a @template automation with no trigger was refused: %v", err)
	}
	// The control: WITHOUT the flag, the same automation is refused. Without
	// this, the assertion above could pass because the gate stopped working.
	err := l.validateTriggerWiring(&Automation{Name: "t"})
	if err == nil {
		t.Fatal("an automation with no trigger, no schedule and no @template was ACCEPTED; the gate is not running")
	}
	if !strings.Contains(err.Error(), "nothing can ever run it") {
		t.Errorf("unexpected refusal: %v", err)
	}
}

// TestTemplateMayNotAlsoBeTriggered pins the mirror rule.
//
// A template that also declares a trigger runs BOTH ways -- once per matching
// event and once per run that named it. That is a double execution with side
// effects, and it presents as a template that works.
func TestTemplateMayNotAlsoBeTriggered(t *testing.T) {
	l := NewLoader(LoaderOptions{})

	cases := []struct {
		name string
		auto *Automation
	}{
		{"event trigger", &Automation{Name: "t", Template: true, Trigger: &TriggerConfig{Event: "graph.node.created.v1:work:run"}}},
		{"schedule", &Automation{Name: "t", Template: true, Schedule: "0 */10 * * * *"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := l.validateTriggerWiring(tc.auto)
			if err == nil {
				t.Fatal("a @template automation carrying a trigger was accepted; it would run twice")
			}
			if !strings.Contains(err.Error(), "@template") {
				t.Errorf("the refusal does not name @template: %v", err)
			}
		})
	}
}

// TestTheWorkTemplatesLoadAndAreCallable is the registration test.
//
// The registration IS the feature: a template the loader drops is a goal that
// opens a run naming an automation nobody can resolve, and the run dispatcher
// then fails it as `automation_not_runnable`. That reads as a broken goal
// rather than a missing construct, so it is asserted against the real tree.
func TestTheWorkTemplatesLoadAndAreCallable(t *testing.T) {
	all, err := NewLoader(LoaderOptions{}).LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	want := map[string]*Automation{"invokeAgent": nil, "produceArtifact": nil}
	for _, a := range all {
		if a == nil {
			continue
		}
		if _, ok := want[a.Name]; ok {
			want[a.Name] = a
		}
	}
	for name, a := range want {
		if a == nil {
			t.Errorf("automation %q did not load; a goal naming it would fail as automation_not_runnable", name)
			continue
		}
		if !a.IsTemplate() {
			t.Errorf("%s is not marked @template, so the trigger gate should have refused it -- which means the gate is not running either", name)
		}
		if a.Trigger != nil && a.Trigger.Event != "" {
			t.Errorf("%s carries an event trigger; a template must be called, not fired", name)
		}
		if a.Schedule != "" {
			t.Errorf("%s carries a schedule; a template must be called, not fired", name)
		}
		if len(a.Steps) == 0 {
			t.Errorf("%s has no steps", name)
		}
		if a.Args == nil || len(a.Args.Fields) == 0 {
			t.Errorf("%s declares no args; the goal's input binds INTO that contract, so without one nothing reaches the step", name)
		}
	}
}
