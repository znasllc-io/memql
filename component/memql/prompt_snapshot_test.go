package memql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"text/template"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

func capturedPromptFixture(t *testing.T) (*MemQLEngine, map[string]any) {
	t.Helper()
	const schema = `{"type":"object","additionalProperties":false,"required":["content"],"properties":{"content":{"type":"string"}}}`
	p := promptWithSchema(t, "reviewExact", `Original {{template "rules" .}}`, schema)
	p.Level, p.DefaultProvider = "strong", "chat54Mini"
	if err := json.Unmarshal([]byte(schema), &p.inputSchemaSource); err != nil {
		t.Fatal(err)
	}
	var err error
	p.tmpl, err = p.tmpl.Parse(`{{define "rules"}}rules {{if .content}}{{template "body" .}}{{else}}empty{{end}}{{end}}{{define "body"}}{{.content}}{{end}}{{define "unused"}}unrelated{{end}}`)
	if err != nil {
		t.Fatal(err)
	}
	e := &MemQLEngine{prompts: newPromptRegistry()}
	e.prompts.set(p)
	snapshot, err := e.CapturePrompt(p.Name)
	if err != nil {
		t.Fatal(err)
	}
	return e, snapshot
}

// The receiving engine has no original registry entry. Both the original
// engine and receiving engine now install different wording and schemas; the
// model must still receive the captured closure and the recorded input rules.
func TestCapturedPromptCrossReplicaUsesExactClosureAndJournal(t *testing.T) {
	first, snapshot := capturedPromptFixture(t)
	if len(snapshot["templates"].(map[string]any)) != 3 {
		t.Fatal("snapshot did not isolate the reachable closure")
	}
	first.prompts.set(&PromptTemplate{Name: "reviewExact", Level: "fast", tmpl: template.Must(template.New("reviewExact").Parse("changed"))})
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err = json.Unmarshal(encoded, &saved); err != nil {
		t.Fatal(err)
	}
	second, model, requests := overrideSeamEngine(t, aiCacheConfig{})
	second.prompts = newPromptRegistry() // no fallback could find the original
	journal := newCountingJournal()
	second.SetModelCallJournal(journal)
	run := common.RunContext{RunId: "review-original", GoalId: "review-goal", StepKey: "independent-review", OwnerUserId: "alice", Mode: common.RunModeLive}
	ctx := common.ContextWithRun(userCtx("alice"), run)
	out, err := second.InvokeCapturedPromptStructured(ctx, saved, map[string]any{"content": "exact patch"}, "verdict", `{"type":"object"}`, true)
	if err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 || model.messages[0].Content != "Original rules exact patch" || len(*requests) != 1 || (*requests)[0].Level != airoute.LevelStrong || (*requests)[0].ExplicitProvider != "chat54Mini" || (*requests)[0].PromptName != "reviewExact" {
		t.Fatalf("captured call changed: calls=%d messages=%+v route=%+v", model.calls, model.messages, *requests)
	}
	if len(journal.rows[run.RunId]) != 1 {
		t.Fatal("captured review bypassed the run journal")
	}
	run.SourceRunId, run.SourceGoalId, run.RunId, run.Mode = run.RunId, run.GoalId, "review-replay", common.RunModeReplay
	replayed, err := second.InvokeCapturedPromptStructured(common.ContextWithRun(userCtx("alice"), run), saved, map[string]any{"content": "exact patch"}, "verdict", `{"type":"object"}`, true)
	if err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 || replayed.Text != out.Text || replayed.Served != "journal" {
		t.Fatalf("review replay called a model or lost receipt: %+v calls=%d", replayed, model.calls)
	}
}

func TestCapturedPromptRefusesMissingClosureAndInvalidInputBeforeModel(t *testing.T) {
	_, snapshot := capturedPromptFixture(t)
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
		data   map[string]any
	}{
		{"missing partial", func(s map[string]any) { delete(s["templates"].(map[string]any), "body") }, map[string]any{"content": "patch"}},
		{"hidden definition", func(s map[string]any) { s["templates"].(map[string]any)["body"] = `{{define "injected"}}bad{{end}}` }, map[string]any{"content": "patch"}},
		{"required input", func(map[string]any) {}, map[string]any{}},
		{"undeclared input", func(map[string]any) {}, map[string]any{"content": "patch", "approved": true}},
		{"unknown snapshot field", func(s map[string]any) { s["installedFallback"] = true }, map[string]any{"content": "patch"}},
		{"external schema", func(s map[string]any) {
			s["inputSchema"] = map[string]any{"$ref": "https://example.invalid/contract.json"}
		}, map[string]any{"content": "patch"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, _ := json.Marshal(snapshot)
			var broken map[string]any
			if err := json.Unmarshal(b, &broken); err != nil {
				t.Fatal(err)
			}
			test.mutate(broken)
			e, model, _ := overrideSeamEngine(t, aiCacheConfig{})
			_, err := e.InvokeCapturedPromptStructured(context.Background(), broken, test.data, "verdict", `{"type":"object"}`, true)
			if err == nil || model.calls != 0 {
				t.Fatalf("invalid snapshot/input reached model: err=%v calls=%d", err, model.calls)
			}
		})
	}
	first, _ := capturedPromptFixture(t)
	p, _ := first.prompts.Get("reviewExact")
	p.inputSchemaSource = nil
	if _, err := first.CapturePrompt("reviewExact"); err == nil {
		t.Fatal("capture silently dropped compiled input validation")
	}
}

func TestCapturedPromptLoaderRetainsItsInputContract(t *testing.T) {
	registry := newPromptRegistry()
	partials, err := loadPartials()
	if err != nil {
		t.Fatal(err)
	}
	report := newLoadReport()
	if _, err = LoadUnifiedPrompts(discardLogger(), registry, partials, report); err != nil {
		t.Fatal(err)
	}
	e := &MemQLEngine{prompts: registry}
	count := 0
	for _, p := range registry.List() {
		snapshot, err := e.CapturePrompt(p.Name)
		if err != nil {
			t.Fatalf("capture loaded %s: %v", p.Name, err)
		}
		restored, err := restoreCapturedPrompt(snapshot)
		if err != nil {
			t.Fatalf("restore loaded %s: %v", p.Name, err)
		}
		if (restored.inputSchema == nil) != (p.inputSchema == nil) {
			t.Fatalf("lost schema for %s", p.Name)
		}
		count++
	}
	if count == 0 {
		t.Fatal("no production prompts were captured")
	}
	_, err = e.CapturePrompt("absentPrompt")
	if err == nil || !strings.Contains(err.Error(), "absentPrompt") {
		t.Fatalf("unknown prompt did not refuse: %v", err)
	}
}
