package memql

import (
	"context"
	"fmt"
	"strings"
)

// expression_evaluator.go holds the in-process expression evaluator
// that backs context-spec evaluation (engine.EvaluateSpec). It walks
// the engine ExpressionNode AST and produces a plain Go value,
// resolving payload / actor / args field references against the
// effective ctx envelope buildSpecCtx assembles.
//
// This evaluator was previously co-located with the decision-policy
// tier (the retired `func (Policy)` runtime, #984). When that tier was
// removed, the shared expression-walk + truthiness + partition helpers
// the live spec evaluator depends on were lifted here. The decision-
// policy-only surface (`policy(...)` sub-calls, trace capture, cache,
// tier checks) is gone; what remains is the minimal predicate
// evaluator a context-spec needs.

// currentPartitionFromContext resolves the active partition for the
// spec ctx envelope. The partition dimension is retired (#56), so this
// is a stable empty string; kept as a single seam in case a future
// caller threads the active partition through call options.
func currentPartitionFromContext(ctx context.Context) string {
	_ = ctx
	return ""
}

// evaluateSpecExpression walks the engine ExpressionNode AST of a
// context-spec body and produces a plain Go value, resolving
// ArgReference / ActorReference against the effective ctx. The
// supported surface is intentionally small — atomic boolean
// predicates (comparisons, &&/||, and spec(...) composition).
func evaluateSpecExpression(goCtx context.Context, engine *MemQLEngine, expr ExpressionNode, ctx map[string]any) (any, error) {
	if expr == nil {
		return nil, nil
	}
	switch node := expr.(type) {
	case *ComparisonExpression:
		return evaluateSpecComparison(node, ctx)
	case *LogicalExpression:
		return evaluateSpecLogical(goCtx, engine, node, ctx)
	case *ArithmeticExpression:
		return evaluateSpecArithmetic(goCtx, engine, node, ctx)
	case *FunctionCallExpression:
		return evaluateSpecFunctionCall(goCtx, engine, node, ctx)
	case *LiteralValueNode:
		// A literal in a spec body (e.g. an arg reference substituted to a
		// concrete value, or a bare constant) evaluates to its value; the
		// caller applies specTruthy for the boolean fold (#1705).
		return node.Value, nil
	case *BuiltinFunctionExpression:
		return nil, fmt.Errorf("builtin function %q is not evaluable inside a spec body", node.Name)
	}
	return nil, fmt.Errorf("unsupported expression type %T in spec body", expr)
}

// evaluateSpecFunctionCall handles function-call-shaped expressions
// inside a spec body. The only callable is `spec("<name>")` —
// context-spec composition, which routes through engine.EvaluateSpec
// (row-specs are rejected there). The retired `policy(...)` call
// (#984) is no longer admitted.
func evaluateSpecFunctionCall(goCtx context.Context, engine *MemQLEngine, call *FunctionCallExpression, ctx map[string]any) (any, error) {
	if call == nil {
		return nil, nil
	}
	name := strings.ToLower(strings.TrimSpace(call.Name))
	switch name {
	case "spec":
		return evaluateSpecSubCall(goCtx, engine, call)
	case "policy":
		return nil, fmt.Errorf("policy() is retired (#984); name the context-spec as a bare conjunct (`... && requiresAdmin`) for caller-context boolean checks")
	default:
		return nil, fmt.Errorf("function call %q is not callable from a spec body (expected `spec(...)`)", call.Name)
	}
}

// evaluateSpecSubCall handles `spec("name")` inside a spec body.
// Routes to engine.EvaluateSpec which enforces that the named spec is
// a context-spec (row-specs belong in query filters). Returns the
// spec's boolean result.
func evaluateSpecSubCall(goCtx context.Context, engine *MemQLEngine, call *FunctionCallExpression) (any, error) {
	if engine == nil {
		return nil, fmt.Errorf("spec() builtin requires an engine context; call via engine.EvaluateSpec(...)")
	}
	rawName, ok := call.Args["0"]
	if !ok {
		return nil, fmt.Errorf("spec() requires the spec name as the first argument")
	}
	name, ok := rawName.(string)
	if !ok {
		return nil, fmt.Errorf("spec() first argument must be a string, got %T", rawName)
	}
	return engine.EvaluateSpec(goCtx, name)
}

// evaluateSpecArithmetic evaluates a binary arithmetic node (#2316) in the
// in-process expression evaluator that backs the logic/spec path. Arithmetic
// is rejected in specs at load (the default AST converter), so this is the
// logic-body backstop -- it evaluates both operands and applies the numeric
// operator.
func evaluateSpecArithmetic(goCtx context.Context, engine *MemQLEngine, expr *ArithmeticExpression, ctx map[string]any) (any, error) {
	if expr == nil {
		return nil, nil
	}
	lhs, err := evaluateSpecExpression(goCtx, engine, expr.Left, ctx)
	if err != nil {
		return nil, err
	}
	rhs, err := evaluateSpecExpression(goCtx, engine, expr.Right, ctx)
	if err != nil {
		return nil, err
	}
	return evalArithmetic(expr.Op, lhs, rhs)
}

func evaluateSpecLogical(goCtx context.Context, engine *MemQLEngine, expr *LogicalExpression, ctx map[string]any) (any, error) {
	if expr == nil {
		return nil, nil
	}
	lhs, err := evaluateSpecExpression(goCtx, engine, expr.Left, ctx)
	if err != nil {
		return nil, err
	}
	if expr.Op == LogicalAnd {
		if !specTruthy(lhs) {
			return false, nil
		}
		rhs, err := evaluateSpecExpression(goCtx, engine, expr.Right, ctx)
		if err != nil {
			return nil, err
		}
		return specTruthy(rhs), nil
	}
	if expr.Op == LogicalOr {
		if specTruthy(lhs) {
			return true, nil
		}
		rhs, err := evaluateSpecExpression(goCtx, engine, expr.Right, ctx)
		if err != nil {
			return nil, err
		}
		return specTruthy(rhs), nil
	}
	return nil, fmt.Errorf("unsupported logical operator %v in spec body", expr.Op)
}

func evaluateSpecComparison(expr *ComparisonExpression, ctx map[string]any) (any, error) {
	if expr == nil {
		return nil, nil
	}
	lhs, err := evaluateSpecFieldRef(expr.Field, ctx)
	if err != nil {
		return nil, err
	}
	rhs, err := evaluateSpecValue(expr.Value, ctx)
	if err != nil {
		return nil, err
	}
	switch expr.Operator {
	case OpEq:
		return specEqual(lhs, rhs), nil
	case OpNe:
		return !specEqual(lhs, rhs), nil
	}
	return nil, fmt.Errorf("operator %v not supported in spec bodies yet", expr.Operator)
}

func evaluateSpecFieldRef(ref FieldReference, ctx map[string]any) (any, error) {
	if len(ref.Parts) == 0 {
		return nil, fmt.Errorf("empty field reference in spec body")
	}
	first := strings.ToLower(strings.TrimSpace(ref.Parts[0]))
	switch first {
	case "payload", "ctx", "args":
		// The parser normalises ctx → payload in field-reference
		// position; `args` is the author-facing form. All three
		// resolve to the spec ctx.
		return lookupPath(ctx, ref.Parts[1:]), nil
	case "actor":
		actor, _ := ctx["actor"].(map[string]any)
		if actor == nil {
			return nil, nil
		}
		return lookupPath(actor, ref.Parts[1:]), nil
	case "caller":
		return nil, fmt.Errorf("caller.X is retired (#221) -- use actor.X (the same auth-context envelope, one canonical spelling)")
	}
	return nil, fmt.Errorf("field reference %q not supported in spec body", ref.Raw)
}

func evaluateSpecValue(value any, ctx map[string]any) (any, error) {
	switch v := value.(type) {
	case *ArgReference:
		return lookupPath(ctx, strings.Split(v.Path, ".")), nil
	case *ActorReference:
		actor, _ := ctx["actor"].(map[string]any)
		if actor == nil {
			return nil, nil
		}
		return lookupPath(actor, strings.Split(v.Path, ".")), nil
	}
	return value, nil
}

// lookupPath walks a dotted path against the ctx envelope. The
// resolver supports the canonical form only:
//   - `args.X` (lowered to ArgReference{Path: "X"}) walks the
//     top-level args mirror via walkValuePath.
//   - `actor.X` / `now` / `partition` / `config.X` walk through the
//     engine-owned reserved fields at the top level.
func lookupPath(root map[string]any, parts []string) any {
	if root == nil || len(parts) == 0 {
		return nil
	}
	return walkValuePath(any(root), parts)
}

// walkValuePath descends parts segments into a value (root may be a
// map; intermediate steps must also be maps). Returns nil on a miss.
func walkValuePath(root any, parts []string) any {
	var current = root
	for _, p := range parts {
		seg := strings.TrimSpace(p)
		if seg == "" {
			return nil
		}
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[seg]
	}
	return current
}

func specEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == b
	}
	// Stringify both sides for the common cross-type comparison
	// (role enums vs strings, ints vs strings) and compare.
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func specTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return strings.TrimSpace(t) != ""
	case int, int32, int64, float32, float64:
		return fmt.Sprintf("%v", t) != "0"
	}
	return true
}
