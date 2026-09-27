package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// work_compile_sections_test.go -- decomposition at compile (epic memql#5414,
// design D24): the boundary rule, catalog-first per section, and the draft
// that calls a catalogued automation instead of planning its section live.

// sectionCatalogEngine answers the catalog reads compile makes for sections
// -- the exact tier by the signature in the query, the owner's catalog, a
// bundle's members and row -- the way the owner-scoped queries do, and hands
// everything else (the triage call, the draft writes) to the counting fake.
type sectionCatalogEngine struct {
	*countingCompileEngine
	mu sync.Mutex
	// constructs are the owner's catalogued constructs.
	constructs []map[string]any
	// bundles are each bundle's members by bundle id.
	bundles map[string][]map[string]any
	// guidance answers workDescriptionGuidance.
	guidance []map[string]any
	// actors records the actor of every catalog read.
	actors []string
}

func newSectionCatalogEngine(triage map[string]any) *sectionCatalogEngine {
	return &sectionCatalogEngine{countingCompileEngine: &countingCompileEngine{triage: triage}, bundles: map[string][]map[string]any{}}
}

var quotedArg = regexp.MustCompile(`(\w+): "((?:[^"\\]|\\.)*)"`)

// argOf reads one string argument off a rendered call.
func argOf(q, name string) string {
	for _, m := range quotedArg.FindAllStringSubmatch(q, -1) {
		if m[1] == name {
			var s string
			_ = json.Unmarshal([]byte(`"`+m[2]+`"`), &s)
			return s
		}
	}
	return ""
}

func (e *sectionCatalogEngine) Execute(ctx context.Context, q string) (any, error) {
	for _, name := range []string{"cataloguedConstructsForGoalSignature", "cataloguedConstructsForOwner", "authoringConstructsForBundle", "authoringBundleById", "workDescriptionGuidance"} {
		if !strings.Contains(q, name+"(") {
			continue
		}
		e.countingCompileEngine.queries = append(e.countingCompileEngine.queries, q)
		e.mu.Lock()
		defer e.mu.Unlock()
		if ac, ok := accessOf(ctx); ok {
			e.actors = append(e.actors, ac)
		}
		var out []map[string]any
		switch name {
		case "cataloguedConstructsForGoalSignature":
			for _, c := range e.constructs {
				if c["goalSignature"] == argOf(q, "goalSignature") {
					out = append(out, c)
				}
			}
		case "cataloguedConstructsForOwner":
			out = append(out, e.constructs...)
		case "authoringConstructsForBundle":
			out = e.bundles[argOf(q, "bundleId")]
		case "authoringBundleById":
			if _, ok := e.bundles[argOf(q, "bundleId")]; ok {
				out = []map[string]any{{"id": argOf(q, "bundleId"), "status": "active"}}
			}
		case "workDescriptionGuidance":
			out = e.guidance
		}
		return out, nil
	}
	return e.countingCompileEngine.Execute(ctx, q)
}

// catalogued is one catalogued automation: its row and its bundle.
func (e *sectionCatalogEngine) catalogued(name, signature, reuse string, override map[string]any, source string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	row := map[string]any{
		"id": "v1:authoring:construct:" + name, "name": name, "kind": "automation", "status": "active",
		"catalogued": true, "goalSignature": signature, "bundleId": "v1:authoring:bundle:" + name,
		"source": source, "ownerUserId": "u1", "reliability": 0.9,
	}
	if reuse != "" {
		row["reuse"] = reuse
	}
	if override != nil {
		row["reuseOverride"] = override
	}
	e.constructs = append(e.constructs, row)
	e.bundles["v1:authoring:bundle:"+name] = []map[string]any{{
		"id": row["id"], "name": name, "kind": "automation", "status": "active", "source": source,
		"bundleId": row["bundleId"], "ownerUserId": "u1",
	}}
}

// catalogSource is a catalogued automation that declares the given
// arguments and uses each.
func catalogSource(name, doc string, args ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "/// %s\n@template\nautomation %s {\n", doc, name)
	if len(args) > 0 {
		b.WriteString("  args {\n")
		for _, a := range args {
			fmt.Fprintf(&b, "    %s any\n", a)
		}
		b.WriteString("  }\n")
	}
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, "args."+a)
	}
	if len(parts) == 0 {
		parts = []string{`"done"`}
	}
	fmt.Fprintf(&b, "  return [%s]\n}\n", strings.Join(parts, ", "))
	return b.String()
}

// invoiceGoal is a goal cut into two ordered sections: a summary, and the
// email that sends it -- which reaches outside, and says how its end is
// checked.
func invoiceGoal() (CompileRequest, map[string]any, []work.Section) {
	req := CompileRequest{
		GoalId: "v1:work:goal:g1", RunId: "v1:work:run:r1", OwnerUserId: "u1",
		Statement: "Summarise last month's invoices and email the totals to finance",
		Input:     map[string]any{"month": "2026-08"},
	}
	sections := []map[string]any{
		{"label": "summary", "instruction": "Summarise the invoices.", "purpose": "Summarise the month's invoices",
			"inputs": []string{"month"}, "outputs": []string{"invoiceSummary"}, "reuseIntent": "reusable", "effects": []string{}},
		{"label": "notify", "instruction": "Email the summary.", "purpose": "email the finance team",
			"inputs": []string{"invoiceSummary"}, "outputs": []string{"emailed"}, "reuseIntent": "reusable",
			"effects": []string{"external"}, "postcondition": "the email to finance was sent"},
	}
	triage := map[string]any{"complexity": "moderate", "requiresFile": false, "sectionable": true, "sections": sections,
		"assembly": "report both"}
	parsed := parseSectionableDecision(triage)
	var ws []work.Section
	for _, sp := range parsed.usableSections(workDraftHeadline(req)) {
		ws = append(ws, workSection(sp))
	}
	return req, triage, ws
}

// persistedSource is the source a compile wrote for one construct.
func persistedSource(t *testing.T, e *countingCompileEngine, name string) string {
	t.Helper()
	for _, q := range e.queries {
		if !strings.HasPrefix(q, "createAuthoringConstruct(") || argOf(q, "name") != name {
			continue
		}
		return argOf(q, "source")
	}
	t.Fatalf("no construct %s was persisted: %v", name, e.queries)
	return ""
}

// TestADecomposedGoalWhoseSectionsAreAllCataloguedSpendsOnlyTheTriageCall is
// #5418's acceptance: "a section with a catalogued reusable automation spends
// no model". The summary is an exact hit on its section signature; the email
// is a near hit on a REUSABLE automation that covers its input. Compile makes
// ONE provider call -- the triage that cut the goal -- and the draft it
// persists calls both automations, carries their sources, calls no agent turn
// and resolves no agent, and returns the sections' results with no assembly
// turn: nothing in the plan spends a model beyond triage.
func TestADecomposedGoalWhoseSectionsAreAllCataloguedSpendsOnlyTheTriageCall(t *testing.T) {
	req, triage, sections := invoiceGoal()
	eng := newSectionCatalogEngine(triage)
	eng.catalogued("summariseInvoices", work.SectionSignature(sections[0]), "", nil,
		catalogSource("summariseInvoices", "Summarise the month's invoices.", "month"))
	eng.catalogued("emailFinanceTeam", "some-other-goal", "reusable", nil,
		catalogSource("emailFinanceTeam", "Email the finance team.", "invoiceSummary"))

	out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
	if err != nil {
		t.Fatalf("CompileGoalForRun: %v", err)
	}
	if out.Route != work.RouteSectionable || out.DecompositionRefused != "" {
		t.Fatalf("outcome = %+v, want a sectionable draft", out)
	}
	if out.ModelCalls != 1 || strings.Join(eng.aiCalls, ",") != "goalComplexityTriage" {
		t.Fatalf("compile spent %d model calls (%v); a decomposition the catalog serves spends only its triage", out.ModelCalls, eng.aiCalls)
	}
	if len(out.Sections) != 2 || out.Sections[0].Route != work.SectionCatalogExact || out.Sections[1].Route != work.SectionCatalogNear {
		t.Fatalf("sections = %+v, want exact then near", out.Sections)
	}
	headline := workDraftHeadline(req)
	src := persistedSource(t, eng.countingCompileEngine, headline)
	for _, want := range []string{
		"automation summariseInvoices(month: args.month)",
		"automation emailFinanceTeam(invoiceSummary: " + headline + "_summary)",
		"return sections",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the draft lacks %q:\n%s", want, src)
		}
	}
	if strings.Contains(src, "runAgentTurn") {
		t.Fatalf("a draft every section of which the catalog serves still calls an agent turn:\n%s", src)
	}
	for _, name := range []string{"summariseInvoices", "emailFinanceTeam"} {
		if !strings.Contains(persistedSource(t, eng.countingCompileEngine, name), "automation "+name) {
			t.Errorf("the draft does not carry %s, so its call would resolve to nothing where the run executes", name)
		}
	}
	for _, q := range eng.queries {
		if strings.Contains(q, "assistantAgentForUser") || strings.Contains(q, "agentById") {
			t.Fatalf("a draft that calls no agent resolved one: %s", q)
		}
	}
	for _, actor := range eng.actors {
		if actor != "u1" {
			t.Fatalf("a catalog read ran as %q, want the goal's owner", actor)
		}
	}
}

// TestADecomposedGoalWithALiveSectionSpendsItAtRunTime is the negative
// control of the headline above: the same goal with nothing catalogued plans
// both sections live, keeps the assembly turn, and still spends only the
// triage call at COMPILE -- the live sections are spent when the run
// executes, not counted here as if they had been. Each live section is an
// automation of its own holding its one agent turn; the template's own turn
// is the assembly.
func TestADecomposedGoalWithALiveSectionSpendsItAtRunTime(t *testing.T) {
	req, triage, _ := invoiceGoal()
	eng := newSectionCatalogEngine(triage)
	out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
	if err != nil {
		t.Fatalf("CompileGoalForRun: %v", err)
	}
	if out.ModelCalls != 1 || len(out.Sections) != 2 || out.Sections[0].Route != work.SectionIntelligence || out.Sections[1].Route != work.SectionIntelligence {
		t.Fatalf("outcome = %+v", out)
	}
	src := persistedSource(t, eng.countingCompileEngine, workDraftHeadline(req))
	if strings.Count(src, "builtin runAgentTurn(") != 1 || !strings.Contains(src, "assemble := ") {
		t.Fatalf("the assembly must be the template's one agent turn:\n%s", src)
	}
	if len(out.LiveSections) != 2 {
		t.Fatalf("LiveSections = %+v, want both sections", out.LiveSections)
	}
	for _, ls := range out.LiveSections {
		if ls.Automation == "" {
			t.Fatalf("section %s stayed inline: %s", ls.Section, ls.Inline)
		}
		if !strings.Contains(src, "automation "+ls.Automation+"(") {
			t.Errorf("the template does not call %s:\n%s", ls.Automation, src)
		}
		if n := strings.Count(persistedSource(t, eng.countingCompileEngine, ls.Automation), "builtin runAgentTurn("); n != 1 {
			t.Errorf("%s holds %d agent turns, want its one", ls.Automation, n)
		}
	}
}

// TestAMidEffectDecompositionFallsBackToAuthoring is #5418's acceptance: "a
// section ending mid-effect is refused". The email reaches outside and
// declares no postcondition, so the decomposition is refused WHOLE -- the
// goal goes to the author route, which asks the design prompt -- and the
// outcome records the boundary rule's reason. No draft of the refused
// sections is persisted. The same decomposition with the postcondition is
// accepted: the refusal is the rule's, not the shape's.
func TestAMidEffectDecompositionFallsBackToAuthoring(t *testing.T) {
	req, triage, _ := invoiceGoal()
	sections := triage["sections"].([]map[string]any)
	sections[1]["postcondition"] = ""
	eng := newSectionCatalogEngine(triage)

	out, _ := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
	if out.Route != work.RouteAuthor {
		t.Fatalf("route = %q, want the author route", out.Route)
	}
	if !strings.HasPrefix(out.DecompositionRefused, "decomposition_refused:") || !strings.Contains(out.DecompositionRefused, `"notify"`) ||
		!strings.Contains(out.DecompositionRefused, "mid-effect") {
		t.Fatalf("DecompositionRefused = %q, want the boundary rule's reason naming the section", out.DecompositionRefused)
	}
	if strings.Join(eng.aiCalls, ",") != "goalComplexityTriage,authoringDesign" {
		t.Fatalf("model calls = %v, want triage and then the design pass", eng.aiCalls)
	}
	for _, q := range eng.queries {
		if strings.HasPrefix(q, "createAuthoringBundle(") {
			t.Fatalf("a refused decomposition persisted a draft: %s", q)
		}
	}

	sections[1]["postcondition"] = "the email to finance was sent"
	accepted := newSectionCatalogEngine(triage)
	out, err := (&PlannerAgentLoop{engine: accepted}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
	if err != nil || out.Route != work.RouteSectionable || out.DecompositionRefused != "" {
		t.Fatalf("with its postcondition the decomposition must be accepted: %+v %v", out, err)
	}
}

// TestAGoalSpecificConstructIsNeverASectionNearHit (D24): a construct cut for
// one goal -- or one account, or not yet labelled -- is never served to a
// DIFFERENT section by resemblance, however close its words and arguments.
// The label that counts is the EFFECTIVE one: a person's override wins over
// the evidence in both directions.
func TestAGoalSpecificConstructIsNeverASectionNearHit(t *testing.T) {
	req, triage, _ := invoiceGoal()
	route := func(t *testing.T, reuse string, override map[string]any) work.SectionRoute {
		t.Helper()
		eng := newSectionCatalogEngine(triage)
		eng.catalogued("emailFinanceTeam", "some-other-goal", reuse, override,
			catalogSource("emailFinanceTeam", "Email the finance team.", "invoiceSummary"))
		out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
		if err != nil || len(out.Sections) != 2 {
			t.Fatalf("compile: %+v %v", out, err)
		}
		return out.Sections[1].Route
	}
	for name, tc := range map[string]struct {
		reuse    string
		override map[string]any
	}{
		"goal-specific":                {reuse: "goalSpecific"},
		"account-specific":             {reuse: "accountSpecific"},
		"unlabelled":                   {},
		"reusable, overridden to goal": {reuse: "reusable", override: map[string]any{"label": "goalSpecific", "version": float64(1)}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := route(t, tc.reuse, tc.override); got != work.SectionIntelligence {
				t.Fatalf("route = %q, want intelligence", got)
			}
		})
	}
	// The controls: reusable by evidence, or by a person's override.
	if got := route(t, "reusable", nil); got != work.SectionCatalogNear {
		t.Fatalf("a reusable construct must be a near hit, got %q", got)
	}
	if got := route(t, "goalSpecific", map[string]any{"label": "reusable", "version": float64(2)}); got != work.SectionCatalogNear {
		t.Fatalf("a person's reusable label must make it a near hit, got %q", got)
	}
	if got := route(t, "reusable", map[string]any{"label": "", "version": float64(3)}); got != work.SectionCatalogNear {
		t.Fatalf("an override handed back to the evidence follows the evidence, got %q", got)
	}
}

// automationCalls records every sub-automation call a draft makes, with its
// evaluated arguments, and completes each.
type automationCalls struct {
	mu    sync.Mutex
	calls []map[string]any
	names []string
}

func (a *automationCalls) TriggerAutomation(ctx context.Context, name string) (*automations.AutomationExecution, error) {
	return a.TriggerAutomationWithArgs(ctx, name, nil)
}

func (a *automationCalls) TriggerAutomationWithArgs(_ context.Context, name string, args map[string]any) (*automations.AutomationExecution, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.names = append(a.names, name)
	a.calls = append(a.calls, args)
	return &automations.AutomationExecution{ID: "exec-" + name, AutomationName: name, Status: "completed"}, nil
}

// TestSectionsBindTheirInputsFromTheGoal: a catalogued section's call binds
// each input by name -- from the goal's input, or from the earlier section
// that produced it -- and the bound value, not its expression, reaches the
// automation when the draft RUNS. A live section written as an automation of
// its own is called the same way, and told the goal it serves. And a
// catalogued section one of whose inputs nothing binds is planned live, and
// -- since no automation could be called with that input either -- stays an
// inline turn that is handed the earlier section's output in its prompt: a
// call without its input would answer a different question.
func TestSectionsBindTheirInputsFromTheGoal(t *testing.T) {
	req, triage, sections := invoiceGoal()
	// A third section reads the summary, and has a catalogued automation
	// whose input `region` neither the goal nor any section provides.
	triage["sections"] = append(triage["sections"].([]map[string]any), map[string]any{
		"label": "archive", "instruction": "File the summary.", "purpose": "file the invoice summary by region",
		"inputs": []string{"invoiceSummary", "region"}, "outputs": []string{"archived"},
	})
	eng := newSectionCatalogEngine(triage)
	eng.catalogued("summariseInvoices", work.SectionSignature(sections[0]), "reusable", nil,
		catalogSource("summariseInvoices", "Summarise the month's invoices.", "month"))
	third := parseSectionableDecision(triage).usableSections(workDraftHeadline(req))[2]
	eng.catalogued("archiveSummary", work.SectionSignature(workSection(third)), "reusable", nil,
		catalogSource("archiveSummary", "File the invoice summary by region.", "invoiceSummary", "region"))

	out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
	if err != nil {
		t.Fatalf("CompileGoalForRun: %v", err)
	}
	if got := []work.SectionRoute{out.Sections[0].Route, out.Sections[1].Route, out.Sections[2].Route}; got[0] != work.SectionCatalogExact ||
		got[1] != work.SectionIntelligence || got[2] != work.SectionIntelligence {
		t.Fatalf("routes = %v, want the summary served and the unbindable archive planned live", got)
	}
	headline := workDraftHeadline(req)
	src := persistedSource(t, eng.countingCompileEngine, headline)
	if strings.Contains(src, "automation archiveSummary(") {
		t.Fatalf("a section whose input nothing binds was served from the catalog:\n%s", src)
	}
	if len(out.LiveSections) != 2 || out.LiveSections[0].Automation == "" || out.LiveSections[1].Automation != "" ||
		!strings.Contains(out.LiveSections[1].Inline, `"region"`) {
		t.Fatalf("LiveSections = %+v, want notify as an automation and archive inline, naming the input nothing binds", out.LiveSections)
	}
	notifyAuto := out.LiveSections[0].Automation
	if !strings.Contains(persistedSource(t, eng.countingCompileEngine, notifyAuto), "It is finished when: the email to finance was sent") {
		t.Fatalf("the live section with an effect is not told how its end is checked:\n%s", persistedSource(t, eng.countingCompileEngine, notifyAuto))
	}

	// Run the draft as the executing node would: the goal's input as the
	// trigger payload, the automation calls through the real step executor.
	auto, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(src, "work-compile/"+headline+".memql")
	if err != nil {
		t.Fatalf("load the draft: %v\n%s", err, src)
	}
	turns := &draftCalls{args: map[string]map[string]any{}}
	subs := &automationCalls{}
	reg := steps.NewRegistry()
	reg.Register(automations.StepTypeFunction, turns)
	ev := events.Event{Topic: "work.run.dispatched", Payload: map[string]any{"month": "2026-08"}}
	exec, err := automations.NewExecutor(automations.ExecutorOptions{StepRegistry: reg, AutomationTrigger: subs}).ExecuteWithEvent(context.Background(), auto, "test", &ev)
	if err != nil || exec.Status != "completed" {
		t.Fatalf("run the draft: %v (%+v)\n%s", err, exec, src)
	}
	if len(subs.names) != 2 || subs.names[0] != "summariseInvoices" || subs.calls[0]["month"] != "2026-08" || subs.names[1] != notifyAuto {
		t.Fatalf("automation calls = %v %v, want summariseInvoices with the goal's month, then %s", subs.names, subs.calls, notifyAuto)
	}
	// automationCalls answers with no returned value, so the summary section
	// binds its run summary -- which is what the notify automation is handed.
	if summary, _ := subs.calls[1]["invoiceSummary"].(map[string]any); summary == nil || summary["executionId"] != "exec-summariseInvoices" {
		t.Fatalf("the notify automation was not handed the summary section's value: %v", subs.calls[1])
	}
	if subs.calls[1][sectionGoalArg] != req.Statement {
		t.Fatalf("the notify automation was told the goal %v, want %q", subs.calls[1][sectionGoalArg], req.Statement)
	}
	archive, _ := turns.args[headline+"_archive"]["prompt"].(string)
	if !strings.Contains(archive, "Results of earlier sections (JSON):") || !strings.Contains(archive, `"invoiceSummary"`) ||
		!strings.Contains(archive, "exec-summariseInvoices") {
		t.Fatalf("the inline section that reads the summary was not handed it:\n%s", archive)
	}
}

// TestGuidanceReachesTriageOnlyWhenAModelIsUsed (D23): the goal's description
// guidance is read, and handed to the prompt, only on the branches that ask a
// model about the goal -- triage, and the design pass when the goal is
// authored. An exact catalog hit and a learned procedure's serve never read
// it. At most five entries reach the prompt, newest first, each as the axes
// a person named and their reason.
func TestGuidanceReachesTriageOnlyWhenAModelIsUsed(t *testing.T) {
	sig := work.GoalSignature(compileReq().Statement, []string{"day"})
	dislikes := func() []map[string]any {
		var out []map[string]any
		for n := 0; n < 7; n++ {
			out = append(out, map[string]any{
				"id": fmt.Sprintf("v1:work:observation:o%d", n), "kind": "feedback",
				"createdAt": time.Date(2026, 9, 20, 12, n, 0, 0, time.UTC).Format(time.RFC3339Nano),
				"data": map[string]any{"verdict": "dislike", "goalSignature": sig, "reason": fmt.Sprintf("reason %d", n),
					"axes": map[string]any{"product": true, "process": n%2 == 0, "performance": false}},
			})
		}
		return out
	}

	t.Run("an exact catalog hit reads none", func(t *testing.T) {
		eng := newSectionCatalogEngine(nil)
		eng.constructs = []map[string]any{{"id": "c1", "name": "summariseTickets", "goalSignature": sig}}
		eng.guidance = dislikes()
		if _, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), compileReq(), nil, nil); err != nil {
			t.Fatal(err)
		}
		assertNoGuidanceRead(t, eng)
	})
	t.Run("a procedure serve reads none", func(t *testing.T) {
		eng := newSectionCatalogEngine(nil)
		eng.procedures = []map[string]any{{"id": "p1", "name": "learnedProcedure_p1", "goalSignature": sig, "ladder": "trusted"}}
		eng.guidance = dislikes()
		if _, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), compileReq(), nil, nil); err != nil {
			t.Fatal(err)
		}
		assertNoGuidanceRead(t, eng)
	})
	t.Run("triage and the design pass get the newest five", func(t *testing.T) {
		eng := &guidanceRecorder{sectionCatalogEngine: newSectionCatalogEngine(map[string]any{"complexity": "complex", "requiresFile": false})}
		eng.guidance = dislikes()
		_, _ = (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), compileReq(), nil, realSandbox{})
		reads := 0
		for _, q := range eng.queries {
			if strings.Contains(q, "workDescriptionGuidance(") {
				reads++
				if !strings.Contains(q, `goalSignature: "`+sig+`"`) {
					t.Errorf("the guidance read is %s, want it keyed by the goal signature", q)
				}
			}
		}
		if reads != 1 {
			t.Fatalf("guidance was read %d times, want once for the goal", reads)
		}
		for _, prompt := range []string{"goalComplexityTriage", "authoringDesign"} {
			got, _ := eng.data[prompt]["guidance"].([]map[string]any)
			if len(got) != maxDescriptionGuidance {
				t.Fatalf("%s got %d guidance entries, want %d: %v", prompt, len(got), maxDescriptionGuidance, eng.data[prompt])
			}
			if got[0]["reason"] != "reason 6" || got[0]["axes"] != "product, process" || got[1]["axes"] != "product" || got[4]["reason"] != "reason 2" {
				t.Fatalf("%s guidance = %v, want the newest five with their axes as words", prompt, got)
			}
		}
	})
}

func assertNoGuidanceRead(t *testing.T, eng *sectionCatalogEngine) {
	t.Helper()
	for _, q := range eng.queries {
		if strings.Contains(q, "workDescriptionGuidance(") {
			t.Fatalf("guidance was read on a goal no model was asked about: %s", q)
		}
	}
	if len(eng.aiCalls) != 0 {
		t.Fatalf("a served goal reached a provider: %v", eng.aiCalls)
	}
}

// guidanceRecorder keeps the data every prompt was invoked with.
type guidanceRecorder struct {
	*sectionCatalogEngine
	data map[string]map[string]any
}

func (g *guidanceRecorder) InvokeAI(ctx context.Context, templateId string, data map[string]any) (any, error) {
	if g.data == nil {
		g.data = map[string]map[string]any{}
	}
	g.data[templateId] = data
	return g.sectionCatalogEngine.InvokeAI(ctx, templateId, data)
}

// accessOf is the user id a context's actor carries.
func accessOf(ctx context.Context) (string, bool) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return "", false
	}
	return ac.UserId, true
}

// TestTheSectionParseIsTolerant: the decision's parse is all-or-nothing, so a
// section field in an unexpected shape must not zero the whole decision. A
// comma-separated string is a list, a section written as a bare string is its
// label, a reuse intent this build does not know is no intent, a list of
// checks is one postcondition -- and a missing field is simply absent.
func TestTheSectionParseIsTolerant(t *testing.T) {
	dec := parseSectionableDecision(map[string]any{
		"complexity": "moderate", "requiresFile": false, "sectionable": true,
		"sections": []any{
			map[string]any{"label": "one", "inputs": "month, team", "outputs": []any{"summary", 7, ""},
				"reuseIntent": "GoalSpecific", "effects": "files", "postcondition": []any{"the file exists", "it is not empty"}},
			"two",
			map[string]any{"label": 3, "reuseIntent": "everywhere"},
		},
	})
	if !dec.Sectionable || len(dec.Sections) != 3 {
		t.Fatalf("a tolerant parse must keep the decision: %+v", dec)
	}
	one := dec.Sections[0]
	if strings.Join(one.Inputs, "|") != "month|team" || strings.Join(one.Outputs, "|") != "summary" ||
		one.ReuseIntent != "goalSpecific" || strings.Join(one.Effects, "|") != "files" ||
		one.Postcondition != "the file exists; it is not empty" {
		t.Fatalf("section one = %+v", one)
	}
	if dec.Sections[1].Label != "two" {
		t.Errorf("a bare string section is its label: %+v", dec.Sections[1])
	}
	if s := dec.Sections[2]; s.Label != "" || s.ReuseIntent != "" || s.Outputs != nil || s.Postcondition != "" {
		t.Errorf("a field that means nothing is absent: %+v", s)
	}
}

// TestAnUnrecognisedEffectStillNeedsAPostcondition: the boundary rule runs on
// "does it change anything", so an effect word this build does not know is
// read as reaching outside -- the direction in which a section ending
// mid-effect is refused rather than passed.
func TestAnUnrecognisedEffectStillNeedsAPostcondition(t *testing.T) {
	if f := footprintOf([]string{"email"}); !f.External || !f.IsSideEffect() {
		t.Fatalf("an unknown effect must be a side effect, got %+v", f)
	}
	if f := footprintOf([]string{"none", ""}); f.IsSideEffect() {
		t.Fatalf("none is no effect, got %+v", f)
	}
	f := footprintOf([]string{"concept:v1:todos:todo", "Files", "concept:v1:todos:todo", "machine", "spend"})
	if strings.Join(f.Concepts, ",") != "v1:todos:todo" || !f.Files || !f.Machine || !f.Spend || f.External {
		t.Fatalf("footprint = %+v", f)
	}
}

// refusingSandbox is Gate 1 refusing any bundle that carries one construct
// -- a catalogued automation whose closure no longer compiles with the
// engine it would run on -- and the real gate for everything else.
type refusingSandbox struct{ refuse string }

func (r refusingSandbox) CompileBundle(cs []memql.SandboxConstruct) memql.SandboxReport {
	for _, c := range cs {
		if c.Name == r.refuse {
			return memql.SandboxReport{OK: false, Diagnostics: []memql.SandboxDiagnostic{{Kind: c.Kind, Name: c.Name, Error: "unresolved reference"}}}
		}
	}
	return realSandbox{}.CompileBundle(cs)
}

// TestACataloguedSectionThatCannotTravelIsPlannedLive: a catalogued
// automation's closure that Gate 1 refuses must not fail the goal. Nothing
// was written by the refused attempt, and the goal is drafted again with
// every section planned live.
func TestACataloguedSectionThatCannotTravelIsPlannedLive(t *testing.T) {
	req, triage, sections := invoiceGoal()
	eng := newSectionCatalogEngine(triage)
	eng.catalogued("summariseInvoices", work.SectionSignature(sections[0]), "", nil,
		catalogSource("summariseInvoices", "Summarise the month's invoices.", "month"))

	out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), req, nil, refusingSandbox{refuse: "summariseInvoices"})
	if err != nil {
		t.Fatalf("a catalogued section that cannot travel failed the goal: %v", err)
	}
	for _, d := range out.Sections {
		if d.Route != work.SectionIntelligence {
			t.Fatalf("sections = %+v, want every section planned live after the refusal", out.Sections)
		}
	}
	src := persistedSource(t, eng.countingCompileEngine, workDraftHeadline(req))
	if strings.Contains(src, "automation summariseInvoices(") || len(out.LiveSections) != 2 {
		t.Fatalf("the redrafted goal must plan both sections live (%+v):\n%s", out.LiveSections, src)
	}
	for _, ls := range out.LiveSections {
		if ls.Automation == "" || !strings.Contains(src, "automation "+ls.Automation+"(") {
			t.Fatalf("live section %+v is not called as an automation of its own:\n%s", ls, src)
		}
	}
	bundles := 0
	for _, q := range eng.queries {
		if strings.HasPrefix(q, "createAuthoringBundle(") {
			bundles++
		}
	}
	if bundles != 1 {
		t.Fatalf("%d draft bundles were written, want the one that passed", bundles)
	}
}

// noProviderTriage answers triage as a cluster with no classifier does.
type noProviderTriage struct{ *countingCompileEngine }

func (n *noProviderTriage) InvokeAI(ctx context.Context, templateId string, data map[string]any) (any, error) {
	n.aiCalls = append(n.aiCalls, templateId)
	return nil, memql.ErrProviderUnavailable("chat54Mini")
}

// TestATriageThatReachedNoProviderIsNotAModelCall: ModelCalls counts only
// calls that ran. A cluster with no classifier made none, and the goal goes on
// to be authored exactly as before.
func TestATriageThatReachedNoProviderIsNotAModelCall(t *testing.T) {
	eng := &noProviderTriage{countingCompileEngine: &countingCompileEngine{}}
	out, _ := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), compileReq(), nil, nil)
	if out.ModelCalls != 0 {
		t.Fatalf("ModelCalls = %d after a triage no provider answered, want 0", out.ModelCalls)
	}
	if out.Route != work.RouteAuthor {
		t.Fatalf("route = %q, want the author route a missing classifier falls back to", out.Route)
	}
}

// TestACataloguedSectionIsNotCalledWithoutAnArgumentItRequires: a reusable
// automation can cover every input a section brings and still REQUIRE one
// more. Called without it, the run would refuse at the automation's own
// argument check; so unless the goal or an earlier section supplies it, the
// section is planned live -- and when the goal does supply it, the call binds
// it too.
func TestACataloguedSectionIsNotCalledWithoutAnArgumentItRequires(t *testing.T) {
	req, triage, _ := invoiceGoal()
	requires := "/// Email the finance team.\n@template\nautomation emailFinanceTeam {\n  args {\n    invoiceSummary any\n    recipient string!\n  }\n  return [args.invoiceSummary, args.recipient]\n}\n"
	compile := func(t *testing.T, input map[string]any) (CompileOutcome, string) {
		t.Helper()
		eng := newSectionCatalogEngine(triage)
		eng.catalogued("emailFinanceTeam", "some-other-goal", "reusable", nil, requires)
		r := req
		r.Input = input
		out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), r, nil, realSandbox{})
		if err != nil {
			t.Fatalf("CompileGoalForRun: %v", err)
		}
		return out, persistedSource(t, eng.countingCompileEngine, workDraftHeadline(r))
	}
	out, src := compile(t, map[string]any{"month": "2026-08"})
	if out.Sections[1].Route != work.SectionIntelligence || strings.Contains(src, "automation emailFinanceTeam(") {
		t.Fatalf("a call missing a required argument was planned: %+v\n%s", out.Sections[1], src)
	}
	out, src = compile(t, map[string]any{"month": "2026-08", "recipient": "finance@example.com"})
	if out.Sections[1].Route != work.SectionCatalogNear ||
		!strings.Contains(src, "automation emailFinanceTeam(invoiceSummary: "+workDraftHeadline(req)+"_summary, recipient: args.recipient)") {
		t.Fatalf("with the goal supplying it the call must bind it: %+v\n%s", out.Sections[1], src)
	}
}

// TestAnOldShapeTriageAnswerStillFansOut: a triage answer that names no
// outputs, effects or postconditions -- the {label, instruction} shape every
// sectionable goal had before decomposition, and what a smaller model still
// writes -- keeps the cheap fan-out. Refusing it as "no end" would send every
// such goal to the author route, a design pass and an emit at a dearer level,
// which is a cost regression on the goals the fan-out exists for. A section
// that CHANGES something without a postcondition is still refused
// (TestAMidEffectDecompositionFallsBackToAuthoring).
func TestAnOldShapeTriageAnswerStillFansOut(t *testing.T) {
	req := CompileRequest{
		GoalId: "v1:work:goal:g2", RunId: "v1:work:run:r2", OwnerUserId: "u1",
		Statement: "Write ten folk tales, each a complete story",
	}
	triage := map[string]any{"complexity": "moderate", "requiresFile": false, "sectionable": true,
		"assembly": "collect the tales in order",
		"sections": []map[string]any{
			{"label": "tale one", "instruction": "Tell the first tale."},
			{"label": "tale two", "instruction": "Tell the second tale."},
		}}
	eng := newSectionCatalogEngine(triage)
	out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Route != work.RouteSectionable || out.DecompositionRefused != "" {
		t.Fatalf("route = %q refused = %q: an old-shape answer must keep the fan-out", out.Route, out.DecompositionRefused)
	}
	for _, c := range eng.aiCalls {
		if c == "authoringDesign" {
			t.Fatalf("model calls = %v: the author route was reached", eng.aiCalls)
		}
	}
}

// TestTriageIsToldTheGoalsInputNames: a section's catalog signature is its
// purpose plus its input names, so triage is handed the goal's own input
// names -- a section that respelled "day" as "date" would miss the automation
// already doing its work, and would bind nothing from the goal's input.
func TestTriageIsToldTheGoalsInputNames(t *testing.T) {
	eng := &guidanceRecorder{sectionCatalogEngine: newSectionCatalogEngine(map[string]any{"complexity": "complex", "requiresFile": false})}
	_, _ = (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), compileReq(), nil, realSandbox{})
	got, _ := eng.data["goalComplexityTriage"]["inputKeys"].([]string)
	if want := inputKeys(compileReq().Input); len(want) == 0 || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("triage inputKeys = %v, want the goal's own input names %v", got, want)
	}
}
