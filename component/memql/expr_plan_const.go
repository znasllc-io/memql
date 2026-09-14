package memql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/language/ast"
)

// expr_plan_const.go -- PlanConstExpression, a row-independent edition-2026
// subexpression inside a pushdown predicate (epic memql#5363, task memql#5366,
// design record D11).
//
// # What a plan constant is
//
// In a pushdown (P-tier) position -- a query filter, a spec body -- a
// subexpression that does not read the row parameter has ONE value for the
// whole scan. `row.expiresAt < addDuration(now, "P1D")` compares every row
// against the same instant; `args.x == nil || row.f == args.x` has a left
// operand that is true or false for the entire call. Lowering such a subtree
// to SQL would mean teaching the SQL compiler every in-process function
// (`addDuration`, `lower`, `? :`, ...), which is the second evaluator this epic
// exists to delete. So the lowering (Lower, task 6 part B) leaves it as this
// leaf, carrying the v1 AST verbatim, and it is EVALUATED ONCE PER CALL by the
// in-process evaluator before the query runs, then replaced by what it
// evaluated to. The SQL compiler therefore never sees one: by the time a tree
// reaches tryCompileCombinedFilter every plan constant is a literal or a
// constant boolean, and one that is not is a bug this file names rather than a
// query that returns the wrong rows (errPlanConstantUnevaluated).
//
// # Where it is replaced: argument expansion, and why there
//
// expandExpressionWithArgs is the first point at which a call's arguments are
// known, and it already folds `args.X` references and the caller-flag
// comparison (memql#4814) there. A plan constant is the general form of both,
// so it is replaced at the same altitude, against the same argument map, with
// the ambient envelope (actor / config / now) the validator already carries
// for memql#3024. One consequence is load-bearing for the result cache: the
// folded value becomes part of plan.Root BEFORE planCacheSignature reads it,
// so two callers whose plan constants evaluate differently cannot share an
// entry (see canonicalExpression).
//
// # The evaluator is a hook, and nil is a refusal
//
// The in-process evaluator (EvalExpr, task 5) lands on a sibling branch, so
// this file reaches it through planConstantEvaluator, which the coordinator
// wires. Until it is wired, expanding a plan constant REFUSES
// (errPlanConstantsNeedEvaluator) rather than guessing: there is no safe
// default value for an arbitrary expression, and a guessed boolean in a filter
// is a gate that is open or closed by accident -- the memql#2962 shape.

// PlanConstExpression is a row-independent v1 subexpression in a pushdown
// predicate. It appears in two positions and is replaced by argument expansion
// in both:
//
//   - PREDICATE position (anywhere a boolean node goes): it must evaluate to a
//     bool, and becomes a constantBoolExpression carrying that value;
//   - a ComparisonExpression's VALUE: its result becomes the comparison value,
//     with an absent (nil) result decided by the absence table
//     (foldPlanConstantComparison).
//
// Expr is the parsed v1 AST, never mutated after parsing, so clones share it.
type PlanConstExpression struct {
	Expr ast.ExpressionNode
}

func (*PlanConstExpression) isExpressionNode() {}

// planConstantEvaluator evaluates a plan constant in process. Nil until the
// coordinator wires the edition-2026 evaluator (EvalExpr); expansion refuses a
// plan constant while it is nil.
//
// bindings carries `args` (the call's argument map, when the expansion is
// inside a call) and whichever of `actor`, `config` and `now` the expansion
// holds -- the ambient envelope's own values, so a plan constant reads exactly
// what a context-spec reads (#2623: one envelope, not two).
//
// A package variable rather than a field on the engine because argument
// expansion is ctx-free and engine-free by design (functionValidator); the
// ctx parameter exists for the evaluator's own use and expansion passes
// context.Background(), since every value it could need is already in
// bindings.
var planConstantEvaluator func(ctx context.Context, n ast.ExpressionNode, bindings map[string]any) (any, error)

// errPlanConstantsNeedEvaluator is the refusal while planConstantEvaluator is
// unwired.
var errPlanConstantsNeedEvaluator = errors.New("plan constants need the in-process evaluator")

// planConstantAmbientRoots is the part of the ambient envelope a plan constant
// is bound to. `partition` is carried by the envelope but is not an edition
// 2026 root (the dimension is retired, #56), so it is not handed on.
var planConstantAmbientRoots = []string{"actor", "config", "now"}

// planConstantBindings assembles what a plan constant may read. args is bound
// only when non-nil: a nil map means the expansion is not inside a call (the
// plan root itself), and a plan constant reading `args` there is a bug the
// evaluator should report as an unknown name rather than one this function
// hides behind an empty map.
func planConstantBindings(args map[string]any, ambient map[string]any) map[string]any {
	bindings := make(map[string]any, 1+len(planConstantAmbientRoots))
	if args != nil {
		bindings["args"] = args
	}
	for _, root := range planConstantAmbientRoots {
		if value, ok := ambient[root]; ok {
			bindings[root] = value
		}
	}
	return bindings
}

// formatPlanConstant renders a plan constant's source for messages and for the
// canonical signature.
func formatPlanConstant(pc *PlanConstExpression) string {
	if pc == nil || pc.Expr == nil {
		return "<empty>"
	}
	return ast.FormatExpr(pc.Expr)
}

// evaluatePlanConstant runs one plan constant through the hook.
func evaluatePlanConstant(ctx context.Context, pc *PlanConstExpression, bindings map[string]any) (any, error) {
	if pc == nil || pc.Expr == nil {
		return nil, fmt.Errorf("plan constant has no expression")
	}
	evaluator := planConstantEvaluator
	if evaluator == nil {
		return nil, fmt.Errorf("plan constant %s: %w", formatPlanConstant(pc), errPlanConstantsNeedEvaluator)
	}
	value, err := evaluator(ctx, pc.Expr, bindings)
	if err != nil {
		return nil, fmt.Errorf("plan constant %s: %w", formatPlanConstant(pc), err)
	}
	return value, nil
}

// errPlanConstantUnevaluated is what every executor-side arm returns for a plan
// constant that reached it. It cannot be compiled (there is nothing to compile
// -- the value was never computed) and it must not be guessed.
func errPlanConstantUnevaluated(pc *PlanConstExpression) error {
	return fmt.Errorf("a plan constant (%s) reached the executor unevaluated: plan constants are replaced during "+
		"argument expansion, so this tree was built on a path that never expanded it", formatPlanConstant(pc))
}

// planConstantPredicate is the predicate-position replacement rule: a bool
// becomes a constant, anything else refuses. No truthiness (D8): a string, a
// number or nil in a condition is a type error, and folding it to a boolean by
// some rule would make `args.status && row.x == 1` mean something nobody wrote.
func planConstantPredicate(pc *PlanConstExpression, value any) (ExpressionNode, error) {
	b, ok := value.(bool)
	if !ok {
		return nil, fmt.Errorf("a plan constant in a condition must be boolean, got %s (%s)",
			planConstantTypeName(value), formatPlanConstant(pc))
	}
	return &constantBoolExpression{value: b, planConstant: true}, nil
}

// planConstantTypeName names a value in the language's vocabulary for the
// refusal above.
func planConstantTypeName(value any) string {
	switch value.(type) {
	case nil:
		return "nil"
	case string:
		return "string"
	case bool:
		return "bool"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return "number"
	case []any, []string:
		return "list"
	case map[string]any:
		return "map"
	default:
		return fmt.Sprintf("%T", value)
	}
}

// normalizePlanConstantValue turns an evaluated plan constant into the value
// shapes the comparison compilers and compareScalarValues accept.
//
// Every integer width is widened to int64 (and float32 to float64) once, here,
// so a plan constant binds, compares and renders as one value whatever width
// the evaluator happened to produce it at: the canonical signature prints an
// int64 and an int identically only by accident of fmt, and a signature is not
// the place to rely on an accident. A time is rendered the way `now` is
// (RFC3339Nano, UTC), because that is the representation stored rows carry
// and the one string ordering is correct for.
func normalizePlanConstantValue(value any) (any, error) {
	switch v := value.(type) {
	case nil, string, bool, int64, float64:
		return v, nil
	case int:
		return int64(v), nil
	case int8:
		return int64(v), nil
	case int16:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case uint8:
		return int64(v), nil
	case uint16:
		return int64(v), nil
	case uint32:
		return int64(v), nil
	case uint:
		if uint64(v) > math.MaxInt64 {
			return float64(v), nil
		}
		return int64(v), nil
	case uint64:
		if v > math.MaxInt64 {
			return float64(v), nil
		}
		return int64(v), nil
	case float32:
		return float64(v), nil
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, nil
		}
		f, err := v.Float64()
		if err != nil {
			return nil, fmt.Errorf("number %q does not parse: %w", v.String(), err)
		}
		return f, nil
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano), nil
	case []string:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i := range v {
			item, err := normalizePlanConstantValue(v[i])
			if err != nil {
				return nil, err
			}
			out[i] = item
		}
		return out, nil
	case map[string]any:
		return nil, fmt.Errorf("it evaluated to a map; a comparison needs a scalar or a list")
	default:
		return nil, fmt.Errorf("it evaluated to an unsupported value %T", value)
	}
}

// foldPlanConstantComparison replaces a comparison whose VALUE was a plan
// constant, now that the constant evaluated to value.
//
// Three shapes need more than "put the value in":
//
//   - nil. The value is UNSET, and the one notion of unset decides (nil, a
//     missing key, JSON null and "" are one value to ==, != and in): `x ==
//     <unset>` is `x == nil` and `x != <unset>` is `x != nil`, so they become
//     OpMissing / OpNotMissing; `<unset> in row.<array>` looks for an unset
//     element. An ordering or a prefix test against nothing is false; `in` an
//     absent LIST has no members and is false; and the legacy `not in` an
//     absent list is "set and not a member of nothing", which is `x != nil`.
//     Handing nil to the compilers instead would fail the read with
//     "unsupported literal type <nil>", which for `args.x` left optional is
//     the ordinary case, not an error.
//   - an empty list for `in` / `not in`. There is nothing to compile it to
//     (normalizeCollectionValues refuses an empty list), so it decides the
//     comparison the same way an absent list does. A list's nil and "" members
//     are NOT dropped: they are unset members, which admit the unset rows.
//   - an intrinsic column. id, concept, type, createdAt and createdBy are NOT
//     NULL, so they are never absent: `== nil` is false and `!= nil` is true
//     outright, where OpMissing would not compile against a column at all.
//
// Everything else re-enters expansion with the literal in place, so the
// comparison gets exactly the treatment an authored literal gets.
func (v *functionValidator) foldPlanConstantComparison(node *ComparisonExpression, value any, args map[string]any) (ExpressionNode, error) {
	normalized, err := normalizePlanConstantValue(value)
	if err != nil {
		return nil, fmt.Errorf("plan constant compared with %s: %w", planConstantFieldLabel(node.Field), err)
	}

	if normalized == nil {
		folded, handled, err := absentPlanConstantComparison(node)
		if err != nil {
			return nil, err
		}
		if handled {
			return folded, nil
		}
	}

	if list, ok := normalized.([]any); ok && len(list) == 0 && (node.Operator == OpIn || node.Operator == OpOut) {
		folded, handled, err := absentPlanConstantComparison(node)
		if err != nil {
			return nil, err
		}
		if handled {
			return folded, nil
		}
	}

	clone := cloneExpressionNode(node).(*ComparisonExpression)
	clone.Value = normalized
	expanded, err := v.expandExpressionWithArgs(clone, args)
	if err != nil {
		return nil, err
	}
	// A comparison whose FIELD is also row-independent (an `args.` flag)
	// folds to a constant on re-entry; that constant came from a plan
	// constant, so it is marked as one and the logical operators around it
	// may fold over it. Fresh node from the fold above, so marking it in
	// place mutates nothing shared.
	if c, ok := expanded.(*constantBoolExpression); ok {
		c.planConstant = true
	}
	return expanded, nil
}

// absentPlanConstantComparison applies the one notion of unset to a comparison
// whose plan-constant value evaluated to nil (or, for `in` / `not in`, to an
// empty list). handled=false means the field is not a row field (an `args.` /
// `actor.` operand) and the caller keeps the ordinary path; Lower never
// produces that shape, since such a comparison is row-independent as a whole
// and would itself be one plan constant.
func absentPlanConstantComparison(node *ComparisonExpression) (ExpressionNode, bool, error) {
	rewrite := func(op ComparisonOperator) ExpressionNode {
		rewritten := cloneExpressionNode(node).(*ComparisonExpression)
		rewritten.Operator = op
		rewritten.Value = nil
		return rewritten
	}
	switch planConstantFieldKind(node.Field) {
	case planConstFieldAbsentable:
		switch node.Operator {
		case OpEq, OpMissing:
			return rewrite(OpMissing), true, nil
		case OpNe, OpNotMissing, OpOut:
			// `not in` nothing is "set and not a member of nothing".
			return rewrite(OpNotMissing), true, nil
		case OpHas:
			// An unset needle is `==` to an unset element; the compilers
			// take a nil needle as exactly that.
			return rewrite(OpHas), true, nil
		default:
			return &constantBoolExpression{value: false, planConstant: true}, true, nil
		}
	case planConstFieldColumn:
		// A NOT NULL column is never absent.
		switch node.Operator {
		case OpNe, OpNotMissing, OpOut:
			return &constantBoolExpression{value: true, planConstant: true}, true, nil
		default:
			return &constantBoolExpression{value: false, planConstant: true}, true, nil
		}
	case planConstFieldProvenance:
		// A provenance leaf CAN be absent, and neither the SQL compiler nor
		// the in-process arm has an absent comparison for one -- so there is
		// no rewrite that both halves would agree on. Refused by name.
		return nil, true, fmt.Errorf("plan constant compared with %s evaluated to nil, and a provenance leaf has no "+
			"absent comparison in the pushdown -- guard the comparison with `<value> == nil ||`", planConstantFieldLabel(node.Field))
	default:
		return nil, false, nil
	}
}

// planConstFieldKind classifies a comparison field for the absent rewrite.
type planConstFieldKind int

const (
	planConstFieldOther      planConstFieldKind = iota // args. / actor. / config. ...: not a row field
	planConstFieldAbsentable                           // a payload property or an array element: may be absent
	planConstFieldColumn                               // a NOT NULL row column: never absent
	planConstFieldProvenance                           // a provenance leaf: may be absent, no absent comparison
)

// planConstantFieldKind reads a field as expansion sees it. Expansion runs
// BEFORE rewriteFilterFieldRefs, so a legacy tree may still carry a bare
// payload property or a `row.<intrinsic>` spelling; the lowering emits the
// final forms (`payload.<f>`, a bare canonical intrinsic, the element root).
// All of them are classified here.
func planConstantFieldKind(field FieldReference) planConstFieldKind {
	if len(field.Parts) == 0 {
		return planConstFieldOther
	}
	head := strings.TrimSpace(field.Parts[0])
	if isArrayElementField(field) || strings.EqualFold(head, "payload") {
		return planConstFieldAbsentable
	}
	intrinsic := head
	if strings.EqualFold(head, rowIntrinsicNamespace) {
		if len(field.Parts) < 2 {
			return planConstFieldOther
		}
		intrinsic = strings.TrimSpace(field.Parts[1])
	}
	if info, ok := resolveIntrinsicField(intrinsic); ok {
		if info.kind == intrinsicFieldProvenance {
			return planConstFieldProvenance
		}
		return planConstFieldColumn
	}
	if reservedFilterHead(head) {
		return planConstFieldOther
	}
	// A bare, unreserved name is a payload property the bare-access rewrite
	// has not prefixed yet.
	return planConstFieldAbsentable
}

// planConstantFieldLabel names a field for a message.
func planConstantFieldLabel(field FieldReference) string {
	if raw := strings.TrimSpace(field.Raw); raw != "" {
		return raw
	}
	return strings.Join(field.Parts, ".")
}

// foldPlanConstantCount replaces a collection count's plan-constant operand.
// A count is never absent -- an absent array counts 0 -- so against an absent
// operand `==` and every ordering are false and `!=` is true.
func foldPlanConstantCount(n *ArrayPredicateExpression, value any) (ExpressionNode, error) {
	normalized, err := normalizePlanConstantValue(value)
	if err != nil {
		return nil, fmt.Errorf("plan constant compared with %s.count(): %w", planConstantFieldLabel(n.Field), err)
	}
	if normalized == nil {
		return &constantBoolExpression{value: n.CountOp == OpNe, planConstant: true}, nil
	}
	out := *n
	out.CountValue = normalized
	return &out, nil
}

// planConstantDecides reports whether an expanded LEFT operand decides op on
// its own: FALSE && x is FALSE and TRUE || x is TRUE whatever x is.
//
// The caller uses it to NOT EXPAND the right operand, and that is the point
// rather than an optimisation: the edition-2026 guard `args.x == nil ||
// row.f == args.x` is written so that the right side is only meaningful when
// the left is false, and expanding it with `args.x` absent would fail the call
// ("required argument not provided") on exactly the input the guard exists to
// admit. Only constants that came from plan constants decide: the older
// caller-flag constants (memql#4814) keep their tree shape, which
// TestFilterArgReferenceOnTheLeftHandSideIsBound pins, and folding them is
// behaviour-neutral but not asked for.
func planConstantDecides(op LogicalOp, left ExpressionNode) (ExpressionNode, bool) {
	c, ok := left.(*constantBoolExpression)
	if !ok || c == nil || !c.planConstant {
		return nil, false
	}
	if (op == LogicalAnd && !c.value) || (op == LogicalOr && c.value) {
		return c, true
	}
	return nil, false
}

// foldPlanConstantLogical applies the four identities once both operands are
// expanded: TRUE && x -> x, x && TRUE -> x, x && FALSE -> FALSE, FALSE || x ->
// x, x || FALSE -> x, x || TRUE -> TRUE. So `args.x == nil || row.f == args.x`
// reaches SQL either as TRUE or as the single comparison, never as a
// disjunction with a constant in it. The deciding left-operand cases are
// planConstantDecides' and never reach here.
func foldPlanConstantLogical(op LogicalOp, left, right ExpressionNode) (ExpressionNode, bool) {
	if c, ok := left.(*constantBoolExpression); ok && c != nil && c.planConstant {
		if (op == LogicalAnd && c.value) || (op == LogicalOr && !c.value) {
			return right, true
		}
	}
	if c, ok := right.(*constantBoolExpression); ok && c != nil && c.planConstant {
		switch {
		case op == LogicalAnd && c.value:
			return left, true
		case op == LogicalAnd && !c.value:
			return c, true
		case op == LogicalOr && c.value:
			return c, true
		case op == LogicalOr && !c.value:
			return left, true
		}
	}
	return nil, false
}

// treeHasPlanConstant reports whether an IR tree carries a plan constant in
// any position. It walks every node kind that can hold a predicate or a
// comparison value; a directive wrapper is walked for its target.
func treeHasPlanConstant(expr ExpressionNode) bool {
	switch n := expr.(type) {
	case nil:
		return false
	case *PlanConstExpression:
		return true
	case *ComparisonExpression:
		_, ok := n.Value.(*PlanConstExpression)
		return ok
	case *LogicalExpression:
		return treeHasPlanConstant(n.Left) || treeHasPlanConstant(n.Right)
	case *NotExpression:
		return treeHasPlanConstant(n.Target)
	case *ArrayPredicateExpression:
		if _, ok := n.CountValue.(*PlanConstExpression); ok {
			return true
		}
		return treeHasPlanConstant(n.Pred)
	case *RelationshipExpression:
		return treeHasPlanConstant(n.Target)
	case *SortExpression:
		return treeHasPlanConstant(n.Target)
	case *PaginateExpression:
		return treeHasPlanConstant(n.Target)
	case *SelectExpression:
		return treeHasPlanConstant(n.Target)
	case *TimestampExpression:
		return treeHasPlanConstant(n.Target)
	case *DepthExpression:
		return treeHasPlanConstant(n.Target)
	case *CountExpression:
		return treeHasPlanConstant(n.Target)
	case *ShapeExpression:
		return treeHasPlanConstant(n.Target)
	default:
		return false
	}
}

// specBodyNeedsExpansion reports whether a spec's body carries a plan
// constant, directly or through the specs it references.
//
// Such a spec cannot be left as a SpecReferenceExpression for the executor to
// inline later, which is what happens to every other spec: the executor
// inlines spec.Expr AS REGISTERED, where the plan constant is still
// unevaluated. So argument expansion inlines it instead (see the
// SpecReferenceExpression arm of expandExpressionWithArgs), once per call, with
// the call's envelope. visiting guards a cyclic spec graph, which the loader
// already refuses.
func (v *functionValidator) specBodyNeedsExpansion(name string, visiting map[string]struct{}) bool {
	key := strings.TrimSpace(name)
	if v == nil || v.specs == nil || key == "" {
		return false
	}
	if _, seen := visiting[key]; seen {
		return false
	}
	spec, err := v.specs.Get(key)
	if err != nil || spec == nil {
		return false
	}
	visiting[key] = struct{}{}
	defer delete(visiting, key)
	if treeHasPlanConstant(spec.Expr) {
		return true
	}
	return v.treeReferencesExpandingSpec(spec.Expr, visiting)
}

// treeReferencesExpandingSpec walks a tree for a spec reference whose body
// needs per-call expansion.
func (v *functionValidator) treeReferencesExpandingSpec(expr ExpressionNode, visiting map[string]struct{}) bool {
	switch n := expr.(type) {
	case nil:
		return false
	case *SpecReferenceExpression:
		return v.specBodyNeedsExpansion(n.Name, visiting)
	case *LogicalExpression:
		return v.treeReferencesExpandingSpec(n.Left, visiting) || v.treeReferencesExpandingSpec(n.Right, visiting)
	case *NotExpression:
		return v.treeReferencesExpandingSpec(n.Target, visiting)
	case *ArrayPredicateExpression:
		return v.treeReferencesExpandingSpec(n.Pred, visiting)
	case *RelationshipExpression:
		return v.treeReferencesExpandingSpec(n.Target, visiting)
	default:
		return false
	}
}

// planConstantReadsRoot reports whether a plan constant's source reads the
// reserved root name. Used where a caller must know what a still-unevaluated
// plan constant DEPENDS on without evaluating it -- planReferencesActor, which
// must fold the caller into the result-cache key if it reads `actor`.
//
// It is a syntactic check and deliberately over-approximates: a lambda
// parameter spelled `actor` counts too, which can only fold the actor into a
// key that did not need it (a wasted cache entry), never leave it out of one
// that did (a cross-caller leak).
func planConstantReadsRoot(pc *PlanConstExpression, root string) bool {
	if pc == nil || pc.Expr == nil {
		return false
	}
	found := false
	ast.WalkV1(pc.Expr, func(n ast.ExpressionNode) bool {
		if found {
			return false
		}
		if ident, ok := n.(*ast.IdentExpr); ok && ident != nil && ident.Name == root {
			found = true
			return false
		}
		return true
	})
	return found
}
