package automations

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// deployer_fleet_test.go -- what an automation declares about its model calls
// (docs/superpowers/specs/2026-10-31-deployer-fleet-design.md).

func TestUsesDeployerFleetCompiles(t *testing.T) {
	flagged := statementAutomation(t, "@template\n@usesDeployerFleet\nautomation probe { return true }")
	if !flagged.UsesDeployerFleet {
		t.Fatal("@usesDeployerFleet lost during compilation")
	}
	plain := statementAutomation(t, "@template\nautomation probe { return true }")
	if plain.UsesDeployerFleet {
		t.Fatal("an automation that declared nothing reads as declaring it")
	}
}

// D5: a workflow's domain is read from where it was loaded, never from its source.
func TestAnAutomationKnowsTheDomainItWasLoadedFrom(t *testing.T) {
	for origin, want := range map[string]string{
		"shop/automations.memql":        "shop",
		"shop/orders/automations.memql": "shop",
	} {
		a, err := NewLoader(LoaderOptions{}).CompileSource("@template\nautomation probe { return true }", origin)
		if err != nil {
			t.Fatalf("%s: %v", origin, err)
		}
		if a.Domain != want {
			t.Errorf("%s loaded into domain %q, want %q", origin, a.Domain, want)
		}
	}
}

// D12: a write must not wait on a person's laptop.
func TestUsesDeployerFleetRefusedOnABeforeWriteHook(t *testing.T) {
	a := &Automation{Name: "hook", UsesDeployerFleet: true, BeforeWrite: &BeforeWriteConfig{}}
	err := prepareDeployerFleetContract(a)
	if err == nil || !strings.Contains(err.Error(), "@usesDeployerFleet") {
		t.Fatalf("a before-write hook declaring it was accepted: %v", err)
	}
	a.BeforeWrite = nil
	if err := prepareDeployerFleetContract(a); err != nil {
		t.Fatalf("a plain automation was refused: %v", err)
	}
}

// fleetDeclarationProbe records what a step's context says its automation
// declared, by asking the engine the way the ai builtin does.
type fleetDeclarationProbe struct {
	domains map[string]string
}

func (p *fleetDeclarationProbe) Execute(ctx context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	if p.domains == nil {
		p.domains = map[string]string{}
	}
	domain, declared := memql.DeployerFleetDeclaration(ctx)
	if !declared {
		domain = "(none)"
	}
	p.domains[step.ID] = domain
	now := time.Now()
	return &StepResult{StepId: step.ID, Status: "completed", Result: map[string]any{"ok": true}, StartedAt: now, CompletedAt: now}, nil
}

// D11: each automation decides for itself. A flagged workflow's steps carry its
// domain, and an unflagged automation entered under one carries nothing.
func TestAnAutomationDecidesItsOwnDeclaration(t *testing.T) {
	probe := &fleetDeclarationProbe{}
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	defer e.Close()

	flagged := &Automation{Name: "flagged", UsesDeployerFleet: true, Domain: "shop", Steps: scheduledSteps("a")}
	if _, err := e.Execute(context.Background(), flagged, "manual"); err != nil {
		t.Fatal(err)
	}
	if probe.domains["a"] != "shop" {
		t.Fatalf("a flagged automation's step reads %q", probe.domains["a"])
	}

	inherited := memql.ContextWithDeployerFleet(context.Background(), true, "shop")
	plain := &Automation{Name: "plain", Domain: "shop", Steps: scheduledSteps("b")}
	if _, err := e.Execute(inherited, plain, "manual"); err != nil {
		t.Fatal(err)
	}
	if probe.domains["b"] != "(none)" {
		t.Fatalf("an unflagged automation inherited a person from its caller: %q", probe.domains["b"])
	}
}

// D11: a resumed run declares what its first attempt declared, and it is a resume that
// must not fall back to the ordinary rules: a failed brief resumed by anyone carries what
// the first attempt carried.
func TestAResumedRunKeepsItsDeclaration(t *testing.T) {
	probe := &fleetDeclarationProbe{}
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	defer e.Close()
	resume := func(ctx context.Context, a *Automation, step string) {
		t.Helper()
		journal := &RunJournal{
			RunId:          "run-" + step,
			AutomationName: a.Name,
			FailedStep:     step,
			StepStates:     states(step, StepState{Status: "failed", Attempt: 1}),
			TriggerEvent:   map[string]any{"topic": "probe.fired", "kind": "message", "payload": map[string]any{}},
		}
		if _, err := e.ResumeFrom(ctx, journal, a, &ResumeOptions{AllowSideEffects: true}); err != nil {
			t.Fatal(err)
		}
	}

	resume(context.Background(), &Automation{Name: "flagged", UsesDeployerFleet: true, Domain: "shop", Steps: scheduledSteps("a")}, "a")
	if probe.domains["a"] != "shop" {
		t.Fatalf("a resumed step reads %q", probe.domains["a"])
	}

	inherited := memql.ContextWithDeployerFleet(context.Background(), true, "shop")
	resume(inherited, &Automation{Name: "plain", Domain: "shop", Steps: scheduledSteps("b")}, "b")
	if probe.domains["b"] != "(none)" {
		t.Fatalf("a child resumed under a flagged parent inherited a person: %q", probe.domains["b"])
	}
}
