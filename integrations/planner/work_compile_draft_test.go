package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
)

func TestCompileGoalForRun_DeterministicRoutesPersistRunnableDraft(t *testing.T) {
	for _, tc := range []struct {
		name   string
		triage map[string]any
		want   work.Route
	}{
		{"trivial", map[string]any{"complexity": "trivial", "requiresFile": false}, work.RouteTrivial},
		{"sectionable", map[string]any{"complexity": "moderate", "requiresFile": false, "sectionable": true, "sections": []map[string]any{{"label": "one", "instruction": "write the first section"}, {"label": "two", "instruction": "write the second section"}}}, work.RouteSectionable},
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
