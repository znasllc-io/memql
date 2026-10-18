package planner

import (
	"context"
	"strings"
	"testing"

	purecompose "github.com/znasllc-io/memql/component/compose"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/work"
)

func TestCompileGoalForRun_DeterministicRoutesPersistRunnableDraft(t *testing.T) {
	for _, tc := range []struct {
		name   string
		triage map[string]any
		want   work.Route
	}{
		{"trivial", map[string]any{"complexity": "trivial", "requiresFile": false}, work.RouteTrivial},
		{"sectionable", map[string]any{"complexity": "moderate", "requiresFile": false, "sectionable": true, "sections": []map[string]any{{"label": "one", "instruction": "write the first section", "outputs": []string{"first"}}, {"label": "two", "instruction": "write the second section", "outputs": []string{"second"}}}}, work.RouteSectionable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &countingCompileEngine{triage: tc.triage}
			loop := &PlannerAgentLoop{engine: eng}
			out, err := loop.CompileGoalForRun(context.Background(), compileReq(), nil, realSandbox{})
			if err != nil {
				t.Fatal(err)
			}
			if out.Route != tc.want || out.AutomationName == "" || out.ConstructId == "" {
				t.Fatalf("compile must return a persisted runnable template: %+v", out)
			}
			if out.ModelCalls != 1 || len(eng.aiCalls) != 1 {
				t.Fatalf("deterministic route spent more than triage: %+v / %v", out, eng.aiCalls)
			}
			var created, validated bool
			for _, query := range eng.queries {
				if strings.Contains(query, "createAuthoringBundle(") {
					created = strings.Contains(query, `sourceRunId: "v1:work:run:r1"`)
				}
				if strings.Contains(query, "recordBundleValidation(") {
					validated = strings.Contains(query, `status: "validated"`)
				}
				if strings.Contains(query, "activateAuthoringBundle(") {
					t.Fatal("run draft bypassed promotion approval")
				}
			}
			if !created || !validated {
				t.Fatalf("draft never persisted and passed Gate 1: %v", eng.queries)
			}
		})
	}
}

func TestCompileGoalForRun_DeterministicDraftRequiresGate1(t *testing.T) {
	eng := &countingCompileEngine{triage: map[string]any{"complexity": "trivial", "requiresFile": false}}
	_, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), compileReq(), nil, nil)
	if err == nil {
		t.Fatal("trivial compilation must not dispatch a draft without Gate 1")
	}
}

func TestReasoningDraftUsesNativeMaterializerOnlyAtFinalDelivery(t *testing.T) {
	for _, sectionable := range []bool{false, true} {
		for _, requiresFile := range []bool{false, true} {
			decision := parseSectionableDecision(map[string]any{
				"sectionable": sectionable, "requiresFile": requiresFile,
				"fileName": "report", "fileFormat": "markdown",
				"sections": []map[string]any{{"label": "one"}, {"label": "two"}},
			})
			bundle, err := synthesizeWorkReasoningBundle(compileReq(), "agent", decision)
			if err != nil {
				t.Fatal(err)
			}
			source := bundle.Constructs[0].Source
			want := 0
			if requiresFile {
				want = 1
			}
			if got := strings.Count(source, "builtin composeMaterialize("); got != want {
				t.Fatalf("sectionable=%v requiresFile=%v: native materialization count=%d, want %d:\n%s", sectionable, requiresFile, got, want, source)
			}
			if sectionable && requiresFile && strings.Index(source, "builtin composeMaterialize(") < strings.Index(source, "assemble := ") {
				t.Fatal("independent section materialized a file before assembly")
			}
			if requiresFile && !sectionable && strings.Contains(source, "runAgentTurn") {
				t.Fatal("known file goal still needs an agent tool turn")
			}
		}
	}
}

func TestReasoningFileDraftRefusesMissingOrUnsupportedOutput(t *testing.T) {
	for _, fields := range []map[string]any{
		{"fileFormat": "markdown"}, {"fileName": "report"}, {"fileName": " ", "fileFormat": "markdown"},
		{"fileName": "report", "fileFormat": "audio"}, {"fileName": "report", "fileFormat": "unknown"},
	} {
		fields["requiresFile"] = true
		if _, err := synthesizeWorkReasoningBundle(compileReq(), "", parseSectionableDecision(fields)); err == nil {
			t.Fatalf("file goal accepted invalid semantic output: %+v", fields)
		}
	}
}

// Triage chooses the file's stem, and a model reaches for "reports/Q3" as readily
// as for "Q3". The Materializer refuses a name that carries a folder path --
// right for a person who typed one, wrong for a stem a model chose, where the
// refusal would come only after the drafting steps had already run -- so the
// stem becomes a plain name before the call is written.
func TestReasoningFileDraftHyphenatesPathSeparatorsInTheChosenFileName(t *testing.T) {
	for _, tc := range []struct{ chosen, want string }{
		{"reports/Q3 summary", "reports-Q3 summary"},
		{`reports\Q3 summary`, "reports-Q3 summary"},
		{"  2026/10/launch\\brief  ", "2026-10-launch-brief"},
		{"report", "report"},
		{"v1.2 notes", "v1.2 notes"},
	} {
		for _, sectionable := range []bool{false, true} {
			decision := parseSectionableDecision(map[string]any{
				"sectionable": sectionable, "requiresFile": true, "fileName": tc.chosen, "fileFormat": "markdown",
				"sections": []map[string]any{{"label": "one"}, {"label": "two"}},
			})
			bundle, err := synthesizeWorkReasoningBundle(compileReq(), "agent", decision)
			if err != nil {
				t.Fatalf("%q (sectionable=%v): %v", tc.chosen, sectionable, err)
			}
			source := bundle.Constructs[0].Source
			if want := "name: " + langparser.QuoteString(tc.want) + ", format:"; !strings.Contains(source, want) {
				t.Fatalf("%q (sectionable=%v) should be delivered as %q, but the call reads:\n%s", tc.chosen, sectionable, tc.want, source)
			}
		}
	}
}

func TestCompileFileDraftNeedsNoAgentLookup(t *testing.T) {
	eng := &countingCompileEngine{triage: map[string]any{"complexity": "trivial", "requiresFile": true, "fileName": "report", "fileFormat": "markdown"}}
	out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), compileReq(), nil, realSandbox{})
	if err != nil || out.ModelCalls != 1 {
		t.Fatalf("native file draft: %+v, %v", out, err)
	}
	for _, q := range eng.queries {
		if strings.Contains(q, "assistantAgentForUser") || strings.Contains(q, "agentById") {
			t.Fatalf("known file capability still requires an agent: %s", q)
		}
	}
}

func TestReasoningDraftRefusesMissingFileDecision(t *testing.T) {
	for _, raw := range []map[string]any{{"complexity": "trivial"}, {"requiresFile": nil}, {"requiresFile": "false"}} {
		if _, err := synthesizeWorkReasoningBundle(compileReq(), "agent", parseSectionableDecision(raw)); err == nil {
			t.Fatalf("malformed triage could silently omit file receipt: %+v", raw)
		}
	}
}

func TestReasoningAgentRefusesUnavailableOrForeignSeed(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		value       any
	}{
		{"missing", "", nil},
		{"other owner", "ownerUserId", "v1:identity:user:other"},
		{"inactive", "active", false},
		{"deleted", "deleted", true},
		{"different identity", "id", "v1:agents:agent:other"},
		{"different role", "roleSlug", "system-trainer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := map[string]any{"id": "v1:agents:agent:plannerAgent-owner", "ownerUserId": "v1:identity:user:owner", "active": true, "deleted": false, "roleSlug": "system-planner"}
			row[tc.field] = tc.value
			engine := &fakeEngine{execResponder: func(query string) (any, error) {
				if strings.Contains(query, "assistantAgentForUser") || tc.field == "" {
					return rowsEnvelope(), nil
				}
				return rowsEnvelope(row), nil
			}}
			if agent, err := (&PlannerAgentLoop{engine: engine}).reasoningAgent(context.Background(), "v1:identity:user:owner"); err == nil || agent != "" {
				t.Fatalf("unavailable/foreign seed was selected: %q, %v", agent, err)
			}
		})
	}
}

// Keep the compiler hook real; the stub only supplies the triage's model response.
var _ authoringSandbox = realSandbox{}

func TestNavigationDraftUsesOneTriageAndNoGeneralAgent(t *testing.T) {
	eng := &countingCompileEngine{triage: map[string]any{"complexity": "trivial", "requiresFile": false, "navigation": map[string]any{"app": "deployables", "section": "deployables", "record": "VS Code and Cursor"}}}
	out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), compileReq(), nil, realSandbox{})
	if err != nil || out.ModelCalls != 1 {
		t.Fatalf("navigation draft: %+v %v", out, err)
	}
	for _, q := range eng.queries {
		if strings.Contains(q, "assistantAgentForUser") || strings.Contains(q, "agentById") {
			t.Fatalf("navigation resolved a general agent: %s", q)
		}
	}
	decision := parseSectionableDecision(eng.triage)
	bundle, err := synthesizeWorkReasoningBundle(compileReq(), "", decision)
	if err != nil {
		t.Fatal(err)
	}
	source := bundle.Constructs[0].Source
	if !strings.Contains(source, `builtin workNavigate(app: "deployables", section: "deployables", record: "VS Code and Cursor")`) || strings.Contains(source, "runAgentTurn") {
		t.Fatalf("wrong navigation step: %s", source)
	}
	yes := true
	decision.RequiresFile = &yes
	decision.FileName = "report"
	decision.FileFormat = "markdown"
	bundle, err = synthesizeWorkReasoningBundle(compileReq(), "agent", decision)
	if err != nil || strings.Contains(bundle.Constructs[0].Source, "workNavigate") {
		t.Fatalf("file goal reduced to navigation: %+v %v", bundle, err)
	}
}

func TestCapabilityArtifactsUseToolsAndRequireASavedFile(t *testing.T) {
	for _, format := range []string{"png", "wav", "mp4", "xlsx", "glb"} {
		for _, delivery := range []string{"", "compose", "capability"} {
			for _, fanout := range []bool{false, true} {
				decision := parseSectionableDecision(map[string]any{"intent": "task", "requiresFile": true, "fileName": "requested", "fileFormat": format, "fileDelivery": delivery, "sectionable": fanout, "sections": []map[string]any{{"label": "one"}, {"label": "two"}}})
				if !decision.needsReasoningAgent("artifact") {
					t.Fatal("capability artifact omitted its executing agent")
				}
				bundle, err := synthesizeWorkReasoningBundle(compileReq(), "agent", decision)
				if err != nil {
					t.Fatal(err)
				}
				source := bundle.Constructs[0].Source
				if strings.Contains(source, "composeMaterialize") || !strings.Contains(source, "runAgentTurn") || strings.Count(source, "requireFile: true") != 1 || !strings.Contains(source, "requiredFileExtension: "+langparser.QuoteString(format)) {
					t.Fatalf("wrong delivery for %s (fanout %v): %s", format, fanout, source)
				}
				if _, err := langparser.ParseFile(source); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func TestCapabilityArtifactsRefuseInvalidExtensionBeforeExecution(t *testing.T) {
	for _, delivery := range []string{"", "compose", "capability", "unknown"} {
		for _, format := range []string{"", "../png", "image/png", "png\\file", "extensiontoolongforsafecontract"} {
			decision := parseSectionableDecision(map[string]any{"requiresFile": true, "fileName": "requested", "fileFormat": format, "fileDelivery": delivery})
			if _, err := synthesizeWorkReasoningBundle(compileReq(), "agent", decision); err == nil {
				t.Fatalf("accepted invalid extension %q", format)
			}
		}
	}
}

func TestDocumentFileExtensionKeepsItsNativeComposer(t *testing.T) {
	decision := parseSectionableDecision(map[string]any{"requiresFile": true, "fileName": "birds.md", "fileFormat": "md", "fileDelivery": "compose"})
	bundle, err := synthesizeWorkReasoningBundle(compileReq(), "", decision)
	if err != nil {
		t.Fatal(err)
	}
	source := bundle.Constructs[0].Source
	if !strings.Contains(source, `format: "markdown"`) || !strings.Contains(source, "composeMaterialize") || strings.Contains(source, "runAgentTurn") {
		t.Fatalf("Markdown extension lost its document route: %s", source)
	}
}

func TestEveryComposerFormatKeepsItsNativeContract(t *testing.T) {
	yes := true
	for _, format := range purecompose.Formats() {
		for _, name := range []string{string(format), "." + format.Extension()} {
			for _, hint := range []string{"", "compose"} {
				decision := sectionableDecision{RequiresFile: &yes, FileName: "output", FileFormat: name, FileDelivery: hint}
				if decision.needsReasoningAgent("artifact") {
					t.Fatalf("composer encoding %q unexpectedly requires discovery", name)
				}
				bundle, err := synthesizeWorkReasoningBundle(compileReq(), "", decision)
				if err != nil {
					t.Fatal(err)
				}
				source := bundle.Constructs[0].Source
				if !strings.Contains(source, "composeMaterialize") || !strings.Contains(source, "format: "+langparser.QuoteString(string(format))) {
					t.Fatalf("lost composer contract for %q: %s", name, source)
				}
			}
		}
	}
}

func TestFileContractPreservesExplicitCapabilitiesAndRejectsUnknownMechanism(t *testing.T) {
	yes := true
	decision := sectionableDecision{RequiresFile: &yes, FileName: "output", FileFormat: "pdf", FileDelivery: "capability"}
	if !decision.needsReasoningAgent("artifact") {
		t.Fatal("lost explicit capability delivery")
	}
	decision.FileDelivery = "invented"
	if _, err := synthesizeWorkReasoningBundle(compileReq(), "agent", decision); err == nil {
		t.Fatal("unknown mechanism accepted")
	}
}
