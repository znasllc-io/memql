package planner

import (
	"context"
	"fmt"
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
	res, err := l.engine.Execute(ownerActorContext(ctx, owner), "query assistantAgentForUser("+encodeArgs(map[string]any{"ownerUserId": owner})+")")
	if err != nil {
		return "", fmt.Errorf("work compile: resolve reasoning agent: %w", err)
	}
	for _, row := range memql.MaterializeRows(res) {
		if getString(row, "id") != "" {
			return getString(row, "id"), nil
		}
	}
	// Core installations seed a planner for every user; an assistant may
	// instead be supplied by a product bundle. Read the persisted planner
	// before using it, so neither a missing seed nor another owner's agent
	// can turn into a synthetic successful dispatch.
	plannerId := "plannerAgent-" + memql.BareShortId(owner)
	res, err = l.engine.Execute(ownerActorContext(ctx, owner), "query agentById("+encodeArgs(map[string]any{"agentId": plannerId})+")")
	if err != nil {
		return "", fmt.Errorf("work compile: resolve seeded reasoning agent: %w", err)
	}
	for _, row := range memql.MaterializeRows(res) {
		if memql.BareShortId(getString(row, "id")) == plannerId && memql.BareShortId(getString(row, "ownerUserId")) == memql.BareShortId(owner) && row["active"] == true && row["deleted"] != true && row["roleSlug"] == "system-planner" {
			return getString(row, "id"), nil
		}
	}
	return "", fmt.Errorf("work compile: no active assistant or seeded planner exists for this goal's owner")
}

// File delivery uses the known Materializer capability directly. Other reasoning
// turns remain synchronous agent calls, so every output belongs to this run.
func synthesizeWorkReasoningBundle(req CompileRequest, agentId string, dec sectionableDecision) (authoringBundle, error) {
	if dec.RequiresFile == nil {
		return authoringBundle{}, fmt.Errorf("work compile: triage must explicitly answer requiresFile with a boolean before dispatching a reasoning draft")
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
	headline := "workRun_" + sanitizeIdent(req.RunId)
	goal := req.Statement
	sections := dec.usableSections(headline)
	fanout := dec.Sectionable && len(sections) >= minSectionsForFanout
	if fanout && len(dec.Sections) > maxSectionFanout {
		return authoringBundle{}, fmt.Errorf("work compile: %d sections exceeds the %d-section execution limit", len(dec.Sections), maxSectionFanout)
	}
	// Runtime variables keep fork overrides in the work instruction while
	// the validated semantic output choices remain in the stored template.
	const inputHeading = "\n\nGoal input (JSON):\n"
	x := workDraftExpressions()
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
	if !nativeFile || fanout {
		b.WriteString("use agents.builtins.{ runAgentTurn }\n")
	}
	fmt.Fprintf(&b, "\n@template\nautomation %s {\n", headline)
	if !fanout {
		fmt.Fprintf(&b, "  step reason {\n    %s\n  }\n", delivery(goal, ""))
	} else {
		b.WriteString("  step sections {\n    parallel {\n      wait: \"all\"\n      failFast: true\n      branches: [\n")
		for n, section := range sections {
			prompt := "Produce only the independent section below and return its complete content as text. This is an intermediate drafting step: do not create or save files and do not call composition tools. Final assembly will produce the deliverable.\n\nOverall goal (context only): " + goal + "\n\nSection: " + section.Spec.Label + "\n" + section.Spec.Instruction
			fmt.Fprintf(&b, "        step %s { builtin runAgentTurn(agentId: %s, prompt: %s) }", section.Name, langparser.QuoteString(agentId), x.join(langparser.QuoteString(prompt+inputHeading), x.goalInput))
			if n < len(sections)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString("      ]\n    }\n  }\n")
		assembly := goal + "\n\nAssemble the completed independent sections into the requested deliverable. Verify completeness. " + dec.Assembly
		fmt.Fprintf(&b, "  step assemble {\n    if steps.sections.status == \"success\" {\n      %s\n    }\n  }\n", delivery(assembly, x.sectionsText))
	}
	b.WriteString("}\n")
	return authoringBundle{AutomationName: headline, Constructs: []memql.SandboxConstruct{{Kind: "automation", Name: headline, Source: b.String()}}}, nil
}

// workDraftText is the expression text a work draft's step arguments are
// written in: the run's goal input, the completed sections, and joining text
// onto them, in edition 2026's `+` and toString(). The prompt a step passes
// writes a map or a list as its JSON, which is what the draft's "Goal input
// (JSON)" heading says.
type workDraftText struct {
	// goalInput is the run's goal input, as text.
	goalInput string
	// sections is the parallel section step's result, as a join operand.
	sections string
	// sectionsText is that result as a standalone text argument.
	sectionsText string
}

func workDraftExpressions() workDraftText {
	// The goal input is the run's trigger payload (adopt.go). The draft
	// declares no args block, so it reads it as `payload`, the envelope's key
	// read bare -- a dotted `event.payload` is refused at load (G5).
	// toString(), not the bare value: `+` over an absent operand is absent,
	// and toString() reads absent as "".
	return workDraftText{goalInput: "toString(payload)", sections: "toString(steps.sections.result)", sectionsText: "toString(steps.sections.result)"}
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
