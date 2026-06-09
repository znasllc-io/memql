package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// TestSynthesizeHeadline_ChainsPhasesInOrder: the synthesized headline names
// every phase automation, runs phase 0 unconditionally, and gates each later
// phase on the prior step's result (the inter-step reference that forces
// sequential ordering).
func TestSynthesizeHeadline_ChainsPhasesInOrder(t *testing.T) {
	c := synthesizeHeadlineAutomation("doTheThing", "Do the thing in phases.", []string{"doTheThingPhase0", "doTheThingPhase1", "doTheThingPhase2"})
	if c.Kind != "automation" || c.Name != "doTheThing" {
		t.Fatalf("headline should be automation doTheThing, got %s/%s", c.Kind, c.Name)
	}
	src := c.Source
	for _, name := range []string{"doTheThingPhase0", "doTheThingPhase1", "doTheThingPhase2"} {
		if !strings.Contains(src, "automation "+name+" {") {
			t.Errorf("headline must chain %s; source:\n%s", name, src)
		}
	}
	// Phase 0 unconditional; phase 1 gated on phase 0; phase 2 on phase 1.
	if !strings.Contains(src, "if steps.phase0.result") {
		t.Errorf("phase 1 must be gated on phase 0's result:\n%s", src)
	}
	if !strings.Contains(src, "if steps.phase1.result") {
		t.Errorf("phase 2 must be gated on phase 1's result:\n%s", src)
	}
	// The ordering reference must point BACKWARD: phase0's step has no "if".
	p0 := strings.Index(src, "step phase0")
	p1 := strings.Index(src, "step phase1")
	if p0 < 0 || p1 < 0 || p0 > p1 {
		t.Errorf("phase steps must appear in order phase0 < phase1:\n%s", src)
	}
}

// TestSynthesizeHeadline_RealGate1Compiles is the load-bearing test: a
// multi-phase bundle (two trigger-less phase sub-automations + the
// Go-synthesized headline that chains them) must COMPILE through the real
// Gate-1 sandbox. Proves the `step { if steps.X.result { automation Y { } } }`
// grammar the synthesizer emits is real + bindable.
func TestSynthesizeHeadline_RealGate1Compiles(t *testing.T) {
	phase0 := memql.SandboxConstruct{Kind: "automation", Name: "digestPhase0",
		Source: "@description(\"phase 0\")\nautomation digestPhase0 {\n  step run {\n    logic digestPhase0Body { }\n  }\n}"}
	phase1 := memql.SandboxConstruct{Kind: "automation", Name: "digestPhase1",
		Source: "@description(\"phase 1\")\nautomation digestPhase1 {\n  step run {\n    logic digestPhase1Body { }\n  }\n}"}
	l0 := memql.SandboxConstruct{Kind: "logic", Name: "digestPhase0Body",
		Source: "logic digestPhase0Body {\n  body { return now }\n}"}
	l1 := memql.SandboxConstruct{Kind: "logic", Name: "digestPhase1Body",
		Source: "logic digestPhase1Body {\n  body { return now }\n}"}
	headline := synthesizeHeadlineAutomation("digest", "Run the digest in two phases.", []string{"digestPhase0", "digestPhase1"})

	report := memql.SandboxCompileBundle([]memql.SandboxConstruct{phase0, phase1, l0, l1, headline})
	if !report.OK {
		var errs []string
		for _, d := range report.Diagnostics {
			if !d.OK && !d.Skipped {
				errs = append(errs, d.Kind+"/"+d.Name+": "+d.Error)
			}
		}
		t.Fatalf("synthesized multi-phase bundle must compile through real Gate 1; errors:\n%s\n--- headline ---\n%s", strings.Join(errs, "\n"), headline.Source)
	}
}

// TestEmitBundle_MultiPhase: a 2-phase design plan emits one sub-automation per
// phase (via per-phase authoringEmit), the synthesized headline, and the
// phases' authored deps -- and bundleAutomationSource resolves to the headline.
func TestEmitBundle_MultiPhase(t *testing.T) {
	// Each per-phase authoringEmit call returns that phase's automation + a
	// spec dep. The fake returns the same shape for every authoringEmit call;
	// the emit code normalizes the automation name to the phase name.
	p0auto := memql.SandboxConstruct{Kind: "automation", Name: "ignored0", Source: "automation ignored0 { }"}
	p0spec := memql.SandboxConstruct{Kind: "spec", Name: "specPhase0", Source: "spec specPhase0 {\n  payload.active == true\n}"}
	fe := &fakeEngine{
		siResponder: func(templateId string, data map[string]any) (any, error) {
			if templateId == "authoringEmit" {
				// Echo the requested automation name so the per-phase emit has a
				// realistic automation construct to normalize.
				name, _ := data["automationName"].(string)
				auto := memql.SandboxConstruct{Kind: "automation", Name: name, Source: "automation " + name + " { }"}
				return emitJSON(t, []memql.SandboxConstruct{auto, p0spec}), nil
			}
			return nil, nil
		},
	}
	_ = p0auto
	l := newDesignLoop(fe)

	plan := designPlan{
		AutomationName:    "weeklyReport",
		AutomationPurpose: "Build the weekly report in phases.",
		Phases: []resolvedPhase{
			{Name: "weeklyReportPhase0", Purpose: "gather", Dependencies: []resolvedDependency{{
				designDependency: designDependency{Kind: "spec", Name: "specPhase0", CandidateSource: p0spec.Source}, Disposition: dispAuthor}}},
			{Name: "weeklyReportPhase1", Purpose: "assemble", Dependencies: []resolvedDependency{{
				designDependency: designDependency{Kind: "spec", Name: "specPhase0", CandidateSource: p0spec.Source}, Disposition: dispAuthor}}},
		},
	}
	if !plan.isMultiPhase() {
		t.Fatal("plan should be multi-phase")
	}

	bundle, err := l.emitBundle(context.Background(), "Build the weekly report.", plan)
	if err != nil {
		t.Fatalf("emitBundle (multi-phase): %v", err)
	}

	names := map[string]string{} // name -> kind
	autos := 0
	for _, c := range bundle.Constructs {
		names[c.Name] = c.Kind
		if c.Kind == "automation" {
			autos++
		}
	}
	for _, want := range []string{"weeklyReport", "weeklyReportPhase0", "weeklyReportPhase1"} {
		if names[want] != "automation" {
			t.Errorf("multi-phase bundle missing automation %q (constructs: %v)", want, names)
		}
	}
	if autos != 3 {
		t.Errorf("want 3 automations (headline + 2 phases), got %d", autos)
	}
	src, ok := bundleAutomationSource(bundle)
	if !ok {
		t.Fatal("bundleAutomationSource should resolve the headline")
	}
	if !strings.Contains(src, "step phase0") || !strings.Contains(src, "step phase1") {
		t.Errorf("headline source should chain phase0 + phase1:\n%s", src)
	}
}

// TestRunDesignPass_ResolvesPhases: an authoringDesign result carrying phases
// produces a multi-phase plan with each phase's deps resolved.
func TestRunDesignPass_ResolvesPhases(t *testing.T) {
	designOut := `{
      "automationName": "onboardUser",
      "automationPurpose": "Onboard in phases.",
      "phases": [
        {"name": "onboardUserPhase0", "purpose": "create", "dependencies": [
          {"kind": "spec", "name": "specNewUser", "purpose": "new", "candidateSource": "spec specNewUser {\n  payload.active == true\n}"}]},
        {"name": "onboardUserPhase1", "purpose": "notify", "dependencies": [
          {"kind": "spec", "name": "specNotify", "purpose": "notify", "candidateSource": "spec specNotify {\n  payload.active == true\n}"}]}
      ]
    }`
	fe := designEngine(designOut, nil)
	l := newDesignLoop(fe)

	plan, err := l.runDesignPass(context.Background(), "Onboard a new user across phases.", "user-1", nil)
	if err != nil {
		t.Fatalf("runDesignPass: %v", err)
	}
	if !plan.isMultiPhase() {
		t.Fatalf("expected a multi-phase plan, got %d phases", len(plan.Phases))
	}
	if len(plan.Phases) != 2 {
		t.Fatalf("want 2 phases, got %d", len(plan.Phases))
	}
	if plan.Phases[0].Name != "onboardUserPhase0" || plan.Phases[1].Name != "onboardUserPhase1" {
		t.Errorf("phase names not carried: %q %q", plan.Phases[0].Name, plan.Phases[1].Name)
	}
	for i, ph := range plan.Phases {
		if len(ph.Dependencies) != 1 || ph.Dependencies[0].Disposition != dispAuthor {
			t.Errorf("phase %d should have 1 authored dep, got %+v", i, ph.Dependencies)
		}
	}
}
