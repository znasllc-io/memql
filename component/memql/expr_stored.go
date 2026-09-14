package memql

import (
	"github.com/znasllc-io/memql/component/language/ast"
)

// expr_stored.go -- how EvalExpr reads a value that came out of a STORED ROW
// where a list or a condition was expected (memql#5369; D7 and D8 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// # Why a stored value is read differently from a computed one
//
// A pushdown expression has two implementations of one meaning: the SQL
// Lower and the executor push down, and EvalExpr, which answers the same
// expression wherever nothing pushes down -- a refine clause, a trigger
// filter, the differential lane. D7 holds them equal, row for row. The SQL
// reads a stored value by what it holds: jsonb_typeof guards every typed
// operation, and a value of the wrong type for one is a MISMATCH that
// satisfies nothing (expr_collection_sql.go, the rules table) -- it is not a
// member, has no element that passes any() or all(), and is not true. The SQL
// cannot refuse a row; it can only answer about it.
//
// EvalExpr, left to itself, REFUSED the same values -- in_requires_list for
// `"a" in row.tags` over a stored scalar, operand_type for .any() over one,
// condition_not_boolean for `row.flag` over a stored "true" -- or read a stored
// object as a one-element list, the step-result convenience its collection
// methods keep for values a logic body computed. So the same predicate
// selected a mistyped row in neither a query filter nor a refine clause, but
// for different reasons: one excluded it, the other failed the whole read. A
// mistyped or malformed stored value is not equal, not ordered, not a member
// and not true; that is data, and data answers.
//
// So a value READ FROM A STORED ROW follows the pushdown's rules in the places
// a list or a condition is expected -- the right side of `in`, the receiver of
// any(), all() and count(), and a condition -- and nowhere else:
//
//	stored value         v in x   x.any(p)   x.all(p)   x.count()           as a condition
//	a list               member?  some      every      its length          not true
//	absent or null       false    false     true       0                   false
//	a string             false    false     false      its characters      not true
//	a bool               false    false     false      0                   itself
//	any other value      false    false     false      0                   not true
//
// count() counts what is stored, whatever the field declares: the pushdown's
// count dispatches on jsonb_typeof the same way, so a declared list holding a
// corrupt string counts its characters on both paths (string.count is the
// catalog entry EvalExpr dispatches to; it counts code points, as char_length
// does in the pushdown).
//
// A value computed in process -- an argument, a step result, a literal, the
// result of a call or of arithmetic -- has no SQL twin (in a pushdown position
// it is a plan constant, which EvalExpr evaluates on both paths), so it keeps
// EvalExpr's refusals: `in` over a number an author passed, or `lower(x)` used
// as a condition, is a mistake to name, not a row to skip.
//
// # What counts as stored
//
// A member path from a name bound to an ExprRow (row.tags, row.?lineage.tags),
// or from an element of such a path that a collection method or a predicate
// application is binding (t in row.tags.any(t => ...)). The row itself is not
// a stored VALUE -- `row` alone is the row -- so a path needs at least one
// member step from it. A ternary or a parenthesis in condition position is not
// a value of its own: evalCondition decides the branch it takes.

// exprStoredKind is what a name is bound to, as exprStoredRead reads it.
type exprStoredKind int

const (
	exprNotStored     exprStoredKind = iota
	exprStoredRow                    // an ExprRow
	exprStoredElement                // an element (or predicate argument) read out of a stored row
)

// exprStoredRead reports whether n reads a value out of a stored row.
func exprStoredRead(n ast.ExpressionNode, scope ExprScope) bool {
	hops := 0
	for {
		switch e := ast.Unparen(n).(type) {
		case *ast.MemberExpr:
			if e == nil {
				return false
			}
			hops++
			n = e.Object
		case *ast.IdentExpr:
			if e == nil {
				return false
			}
			switch exprStoredRoot(scope, e.Name) {
			case exprStoredRow:
				return hops > 0
			case exprStoredElement:
				return true
			}
			return false
		default:
			return false
		}
	}
}

// exprStoredRoot finds what name is bound to: a lambda parameter first, then
// the caller's scope.
func exprStoredRoot(scope ExprScope, name string) exprStoredKind {
	for scope != nil {
		ls, ok := scope.(*exprLambdaScope)
		if !ok {
			if v, found := scope.Lookup(name); found {
				if _, isRow := v.(ExprRow); isRow {
					return exprStoredRow
				}
			}
			return exprNotStored
		}
		for i, n := range ls.names {
			if n != name {
				continue
			}
			if i < len(ls.stored) && ls.stored[i] {
				return exprStoredElement
			}
			if _, isRow := ls.values[i].(ExprRow); isRow {
				return exprStoredRow
			}
			return exprNotStored
		}
		scope = ls.parent
	}
	return exprNotStored
}

// exprStoredMismatch reports whether a normalised stored value is present and
// not a list: the pushdown's type mismatch.
func exprStoredMismatch(v any) bool {
	switch v.(type) {
	case nil, absentValue, []any:
		return false
	}
	return true
}

// exprStoredListAnswer answers any(), all() and count() over a stored value
// that is present and not a list, as the pushdown's rules table does. ok is
// false for every other method, which keeps its in-process behaviour.
func exprStoredListAnswer(name string) (any, bool) {
	switch name {
	case "any", "all":
		return false, true
	case "count":
		return int64(0), true
	}
	return nil, false
}
