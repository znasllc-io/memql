package planner

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/events"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// work_compile_section_automations_test.go -- a section planned live is an
// automation of its own, so a run that succeeds can catalogue it and the next
// goal asking for the same section is served it (epic memql#5414, D24).

// persistedArgs is the arguments a compile wrote one construct with.
func persistedArgs(t *testing.T, e *countingCompileEngine, name string) map[string]string {
	t.Helper()
	for _, q := range e.queries {
		if strings.HasPrefix(q, "createAuthoringConstruct(") && argOf(q, "name") == name {
			out := map[string]string{}
			for _, k := range []string{"constructId", "bundleId", "kind", "name", "targetNamespace", "source"} {
				out[k] = argOf(q, k)
			}
			return out
		}
	}
	t.Fatalf("no construct %s was persisted: %v", name, e.queries)
	return nil
}

// sectionTurns answers every agent turn by the section its prompt names, and
// records each prompt: the value a section automation returns is its turn's
// reply, which is what a later section and the assembly must be handed.
type sectionTurns struct {
	prompts map[string]string
}

func (s *sectionTurns) Execute(ctx context.Context, step *automations.Step, sc *automations.StepContext) (*automations.StepResult, error) {
	args, err := sc.Evaluator.ResolveV1Map(ctx, step.Function.Args)
	if err != nil {
		return nil, err
	}
	prompt, _ := args["prompt"].(string)
	answer := "ASSEMBLED"
	for _, label := range []string{"summary", "notify", "archive"} {
		if strings.Contains(prompt, "\nSection: "+label+"\n") {
			answer = strings.ToUpper(label) + "_TEXT"
		}
	}
	s.prompts[answer] = prompt
	now := time.Now()
	return &automations.StepResult{StepId: step.ID, Status: "success", Result: answer, StartedAt: now, CompletedAt: now}, nil
}

// draftMembers resolves a sub-automation among the draft's members and runs it
// on the real executor over the same registry, the way the node a work run
// executes on resolves one (app/work_template_trigger.go).
type draftMembers struct {
	members map[string]*automations.Automation
	reg     *steps.Registry
	args    map[string]map[string]any
}

func (d *draftMembers) TriggerAutomation(ctx context.Context, name string) (*automations.AutomationExecution, error) {
	return d.TriggerAutomationWithArgs(ctx, name, nil)
}

func (d *draftMembers) TriggerAutomationWithArgs(ctx context.Context, name string, args map[string]any) (*automations.AutomationExecution, error) {
	auto := d.members[name]
	if auto == nil {
		return nil, fmt.Errorf("%s is not a member of the draft", name)
	}
	d.args[name] = args
	exec, err := automations.NewExecutor(automations.ExecutorOptions{StepRegistry: d.reg, AutomationTrigger: d}).
		ExecuteWithClientEvent(ctx, auto, "work.subtemplate", &events.Event{Payload: args})
	if err == nil && exec.Status != "completed" {
		err = fmt.Errorf("member %s ended %s: %s", name, exec.Status, exec.Error)
	}
	return exec, err
}

// runDraft loads every construct a compile persisted in the draft's bundle
// and runs the headline with the goal's input, every agent turn answered by
// sectionTurns.
func runDraft(t *testing.T, e *countingCompileEngine, headline string, input map[string]any) (*sectionTurns, *draftMembers) {
	t.Helper()
	loader := automations.NewLoader(automations.LoaderOptions{})
	turns := &sectionTurns{prompts: map[string]string{}}
	reg := steps.NewRegistry()
	reg.Register(automations.StepTypeFunction, turns)
	members := &draftMembers{members: map[string]*automations.Automation{}, reg: reg, args: map[string]map[string]any{}}
	bundle := persistedArgs(t, e, headline)["bundleId"]
	for _, q := range e.queries {
		if !strings.HasPrefix(q, "createAuthoringConstruct(") || argOf(q, "bundleId") != bundle || argOf(q, "kind") != "automation" {
			continue
		}
		auto, err := loader.CompileSource(argOf(q, "source"), "")
		if err != nil {
			t.Fatalf("member %s does not load: %v", argOf(q, "name"), err)
		}
		members.members[auto.Name] = auto
	}
	auto := members.members[headline]
	if auto == nil {
		t.Fatalf("the draft's bundle holds no headline %s", headline)
	}
	ev := events.Event{Topic: "work.run.dispatched", Payload: input}
	exec, err := automations.NewExecutor(automations.ExecutorOptions{StepRegistry: reg, AutomationTrigger: members}).ExecuteWithEvent(context.Background(), auto, "test", &ev)
	if err != nil || exec.Status != "completed" {
		t.Fatalf("run the draft: %v (%+v)", err, exec)
	}
	return turns, members
}

// TestALiveSectionIsItsOwnAutomationInTheDraft: every section planned live is
// written as an automation of its own in the draft's bundle -- named by its
// section signature, its purpose as @description, its inputs as arguments --
// and the template calls it the way it calls a catalogued one: each input
// bound from the goal or from the section that produced it, and the goal as
// context. The draft passes the REAL Gate 1, and run as the executing node
// runs it, each section is handed the value the section before it RETURNED,
// and the assembly both.
func TestALiveSectionIsItsOwnAutomationInTheDraft(t *testing.T) {
	req, triage, sections := invoiceGoal()
	eng := newSectionCatalogEngine(triage)
	out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
	if err != nil {
		t.Fatalf("CompileGoalForRun: %v", err)
	}
	headline := workDraftHeadline(req)
	if len(out.LiveSections) != 2 {
		t.Fatalf("LiveSections = %+v, want both sections", out.LiveSections)
	}
	src := persistedSource(t, eng.countingCompileEngine, headline)
	template := persistedArgs(t, eng.countingCompileEngine, headline)
	names := make([]string, len(sections))
	for n, s := range sections {
		ls := out.LiveSections[n]
		sig := work.SectionSignature(s)
		if ls.Section != s.Name || ls.Inline != "" || ls.Automation != sectionAutomationNameFor(sig, s.Label) {
			t.Fatalf("section %s was written %+v, want the automation its signature names", s.Name, ls)
		}
		names[n] = ls.Automation
		construct := persistedArgs(t, eng.countingCompileEngine, ls.Automation)
		if construct["kind"] != "automation" || construct["targetNamespace"] != authoredTargetNamespace || construct["bundleId"] != template["bundleId"] {
			t.Errorf("%s was persisted as %v, want an authored automation in the draft's own bundle", ls.Automation, construct)
		}
		read, err := ReadSectionAutomation(construct["source"])
		if err != nil {
			t.Fatalf("%s does not read as a section automation: %v\n%s", ls.Automation, err, construct["source"])
		}
		if read.Signature != sig || read.Purpose != s.Purpose || strings.Join(read.Inputs, ",") != strings.Join(s.Inputs, ",") {
			t.Errorf("%s reads as %+v, want the section's purpose, inputs and signature %s", ls.Automation, read, sig)
		}
	}
	for _, want := range []string{
		headline + "_summary := automation " + names[0] + "(month: args.month, overallGoal: " + langparser.QuoteString(req.Statement) + ")",
		headline + "_notify := automation " + names[1] + "(invoiceSummary: " + headline + "_summary, overallGoal: " + langparser.QuoteString(req.Statement) + ")",
		"assemble := builtin runAgentTurn(",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the template lacks %q:\n%s", want, src)
		}
	}
	if strings.Count(src, "builtin runAgentTurn(") != 1 {
		t.Errorf("the template's one agent turn is the assembly:\n%s", src)
	}
	validated := false
	for _, q := range eng.queries {
		if strings.HasPrefix(q, "recordBundleValidation(") && strings.Contains(q, `status: "validated"`) {
			validated = true
		}
	}
	if !validated {
		t.Fatal("the draft with its section automations did not pass Gate 1")
	}

	turns, members := runDraft(t, eng.countingCompileEngine, headline, req.Input)
	if got := members.args[names[0]]; got["month"] != "2026-08" || got[sectionGoalArg] != req.Statement {
		t.Fatalf("%s was called with %v, want the goal's month and the goal", names[0], got)
	}
	if got := members.args[names[1]]["invoiceSummary"]; got != "SUMMARY_TEXT" {
		t.Fatalf("%s was handed %v, want what the summary section returned", names[1], got)
	}
	notify := turns.prompts["NOTIFY_TEXT"]
	if !strings.Contains(notify, "Inputs (JSON):\n{\"invoiceSummary\":\"SUMMARY_TEXT\"}") ||
		!strings.Contains(notify, "Overall goal (context only): "+req.Statement) ||
		!strings.Contains(notify, "It is finished when: the email to finance was sent") {
		t.Fatalf("the notify section's turn is not told its input, its goal and its end:\n%s", notify)
	}
	assembly := turns.prompts["ASSEMBLED"]
	if !strings.Contains(assembly, "SUMMARY_TEXT") || !strings.Contains(assembly, "NOTIFY_TEXT") {
		t.Fatalf("the assembly was not handed what the sections returned:\n%s", assembly)
	}
}

// TestDecompositionSpendsIntelligenceOnce is #5418's acceptance at the
// compile seam (D24). Goal A plans its summary section live, as an automation
// of its own. Catalogued as the handler (integrations/procedure,
// catalogSections) writes it -- the source alone in a bundle, active,
// catalogued, signed with what ReadSectionAutomation computes -- it is an
// EXACT catalog hit for goal B, a different goal whose triage names a section
// with the same purpose and inputs: B plans it not at all, spends only its
// triage call, calls the catalogued automation with its own input and its own
// goal, and carries the source unchanged. The control is the same goal B with
// nothing catalogued, which plans the section live.
func TestDecompositionSpendsIntelligenceOnce(t *testing.T) {
	reqA, triageA, _ := invoiceGoal()
	engA := newSectionCatalogEngine(triageA)
	outA, err := (&PlannerAgentLoop{engine: engA}).CompileGoalForRun(context.Background(), reqA, nil, realSandbox{})
	if err != nil || len(outA.LiveSections) != 2 || outA.LiveSections[0].Automation == "" {
		t.Fatalf("goal A: %+v %v", outA, err)
	}
	summary := outA.LiveSections[0].Automation
	source := persistedSource(t, engA.countingCompileEngine, summary)
	read, err := ReadSectionAutomation(source)
	if err != nil {
		t.Fatalf("goal A's section does not read back: %v", err)
	}

	reqB := CompileRequest{
		GoalId: "v1:work:goal:g2", RunId: "v1:work:run:r2", OwnerUserId: "u1",
		Statement: "Summarise last month's invoices and file the summary by region",
		Input:     map[string]any{"month": "2026-09"},
	}
	triageB := map[string]any{"complexity": "moderate", "requiresFile": false, "sectionable": true, "assembly": "report both",
		"sections": []map[string]any{
			{"label": "invoice summary", "instruction": "Summarise the month's invoices, briefly.", "purpose": "summarise the month's invoices",
				"inputs": []string{"month"}, "outputs": []string{"invoiceSummary"}},
			{"label": "file", "instruction": "File the summary.", "purpose": "file the invoice summary by region",
				"inputs": []string{"invoiceSummary"}, "outputs": []string{"filed"}},
		}}
	compileB := func(t *testing.T, catalogued bool) (CompileOutcome, *sectionCatalogEngine) {
		t.Helper()
		eng := newSectionCatalogEngine(triageB)
		if catalogued {
			eng.catalogued(read.Name, read.Signature, "", nil, source)
		}
		out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), reqB, nil, realSandbox{})
		if err != nil {
			t.Fatalf("goal B: %v", err)
		}
		return out, eng
	}

	control, _ := compileB(t, false)
	if control.Sections[0].Route != work.SectionIntelligence {
		t.Fatalf("with nothing catalogued goal B's summary routed %q, want planned live -- the control fails, so the hit below proves nothing", control.Sections[0].Route)
	}

	out, eng := compileB(t, true)
	if out.Sections[0].Route != work.SectionCatalogExact || out.Sections[0].Candidate == nil || out.Sections[0].Candidate.Name != summary {
		t.Fatalf("goal B's summary routed %+v, want an exact hit on %s", out.Sections[0], summary)
	}
	if out.ModelCalls != 1 || strings.Join(eng.aiCalls, ",") != "goalComplexityTriage" {
		t.Fatalf("goal B spent %d model calls (%v), want only its triage", out.ModelCalls, eng.aiCalls)
	}
	headline := workDraftHeadline(reqB)
	src := persistedSource(t, eng.countingCompileEngine, headline)
	if want := headline + "_invoice_summary := automation " + summary + "(month: args.month, overallGoal: " + langparser.QuoteString(reqB.Statement) + ")"; !strings.Contains(src, want) {
		t.Fatalf("goal B's draft lacks %q:\n%s", want, src)
	}
	if got := persistedSource(t, eng.countingCompileEngine, summary); got != source {
		t.Fatalf("goal B carries a different source for %s:\n%s", summary, got)
	}
	if out.LiveSections[0].Section != "file" || out.LiveSections[0].Automation == "" {
		t.Fatalf("goal B's other section = %+v, want it planned live as an automation of its own", out.LiveSections)
	}
}

// TestASectionThatCannotBeCutOutStaysInline: a live section is written inline,
// as it always was, when no automation could stand for it -- it names no
// purpose (the shape every sectionable goal had before decomposition, which
// reads the whole goal input), or one of its inputs is bound by nothing -- and
// the outcome says which and why.
func TestASectionThatCannotBeCutOutStaysInline(t *testing.T) {
	req, triage, _ := invoiceGoal()
	s := triage["sections"].([]map[string]any)
	s[0]["purpose"] = ""
	s[1]["inputs"] = []string{"invoiceSummary", "region"}
	eng := newSectionCatalogEngine(triage)
	out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
	if err != nil {
		t.Fatalf("CompileGoalForRun: %v", err)
	}
	if len(out.LiveSections) != 2 || out.LiveSections[0].Automation != "" || out.LiveSections[1].Automation != "" {
		t.Fatalf("LiveSections = %+v, want both inline", out.LiveSections)
	}
	if !strings.Contains(out.LiveSections[0].Inline, "no purpose") || !strings.Contains(out.LiveSections[1].Inline, `"region"`) {
		t.Fatalf("LiveSections = %+v, want the missing purpose and the unbound input named", out.LiveSections)
	}
	src := persistedSource(t, eng.countingCompileEngine, workDraftHeadline(req))
	if strings.Count(src, "builtin runAgentTurn(") != 3 || strings.Contains(src, "automation "+SectionAutomationPrefix) {
		t.Fatalf("both sections and the assembly must be inline agent turns:\n%s", src)
	}
	for _, q := range eng.queries {
		if strings.HasPrefix(q, "createAuthoringConstruct(") && strings.HasPrefix(argOf(q, "name"), SectionAutomationPrefix) {
			t.Fatalf("an inline section was persisted as an automation too: %s", argOf(q, "name"))
		}
	}
}

// refusingSections is Gate 1 refusing any bundle that carries a section
// automation, and the real gate for everything else.
type refusingSections struct{}

func (refusingSections) CompileBundle(cs []memql.SandboxConstruct) memql.SandboxReport {
	for _, c := range cs {
		if strings.HasPrefix(c.Name, SectionAutomationPrefix) {
			return memql.SandboxReport{OK: false, Diagnostics: []memql.SandboxDiagnostic{{Kind: c.Kind, Name: c.Name, Error: "refused"}}}
		}
	}
	return realSandbox{}.CompileBundle(cs)
}

// TestADraftWhoseSectionAutomationsFailGate1IsWrittenInline: a section
// automation that cannot persist never fails the goal. Nothing of the refused
// draft was written; the goal is drafted again with every live section an
// inline turn, and the outcome says why for each.
func TestADraftWhoseSectionAutomationsFailGate1IsWrittenInline(t *testing.T) {
	req, triage, _ := invoiceGoal()
	eng := newSectionCatalogEngine(triage)
	out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), req, nil, refusingSections{})
	if err != nil {
		t.Fatalf("a refused section automation failed the goal: %v", err)
	}
	for _, ls := range out.LiveSections {
		if ls.Automation != "" || !strings.Contains(ls.Inline, "did not persist") {
			t.Fatalf("LiveSections = %+v, want every section inline, saying the automation form did not persist", out.LiveSections)
		}
	}
	src := persistedSource(t, eng.countingCompileEngine, workDraftHeadline(req))
	if strings.Count(src, "builtin runAgentTurn(") != 3 {
		t.Fatalf("the redrafted goal must write both sections inline:\n%s", src)
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

// TestReadSectionAutomationIsTheOneReading: what compile writes reads back as
// the section it was written for, awkward purposes included, and a source
// that is not a section automation -- or whose name does not carry the
// signature its own purpose and inputs compute -- is refused rather than
// catalogued under a key it does not answer.
func TestReadSectionAutomationIsTheOneReading(t *testing.T) {
	purpose := "summarise \"the\" month's invoices \\ by region\nand team"
	sp := sectionPlan{Name: "workRun_x_summary", Spec: sectionSpec{Label: "summary", Instruction: "Summarise.", Purpose: purpose, Inputs: []string{"team", "month"}}}
	sig := work.SectionSignature(workSection(sp))
	ls := &liveSection{Automation: sectionAutomationNameFor(sig, "summary"), Signature: sig, Inputs: []string{"month", "team"}}
	read, err := ReadSectionAutomation(sectionAutomationSource(ls, "agent-1", sp.Spec))
	if err != nil {
		t.Fatalf("ReadSectionAutomation: %v", err)
	}
	if read.Signature != sig || read.Purpose != purpose || strings.Join(read.Inputs, ",") != "month,team" || read.Name != ls.Automation {
		t.Fatalf("read back %+v, want %s with purpose %q and signature %s", read, ls.Automation, purpose, sig)
	}

	renamed := strings.ReplaceAll(sectionAutomationSource(ls, "agent-1", sp.Spec), ls.Automation, SectionAutomationPrefix+"00000000_summary")
	if _, err := ReadSectionAutomation(renamed); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("a name that does not carry its signature was read: %v", err)
	}
	if _, err := ReadSectionAutomation(catalogSource("summariseInvoices", "Summarise.", "month")); err == nil {
		t.Fatal("an automation that is not a section automation was read as one")
	}
	noPurpose := "@template\nautomation " + SectionAutomationPrefix + sig[:8] + " {\n  return 1\n}\n"
	if _, err := ReadSectionAutomation(noPurpose); err == nil || !strings.Contains(err.Error(), "purpose") {
		t.Fatalf("a section automation with no purpose was read: %v", err)
	}
}

// TestACataloguedSectionIsNeverServedAsAWholeGoal: a catalogued section is
// signed with its SECTION's signature, which a goal whose statement and
// inputs normalize to the section's purpose and inputs shares. The goal tier
// passes it over -- served whole, its bundle is no run's draft and the
// executing node refuses it -- and the goal is triaged instead. The control is
// the same row catalogued as a goal's own template, which the tier serves.
func TestACataloguedSectionIsNeverServedAsAWholeGoal(t *testing.T) {
	sig := work.GoalSignature(compileReq().Statement, []string{"day"})
	row := func(catalogKey string) map[string]any {
		return map[string]any{"id": "v1:authoring:construct:s1", "name": SectionAutomationPrefix + sig[:8] + "_summary",
			"goalSignature": sig, "catalogKey": catalogKey, "reliability": 0.9}
	}
	eng := &countingCompileEngine{catalogue: []map[string]any{row(SectionCatalogKeyPrefix + sig)}}
	out, _ := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), compileReq(), nil, nil)
	if out.Route == work.RouteCatalogExact || out.ModelCalls == 0 {
		t.Fatalf("a catalogued section was served as the whole goal: %+v", out)
	}

	control := &countingCompileEngine{catalogue: []map[string]any{row("")}}
	out, err := (&PlannerAgentLoop{engine: control}).CompileGoalForRun(context.Background(), compileReq(), nil, nil)
	if err != nil || out.Route != work.RouteCatalogExact || out.ConstructId != "v1:authoring:construct:s1" {
		t.Fatalf("the control -- the row as a goal's own template -- was not served: %+v %v", out, err)
	}
}
