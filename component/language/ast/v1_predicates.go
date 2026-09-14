package ast

// v1_predicates.go -- the questions the DSL gates ask of an edition-2026
// predicate (epic memql#5363, task memql#5368).
//
// Every gate that reads a filter clause asks one of three things: which fields
// does it read, which conjuncts does it always apply, and does every row it
// admits satisfy some property. The legacy gates answered them by splitting
// clause TEXT on `&&` / `||` / `,`, peeling parentheses and `when(...)` blocks
// by hand, and matching bare field names with regexes -- and each of those was
// filed at least once for going blind (memql#2787, #2832, #3612). An
// edition-2026 predicate is a tree, so the questions are answered on the tree,
// once, here.

// MemberPath decomposes a member chain `root.f1.f2` into its root identifier
// and its field names, in order. ok is false for anything else -- a call
// result (`x.first().id`), a parenthesised object, a literal.
func MemberPath(n ExpressionNode) (root string, fields []string, ok bool) {
	for {
		switch e := n.(type) {
		case *MemberExpr:
			fields = append(fields, e.Field)
			n = e.Object
		case *IdentExpr:
			for i, j := 0, len(fields)-1; i < j; i, j = i+1, j-1 {
				fields[i], fields[j] = fields[j], fields[i]
			}
			return e.Name, fields, true
		default:
			return "", nil, false
		}
	}
}

// MemberPaths calls fn for every maximal member chain in n whose root is an
// identifier -- `row.a.b` once, never also `row.a` -- in source order. A chain
// hanging off a call result is not reported, but the call's receiver and
// arguments are still walked, so `row.items.first()` reports `row.items`.
func MemberPaths(n ExpressionNode, fn func(root string, fields []string)) {
	WalkV1(n, func(e ExpressionNode) bool {
		if _, ok := e.(*MemberExpr); !ok {
			return true
		}
		if root, fields, ok := MemberPath(e); ok {
			fn(root, fields)
			return false
		}
		return true
	})
}

// maxPredicateDepth bounds the structural walks below. A parsed expression is
// already depth-bounded by the parser; this is the backstop for a tree built
// by hand.
const maxPredicateDepth = 512

// Guarantees reports whether EVERY row a boolean predicate admits satisfies
// leaf -- whether the property holds on all paths through the predicate's
// structure, not merely somewhere in it.
//
//   - `a || b` widens: it guarantees the property only if both arms do.
//   - `a && b` narrows: it guarantees it if either conjunct does.
//   - `!a` inverts, and is never a guarantee: `!(row.ownerUserId ==
//     actor.userId)` is precisely "rows I do not own".
//   - `c ? a : b` admits rows from both branches, so both must guarantee it.
//   - Parentheses are transparent.
//
// Everything else is a leaf, judged by leaf. The optional-argument guard needs
// no rule of its own: `(args.x == nil || e)` is a disjunction whose first arm
// is no guarantee of anything, so the guarded predicate is conditional exactly
// as the legacy `when(args.x) { e }` block was.
func Guarantees(n ExpressionNode, leaf func(ExpressionNode) bool) bool {
	return guaranteesAt(n, leaf, 0)
}

func guaranteesAt(n ExpressionNode, leaf func(ExpressionNode) bool, depth int) bool {
	if n == nil || depth > maxPredicateDepth {
		return false // an unreadable predicate never counts as a guarantee
	}
	switch e := n.(type) {
	case *ParenExpr:
		return guaranteesAt(e.Inner, leaf, depth+1)
	case *BinaryExpr:
		switch e.Op {
		case "&&":
			return guaranteesAt(e.Left, leaf, depth+1) || guaranteesAt(e.Right, leaf, depth+1)
		case "||":
			return guaranteesAt(e.Left, leaf, depth+1) && guaranteesAt(e.Right, leaf, depth+1)
		}
	case *UnaryExpr:
		if e.Op == "!" {
			return false
		}
	case *TernaryExpr:
		return guaranteesAt(e.Then, leaf, depth+1) && guaranteesAt(e.Else, leaf, depth+1)
	}
	return leaf(n)
}

// PredicateLeaves calls fn for every leaf of a boolean predicate: it looks
// through `&&`, `||`, `!`, parentheses and both branches of a conditional, and
// the conditional's own condition is a leaf too. A leaf is typically a
// comparison or a predicate application (`isActiveRecord(row)`).
func PredicateLeaves(n ExpressionNode, fn func(ExpressionNode)) {
	predicateLeavesAt(n, fn, 0)
}

func predicateLeavesAt(n ExpressionNode, fn func(ExpressionNode), depth int) {
	if n == nil || depth > maxPredicateDepth {
		return
	}
	switch e := n.(type) {
	case *ParenExpr:
		predicateLeavesAt(e.Inner, fn, depth+1)
		return
	case *BinaryExpr:
		if e.Op == "&&" || e.Op == "||" {
			predicateLeavesAt(e.Left, fn, depth+1)
			predicateLeavesAt(e.Right, fn, depth+1)
			return
		}
	case *UnaryExpr:
		if e.Op == "!" {
			predicateLeavesAt(e.Operand, fn, depth+1)
			return
		}
	case *TernaryExpr:
		predicateLeavesAt(e.Condition, fn, depth+1)
		predicateLeavesAt(e.Then, fn, depth+1)
		predicateLeavesAt(e.Else, fn, depth+1)
		return
	}
	fn(n)
}

// Conjuncts returns the operands of a predicate's top-level `&&` chain, each
// with its own parentheses removed; a predicate with no top-level `&&` is its
// own single conjunct. An operand that is itself an `||` is returned whole: a
// disjunction as a conjunct still narrows, but nothing inside it does on its
// own.
func Conjuncts(n ExpressionNode) []ExpressionNode {
	var out []ExpressionNode
	var walk func(ExpressionNode)
	walk = func(e ExpressionNode) {
		e = Unparen(e)
		if b, ok := e.(*BinaryExpr); ok && b.Op == "&&" {
			walk(b.Left)
			walk(b.Right)
			return
		}
		if e != nil {
			out = append(out, e)
		}
	}
	walk(n)
	return out
}
