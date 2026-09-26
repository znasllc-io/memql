package planner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	purecompose "github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/agents"
	composeintegration "github.com/znasllc-io/memql/integrations/compose"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

type draftDBCompiler struct {
	engine *memql.MemQLEngine
	triage any
	calls  int
}

func (e *draftDBCompiler) Execute(ctx context.Context, q string) (any, error) {
	return e.engine.Execute(ctx, q)
}
func (e *draftDBCompiler) InvokeAI(_ context.Context, name string, _ map[string]any) (any, error) {
	e.calls++
	if name != "goalComplexityTriage" {
		return nil, fmt.Errorf("unexpected expensive compile call %s", name)
	}
	return e.triage, nil
}
func (*draftDBCompiler) InvokeAIChatWithFilteredTools(context.Context, string, map[string]any, []string) (string, error) {
	return "", fmt.Errorf("unexpected authoring call")
}
func (e *draftDBCompiler) CompileBundle(cs []memql.SandboxConstruct) memql.SandboxReport {
	return memql.SandboxCompileBundleWithEngine(cs, e.engine)
}

type draftTurnProbe struct {
	agentId string
	mu      sync.Mutex
	prompts []string
}

type draftComposerProbe struct {
	requests []composeintegration.ComposeRequest
	runs     []common.RunContext
}

func (p *draftComposerProbe) Compose(ctx context.Context, req composeintegration.ComposeRequest) (composeintegration.ComposeReply, error) {
	run, ok := common.RunFromContext(ctx)
	ac, _ := auth.AccessFromContext(ctx)
	if !ok || run.RunId == "" || run.GoalId == "" || run.StepKey == "" || ac == nil || ac.UserId != "v1:identity:user:draft-owner" {
		return composeintegration.ComposeReply{}, fmt.Errorf("native composer lost run/step/owner context")
	}
	p.requests = append(p.requests, req)
	p.runs = append(p.runs, run)
	return composeintegration.ComposeReply{Draft: purecompose.Draft{Title: "Native report", Body: "NATIVE_COMPOSED_CONTENT", Header: []string{"region"}, Rows: []map[string]any{{"region": "EMEA"}}}}, nil
}

type draftUploaderProbe struct {
	data []byte
	fail bool
}

func (p *draftUploaderProbe) Upload(_ context.Context, _, objectName string, data []byte, _ string) (string, error) {
	if p.fail {
		return "", fmt.Errorf("storage unavailable")
	}
	p.data = append([]byte(nil), data...)
	return "https://test.invalid/" + objectName, nil
}

func (p *draftTurnProbe) RunTurn(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg) (string, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac.UserId != "v1:identity:user:draft-owner" {
		return "", fmt.Errorf("lost owner actor: %+v", ac)
	}
	if p.agentId != "" && msg.AgentId != p.agentId {
		return "", fmt.Errorf("wrong reasoning agent %q, want %q", msg.AgentId, p.agentId)
	}
	prompt := msg.History[0].Content
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prompts = append(p.prompts, prompt)
	if strings.Contains(prompt, "first section") {
		return "FIRST_SECTION_CONTENT", nil
	}
	if strings.Contains(prompt, "second section") {
		return "SECOND_SECTION_CONTENT", nil
	}
	return "DELIVERABLE_SAVED", nil
}

func TestCompileDraftDB_SeparateReplicaReadsAndRunsValidatedDraft(t *testing.T) {
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "work draft compile", dbtest.DSN(), err)
	}
	for _, q := range []string{`CREATE TEMP TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`, `CREATE TEMP TABLE node_vectors (LIKE public.node_vectors INCLUDING ALL)`} {
		if _, err := db.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	newEngine := func() *memql.MemQLEngine {
		e, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		e.Logger = testLogger()
		if err = e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		return e
	}
	plannerEngine, receiver := newEngine(), newEngine()
	for _, name := range []string{"invokeAgent", "produceArtifact"} {
		source, ok := memql.DSLConstructSource(testLogger(), "automation", name)
		if !ok {
			t.Fatalf("missing direct agent template %s", name)
		}
		if report := memql.SandboxCompileBundleWithEngine([]memql.SandboxConstruct{{Kind: "automation", Name: name, Source: source}}, plannerEngine); !report.OK {
			t.Fatalf("direct template %s cannot pass engine Gate 1: %+v", name, report)
		}
	}
	ctx, err := auth.ContextWithPersistedOwner(context.Background(), "v1:identity:user:draft-owner", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Default core installation state: materialize the actual shipped per-user
	// planner/trainer seeds. There is no product-provided assistant seed.
	_, err = db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload) VALUES ('v1:identity:user:draft-owner','v1:identity:user',now(),'draft-test','{}','{"active":true,"deleted":false}')`)
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry := memql.NewSeedRegistry()
	for _, name := range []string{"plannerAgent", "trainerAgent"} {
		def, ok := plannerEngine.Seeds().Get(name)
		if !ok {
			t.Fatalf("missing core seed %s", name)
		}
		if err = seedRegistry.Upsert(def); err != nil {
			t.Fatal(err)
		}
	}
	materializer := memql.NewSeedMaterializer(plannerEngine, seedRegistry)
	if err = materializer.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer materializer.Stop(context.Background())
	probe := &draftTurnProbe{agentId: "v1:agents:agent:plannerAgent-draft-owner"}
	agentIntegration := agents.New(memql.NewAgentRegistry(), receiver)
	agentIntegration.SetAgentTurnRunner(probe)
	if err = receiver.RegisterIntegration(agentIntegration); err != nil {
		t.Fatal(err)
	}
	composerProbe, uploaderProbe := &draftComposerProbe{}, &draftUploaderProbe{}
	composeIntegration := composeintegration.New(receiver, testLogger())
	composeIntegration.SetComposer(composerProbe)
	composeIntegration.SetUploader(uploaderProbe, "files")
	if err = receiver.RegisterIntegration(composeIntegration); err != nil {
		t.Fatal(err)
	}
	t.Run("navigation adopts Ask variables on a separate receiver", func(t *testing.T) {
		bridge := &draftDBCompiler{engine: plannerEngine, triage: map[string]any{"complexity": "trivial", "requiresFile": false, "navigation": map[string]any{"app": "deployables", "section": "deployables"}}}
		req := CompileRequest{GoalId: "v1:work:goal:nav", RunId: "v1:work:run:" + id.NewShortId(), OwnerUserId: "v1:identity:user:draft-owner", Statement: "Open Deployables", Input: map[string]any{"conversation": []any{map[string]any{"role": "user", "content": "Open Deployables"}}}}
		out, err := (&PlannerAgentLoop{engine: bridge, logger: testLogger()}).CompileGoalForRun(ctx, req, nil, bridge)
		if err != nil {
			t.Fatal(err)
		}
		result, err := receiver.Execute(ctx, "query authoringConstructById("+encodeArgs(map[string]any{"constructId": out.ConstructId})+")")
		if err != nil {
			t.Fatal(err)
		}
		rows := memql.MaterializeRows(result)
		if len(rows) != 1 {
			t.Fatalf("receiver cannot read draft: %v", rows)
		}
		source, _ := rows[0]["source"].(string)
		automation, err := automations.NewLoader(automations.LoaderOptions{Logger: testLogger()}).CompileSource(source, "")
		if err != nil {
			t.Fatal(err)
		}
		_, err = receiver.Execute(auth.ContextWithInternalOrigin(ctx), "mutation createWorkRun("+encodeArgs(map[string]any{"runId": req.RunId, "goalId": req.GoalId, "automationName": out.AutomationName, "templateFingerprint": out.TemplateFingerprint, "status": "running", "input": req.Input, "variables": req.Input, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})+")")
		if err != nil {
			t.Fatal(err)
		}
		executor := automations.NewExecutor(automations.ExecutorOptions{Engine: receiver, Logger: testLogger(), StepRegistry: steps.NewRegistry()})
		defer executor.Close()
		executionCtx := common.ContextWithRun(ctx, common.RunContext{RunId: req.RunId, GoalId: req.GoalId, OwnerUserId: req.OwnerUserId})
		execution, err := executor.ExecuteAdopted(executionCtx, automation, automations.RunAdoption{RunId: req.RunId, Variables: req.Input})
		if err != nil || execution.Status != "completed" {
			t.Fatalf("navigation failed: %+v %v", execution, err)
		}
		journal, err := automations.LoadRunJournal(ctx, receiver, req.RunId)
		if err != nil {
			t.Fatal(err)
		}
		receipt, _ := json.Marshal(journal.Steps["navigate"])
		if !strings.Contains(string(receipt), "Opening Deployables") || bridge.calls != 1 {
			t.Fatalf("missing deterministic receipt or unnecessary model calls: %s / %d", receipt, bridge.calls)
		}
	})
	for _, sectionable := range []bool{false, true} {
		t.Run(fmt.Sprintf("sectionable=%t", sectionable), func(t *testing.T) {
			triage := map[string]any{"complexity": "trivial", "requiresFile": false}
			if sectionable {
				triage = map[string]any{"complexity": "moderate", "requiresFile": false, "sectionable": true, "sections": []map[string]any{{"label": "one", "instruction": "first section"}, {"label": "two", "instruction": "second section"}}}
			}
			bridge := &draftDBCompiler{engine: plannerEngine, triage: triage}
			req := CompileRequest{GoalId: "v1:work:goal:draft-goal", RunId: "v1:work:run:" + id.NewShortId(), OwnerUserId: "v1:identity:user:draft-owner", Statement: "Create and save the requested deliverable", Input: map[string]any{"filename": "draft.md", "region": "EMEA"}}
			out, err := (&PlannerAgentLoop{engine: bridge, logger: testLogger()}).CompileGoalForRun(ctx, req, nil, bridge)
			if err != nil {
				t.Fatal(err)
			}
			read := func(name string, args map[string]any) []map[string]any {
				r, err := receiver.Execute(ctx, "query "+name+"("+encodeArgs(args)+")")
				if err != nil {
					t.Fatal(err)
				}
				return memql.MaterializeRows(r)
			}
			headlines := read("authoringConstructById", map[string]any{"constructId": out.ConstructId})
			if len(headlines) != 1 {
				t.Fatalf("headline unavailable on receiver: %+v", headlines)
			}
			bundle := read("authoringBundleById", map[string]any{"bundleId": headlines[0]["bundleId"]})
			if len(bundle) != 1 || bundle[0]["status"] != "validated" || bundle[0]["sourceRunId"] != req.RunId {
				t.Fatalf("run draft not durably validated: %+v", bundle)
			}
			source, _ := headlines[0]["source"].(string)
			auto, err := automations.NewLoader(automations.LoaderOptions{Logger: testLogger()}).CompileSource(source, "")
			if err != nil {
				t.Fatal(err)
			}
			if out.TemplateFingerprint == "" || auto.DefinitionFingerprint(id.NewUntracked()) != out.TemplateFingerprint {
				t.Fatal("receiver template fingerprint differs")
			}
			_, err = receiver.Execute(auth.ContextWithInternalOrigin(ctx), "mutation createWorkRun("+encodeArgs(map[string]any{
				"runId": req.RunId, "goalId": req.GoalId, "automationName": out.AutomationName,
				"templateFingerprint": out.TemplateFingerprint, "templateConstructId": out.ConstructId,
				"templateVersion": out.TemplateVersion, "input": req.Input, "variables": req.Input,
				"status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano),
			})+")")
			if err != nil {
				t.Fatal(err)
			}
			executor := automations.NewExecutor(automations.ExecutorOptions{Engine: receiver, Logger: testLogger(), StepRegistry: steps.NewRegistry()})
			defer executor.Close()
			for _, fork := range []bool{false, true} {
				t.Run(fmt.Sprintf("fork=%t", fork), func(t *testing.T) {
					runId, filename := req.RunId, "draft.md"
					if fork {
						workIntegration := workintegration.New(receiver, testLogger())
						for _, capability := range workIntegration.Capabilities() {
							if capability.Name != "forkRun" {
								continue
							}
							nodes, err := capability.Handler(ctx, map[string]any{"runId": req.RunId, "atStepKey": auto.Steps[0].ID, "variables": map[string]any{"filename": "fork.md"}}, 0)
							if err != nil {
								t.Fatal(err)
							}
							if len(nodes) != 1 {
								t.Fatalf("fork reply: %+v", nodes)
							}
							var reply map[string]any
							if err = json.Unmarshal(nodes[0].Payload, &reply); err != nil {
								t.Fatal(err)
							}
							runId, _ = reply["runId"].(string)
						}
						if runId == req.RunId || runId == "" {
							t.Fatal("fork did not create its own run")
						}
						filename = "fork.md"
					}
					journal, err := automations.LoadRunJournal(ctx, receiver, runId)
					if err != nil {
						t.Fatal(err)
					}
					if journal.Variables["filename"] != filename || journal.Variables["region"] != "EMEA" {
						t.Fatalf("persisted fork variables: %+v", journal.Variables)
					}
					probe.mu.Lock()
					probe.prompts = nil
					probe.mu.Unlock()
					execution, err := executor.ExecuteAdopted(ctx, auto, automations.RunAdoption{RunId: runId, Variables: journal.Variables, Journal: journal})
					if err != nil {
						t.Fatal(err)
					}
					if execution.Status != "completed" {
						b, _ := json.Marshal(execution)
						t.Fatalf("draft execution failed: %s", b)
					}
					finished, err := automations.LoadRunJournal(ctx, receiver, runId)
					if err != nil {
						t.Fatal(err)
					}
					lastStep := "reason"
					if sectionable {
						lastStep = "assemble"
					}
					persisted, _ := json.Marshal(finished.Steps[lastStep])
					if !strings.Contains(string(persisted), "DELIVERABLE_SAVED") {
						t.Fatalf("journal lost agent output: %s", persisted)
					}
					probe.mu.Lock()
					defer probe.mu.Unlock()
					want := 1
					if sectionable {
						want = 3
					}
					if len(probe.prompts) != want {
						t.Fatalf("turns=%d, want %d: %v", len(probe.prompts), want, probe.prompts)
					}
					for _, prompt := range probe.prompts {
						if !strings.Contains(prompt, filename) || !strings.Contains(prompt, "EMEA") {
							t.Fatalf("turn ignored runtime variables: %s", prompt)
						}
						if fork && strings.Contains(prompt, "draft.md") {
							t.Fatalf("fork reused the input embedded at compile: %s", prompt)
						}
					}
					if sectionable {
						assembly := probe.prompts[len(probe.prompts)-1]
						if !strings.Contains(assembly, "FIRST_SECTION_CONTENT") || !strings.Contains(assembly, "SECOND_SECTION_CONTENT") {
							t.Fatalf("assembly lost branch results: %s", assembly)
						}
					}
				})
			}
			formats := purecompose.Formats()
			for index, format := range append(formats, purecompose.FormatMarkdown) {
				t.Run("native_file_"+string(format), func(t *testing.T) {
					triage["requiresFile"], triage["fileName"], triage["fileFormat"] = true, "native-report", string(format)
					req.RunId = "v1:work:run:" + id.NewShortId()
					req.Statement = "Create a \"Marvel\" report and save it in the Library.\nInclude the literal path C:\\notes\\hero.txt and the characters \\n."
					out, err := (&PlannerAgentLoop{engine: bridge, logger: testLogger()}).CompileGoalForRun(ctx, req, nil, bridge)
					if err != nil {
						t.Fatal(err)
					}
					headlines := read("authoringConstructById", map[string]any{"constructId": out.ConstructId})
					if len(headlines) != 1 {
						t.Fatalf("file draft unavailable: %+v", headlines)
					}
					auto, err := automations.NewLoader(automations.LoaderOptions{Logger: testLogger()}).CompileSource(headlines[0]["source"].(string), "")
					if err != nil {
						t.Fatal(err)
					}
					_, err = receiver.Execute(auth.ContextWithInternalOrigin(ctx), "mutation createWorkRun("+encodeArgs(map[string]any{
						"runId": req.RunId, "goalId": req.GoalId, "automationName": out.AutomationName,
						"templateFingerprint": out.TemplateFingerprint, "templateConstructId": out.ConstructId,
						"templateVersion": out.TemplateVersion, "input": req.Input, "variables": req.Input,
						"status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano),
					})+")")
					if err != nil {
						t.Fatal(err)
					}
					runCtx := common.ContextWithRun(ctx, common.RunContext{RunId: req.RunId, GoalId: req.GoalId, OwnerUserId: req.OwnerUserId, Mode: common.RunModeLive})
					probe.mu.Lock()
					probe.prompts = nil
					probe.mu.Unlock()
					composerProbe.requests, composerProbe.runs, uploaderProbe.data = nil, nil, nil
					uploaderProbe.fail = index == len(formats)
					variables := map[string]any{"filename": "runtime-input", "region": "EMEA"}
					execution, runErr := executor.ExecuteAdopted(runCtx, auto, automations.RunAdoption{RunId: req.RunId, Variables: variables})
					if uploaderProbe.fail {
						journal, err := automations.LoadRunJournal(ctx, receiver, req.RunId)
						if runErr == nil || execution.Status != "failed" || err != nil || journal.Status != "failed" {
							t.Fatalf("failed storage falsely completed file run: execution=%+v runErr=%v journal=%+v err=%v", execution, runErr, journal, err)
						}
						return
					}
					if runErr != nil || execution.Status != "completed" {
						t.Fatalf("native file execution failed: result=%+v err=%v", execution, runErr)
					}
					stepKey, wantTurns := "reason", 0
					if sectionable {
						stepKey, wantTurns = "assemble", 2
					}
					if len(probe.prompts) != wantTurns || len(composerProbe.requests) != 1 {
						t.Fatalf("native delivery required redundant agent turn: turns=%d want=%d compose=%d", len(probe.prompts), wantTurns, len(composerProbe.requests))
					}
					composed := composerProbe.requests[0]
					if !strings.Contains(composed.Statement, req.Statement) || !strings.Contains(composed.Statement, "runtime-input") || !strings.Contains(composed.Statement, "EMEA") || strings.Contains(composed.Statement, "draft.md") {
						t.Fatalf("composer lost original goal or runtime input: %+v", composed)
					}
					if !strings.Contains(composed.Statement, "\n\nGoal input (JSON):\n") {
						t.Fatalf("composer received escaped source instead of real line breaks: %q", composed.Statement)
					}
					if composed.Format != format || composerProbe.runs[0].StepKey != stepKey {
						t.Fatalf("composer lost validated format or caller step: req=%+v run=%+v", composed, composerProbe.runs[0])
					}
					if sectionable && (!strings.Contains(composed.Draft, "FIRST_SECTION_CONTENT") || !strings.Contains(composed.Draft, "SECOND_SECTION_CONTENT")) {
						t.Fatalf("native assembly lost completed section content: %+v", composed)
					}
					files := read("libraryFilesForOwner", map[string]any{"runId": req.RunId, "stepKey": stepKey, "status": "ready"})
					if len(files) != 1 || len(uploaderProbe.data) == 0 || files[0]["mimeType"] != format.MimeType() || files[0]["producedByStepKey"] != stepKey {
						t.Fatalf("native file was not durably saved by this step: files=%+v bytes=%d", files, len(uploaderProbe.data))
					}
					journal, err := automations.LoadRunJournal(ctx, receiver, req.RunId)
					if err != nil || journal.Status != "succeeded" {
						t.Fatalf("native file completion not persisted: journal=%+v err=%v", journal, err)
					}
					encoded, _ := json.Marshal(journal.Steps[stepKey])
					if !strings.Contains(string(encoded), "outputFileId") {
						t.Fatalf("journal lost native output receipt: %s", encoded)
					}
				})
			}
		})
	}
}
