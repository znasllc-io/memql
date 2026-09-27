package planner

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/automations"
	purecompose "github.com/znasllc-io/memql/component/compose"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// reasoningAgent resolves the owner's existing assistant without another model
// call. Its named query filters on the explicit owner under that owner's actor.
func (l *PlannerAgentLoop) reasoningAgent(ctx context.Context, owner string) (string, error) {
	agent, err := ResolveReasoningAgent(ctx, l.engine, owner)
	return agent.Id, err
}

// ReasoningAgentQuerier is the one engine method ResolveReasoningAgent needs.
type ReasoningAgentQuerier interface {
	Execute(ctx context.Context, query string) (any, error)
}

// ReasoningAgent is the agent an owner's reasoning runs under, with the role
// slug its row carries: "assistant", or "system-planner" for the seeded
// planner.
type ReasoningAgent struct {
	Id       string
	RoleSlug string
}

// The two role slugs a reasoning agent resolves to.
const (
	reasoningAssistantRole = "assistant"
	reasoningPlannerRole   = "system-planner"
)

// ResolveReasoningAgent is the ONE resolution of an owner's reasoning agent:
// their active assistant, else the planner seeded for them.
//
// EXPORTED SO THAT IT IS NOT COPIED. A learned procedure's replay acts for the
// owner outside compile -- it dispatches to their machine and hands a diverged
// goal back to their app (epic memql#5408) -- and must answer with the agent
// compile would, whose standing computer-use scope is the consent both of
// those paths are gated on. A second copy of this rule is a second answer to
// "which agent is this person's", and the two would disagree exactly when a
// product bundle supplies the assistant.
func ResolveReasoningAgent(ctx context.Context, engine ReasoningAgentQuerier, owner string) (ReasoningAgent, error) {
	res, err := engine.Execute(ownerActorContext(ctx, owner), "query assistantAgentForUser("+encodeArgs(map[string]any{"ownerUserId": owner})+")")
	if err != nil {
		return ReasoningAgent{}, fmt.Errorf("work compile: resolve reasoning agent: %w", err)
	}
	for _, row := range memql.MaterializeRows(res) {
		if getString(row, "id") != "" {
			return ReasoningAgent{Id: getString(row, "id"), RoleSlug: reasoningAssistantRole}, nil
		}
	}
	// Core installations seed a planner for every user; an assistant may
	// instead be supplied by a product bundle. Read the persisted planner
	// before using it, so neither a missing seed nor another owner's agent
	// can turn into a synthetic successful dispatch.
	plannerId := "plannerAgent-" + memql.BareShortId(owner)
	res, err = engine.Execute(ownerActorContext(ctx, owner), "query agentById("+encodeArgs(map[string]any{"agentId": plannerId})+")")
	if err != nil {
		return ReasoningAgent{}, fmt.Errorf("work compile: resolve seeded reasoning agent: %w", err)
	}
	for _, row := range memql.MaterializeRows(res) {
		if memql.BareShortId(getString(row, "id")) == plannerId && memql.BareShortId(getString(row, "ownerUserId")) == memql.BareShortId(owner) && row["active"] == true && row["deleted"] != true && row["roleSlug"] == reasoningPlannerRole {
			return ReasoningAgent{Id: getString(row, "id"), RoleSlug: reasoningPlannerRole}, nil
		}
	}
	return ReasoningAgent{}, fmt.Errorf("work compile: no active assistant or seeded planner exists for this goal's owner")
}

// File delivery uses the known Materializer capability directly. Other reasoning
// turns remain synchronous agent calls, so every output belongs to this run.
func synthesizeWorkReasoningBundle(req CompileRequest, agentId string, dec sectionableDecision) (authoringBundle, error) {
	if dec.RequiresFile == nil {
		return authoringBundle{}, fmt.Errorf("work compile: triage must explicitly answer requiresFile with a boolean before dispatching a reasoning draft")
	}
	if dec.Navigation != nil && !*dec.RequiresFile && !dec.Sectionable && strings.TrimSpace(dec.Navigation.App) != "" {
		target := dec.Navigation
		headline := workDraftHeadline(req)
		arguments := []string{"app: " + langparser.QuoteString(strings.TrimSpace(target.App))}
		if section := strings.TrimSpace(target.Section); section != "" {
			arguments = append(arguments, "section: "+langparser.QuoteString(section))
		}
		if record := strings.TrimSpace(target.Record); record != "" {
			arguments = append(arguments, "record: "+langparser.QuoteString(record))
		}
		source := fmt.Sprintf("use work.builtins.{ workNavigate }\n\n@template\nautomation %s {\n  navigate := builtin workNavigate(%s)\n}\n", headline, strings.Join(arguments, ", "))
		return authoringBundle{AutomationName: headline, Constructs: []memql.SandboxConstruct{{Kind: "automation", Name: headline, Source: source}}}, nil
	}
	nativeFile := *dec.RequiresFile
	fileName, fileFormat := "", purecompose.Format("")
	if nativeFile {
		fileName = strings.TrimSpace(dec.FileName)
		if fileName == "" {
			return authoringBundle{}, fmt.Errorf("work compile: a file goal requires an explicit fileName from triage")
		}
		var err error
		fileFormat, err = purecompose.ParseFormat(dec.FileFormat)
		if err != nil {
			return authoringBundle{}, fmt.Errorf("work compile: triage fileFormat: %w", err)
		}
	}
	headline := workDraftHeadline(req)
	goal := req.Statement
	sections := dec.usableSections(headline)
	fanout := dec.Sectionable && len(sections) >= minSectionsForFanout
	if fanout && len(dec.Sections) > maxSectionFanout {
		return authoringBundle{}, fmt.Errorf("work compile: %d sections exceeds the %d-section execution limit", len(dec.Sections), maxSectionFanout)
	}
	// Runtime variables keep fork overrides in the work instruction while
	// the validated semantic output choices remain in the stored template.
	const inputHeading = "\n\nGoal input (JSON):\n"
	inputNames, err := workDraftInputNames(req.Input)
	if err != nil {
		return authoringBundle{}, err
	}
	x := workDraftExpressions(inputNames, sections)
	delivery := func(statement, draft string) string {
		instruction := x.join(langparser.QuoteString(statement+inputHeading), x.goalInput)
		if nativeFile {
			args := fmt.Sprintf("name: %s, format: %s, statement: %s", langparser.QuoteString(fileName), langparser.QuoteString(string(fileFormat)), instruction)
			if draft != "" {
				args += ", draft: " + draft
			}
			return "builtin composeMaterialize(" + args + ")"
		}
		if draft != "" {
			instruction = x.join(langparser.QuoteString(statement+inputHeading), x.goalInput, langparser.QuoteString("\n\nCompleted sections:\n"), x.sections)
		}
		return fmt.Sprintf("builtin runAgentTurn(agentId: %s, prompt: %s)", langparser.QuoteString(agentId), instruction)
	}
	var b strings.Builder
	if nativeFile {
		b.WriteString("use compose.builtins.{ composeMaterialize }\n")
	}
	// runAgentTurn is imported only when a statement calls it: a single
	// answer turn, or a section planned live (whose presence also keeps the
	// assembly turn). A decomposition the catalog serves whole calls none.
	if (!fanout && !nativeFile) || (fanout && dec.anyIntelligence(headline)) {
		b.WriteString("use agents.builtins.{ runAgentTurn }\n")
	}
	fmt.Fprintf(&b, "\n@template\nautomation %s {\n", headline)
	if len(inputNames) > 0 {
		b.WriteString("  args {\n")
		for _, name := range inputNames {
			fmt.Fprintf(&b, "    %s any\n", name)
		}
		b.WriteString("  }\n")
	}
	if x.goalInputStatement != "" {
		fmt.Fprintf(&b, "  %s\n", x.goalInputStatement)
	}
	if !fanout {
		fmt.Fprintf(&b, "  reason := %s\n", delivery(goal, ""))
	} else {
		// The sections run one after another, in triage's order, each bound
		// to its own name: a later section and the assembly read earlier
		// values by name, and a parallel branch's names end with the branch.
		producers := map[string]string{}
		dependent := false
		for _, section := range sections {
			if cat := dec.catalogFor(section.Index); cat != nil {
				// A CATALOGUED SECTION CALLS ITS AUTOMATION and spends no
				// model (D24). Its source travels in this bundle, below.
				fmt.Fprintf(&b, "  %s := %s\n", section.Name, cat.callText())
			} else {
				consumed := consumedOutputs(section.Spec.Inputs, producers)
				text := x.join(langparser.QuoteString(sectionPrompt(goal, section.Spec, len(consumed) > 0)+inputHeading), x.goalInput)
				if len(consumed) > 0 {
					// What an earlier section produced reaches this one by
					// name, bound as a map of its own for the reason
					// goalInput is: a call's argument may not be a map
					// literal.
					dependent = true
					mapName := "inputsFor_" + section.Name
					fmt.Fprintf(&b, "  %s := {%s}\n", mapName, strings.Join(consumed, ", "))
					text = x.join(text, langparser.QuoteString(earlierResultsHeading), "toString("+mapName+")")
				}
				fmt.Fprintf(&b, "  %s := builtin runAgentTurn(agentId: %s, prompt: %s)\n", section.Name, langparser.QuoteString(agentId), text)
			}
			for _, o := range section.Spec.Outputs {
				producers[o] = section.Name
			}
		}
		fmt.Fprintf(&b, "  %s\n", x.sectionsStatement)
		if nativeFile || dec.anyIntelligence(headline) {
			kind := "independent sections"
			if dependent {
				kind = "sections"
			}
			assembly := goal + "\n\nAssemble the completed " + kind + " into the requested deliverable. Verify completeness. " + dec.Assembly
			fmt.Fprintf(&b, "  assemble := %s\n", delivery(assembly, x.sections))
		} else {
			// EVERY SECTION WAS SERVED FROM THE CATALOG, so a text answer is
			// the sections' own results. An assembly turn here would spend
			// the one model call the catalog exists to save (D24): the run
			// returns them as they are.
			b.WriteString("  return sections\n")
		}
	}
	b.WriteString("}\n")
	bundle := authoringBundle{AutomationName: headline, Constructs: []memql.SandboxConstruct{{Kind: "automation", Name: headline, Source: b.String()}}}
	if fanout {
		carryCatalogSections(&bundle, dec, sections)
	}
	return bundle, nil
}

// earlierResultsHeading introduces, in a live section's prompt, what the
// earlier sections it reads produced.
const earlierResultsHeading = "\n\nResults of earlier sections (JSON):\n"

// sectionPrompt is the instruction a live section's agent turn runs. A
// section that only produces text drafts its part for the assembly -- and is
// told not to write files, which the assembly or the Materializer does. A
// section that declared EFFECTS makes exactly those changes, which the
// drafting instruction would forbid: its end is the postcondition it named.
func sectionPrompt(goal string, s sectionSpec, dependent bool) string {
	var b strings.Builder
	switch {
	case footprintOf(s.Effects).IsSideEffect():
		b.WriteString("Carry out only the section below, one part of a larger goal, and report what you did as text. Make only the changes this section describes.")
		if dependent {
			b.WriteString(" It builds on the results of earlier sections, given after the goal input.")
		}
	case dependent:
		b.WriteString("Produce only the section below and return its complete content as text; it builds on the results of earlier sections, given after the goal input. This is an intermediate drafting step: do not create or save files and do not call composition tools. Final assembly will produce the deliverable.")
	default:
		b.WriteString("Produce only the independent section below and return its complete content as text. This is an intermediate drafting step: do not create or save files and do not call composition tools. Final assembly will produce the deliverable.")
	}
	b.WriteString("\n\nOverall goal (context only): " + goal + "\n\nSection: " + s.Label + "\n" + s.Instruction)
	if purpose := strings.TrimSpace(s.Purpose); purpose != "" {
		b.WriteString("\nPurpose: " + purpose)
	}
	if post := strings.TrimSpace(s.Postcondition); post != "" {
		b.WriteString("\nIt is finished when: " + post)
	}
	return b.String()
}

// consumedOutputs are the map entries, `<input>: <statement>`, by which a
// section reads earlier sections' outputs, sorted by input name. An input no
// earlier section produced is the goal's, already in the goal input; one that
// is not a name a map may key on is left to that context too.
func consumedOutputs(inputs []string, producers map[string]string) []string {
	seen := map[string]bool{}
	var names []string
	for _, in := range inputs {
		in = strings.TrimSpace(in)
		if seen[in] || producers[in] == "" || !workDraftArgName.MatchString(in) || workDraftReservedNames[in] {
			continue
		}
		seen[in] = true
		names = append(names, in)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, in := range names {
		out = append(out, in+": "+producers[in])
	}
	return out
}

// carryCatalogSections puts every catalogued section's automation, and the
// rest of its bundle, into the draft -- where the run's template is reloaded
// from on the node that executes it -- with what those bundles reused, for
// sealWorkDraft to capture.
func carryCatalogSections(bundle *authoringBundle, dec sectionableDecision, sections []sectionPlan) {
	seen := map[string]bool{}
	for _, c := range bundle.Constructs {
		seen[c.Kind+"/"+c.Name+"\x00"+c.Source] = true
	}
	edges := map[reuseEdge]bool{}
	for _, e := range bundle.ReuseEdges {
		edges[e] = true
	}
	for _, section := range sections {
		cat := dec.catalogFor(section.Index)
		if cat == nil {
			continue
		}
		for _, c := range cat.Closure {
			if key := c.Kind + "/" + c.Name + "\x00" + c.Source; !seen[key] {
				seen[key] = true
				bundle.Constructs = append(bundle.Constructs, c)
			}
		}
		for _, e := range cat.ReuseEdges {
			if !edges[e] {
				edges[e] = true
				bundle.ReuseEdges = append(bundle.ReuseEdges, e)
			}
		}
	}
}

// workDraftText is the text a work draft's call arguments are written in: the
// run's goal input, the completed sections, and joining text onto them, with
// `+` and toString(). The prompt a call passes writes a map as its JSON, which
// is what the draft's "Goal input (JSON)" heading says.
type workDraftText struct {
	// goalInputStatement binds the goal input as a map, `goalInput :=
	// {day: args.day}`; empty when the goal takes no input.
	goalInputStatement string
	// goalInput is the goal input, as text.
	goalInput string
	// sectionsStatement binds every section's value, keyed by the section's
	// name, as `sections`.
	sectionsStatement string
	// sections is that map, as text.
	sections string
}

// workDraftExpressions writes the goal input and the sections. The goal input
// is the run's variables, which adopt.go binds into the draft's args block
// (inputNames), so it is read back as a map of those args. Each map is bound
// by a statement of its own and passed to toString() by name: a call's one
// argument may not be a map literal. toString(), not the bare value: `+` over
// an absent operand is absent, and toString() reads absent as "".
func workDraftExpressions(inputNames []string, sections []sectionPlan) workDraftText {
	x := workDraftText{goalInput: langparser.QuoteString("{}")}
	if len(inputNames) > 0 {
		entries := make([]string, 0, len(inputNames))
		for _, name := range inputNames {
			if name == "conversation" {
				continue
			}
			entries = append(entries, name+": args."+name)
		}
		x.goalInputStatement = "goalInput := {" + strings.Join(entries, ", ") + "}"
		x.goalInput = "toString(goalInput)"
	}
	names := make([]string, 0, len(sections))
	for _, section := range sections {
		names = append(names, section.Name+": "+section.Name)
	}
	x.sectionsStatement = "sections := {" + strings.Join(names, ", ") + "}"
	x.sections = "toString(sections)"
	return x
}

// workDraftInputNames is the goal input's keys, sorted: the draft declares one
// arg per key, which is how the run's variables reach it (adopt.go binds them
// into the args block). A key a draft cannot declare -- not a name, or one of
// the names every body reserves -- refuses the compile: dropping it would run
// the goal without an input its person gave.
func workDraftInputNames(input map[string]any) ([]string, error) {
	names := make([]string, 0, len(input))
	for key := range input {
		if !workDraftArgName.MatchString(key) || workDraftReservedNames[key] {
			return nil, fmt.Errorf("work compile: goal input key %q cannot be a template argument: an argument is a name (letters, digits and _, starting with a letter) and not one of %s", key, "args, actor, event, now, config, partition, trace")
		}
		names = append(names, key)
	}
	sort.Strings(names)
	return names, nil
}

// workDraftArgName is the shape of a declarable argument name.
var workDraftArgName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// workDraftReservedNames are the roots every body reads by name; an args
// field may not take one.
var workDraftReservedNames = map[string]bool{
	"args": true, "actor": true, "event": true, "now": true, "config": true, "partition": true, "trace": true,
}

// join is text operands joined in order.
func (x workDraftText) join(parts ...string) string {
	return strings.Join(parts, " + ")
}

// A compile draft is durable and bound to one run, never globally activated.
// Promotion still requires the existing dry-run and user approval gates.
func (l *PlannerAgentLoop) persistWorkDraft(ctx context.Context, req CompileRequest, out CompileOutcome, bundle authoringBundle, sandbox authoringSandbox) (CompileOutcome, error) {
	if req.RunId == "" || req.OwnerUserId == "" || sandbox == nil {
		return out, fmt.Errorf("work compile: a validated draft requires its run and owner")
	}
	sealed, err := l.sealWorkDraft(ctx, req.OwnerUserId, bundle)
	if err != nil {
		return out, err
	}
	bundle = sealed
	report := sandbox.CompileBundle(bundle.Constructs)
	if !report.OK {
		return out, fmt.Errorf("work compile: sealed draft did not pass Gate 1: %s", firstFailureHeadline(report))
	}
	var headline memql.SandboxConstruct
	for _, construct := range bundle.Constructs {
		if construct.Kind == "automation" && construct.Name == bundle.AutomationName {
			headline = construct
			break
		}
	}
	if headline.Source == "" {
		return out, fmt.Errorf("work compile: draft contains no headline automation %q", bundle.AutomationName)
	}
	auto, err := automations.NewLoader(automations.LoaderOptions{Logger: l.logger}).CompileSource(headline.Source, "work-compile/"+headline.Name+".memql")
	if err != nil {
		return out, fmt.Errorf("work compile: load validated headline: %w", err)
	}
	bundleId := id.NewShortId()
	write := func(name string, args map[string]any) error {
		_, err := l.engine.Execute(ownerActorContext(ctx, req.OwnerUserId), name+"("+encodeArgs(args)+")")
		return err
	}
	args := map[string]any{"bundleId": bundleId, "title": truncate(req.Statement, 120), "sourceRunId": req.RunId}
	if len(bundle.ReuseEdges) > 0 {
		args["reusedConstructRefs"] = bundle.ReuseEdges
	}
	if err := write("createAuthoringBundle", args); err != nil {
		return out, fmt.Errorf("work compile: persist bundle: %w", err)
	}
	for _, construct := range bundle.Constructs {
		constructId := id.NewShortId()
		if err := write("createAuthoringConstruct", map[string]any{"constructId": constructId, "bundleId": bundleId, "kind": construct.Kind, "name": construct.Name, "targetNamespace": authoredTargetNamespace, "source": construct.Source}); err != nil {
			return out, fmt.Errorf("work compile: persist %s/%s: %w", construct.Kind, construct.Name, err)
		}
		if construct.Kind == "automation" && construct.Name == bundle.AutomationName {
			out.ConstructId = "v1:authoring:construct:" + constructId
		}
	}
	if err := write("recordBundleValidation", map[string]any{"bundleId": bundleId, "status": "validated", "validationReport": structToObject(report)}); err != nil {
		return out, fmt.Errorf("work compile: persist Gate 1: %w", err)
	}
	out.AutomationName = bundle.AutomationName
	out.TemplateFingerprint = auto.DefinitionFingerprint(id.NewUntracked())
	out.TemplateVersion = memql.WorkBundleVersion(bundle.Constructs)
	return out, nil
}
