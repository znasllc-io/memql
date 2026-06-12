package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// TestBuildPhaseLayers_DAG: independent phases share a layer; dependents fall
// into later layers; order within a layer is by index.
func TestBuildPhaseLayers_DAG(t *testing.T) {
	// a, b independent (layer 0); c depends on a+b (layer 1); d depends on c (layer 2).
	phases := []phaseNode{
		{Name: "a"},
		{Name: "b"},
		{Name: "c", DependsOn: []string{"a", "b"}},
		{Name: "d", DependsOn: []string{"c"}},
	}
	layers := buildPhaseLayers(phases)
	if len(layers) != 3 {
		t.Fatalf("want 3 layers, got %d: %v", len(layers), layers)
	}
	if len(layers[0]) != 2 || layers[0][0] != 0 || layers[0][1] != 1 {
		t.Errorf("layer 0 should be [a,b] (indices 0,1), got %v", layers[0])
	}
	if len(layers[1]) != 1 || layers[1][0] != 2 {
		t.Errorf("layer 1 should be [c] (index 2), got %v", layers[1])
	}
	if len(layers[2]) != 1 || layers[2][0] != 3 {
		t.Errorf("layer 2 should be [d] (index 3), got %v", layers[2])
	}
}

// TestBuildPhaseLayers_NoDeps_AllParallel: with no dependsOn every phase is a
// root -> a single layer (all concurrent). (The sequential #1163 shape comes
// from the back-compat wrapper injecting implicit chain deps, not from here.)
func TestBuildPhaseLayers_NoDeps_AllParallel(t *testing.T) {
	layers := buildPhaseLayers([]phaseNode{{Name: "a"}, {Name: "b"}, {Name: "c"}})
	if len(layers) != 1 || len(layers[0]) != 3 {
		t.Fatalf("no-deps phases should collapse to one all-parallel layer, got %v", layers)
	}
}

// TestBuildPhaseLayers_CycleTerminates: a dependency cycle must not deadlock --
// the leftover phases collapse into singleton sequential layers.
func TestBuildPhaseLayers_CycleTerminates(t *testing.T) {
	phases := []phaseNode{
		{Name: "x", DependsOn: []string{"y"}},
		{Name: "y", DependsOn: []string{"x"}},
	}
	layers := buildPhaseLayers(phases)
	total := 0
	for _, l := range layers {
		total += len(l)
	}
	if total != 2 {
		t.Fatalf("cycle handling must still cover all phases exactly once, got %d in %v", total, layers)
	}
}

// TestSynthesizeParallelHeadline_EmitsParallelStep: a layer with 2+
// independent phases emits ONE `parallel { branches: [...] }` step so they
// run concurrently (memql#1164, restored on the authored grammar in
// memql#1368); the dependent phase is gated on that layer step succeeding.
func TestSynthesizeParallelHeadline_EmitsParallelStep(t *testing.T) {
	phases := []phaseNode{
		{Name: "fetchA"},
		{Name: "fetchB"},
		{Name: "merge", DependsOn: []string{"fetchA", "fetchB"}},
	}
	c := synthesizePhasedHeadline("gather", "Gather then merge.", phases)
	src := c.Source
	if !strings.Contains(src, "parallel {") || !strings.Contains(src, "branches: [") {
		t.Fatalf("independent phases must emit a parallel step:\n%s", src)
	}
	if !strings.Contains(src, "step fetchA { automation fetchA { } }") ||
		!strings.Contains(src, "step fetchB { automation fetchB { } }") {
		t.Errorf("parallel branches must invoke both independent phases:\n%s", src)
	}
	// The fan-out layer waits for every branch and fails on any branch
	// failure, so the downstream success gate is meaningful.
	if !strings.Contains(src, `wait: "all"`) || !strings.Contains(src, "failFast: true") {
		t.Errorf("the parallel layer must carry wait:\"all\" + failFast:true:\n%s", src)
	}
	// merge runs in a later step gated on the parallel layer's success.
	if !strings.Contains(src, "automation merge { }") ||
		!strings.Contains(src, `if steps.layer0.status == "success"`) {
		t.Errorf("dependent phase must be gated on the parallel layer succeeding:\n%s", src)
	}
	// Ordering: the parallel layer must precede merge (the executor runs
	// steps sequentially in list order; layering IS the order).
	if strings.Index(src, "step merge") < strings.Index(src, "step layer0") {
		t.Errorf("dependent phase must come after its dependency layer:\n%s", src)
	}
}

// TestSynthesizeParallelHeadline_RealGate1Compiles is the load-bearing test:
// the synthesized headline with a `parallel` layer must COMPILE through the
// real Gate-1 sandbox alongside its phase sub-automations.
func TestSynthesizeParallelHeadline_RealGate1Compiles(t *testing.T) {
	mk := func(name string) memql.SandboxConstruct {
		return memql.SandboxConstruct{Kind: "automation", Name: name,
			Source: "@description(\"" + name + "\")\nautomation " + name + " {\n  step run {\n    logic " + name + "Body { }\n  }\n}"}
	}
	mkLogic := func(name string) memql.SandboxConstruct {
		return memql.SandboxConstruct{Kind: "logic", Name: name + "Body",
			Source: "logic " + name + "Body {\n  body { return now }\n}"}
	}
	phases := []phaseNode{
		{Name: "fetchA"},
		{Name: "fetchB"},
		{Name: "merge", DependsOn: []string{"fetchA", "fetchB"}},
	}
	headline := synthesizePhasedHeadline("gather", "Gather in parallel then merge.", phases)

	bundle := []memql.SandboxConstruct{
		mk("fetchA"), mkLogic("fetchA"),
		mk("fetchB"), mkLogic("fetchB"),
		mk("merge"), mkLogic("merge"),
		headline,
	}
	if !strings.Contains(headline.Source, "parallel {") {
		t.Fatalf("the multi-phase layer must emit a parallel step (memql#1368); headline:\n%s", headline.Source)
	}
	report := memql.SandboxCompileBundle(bundle)
	requireAutomationsActuallyCompiled(t, report) // anti-vacuity (memql#1366)
	if !report.OK {
		var errs []string
		for _, d := range report.Diagnostics {
			if !d.OK && !d.Skipped {
				errs = append(errs, d.Kind+"/"+d.Name+": "+d.Error)
			}
		}
		t.Fatalf("parallel headline must compile through real Gate 1; errors:\n%s\n--- headline ---\n%s", strings.Join(errs, "\n"), headline.Source)
	}
}

// TestRunDesignPass_ResolvesPhaseDependsOn: dependsOn references (the model's
// phase names) survive the design pass and are remapped to the normalized
// sub-automation names the headline chains.
func TestRunDesignPass_ResolvesPhaseDependsOn(t *testing.T) {
	designOut := `{
      "automationName": "report",
      "automationPurpose": "Parallel fetch then merge.",
      "phases": [
        {"name": "fetchSales", "purpose": "sales", "dependencies": [
          {"kind": "spec", "name": "s1", "purpose": "p", "candidateSource": "spec s1 {\n  payload.active == true\n}"}]},
        {"name": "fetchSupport", "purpose": "support", "dependencies": [
          {"kind": "spec", "name": "s2", "purpose": "p", "candidateSource": "spec s2 {\n  payload.active == true\n}"}]},
        {"name": "merge", "purpose": "merge", "dependsOn": ["fetchSales", "fetchSupport"], "dependencies": [
          {"kind": "spec", "name": "s3", "purpose": "p", "candidateSource": "spec s3 {\n  payload.active == true\n}"}]}
      ]
    }`
	fe := designEngine(designOut, nil)
	l := newDesignLoop(fe)

	plan, err := l.runDesignPass(context.Background(), "Fetch sales + support in parallel, then merge.", "user-1", nil)
	if err != nil {
		t.Fatalf("runDesignPass: %v", err)
	}
	if len(plan.Phases) != 3 {
		t.Fatalf("want 3 phases, got %d", len(plan.Phases))
	}
	if len(plan.Phases[0].DependsOn) != 0 || len(plan.Phases[1].DependsOn) != 0 {
		t.Errorf("the two fetch phases must have no dependsOn (DAG roots)")
	}
	got := plan.Phases[2].DependsOn
	if len(got) != 2 || got[0] != "fetchSales" || got[1] != "fetchSupport" {
		t.Errorf("merge.dependsOn should remap to [fetchSales, fetchSupport], got %v", got)
	}
}

// TestSynthesizeHeadline_NoDeps_StillSequential: the #1163 back-compat wrapper
// (no dependsOn) must still produce a strict one-per-layer sequential chain --
// each phase gated on the prior phase's success.
func TestSynthesizeHeadline_NoDeps_StillSequential(t *testing.T) {
	c := synthesizeHeadlineAutomation("seq", "Sequential.", []string{"seqPhase0", "seqPhase1", "seqPhase2"})
	if strings.Contains(c.Source, "parallel {") {
		t.Fatalf("a no-dependsOn chain must stay sequential (no parallel step):\n%s", c.Source)
	}
	if !strings.Contains(c.Source, `if steps.seqPhase0.status == "success"`) ||
		!strings.Contains(c.Source, `if steps.seqPhase1.status == "success"`) {
		t.Errorf("sequential chain must gate each phase on the prior phase succeeding:\n%s", c.Source)
	}
}
