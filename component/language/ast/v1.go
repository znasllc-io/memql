package ast

// v1.go -- the expression nodes of edition 2026 (epic memql#5363, D1 and D9 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// Every expression an author writes in edition 2026 parses to the small set of
// nodes below, plus four this package already had and whose shape fits
// unchanged: LambdaExpr, TernaryExpr, LiteralExpr and NilExpr. The older node
// zoo (ComparisonExpr, SpecReferenceExpr, CondExpr, ConcatExpr, ...) is what
// the engine's internal query form still parses to -- the string an SDK sends
// to Execute and the wrapper the struct-form rewriter generates -- and never
// what an authored expression becomes.
//
// The set is deliberately uniform. A payload field is not a node kind: it is a
// MemberExpr on the lambda parameter. A spec or trait use is not a node kind: it
// is a CallExpr whose argument is the parameter. `args.x` is a MemberExpr on the
// root IdentExpr `args`. What each name MEANS is decided where the expression is
// lowered or evaluated, against the position it sits in, and never by the
// parser -- which is what lets one grammar serve every position.

// Span locates a node in the source it was parsed from. Line and column are
// 1-indexed; EndLine/EndCol are the first position after the node. A node built
// by hand (a rewrite, a test) carries the zero Span.
type Span struct {
	Line, Col       int
	EndLine, EndCol int
}

// IsZero reports whether the span was never set.
func (s Span) IsZero() bool { return s.Line == 0 && s.Col == 0 }

// IdentExpr is a bare name: a lambda parameter (`row`), a reserved root
// (`args`, `actor`, `now`, `config`, `event`), a body-local or step name, or --
// as the callee of a CallExpr -- a function or predicate name.
type IdentExpr struct {
	Name string
	Span Span
}

// MemberExpr reads a field: `x.f`, or with Optional `x.?f`. Both propagate
// absence at run time; `.?` is the spelling the loader requires where the
// object itself may be absent.
type MemberExpr struct {
	Object   ExpressionNode
	Field    string
	Optional bool
	Span     Span
}

// CallExpr is a call. Three shapes share it:
//
//   - a function or predicate call, `lower(x)` / `isActiveRecord(row)`:
//     Receiver nil, Kind "", positional Args;
//   - a method call, `row.tags.any(t => t == "x")`: Receiver set;
//   - a construct call, `query activeUsers(status: "active")`: Kind set to the
//     construct-call prefix (query, mutation, logic, builtin, automation,
//     action, capability) and the arguments in Named.
type CallExpr struct {
	Receiver ExpressionNode
	Name     string
	Kind     string
	Args     []ExpressionNode
	Named    []NamedArg
	Span     Span
}

// NamedArg is one `name: value` argument of a construct call.
type NamedArg struct {
	Name  string
	Value ExpressionNode
}

// UnaryExpr is `!x` or `-x`.
type UnaryExpr struct {
	Op      string
	Operand ExpressionNode
	Span    Span
}

// BinaryExpr is every binary operator: `* / %`, `+ -`, `??`, the comparisons
// `== != < <= > >=`, `in`, `startsWith`, `&&` and `||`. A `??` chain folds to
// nested nodes, left first.
type BinaryExpr struct {
	Op          string
	Left, Right ExpressionNode
	Span        Span
}

// ListExpr is a list literal whose elements are expressions: `[a, "b", 3]`.
type ListExpr struct {
	Elems []ExpressionNode
	Span  Span
}

// MapExpr is an object literal whose values are expressions: `{a: 1, b: x}`.
// Keys are unquoted identifiers (authoring rule 18).
type MapExpr struct {
	Entries []MapEntry
	Span    Span
}

// MapEntry is one `key: value` of a MapExpr.
type MapEntry struct {
	Key   string
	Value ExpressionNode
}

// ParenExpr keeps an author's parentheses, so the printer can reproduce them
// and the pushdown tier can tell a nested ternary that was parenthesised from
// one that was not.
type ParenExpr struct {
	Inner ExpressionNode
	Span  Span
}

func (*IdentExpr) node()            {}
func (*IdentExpr) expressionNode()  {}
func (*MemberExpr) node()           {}
func (*MemberExpr) expressionNode() {}
func (*CallExpr) node()             {}
func (*CallExpr) expressionNode()   {}
func (*UnaryExpr) node()            {}
func (*UnaryExpr) expressionNode()  {}
func (*BinaryExpr) node()           {}
func (*BinaryExpr) expressionNode() {}
func (*ListExpr) node()             {}
func (*ListExpr) expressionNode()   {}
func (*MapExpr) node()              {}
func (*MapExpr) expressionNode()    {}
func (*ParenExpr) node()            {}
func (*ParenExpr) expressionNode()  {}

// Unparen strips any number of ParenExpr wrappers.
func Unparen(n ExpressionNode) ExpressionNode {
	for {
		p, ok := n.(*ParenExpr)
		if !ok || p == nil {
			return n
		}
		n = p.Inner
	}
}

// WalkV1 visits n and, while visit returns true, its children, depth first.
// It descends only through the edition-2026 node set; a node outside it is
// visited and not descended into.
func WalkV1(n ExpressionNode, visit func(ExpressionNode) bool) {
	if n == nil || !visit(n) {
		return
	}
	switch e := n.(type) {
	case *MemberExpr:
		WalkV1(e.Object, visit)
	case *CallExpr:
		if e.Receiver != nil {
			WalkV1(e.Receiver, visit)
		}
		for _, a := range e.Args {
			WalkV1(a, visit)
		}
		for _, a := range e.Named {
			WalkV1(a.Value, visit)
		}
	case *UnaryExpr:
		WalkV1(e.Operand, visit)
	case *BinaryExpr:
		WalkV1(e.Left, visit)
		WalkV1(e.Right, visit)
	case *ListExpr:
		for _, el := range e.Elems {
			WalkV1(el, visit)
		}
	case *MapExpr:
		for _, en := range e.Entries {
			WalkV1(en.Value, visit)
		}
	case *ParenExpr:
		WalkV1(e.Inner, visit)
	case *TernaryExpr:
		WalkV1(e.Condition, visit)
		WalkV1(e.Then, visit)
		WalkV1(e.Else, visit)
	case *LambdaExpr:
		WalkV1(e.Body, visit)
	}
}
