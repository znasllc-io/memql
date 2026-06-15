package planner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// --- pure synthesis -------------------------------------------------------

// TestSynthesizeSectionableBundle_EmitsParallelLayer: a sectionable deliverable
// with N independent sections synthesizes ONE layer-0 `parallel` block fanning
// out a production sub-automation per section, plus an assemble step gated on
// that layer succeeding (memql#1394 over the #1368 grammar).
func TestSynthesizeSectionableBundle_EmitsParallelLayer(t *testing.T) {
	dec := sectionableDecision{
		Sectionable: true,
		Assembly:    "Concatenate the three tales into one markdown file.",
		Sections: []sectionSpec{
			{Label: "Red Riding Hood", Instruction: "Write the full story of Red Riding Hood."},
			{Label: "Hansel & Gretel", Instruction: "Write the full story of Hansel and Gretel."},
			{Label: "Rapunzel", Instruction: "Write the full story of Rapunzel."},
		},
	}
	bundle, ok := synthesizeSectionableBundle("tales", "three German folk tales as full stories", dec)
	if !ok {
		t.Fatalf("3-section deliverable must synthesize a bundle")
	}
	// One production automation per section + 1 assemble + 1 headline.
	if got := len(bundle.Constructs); got != 5 {
		t.Fatalf("want 5 constructs (3 sections + assemble + headline), got %d: %+v", got, bundle.Constructs)
	}
	headline := lastConstruct(t, bundle, bundle.AutomationName)
	src := headline.Source
	if !strings.Contains(src, "parallel {") || !strings.Contains(src, "branches: [") {
		t.Fatalf("headline must fan the sections out in a parallel block:\n%s", src)
	}
	if !strings.Contains(src, `wait: "all"`) || !strings.Contains(src, "failFast: true") {
		t.Errorf("the section layer must carry wait:\"all\" + failFast:true:\n%s", src)
	}
	// The assemble step runs AFTER the parallel layer, gated on it succeeding.
	assemble := assembleAutomationName(bundle.AutomationName)
	if !strings.Contains(src, "automation "+assemble+" { }") {
		t.Errorf("headline must invoke the assemble sub-automation:\n%s", src)
	}
	if !strings.Contains(src, `if steps.layer0.status == "success"`) {
		t.Errorf("assemble must be gated on the parallel layer succeeding:\n%s", src)
	}
	if strings.Index(src, "step "+assemble) < strings.Index(src, "step layer0") {
		t.Errorf("assemble must come after the parallel section layer:\n%s", src)
	}
}

// TestSynthesizeSectionableBundle_RealGate1Compiles is the load-bearing test:
// the generated bundle (sections + assemble + headline + logic closure) must
// COMPILE through the real Gate-1 sandbox, proving the emitted parallel
// plan-automation is valid authored MemQL.
func TestSynthesizeSectionableBundle_RealGate1Compiles(t *testing.T) {
	dec := sectionableDecision{
		Sectionable: true,
		Sections: []sectionSpec{
			{Label: "vendor Acme", Instruction: "Profile vendor Acme."},
			{Label: "vendor Globex", Instruction: "Profile vendor Globex."},
			{Label: "vendor Initech", Instruction: "Profile vendor Initech."},
		},
	}
	bundle, ok := synthesizeSectionableBundle("vendors", "profile three vendors", dec)
	if !ok {
		t.Fatalf("synthesize failed")
	}
	bundle = withSectionableLogic(bundle)

	if !strings.Contains(lastConstruct(t, bundle, bundle.AutomationName).Source, "parallel {") {
		t.Fatalf("headline must emit a parallel step (memql#1368)")
	}

	report := memql.SandboxCompileBundle(bundle.Constructs)
	requireAutomationsActuallyCompiled(t, report) // anti-vacuity (memql#1366)
	if !report.OK {
		var errs []string
		for _, d := range report.Diagnostics {
			if !d.OK && !d.Skipped {
				errs = append(errs, d.Kind+"/"+d.Name+": "+d.Error)
			}
		}
		t.Fatalf("generated sectionable bundle must compile through real Gate 1; errors:\n%s", strings.Join(errs, "\n"))
	}
}

// TestSynthesizeSectionableBundle_DeclinesBelowFloor: a 1-section (or
// 0-section) result is not worth fanning out -- the generator declines so the
// caller routes normally.
func TestSynthesizeSectionableBundle_DeclinesBelowFloor(t *testing.T) {
	for _, n := range []int{0, 1} {
		secs := make([]sectionSpec, n)
		for i := range secs {
			secs[i] = sectionSpec{Label: "only"}
		}
		if _, ok := synthesizeSectionableBundle("x", "g", sectionableDecision{Sectionable: true, Sections: secs}); ok {
			t.Errorf("%d sections must NOT synthesize a fan-out", n)
		}
	}
}

// TestSynthesizeSectionableBundle_NotSectionable: sectionable=false declines
// regardless of section count.
func TestSynthesizeSectionableBundle_NotSectionable(t *testing.T) {
	dec := sectionableDecision{Sectionable: false, Sections: []sectionSpec{{Label: "a"}, {Label: "b"}}}
	if _, ok := synthesizeSectionableBundle("x", "g", dec); ok {
		t.Fatalf("sectionable=false must decline")
	}
}

// TestUsableSections_CapsAndUniquifies: the section count is bounded by the
// fan-out cap and colliding labels get unique sub-automation names.
func TestUsableSections_CapsAndUniquifies(t *testing.T) {
	// More than the cap -> truncated.
	many := make([]sectionSpec, maxSectionFanout+5)
	for i := range many {
		many[i] = sectionSpec{Label: "sec"}
	}
	got := sectionableDecision{Sections: many}.usableSections("h")
	if len(got) != maxSectionFanout {
		t.Fatalf("want %d capped sections, got %d", maxSectionFanout, len(got))
	}
	// All names unique despite identical labels.
	seen := map[string]bool{}
	for _, sp := range got {
		if seen[sp.Name] {
			t.Fatalf("duplicate section automation name %q", sp.Name)
		}
		seen[sp.Name] = true
	}
}

// TestSanitizeIdent: arbitrary labels reduce to safe DSL identifiers.
func TestSanitizeIdent(t *testing.T) {
	cases := map[string]string{
		"Red Riding Hood": "Red_Riding_Hood",
		"vendor (Acme!)":  "vendor_Acme",
		"  ":              "",
		"123start":        "s123start",
		"a-b_c":           "a_b_c",
	}
	for in, want := range cases {
		if got := sanitizeIdent(in); got != want {
			t.Errorf("sanitizeIdent(%q)=%q, want %q", in, got, want)
		}
	}
}

// --- parse off the shared triage response ---------------------------------

// TestParseSectionableDecision_ReadsShape: the sectionable fields are read off
// the SAME goalComplexityTriage response that carries the complexity verdict.
func TestParseSectionableDecision_ReadsShape(t *testing.T) {
	resp := map[string]any{
		"complexity":  "moderate",
		"reasoning":   "ten full stories",
		"sectionable": true,
		"assembly":    "concatenate into one file",
		"sections": []any{
			map[string]any{"label": "one", "instruction": "write one"},
			map[string]any{"label": "two", "instruction": "write two"},
		},
	}
	dec := parseSectionableDecision(resp)
	if !dec.Sectionable || len(dec.Sections) != 2 || dec.Assembly != "concatenate into one file" {
		t.Fatalf("did not parse sectionable shape: %+v", dec)
	}
	// And the complexity parse still works off the same payload.
	c, _, err := parseGoalComplexity(resp)
	if err != nil || c != complexityModerate {
		t.Fatalf("complexity parse off shared payload: c=%q err=%v", c, err)
	}
}

// TestParseSectionableDecision_MissingFieldsAreNonSectionable: a response with
// no sectionable block (the pre-#1394 shape) yields a non-sectionable decision
// and never errors -- back-compat for every existing triage caller.
func TestParseSectionableDecision_MissingFieldsAreNonSectionable(t *testing.T) {
	dec := parseSectionableDecision(map[string]any{"complexity": "trivial", "reasoning": "x"})
	if dec.Sectionable || len(dec.Sections) != 0 {
		t.Fatalf("missing sectionable block must be non-sectionable, got %+v", dec)
	}
	// nil / junk also safe.
	if parseSectionableDecision(nil).Sectionable {
		t.Fatalf("nil response must be non-sectionable")
	}
}

// --- end-to-end generate + persist ----------------------------------------

// sectionableTriageJSON builds a goalComplexityTriage response that classifies
// the goal as moderate AND sectionable with n sections.
func sectionableTriageJSON(t *testing.T, n int) string {
	t.Helper()
	secs := make([]map[string]any, n)
	for i := 0; i < n; i++ {
		secs[i] = map[string]any{"label": "tale", "instruction": "write the full tale"}
	}
	b, _ := json.Marshal(map[string]any{
		"complexity":  "moderate",
		"reasoning":   "many full stories",
		"sectionable": true,
		"assembly":    "concatenate into one markdown file",
		"sections":    secs,
	})
	return string(b)
}

// newSectionableLoop builds a loop whose engine satisfies authoringSandbox
// (clean Gate-1) and answers the triage prompt with a sectionable verdict.
func newSectionableLoop(t *testing.T, triageOut string, planRow map[string]any) (*PlannerAgentLoop, *fakeCaptureEngine) {
	t.Helper()
	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			if strings.Contains(query, "queryPlanById") {
				return map[string]any{"output": []any{planRow}}, nil
			}
			return nil, nil
		},
		siResponder: func(templateId string, _ map[string]any) (any, error) {
			if templateId == "goalComplexityTriage" {
				return triageOut, nil
			}
			return nil, nil
		},
	}
	// Clean Gate-1 for whatever bundle is compiled.
	ce := &fakeCaptureEngine{fakeEngine: fe, sandbox: &fakeSandbox{reports: []memql.SandboxReport{{OK: true}}}}
	return &PlannerAgentLoop{engine: ce, logger: authoringTestLogger()}, ce
}

// TestMaybeGenerateSectionable_PersistsParallelAutomation: a sectionable
// moderate deliverable generates the parallel plan-automation, Gate-1 compiles
// it, and persists it through the authoring-bundle pipeline (create bundle +
// one construct per member + a validated record).
func TestMaybeGenerateSectionable_PersistsParallelAutomation(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_SECTIONABLE_ENABLED", "1")
	plan := map[string]any{
		"id":          "v1:planner:plan:p1",
		"kind":        produceArtifactPlanKind,
		"status":      "planning",
		"goal":        "a markdown file with 10 folk tales, each a complete story",
		"requestedBy": "user-1",
	}
	l, ce := newSectionableLoop(t, sectionableTriageJSON(t, 4), plan)
	dec := parseSectionableDecision(sectionableTriageJSON(t, 4))

	handled, err := l.maybeGenerateSectionable(context.Background(), "v1:planner:plan:p1", plan, dec)
	if err != nil || !handled {
		t.Fatalf("sectionable deliverable must generate + persist: handled=%v err=%v", handled, err)
	}
	exec, _, _ := ce.snapshot()
	if countContains(exec, "mutationCreateAuthoringBundle") != 1 {
		t.Fatalf("expected 1 mutationCreateAuthoringBundle, got %d", countContains(exec, "mutationCreateAuthoringBundle"))
	}
	// 4 sections + assemble + headline = 6 automations + 5 logic = 11 constructs.
	if got := countContains(exec, "mutationCreateAuthoringConstruct"); got != 11 {
		t.Fatalf("expected 11 mutationCreateAuthoringConstruct, got %d", got)
	}
	if countContains(exec, "mutationRecordBundleValidation") != 1 {
		t.Fatalf("expected 1 mutationRecordBundleValidation, got %d", countContains(exec, "mutationRecordBundleValidation"))
	}
}

// TestMaybeGenerateSectionable_NoSandboxFallsThrough: a binary without the
// Gate-1 seam can't validate the generated automation, so generation declines
// (handled=false) and the Plan routes through the normal decompose loop.
func TestMaybeGenerateSectionable_NoSandboxFallsThrough(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_SECTIONABLE_ENABLED", "1")
	plan := map[string]any{"goal": "g", "requestedBy": "u1"}
	// Bare fakeEngine does NOT implement authoringSandbox.
	l := &PlannerAgentLoop{engine: &fakeEngine{}, logger: authoringTestLogger()}
	dec := sectionableDecision{Sectionable: true, Sections: []sectionSpec{{Label: "a"}, {Label: "b"}}}
	handled, err := l.maybeGenerateSectionable(context.Background(), "p1", plan, dec)
	if err != nil || handled {
		t.Fatalf("no-sandbox binary must fall through: handled=%v err=%v", handled, err)
	}
}

// TestMaybeGenerateSectionable_Gate1FailureFallsThrough: a generated automation
// that fails Gate-1 is NOT persisted; generation declines so the Plan routes
// normally (generation is an optimization, never a hard requirement).
func TestMaybeGenerateSectionable_Gate1FailureFallsThrough(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_SECTIONABLE_ENABLED", "1")
	plan := map[string]any{"id": "p1", "goal": "g", "requestedBy": "u1"}
	fe := &fakeEngine{execResponder: func(q string) (any, error) { return nil, nil }}
	ce := &fakeCaptureEngine{fakeEngine: fe, sandbox: &fakeSandbox{reports: []memql.SandboxReport{{OK: false, Diagnostics: []memql.SandboxDiagnostic{{Kind: "automation", Name: "x", OK: false, Error: "boom"}}}}}}
	l := &PlannerAgentLoop{engine: ce, logger: authoringTestLogger()}
	dec := sectionableDecision{Sectionable: true, Sections: []sectionSpec{{Label: "a"}, {Label: "b"}}}
	handled, err := l.maybeGenerateSectionable(context.Background(), "p1", plan, dec)
	if err != nil || handled {
		t.Fatalf("Gate-1 failure must fall through, not persist: handled=%v err=%v", handled, err)
	}
	if exec, _, _ := ce.snapshot(); countContains(exec, "mutationCreateAuthoringBundle") != 0 {
		t.Fatalf("a Gate-1 failure must NOT persist a bundle")
	}
}

// TestMaybeGenerateSectionable_DisabledFallsThrough: the env kill-switch routes
// every sectionable deliverable back to the decompose loop.
func TestMaybeGenerateSectionable_DisabledFallsThrough(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_SECTIONABLE_ENABLED", "0")
	plan := map[string]any{"id": "p1", "goal": "g", "requestedBy": "u1"}
	l, _ := newSectionableLoop(t, sectionableTriageJSON(t, 3), plan)
	dec := parseSectionableDecision(sectionableTriageJSON(t, 3))
	handled, err := l.maybeGenerateSectionable(context.Background(), "p1", plan, dec)
	if err != nil || handled {
		t.Fatalf("disabled flag must fall through: handled=%v err=%v", handled, err)
	}
}

// --- triage routing -------------------------------------------------------

// TestTriage_SectionableModerate_GeneratesParallel: a moderate + sectionable
// produceArtifact goal routes through generation (handled=true) and makes ZERO
// plannerAgent decompose calls -- the parallel plan-automation replaces the
// serial chain.
func TestTriage_SectionableModerate_GeneratesParallel(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_SECTIONABLE_ENABLED", "1")
	t.Setenv("MEMQL_PLANNER_GOAL_TRIAGE_ENABLED", "1")
	plan := map[string]any{
		"id":           "v1:planner:plan:p1",
		"kind":         produceArtifactPlanKind,
		"status":       "planning",
		"goal":         "a file with 10 folk tales, each a complete story",
		"requestedBy":  "user-1",
		"ownerAgentId": "agent-1",
	}
	l, ce := newSectionableLoop(t, sectionableTriageJSON(t, 5), plan)
	handled, err := l.triageAndMaybeShortcut(context.Background(), "v1:planner:plan:p1", "")
	if err != nil || !handled {
		t.Fatalf("sectionable moderate must be handled via generation: handled=%v err=%v", handled, err)
	}
	exec, si, _ := ce.snapshot()
	if countContains(si, "plannerAgent") != 0 {
		t.Fatalf("sectionable generation must make ZERO plannerAgent decompose calls, got %d", countContains(si, "plannerAgent"))
	}
	if countContains(si, "goalComplexityTriage") != 1 {
		t.Fatalf("expected exactly 1 cheap triage call, got %d", countContains(si, "goalComplexityTriage"))
	}
	if countContains(exec, "mutationCreateAuthoringBundle") != 1 {
		t.Fatalf("expected the generated parallel automation to be persisted once, got %d", countContains(exec, "mutationCreateAuthoringBundle"))
	}
	// Not a single direct turn -- the parallel automation IS the execution.
	if countContains(exec, "mutationStartPlan") != 0 {
		t.Fatalf("sectionable generation must not shortcut to a single direct turn")
	}
}

// TestTriage_NonSectionableModerate_FallsThroughToDecompose: a moderate goal
// the classifier does NOT mark sectionable still falls through to the decompose
// loop (the #1393 behavior is preserved when generation declines).
func TestTriage_NonSectionableModerate_FallsThroughToDecompose(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_SECTIONABLE_ENABLED", "1")
	t.Setenv("MEMQL_PLANNER_GOAL_TRIAGE_ENABLED", "1")
	plan := map[string]any{
		"id":           "p1",
		"kind":         produceArtifactPlanKind,
		"status":       "planning",
		"goal":         "compare two documents and write up the differences",
		"requestedBy":  "user-1",
		"ownerAgentId": "agent-1",
	}
	// moderate but NOT sectionable.
	triage, _ := json.Marshal(map[string]any{"complexity": "moderate", "reasoning": "a few steps", "sectionable": false})
	l, ce := newSectionableLoop(t, string(triage), plan)
	handled, err := l.triageAndMaybeShortcut(context.Background(), "p1", "")
	if err != nil || handled {
		t.Fatalf("non-sectionable moderate must fall through to decompose: handled=%v err=%v", handled, err)
	}
	if exec, _, _ := ce.snapshot(); countContains(exec, "mutationCreateAuthoringBundle") != 0 {
		t.Fatalf("non-sectionable goal must NOT generate a parallel automation")
	}
}

// lastConstruct returns the construct in the bundle with the given name.
func lastConstruct(t *testing.T, bundle authoringBundle, name string) memql.SandboxConstruct {
	t.Helper()
	for _, c := range bundle.Constructs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("construct %q not in bundle", name)
	return memql.SandboxConstruct{}
}
