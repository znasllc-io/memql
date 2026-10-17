package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

func testFileEvidenceDraftAcrossReplica(t *testing.T, db *bun.DB, compiler, receiver *memql.MemQLEngine, prompts *draftPromptEngine, composer *draftComposerProbe, uploader *draftUploaderProbe, ctx context.Context) {
	for _, tc := range []struct {
		name, evidence                      string
		receipts, usesReceipts, createsFile bool
	}{
		{"missing evidence replies without a file", "missing", true, false, false},
		{"recorded evidence reaches the writer", "supported", true, true, true},
		{"truthful no-source draft cannot invent receipts", "supported", false, true, false},
		{"self-contained writing needs no receipts", "not_required", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conversationID := "v1:os:askConversation:" + id.NewShortId()
			prior := "v1:work:run:" + id.NewShortId()
			insert := func(concept, record string, payload map[string]any) {
				raw, _ := json.Marshal(payload)
				_, err := db.DB.ExecContext(ctx, `INSERT INTO "MemoryNodes"(id,concept,"createdAt","createdBy",schema,payload) VALUES($1,$2,$3,'source-test','{}',$4)`, record, concept, time.Now().UTC(), string(raw))
				if err != nil {
					t.Fatal(err)
				}
			}
			owner := "v1:identity:user:draft-owner"
			insert("v1:work:run", prior, map[string]any{"ownerUserId": owner, "status": "succeeded"})
			if tc.receipts {
				insert("v1:work:step", "v1:work:step:"+id.NewShortId(), map[string]any{"ownerUserId": owner, "runId": prior, "key": "viewer", "status": "done", "call": map[string]any{"name": "workViewerContext"}, "result": map[string]any{"value": map[string]any{"person": map[string]any{"primaryRole": "Founder"}, "organizationMemberships": []any{map[string]any{"name": "ZNAS LLC"}}, "source": "recorded profile"}}})
			}
			turns := []any{map[string]any{"id": "previous", "runId": prior, "prompt": "How can you help?", "answer": "Founder of ZNAS LLC", "state": "done", "startedAt": time.Now().UTC()}, map[string]any{"id": "now", "prompt": "Explain your source in an MD file", "state": "running", "startedAt": time.Now().UTC()}}
			insert("v1:os:askConversation", conversationID, map[string]any{"ownerUserId": owner, "title": "Source check", "transcript": map[string]any{"turns": turns}})
			req := CompileRequest{GoalId: "v1:work:goal:" + id.NewShortId(), RunId: "v1:work:run:" + id.NewShortId(), OwnerUserId: owner, Statement: "Explain how you knew my founder role in an MD file", Input: map[string]any{"conversation": map[string]any{"id": memql.BareShortId(conversationID), "turnId": "now", "messages": []any{map[string]any{"role": "assistant", "content": "Founder of ZNAS LLC"}}}}}
			var err error
			req.Spine, err = workintegration.CaptureSpine("")
			if err != nil {
				t.Fatal(err)
			}
			bridge := &draftDBCompiler{engine: compiler, triage: map[string]any{"intent": "task", "workload": "research", "complexity": "trivial", "requiresFile": true, "requiresResearch": false, "fileName": "founder-source", "fileFormat": "markdown", "acknowledgement": "I will check the source before creating the file."}}
			out, err := (&PlannerAgentLoop{engine: bridge, logger: testLogger()}).CompileGoalForRun(ctx, req, nil, bridge)
			if err != nil {
				t.Fatal(err)
			}
			result, err := receiver.Execute(ctx, "query authoringConstructById("+encodeArgs(map[string]any{"constructId": out.ConstructId})+")")
			if err != nil {
				t.Fatal(err)
			}
			source := memql.MaterializeRows(result)[0]["source"].(string)
			if !strings.Contains(source, "workPrepareFileEvidence") {
				t.Fatal("file has no evidence gate")
			}
			auto, err := automations.NewLoader(automations.LoaderOptions{Logger: testLogger()}).CompileSource(source, "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = receiver.Execute(auth.ContextWithInternalOrigin(ctx), "mutation createWorkRun("+encodeArgs(map[string]any{"spine": req.Spine.Map(), "runId": req.RunId, "goalId": req.GoalId, "automationName": out.AutomationName, "templateConstructId": out.ConstructId, "templateFingerprint": out.TemplateFingerprint, "templateVersion": out.TemplateVersion, "input": req.Input, "variables": req.Input, "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})+")")
			if err != nil {
				t.Fatal(err)
			}
			composer.requests, uploader.data = nil, nil
			calls := 0
			basis := "The recorded workViewerContext read Founder and ZNAS LLC membership separately; their relationship was an unsupported inference."
			reply := "I could not identify a source for that claim. I have not created a file."
			if !tc.receipts && tc.usesReceipts {
				// This is the approval returned by the production model after
				// unsuccessful research. Its claimed citations do not exist.
				basis = "The recorded sources are empty. The draft accurately states no source exists and lists searches yielding no results."
				reply = "I could not verify a recorded source for that explanation"
			}
			prompts.invoke = func(callCtx context.Context, name string, data map[string]any) (any, error) {
				calls++
				if name != "workFileEvidence" || (tc.receipts && (!strings.Contains(fmt.Sprint(data["sources"]), "recorded profile") || !strings.Contains(fmt.Sprint(data["sources"]), memql.BareShortId(prior)))) || !strings.Contains(fmt.Sprint(data["conversation"]), "Founder of ZNAS LLC") {
					return nil, fmt.Errorf("source context lost: %s %+v", name, data)
				}
				access, _ := auth.AccessFromContext(callCtx)
				run, _ := common.RunFromContext(callCtx)
				if access == nil || access.UserId != owner || run.RunId == "" {
					return nil, fmt.Errorf("source gate lost owner/run")
				}
				response := map[string]any{"ready": tc.evidence != "missing", "evidence": tc.evidence, "reply": reply, "basis": "", "usesRecordedSources": tc.usesReceipts}
				if tc.evidence != "missing" {
					response["reply"] = ""
					response["basis"] = basis
				}
				raw, _ := json.Marshal(response)
				return string(raw), nil
			}
			defer func() { prompts.invoke = nil }()
			runCtx := common.ContextWithRun(ctx, common.RunContext{RunId: req.RunId, GoalId: req.GoalId, OwnerUserId: owner, Mode: common.RunModeLive})
			childSource, ok := memql.DSLConstructSource(testLogger(), "automation", "workPrepareFileEvidence")
			if !ok {
				t.Fatal("missing evidence recipe")
			}
			child, err := automations.NewLoader(automations.LoaderOptions{Logger: testLogger()}).CompileSource(childSource, "")
			if err != nil {
				t.Fatal(err)
			}
			trigger := &memberTrigger{engine: receiver, members: map[string]*automations.Automation{"workPrepareFileEvidence": child}, args: map[string]map[string]any{}}
			executor := automations.NewExecutor(automations.ExecutorOptions{Engine: receiver, Logger: testLogger(), StepRegistry: steps.NewRegistry(), AutomationTrigger: trigger})
			execution, err := executor.ExecuteAdopted(runCtx, auto, automations.RunAdoption{RunId: req.RunId, Variables: req.Input})
			executor.Close()
			if err != nil || execution.Status != "completed" || calls != 1 {
				t.Fatalf("source gate failed: %+v %v calls=%d", execution, err, calls)
			}
			if tc.createsFile {
				if len(composer.requests) != 1 || !strings.Contains(composer.requests[0].Draft, basis) || (tc.usesReceipts && len(composer.requests[0].Sources) == 0) || (!tc.usesReceipts && len(composer.requests[0].Sources) != 0) || len(uploader.data) == 0 {
					t.Fatalf("evidence did not reach file: %+v", composer.requests)
				}
			} else {
				if len(composer.requests) != 0 || len(uploader.data) != 0 {
					t.Fatal("missing evidence created a file")
				}
				journal, err := automations.LoadRunJournal(ctx, compiler, req.RunId)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(journal)
				if !strings.Contains(string(raw), reply) {
					t.Fatal("missing-source answer not durably returned")
				}
			}
		})
	}
}
