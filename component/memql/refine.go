package memql

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/functions"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
	"github.com/znasllc-io/memql/component/metrics"
)

// refine.go -- the `refine` clause (epic memql#5363, task memql#5366; D11 of
// the design record: "evaluating an expression in process over a bounded page
// is a named construct, never an automatic escape").
//
//	query ticket urgentOpen {
//	  filter   row => row.status == "open"
//	  paginate 50
//	  refine   row => row.title.includes(args.q) || lower(row.title) == args.q
//	}
//
// The filter pushes down; the page of at most 50 rows comes back from SQL; and
// only THEN is each row of that page decided by EvalExpr, the in-process
// evaluator, with the lambda's parameter bound to the row (NewExprRow). A row
// the predicate refuses is dropped, so a refined query may return FEWER rows
// than its page size -- that is the construct's contract, and the reason the
// rewriter refuses a refine without an authored paginate: without one, the
// "page" would be the whole matching set, which is the silent client-side scan
// the pushdown tier exists to refuse.
//
// # Where it runs in the plan
//
// The struct-form rewriter places it inside shape and outside paginate:
// shape(refine(paginate(...), row => ...)). applyDirectiveWrappers peels it off
// into plan.Refine like every other directive, and Execute applies it to the
// page between the SQL read and the bundle -- so the page CURSOR is minted
// from the last row SQL returned, not the last row refine kept. A caller
// paging through a refined query therefore never skips the rows a dropped
// page was hiding, and a page that refine emptied still carries a cursor to
// the next one.
//
// # What it may read
//
// It is an M-tier position (tiers.PositionQueryRefine): every catalog
// function, the row's fields through the parameter, the query's arguments,
// the actor, `now` and `config` -- the same bindings a plan constant reads,
// captured when the call's arguments are expanded -- and predicates applied to
// the row, which must be edition-2026 specs or traits (EvalExpr evaluates a
// predicate from its v1 body; a legacy body has none). The engine's Init pass
// checks all of it, and refuses a lambda whose static cost estimate exceeds
// tiers.MaxStaticCost (validateRefine).

// RefineExpression is `refine <lambda>` over the page Target reads.
type RefineExpression struct {
	Target ExpressionNode
	Lambda *ast.LambdaExpr
	// Bindings are the names the lambda may read besides its parameter --
	// args, actor, config, now -- captured by argument expansion, which is
	// the first point the call's arguments are known. Nil on a registered
	// (unexpanded) tree.
	Bindings map[string]any
}

func (*RefineExpression) isExpressionNode() {}

// convertRefineExpr converts the parser's refine directive. The target is an
// ordinary query tree; the lambda stays the parsed v1 AST, because it is
// evaluated in process by EvalExpr and never lowered.
func (c *ASTConverter) convertRefineExpr(expr *languageParser.RefineExpr) (ExpressionNode, error) {
	if expr == nil {
		return nil, nil
	}
	if expr.Lambda == nil || len(expr.Lambda.Params) != 1 {
		return nil, fmt.Errorf("refine takes a lambda of one parameter, the row: refine row => <predicate>")
	}
	target, err := c.ConvertExpression(expr.Target)
	if err != nil {
		return nil, fmt.Errorf("convert refine target: %w", err)
	}
	return &RefineExpression{Target: target, Lambda: expr.Lambda}, nil
}

// refineScope is what a refine lambda reads: its parameter is the row, and
// every other name is one of the bindings.
type refineScope struct {
	param    string
	row      ExprRow
	bindings map[string]any
}

func (s refineScope) Lookup(name string) (any, bool) {
	if name == s.param {
		return s.row, true
	}
	v, ok := s.bindings[name]
	return v, ok
}

// applyRefine filters one SQL page through a refine lambda, in page order, and
// counts what it kept and dropped (metrics.QueryRefineRows). A row the
// predicate cannot decide -- a computed value that is not boolean, an
// in-process error -- fails the read naming the row: dropping it silently
// would make an error look like a short page. A stored value of the wrong
// type is decided, as the pushdown decides it: it is not true.
func (e *MemQLEngine) applyRefine(ctx context.Context, refine *RefineExpression, query string, nodes []memorynodes.MemoryNode) ([]memorynodes.MemoryNode, error) {
	if refine == nil || refine.Lambda == nil || len(nodes) == 0 {
		return nodes, nil
	}
	bindings := make(map[string]any, 4)
	// The ambient envelope first, so a refine reached without argument
	// expansion (a raw runtime query string) still reads the actor and the
	// clock; then the expansion's own capture, which wins.
	ambient := buildAmbientEnvelope(ctx, e)
	for _, root := range planConstantAmbientRoots {
		if v, ok := ambient[root]; ok {
			bindings[root] = v
		}
	}
	for k, v := range refine.Bindings {
		bindings[k] = v
	}
	opts := EvalOptions{Predicates: e.refinePredicates(), CanonicalID: canonicalIDForPlanConstant}
	param := refine.Lambda.Params[0]
	kept := make([]memorynodes.MemoryNode, 0, len(nodes))
	for _, node := range nodes {
		row, err := NewExprRow(node)
		if err != nil {
			return nil, fmt.Errorf("refine %s: %w", ast.FormatExpr(refine.Lambda), err)
		}
		ok, err := EvalCondition(ctx, refine.Lambda.Body, refineScope{param: param, row: row, bindings: bindings}, opts)
		if err != nil {
			return nil, fmt.Errorf("refine %s over row %s: %w", ast.FormatExpr(refine.Lambda), node.ID, err)
		}
		if ok {
			kept = append(kept, node)
		}
	}
	metrics.QueryRefineRows(query, len(kept), len(nodes)-len(kept))
	return kept, nil
}

// refinePredicates is EvalOptions.Predicates over the spec registry: an
// edition-2026 spec or trait by name, as its parameter and body. A spec with a
// legacy body has no v1 body to evaluate and is not found, which EvalExpr
// reports as an unknown predicate; the Init pass refuses that shape at load.
func (e *MemQLEngine) refinePredicates() func(string) (string, ast.ExpressionNode, bool) {
	return func(name string) (string, ast.ExpressionNode, bool) {
		if e == nil || e.specs == nil {
			return "", nil, false
		}
		spec, err := e.specs.Get(name)
		if err != nil || spec == nil || spec.Lambda == nil || len(spec.Lambda.Params) != 1 {
			return "", nil, false
		}
		return spec.Lambda.Params[0], spec.Lambda.Body, true
	}
}

// validateRefine is the load-time half of the refine contract (the Init pass):
//
//   - the lambda's static cost estimate is within tiers.MaxStaticCost;
//   - every node kind is one the refine position admits (a construct call is
//     not: a refine decides about a row, it does not read or write);
//   - every call is a catalog function or method, or a predicate applied to
//     the row whose v1 body EvalExpr can evaluate;
//   - every name is the parameter, a lambda parameter, args (a declared
//     argument), actor, now or config;
//   - no condition position holds a bare field of the bound concept whose
//     declared type is not boolean (CheckConditionFields, D8): at run time
//     such a field is "not true" on every row, so the refine would load and
//     silently empty every page.
func (e *MemQLEngine) validateRefine(fn *Function, refine *RefineExpression) error {
	var concept *memorynodes.Concept
	if e != nil && e.concepts != nil && fn != nil && fn.BoundConcept != "" {
		concept, _ = e.concepts.Get(fn.BoundConcept)
	}
	return validateRefineIn(fn, refine, e.predicateLookup(), concept)
}

// validateRefineIn is validateRefine over an explicit spec lookup -- the
// engine's registry at Init, the scope an authored construct is lowered in at
// define time. A nil lookup (an engine-free validation, which sees no spec
// registry) defers the predicate checks to the lowering that has one.
// concept is the query's bound concept: a bare field of a declared non-boolean
// type used as the refine's condition is refused against it
// (CheckConditionFields), as Lower refuses one in a filter. Nil checks no
// field types.
func validateRefineIn(fn *Function, refine *RefineExpression, lookup func(string) (*Spec, bool), concept *memorynodes.Concept) error {
	lam := refine.Lambda
	if lam == nil || len(lam.Params) != 1 {
		return fmt.Errorf("refine takes a lambda of one parameter, the row")
	}
	param := lam.Params[0]
	if cost := EstimateCost(lam.Body); cost > tiers.MaxStaticCost {
		return &LowerError{Node: ast.FormatExpr(lam), Position: tiers.PositionQueryRefine,
			Reason: fmt.Sprintf("its static cost estimate is %d node evaluations, above tiers.MaxStaticCost (%d)", cost, tiers.MaxStaticCost),
			Fix:    "Scan one list per element rather than nesting scans, or move the work into a logic body over a smaller input",
			Span:   nodeSpan(lam)}
	}
	declared := map[string]bool{}
	if fn != nil && fn.ArgsSchema != nil {
		for _, f := range fn.ArgsSchema.Fields {
			if f != nil {
				declared[f.Name] = true
			}
		}
	}
	var walkErr error
	var walk func(n ast.ExpressionNode, local map[string]bool)
	refuse := func(n ast.ExpressionNode, reason, fix string) {
		if walkErr == nil {
			walkErr = &LowerError{Node: ast.FormatExpr(n), Position: tiers.PositionQueryRefine, Reason: reason, Fix: fix, Span: nodeSpan(n)}
		}
	}
	walk = func(n ast.ExpressionNode, local map[string]bool) {
		if walkErr != nil || n == nil {
			return
		}
		if kind := ast.KindOf(n); kind != "" && tiers.KindAdmission(tiers.PositionQueryRefine, kind) == tiers.Refused {
			refuse(n, fmt.Sprintf("%s is not admitted in a refine clause: a refine decides about a row, it does not read or write", kind),
				"Call it from a logic body and pass the result to the query as an argument")
			return
		}
		switch x := n.(type) {
		case *ast.IdentExpr:
			switch {
			case local[x.Name] || x.Name == param:
			case x.Name == "args" || x.Name == "actor" || x.Name == "now" || x.Name == "config":
			default:
				refuse(x, fmt.Sprintf("`%s` is not defined here", x.Name), "A refine reads its row ("+param+"), args, actor, now and config")
			}
		case *ast.MemberExpr:
			if root, path, ok := simpleRootPath(x); ok && !local[root] && root == "args" && !declared[path[0]] {
				refuse(x, fmt.Sprintf("`args.%s` is not a declared argument", path[0]), "Declare it in the query's args block")
				return
			}
			walk(x.Object, local)
		case *ast.CallExpr:
			if x.Receiver == nil && x.Kind == "" {
				if _, ok := functions.Lookup(x.Name); !ok {
					checkRefinePredicate(x, param, local, refuse, lookup)
				} else if isTraversalName(x.Name) {
					refuse(x, "a traversal selects rows in SQL and has no in-process value", "Move it into the query's filter")
					return
				}
			}
			if x.Receiver != nil {
				walk(x.Receiver, local)
			}
			for _, a := range x.Args {
				walk(a, local)
			}
			for _, a := range x.Named {
				walk(a.Value, local)
			}
		case *ast.LambdaExpr:
			inner := make(map[string]bool, len(local)+len(x.Params))
			for k, v := range local {
				inner[k] = v
			}
			for _, p := range x.Params {
				inner[p] = true
			}
			walk(x.Body, inner)
		case *ast.UnaryExpr:
			walk(x.Operand, local)
		case *ast.BinaryExpr:
			walk(x.Left, local)
			walk(x.Right, local)
		case *ast.TernaryExpr:
			walk(x.Condition, local)
			walk(x.Then, local)
			walk(x.Else, local)
		case *ast.ListExpr:
			for _, el := range x.Elems {
				walk(el, local)
			}
		case *ast.MapExpr:
			for _, en := range x.Entries {
				walk(en.Value, local)
			}
		case *ast.ParenExpr:
			walk(x.Inner, local)
		}
	}
	walk(lam.Body, map[string]bool{})
	if walkErr != nil {
		return walkErr
	}
	return CheckConditionFields(lam, concept, tiers.PositionQueryRefine)
}

// checkRefinePredicate checks a predicate application inside a refine: it is
// applied to the row, and it names an edition-2026 row spec or trait -- the
// only kind EvalExpr can evaluate.
func checkRefinePredicate(call *ast.CallExpr, param string, local map[string]bool, refuse func(ast.ExpressionNode, string, string), lookup func(string) (*Spec, bool)) {
	if len(call.Args) != 1 {
		refuse(call, fmt.Sprintf("a predicate is applied to exactly one argument, and %s() has %d", call.Name, len(call.Args)), "Apply it to the row: `"+call.Name+"("+param+")`")
		return
	}
	if arg, ok := ast.Unparen(call.Args[0]).(*ast.IdentExpr); !ok || arg.Name != param || local[arg.Name] {
		refuse(call, "a predicate in a refine is applied to the row", "Apply it to the row: `"+call.Name+"("+param+")`")
		return
	}
	if lookup == nil {
		// No registry to ask: the engine-aware lowering decides.
		return
	}
	spec, _ := lookup(call.Name)
	switch {
	case spec == nil:
		refuse(call, fmt.Sprintf("`%s` is not a spec, trait or catalog function known here", call.Name), "Check the name and the file-top `use` import")
	case spec.Kind == SpecKindContext:
		refuse(call, fmt.Sprintf("`%s` is a context spec over the actor, not a predicate over rows", call.Name), "Apply it in the query's filter: `"+call.Name+"(actor)`")
	case spec.Lambda == nil:
		refuse(call, fmt.Sprintf("`%s` has a pre-2026 body, which the in-process evaluator cannot evaluate", call.Name),
			"Migrate it with memqlmigrate --rewrite=expressions, or apply it in the query's filter instead")
	}
}

// refineIn finds the refine clause of a registered query tree, under the
// directive wrappers it sits among.
func refineIn(expr ExpressionNode) *RefineExpression {
	for {
		switch n := expr.(type) {
		case *RefineExpression:
			return n
		case *ShapeExpression:
			expr = n.Target
		case *SortExpression:
			expr = n.Target
		case *PaginateExpression:
			expr = n.Target
		case *SelectExpression:
			expr = n.Target
		case *TimestampExpression:
			expr = n.Target
		case *DepthExpression:
			expr = n.Target
		case *CountExpression:
			expr = n.Target
		default:
			return nil
		}
	}
}
