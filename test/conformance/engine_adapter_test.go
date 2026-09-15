package conformance

// engine_adapter_test.go -- the one place the corpus runner reaches the
// expression engine for its `lower` and `evaluate` verdicts (epic memql#5356,
// task memql#5361; swapped to the edition-2026 engine by memql#5369).
//
// Nothing else in the runner knows which engine answers. The two verdicts
// reach the two evaluators of D7 -- one parser, one AST, two evaluators -- per
// the tier the position evaluates in (tiers.TierOf):
//
//   - An M position (an automation condition, a trigger filter, a logic body, a
//     mutation value, a step argument, a prompt input, a query's refine clause)
//     evaluates IN PROCESS. The case source is parsed with the edition-2026
//     parser -- ParseV1Lambda at the two row-lambda positions, whose parameter
//     is bound to the case row as memql.NewExprRow reads a stored node,
//     ParseV1Expression elsewhere -- and run through memql.EvalExpr (a
//     condition through memql.EvalCondition, which refuses a non-boolean).
//   - A P position (a query filter, a spec body) PUSHES DOWN. Its answer is
//     memql.Lower plus the executor, through MemQLEngine.ProbeLower: `lower` is
//     the SQL the pushdown compiler emits once a call has bound the case's
//     args and caller, and `evaluate` is the executor's own in-process
//     post-filter over the case row, with the SQL required to lower as well.
//   - The three literal positions (a sort key, an @rowAuthz argument, a tool
//     @default) are not expressions at all: their syntax is a literal and the
//     v1 grammar does not change it. An `evaluate` case there holds the literal
//     and answers with its value; nothing lowers.
//
// A case binds the roots its position has, and only those. `args` and `actor`
// are refused where the position has no such root -- a spec body reads only
// its parameter and the clock, a trigger filter only the triggering row -- so
// a case cannot claim an answer the position could never give.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
	"github.com/znasllc-io/memql/component/memql"
)

// ExprEnv is what an expression is lowered or evaluated against.
//
// (Not `Env`: that name belongs to the MCP conformance harness in this
// package.)
type ExprEnv struct {
	// Row is the row the expression is decided against (evaluate).
	Row map[string]any
	// Args and Actor are the values `args.*` and `actor.*` read.
	Args  map[string]any
	Actor map[string]any
	// Concept is the canonical id of the concept the expression is over.
	Concept string
	// Engine is a booted engine with the case directory's fixture mounted.
	Engine *memql.MemQLEngine
	// Predicates resolves a spec or trait the case directory's fixture
	// declares in the edition-2026 form to its parameter and body, for the
	// in-process evaluator (memql.EvalOptions.Predicates). Nil resolves none.
	Predicates func(name string) (param string, body ast.ExpressionNode, ok bool)
	// Calls answers the construct calls an expression makes, by
	// "<kind> <name>" (the case's `calls`).
	Calls map[string]json.RawMessage
}

// corpusNow is the instant `now` reads in every case, so a case that reads
// the clock has one answer on every run. The corpus README names it.
var corpusNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// lower returns the SQL an expression at a pushdown position lowers to.
func lower(position tiers.Position, src string, env ExprEnv) (string, error) {
	if err := adapterReady(position, env); err != nil {
		return "", err
	}
	if adapterLiteralPosition(position) {
		return "", fmt.Errorf("position %s is a literal, not an expression: nothing lowers there", position)
	}
	if tiers.TierOf(position) != tiers.TierP {
		return "", fmt.Errorf("position %s evaluates in process (tier M): it has no SQL; write an evaluate case", position)
	}
	res, err := adapterPushdown(position, src, env)
	if err != nil {
		return "", err
	}
	return res.SQL, nil
}

// evaluate returns what an expression at a position decides against env.
func evaluate(position tiers.Position, src string, env ExprEnv) (any, error) {
	if err := adapterReady(position, env); err != nil {
		return nil, err
	}
	switch {
	case adapterLiteralPosition(position):
		return evaluateLiteral(position, src)
	case tiers.TierOf(position) == tiers.TierP:
		res, err := adapterPushdown(position, src, env)
		if err != nil {
			return nil, err
		}
		return res.Matches(env.Row)
	}

	opts := memql.EvalOptions{
		Now:        corpusNow,
		Predicates: env.Predicates,
		CanonicalID: func(ctx context.Context, value any, concept string) (string, error) {
			return env.Engine.CanonicalizeIdValue(ctx, fmt.Sprint(value), concept)
		},
	}
	if tiers.Allows(position, ast.KindConstructCall) {
		opts.Calls = adapterCalls(env)
	}
	scope := adapterScope(position, env)
	ctx := context.Background()

	if adapterRowLambdaPosition(position) {
		lam, err := langparser.ParseV1Lambda(src)
		if err != nil {
			return nil, err
		}
		if len(lam.Params) != 1 {
			return nil, fmt.Errorf("position %s takes a lambda of one parameter, the row", position)
		}
		row, err := adapterRow(env)
		if err != nil {
			return nil, err
		}
		scope[lam.Params[0]] = row
		return memql.EvalCondition(ctx, lam.Body, scope, opts)
	}

	n, err := langparser.ParseV1Expression(src)
	if err != nil {
		return nil, err
	}
	if position == tiers.PositionAutomationCondition {
		return memql.EvalCondition(ctx, n, scope, opts)
	}
	v, err := memql.EvalExpr(ctx, n, scope, opts)
	if err != nil {
		return nil, err
	}
	if memql.IsAbsent(v) {
		// The top-level Absent sentinel is JSON null in every value a
		// position hands on (its MarshalJSON), so a case pins it as null.
		return nil, nil
	}
	return v, nil
}

// adapterPushdown lowers a pushdown-position lambda through the engine's
// probe: Lower at the position, the case's args and caller bound as a call
// binds them, the pushdown compiled.
func adapterPushdown(position tiers.Position, src string, env ExprEnv) (*memql.ProbeLowered, error) {
	lam, err := langparser.ParseV1Lambda(src)
	if err != nil {
		return nil, err
	}
	return env.Engine.ProbeLower(adapterCaller(env.Actor), position, env.Concept, lam, env.Args, corpusNow)
}

// adapterCaller is the call a pushdown case is made in: as the case's actor
// when it names one, else as a caller with no role.
func adapterCaller(actor map[string]any) context.Context {
	user, _ := actor["userId"].(string)
	role, _ := actor["role"].(string)
	if user == "" {
		return context.Background()
	}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: auth.Role(role)})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: user})
}

// evaluateLiteral answers a literal position: the one node must be a literal,
// and its value is the answer.
func evaluateLiteral(position tiers.Position, src string) (any, error) {
	n, err := langparser.ParseV1Expression(src)
	if err != nil {
		return nil, err
	}
	lit, ok := n.(*ast.LiteralExpr)
	if !ok || tiers.KindAdmission(position, ast.KindOf(n)) == tiers.Refused {
		return nil, fmt.Errorf("position %s takes a literal; `%s` is %s", position, ast.FormatExpr(n), ast.KindOf(n))
	}
	return lit.Value, nil
}

// adapterReady refuses a case the position cannot answer: an engine that did
// not boot, or a root the position does not have.
func adapterReady(position tiers.Position, env ExprEnv) error {
	if env.Engine == nil {
		return errors.New("no engine booted for this case")
	}
	roots := adapterRoots[position]
	if len(env.Args) > 0 && !roots.args {
		return fmt.Errorf("position %s has no `args` root: a case there binds no args", position)
	}
	if len(env.Actor) > 0 && !roots.actor {
		return fmt.Errorf("position %s has no `actor` root: a case there binds no actor", position)
	}
	return nil
}

// adapterRootSet is which of the two caller-supplied roots a position reads.
type adapterRootSet struct{ args, actor bool }

// adapterRoots restates, for the two roots a case binds, the roots each
// position evaluates with -- the in-process scope the evaluator binds and the
// plan constants a pushdown position folds -- as Sense's completion states
// them (component/memql/sense/complete_expr.go, positionRoots). A spec body
// reads its parameter and the clock; a trigger filter its row and the clock;
// a prompt input has no actor. The literal positions read nothing.
var adapterRoots = map[tiers.Position]adapterRootSet{
	tiers.PositionBeforeWriteValue:    {args: true, actor: true},
	tiers.PositionQueryFilter:         {args: true, actor: true},
	tiers.PositionQueryRefine:         {args: true, actor: true},
	tiers.PositionAutomationCondition: {args: true, actor: true},
	tiers.PositionLogicBody:           {args: true, actor: true},
	tiers.PositionMutationValue:       {args: true, actor: true},
	tiers.PositionStepArgument:        {args: true, actor: true},
	tiers.PositionPromptInput:         {args: true},
}

// adapterScope binds the roots the position has: `args` and `actor` as the
// case names them (empty when it names none, as a call with no arguments
// still has an args root).
func adapterScope(position tiers.Position, env ExprEnv) memql.MapScope {
	scope := memql.MapScope{}
	if position == tiers.PositionBeforeWriteValue {
		scope["row"] = adapterMap(env.Row)
	}
	roots := adapterRoots[position]
	if roots.args {
		scope["args"] = adapterMap(env.Args)
	}
	if roots.actor {
		scope["actor"] = adapterMap(env.Actor)
	}
	return scope
}

func adapterMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// adapterRow is the case row as a stored node reads: the whole map is the
// payload, and an `id` key is also the row's id, so `row.id` reads the
// intrinsic exactly as it does on a stored row.
func adapterRow(env ExprEnv) (memql.ExprRow, error) {
	payload, err := json.Marshal(adapterMap(env.Row))
	if err != nil {
		return memql.ExprRow{}, fmt.Errorf("the case row is not JSON: %w", err)
	}
	id, _ := env.Row["id"].(string)
	return memql.NewExprRow(memorynodes.MemoryNode{ID: id, Concept: env.Concept, Payload: payload})
}

// adapterLiteralPosition reports the positions whose syntax is a literal: the
// tier manifest admits no other node kind there.
func adapterLiteralPosition(position tiers.Position) bool {
	for _, k := range tiers.NodeKinds(position) {
		if k != ast.KindLiteral {
			return false
		}
	}
	return tiers.Allows(position, ast.KindLiteral)
}

// adapterRowLambdaPosition reports the in-process positions written as a
// lambda over a row: a trigger filter and a refine clause.
func adapterRowLambdaPosition(position tiers.Position) bool {
	return position == tiers.PositionTriggerFilter || position == tiers.PositionQueryRefine
}

// adapterCalls answers a construct call from the case's `calls`, at the
// positions that admit one (a logic body, a step argument; everywhere else the
// evaluator refuses the call as construct_call_not_allowed, which is the tier
// manifest's answer). The corpus boots no database, so what the call returns
// is the case's to state; what is held to the engine is everything around it
// -- that the construct is one the fixture declares and the engine
// registered, and how the expression evaluates the call's named arguments and
// uses its result.
func adapterCalls(env ExprEnv) func(ctx context.Context, call *ast.CallExpr, named map[string]any) (any, error) {
	return func(_ context.Context, call *ast.CallExpr, _ map[string]any) (any, error) {
		key := call.Kind + " " + call.Name
		switch call.Kind {
		case "query", "mutation", "logic":
			if !env.Engine.Functions().Has(call.Name) {
				return nil, fmt.Errorf("%s is not a construct the engine registered: the case directory's fixture declares it", key)
			}
		default:
			return nil, fmt.Errorf("the corpus answers query, mutation and logic calls; %s is none of them", key)
		}
		raw, ok := env.Calls[key]
		if !ok {
			return nil, fmt.Errorf("the case names no answer for %s: add it to calls", key)
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("calls[%q] is not JSON: %w", key, err)
		}
		return v, nil
	}
}

// adapterPredicates reads the edition-2026 specs and traits a fixture
// declares: name to parameter and body. A fixture with none, or one that does
// not parse, yields none -- its own load reports why.
func adapterPredicates(edition, fixture string) func(name string) (string, ast.ExpressionNode, bool) {
	file, err := corpusParseFile(edition, fixture)
	if err != nil || file == nil {
		return nil
	}
	preds := map[string]*ast.LambdaExpr{}
	for _, def := range file.Definitions {
		if decl, ok := def.(*ast.SpecDecl); ok && decl.Lambda != nil && len(decl.Lambda.Params) == 1 {
			preds[decl.Name] = decl.Lambda
		}
	}
	if len(preds) == 0 {
		return nil
	}
	return func(name string) (string, ast.ExpressionNode, bool) {
		lam, ok := preds[strings.TrimSpace(name)]
		if !ok {
			return "", nil, false
		}
		return lam.Params[0], lam.Body, true
	}
}
