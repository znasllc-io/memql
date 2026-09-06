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
	// ConstructId is the catalogued template reused, for the two catalog
	// routes.
	ConstructId string
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
}

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
	in.Exact = exact

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
		return l.finishCompile(ctx, req, d, out, sandbox)
	}

	// Tier 3: ONE classifier call answering complexity AND sectionability.
	complexity, _, sectionable, cerr := l.classifySectionable(ctx, req.Statement, time.Now().UTC().Format(time.RFC3339))
	out.ModelCalls++
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
	return l.finishCompile(ctx, req, d, out, sandbox)
}

// finishCompile carries out whichever route was decided.
func (l *PlannerAgentLoop) finishCompile(ctx context.Context, req CompileRequest, d work.Decision, out CompileOutcome, sandbox authoringSandbox) (CompileOutcome, error) {
	out.Route = d.Route
	if d.Candidate != nil {
		out.ConstructId = d.Candidate.ConstructId
		out.AutomationName = d.Candidate.Name
	}
	out.Gaps = d.Gaps

	switch d.Route {
	case work.RouteCatalogExact, work.RouteCatalogNear:
		// The template is the catalogue's. Nothing more to author; the
		// near route's gap list is closed by the run's own reasoning
		// steps rather than by a second compile pass.
		return out, nil
	case work.RouteSectionable:
		// Deterministic after the one triage call already counted.
		return out, nil
	case work.RouteTrivial:
		// One reasoning step; the run needs no draft.
		return out, nil
	case work.RouteAuthor:
		if sandbox == nil {
			return out, fmt.Errorf("work compile: goal %s needs authoring and no sandbox is available; a draft that cannot pass Gate 1 must not be run", req.GoalId)
		}
		plan, err := l.runDesignPass(ctx, req.Statement, req.OwnerUserId, nil)
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
		out.AutomationName = bundle.AutomationName
		return out, nil
	default:
		return out, fmt.Errorf("work compile: goal %s reached no route", req.GoalId)
	}
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
