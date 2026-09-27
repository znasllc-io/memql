package planner

// work_compile.go -- the work spine's compile pass (epic memql#4966,
// design record docs/superpowers/specs/2026-09-05-work-spine-design.md,
// section B "Compile").
//
// WHY IT LIVES HERE rather than in component/work. The order it obeys is
// pure and lives there (work.Decide); what it needs to DO the work with
// is the authoring pipeline -- runDesignPass, emitAndRepairBundle,
// classifySectionable, maybeGenerateSectionable, loadCatalog -- and every
// one of those is an unexported method on *PlannerAgentLoop. Exporting
// five of them to move one caller would widen a surface for no gain, so
// the caller moved instead.
//
// THE ORDER IS THE PRODUCT. Catalog exact match, then near match with a
// gap list, then ONE triage call. An exact hit reaches no model AT ALL --
// not even the cheap classifier -- and that is the spec's headline claim,
// the reason the catalog is worth keeping, and the thing that gets
// quietly broken by an innocent-looking refactor. CompileGoalForRun
// therefore reports what it did in CompileOutcome.Route and how many
// provider calls it made in CompileOutcome.ModelCalls, and the test
// asserts BOTH: a route with no calls, proved by a counting provider.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// CompileRequest is one goal to compile.
type CompileRequest struct {
	// GoalId is the v1:work:goal being compiled.
	GoalId string
	// RunId is the run opened for it, already in `compiling`.
	RunId string
	// OwnerUserId is whose goal it is. Every catalog read runs under this
	// person's actor: the catalog is owner-scoped, and reading it under
	// anyone else answers zero rows and no error.
	OwnerUserId string
	// Statement is the goal in the person's words.
	Statement string
	// MaxModelCalls is the run's ceiling on model calls, inherited from its
	// goal. ZERO IS UNSET, never "nothing allowed" -- the same reading
	// component/work.CheckCeilings gives every ceiling, and the one that
	// keeps a goal with no ceilings declared runnable rather than dead on
	// arrival.
	MaxModelCalls int
	// Input is the typed input object; its KEYS are what the signature
	// is computed over, because two goals worded alike but taking
	// different arguments want different templates.
	Input map[string]any
}

// CompileOutcome is what compile decided and what it cost.
type CompileOutcome struct {
	// Route is the tier that answered.
	Route work.Route
	// ConstructId identifies the reused catalog template or the stored run draft.
	ConstructId string
	// TemplateFingerprint binds the stored headline to the run execution contract.
	TemplateFingerprint string
	// TemplateVersion seals all authored dependency source for execution.
	TemplateVersion string
	// AutomationName is the template to run.
	AutomationName string
	// Gaps are the arguments a near match must close.
	Gaps []string
	// Signature is the goal's catalog key, recorded on the construct
	// after a successful run so the next goal like it is an exact hit.
	Signature string
	// ModelCalls is how many provider calls compile made. Zero on an
	// exact catalog hit, and asserted as zero by the headline test.
	ModelCalls int
	// Variables are what the chosen template must run with BESIDES the
	// goal's own input. Today that is only a learned procedure's construct
	// id: replayLearnedProcedure is ONE embedded automation that serves every
	// procedure on the ladder, so it has to be told which (epic memql#5408).
	// WorkCompiler.Compile merges them over the goal's input.
	Variables map[string]any
	// DecompositionRefused is why triage's decomposition was refused, as the
	// boundary rule words it ("decomposition_refused: section ... ends
	// mid-effect ..."), when it was; the goal was then authored whole (epic
	// memql#5414, D24). Empty otherwise.
	DecompositionRefused string
	// Sections is how a decomposed goal's sections were routed -- served by a
	// catalogued automation, or planned live -- as the draft has them. Empty
	// for a goal that was not decomposed.
	Sections []work.SectionDecision
	// LiveSections is how the draft wrote each section planned live, in its
	// order: as an automation of its own, which a succeeded run catalogues
	// for the next goal (D24), or inline in the template, with why
	// (work_compile_section_automations.go). Empty for a goal that was not
	// decomposed.
	LiveSections []LiveSection
}

// replayProcedureAutomation is the embedded template a learned procedure is
// served through (dsl/procedure/automations.memql). A procedure is NEVER run
// as its own authored automation: it is served only through the ladder, and
// the runner behind this automation re-reads the rung at execution, so a
// procedure demoted between compile and dispatch falls back to the app
// instead of running on a decision that is no longer true.
const replayProcedureAutomation = "replayLearnedProcedure"

// catalogReader is the narrow seam compile needs for its exact tier. The
// near tier already has one (authoringNearMatcher), and this is its
// sibling rather than a widening of it, because the two answer different
// questions: "is there a template for exactly this goal" is a filter the
// database can push down, and "what is close to this text" is a vector
// search.
type catalogReader interface {
	Execute(ctx context.Context, query string) (any, error)
}

// CompileGoalForRun runs the compile order for one goal.
//
// near and sandbox may be nil: a build with no similarity provider has no
// near tier, and a build whose sandbox is unavailable cannot author. Both
// degrade to the next tier rather than failing, which is the behaviour
// the authoring pipeline already has -- an author path that refuses
// because a nice-to-have is missing would make the whole spine unusable
// on a cluster with no vector index.
func (l *PlannerAgentLoop) CompileGoalForRun(ctx context.Context, req CompileRequest, near authoringNearMatcher, sandbox authoringSandbox) (CompileOutcome, error) {
	if strings.TrimSpace(req.Statement) == "" {
		return CompileOutcome{}, fmt.Errorf("work compile: goal %s has an empty statement", req.GoalId)
	}
	keys := inputKeys(req.Input)
	sig := work.GoalSignature(req.Statement, keys)
	out := CompileOutcome{Signature: sig}

	in := work.CompileInput{
		Statement:     req.Statement,
		InputKeys:     keys,
		NearThreshold: nearMatchThreshold,
	}

	// Tier 1: exact, pushed down as a filter. Free.
	exact, err := l.cataloguedForSignature(ctx, req.OwnerUserId, sig)
	if err != nil {
		// A catalog read that fails must not make the goal unrunnable --
		// it makes it EXPENSIVE, which is a different and recoverable
		// problem. Logged and treated as a miss.
		l.warnCompile("work compile: exact catalog read failed; falling through to the paid tiers", req, err)
	}
	// The ladder's half of the same tier (epic memql#5408): learned
	// procedures on a rung DecideServe serves from, ranked ahead of the
	// authored catalog. A failed read is a miss for the same reason as above.
	procedures, perr := l.servableProceduresForSignature(ctx, req.OwnerUserId, sig)
	if perr != nil {
		l.warnCompile("work compile: learned-procedure read failed; the ladder is skipped for this goal", req, perr)
	}
	in.Exact = append(procedures, exact...)

	// Tier 2: near. Only consulted when the exact tier missed, because
	// building the candidate list costs a vector search.
	if len(in.Exact) == 0 && near != nil {
		matchText := work.NormalizeStatement(req.Statement)
		if candidates, nerr := near.CatalogNearMatches(ctx, matchText, maxNearMatchCandidates); nerr != nil {
			l.warnCompile("work compile: near-match read failed; falling through to triage", req, nerr)
		} else {
			in.Near = nearCandidates(candidates, keys)
		}
	}

	// Decide with what we have. A decision that needs triage is the ONLY
	// way a model is reached before the author tier.
	d := work.Decide(in)
	if !d.NeedsTriage {
		return l.finishCompile(ctx, req, d, out, sandbox, sectionableDecision{}, nil)
	}

	// DESCRIPTION GUIDANCE (epic memql#5414, D23), read HERE and nowhere
	// earlier: this is the first point at which a model is genuinely about to
	// be used for the goal. An exact catalog hit and a procedure serve never
	// get this far, and a replay never compiles at all -- text is not a row a
	// replay can act on. The same guidance reaches the design pass if the
	// goal is authored.
	guidance := l.descriptionGuidance(ctx, req, sig)

	// Tier 3: ONE classifier call answering complexity AND sectionability.
	complexity, _, sectionable, cerr := l.classifySectionable(ctx, req.Statement, time.Now().UTC().Format(time.RFC3339), guidance, inputKeys(req.Input))
	if cerr == nil || !memql.IsProviderUnavailable(cerr) {
		// Counted when the call reached a provider. A cluster with no
		// classifier made no call, and ModelCalls counts only calls that ran.
		out.ModelCalls++
	}
	if cerr != nil {
		if memql.IsProviderUnavailable(cerr) {
			// No classifier on this cluster. Authoring is the honest
			// fallback: refusing here would make every uncatalogued goal
			// unrunnable on a cluster with no cheap model.
			l.warnCompile("work compile: no triage provider; authoring directly", req, cerr)
		} else {
			l.warnCompile("work compile: triage failed; authoring directly", req, cerr)
		}
		in.Complexity = string(complexityComplex)
	} else {
		in.Complexity = string(complexity)
		in.Sectionable = sectionable.Sectionable
	}
	if in.Complexity == "" {
		// complexityUnknown is the zero value and is deliberately NOT
		// trivial: an unclassified goal takes the careful path.
		in.Complexity = string(complexityComplex)
	}

	d = work.Decide(in)
	// DECOMPOSITION (epic memql#5414, D24): a decomposition that breaks the
	// boundary rule sends the goal to the author route; one that holds asks
	// the catalog for every section before any is planned live. Neither
	// reaches a model.
	d, sectionable = l.decideDecomposition(ctx, req, d, sectionable, &out)
	return l.finishCompile(ctx, req, d, out, sandbox, sectionable, guidance)
}

// finishCompile carries out whichever route was decided. guidance is the
// goal's description guidance, read before triage; nil when no model has
// been asked about the goal.
func (l *PlannerAgentLoop) finishCompile(ctx context.Context, req CompileRequest, d work.Decision, out CompileOutcome, sandbox authoringSandbox, sectionable sectionableDecision, guidance []map[string]any) (CompileOutcome, error) {
	out.Route = d.Route
	if d.Candidate != nil {
		out.ConstructId = d.Candidate.ConstructId
		out.AutomationName = d.Candidate.Name
	}
	out.Gaps = d.Gaps

	switch d.Route {
	case work.RouteCatalogExact, work.RouteCatalogNear:
		if d.Route == work.RouteCatalogExact && d.Candidate != nil && d.Candidate.Rung != work.RungNone {
			// A LEARNED PROCEDURE on a servable rung. It is served through
			// the one embedded replay template, never as its own construct:
			// no templateConstructId (the template loader would try to run
			// the procedure's bundle, which was validated for another run),
			// and the construct id rides the run's variables instead.
			out.AutomationName = replayProcedureAutomation
			out.ConstructId = ""
			out.Variables = map[string]any{"procedureConstructId": d.Candidate.ConstructId}
			return out, nil
		}
		// The template is the catalogue's. Nothing more to author; the
		// near route's gap list is closed by the run's own reasoning
		// steps rather than by a second compile pass.
		return out, nil
	case work.RouteSectionable, work.RouteTrivial:
		if sandbox == nil {
			return out, fmt.Errorf("work compile: goal %s needs a runnable draft and no Gate 1 sandbox is available", req.GoalId)
		}
		persisted, err := l.reasoningDraft(ctx, req, out, sandbox, sectionable)
		if err != nil && sectionable.catalog != nil {
			// A CATALOGUED SECTION THAT CANNOT TRAVEL IS PLANNED LIVE, never a
			// failed goal. The draft carries every served automation's bundle,
			// and Gate 1 or the dependency seal can refuse the combination --
			// two bundles naming one construct differently, a member that no
			// longer compiles. Nothing was written (persistWorkDraft writes
			// only after both), so the goal is drafted again with every
			// section live, which costs a model per section rather than the
			// goal.
			l.warnCompile("work compile: a catalogued section could not be carried into the draft; every section is planned live", req, err)
			sectionable = sectionable.withoutCatalog()
			for n := range out.Sections {
				out.Sections[n].Route, out.Sections[n].Candidate, out.Sections[n].Similarity = work.SectionIntelligence, nil, 0
			}
			persisted, err = l.reasoningDraft(ctx, req, out, sandbox, sectionable)
		}
		if err != nil && sectionable.cutsSectionAutomations(req) {
			// A SECTION AUTOMATION THAT DOES NOT PERSIST IS WRITTEN INLINE,
			// never a failed goal -- the same answer the catalog fallback
			// gives, one level down. The draft is written again with every
			// live section an agent turn of the template, as it was before
			// sections became automations, and the outcome says why for each.
			l.warnCompile("work compile: the draft with its live sections as automations did not persist; every live section is written inline", req, err)
			sectionable.inlineAll = "the draft that wrote it as an automation of its own did not persist: " + err.Error()
			persisted, err = l.reasoningDraft(ctx, req, out, sandbox, sectionable)
		}
		return persisted, err
	case work.RouteAuthor:
		if sandbox == nil {
			return out, fmt.Errorf("work compile: goal %s needs authoring and no sandbox is available; a draft that cannot pass Gate 1 must not be run", req.GoalId)
		}
		plan, err := l.runDesignPassGuided(ctx, req.Statement, req.OwnerUserId, nil, guidance)
		if err != nil {
			return out, fmt.Errorf("work compile: design pass for goal %s: %w", req.GoalId, err)
		}
		out.ModelCalls++
		// THE REPAIR LOOP IS BOUNDED HERE (memql#5000). It used to be handed
		// an empty plan id, and the gate that read it returned "not
		// exhausted" on its first line -- so this path, the one the work
		// spine actually uses, was bounded by repairAttemptCap and nothing
		// else. `out.ModelCalls` is what the compile has spent before the
		// bundle; the gate adds the emit and each repair to it.
		spent := out.ModelCalls
		budget := callCapGate(req.MaxModelCalls, "this run's maxModelCalls ceiling")
		bundle, _, clean, err := l.emitAndRepairBundle(ctx,
			func(gctx context.Context, callsMade int) (bool, string) {
				return budget(gctx, spent+callsMade)
			}, req.Statement, plan, sandbox)
		if err != nil {
			return out, fmt.Errorf("work compile: emit for goal %s: %w", req.GoalId, err)
		}
		out.ModelCalls++
		if !clean {
			// Gate 1 refused it. The run does NOT proceed on a draft that
			// did not compile -- that is the whole reason the gate is
			// before execution rather than after it.
			return out, fmt.Errorf("work compile: the draft for goal %s did not pass Gate 1", req.GoalId)
		}
		return l.persistWorkDraft(ctx, req, out, bundle, sandbox)
	default:
		return out, fmt.Errorf("work compile: goal %s reached no route", req.GoalId)
	}
}

// reasoningDraft synthesizes the deterministic draft for a trivial or
// sectionable goal and persists it through Gate 1. The owner's reasoning
// agent is resolved only when the draft calls it.
func (l *PlannerAgentLoop) reasoningDraft(ctx context.Context, req CompileRequest, out CompileOutcome, sandbox authoringSandbox, dec sectionableDecision) (CompileOutcome, error) {
	agentId := ""
	if dec.needsReasoningAgent(workDraftHeadline(req)) {
		var err error
		if agentId, err = l.reasoningAgent(ctx, req.OwnerUserId); err != nil {
			return out, err
		}
	}
	bundle, err := synthesizeWorkReasoningBundle(req, agentId, dec)
	if err != nil {
		return out, err
	}
	out.LiveSections = dec.liveSectionOutcome(req)
	for _, ls := range out.LiveSections {
		// The fallback that inlined every section said so once already.
		if ls.Inline != "" && dec.inlineAll == "" {
			l.infoCompile("work compile: a live section stays inline in the template", req, "section", ls.Section, "reason", ls.Inline)
		}
	}
	return l.persistWorkDraft(ctx, req, out, bundle, sandbox)
}

// maxDescriptionGuidance is how many of a goal shape's dislikes reach a
// prompt: the newest, because the person's most recent objection is the one
// most likely to still apply.
const maxDescriptionGuidance = 5

// descriptionGuidance is what the goal's owner disliked about earlier answers
// to this goal shape (epic memql#5414, design D23): their dislikes on runs of
// the same goal signature, newest first, at most maxDescriptionGuidance, each
// as the prompts' guidance input declares it -- {axes: "product, process",
// reason}. Read under the OWNER, because guidance mined from somebody else's
// dislikes would steer this person's goal by another person's taste. A failed
// read is no guidance, never a failed compile.
func (l *PlannerAgentLoop) descriptionGuidance(ctx context.Context, req CompileRequest, signature string) []map[string]any {
	if l.engine == nil || strings.TrimSpace(req.OwnerUserId) == "" || signature == "" {
		return nil
	}
	call, err := langparser.RenderCall("workDescriptionGuidance", map[string]any{"goalSignature": signature})
	if err != nil {
		l.warnCompile("work compile: the description guidance read could not be rendered", req, err)
		return nil
	}
	res, err := l.engine.Execute(ownerActorContext(ctx, req.OwnerUserId), "query "+call)
	if err != nil {
		l.warnCompile("work compile: the description guidance read failed; the goal's model calls go without it", req, err)
		return nil
	}
	rows := memql.MaterializeRows(res)
	// Newest first, as the query sorts -- ordered again here because the
	// FIVE kept must be the newest five, and a reader that trusted the order
	// would keep whichever five arrived first.
	createdAt := func(r map[string]any) time.Time {
		t, _ := time.Parse(time.RFC3339Nano, getString(r, "createdAt"))
		return t
	}
	sort.SliceStable(rows, func(i, j int) bool { return createdAt(rows[i]).After(createdAt(rows[j])) })
	var out []map[string]any
	for _, r := range rows {
		data := mapField(r, "data")
		// The query filters on both; checked again, because a guidance
		// entry from another goal shape is advice about a different goal.
		if work.ParseVerdict(getString(data, "verdict")) != work.VerdictDislike || getString(data, "goalSignature") != signature {
			continue
		}
		axes := mapField(data, "axes")
		named := work.Axes{
			Product:     axes["product"] == true,
			Process:     axes["process"] == true,
			Performance: axes["performance"] == true,
		}.Names()
		reason := strings.TrimSpace(getString(data, "reason"))
		if len(named) == 0 && reason == "" {
			continue
		}
		out = append(out, map[string]any{"axes": strings.Join(named, ", "), "reason": reason})
		if len(out) == maxDescriptionGuidance {
			break
		}
	}
	return out
}

// cataloguedForSignature is the exact tier: one owner-scoped, signature-
// filtered read. Ranked by reliability descending, so the most-proven
// template for a repeated goal is the one served.
func (l *PlannerAgentLoop) cataloguedForSignature(ctx context.Context, ownerUserId, signature string) ([]work.CatalogCandidate, error) {
	if l.engine == nil || ownerUserId == "" {
		return nil, nil
	}
	// The named-args invocation form, NOT the object-literal wrapper
	// `name({...})`: the parser refuses that outright (#2335, Story 9), and
	// the refusal is invisible from Go -- a recording fake accepts whatever
	// string it is handed, so a package can be green with nothing written.
	res, err := l.engine.Execute(ownerActorContext(ctx, ownerUserId),
		"query cataloguedConstructsForGoalSignature("+encodeArgs(map[string]any{"goalSignature": signature})+")")
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(res)
	out := make([]work.CatalogCandidate, 0, len(rows))
	for _, r := range rows {
		// The query already filters on this, and the check is still here:
		// an exact hit is served WITHOUT verification -- no model reads the
		// template, no gap list is closed -- so a row that is not actually
		// for this goal would be run confidently and wrongly. That is the
		// one failure mode on this path worth a redundant comparison.
		if got := getString(r, "goalSignature"); got != signature {
			l.warnCompile("work compile: dropping a catalogued row whose goalSignature does not match the query's argument",
				CompileRequest{OwnerUserId: ownerUserId},
				fmt.Errorf("row %s carries %q, asked for %q", getString(r, "id"), got, signature))
			continue
		}
		out = append(out, work.CatalogCandidate{
			ConstructId: getString(r, "id"),
			Name:        getString(r, "name"),
			Signature:   getString(r, "goalSignature"),
			Similarity:  1,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return reliabilityOf(rows, out[i].ConstructId) > reliabilityOf(rows, out[j].ConstructId)
	})
	return out, nil
}

// servableProceduresForSignature is the ladder's half of the exact tier: the
// owner's learned procedures for this signature that DecideServe says to
// serve FROM THE CONSTRUCT -- trusted, then canary. A shadow procedure is not
// a candidate at all: the app serves that goal, and the procedure is
// compared beside it when the recording succeeds. Candidate, retired and any
// rung this build does not know are never served.
//
// The rung is decided HERE, not trusted from the query's own filter, for the
// reason cataloguedForSignature re-checks the signature: an exact hit is
// served without a model, so a row that should not be served must be dropped
// by the one decision that owns the question.
func (l *PlannerAgentLoop) servableProceduresForSignature(ctx context.Context, ownerUserId, signature string) ([]work.CatalogCandidate, error) {
	if l.engine == nil || ownerUserId == "" {
		return nil, nil
	}
	res, err := l.engine.Execute(ownerActorContext(ctx, ownerUserId),
		"query procedureConstructsForGoalSignature("+encodeArgs(map[string]any{"goalSignature": signature})+")")
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(res)
	out := make([]work.CatalogCandidate, 0, len(rows))
	for _, r := range rows {
		if getString(r, "goalSignature") != signature {
			continue
		}
		rung, known := work.ParseRung(getString(r, "ladder"))
		if !known || rung == work.RungNone {
			continue
		}
		verdict := work.DecideServe(work.ReplayContext{Mode: "live", ConstructRung: rung})
		if verdict.Source != work.ServeConstruct {
			continue
		}
		out = append(out, work.CatalogCandidate{
			ConstructId: getString(r, "id"),
			Name:        getString(r, "name"),
			Signature:   signature,
			Similarity:  1,
			Rung:        rung,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := rungOrder(out[i].Rung), rungOrder(out[j].Rung); ri != rj {
			return ri < rj
		}
		return reliabilityOf(rows, out[i].ConstructId) > reliabilityOf(rows, out[j].ConstructId)
	})
	return out, nil
}

// rungOrder ranks trusted ahead of canary: a canary still has the app on
// standby, and when both exist for one goal the proven one serves.
func rungOrder(r work.Rung) int {
	if r == work.RungTrusted {
		return 0
	}
	return 1
}

func reliabilityOf(rows []map[string]any, id string) float64 {
	for _, r := range rows {
		if getString(r, "id") == id {
			if f, ok := r["reliability"].(float64); ok {
				return f
			}
		}
	}
	return 0
}

// nearCandidates maps the authoring pipeline's near matches onto the
// decision's shape. The list arrives similarity-descending and stays so.
func nearCandidates(in []memql.CatalogNearMatch, goalKeys []string) []work.CatalogCandidate {
	out := make([]work.CatalogCandidate, 0, len(in))
	for _, m := range in {
		out = append(out, work.CatalogCandidate{
			ConstructId: m.Name,
			Name:        m.Name,
			Similarity:  m.Similarity,
			// Every key the goal supplies is a gap until the template is
			// read and shown to declare it. Over-reporting a gap costs a
			// reasoning step; under-reporting one runs a template with an
			// argument it never receives.
			MissingArgs: append([]string(nil), goalKeys...),
		})
	}
	return out
}

// inputKeys returns the goal's argument names, sorted -- argument order
// is a spelling, not a difference.
func inputKeys(in map[string]any) []string {
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (l *PlannerAgentLoop) warnCompile(msg string, req CompileRequest, err error) {
	if l.logger == nil {
		return
	}
	l.logger.Warn(msg, "goalId", req.GoalId, "runId", req.RunId, "error", err)
}
