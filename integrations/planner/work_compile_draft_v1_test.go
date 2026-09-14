package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// work_compile_draft_v1_test.go -- the work draft follows the grammar the
// engine parses with (langparser.DefaultOptions), so the flip of the tree to
// edition 2026 flips what the generator writes with it (memql#5367).

// workDraftVariants synthesizes the draft for every route shape: one turn or
// sections, a text answer or a native file.
func workDraftVariants(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, sectionable := range []bool{false, true} {
		for _, requiresFile := range []bool{false, true} {
			decision := parseSectionableDecision(map[string]any{
				"sectionable": sectionable, "requiresFile": requiresFile,
				"fileName": "report", "fileFormat": "markdown",
				"sections": []map[string]any{{"label": "one", "instruction": "first"}, {"label": "two", "instruction": "second"}},
			})
			bundle, err := synthesizeWorkReasoningBundle(compileReq(), "agent-1", decision)
			if err != nil {
				t.Fatal(err)
			}
			name := "turn"
			if sectionable {
				name = "sections"
			}
			if requiresFile {
				name += "+file"
			}
			out[name] = bundle.Constructs[0].Source
		}
	}
	return out
}

// TestSectionableBundleCompilesInEdition2026: the sectionable generator's
// logic closure -- `body { return now }` -- and its automations are written
// the same in both grammars, and compile through the real Gate 1 with
// edition 2026 the default: the logic body is a one-`return` expression,
// which the loader's bridge runs on the LogicRunner
// (component/memql/logic_body_v1.go).
func TestSectionableBundleCompilesInEdition2026(t *testing.T) {
	saved := langparser.DefaultOptions
	langparser.DefaultOptions = langparser.Options{ExpressionsV1: true}
	t.Cleanup(func() { langparser.DefaultOptions = saved })

	bundle, ok := synthesizeSectionableBundle("vendors", "profile two vendors", sectionableDecision{
		Sectionable: true,
		Sections:    []sectionSpec{{Label: "vendor Acme", Instruction: "Profile Acme."}, {Label: "vendor Globex", Instruction: "Profile Globex."}},
	})
	if !ok {
		t.Fatal("synthesize failed")
	}
	bundle = withSectionableLogic(bundle)
	report := memql.SandboxCompileBundle(bundle.Constructs)
	requireAutomationsActuallyCompiled(t, report)
	if !report.OK {
		t.Fatalf("the sectionable bundle does not compile in edition 2026: %s", firstFailureHeadline(report))
	}
}

// TestWorkDraftLegacyTextIsUnchanged: with the legacy grammar the default,
// the draft is written exactly as it was.
func TestWorkDraftLegacyTextIsUnchanged(t *testing.T) {
	if langparser.DefaultOptions.ExpressionsV1 {
		t.Skip("the tree parses edition 2026 by default")
	}
	for name, src := range workDraftVariants(t) {
		if !strings.Contains(src, `, field(event, "payload")`) || !strings.Contains(src, "concat(") {
			t.Fatalf("%s: the legacy draft lost its concat(..., field(event, \"payload\")):\n%s", name, src)
		}
		if strings.Contains(src, "toString(") || strings.Contains(src, " + ") {
			t.Fatalf("%s: the legacy draft carries edition-2026 text:\n%s", name, src)
		}
	}
}

// TestWorkDraftFollowsEdition2026: with edition 2026 the default, every draft
// shape passes Gate 1 and loads as the engine loads a validated headline --
// and the prompt a step passes evaluates to the statement, the heading and
// the goal input as the JSON its heading promises.
func TestWorkDraftFollowsEdition2026(t *testing.T) {
	saved := langparser.DefaultOptions
	langparser.DefaultOptions = langparser.Options{ExpressionsV1: true}
	t.Cleanup(func() { langparser.DefaultOptions = saved })

	for name, src := range workDraftVariants(t) {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(src, "concat(") || strings.Contains(src, "field(") {
				t.Fatalf("the edition-2026 draft carries a retired call:\n%s", src)
			}
			construct := memql.SandboxConstruct{Kind: "automation", Name: "workRun_v1_work_run_r1", Source: src}
			report := memql.SandboxCompileBundle([]memql.SandboxConstruct{construct})
			requireAutomationsActuallyCompiled(t, report)
			if !report.OK {
				t.Fatalf("Gate 1 refused the draft: %s\n%s", firstFailureHeadline(report), src)
			}
			auto, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(src, "work-compile/"+construct.Name+".memql")
			if err != nil {
				t.Fatalf("load: %v\n%s", err, src)
			}
			if !auto.IsV1() {
				t.Fatal("the draft did not load as an edition-2026 automation")
			}
			if name != "turn" {
				return
			}
			e := automations.NewEvaluator()
			e.SetCustom("event", map[string]any{"payload": map[string]any{"audience": "board"}})
			step := auto.Steps[0]
			args, err := e.ResolveV1Map(context.Background(), step.Function.Args)
			if err != nil {
				t.Fatalf("resolve the step's arguments: %v", err)
			}
			want := compileReq().Statement + "\n\nGoal input (JSON):\n" + `{"audience":"board"}`
			if args["prompt"] != want {
				t.Fatalf("prompt = %q, want %q", args["prompt"], want)
			}
		})
	}
}
