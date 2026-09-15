package automations

// loop_until.go -- whether an automation's @filter excludes the rows its
// @loop's until holds on (D-H of the loop protection plan, epic memql#5380).
//
// A deliberate cycle stops because the row it rewrites converges: the write
// that makes until hold is the last one the automation fires for, because its
// @filter no longer matches that row. That holds only when the filter
// excludes every row until holds on, and the proof the plan settles on is
// syntactic: the filter holds the NEGATION of until's predicate P as a
// top-level conjunct --
//
//   - `!(P)`;
//   - P's inverted comparison, when P is one comparison (== and !=, < and >=,
//     > and <=);
//   - the predicate P negates, when P is itself a negation (`!row.pending` is
//     excluded by `row.pending`).
//
// A negation inside an `||` narrows nothing on its own and does not count. An
// equivalent the check cannot see -- a rewritten comparison, a helper spec --
// is not found, and the refusal prints the conjunct to add.
//
// The two lambdas need not name their parameter alike: until's is renamed to
// the filter's before comparing, on a copy. Both sides are compared as
// canonical source (ast.FormatExpr), so spacing does not matter, and neither
// do parentheses around a conjunct or around the predicate a `!` negates.

import "github.com/znasllc-io/memql/component/language/ast"

// untilInFilter reports whether filter holds the negation of until's
// predicate as a top-level conjunct, written in filter's own parameter. A nil
// filter or until, or one that is not a lambda of one parameter, covers
// nothing.
func untilInFilter(filter, until *ast.LambdaExpr) bool {
	if filter == nil || until == nil || len(filter.Params) != 1 || len(until.Params) != 1 {
		return false
	}
	if renameWouldCapture(until.Body, until.Params[0], filter.Params[0]) {
		return false
	}
	p := renameParam(until.Body, until.Params[0], filter.Params[0])
	want := map[string]bool{}
	for _, n := range untilNegations(p) {
		want[ast.FormatExpr(n)] = true
	}
	for _, c := range ast.Conjuncts(filter.Body) {
		if want[ast.FormatExpr(canonicalConjunct(c))] {
			return true
		}
	}
	return false
}

// untilFilterConjunct is the conjunct a @filter adds to exclude the rows p
// holds on: p's inverted comparison when p is one, what p negates when p is a
// negation, and `!(p)` otherwise -- the form a person would write.
func untilFilterConjunct(p ast.ExpressionNode) ast.ExpressionNode {
	inner := ast.Unparen(p)
	if inv, ok := negateComparison(inner); ok {
		return inv
	}
	if u, ok := inner.(*ast.UnaryExpr); ok && u.Op == "!" {
		return ast.Unparen(u.Operand)
	}
	return &ast.UnaryExpr{Op: "!", Operand: inner}
}

// untilNegations are every conjunct that counts as excluding the rows p
// holds on: `!(p)`, and untilFilterConjunct's form when it differs.
func untilNegations(p ast.ExpressionNode) []ast.ExpressionNode {
	inner := ast.Unparen(p)
	out := []ast.ExpressionNode{&ast.UnaryExpr{Op: "!", Operand: inner}}
	if n := untilFilterConjunct(inner); ast.FormatExpr(n) != ast.FormatExpr(out[0]) {
		out = append(out, n)
	}
	return out
}

// canonicalConjunct drops the parentheses around a negated conjunct's
// operand, where a negation's redundant parentheses sit: `!(row.closed)` and
// `!row.closed` are one conjunct, and so are `!((P))` and `!(P)`.
func canonicalConjunct(c ast.ExpressionNode) ast.ExpressionNode {
	if u, ok := c.(*ast.UnaryExpr); ok && u.Op == "!" {
		return &ast.UnaryExpr{Op: "!", Operand: ast.Unparen(u.Operand)}
	}
	return c
}

// invertedComparison maps each comparison operator to its complement.
var invertedComparison = map[string]string{
	"==": "!=", "!=": "==",
	"<": ">=", ">=": "<",
	">": "<=", "<=": ">",
}

// negateComparison returns n's inverted comparison -- `a != b` for `a == b`,
// `a >= b` for `a < b` -- when n, parentheses aside, is one comparison. The
// result shares n's operands. Anything else has no inversion: `in` and
// `startsWith` have no inverse operator, and a connective is not one
// comparison.
func negateComparison(n ast.ExpressionNode) (ast.ExpressionNode, bool) {
	b, ok := ast.Unparen(n).(*ast.BinaryExpr)
	if !ok {
		return nil, false
	}
	op, ok := invertedComparison[b.Op]
	if !ok {
		return nil, false
	}
	return &ast.BinaryExpr{Op: op, Left: b.Left, Right: b.Right}, true
}

// renameParam returns a copy of n in which every use of the name from reads
// to, leaving n untouched. A lambda inside n that binds from shadows it: its
// body keeps the name, since there it names the inner parameter.
func renameParam(n ast.ExpressionNode, from, to string) ast.ExpressionNode {
	if from == to || n == nil {
		return n
	}
	walk := func(e ast.ExpressionNode) ast.ExpressionNode { return renameParam(e, from, to) }
	switch e := n.(type) {
	case *ast.IdentExpr:
		if e.Name != from {
			return e
		}
		c := *e
		c.Name = to
		return &c
	case *ast.MemberExpr:
		c := *e
		c.Object = walk(e.Object)
		return &c
	case *ast.CallExpr:
		c := *e
		c.Receiver = walk(e.Receiver)
		c.Args = make([]ast.ExpressionNode, len(e.Args))
		for i, a := range e.Args {
			c.Args[i] = walk(a)
		}
		c.Named = make([]ast.NamedArg, len(e.Named))
		for i, a := range e.Named {
			c.Named[i] = ast.NamedArg{Name: a.Name, Value: walk(a.Value)}
		}
		return &c
	case *ast.UnaryExpr:
		c := *e
		c.Operand = walk(e.Operand)
		return &c
	case *ast.BinaryExpr:
		c := *e
		c.Left, c.Right = walk(e.Left), walk(e.Right)
		return &c
	case *ast.ListExpr:
		c := *e
		c.Elems = make([]ast.ExpressionNode, len(e.Elems))
		for i, el := range e.Elems {
			c.Elems[i] = walk(el)
		}
		return &c
	case *ast.MapExpr:
		c := *e
		c.Entries = make([]ast.MapEntry, len(e.Entries))
		for i, en := range e.Entries {
			c.Entries[i] = ast.MapEntry{Key: en.Key, Value: walk(en.Value)}
		}
		return &c
	case *ast.ParenExpr:
		c := *e
		c.Inner = walk(e.Inner)
		return &c
	case *ast.TernaryExpr:
		return &ast.TernaryExpr{Condition: walk(e.Condition), Then: walk(e.Then), Else: walk(e.Else)}
	case *ast.LambdaExpr:
		for _, p := range e.Params {
			if p == from {
				return e
			}
		}
		c := *e
		c.Body = walk(e.Body)
		return &c
	}
	// A literal, nil, or a node outside the edition-2026 set: nothing in it
	// names the parameter.
	return n
}

// A nested parameter must not capture the outer row name during comparison.
// Conservatively refuse an ambiguous rename; retaining an edge is safer than
// proving convergence with two different predicates.
func renameWouldCapture(n ast.ExpressionNode, from, to string) bool {
	if from == to {
		return false
	}
	capture := false
	ast.WalkV1(n, func(e ast.ExpressionNode) bool {
		lam, ok := e.(*ast.LambdaExpr)
		if !ok {
			return true
		}
		for _, p := range lam.Params {
			if p == from {
				return false
			}
		}
		binds := false
		for _, p := range lam.Params {
			if p == to {
				binds = true
			}
		}
		if binds {
			ast.WalkV1(lam.Body, func(child ast.ExpressionNode) bool {
				if id, ok := child.(*ast.IdentExpr); ok && id.Name == from {
					capture = true
				}
				return true
			})
		}
		return !capture
	})
	return capture
}
