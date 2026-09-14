package memql

// conformance_probe.go -- the conformance corpus's seam onto the pushdown
// (epic memql#5356, task memql#5361; edition 2026, memql#5369; D23 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// The corpus under test/conformance/<edition>/ pins two verdicts that are not
// load outcomes: `lower` (the SQL a pushdown expression becomes) and
// `evaluate` (what it decides against one row). At a pushdown position both
// are the executor's own behaviour over what Lower produced, and the functions
// that produce it -- argument expansion, tryCompileCombinedFilter,
// nodeMatchesExpression -- are unexported because nothing outside the engine
// should compose them. ProbeLower exposes exactly that composition and nothing
// else, so a corpus case exercises the code a read runs rather than a copy of
// it:
//
//  1. Lower, at the case's position, over the case's concept, with its
//     arguments declared, against this engine's specs and traits -- the pass
//     the loader and Init run over a query filter or a spec body;
//  2. argument expansion, binding the arguments and the caller's envelope the
//     way a call does, so every plan constant is evaluated and folded;
//  3. the executor's pushdown compiler, which must produce one SQL predicate:
//     a corpus case asking for SQL is asking whether the expression lowers.
//
// The in-process positions need no probe: EvalExpr is exported and the corpus
// calls it directly.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// ProbeLowered is one pushdown expression as a call leaves it: lowered, its
// arguments bound and its plan constants folded, ready to run.
type ProbeLowered struct {
	// SQL is the predicate the executor pushes down, with `?` placeholders.
	SQL     string
	root    ExpressionNode
	concept string
}

// ProbeLower lowers one edition-2026 predicate lambda at a pushdown position
// (a query filter, a spec or trait body) over the concept conceptID, binds
// args and the caller ctx carries the way a call binds them, and compiles the
// result. now, when set, is the instant a plan constant reading `now` sees.
//
// A query filter's arguments are the ones its body reads, each declared with
// the type of the value args gives it (untyped when args gives none); a spec
// body takes no arguments, so an `args.x` there is refused as it is at load.
func (e *MemQLEngine) ProbeLower(ctx context.Context, position tiers.Position, conceptID string, lam *ast.LambdaExpr, args map[string]any, now time.Time) (*ProbeLowered, error) {
	if lam == nil || len(lam.Params) != 1 {
		return nil, errors.New("a pushdown expression is a lambda of one parameter, the row")
	}
	env := LowerEnv{Position: position, Param: lam.Params[0], Predicate: e.predicateLookup()}
	if e.concepts != nil && strings.TrimSpace(conceptID) != "" {
		c, err := e.concepts.Get(strings.TrimSpace(conceptID))
		if err != nil {
			return nil, fmt.Errorf("probe a pushdown over %s: %w", conceptID, err)
		}
		env.Concept = c
	}
	if position == tiers.PositionQueryFilter {
		env.Args = probeArgTypes(lam.Body, args)
	}
	ir, err := Lower(lam.Body, env)
	if err != nil {
		return nil, err
	}

	ambient := buildAmbientEnvelope(ctx, e)
	if !now.IsZero() {
		ambient["now"] = now.UTC().Format(time.RFC3339Nano)
	}
	var fns map[string]*Function
	if e.functions != nil {
		fns = e.functions.LookupIndex()
	}
	if args == nil {
		args = map[string]any{}
	}
	expanded, err := newFunctionValidatorWithAmbient(fns, e.specs, auth.OriginFromContext(ctx), ambient).expandExpressionWithArgs(ir, args)
	if err != nil {
		return nil, err
	}
	if err := rewriteFilterFieldRefs(expanded); err != nil {
		return nil, err
	}

	// The read path's own passes, in its order (evaluateExpressionSet): the
	// caller's actor references resolved, canonicalId values resolved,
	// relationship values canonicalised. A rank or account scope needs the
	// principal tables, which a probe does not read.
	resolved, err := resolveActorReferences(ctx, expanded)
	if err != nil {
		return nil, err
	}
	resolved = e.resolveCanonicalIdComparisons(ctx, resolved)
	resolved = e.canonicalizeRelationshipComparisons(ctx, resolved, conceptID)
	if treeHasRankScope(resolved) || treeHasAccountScope(resolved) {
		return nil, fmt.Errorf("`%s` reads a rank or account scope, which the probe does not resolve", ast.FormatExpr(lam))
	}
	compiled, ok := e.tryCompileCombinedFilter(ctx, resolved, conceptID)
	if !ok {
		return nil, fmt.Errorf("`%s` lowered to %s, which does not compile to one SQL predicate over %s",
			ast.FormatExpr(lam), canonicalExpression(resolved), conceptID)
	}

	// And the post-filter's (executeCombinedFilterQuery): ids resolved for
	// execution, spec references expanded, actor comparisons folded.
	post, err := resolveExpressionForExecution(resolved, conceptID)
	if err != nil {
		return nil, err
	}
	if post, err = e.expandSpecReferences(post); err != nil {
		return nil, err
	}
	if post, err = resolveActorComparisonsToConstants(ctx, post); err != nil {
		return nil, err
	}
	return &ProbeLowered{SQL: compiled.sql, root: post, concept: conceptID}, nil
}

// Matches decides one row the way the executor's in-process post-filter does
// -- the half a read re-runs on every candidate the SQL returned. The row is
// the payload; an `id` key is also the row's id.
func (p *ProbeLowered) Matches(row map[string]any) (bool, error) {
	if row == nil {
		row = map[string]any{}
	}
	payload, err := json.Marshal(row)
	if err != nil {
		return false, fmt.Errorf("row is not JSON: %w", err)
	}
	id, _ := row["id"].(string)
	node := memorynodes.MemoryNode{ID: id, Concept: p.concept, Payload: payload}
	return nodeMatchesExpression(node, p.root, map[string]map[string]any{})
}

// probeArgTypes declares every argument body reads, typed by the value args
// binds it to.
func probeArgTypes(body ast.ExpressionNode, args map[string]any) map[string]ArgType {
	out := map[string]ArgType{}
	ast.WalkV1(body, func(n ast.ExpressionNode) bool {
		m, ok := n.(*ast.MemberExpr)
		if !ok || m == nil {
			return true
		}
		if id, ok := m.Object.(*ast.IdentExpr); ok && id != nil && id.Name == "args" {
			out[m.Field] = probeArgType(args[m.Field])
		}
		return true
	})
	return out
}

func probeArgType(v any) ArgType {
	switch x := v.(type) {
	case string:
		return "string"
	case bool:
		return "bool"
	case float64:
		// A whole number declares an int: the value is only inspected, never
		// narrowed.
		if !math.IsInf(x, 0) && math.Trunc(x) == x {
			return "int"
		}
		return "float"
	case int, int64:
		return "int"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return ""
}

// LoadReport is the account of the last Init's DSL load: what registered, what
// was skipped and why, and what was registered twice. Nil before Init. The
// caller must not modify it.
func (e *MemQLEngine) LoadReport() *LoadReport {
	return e.loadReport
}
