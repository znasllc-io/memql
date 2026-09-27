package planner

// work_compile_sections.go -- decomposition at compile: the boundary rule, and
// the catalog asked for every section before intelligence (epic memql#5414,
// design D24).
//
// Triage cuts a sectionable goal into ordered sections, each with a purpose,
// inputs, outputs, a reuse intent, the effects it has and how its end is
// checked. Two pure decisions in component/work then say what happens to it:
//
//	CheckSectionBoundaries  a section that ends mid-effect -- or has no end at
//	                        all -- refuses the WHOLE decomposition, and the goal
//	                        is authored as one piece instead
//	DecideSections          per section: an exact catalog hit on its signature,
//	                        else a near hit among REUSABLE automations only,
//	                        else intelligence
//
// This file obeys them. What it adds is only what a pure decision cannot know:
// the rows (read under the goal's owner, as every catalog read is), the
// arguments a catalogued automation declares, whether each of a served
// section's inputs can actually be bound in the draft, and the construct
// closure the draft must carry for the call to run on the node that executes
// it.
//
// A SECTION IS SERVED BY CALLING THE AUTOMATION, `automation <name>(...)`, and
// the automation's source -- with the rest of its bundle -- travels INSIDE the
// run's draft. A run's template is reloaded on the executing node from its own
// bundle rows (app/work_template.go) and a sub-automation resolves against
// those members first, so a catalogued automation that was not carried in
// would be a call to nothing on every replica.
//
// A LEARNED PROCEDURE IS NOT A SECTION. It is served only through
// replayLearnedProcedure, whose runner reads the RUN's goal -- its statement
// and its whole input -- and, on a divergence, hands the whole goal back to
// the app: served as one statement of a larger draft it would replay against
// the wrong input and hand back work the section never owned. And its rendered
// source is never runnable (every step is procedureStep, which refuses). So
// the section tiers ask the authored catalog only; a procedure keeps serving
// whole goals through the exact tier.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/automations"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// maxCatalogPages bounds the owner-catalog walk that builds the near set.
const maxCatalogPages = 8

// catalogSection is a section the catalog serves: the automation the draft
// calls, the arguments it binds, and what the draft must carry for the call to
// resolve where the run executes.
type catalogSection struct {
	ConstructId string
	// Automation is the callee's name, which is also its statement's call.
	Automation string
	// Args are the call's arguments, sorted by name: each is a goal input
	// (`args.<name>`) or an earlier section's value (its statement name).
	Args []boundArg
	// Closure is the callee and the rest of its bundle.
	Closure []memql.SandboxConstruct
	// ReuseEdges are what the callee's own bundle reused from the catalog,
	// for sealWorkDraft to capture as it does for any composed dependency.
	ReuseEdges []reuseEdge
}

// boundArg is one argument of a catalog section's call.
type boundArg struct {
	Name string
	Expr string
}

// callText is the section's statement right-hand side.
func (c *catalogSection) callText() string {
	parts := make([]string, 0, len(c.Args))
	for _, a := range c.Args {
		parts = append(parts, a.Name+": "+a.Expr)
	}
	return "automation " + c.Automation + "(" + strings.Join(parts, ", ") + ")"
}

// workDraftHeadline is the name of a run's draft automation.
func workDraftHeadline(req CompileRequest) string {
	return "workRun_" + sanitizeIdent(req.RunId)
}

// fansOut reports whether the draft is cut into sections at all.
func (d sectionableDecision) fansOut(headline string) bool {
	return d.Sectionable && len(d.usableSections(headline)) >= minSectionsForFanout
}

// catalogFor is the catalog section at a section's index, or nil.
func (d sectionableDecision) catalogFor(index int) *catalogSection {
	if d.catalog == nil {
		return nil
	}
	return d.catalog[index]
}

// anyIntelligence reports whether any usable section is planned live -- the
// only case in which the draft needs a model beyond its delivery.
func (d sectionableDecision) anyIntelligence(headline string) bool {
	for _, sp := range d.usableSections(headline) {
		if d.catalogFor(sp.Index) == nil {
			return true
		}
	}
	return false
}

// withoutCatalog is the decision with every section planned live.
func (d sectionableDecision) withoutCatalog() sectionableDecision {
	d.catalog = nil
	return d
}

// workSection maps one usable section onto component/work's decision shape.
// Its Name is the label a person reads in a refusal; the statement name is
// the draft's business.
func workSection(sp sectionPlan) work.Section {
	s := sp.Spec
	name := strings.TrimSpace(s.Label)
	if name == "" {
		name = sp.Name
	}
	effects := footprintOf(s.Effects)
	outputs := append([]string(nil), s.Outputs...)
	// A SECTION THAT CHANGES NOTHING ENDS ON ITS OWN TEXT. Every section runs
	// as a turn that answers, and a section with no effects has no other end:
	// what it produced IS its output, bound under its statement name. So a
	// triage answer that names no outputs -- the {label, instruction} shape
	// every sectionable goal had before decomposition, and what a smaller model
	// still writes -- keeps fanning out instead of being refused as endless and
	// sent to the far dearer author route. The boundary rule then refuses only
	// what it exists for: a section with an EFFECT and no postcondition.
	if len(nonEmptyNames(outputs)) == 0 && !effects.IsSideEffect() {
		outputs = []string{sp.Name}
	}
	return work.Section{
		Name:          name,
		Label:         s.Label,
		Instruction:   s.Instruction,
		Purpose:       strings.TrimSpace(s.Purpose),
		Inputs:        append([]string(nil), s.Inputs...),
		Outputs:       outputs,
		ReuseIntent:   s.ReuseIntent,
		Effects:       effects,
		Postcondition: strings.TrimSpace(s.Postcondition),
	}
}

// nonEmptyNames is names with the blanks dropped.
func nonEmptyNames(names []string) []string {
	var out []string
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// footprintOf reads a section's declared effects. An effect this build does
// not recognise is read as reaching OUTSIDE: the boundary rule runs on "does
// it change anything", and reading an unknown word as "nothing" is the
// direction in which a section ending mid-effect passes. "none" is the one
// spelling of nothing besides an empty list.
func footprintOf(effects []string) work.Footprint {
	var f work.Footprint
	seen := map[string]bool{}
	for _, e := range effects {
		e = strings.TrimSpace(e)
		switch lower := strings.ToLower(e); {
		case lower == "" || lower == "none":
		case lower == "files" || lower == "file":
			f.Files = true
		case lower == "machine":
			f.Machine = true
		case lower == "external":
			f.External = true
		case lower == "spend":
			f.Spend = true
		case strings.HasPrefix(lower, "concept:") && strings.TrimSpace(e[len("concept:"):]) != "":
			if id := strings.TrimSpace(e[len("concept:"):]); !seen[id] {
				seen[id] = true
				f.Concepts = append(f.Concepts, id)
			}
		default:
			f.External = true
		}
	}
	sort.Strings(f.Concepts)
	return f
}

// decideDecomposition applies the boundary rule and the catalog to triage's
// decomposition, for a route that would use one. A refused decomposition
// sends the goal to the author route with the reason on the outcome; an
// accepted one comes back with the sections the catalog serves.
func (l *PlannerAgentLoop) decideDecomposition(ctx context.Context, req CompileRequest, d work.Decision, dec sectionableDecision, out *CompileOutcome) (work.Decision, sectionableDecision) {
	headline := workDraftHeadline(req)
	if (d.Route != work.RouteSectionable && d.Route != work.RouteTrivial) || !dec.fansOut(headline) {
		return d, dec
	}
	usable := dec.usableSections(headline)
	sections := make([]work.Section, len(usable))
	for n, sp := range usable {
		sections[n] = workSection(sp)
	}
	if err := work.CheckSectionBoundaries(sections); err != nil {
		out.DecompositionRefused = err.Error()
		l.infoCompile("work compile: the decomposition was refused; the goal is authored whole", req, "reason", err.Error())
		dec.Sectionable, dec.Sections = false, nil
		return work.Decision{Route: work.RouteAuthor, NeedsModel: true}, dec
	}
	plan, rows := l.routeSections(ctx, req, sections)
	dec.catalog = l.bindCatalogSections(ctx, req, usable, &plan, rows)
	out.Sections = plan.Sections
	return d, dec
}

// routeSections asks the catalog for every section: the exact tier per
// section signature, the near tier over the owner's REUSABLE automations.
// A failed read is a miss, never a failed compile -- it makes the section
// live, which costs a model, and nothing worse.
func (l *PlannerAgentLoop) routeSections(ctx context.Context, req CompileRequest, sections []work.Section) (work.SectionPlan, map[string]map[string]any) {
	rows := map[string]map[string]any{}
	exact := map[string][]work.SectionCandidate{}
	for _, s := range sections {
		sig := work.SectionSignature(s)
		if _, asked := exact[sig]; asked {
			continue
		}
		found, err := l.sectionReads(ctx, req.OwnerUserId, "cataloguedConstructsForGoalSignature", map[string]any{"goalSignature": sig})
		if err != nil {
			l.warnCompile("work compile: a section's exact catalog read failed; the section is planned live", req, err)
		}
		var cands []work.SectionCandidate
		for _, r := range found {
			if getString(r, "goalSignature") != sig {
				continue
			}
			if c, ok := l.sectionCandidate(r, false); ok {
				c.Signature = sig
				cands = append(cands, c)
				rows[c.ConstructId] = r
			}
		}
		sort.SliceStable(cands, func(i, j int) bool {
			return reliabilityOf(found, cands[i].ConstructId) > reliabilityOf(found, cands[j].ConstructId)
		})
		exact[sig] = cands
	}
	owned, err := l.sectionReads(ctx, req.OwnerUserId, "cataloguedConstructsForOwner", nil)
	if err != nil {
		l.warnCompile("work compile: the owner's catalog read failed; no section has a near tier", req, err)
	}
	var reusable []work.SectionCandidate
	for _, r := range owned {
		c, ok := l.sectionCandidate(r, true)
		if !ok {
			continue
		}
		reusable = append(reusable, c)
		rows[c.ConstructId] = r
	}
	return work.DecideSections(sections, exact, reusable, work.DefaultSectionNearThreshold), rows
}

// sectionCandidate reads a catalogued construct as a section candidate: an
// ACTIVE automation whose source compiles, with the arguments it declares.
// Anything else cannot be a section's statement. onlyReusable keeps just the
// constructs whose EFFECTIVE label is reusable -- the near tier's set -- and
// is checked before the source is compiled, so walking a large catalog costs
// a compile only for what could be served.
func (l *PlannerAgentLoop) sectionCandidate(r map[string]any, onlyReusable bool) (work.SectionCandidate, bool) {
	if getString(r, "kind") != "automation" || getString(r, "status") != "active" || getString(r, "id") == "" {
		return work.SectionCandidate{}, false
	}
	reuse := work.EffectiveReuse(work.ParseReuseLabel(getString(r, "reuse")),
		work.ParseReuseLabel(getString(mapField(r, "reuseOverride"), "label")))
	if onlyReusable && reuse != work.ReuseReusable {
		return work.SectionCandidate{}, false
	}
	auto, err := l.compileCatalogued(r)
	if err != nil {
		return work.SectionCandidate{}, false
	}
	var args []string
	if auto.Args != nil {
		for _, f := range auto.Args.Fields {
			args = append(args, f.Name)
		}
	}
	return work.SectionCandidate{
		ConstructId: getString(r, "id"),
		Name:        getString(r, "name"),
		Reuse:       string(reuse),
		Text:        candidateText(r),
		Args:        args,
		Rung:        work.RungNone,
	}, true
}

// compileCatalogued compiles a catalogued automation's stored source.
func (l *PlannerAgentLoop) compileCatalogued(r map[string]any) (*automations.Automation, error) {
	source := getString(r, "source")
	if strings.TrimSpace(source) == "" {
		return nil, fmt.Errorf("construct %s has no source", getString(r, "id"))
	}
	auto, err := automations.NewLoader(automations.LoaderOptions{Logger: l.logger}).CompileSource(source, "work-compile/section/"+getString(r, "name")+".memql")
	if err != nil {
		return nil, err
	}
	if auto.Name != getString(r, "name") {
		return nil, fmt.Errorf("construct %s declares %q, not its own name", getString(r, "id"), auto.Name)
	}
	return auto, nil
}

// candidateText is the construct's own words the near tier compares with a
// section's purpose: the intent the catalog embedded (the `intent:` part of
// catalogMatchText, which is its doc comment), else the source's doc comment,
// else its name broken into words. Never the whole match text: its canonical
// body is dozens of words, and a lexical overlap diluted by them would never
// reach the threshold for anything.
func candidateText(r map[string]any) string {
	if text := getString(r, "catalogMatchText"); text != "" {
		if _, rest, ok := strings.Cut(text, "intent:"); ok {
			intent, _, _ := strings.Cut(rest, " form:")
			if intent = strings.TrimSpace(intent); intent != "" {
				return intent
			}
		}
	}
	if doc := strings.TrimSpace(langparser.LeadingDocComment(getString(r, "source"))); doc != "" {
		return doc
	}
	return nameWords(getString(r, "name"))
}

// nameWords breaks a lowerCamel or snake name into words.
func nameWords(name string) string {
	var b strings.Builder
	for i, r := range name {
		switch {
		case r == '_' || r == '-':
			b.WriteByte(' ')
		case i > 0 && r >= 'A' && r <= 'Z':
			b.WriteByte(' ')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// bindCatalogSections turns every catalog route into a call the draft can
// make, and demotes to intelligence a section whose call cannot be made: one
// of its inputs -- or an argument the automation REQUIRES -- is neither a
// goal input nor an earlier section's output, or the automation's bundle
// cannot travel in a run's draft. The plan is updated in place, so the
// outcome reports the routes the draft actually has.
func (l *PlannerAgentLoop) bindCatalogSections(ctx context.Context, req CompileRequest, usable []sectionPlan, plan *work.SectionPlan, rows map[string]map[string]any) map[int]*catalogSection {
	goalInputs := map[string]bool{}
	if names, err := workDraftInputNames(req.Input); err == nil {
		for _, n := range names {
			goalInputs[n] = true
		}
	}
	producers := map[string]string{}
	served := map[int]*catalogSection{}
	for n := range plan.Sections {
		d := &plan.Sections[n]
		sp := usable[n]
		if d.Candidate != nil {
			cat, why := l.catalogCall(ctx, req, *d.Candidate, rows[d.Candidate.ConstructId], d.Section.Inputs, goalInputs, producers)
			if cat != nil {
				served[sp.Index] = cat
			} else {
				l.infoCompile("work compile: a catalogued section is planned live instead", req,
					"section", d.Section.Name, "construct", d.Candidate.Name, "reason", why)
				d.Route, d.Candidate, d.Similarity = work.SectionIntelligence, nil, 0
			}
		}
		for _, o := range sp.Spec.Outputs {
			producers[o] = sp.Name
		}
	}
	plan.NeedsModel = false
	for _, d := range plan.Sections {
		if d.Route == work.SectionIntelligence {
			plan.NeedsModel = true
		}
	}
	if len(served) == 0 {
		return nil
	}
	return served
}

// catalogCall builds one catalog section's call, or says why it cannot.
func (l *PlannerAgentLoop) catalogCall(ctx context.Context, req CompileRequest, c work.SectionCandidate, row map[string]any, inputs []string, goalInputs map[string]bool, producers map[string]string) (*catalogSection, string) {
	if row == nil {
		return nil, "its row was not read"
	}
	auto, err := l.compileCatalogued(row)
	if err != nil {
		return nil, "its source does not compile: " + err.Error()
	}
	names := map[string]bool{}
	for _, in := range inputs {
		names[strings.TrimSpace(in)] = true
	}
	if auto.Args != nil {
		for _, f := range auto.Args.Fields {
			if !f.Optional {
				names[f.Name] = true
			}
		}
	}
	cat := &catalogSection{ConstructId: c.ConstructId, Automation: c.Name}
	for _, name := range sortedNames(names) {
		switch {
		case !workDraftArgName.MatchString(name) || workDraftReservedNames[name]:
			return nil, fmt.Sprintf("its input %q is not a name a draft can bind", name)
		case producers[name] != "":
			cat.Args = append(cat.Args, boundArg{Name: name, Expr: producers[name]})
		case goalInputs[name]:
			cat.Args = append(cat.Args, boundArg{Name: name, Expr: "args." + name})
		default:
			return nil, fmt.Sprintf("nothing binds its input %q: it is neither a goal input nor an earlier section's output", name)
		}
	}
	closure, edges, why := l.catalogClosure(ctx, req, row)
	if why != "" {
		return nil, why
	}
	cat.Closure, cat.ReuseEdges = closure, edges
	return cat, ""
}

// catalogClosure reads the callee's bundle: every construct a run's draft can
// carry, and what that bundle itself reused. A bundle that holds anything a
// draft cannot carry (a concept, a prompt, a retired member) cannot travel.
func (l *PlannerAgentLoop) catalogClosure(ctx context.Context, req CompileRequest, row map[string]any) ([]memql.SandboxConstruct, []reuseEdge, string) {
	bundleId := getString(row, "bundleId")
	if bundleId == "" {
		return nil, nil, "it names no bundle"
	}
	members, err := l.sectionReads(ctx, req.OwnerUserId, "authoringConstructsForBundle", map[string]any{"bundleId": bundleId})
	if err != nil {
		return nil, nil, "its bundle is not readable: " + err.Error()
	}
	var closure []memql.SandboxConstruct
	found := false
	for _, m := range members {
		kind, name, source := getString(m, "kind"), getString(m, "name"), getString(m, "source")
		if getString(m, "status") != "active" || strings.TrimSpace(source) == "" || name == "" {
			return nil, nil, fmt.Sprintf("its bundle member %s is not active", name)
		}
		switch kind {
		case "automation", "query", "mutation", "logic", "spec", "trait", "shape":
		default:
			return nil, nil, fmt.Sprintf("its bundle carries a %s, which a run's draft cannot", kind)
		}
		if kind == "automation" && name == getString(row, "name") {
			found = true
		}
		closure = append(closure, memql.SandboxConstruct{Kind: kind, Name: name, Source: source})
	}
	if !found {
		return nil, nil, "its bundle does not hold it"
	}
	bundles, err := l.sectionReads(ctx, req.OwnerUserId, "authoringBundleById", map[string]any{"bundleId": bundleId})
	if err != nil || len(bundles) != 1 {
		return nil, nil, "its bundle row is not readable"
	}
	var edges []reuseEdge
	if refs, ok := bundles[0]["reusedConstructRefs"].([]any); ok {
		for _, ref := range refs {
			m, _ := ref.(map[string]any)
			edges = append(edges, reuseEdge{Name: getString(m, "name"), Kind: getString(m, "kind"), Namespace: getString(m, "namespace")})
		}
	}
	return closure, edges, ""
}

// sectionReads runs one owner-scoped read and, for a paginated one, walks its
// pages up to a bound. The read renders through RenderCall, so every value is
// a MemQL literal.
func (l *PlannerAgentLoop) sectionReads(ctx context.Context, owner, name string, args map[string]any) ([]map[string]any, error) {
	if l.engine == nil || strings.TrimSpace(owner) == "" {
		return nil, nil
	}
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		return nil, err
	}
	ctx = ownerActorContext(ctx, owner)
	var out []map[string]any
	cursor := ""
	for page := 0; page < maxCatalogPages; page++ {
		pageCtx := ctx
		if cursor != "" {
			pageCtx = memql.ContextWithCursor(ctx, cursor)
		}
		res, err := l.engine.Execute(pageCtx, "query "+call)
		if err != nil {
			return out, err
		}
		rows := memql.MaterializeRows(res)
		out = append(out, rows...)
		next := ""
		if er, ok := res.(*memql.ExecuteResult); ok && er.GetMeta() != nil {
			next = strings.TrimSpace(er.GetMeta().Cursor)
		}
		if next == "" || next == cursor || len(rows) == 0 {
			break
		}
		cursor = next
	}
	return out, nil
}

func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		if k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// infoCompile logs one of compile's decisions, nil-safe like warnCompile.
func (l *PlannerAgentLoop) infoCompile(msg string, req CompileRequest, kv ...any) {
	if l.logger == nil {
		return
	}
	l.logger.Info(msg, append([]any{"goalId", req.GoalId, "runId", req.RunId}, kv...)...)
}

// needsReasoningAgent reports whether the draft calls the owner's reasoning
// agent: a single answer turn, or a section planned live. A navigation, a
// file the Materializer delivers whole and a decomposition the catalog serves
// entirely call none -- and resolving an agent nobody calls would fail a goal
// on a missing seed it never needed.
func (d sectionableDecision) needsReasoningAgent(headline string) bool {
	nativeFile := d.RequiresFile != nil && *d.RequiresFile
	if d.Navigation != nil && !nativeFile && !d.Sectionable && strings.TrimSpace(d.Navigation.App) != "" {
		return false
	}
	if d.fansOut(headline) {
		return d.anyIntelligence(headline)
	}
	return !nativeFile
}
