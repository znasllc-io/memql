package planner

import (
	"context"
	"encoding/json"
	"fmt"
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

// Use the compiled workflow and the real durable budget reader on a receiving
// engine. The fourth call must reach composition under the ordinary allowance,
// but a person's explicit three-call ceiling must still stop it.
func testFileCallBudgetAcrossReplica(t *testing.T, db *bun.DB, compiler, receiver *memql.MemQLEngine, prompts *draftPromptEngine, composer *draftComposerProbe, uploader *draftUploaderProbe, ctx context.Context) {
	for _, limit := range []int{12, 3} {
		t.Run(fmt.Sprintf("declared calls %d", limit), func(t *testing.T) {
			owner := "v1:identity:user:draft-owner"
			conversationID := "v1:os:askConversation:" + id.NewShortId()
			req := CompileRequest{GoalId: "v1:work:goal:" + id.NewShortId(), RunId: "v1:work:run:" + id.NewShortId(), OwnerUserId: owner, Statement: "Create a Markdown file with the title Coffee and the sentence Grind fresh beans before brewing.", MaxModelCalls: limit, Input: map[string]any{"conversation": map[string]any{"id": memql.BareShortId(conversationID), "turnId": "now", "messages": []any{}}}}
			insert := func(concept, record string, payload map[string]any) {
				raw, _ := json.Marshal(payload)
				if _, err := db.DB.ExecContext(ctx, `INSERT INTO "MemoryNodes"(id,concept,"createdAt","createdBy",schema,payload) VALUES($1,$2,$3,'budget-test','{}',$4)`, record, concept, time.Now().UTC(), string(raw)); err != nil {
					t.Fatal(err)
				}
			}
			insert("v1:work:goal", req.GoalId, map[string]any{"ownerUserId": owner, "ceilings": map[string]any{"maxModelCalls": limit, "wallClockMs": 600000}})
			insert("v1:os:askConversation", conversationID, map[string]any{"ownerUserId": owner, "title": "File budget test", "transcript": map[string]any{"turns": []any{map[string]any{"id": "now", "prompt": req.Statement, "state": "running"}}}})
			var err error
			req.Spine, err = workintegration.CaptureSpine("")
			if err != nil {
				t.Fatal(err)
			}
			// Missing acknowledgment exercises the extra call seen in production.
			bridge := &draftDBCompiler{engine: compiler, triage: map[string]any{"intent": "task", "workload": "quick", "complexity": "trivial", "requiresFile": true, "requiresResearch": false, "fileName": "coffee", "fileFormat": "markdown"}}
			out, err := (&PlannerAgentLoop{engine: bridge, logger: testLogger()}).CompileGoalForRun(ctx, req, nil, bridge)
			if err != nil {
				t.Fatal(err)
			}
			if out.ModelCalls != 2 || bridge.calls != 2 || out.Acknowledgement == "" {
				t.Fatalf("expected classification and acknowledgment: %+v calls=%d", out, bridge.calls)
			}
			result, err := receiver.Execute(ctx, "query authoringConstructById("+encodeArgs(map[string]any{"constructId": out.ConstructId})+")")
			if err != nil {
				t.Fatal(err)
			}
			source := memql.MaterializeRows(result)[0]["source"].(string)
			auto, err := automations.NewLoader(automations.LoaderOptions{Logger: testLogger()}).CompileSource(source, "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = compiler.Execute(auth.ContextWithInternalOrigin(ctx), "mutation createWorkRun("+encodeArgs(map[string]any{"spine": req.Spine.Map(), "runId": req.RunId, "goalId": req.GoalId, "automationName": out.AutomationName, "templateConstructId": out.ConstructId, "templateFingerprint": out.TemplateFingerprint, "templateVersion": out.TemplateVersion, "input": req.Input, "variables": req.Input, "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})+")")
			if err != nil {
				t.Fatal(err)
			}
			// Intake creates the row before the compiler records its classification.
			_, err = compiler.Execute(auth.ContextWithInternalOrigin(ctx), "mutation updateWorkRun("+encodeArgs(map[string]any{"runId": req.RunId, "classification": map[string]any{"workload": out.Workload}})+")")
			if err != nil {
				t.Fatal(err)
			}
			rc := common.RunContext{RunId: req.RunId, GoalId: req.GoalId, OwnerUserId: owner, Mode: common.RunModeLive}
			runCtx := common.ContextWithRun(ctx, rc)
			budget := workintegration.NewRunCeilings(receiver, testLogger())
			used := 0
			charge := func(context.Context) error {
				if breach := budget.Admit(runCtx, rc, 0); breach != nil {
					return fmt.Errorf("model budget: %s %s", breach.Ceiling, breach.Limit)
				}
				budget.Charge(runCtx, rc, memql.ModelSpend{Served: memql.ServedLocal})
				used++
				return nil
			}
			for range out.ModelCalls {
				if err = charge(runCtx); err != nil {
					t.Fatal(err)
				}
			}
			prompts.invoke = func(callCtx context.Context, name string, data map[string]any) (any, error) {
				if name != "workFileEvidence" {
					return nil, fmt.Errorf("unexpected prompt %s", name)
				}
				if err := charge(callCtx); err != nil {
					return nil, err
				}
				return `{"ready":true,"evidence":"not_required","reply":"","basis":"Use the title and sentence supplied by the user.","usesRecordedSources":false}`, nil
			}
			composer.beforeCompose = charge
			composer.requests, uploader.data = nil, nil
			defer func() { prompts.invoke = nil; composer.beforeCompose = nil }()
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
			if limit == 12 {
				if err != nil || execution.Status != "completed" || used != 4 || len(composer.requests) != 1 || len(uploader.data) == 0 {
					t.Fatalf("ordinary file exhausted estimated budget: execution=%+v err=%v used=%d files=%d", execution, err, used, len(composer.requests))
				}
			} else if used != 3 || len(composer.requests) != 0 || len(uploader.data) != 0 || (err == nil && execution.Status == "completed") {
				t.Fatalf("explicit ceiling relaxed: execution=%+v err=%v used=%d files=%d", execution, err, used, len(composer.requests))
			}
			if out.Workload != "lookup" {
				t.Fatalf("file retained direct-reply estimate: %s", out.Workload)
			}
		})
	}
}
