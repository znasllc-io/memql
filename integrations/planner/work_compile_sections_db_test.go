package planner

import (
	"context"
	"database/sql"
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
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// work_compile_sections_db_test.go -- a decomposed goal whose sections the
// catalog serves, end to end against the real schema and engine (epic
// memql#5414, D24): the owner's catalogued automations are found by their
// section signatures, the draft that calls them passes the ENGINE's Gate 1,
// is reloaded from its own bundle rows and defined the way the executing node
// defines a work template (app/work_template.go), runs as an adopted work
// run, and its automation steps are then what the reuse sweep counts.

// memberTrigger resolves a sub-automation among the draft's own members and
// runs it, as app's workTemplateTrigger does, recording each call's
// arguments.
type memberTrigger struct {
	engine  *memql.MemQLEngine
	members map[string]*automations.Automation
	mu      sync.Mutex
	args    map[string]map[string]any
}

func (m *memberTrigger) TriggerAutomation(ctx context.Context, name string) (*automations.AutomationExecution, error) {
	return m.TriggerAutomationWithArgs(ctx, name, nil)
}

func (m *memberTrigger) TriggerAutomationWithArgs(ctx context.Context, name string, args map[string]any) (*automations.AutomationExecution, error) {
	auto := m.members[name]
	if auto == nil {
		return nil, fmt.Errorf("%s is not a member of the draft", name)
	}
	m.mu.Lock()
	m.args[name] = args
	m.mu.Unlock()
	executor := automations.NewExecutor(automations.ExecutorOptions{Engine: m.engine, Logger: testLogger(), StepRegistry: steps.NewRegistry(), AutomationTrigger: m})
	defer executor.Close()
	res, err := executor.ExecuteWithClientEvent(ctx, auto, "work.subtemplate", &events.Event{Payload: args})
	if err == nil && res.Status != "completed" {
		err = fmt.Errorf("member %s ended %s: %s", name, res.Status, res.Error)
	}
	return res, err
}

func TestCompileSectionsDB_ACataloguedSectionRunsFromTheDraft(t *testing.T) {
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "catalogued sections", dbtest.DSN(), err)
	}
	for _, q := range []string{`CREATE TEMP TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`, `CREATE TEMP TABLE node_vectors (LIKE public.node_vectors INCLUDING ALL)`} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	e, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	e.Logger = testLogger()
	if err := e.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	const owner = "v1:identity:user:sections-owner"
	ownerCtx := auth.ContextWithUserActor(ctx, owner)
	write := func(internal bool, name string, args map[string]any) {
		t.Helper()
		wctx := ownerCtx
		if internal {
			wctx = auth.ContextWithInternalOrigin(ownerCtx)
		}
		if _, err := e.Execute(wctx, "mutation "+name+"("+encodeArgs(args)+")"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	req := CompileRequest{
		GoalId: "v1:work:goal:sections-goal", RunId: "v1:work:run:" + id.NewShortId(), OwnerUserId: owner,
		Statement: "Summarise last month's invoices and count their lines",
		Input:     map[string]any{"month": "2026-08"},
	}
	triage := map[string]any{"complexity": "moderate", "requiresFile": false, "sectionable": true, "sections": []map[string]any{
		{"label": "summary", "instruction": "Summarise the invoices.", "purpose": "summarise the month's invoices",
			"inputs": []string{"month"}, "outputs": []string{"invoiceSummary"}, "reuseIntent": "reusable"},
		{"label": "count", "instruction": "Count the lines.", "purpose": "count the invoice lines",
			"inputs": []string{"invoiceSummary"}, "outputs": []string{"lineCount"}, "reuseIntent": "reusable"},
	}}
	usable := parseSectionableDecision(triage).usableSections(workDraftHeadline(req))
	// The owner's catalog: one active, catalogued automation per section,
	// each recorded under the signature of the section it answers.
	for n, name := range []string{"summariseInvoices", "countInvoiceLines"} {
		arg := []string{"month", "invoiceSummary"}[n]
		bundleId, constructId := "sections-bundle-"+name, "sections-"+name
		write(false, "createAuthoringBundle", map[string]any{"bundleId": bundleId, "title": name})
		write(false, "createAuthoringConstruct", map[string]any{"constructId": constructId, "bundleId": bundleId, "kind": "automation",
			"name": name, "targetNamespace": authoredTargetNamespace, "source": catalogSource(name, "Answer a section.", arg)})
		write(false, "setConstructStatus", map[string]any{"constructId": constructId, "status": "active"})
		write(false, "activateAuthoringBundle", map[string]any{"bundleId": bundleId})
		write(false, "catalogueConstruct", map[string]any{"constructId": constructId, "catalogKey": name, "catalogMatchText": "kind:automation intent:Answer a section.", "fromBundleId": bundleId})
		write(true, "recordConstructGoalSignature", map[string]any{"constructId": constructId, "goalSignature": work.SectionSignature(workSection(usable[n]))})
	}

	bridge := &draftDBCompiler{engine: e, triage: triage}
	out, err := (&PlannerAgentLoop{engine: bridge, logger: testLogger()}).CompileGoalForRun(ownerCtx, req, nil, bridge)
	if err != nil {
		t.Fatalf("CompileGoalForRun: %v", err)
	}
	if out.ModelCalls != 1 || bridge.calls != 1 {
		t.Fatalf("compile spent %d model calls (%d reached the bridge), want only the triage", out.ModelCalls, bridge.calls)
	}
	if len(out.Sections) != 2 || out.Sections[0].Route != work.SectionCatalogExact || out.Sections[1].Route != work.SectionCatalogExact {
		t.Fatalf("sections = %+v, want both served from the catalog", out.Sections)
	}

	// Reload the draft from its OWN bundle rows, as the executing node does.
	read := func(name string, args map[string]any) []map[string]any {
		t.Helper()
		r, err := e.Execute(ownerCtx, "query "+name+"("+encodeArgs(args)+")")
		if err != nil {
			t.Fatal(err)
		}
		return memql.MaterializeRows(r)
	}
	headlines := read("authoringConstructById", map[string]any{"constructId": out.ConstructId})
	if len(headlines) != 1 {
		t.Fatalf("the draft headline is not readable: %+v", headlines)
	}
	bundleId, _ := headlines[0]["bundleId"].(string)
	if b := read("authoringBundleById", map[string]any{"bundleId": bundleId}); len(b) != 1 || b[0]["status"] != "validated" {
		t.Fatalf("the draft bundle did not pass Gate 1: %+v", b)
	}
	members := read("authoringConstructsForBundle", map[string]any{"bundleId": bundleId})
	var sources []string
	trigger := &memberTrigger{engine: e, members: map[string]*automations.Automation{}, args: map[string]map[string]any{}}
	loader := automations.NewLoader(automations.LoaderOptions{Logger: testLogger()})
	var headline *automations.Automation
	for _, m := range members {
		source, _ := m["source"].(string)
		sources = append(sources, source)
		auto, err := loader.CompileSource(source, "")
		if err != nil {
			t.Fatalf("member %v does not load: %v", m["name"], err)
		}
		trigger.members[auto.Name] = auto
		if auto.Name == out.AutomationName {
			headline = auto
		}
	}
	if headline == nil || trigger.members["summariseInvoices"] == nil || trigger.members["countInvoiceLines"] == nil {
		t.Fatalf("the draft does not carry its headline and both catalogued automations: %v", members)
	}
	defined, err := e.DefineSessionBundle(memql.NewAuthoredRuntimeRegistry(), owner, strings.Join(sources, "\n\n"), "")
	if err != nil || !defined.OK {
		t.Fatalf("the executing node would refuse the draft: %v (%+v)", err, defined.Diagnostics)
	}

	// Run it as the adopted work run compile opened it for.
	if _, err := e.Execute(auth.ContextWithInternalOrigin(ownerCtx), "mutation createWorkRun("+encodeArgs(map[string]any{
		"runId": req.RunId, "goalId": req.GoalId, "automationName": out.AutomationName, "goalSignature": out.Signature,
		"templateFingerprint": out.TemplateFingerprint, "templateConstructId": out.ConstructId, "templateVersion": out.TemplateVersion,
		"input": req.Input, "variables": req.Input, "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano),
	})+")"); err != nil {
		t.Fatal(err)
	}
	executor := automations.NewExecutor(automations.ExecutorOptions{Engine: e, Logger: testLogger(), StepRegistry: steps.NewRegistry(), AutomationTrigger: trigger})
	defer executor.Close()
	runCtx := common.ContextWithRun(ownerCtx, common.RunContext{RunId: req.RunId, GoalId: req.GoalId, OwnerUserId: owner, Mode: common.RunModeLive})
	execution, err := executor.ExecuteAdopted(runCtx, headline, automations.RunAdoption{RunId: req.RunId, Variables: req.Input})
	if err != nil || execution.Status != "completed" {
		t.Fatalf("the draft did not run: %+v %v", execution, err)
	}
	if got := trigger.args["summariseInvoices"]["month"]; got != "2026-08" {
		t.Fatalf("summariseInvoices got month %v, want the goal's", got)
	}
	summary, _ := trigger.args["countInvoiceLines"]["invoiceSummary"].(map[string]any)
	if summary == nil || summary["automation"] != "summariseInvoices" {
		t.Fatalf("countInvoiceLines was not handed the summary section's value: %v", trigger.args["countInvoiceLines"])
	}

	// And those calls are the reuse sweep's evidence: automation steps of
	// the owner's run, each naming the construct it called.
	called := map[string]bool{}
	for _, s := range read("workAutomationStepsForOwner", nil) {
		if memql.BareShortId(fmt.Sprint(s["runId"])) != memql.BareShortId(req.RunId) {
			continue
		}
		if c, _ := s["call"].(map[string]any); c != nil {
			called[fmt.Sprint(c["name"])] = true
		}
	}
	if !called["summariseInvoices"] || !called["countInvoiceLines"] {
		t.Fatalf("the run's automation steps are %v; the reuse sweep would count neither section", called)
	}
}
