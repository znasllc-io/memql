package procedure

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/agents"
	"github.com/znasllc-io/memql/integrations/planner"
)

// catalog_sections_db_test.go -- D24's loop end to end, against the real
// schema and engine (epic memql#5414, #5418): goal A plans a section live as
// an automation of its own and RUNS; its success catalogues the section
// through the real handler; goal B, a different goal asking for the same
// section, is served it by the exact tier with no planning and runs it; and
// the reuse sweep then counts the two goals that used it and labels it
// reusable, after which a goal asking for something merely CLOSE is served it
// by the near tier.

const catalogDBAgent = "v1:agents:agent:catalog-assistant"

// catalogDBBridge is the compile surface: the engine for every read and
// write, the owner's assistant for the one agent read, a canned triage for the
// one model call compile may make, and the engine's own Gate 1.
type catalogDBBridge struct {
	engine *memql.MemQLEngine
	triage any
	calls  int
}

func (b *catalogDBBridge) Execute(ctx context.Context, q string) (any, error) {
	if strings.Contains(q, "assistantAgentForUser(") {
		return []map[string]any{{"id": catalogDBAgent}}, nil
	}
	return b.engine.Execute(ctx, q)
}

func (b *catalogDBBridge) InvokeAI(_ context.Context, name string, _ map[string]any) (any, error) {
	b.calls++
	if name != "goalComplexityTriage" {
		return nil, fmt.Errorf("unexpected model call %s at compile", name)
	}
	return b.triage, nil
}

func (*catalogDBBridge) InvokeAIChatWithFilteredTools(context.Context, string, map[string]any, []string) (string, error) {
	return "", fmt.Errorf("unexpected authoring call")
}

func (b *catalogDBBridge) CompileBundle(cs []memql.SandboxConstruct) memql.SandboxReport {
	return memql.SandboxCompileBundleWithEngine(cs, b.engine)
}

// catalogDBTurns is the agent runtime: every turn answered by the section its
// prompt names, every prompt kept.
type catalogDBTurns struct {
	mu      sync.Mutex
	prompts []string
}

func (p *catalogDBTurns) RunTurn(_ context.Context, msg *memqlv1.AgentGenerateTurnMsg) (string, error) {
	prompt := msg.History[len(msg.History)-1].Content
	p.mu.Lock()
	p.prompts = append(p.prompts, prompt)
	p.mu.Unlock()
	for _, label := range []string{"summary", "count", "archive"} {
		if strings.Contains(prompt, "\nSection: "+label+"\n") {
			return strings.ToUpper(label) + "_REPLY", nil
		}
	}
	return "ASSEMBLED_REPLY", nil
}

func (p *catalogDBTurns) take() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.prompts
	p.prompts = nil
	return out
}

// catalogDBMembers resolves a sub-automation among a draft's members and runs
// it on the real executor and engine, as the executing node does
// (app/work_template_trigger.go).
type catalogDBMembers struct {
	engine  *memql.MemQLEngine
	logger  *slog.Logger
	members map[string]*automations.Automation
}

func (m *catalogDBMembers) TriggerAutomation(ctx context.Context, name string) (*automations.AutomationExecution, error) {
	return m.TriggerAutomationWithArgs(ctx, name, nil)
}

func (m *catalogDBMembers) TriggerAutomationWithArgs(ctx context.Context, name string, args map[string]any) (*automations.AutomationExecution, error) {
	auto := m.members[name]
	if auto == nil {
		return nil, fmt.Errorf("%s is not a member of the draft", name)
	}
	executor := automations.NewExecutor(automations.ExecutorOptions{Engine: m.engine, Logger: m.logger, StepRegistry: steps.NewRegistry(), AutomationTrigger: m})
	defer executor.Close()
	res, err := executor.ExecuteWithClientEvent(ctx, auto, "work.subtemplate", &events.Event{Payload: args})
	if err == nil && res.Status != "completed" {
		err = fmt.Errorf("member %s ended %s: %s", name, res.Status, res.Error)
	}
	return res, err
}

func TestDecompositionSpendsIntelligenceOnceDB(t *testing.T) {
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "section catalogue end to end", dbtest.DSN(), err)
	}
	for _, q := range []string{`CREATE TEMP TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`, `CREATE TEMP TABLE node_vectors (LIKE public.node_vectors INCLUDING ALL)`} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	e, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	e.Logger = quiet
	if err := e.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	// The two plug-ins this path reaches on a node: the agent runtime every
	// section's turn runs on, and the work integration whose workStepVersions
	// the handler reads the run's versions through -- materialized as a node
	// materializes it.
	turns := &catalogDBTurns{}
	agentIntegration := agents.New(memql.NewAgentRegistry(), e)
	agentIntegration.SetAgentTurnRunner(turns)
	if err := e.RegisterIntegration(agentIntegration); err != nil {
		t.Fatal(err)
	}
	materialized := false
	for _, p := range memql.RegisteredPlugins() {
		if p.Name != "work" {
			continue
		}
		prov, err := p.Factory(memql.PluginContext{Logger: quiet, Engine: e, BunDB: func() *bun.DB { return db }, AdmitSourceRow: memql.AdmitSourceRow})
		if err != nil {
			t.Fatalf("materialize the work plug-in: %v", err)
		}
		if err := e.RegisterIntegration(prov); err != nil {
			t.Fatalf("register the work plug-in: %v", err)
		}
		materialized = true
	}
	if !materialized {
		t.Fatal("the work plug-in is not registered in this binary")
	}

	owner := "v1:identity:user:catalog-owner-" + id.NewShortId()
	ownerCtx := auth.ContextWithUserActor(ctx, owner)
	read := func(name string, args map[string]any) []map[string]any {
		t.Helper()
		r, err := e.Execute(ownerCtx, "query "+call(name, args))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return memql.MaterializeRows(r)
	}

	// compileAndRun compiles one goal and runs its draft as the executing
	// node does: reloaded from its own bundle rows, adopted onto the run
	// compile opened, every sub-automation a member of the draft.
	compileAndRun := func(t *testing.T, statement string, triage map[string]any) (planner.CompileOutcome, *catalogDBBridge, string) {
		t.Helper()
		req := planner.CompileRequest{
			GoalId: "v1:work:goal:" + id.NewShortId(), RunId: "v1:work:run:" + id.NewShortId(), OwnerUserId: owner,
			Statement: statement, Input: map[string]any{"month": "2026-08"},
		}
		bridge := &catalogDBBridge{engine: e, triage: triage}
		out, err := planner.NewPlannerAgentLoop(bridge, quiet).CompileGoalForRun(ownerCtx, req, nil, bridge)
		if err != nil {
			t.Fatalf("compile %q: %v", statement, err)
		}
		headlines := read("authoringConstructById", map[string]any{"constructId": out.ConstructId})
		if len(headlines) != 1 {
			t.Fatalf("the draft headline is not readable: %+v", headlines)
		}
		members := read("authoringConstructsForBundle", map[string]any{"bundleId": headlines[0]["bundleId"]})
		runner := &catalogDBMembers{engine: e, logger: quiet, members: map[string]*automations.Automation{}}
		var sources []string
		for _, m := range members {
			source, _ := m["source"].(string)
			sources = append(sources, source)
			auto, err := automations.NewLoader(automations.LoaderOptions{Logger: quiet}).CompileSource(source, "")
			if err != nil {
				t.Fatalf("member %v does not load: %v", m["name"], err)
			}
			runner.members[auto.Name] = auto
		}
		defined, err := e.DefineSessionBundle(memql.NewAuthoredRuntimeRegistry(), owner, strings.Join(sources, "\n\n"), "")
		if err != nil || !defined.OK {
			t.Fatalf("the executing node would refuse the draft: %v (%+v)", err, defined.Diagnostics)
		}
		if _, err := e.Execute(auth.ContextWithInternalOrigin(ownerCtx), "mutation createWorkRun("+renderArgs(t, map[string]any{
			"runId": req.RunId, "goalId": req.GoalId, "automationName": out.AutomationName, "goalSignature": out.Signature,
			"templateFingerprint": out.TemplateFingerprint, "templateConstructId": out.ConstructId, "templateVersion": out.TemplateVersion,
			"input": req.Input, "variables": req.Input, "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano),
		})+")"); err != nil {
			t.Fatal(err)
		}
		executor := automations.NewExecutor(automations.ExecutorOptions{Engine: e, Logger: quiet, StepRegistry: steps.NewRegistry(), AutomationTrigger: runner})
		defer executor.Close()
		runCtx := common.ContextWithRun(ownerCtx, common.RunContext{RunId: req.RunId, GoalId: req.GoalId, OwnerUserId: owner, Mode: common.RunModeLive})
		exec, err := executor.ExecuteAdopted(runCtx, runner.members[out.AutomationName], automations.RunAdoption{RunId: req.RunId, Variables: req.Input})
		if err != nil || exec.Status != "completed" {
			t.Fatalf("goal %q did not run: %+v %v", statement, exec, err)
		}
		if runs := read("workRunForOwner", map[string]any{"runId": req.RunId}); len(runs) != 1 || runs[0]["status"] != "succeeded" {
			t.Fatalf("goal %q's run did not close succeeded: %+v", statement, runs)
		}
		return out, bridge, req.RunId
	}

	integ := New(e, quiet)
	integ.SetCompiler(&catalogDBBridge{engine: e})
	catalogue := func(t *testing.T, runId string) map[string]any {
		t.Helper()
		trigger := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: catalogAutomationActor, Synthetic: true}))
		nodes, err := integ.handleCatalogSections(trigger, map[string]any{"runId": runId, "ownerUserId": owner}, 0)
		if err != nil {
			t.Fatalf("catalogSections: %v", err)
		}
		return decodeReply(t, nodes)
	}

	summary := map[string]any{"label": "summary", "instruction": "Summarise the invoices.", "purpose": "summarise the month's invoices",
		"inputs": []string{"month"}, "outputs": []string{"invoiceSummary"}, "reuseIntent": "reusable"}

	// Goal A: both sections planned live, each an automation of its own,
	// each handed what the section before it RETURNED.
	outA, _, runA := compileAndRun(t, "Summarise last month's invoices and count their lines", map[string]any{
		"complexity": "moderate", "requiresFile": false, "sectionable": true, "assembly": "report both",
		"sections": []map[string]any{summary, {"label": "count", "instruction": "Count the lines.", "purpose": "count the invoice lines",
			"inputs": []string{"invoiceSummary"}, "outputs": []string{"lineCount"}}},
	})
	if len(outA.LiveSections) != 2 || outA.LiveSections[0].Automation == "" || outA.LiveSections[1].Automation == "" {
		t.Fatalf("goal A wrote its live sections as %+v, want both as automations", outA.LiveSections)
	}
	summaryAuto := outA.LiveSections[0].Automation
	promptsA := turns.take()
	if len(promptsA) != 3 || !strings.Contains(promptsA[1], `{"invoiceSummary":`) || !strings.Contains(promptsA[1], "SUMMARY_REPLY") ||
		!strings.Contains(promptsA[2], "SUMMARY_REPLY") || !strings.Contains(promptsA[2], "COUNT_REPLY") {
		t.Fatalf("goal A's turns were not handed what the sections returned: %q", promptsA)
	}

	// Its success catalogues both, through the real handler.
	reply := catalogue(t, runA)
	if catalogued, _ := reply["catalogued"].([]any); len(catalogued) != 2 {
		t.Fatalf("goal A's success catalogued %v, want both sections", reply)
	}
	sig := work.SectionSignature(work.Section{Purpose: "summarise the month's invoices", Inputs: []string{"month"}})
	held := read("cataloguedConstructsForGoalSignature", map[string]any{"goalSignature": sig})
	if len(held) != 1 || held[0]["name"] != summaryAuto || held[0]["status"] != "active" || held[0]["catalogued"] != true {
		t.Fatalf("the owner's catalog holds %+v for the summary's signature, want %s active and catalogued", held, summaryAuto)
	}
	if again := catalogue(t, runA); len(again["catalogued"].([]any)) != 0 {
		t.Fatalf("a second delivery catalogued again: %v", again)
	}

	// Goal B: a different goal asking for the same section. Served by the
	// exact tier -- no planning, only triage spent -- and run from B's own
	// bundle, which carries the catalogued source.
	outB, bridgeB, runB := compileAndRun(t, "Summarise last month's invoices and archive the summary", map[string]any{
		"complexity": "moderate", "requiresFile": false, "sectionable": true, "assembly": "report both",
		"sections": []map[string]any{summary, {"label": "archive", "instruction": "Archive the summary.", "purpose": "archive the invoice summary",
			"inputs": []string{"invoiceSummary"}, "outputs": []string{"archived"}}},
	})
	if outB.Sections[0].Route != work.SectionCatalogExact || outB.Sections[0].Candidate == nil || outB.Sections[0].Candidate.Name != summaryAuto {
		t.Fatalf("goal B's summary routed %+v, want an exact hit on %s", outB.Sections[0], summaryAuto)
	}
	if outB.ModelCalls != 1 || bridgeB.calls != 1 {
		t.Fatalf("goal B's compile spent %d model calls, want only its triage", outB.ModelCalls)
	}
	promptsB := turns.take()
	if len(promptsB) != 3 || !strings.Contains(promptsB[0], "Overall goal (context only): Summarise last month's invoices and archive the summary") {
		t.Fatalf("the catalogued summary was not told goal B's goal: %q", promptsB)
	}
	// B's success catalogues its own live section, and writes nothing for
	// the summary it was served: the owner's catalog already holds it.
	replyB := catalogue(t, runB)
	left, _ := replyB["notCatalogued"].([]any)
	if catalogued, _ := replyB["catalogued"].([]any); len(catalogued) != 1 || len(left) != 1 ||
		left[0].(map[string]any)["automation"] != summaryAuto || !strings.Contains(fmt.Sprint(left[0]), "already holds") {
		t.Fatalf("goal B's success = %v, want its archive catalogued and the served summary left as it is", replyB)
	}

	// The reuse sweep counts both goals that called it by name: reusable.
	var res ReuseSweepResult
	integ.reuseSweepOwner(ctx, owner, &res)
	if res.Errors != 0 {
		t.Fatalf("the reuse sweep failed: %+v", res)
	}
	labelled := read("cataloguedConstructsForGoalSignature", map[string]any{"goalSignature": sig})
	if len(labelled) != 1 || labelled[0]["reuse"] != "reusable" {
		t.Fatalf("after goals A and B the summary is labelled %+v, want reusable", labelled)
	}

	// A goal asking for something merely CLOSE is now served by the near
	// tier, which only a reusable automation can reach.
	nearSummary := map[string]any{"label": "monthly summary", "instruction": "Summarise them.", "purpose": "summarise the month's invoices by team",
		"inputs": []string{"month"}, "outputs": []string{"invoiceSummary"}}
	bridgeC := &catalogDBBridge{engine: e, triage: map[string]any{
		"complexity": "moderate", "requiresFile": false, "sectionable": true, "assembly": "report both",
		"sections": []map[string]any{nearSummary, {"label": "archive", "instruction": "Archive the summary.", "purpose": "archive the invoice summary",
			"inputs": []string{"invoiceSummary"}, "outputs": []string{"archived"}}},
	}}
	outC, err := planner.NewPlannerAgentLoop(bridgeC, quiet).CompileGoalForRun(ownerCtx, planner.CompileRequest{
		GoalId: "v1:work:goal:" + id.NewShortId(), RunId: "v1:work:run:" + id.NewShortId(), OwnerUserId: owner,
		Statement: "Summarise last month's invoices by team and archive them", Input: map[string]any{"month": "2026-08"},
	}, nil, bridgeC)
	if err != nil {
		t.Fatal(err)
	}
	if outC.Sections[0].Route != work.SectionCatalogNear || outC.Sections[0].Candidate == nil || outC.Sections[0].Candidate.Name != summaryAuto {
		t.Fatalf("a close section routed %+v, want a near hit on the reusable %s", outC.Sections[0], summaryAuto)
	}
	if outC.Sections[1].Route != work.SectionCatalogExact {
		t.Fatalf("goal C's archive section routed %q, want the exact hit goal B's success catalogued", outC.Sections[1].Route)
	}
	if outC.ModelCalls != 1 || bridgeC.calls != 1 {
		t.Fatalf("goal C's compile spent %d model calls, want only its triage", outC.ModelCalls)
	}
}

// renderArgs renders a call's arguments.
func renderArgs(t *testing.T, args map[string]any) string {
	t.Helper()
	c := call("x", args)
	return strings.TrimSuffix(strings.TrimPrefix(c, "x("), ")")
}
