package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// TestExtractJSONObject_StripsProseWrapper: the exact failure from the live
// test -- the repair model wrapped its JSON in prose -- now parses, because
// extractJSONObject pulls out the first balanced object.
func TestExtractJSONObject_StripsProseWrapper(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", `{"constructs":[]}`, `{"constructs":[]}`},
		{"prose preamble", "Looking at each diagnostic:\n\n1. fix the spec\n\n{\"constructs\":[{\"kind\":\"spec\"}]}", `{"constructs":[{"kind":"spec"}]}`},
		{"prose pre+post", "Here is the corrected source:\n{\"constructs\":[]}\nLet me know if you need more.", `{"constructs":[]}`},
		{"fenced", "```json\n{\"constructs\":[]}\n```", `{"constructs":[]}`},
		{"brace inside string", `prefix {"name":"a}b","kind":"spec"} suffix`, `{"name":"a}b","kind":"spec"}`},
		{"nested objects", `note {"a":{"b":1},"c":2} end`, `{"a":{"b":1},"c":2}`},
	}
	for _, c := range cases {
		got := string(extractJSONObject([]byte(c.in)))
		if got != c.want {
			t.Errorf("%s: extractJSONObject = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestParseEmittedConstructs_ToleratesProse: end-to-end, the emit/repair parser
// now recovers a prose-wrapped construct list instead of erroring.
func TestParseEmittedConstructs_ToleratesProse(t *testing.T) {
	resp := "Looking at each diagnostic:\n1. The spec needed `payload`.\n\n" +
		`{"constructs":[{"kind":"spec","name":"specActive","source":"spec activeRowTrait specActive {\n  return active == true\n}"}]}` +
		"\n\nThat should compile now."
	got, err := parseEmittedConstructs(resp)
	if err != nil {
		t.Fatalf("parseEmittedConstructs should recover prose-wrapped JSON: %v", err)
	}
	if len(got) != 1 || got[0].Name != "specActive" {
		t.Fatalf("want 1 construct specActive, got %+v", got)
	}
}

// TestFlattenToSingleDeliverable_MergesPhases: a multi-phase plan flattens into
// one automation with the union of phase deps (deduped); a single-phase plan is
// unchanged.
func TestFlattenToSingleDeliverable_MergesPhases(t *testing.T) {
	dep := func(kind, name string) resolvedDependency {
		return resolvedDependency{designDependency: designDependency{Kind: kind, Name: name}, Disposition: dispAuthor}
	}
	multi := designPlan{
		AutomationName: "writeArticle",
		Phases: []resolvedPhase{
			{Name: "writeArticlePhase0", Dependencies: []resolvedDependency{dep("logic", "gather"), dep("spec", "shared")}},
			{Name: "writeArticlePhase1", Dependencies: []resolvedDependency{dep("logic", "render"), dep("spec", "shared")}},
		},
	}
	got := flattenToSingleDeliverable(multi)
	if got.isMultiPhase() {
		t.Fatal("flattened plan must not be multi-phase")
	}
	if len(got.Phases) != 0 {
		t.Errorf("phases must be cleared, got %d", len(got.Phases))
	}
	// gather, shared, render -- 'shared' deduped across the two phases.
	if len(got.Dependencies) != 3 {
		t.Fatalf("want 3 deduped deps, got %d: %+v", len(got.Dependencies), got.Dependencies)
	}
	names := map[string]bool{}
	for _, d := range got.Dependencies {
		names[d.Name] = true
	}
	for _, want := range []string{"gather", "shared", "render"} {
		if !names[want] {
			t.Errorf("flattened deps missing %q", want)
		}
	}

	// Single-phase is untouched.
	single := designPlan{AutomationName: "x", Dependencies: []resolvedDependency{dep("spec", "s")}}
	if out := flattenToSingleDeliverable(single); out.isMultiPhase() || len(out.Dependencies) != 1 {
		t.Errorf("single-phase plan should be unchanged, got %+v", out)
	}
}

// TestRunCapture_FlattensMultiPhaseDesign: the capture path forces a single-
// automation bundle even when the design pass came back multi-phase -- proving
// the over-decomposition fix is wired into runCapture (no parallel/headline
// constructs, exactly one automation).
func TestRunCapture_FlattensMultiPhaseDesign(t *testing.T) {
	t.Setenv("MEMQL_AUTHORING_CAPTURE_ENABLED", "1")
	plan := capturePlanRow("p-flat", "user-7", "Write a list of birds")
	// Design returns a 2-phase decomposition; emit echoes one automation + a
	// spec per authoringEmit call.
	designOut := `{
      "automationName": "birdList",
      "automationPurpose": "Make a bird list.",
      "phases": [
        {"name": "birdListPhase0", "purpose": "gather", "dependencies": [
          {"kind":"spec","name":"specA","purpose":"p","candidateSource":"spec activeRowTrait specA {\n  return active == true\n}"}]},
        {"name": "birdListPhase1", "purpose": "render", "dependencies": [
          {"kind":"spec","name":"specB","purpose":"p","candidateSource":"spec activeRowTrait specB {\n  return active == true\n}"}]}
      ]
    }`
	fe := &fakeEngine{
		aiResponder: func(templateId string, data map[string]any) (any, error) {
			switch templateId {
			case "authoringDesign":
				return designOut, nil
			case "authoringEmit":
				name, _ := data["automationName"].(string)
				auto := memql.SandboxConstruct{Kind: "automation", Name: name, Source: "automation " + name + " { }"}
				spec := memql.SandboxConstruct{Kind: "spec", Name: "specA", Source: "spec activeRowTrait specA {\n  return active == true\n}"}
				return emitJSON(t, []memql.SandboxConstruct{auto, spec}), nil
			}
			return nil, nil
		},
		execResponder: func(query string) (any, error) {
			switch {
			case strings.Contains(query, "authoringBundleForPlan"):
				return map[string]any{"output": []any{}}, nil
			case strings.Contains(query, "planById"):
				return map[string]any{"output": []any{plan}}, nil
			case strings.Contains(query, "cataloguedConstructsForOwner"):
				return map[string]any{"output": []any{}}, nil
			}
			return nil, nil
		},
	}
	ce := &fakeCaptureEngine{fakeEngine: fe, sandbox: &fakeSandbox{reports: []memql.SandboxReport{okReport()}}, dryRun: memql.BundleDryRunReport{OK: true}}
	loop := &PlannerAgentLoop{engine: ce, logger: authoringTestLogger()}
	d := NewAuthoringCaptureDispatcher(loop, ce, authoringTestLogger())

	if err := d.runCapture(context.Background(), "p-flat", "produceArtifact"); err != nil {
		t.Fatalf("runCapture: %v", err)
	}

	// Exactly ONE automation construct persisted (the flat headline) -- no phase
	// sub-automations, no synthesized chaining headline.
	exec, _, _ := ce.snapshot()
	autos := 0
	for _, q := range exec {
		if strings.Contains(q, "createAuthoringConstruct") && strings.Contains(q, `"kind":"automation"`) {
			autos++
		}
	}
	if autos != 1 {
		t.Fatalf("flattened capture must persist exactly 1 automation, got %d (exec=%v)", autos, exec)
	}
}
