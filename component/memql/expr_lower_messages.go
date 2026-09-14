package memql

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/functions"
)

// expr_lower_messages.go -- the refusals Lower writes, and the nearest pushdown
// spelling each one carries (D11, D24 of the design record: a refusal names the
// node, the position and the fix, and when the fix is mechanical, the rewritten
// line).
//
// Every spelling is built as an AST and printed with ast.FormatExpr, never
// concatenated as text: the printer parenthesises by precedence, so the
// suggestion re-parses to exactly the tree it describes, and a suggestion that
// did not would be a second refusal waiting for the author who pastes it.

// valueRefusal refuses a comparison whose side is row-dependent but not a
// field or a value: an in-process function or method over the row,
// arithmetic, `??` or a value ternary over the row, a predicate or traversal
// used as a value, or the row itself.
func (l *lowerer) valueRefusal(cmp *ast.BinaryExpr, side, other lowOperand) error {
	n := ast.Unparen(side.node)
	switch side.kind {
	case operandRow:
		return l.refuse(side.node, fmt.Sprintf("`%s` is the row itself, not a value", ast.FormatExpr(side.node)),
			"Compare one of its fields: `"+ast.FormatExpr(side.node)+".id == "+ast.FormatExpr(other.node)+"`")
	case operandCount:
		return l.refuse(cmp, "a `.count()` compares with a number, not with a field",
			"Compare it with a number, as in `"+ast.FormatExpr(side.node)+" > 0`")
	}
	switch e := n.(type) {
	case *ast.CallExpr:
		switch {
		case e.Kind != "":
			return l.unboundedSourceRefusal(e)
		case e.Receiver != nil:
			return l.inProcessMethodRefusal(e, cmp, other.node)
		case isTraversalName(e.Name):
			return l.refuse(e, "a traversal selects rows: it is a condition, not a value",
				"Write it as a condition on its own: `"+ast.FormatExpr(e)+"`")
		}
		if fn, ok := functions.Lookup(e.Name); ok {
			return l.inProcessFunctionRefusal(e, fn, cmp, other.node)
		}
		fix := "Apply it directly: `" + ast.FormatExpr(e) + "`"
		if lit, ok := ast.Unparen(other.node).(*ast.LiteralExpr); ok && lit.Value == false {
			fix = "Apply it directly, negated: `" + ast.FormatExpr(&ast.UnaryExpr{Op: "!", Operand: e}) + "`"
		}
		return l.refuse(e, fmt.Sprintf("`%s` is a predicate: a condition, not a value", e.Name), fix)
	case *ast.BinaryExpr:
		if e.Op == "??" {
			return l.coalesceComparisonRefusal(cmp, e, other.node)
		}
		return l.arithmeticRefusal(cmp, e, other.node)
	case *ast.TernaryExpr:
		return l.valueTernaryRefusal(cmp, e, other.node)
	case *ast.UnaryExpr:
		return l.refuse(e, "negating a field runs in process",
			"Negate the value instead: `"+ast.FormatExpr(replaceSide(cmp, e, e.Operand, &ast.UnaryExpr{Op: "-", Operand: other.node}))+"`")
	case *ast.ListExpr, *ast.MapExpr:
		return l.refuse(e, "a collection built from the row runs in process", "Compare the fields one at a time")
	}
	return l.refuse(side.node, "it reads the row, but is neither a field nor a value",
		"Compare a field of the row with a value, as in `"+l.rowParam()+".status == \"open\"`")
}

// replaceSide rebuilds a comparison with the operand `from` replaced by `to`
// and the OTHER operand replaced by `otherTo`, keeping the side each was on.
func replaceSide(cmp *ast.BinaryExpr, from ast.ExpressionNode, to, otherTo ast.ExpressionNode) ast.ExpressionNode {
	if ast.Unparen(cmp.Left) == from || cmp.Left == from {
		return &ast.BinaryExpr{Op: cmp.Op, Left: to, Right: otherTo}
	}
	return &ast.BinaryExpr{Op: cmp.Op, Left: otherTo, Right: to}
}

// inProcessFunctionRefusal refuses an in-process catalog function over the
// row. The three normalisers have a mechanical spelling -- apply them to the
// other side and compare the stored field -- and addDuration moves to the
// other side with its duration negated; everything else is pointed at a
// computed value or at refine.
func (l *lowerer) inProcessFunctionRefusal(call *ast.CallExpr, fn functions.Function, cmp *ast.BinaryExpr, other ast.ExpressionNode) error {
	reason := fmt.Sprintf("`%s` runs in process and reads the row", call.Name)
	if cmp != nil && len(call.Args) >= 1 {
		field := call.Args[0]
		switch call.Name {
		case "lower", "upper", "trim":
			if len(call.Args) == 1 {
				return l.refuse(call, reason, "Compare against a computed value instead: `"+
					ast.FormatExpr(replaceSide(cmp, call, field, &ast.CallExpr{Name: call.Name, Args: []ast.ExpressionNode{other}}))+"`")
			}
		case "addDuration":
			if len(call.Args) == 2 {
				if lit, ok := ast.Unparen(call.Args[1]).(*ast.LiteralExpr); ok {
					if dur, isString := lit.Value.(string); isString {
						neg := "-" + dur
						if strings.HasPrefix(dur, "-") {
							neg = strings.TrimPrefix(dur, "-")
						}
						moved := &ast.CallExpr{Name: "addDuration", Args: []ast.ExpressionNode{other, &ast.LiteralExpr{Value: neg}}}
						return l.refuse(call, reason, "Move the duration to the other side: `"+ast.FormatExpr(replaceSide(cmp, call, field, moved))+"`")
					}
				}
			}
		}
	}
	subject := ast.ExpressionNode(call)
	if cmp != nil {
		subject = cmp
	}
	return l.refuse(call, reason, "Compare the stored field with a value computed from the arguments, or evaluate this over a page: `paginate 50` then `refine "+l.rowParam()+" => "+ast.FormatExpr(subject)+"`")
}

// inProcessMethodRefusal refuses an in-process method over the row's value.
// `.where(p).count() > 0` has an exact pushdown spelling, `.any(p)`.
func (l *lowerer) inProcessMethodRefusal(call *ast.CallExpr, cmp *ast.BinaryExpr, other ast.ExpressionNode) error {
	reason := fmt.Sprintf("`.%s()` runs in process over the row's value", call.Name)
	if call.Name == "count" {
		if where, ok := ast.Unparen(call.Receiver).(*ast.CallExpr); ok && where.Name == "where" && where.Receiver != nil && len(where.Args) == 1 {
			reason = "`.where()` runs in process over the row's list"
			return l.refuse(call, reason, "Test the elements directly: `"+ast.FormatExpr(&ast.CallExpr{Receiver: where.Receiver, Name: "any", Args: where.Args})+"`")
		}
	}
	if call.Name == "where" && len(call.Args) == 1 {
		return l.refuse(call, reason, "Test the elements directly: `"+ast.FormatExpr(&ast.CallExpr{Receiver: call.Receiver, Name: "any", Args: call.Args})+"`")
	}
	subject := ast.ExpressionNode(call)
	if cmp != nil {
		subject = cmp
	}
	return l.refuse(call, reason, "Scan a row array with `.any()` or `.all()`, or evaluate this over a page: `paginate 50` then `refine "+l.rowParam()+" => "+ast.FormatExpr(subject)+"`")
}

// arithmeticRefusal refuses arithmetic over the row, moving it to the other
// side of the comparison when it is a `+`, `-`, or a `*` / `/` by a positive
// number: `row.a + 1 > 2` is `row.a > 1`.
func (l *lowerer) arithmeticRefusal(cmp *ast.BinaryExpr, arith *ast.BinaryExpr, other ast.ExpressionNode) error {
	reason := "arithmetic over the row runs in process"
	op := cmp.Op
	onRight := ast.Unparen(cmp.Right) == arith || cmp.Right == ast.ExpressionNode(arith)
	if onRight {
		op = flipTextOperator(op)
	}
	fieldLeft := l.rowDependent(arith.Left) && !l.rowDependent(arith.Right)
	fieldRight := !l.rowDependent(arith.Left) && l.rowDependent(arith.Right)
	var field, moved ast.ExpressionNode
	switch {
	case arith.Op == "+" && fieldLeft:
		field, moved = arith.Left, foldArith("-", other, arith.Right)
	case arith.Op == "+" && fieldRight:
		field, moved = arith.Right, foldArith("-", other, arith.Left)
	case arith.Op == "-" && fieldLeft:
		field, moved = arith.Left, foldArith("+", other, arith.Right)
	case arith.Op == "-" && fieldRight:
		// c - f OP v  <=>  f OP' c - v
		field, moved, op = arith.Right, foldArith("-", arith.Left, other), flipTextOperator(op)
	case (arith.Op == "*" || arith.Op == "/") && fieldLeft && isPositiveNumberLiteral(arith.Right):
		inverse := "/"
		if arith.Op == "/" {
			inverse = "*"
		}
		field, moved = arith.Left, foldArith(inverse, other, arith.Right)
	}
	if field == nil || !isComparisonText(op) {
		return l.refuse(arith, reason, "Compute the value on the other side of the comparison, from the arguments, and compare the stored field with it")
	}
	return l.refuse(arith, reason, "Move the arithmetic to the other side: `"+ast.FormatExpr(&ast.BinaryExpr{Op: op, Left: field, Right: moved})+"`")
}

func isComparisonText(op string) bool {
	switch op {
	case "==", "!=", "<", "<=", ">", ">=":
		return true
	}
	return false
}

// flipTextOperator is flipOperator on the source spelling.
func flipTextOperator(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	}
	return op
}

func isPositiveNumberLiteral(n ast.ExpressionNode) bool {
	lit, ok := ast.Unparen(n).(*ast.LiteralExpr)
	if !ok {
		return false
	}
	switch v := lit.Value.(type) {
	case int64:
		return v > 0
	case float64:
		return v > 0
	}
	return false
}

// foldArith builds `a op b`, folded to one literal when both are numbers, so
// the suggestion for `row.a + 1 > 2` reads `row.a > 1` rather than `2 - 1`.
func foldArith(op string, a, b ast.ExpressionNode) ast.ExpressionNode {
	la, aok := ast.Unparen(a).(*ast.LiteralExpr)
	lb, bok := ast.Unparen(b).(*ast.LiteralExpr)
	if aok && bok {
		if x, ok := la.Value.(int64); ok {
			if y, ok := lb.Value.(int64); ok {
				switch op {
				case "+":
					return &ast.LiteralExpr{Value: x + y}
				case "-":
					return &ast.LiteralExpr{Value: x - y}
				case "*":
					return &ast.LiteralExpr{Value: x * y}
				case "/":
					if y != 0 && x%y == 0 {
						return &ast.LiteralExpr{Value: x / y}
					}
				}
			}
		}
		fx, xok := numberAsFloat(la.Value)
		fy, yok := numberAsFloat(lb.Value)
		if xok && yok {
			switch op {
			case "+":
				return &ast.LiteralExpr{Value: fx + fy}
			case "-":
				return &ast.LiteralExpr{Value: fx - fy}
			case "*":
				return &ast.LiteralExpr{Value: fx * fy}
			case "/":
				if fy != 0 {
					return &ast.LiteralExpr{Value: fx / fy}
				}
			}
		}
	}
	return &ast.BinaryExpr{Op: op, Left: a, Right: b}
}

func numberAsFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// coalesceComparisonRefusal refuses `row.a ?? d OP v`, spelling the unset case
// out: `(row.a != nil && row.a OP v) || (row.a == nil && d OP v)`.
func (l *lowerer) coalesceComparisonRefusal(cmp *ast.BinaryExpr, co *ast.BinaryExpr, other ast.ExpressionNode) error {
	set := &ast.BinaryExpr{Op: "&&",
		Left:  &ast.BinaryExpr{Op: "!=", Left: co.Left, Right: &ast.NilExpr{}},
		Right: replaceSide(cmp, co, co.Left, other)}
	unset := &ast.BinaryExpr{Op: "&&",
		Left:  &ast.BinaryExpr{Op: "==", Left: co.Left, Right: &ast.NilExpr{}},
		Right: replaceSide(cmp, co, co.Right, other)}
	return l.refuse(co, "`??` over the row runs in process",
		"Compare the field directly and name the unset case: `"+ast.FormatExpr(&ast.BinaryExpr{Op: "||", Left: set, Right: unset})+"`")
}

// coalesceConditionRefusal refuses `row.flag ?? false` used as a condition. The
// two boolean defaults have exact spellings: `?? false` is `== true` and `??
// true` is `!= false` (an unset flag is false under the first, true under the
// second, on both evaluators).
func (l *lowerer) coalesceConditionRefusal(co *ast.BinaryExpr) error {
	reason := "`??` over the row runs in process"
	if lit, ok := ast.Unparen(co.Right).(*ast.LiteralExpr); ok {
		if b, isBool := lit.Value.(bool); isBool {
			op, value := "==", true
			if b {
				op, value = "!=", false
			}
			return l.refuse(co, reason, "Write the comparison it means: `"+ast.FormatExpr(&ast.BinaryExpr{Op: op, Left: co.Left, Right: &ast.LiteralExpr{Value: value}})+"`")
		}
	}
	spelled := &ast.BinaryExpr{Op: "||",
		Left:  &ast.BinaryExpr{Op: "&&", Left: &ast.BinaryExpr{Op: "!=", Left: co.Left, Right: &ast.NilExpr{}}, Right: co.Left},
		Right: &ast.BinaryExpr{Op: "&&", Left: &ast.BinaryExpr{Op: "==", Left: co.Left, Right: &ast.NilExpr{}}, Right: co.Right}}
	return l.refuse(co, reason, "Name the unset case: `"+ast.FormatExpr(spelled)+"`")
}

// valueTernaryRefusal refuses `(c ? x : y) OP v`, naming the boolean form
// `(c && x OP v) || (!c && y OP v)`, which is exact.
func (l *lowerer) valueTernaryRefusal(cmp *ast.BinaryExpr, t *ast.TernaryExpr, other ast.ExpressionNode) error {
	boolean := &ast.BinaryExpr{Op: "||",
		Left:  &ast.BinaryExpr{Op: "&&", Left: t.Condition, Right: replaceSide(cmp, t, t.Then, other)},
		Right: &ast.BinaryExpr{Op: "&&", Left: &ast.UnaryExpr{Op: "!", Operand: t.Condition}, Right: replaceSide(cmp, t, t.Else, other)}}
	return l.refuse(t, "a value ternary over the row runs in process",
		"Write the boolean form: `"+ast.FormatExpr(boolean)+"`")
}

// negatedTraversalRefusal refuses `!childOf(...)`, and a traversal used as a
// ternary's condition (which lowers through its negation). The executor does
// not compute the complement of a row set (expr_not.go); the nearest spelling
// moves the negation into the traversal's own predicate.
func (l *lowerer) negatedTraversalRefusal(n ast.ExpressionNode) error {
	fix := "Move the negation inside the traversal's predicate, or select the complement with the query's own filter"
	var call *ast.CallExpr
	switch e := ast.Unparen(n).(type) {
	case *ast.UnaryExpr:
		call, _ = ast.Unparen(e.Operand).(*ast.CallExpr)
	case *ast.CallExpr:
		call = e
	}
	if call != nil && len(call.Args) > 0 {
		if lam, ok := ast.Unparen(call.Args[len(call.Args)-1]).(*ast.LambdaExpr); ok && lam != nil {
			args := append([]ast.ExpressionNode(nil), call.Args[:len(call.Args)-1]...)
			args = append(args, &ast.LambdaExpr{Params: lam.Params, Body: &ast.UnaryExpr{Op: "!", Operand: lam.Body}})
			fix = "Move the negation inside the traversal's predicate: `" + ast.FormatExpr(&ast.CallExpr{Name: call.Name, Args: args}) + "`"
		}
	}
	return l.refuse(n, "a negated traversal needs the complement of a row set, which the executor does not compute", fix)
}

// outerElementRefusal refuses an inner collection predicate reading an OUTER
// element: the IR names only the innermost element, so the SQL could not bind
// the outer one.
func (l *lowerer) outerElementRefusal(n ast.ExpressionNode) error {
	return l.refuse(n, "an inner collection predicate reads its own element and the row, not an outer element",
		"Compare the inner element with a value, or test the two lists separately")
}

// outerRowRefusal refuses a traversal's predicate reading the row outside it.
func (l *lowerer) outerRowRefusal(n ast.ExpressionNode) error {
	return l.refuse(n, "a traversal's predicate reads only its own row: the rows it starts from are selected by a scan of their own",
		"Select the starting rows by value, as in `childOf(p => p.id == args.parentId)`")
}

// unboundedSourceRefusal refuses a collection method over a query result.
func (l *lowerer) unboundedSourceRefusal(n ast.ExpressionNode) error {
	return l.refuse(n, "a query result is an unbounded source, and a pushdown position scans only a row's own array",
		"Select the rows with this query's filter or a traversal, as in `childOf(p => p.id == args.id)`, or pass the values as an argument and test `row.f in args.values`")
}

// undeclaredFieldRefusal refuses a read of a field the bound concept does not
// declare, suggesting the nearest declared name.
func (l *lowerer) undeclaredFieldRefusal(e *ast.MemberExpr, segs []memberSeg) error {
	names := make([]string, len(segs))
	for i, s := range segs {
		names[i] = s.name
	}
	path := strings.Join(names, ".")
	concept := "the bound concept"
	if l.env.Concept != nil {
		concept = l.env.Concept.Name
	}
	prefix := ""
	if len(names) > 1 {
		prefix = strings.Join(names[:len(names)-1], ".") + "."
	}
	var candidates []string
	for declared := range l.fields {
		if !strings.HasPrefix(declared, prefix) || strings.Contains(strings.TrimPrefix(declared, prefix), ".") {
			continue
		}
		if levenshtein(declared, path) <= 2 {
			candidates = append(candidates, declared)
		}
	}
	sort.Strings(candidates)
	fix := "Read a field the concept declares, or declare this one"
	if len(candidates) > 0 {
		// Rebuild the author's own path with the last segment corrected, so
		// the suggestion keeps every `.?` they wrote -- a suggestion that
		// dropped one would be refused by the optional-hop rule the moment it
		// was taken.
		var rebuilt ast.ExpressionNode = &ast.IdentExpr{Name: l.rowParam()}
		for i, s := range segs {
			name := s.name
			if i == len(segs)-1 {
				name = strings.TrimPrefix(candidates[0], prefix)
			}
			rebuilt = &ast.MemberExpr{Object: rebuilt, Field: name, Optional: s.optional}
		}
		fix = "Did you mean `" + ast.FormatExpr(rebuilt) + "`?"
	}
	return l.refuse(e, fmt.Sprintf("`%s` is not a declared field of %s", path, concept), fix)
}

// optionalHopRefusal refuses `row.a.b` where `a` is an optional object field:
// the object may be absent, and edition 2026 asks for the read through it to
// say so (`.?`, D8). Both spellings read the same value at run time; the rule
// is that the source shows where absence can enter.
func (l *lowerer) optionalHopRefusal(e *ast.MemberExpr, segs []memberSeg, idx int) error {
	var rebuilt ast.ExpressionNode = &ast.IdentExpr{Name: l.rowParam()}
	for i, s := range segs {
		rebuilt = &ast.MemberExpr{Object: rebuilt, Field: s.name, Optional: s.optional || i == idx}
	}
	names := make([]string, idx+1)
	for i := 0; i <= idx; i++ {
		names[i] = segs[i].name
	}
	return l.refuse(e, "`"+l.rowParam()+"."+strings.Join(names, ".")+"` is an optional object, so it may be absent",
		"Read through it with `.?`: `"+ast.FormatExpr(rebuilt)+"`")
}

// literalListKinds reports the distinct kinds of a literal list's set members
// (strings, numbers, bools), for the one-type rule of a membership list.
func literalListKinds(values []any) []string {
	seen := map[string]bool{}
	for _, v := range values {
		if isUnsetLiteral(v) {
			continue
		}
		seen[literalTypeWord(v)] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
