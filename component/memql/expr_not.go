package memql

import (
	"fmt"
	"strings"
)

// expr_not.go -- NotExpression, the executor's IR for edition 2026's `!`
// (epic memql#5363, task memql#5366).
//
// # Why negation is a node and not an operator
//
// The legacy grammar had no `!` in a predicate position: ast_converter.go
// refused it and pointed the author at the `!=` spelling, so every negation the
// executor ever saw arrived ON AN OPERATOR (`!=`, `not in`). Edition 2026
// admits `!` everywhere (D9), and `!(row.status == "archived" || row.deleted ==
// true)` negates a SUBTREE that no single operator can carry. So the IR gains
// one node, and the rest of this file is what it costs to keep that node honest
// in the two halves of the executor that must agree about every row: the SQL
// pushdown (tryCompileCombinedFilter) and its in-process twin
// (nodeMatchesExpression), which executeCombinedFilterQuery re-runs on every
// scanned candidate.
//
// # Two-valued, exactly -- the absence table's `!e` row
//
// Every predicate the executor lowers is TWO-VALUED in process:
// nodeMatchesExpression answers true or false, never "unknown". SQL is
// three-valued -- `payload #>> '{f}' = 'a'` is NULL, not false, when `f` is
// absent -- and a bare `NOT` over a NULL is still NULL, which a WHERE clause
// reads as "not returned". So `NOT (x = 'a')` would DROP the absent row while
// the in-process evaluator (`!false`) keeps it: the halves would disagree on
// exactly the rows the absence table exists to pin, and because the combined
// path INTERSECTS them the row would silently vanish. `NOT COALESCE((<target>),
// FALSE)` collapses the unknown to false BEFORE negating, which is what makes
// `!(x == v)` exactly `x != v` for an absent `x` (both true) rather than
// approximately so.
//
// # The one shape it refuses: a negated set
//
// The combined compiler turns a whole tree into one WHERE clause, and a NOT
// over anything it can compile compiles. When it cannot -- the operand holds a
// relationship traversal, which the executor evaluates as a SET of rows by
// walking edges -- the split evaluator would need the COMPLEMENT of that set,
// and a complement is only defined against the universe it is taken in: every
// row of the concept, which the executor never enumerates. The tempting
// approximation, the complement within the rows a scan happened to read, is a
// different and smaller answer that looks right, so the read refuses by name
// instead (notDoesNotLowerError). Set complement is deliberately unsupported.

// NotExpression negates a two-valued predicate: `!e` in edition 2026.
//
// SQL: `(NOT COALESCE((<target sql>), FALSE))`. In process: the negation of
// the target's match. A NotExpression whose target cannot be compiled to SQL
// does not lower -- see the file header for why that is a refusal rather than
// a set complement.
type NotExpression struct {
	Target ExpressionNode
}

func (*NotExpression) isExpressionNode() {}

// compileNotSQL wraps a compiled operand in the two-valued negation. Kept as
// one function so the exact spelling -- the COALESCE that makes NULL read as
// false before it is negated -- lives in one place and the SQL-text tests pin
// that place.
func compileNotSQL(inner compiledExpression) compiledExpression {
	return compiledExpression{
		sql:  fmt.Sprintf("(NOT COALESCE((%s), FALSE))", inner.sql),
		args: inner.args,
	}
}

// notDoesNotLowerError is the refusal evaluateExpressionSetWithContext returns
// for a NotExpression whose operand the combined compiler could not compile.
//
// It names the operand. "`!` does not lower" alone would send the author
// looking at the negation, when what they have to change is what it negates --
// almost always a relationship traversal, whose predicate can carry the
// negation instead (`childOf(c => c.status != "done")` rather than
// `!childOf(c => c.status == "done")`, which are different questions anyway:
// the first asks for rows with SOME child not done, the second for rows with
// NO child done).
func (e *MemQLEngine) notDoesNotLowerError(target ExpressionNode) error {
	if e.treeReachesRelationship(target, map[string]struct{}{}) {
		return fmt.Errorf("`!` over a relationship traversal does not lower: the executor evaluates a traversal " +
			"as a set of rows, and negating it needs the complement of that set within every row of the " +
			"concept, which it never enumerates -- move the negation inside the traversal's predicate")
	}
	return fmt.Errorf("`!` over %s does not lower: its operand does not compile to SQL, and the executor "+
		"cannot take the complement of a row set", describeIRNode(target))
}

// treeReachesRelationship reports whether a predicate tree contains a
// relationship traversal, looking through spec references (a spec body is
// where a traversal most often hides from the author of the negation).
// visiting guards a cyclic spec graph; the loader refuses cycles, so it only
// has to be correct, not informative.
func (e *MemQLEngine) treeReachesRelationship(expr ExpressionNode, visiting map[string]struct{}) bool {
	switch n := expr.(type) {
	case nil:
		return false
	case *RelationshipExpression:
		return true
	case *LogicalExpression:
		return e.treeReachesRelationship(n.Left, visiting) || e.treeReachesRelationship(n.Right, visiting)
	case *NotExpression:
		return e.treeReachesRelationship(n.Target, visiting)
	case *SpecReferenceExpression:
		name := strings.TrimSpace(n.Name)
		if e == nil || e.specs == nil || name == "" {
			return false
		}
		if _, seen := visiting[name]; seen {
			return false
		}
		spec, err := e.specs.Get(name)
		if err != nil || spec == nil {
			return false
		}
		visiting[name] = struct{}{}
		defer delete(visiting, name)
		return e.treeReachesRelationship(spec.Expr, visiting)
	default:
		return false
	}
}

// describeIRNode names an IR node for an author-facing message: what it IS in
// the language, not its Go type, which means nothing to someone reading a
// load or execution refusal.
func describeIRNode(expr ExpressionNode) string {
	switch n := expr.(type) {
	case nil:
		return "nothing"
	case *RelationshipExpression:
		return fmt.Sprintf("a %s traversal", n.Function)
	case *BuiltinFunctionExpression:
		return fmt.Sprintf("the builtin %q", n.Name)
	case *FunctionCallExpression:
		return fmt.Sprintf("the call %q", n.Name)
	case *SpecReferenceExpression:
		return fmt.Sprintf("the spec %q", n.Name)
	case *ArrayPredicateExpression:
		return fmt.Sprintf("the collection predicate %s", canonicalExpression(n))
	case *PlanConstExpression:
		return "an unevaluated plan constant"
	case *LogicalExpression:
		return "a logical expression"
	case *NotExpression:
		return "a negation"
	default:
		return fmt.Sprintf("a %T", expr)
	}
}
