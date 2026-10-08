package planner

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/automations"
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
		if err != nil {
			return &draftPersistenceError{err: err}
		}
		return nil
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
