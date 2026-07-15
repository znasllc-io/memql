package memql

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	parser "github.com/znasllc-io/memql/component/language/parser"
)

// collection_method.go implements the Story 4 (#2302 / ADR §2.2)
// LINQ-style, method-chained collection library with arrow lambdas. The
// surface is evaluated IN-MEMORY, post-fetch:
//
//	args.members.where(m => m.role == "admin" && m.active).count()
//
// It is available in logic bodies (single-statement `return <chain>`) and
// automation forEach; specs and query filters reject it at load (the
// ASTConverter only admits these nodes when constructed via
// NewASTConverter(WithCollectionMethods())). Lambda bodies must be pure --
// a mutation/action call inside a lambda is a load error (enforced in the
// post-load function validation pass).

// CollectionMethodExpression is the engine-side node for a single
// method-chain call: `<receiver>.<method>(<args>...)`. The receiver
// evaluates to a collection (or a prior CollectionMethodExpression in a
// chain); Args carry the parsed call arguments, each of which may be a
// LambdaExpression.
type CollectionMethodExpression struct {
	Receiver ExpressionNode
	Method   string
	Args     []ExpressionNode
}

func (*CollectionMethodExpression) isExpressionNode() {}

// LambdaExpression is the engine-side arrow lambda used as a collection
// method argument: `m => m.active` or `(acc, x) => acc + x`.
type LambdaExpression struct {
	Params []string
	Body   ExpressionNode
}

func (*LambdaExpression) isExpressionNode() {}

// DotAccessExpression is the engine-side node for a field access on a call
// result (#2542 item 4): `<object>.<field>`, e.g. `rows.first().createdAt`.
// The parser only produces the corresponding DotAccessExpr after a call
// form (consumePostCallDotAccess), so the object is always an in-memory
// value -- the node is gated like the collection-method surface and never
// reaches SQL compilation.
type DotAccessExpression struct {
	Object ExpressionNode
	Field  string
}

func (*DotAccessExpression) isExpressionNode() {}

// BinaryComparisonExpression is the engine-side EXPRESSION-LED comparison
// (#2542 item 5 residual): both operands are expression nodes, unlike the
// Field-led ComparisonExpression (a field reference vs a literal/arg value).
// It is produced by the converter from the parser's BinaryComparisonExpr
// (`r.count - 10 > 0`, `0 < r.count`) and gated like the rest of the
// collection-method surface -- admitted only under WithCollectionMethods and
// evaluated IN-MEMORY (evalCollScalar), never compiled to SQL.
type BinaryComparisonExpression struct {
	Left     ExpressionNode
	Operator ComparisonOperator
	Right    ExpressionNode
}

func (*BinaryComparisonExpression) isExpressionNode() {}

// collectionMethodNames is the closed set of collection operators (ADR
// §2.2). Kept in sync with the parser-side collectionMethods set.
var collectionMethodNames = map[string]bool{
	"where": true, "select": true, "first": true, "last": true,
	"single": true, "any": true, "all": true, "count": true,
	"sum": true, "min": true, "max": true, "avg": true,
	"orderby": true, "orderbydesc": true, "take": true, "skip": true,
	"distinct": true, "groupby": true, "reduce": true, "empty": true,
	"contains": true,
}

// evaluateCollectionMethodExpression evaluates a (possibly chained)
// collection-method expression to a plain Go value. args carries the
// caller arguments (for any unsubstituted ArgRefExpression receivers);
// the per-element lambda scope is threaded internally.
func evaluateCollectionMethodExpression(expr *CollectionMethodExpression, args map[string]any) (any, error) {
	return evalCollScalar(expr, args, nil)
}

// LooksLikeCollectionChain reports whether a source string looks like a
// Story 4 (#2302 / ADR §2.2) collection-method chain -- it carries an
// arrow lambda or a `.<method>(` collection-operator call. Used by the
// automations forEach source resolver to decide whether to route a source
// string through EvaluateCollectionChainString. Legacy step/path sources
// never match (no `=>`, no collection-operator method call).
func LooksLikeCollectionChain(src string) bool {
	if strings.Contains(src, "=>") {
		return true
	}
	for m := range collectionMethodNames {
		if strings.Contains(src, "."+m+"(") {
			return true
		}
	}
	return false
}

// EvaluateCollectionChainString parses, converts, and evaluates a
// collection-method chain string (Story 4 / #2302 / ADR §2.2) for use as
// an automation forEach source, a logic-body chain (#2317), or a forEach
// filter (#2318). The chain's base receiver path (e.g. `args.members`,
// `loadUsers.result`, a prior `:=` step variable) is resolved by the
// caller-supplied resolveBase callback (the automations evaluator), then the
// in-memory method chain runs over it.
//
// args carries the caller's outer arguments so a lambda body can read an outer
// `args.X` reference that is NOT the bound element (#2318); pass nil when there
// are none (element-only lambdas don't need it). The base receiver is resolved
// via resolveBase regardless, so an `args.X` BASE keeps resolving through the
// caller.
//
// Returns isChain=false (with a nil error) when src does not parse to a
// collection-method chain, so the caller falls back to its normal source
// resolution. A genuine evaluation failure returns isChain=true + the error.
func EvaluateCollectionChainString(src string, resolveBase func(basePath string) (any, error), args map[string]any) (value any, isChain bool, err error) {
	pexpr, perr := parser.ParseExpression(strings.TrimSpace(src))
	if perr != nil {
		return nil, false, nil
	}
	// A trailing field pluck (`<chain>.first().createdAt`, #2542 item 4)
	// parses as DotAccessExpr layers around the MethodCallExpr chain; peel
	// them for the chain check only -- the full expression (dot accesses
	// included) is what gets evaluated below.
	inner := pexpr
	for {
		dot, ok := inner.(*parser.DotAccessExpr)
		if !ok {
			break
		}
		inner = dot.Object
	}
	if _, ok := inner.(*parser.MethodCallExpr); !ok {
		return nil, false, nil
	}
	engineExpr, cerr := NewASTConverter(WithCollectionMethods()).ConvertExpression(pexpr)
	if cerr != nil {
		return nil, true, cerr
	}
	root := engineExpr
	for {
		dot, ok := root.(*DotAccessExpression)
		if !ok {
			break
		}
		root = dot.Object
	}
	chain, ok := root.(*CollectionMethodExpression)
	if !ok {
		return nil, false, nil
	}

	// Resolve the chain's base receiver via the caller, then substitute it
	// as a concrete literal collection so the in-memory evaluator runs over
	// real data.
	basePath := collectionChainBasePath(chain)
	if basePath == "" {
		return nil, true, fmt.Errorf("collection chain has no resolvable base receiver")
	}
	resolved, rerr := resolveBase(basePath)
	if rerr != nil {
		return nil, true, fmt.Errorf("resolve collection source %q: %w", basePath, rerr)
	}
	substituteChainBase(chain, &LiteralValueNode{Value: resolved})

	val, eerr := evalCollScalar(engineExpr, args, nil)
	if eerr != nil {
		return nil, true, eerr
	}
	return val, true, nil
}

// collectionChainBasePath returns the dotted path string of the chain's
// innermost (base) receiver, in the spelling the automations evaluator
// expects (`args.X` keeps its prefix; a bare spec/step path is verbatim).
func collectionChainBasePath(chain *CollectionMethodExpression) string {
	base := baseReceiver(chain)
	switch n := base.(type) {
	case *ArgRefExpression:
		return "args." + n.Path
	case *SpecReferenceExpression:
		return n.Name
	default:
		return ""
	}
}

// baseReceiver descends a method chain to its innermost non-method receiver.
func baseReceiver(chain *CollectionMethodExpression) ExpressionNode {
	cur := chain.Receiver
	for {
		inner, ok := cur.(*CollectionMethodExpression)
		if !ok {
			return cur
		}
		cur = inner.Receiver
	}
}

// substituteChainBase replaces the innermost receiver of a method chain
// with the given node (a resolved literal collection).
func substituteChainBase(chain *CollectionMethodExpression, literal ExpressionNode) {
	if inner, ok := chain.Receiver.(*CollectionMethodExpression); ok {
		substituteChainBase(inner, literal)
		return
	}
	chain.Receiver = literal
}

// evalCollScalar evaluates an engine expression node to a scalar value
// within the collection-lambda subset. locals binds lambda parameters to
// the current element(s); args carries caller arguments.
func evalCollScalar(expr ExpressionNode, args map[string]any, locals map[string]any) (any, error) {
	switch node := expr.(type) {
	case nil:
		return nil, nil
	case *CollectionMethodExpression:
		return evalCollectionMethod(node, args, locals)
	case *LambdaExpression:
		return nil, fmt.Errorf("a lambda cannot be evaluated outside a collection method")
	case *LiteralValueNode:
		// A projection object / array literal (#2542 item 3) carries engine
		// ExpressionNode values (`{worker: g.key, acc: (g.a.count() /
		// g.b.count())}`); evaluate each against the current lambda scope so
		// the projection yields resolved per-element values, not the AST. A
		// plain scalar literal falls through to resolveLiteralValue unchanged.
		return evalCollLiteralValue(node.Value, args, locals)
	case *ArgRefExpression:
		if val, ok := getNestedValue(args, node.Path); ok {
			return val, nil
		}
		return nil, nil
	case *SpecReferenceExpression:
		// A bare reference inside a lambda body (`m`, `m.active`,
		// `m.profile.age`). The first dotted segment names a bound lambda
		// param; the remainder walks into the element.
		return resolveLambdaPath(node.Name, locals), nil
	case *FunctionCallExpression:
		// Date/duration builtins (#2541) -- the converter emits them as
		// positional FunctionCallExpressions (like coalesce/cond/concat).
		// They appear here as a lambda comparison value or an arithmetic
		// operand of a logic terminal return (engine.go's plan-root branch
		// routes through evalCollScalar). Any other call name keeps the
		// unsupported error below.
		if IsDateBuiltin(node.Name) {
			return evalCollDateBuiltin(node, args, locals)
		}
		if node.Name == "cond" {
			// cond(predicate, thenValue, elseValue) (#2542 item 2): the
			// converter emits a positional FunctionCallExpression. Evaluate the
			// predicate to a truthy value, then the chosen branch -- so a cond
			// nested in a lambda / projection (`select(g => cond(g.items.any(),
			// g.key, "none"))`) and a single-return `return cond(...)` both
			// drive their chain operands through this evaluator.
			return evalCollCond(node, args, locals)
		}
		return nil, fmt.Errorf("unsupported expression %T inside a collection lambda", expr)
	case *ComparisonExpression:
		return evalCollComparison(node, args, locals)
	case *BinaryComparisonExpression:
		return evalCollBinaryComparison(node, args, locals)
	case *LogicalExpression:
		return evalCollLogical(node, args, locals)
	case *ArithmeticExpression:
		return evalCollArithmetic(node, args, locals)
	case *DotAccessExpression:
		// Field access on a call result (#2542 item 4): evaluate the
		// object, then walk one field segment. A nil object (e.g. an
		// empty collection's .first()) or a missing/non-map field yields
		// nil -- the same absent-value semantics walkValuePath applies
		// everywhere else, so `coalesce(rows.first().x, "d")` patterns
		// keep working.
		obj, err := evalCollScalar(node.Object, args, locals)
		if err != nil {
			return nil, err
		}
		return walkValuePath(obj, []string{node.Field}), nil
	default:
		return nil, fmt.Errorf("unsupported expression %T inside a collection lambda", expr)
	}
}

// evalCollDateBuiltin resolves each positional operand of a date/duration
// builtin call (#2541) through the collection-lambda subset, then dispatches
// the shared name-keyed evaluator. Operands are positional ("0", "1", ...);
// a substituted caller arg arrives as a raw Go value and passes through
// verbatim.
func evalCollDateBuiltin(node *FunctionCallExpression, args, locals map[string]any) (any, error) {
	vals := make([]any, len(node.Args))
	for i := range vals {
		raw, ok := node.Args[strconv.Itoa(i)]
		if !ok {
			return nil, fmt.Errorf("%s() is missing positional arg %d", node.Name, i)
		}
		if expr, isExpr := raw.(ExpressionNode); isExpr {
			v, err := evalCollScalar(expr, args, locals)
			if err != nil {
				return nil, fmt.Errorf("%s() arg %d: %w", node.Name, i, err)
			}
			vals[i] = v
			continue
		}
		vals[i] = raw
	}
	return EvaluateDateBuiltin(node.Name, vals)
}

// evalCollLiteralValue evaluates a literal value that may be a projection
// container (#2542 item 3). An engine ExpressionNode value evaluates against
// the current lambda scope; a map / array recurses per entry; any other scalar
// resolves through resolveLiteralValue. This backs a `select(g => {...})`
// object-literal projection whose values are per-group expressions.
func evalCollLiteralValue(v any, args, locals map[string]any) (any, error) {
	switch val := v.(type) {
	case ExpressionNode:
		return evalCollScalar(val, args, locals)
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, item := range val {
			ev, err := evalCollLiteralValue(item, args, locals)
			if err != nil {
				return nil, fmt.Errorf("projection value %q: %w", k, err)
			}
			out[k] = ev
		}
		return out, nil
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			ev, err := evalCollLiteralValue(item, args, locals)
			if err != nil {
				return nil, fmt.Errorf("projection element %d: %w", i, err)
			}
			out[i] = ev
		}
		return out, nil
	default:
		return resolveLiteralValue(v), nil
	}
}

// evalCollCond evaluates a positional cond(predicate, then, else) builtin
// (#2542 item 2) inside the collection-lambda subset. Each positional operand
// is an ExpressionNode ("0","1","2"); the predicate is evaluated for
// truthiness, then only the chosen branch is evaluated (short-circuit).
func evalCollCond(node *FunctionCallExpression, args, locals map[string]any) (any, error) {
	if len(node.Args) != 3 {
		return nil, fmt.Errorf("cond() requires three arguments: cond(predicate, thenValue, elseValue)")
	}
	operand := func(i int) (ExpressionNode, error) {
		raw, ok := node.Args[strconv.Itoa(i)]
		if !ok {
			return nil, fmt.Errorf("cond() is missing positional arg %d", i)
		}
		expr, ok := raw.(ExpressionNode)
		if !ok {
			return nil, fmt.Errorf("cond() arg %d is not an expression (%T)", i, raw)
		}
		return expr, nil
	}
	condExpr, err := operand(0)
	if err != nil {
		return nil, err
	}
	condVal, err := evalCollScalar(condExpr, args, locals)
	if err != nil {
		return nil, fmt.Errorf("cond() predicate: %w", err)
	}
	branch := 2
	if isTruthy(condVal) {
		branch = 1
	}
	chosen, err := operand(branch)
	if err != nil {
		return nil, err
	}
	return evalCollScalar(chosen, args, locals)
}

// resolveLiteralValue unwraps the engine literal wrapper, resolving the
// reserved clock node (`now`) to the current timestamp string.
func resolveLiteralValue(v any) any {
	if _, ok := v.(*languageAst.TimestampExprFunc); ok {
		return time.Now().UTC().Format(time.RFC3339)
	}
	return v
}

// resolveLambdaPath resolves a dotted path against the bound lambda
// scope. `m` returns the bound element; `m.active` indexes into it.
func resolveLambdaPath(name string, locals map[string]any) any {
	parts := strings.Split(strings.TrimSpace(name), ".")
	if len(parts) == 0 {
		return nil
	}
	cur, ok := locals[parts[0]]
	if !ok {
		return nil
	}
	return walkValuePath(cur, parts[1:])
}

func evalCollComparison(node *ComparisonExpression, args, locals map[string]any) (any, error) {
	lhs := walkLambdaFieldRef(node.Field, locals)
	rhs, err := evalCollComparisonValue(node.Value, args, locals)
	if err != nil {
		return nil, err
	}
	switch node.Operator {
	case OpEq:
		return runtimeCompareValues(lhs, rhs) == 0, nil
	case OpNe:
		return runtimeCompareValues(lhs, rhs) != 0, nil
	case OpGt:
		return runtimeCompareValues(lhs, rhs) > 0, nil
	case OpGe:
		return runtimeCompareValues(lhs, rhs) >= 0, nil
	case OpLt:
		return runtimeCompareValues(lhs, rhs) < 0, nil
	case OpLe:
		return runtimeCompareValues(lhs, rhs) <= 0, nil
	default:
		return nil, fmt.Errorf("operator %q is not supported inside a collection lambda", node.Operator)
	}
}

// evalCollBinaryComparison evaluates an EXPRESSION-LED comparison (#2542 item
// 5 residual): both sides are arbitrary expressions (arithmetic, a literal, a
// bound field, a call result). Evaluate each side through the collection
// subset, then apply the ordering/equality operator via the same
// runtimeCompareValues the Field-led path uses -- so `r.count - 10 > 0` and
// `0 < r.count` compare with identical numeric semantics.
func evalCollBinaryComparison(node *BinaryComparisonExpression, args, locals map[string]any) (any, error) {
	lhs, err := evalCollScalar(node.Left, args, locals)
	if err != nil {
		return nil, err
	}
	rhs, err := evalCollScalar(node.Right, args, locals)
	if err != nil {
		return nil, err
	}
	return EvaluateComparison(node.Operator, lhs, rhs)
}

// EvaluateComparison applies one of the six symbol comparison operators
// (== != < <= > >=) to two resolved operand values via runtimeCompareValues --
// the same numeric/string ordering the Field-led comparison path uses. Exported
// so the automations logic runner evaluates a re-parsed expression-led terminal
// return (`return (args.a - args.b) > 0`, #2542) with exactly the engine's
// semantics instead of re-implementing them -- the multi-step counterpart to the
// engine single-return BinaryComparisonExpression plan-root branch. The
// membership / nil operators are identifier-led only and never reach an
// expression-led comparison, so they surface here as a named error.
func EvaluateComparison(op ComparisonOperator, lhs, rhs any) (any, error) {
	switch op {
	case OpEq:
		return runtimeCompareValues(lhs, rhs) == 0, nil
	case OpNe:
		return runtimeCompareValues(lhs, rhs) != 0, nil
	case OpGt:
		return runtimeCompareValues(lhs, rhs) > 0, nil
	case OpGe:
		return runtimeCompareValues(lhs, rhs) >= 0, nil
	case OpLt:
		return runtimeCompareValues(lhs, rhs) < 0, nil
	case OpLe:
		return runtimeCompareValues(lhs, rhs) <= 0, nil
	default:
		return nil, fmt.Errorf("operator %q is not supported in an expression-led comparison", op)
	}
}

// walkLambdaFieldRef resolves a comparison LHS field reference against the
// lambda scope (`m.role` -> element["role"]).
func walkLambdaFieldRef(ref FieldReference, locals map[string]any) any {
	parts := ref.Parts
	if len(parts) == 0 {
		parts = strings.Split(ref.Raw, ".")
	}
	if len(parts) == 0 {
		return nil
	}
	cur, ok := locals[parts[0]]
	if !ok {
		return nil
	}
	return walkValuePath(cur, parts[1:])
}

func evalCollComparisonValue(value any, args, locals map[string]any) (any, error) {
	switch v := value.(type) {
	case ExpressionNode:
		return evalCollScalar(v, args, locals)
	case *ArgReference:
		if val, ok := getNestedValue(args, v.Path); ok {
			return val, nil
		}
		return nil, nil
	default:
		return value, nil
	}
}

// evalCollArithmetic evaluates a binary arithmetic node (#2316) inside the
// collection-lambda subset: evaluate both operands, then apply the numeric
// operator (int op int -> int64, any float -> float64; div/mod by zero error).
func evalCollArithmetic(node *ArithmeticExpression, args, locals map[string]any) (any, error) {
	lhs, err := evalCollScalar(node.Left, args, locals)
	if err != nil {
		return nil, err
	}
	rhs, err := evalCollScalar(node.Right, args, locals)
	if err != nil {
		return nil, err
	}
	return evalArithmetic(node.Op, lhs, rhs)
}

func evalCollLogical(node *LogicalExpression, args, locals map[string]any) (any, error) {
	lhs, err := evalCollScalar(node.Left, args, locals)
	if err != nil {
		return nil, err
	}
	switch node.Op {
	case LogicalAnd:
		if !isTruthy(lhs) {
			return false, nil
		}
		rhs, err := evalCollScalar(node.Right, args, locals)
		if err != nil {
			return nil, err
		}
		return isTruthy(rhs), nil
	case LogicalOr:
		if isTruthy(lhs) {
			return true, nil
		}
		rhs, err := evalCollScalar(node.Right, args, locals)
		if err != nil {
			return nil, err
		}
		return isTruthy(rhs), nil
	default:
		return nil, fmt.Errorf("unsupported logical operator %q inside a collection lambda", node.Op)
	}
}

// evalCollectionMethod evaluates the receiver to a collection then applies
// the named operator.
func evalCollectionMethod(node *CollectionMethodExpression, args, locals map[string]any) (any, error) {
	recv, err := evalCollScalar(node.Receiver, args, locals)
	if err != nil {
		return nil, err
	}
	coll := toCollection(recv)
	method := strings.ToLower(strings.TrimSpace(node.Method))

	// Split args into lambdas and value expressions.
	lambdaAt := func(i int) (*LambdaExpression, bool) {
		if i < len(node.Args) {
			if lam, ok := node.Args[i].(*LambdaExpression); ok {
				return lam, true
			}
		}
		return nil, false
	}

	applyLambda := func(lam *LambdaExpression, elem any) (any, error) {
		scope := cloneScope(locals)
		if len(lam.Params) > 0 {
			scope[lam.Params[0]] = elem
		}
		return evalCollScalar(lam.Body, args, scope)
	}

	switch method {
	case "where":
		lam, ok := lambdaAt(0)
		if !ok {
			return nil, fmt.Errorf("where() requires a lambda argument")
		}
		out := make([]any, 0, len(coll))
		for _, e := range coll {
			v, err := applyLambda(lam, e)
			if err != nil {
				return nil, err
			}
			if isTruthy(v) {
				out = append(out, e)
			}
		}
		return out, nil

	case "select":
		lam, ok := lambdaAt(0)
		if !ok {
			return nil, fmt.Errorf("select() requires a lambda argument")
		}
		out := make([]any, 0, len(coll))
		for _, e := range coll {
			v, err := applyLambda(lam, e)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil

	case "first":
		if lam, ok := lambdaAt(0); ok {
			for _, e := range coll {
				v, err := applyLambda(lam, e)
				if err != nil {
					return nil, err
				}
				if isTruthy(v) {
					return e, nil
				}
			}
			return nil, nil
		}
		if len(coll) > 0 {
			return coll[0], nil
		}
		return nil, nil

	case "last":
		if len(coll) > 0 {
			return coll[len(coll)-1], nil
		}
		return nil, nil

	case "single":
		var match []any
		if lam, ok := lambdaAt(0); ok {
			for _, e := range coll {
				v, err := applyLambda(lam, e)
				if err != nil {
					return nil, err
				}
				if isTruthy(v) {
					match = append(match, e)
				}
			}
		} else {
			match = coll
		}
		if len(match) != 1 {
			return nil, fmt.Errorf("single() expected exactly one matching element, got %d", len(match))
		}
		return match[0], nil

	case "any":
		if lam, ok := lambdaAt(0); ok {
			for _, e := range coll {
				v, err := applyLambda(lam, e)
				if err != nil {
					return nil, err
				}
				if isTruthy(v) {
					return true, nil
				}
			}
			return false, nil
		}
		return len(coll) > 0, nil

	case "all":
		lam, ok := lambdaAt(0)
		if !ok {
			return nil, fmt.Errorf("all() requires a lambda argument")
		}
		for _, e := range coll {
			v, err := applyLambda(lam, e)
			if err != nil {
				return nil, err
			}
			if !isTruthy(v) {
				return false, nil
			}
		}
		return true, nil

	case "count":
		if lam, ok := lambdaAt(0); ok {
			n := 0
			for _, e := range coll {
				v, err := applyLambda(lam, e)
				if err != nil {
					return nil, err
				}
				if isTruthy(v) {
					n++
				}
			}
			return n, nil
		}
		return len(coll), nil

	case "sum", "min", "max", "avg":
		lam, ok := lambdaAt(0)
		if !ok {
			return nil, fmt.Errorf("%s() requires a lambda argument", method)
		}
		return aggregateNumeric(method, coll, applyLambda, lam)

	case "orderby", "orderbydesc":
		lam, ok := lambdaAt(0)
		if !ok {
			return nil, fmt.Errorf("%s() requires a lambda argument", method)
		}
		return orderByLambda(coll, applyLambda, lam, method == "orderbydesc")

	case "take", "skip":
		n, err := evalIntArg(node, 0, args, locals)
		if err != nil {
			return nil, err
		}
		if method == "take" {
			if n < 0 {
				n = 0
			}
			if n > len(coll) {
				n = len(coll)
			}
			return append([]any(nil), coll[:n]...), nil
		}
		if n < 0 {
			n = 0
		}
		if n > len(coll) {
			n = len(coll)
		}
		return append([]any(nil), coll[n:]...), nil

	case "distinct":
		lam, hasLam := lambdaAt(0)
		seen := make(map[string]bool)
		out := make([]any, 0, len(coll))
		for _, e := range coll {
			var key any = e
			if hasLam {
				k, err := applyLambda(lam, e)
				if err != nil {
					return nil, err
				}
				key = k
			}
			ks := fmt.Sprintf("%v", key)
			if seen[ks] {
				continue
			}
			seen[ks] = true
			out = append(out, e)
		}
		return out, nil

	case "groupby":
		lam, ok := lambdaAt(0)
		if !ok {
			return nil, fmt.Errorf("groupBy() requires a lambda argument")
		}
		return groupByLambda(coll, applyLambda, lam)

	case "reduce":
		// reduce(seed, (acc, x) => <expr>)
		if len(node.Args) < 2 {
			return nil, fmt.Errorf("reduce() requires a seed and a 2-parameter lambda")
		}
		seed, err := evalCollScalar(node.Args[0], args, locals)
		if err != nil {
			return nil, err
		}
		lam, ok := node.Args[1].(*LambdaExpression)
		if !ok || len(lam.Params) < 2 {
			return nil, fmt.Errorf("reduce() second argument must be a 2-parameter lambda (acc, x)")
		}
		acc := seed
		for _, e := range coll {
			scope := cloneScope(locals)
			scope[lam.Params[0]] = acc
			scope[lam.Params[1]] = e
			v, err := evalCollScalar(lam.Body, args, scope)
			if err != nil {
				return nil, err
			}
			acc = v
		}
		return acc, nil

	case "empty":
		return len(coll) == 0, nil

	case "contains":
		if len(node.Args) < 1 {
			return nil, fmt.Errorf("contains() requires a value argument")
		}
		target, err := evalCollScalar(node.Args[0], args, locals)
		if err != nil {
			return nil, err
		}
		for _, e := range coll {
			if runtimeCompareValues(e, target) == 0 {
				return true, nil
			}
		}
		return false, nil

	default:
		return nil, fmt.Errorf("unknown collection method %q", node.Method)
	}
}

type lambdaApplier func(lam *LambdaExpression, elem any) (any, error)

func aggregateNumeric(method string, coll []any, apply lambdaApplier, lam *LambdaExpression) (any, error) {
	if len(coll) == 0 {
		if method == "sum" {
			return float64(0), nil
		}
		return nil, nil
	}
	var sum float64
	var best float64
	bestSet := false
	count := 0
	for _, e := range coll {
		v, err := apply(lam, e)
		if err != nil {
			return nil, err
		}
		f, ok := runtimeToFloat64(v)
		if !ok {
			return nil, fmt.Errorf("%s() lambda must return a number, got %T", method, v)
		}
		sum += f
		count++
		switch method {
		case "min":
			if !bestSet || f < best {
				best = f
				bestSet = true
			}
		case "max":
			if !bestSet || f > best {
				best = f
				bestSet = true
			}
		}
	}
	switch method {
	case "sum":
		return sum, nil
	case "avg":
		if count == 0 {
			return nil, nil
		}
		return sum / float64(count), nil
	default: // min/max
		return best, nil
	}
}

func orderByLambda(coll []any, apply lambdaApplier, lam *LambdaExpression, desc bool) (any, error) {
	type keyed struct {
		key  any
		elem any
	}
	items := make([]keyed, len(coll))
	for i, e := range coll {
		k, err := apply(lam, e)
		if err != nil {
			return nil, err
		}
		items[i] = keyed{key: k, elem: e}
	}
	sort.SliceStable(items, func(i, j int) bool {
		c := runtimeCompareValues(items[i].key, items[j].key)
		if desc {
			return c > 0
		}
		return c < 0
	})
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = it.elem
	}
	return out, nil
}

func groupByLambda(coll []any, apply lambdaApplier, lam *LambdaExpression) (any, error) {
	order := []string{}
	groups := map[string][]any{}
	keys := map[string]any{}
	for _, e := range coll {
		k, err := apply(lam, e)
		if err != nil {
			return nil, err
		}
		ks := fmt.Sprintf("%v", k)
		if _, ok := groups[ks]; !ok {
			order = append(order, ks)
			keys[ks] = k
		}
		groups[ks] = append(groups[ks], e)
	}
	out := make([]any, 0, len(order))
	for _, ks := range order {
		out = append(out, map[string]any{"key": keys[ks], "items": groups[ks]})
	}
	return out, nil
}

// evalIntArg evaluates the i-th method argument to an int (for take/skip).
func evalIntArg(node *CollectionMethodExpression, i int, args, locals map[string]any) (int, error) {
	if i >= len(node.Args) {
		return 0, fmt.Errorf("%s() requires an integer argument", node.Method)
	}
	v, err := evalCollScalar(node.Args[i], args, locals)
	if err != nil {
		return 0, err
	}
	f, ok := runtimeToFloat64(v)
	if !ok {
		return 0, fmt.Errorf("%s() argument must be an integer, got %T", node.Method, v)
	}
	// Narrow to int through a GUARDED int64 sink to satisfy the
	// integer-conversion analyzer (go/incorrect-integer-conversion): the value
	// can flow from a parsed int64 and `int` is platform-width, so the int()
	// conversion's source must be a bounded int64. Clamp to the int32 range
	// (take/skip counts are small; the caller further clamps to [0, len(coll)]).
	n := int64(f)
	if n > math.MaxInt32 {
		n = math.MaxInt32
	} else if n < math.MinInt32 {
		n = math.MinInt32
	}
	return int(n), nil
}

func cloneScope(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// toCollection coerces a receiver value into a []any. It handles []any,
// []map[string]any, a single map (wrapped as one element), and nil (empty).
func toCollection(v any) []any {
	switch c := v.(type) {
	case nil:
		return nil
	case []any:
		return c
	case []map[string]any:
		out := make([]any, len(c))
		for i := range c {
			out[i] = c[i]
		}
		return out
	case map[string]any:
		// A nodes/bundle envelope: prefer an explicit "nodes" list when present.
		if nodes, ok := c["nodes"]; ok {
			return toCollection(nodes)
		}
		return []any{c}
	default:
		return []any{v}
	}
}
