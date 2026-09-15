package planner

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// --- pure synthesis -------------------------------------------------------

// TestSynthesizeSectionableBundle_EmitsParallelLayer: a sectionable deliverable
// with N independent sections synthesizes ONE layer-0 `parallel` statement
// with a branch per section's production sub-automation, plus the assemble
// call after it (memql#1394 over the #1368 grammar).
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
	if !strings.Contains(src, "  parallel {\n") || strings.Count(src, "    branch ") != 3 {
		t.Fatalf("headline must fan the three sections out in one parallel statement:\n%s", src)
	}
	// The assemble call runs AFTER the parallel layer: statements run in
	// order and a failed branch fails the parallel, which ends the run.
	assemble := assembleAutomationName(bundle.AutomationName)
	at := strings.Index(src, "  automation "+assemble+"()\n")
	if at < 0 {
		t.Errorf("headline must call the assemble sub-automation:\n%s", src)
	}
	if at < strings.Index(src, "  parallel {") {
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
		t.Fatalf("headline must emit a parallel statement (memql#1368)")
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

// The tests that used to follow here drove `maybeGenerateSectionable` and the
// plan-routing triage -- the SYNTHESIS half of this file and
// agent_loop_sectionable_generate.go -- both deleted with the plan loop
// (memql#5052). What they asserted was that a sectionable moderate goal
// generated a parallel automation and persisted it INTO A PLAN, and there are
// no Plans.
//
// The pure half above is what work_compile.go still reaches: the parser
// (parseSectionableDecision) and the section shaping (usableSections,
// sanitizeIdent). The bundle synthesis (synthesizeSectionableBundle) has no
// caller outside these tests.

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
