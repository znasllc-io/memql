package ast

// body.go -- the statements of edition 2026 (epic memql#5370, D12-D14 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// A `logic` and an `automation` share ONE body: statements, in source order.
// A name is its value (`rows := query x(...)` then `rows.count()`), and
// nothing reorders them.
//
// The statement set is closed and small on purpose. What each form means at
// run time is the body compiler's business (component/language/compiler); what
// names are in scope where is the scope checker's. This file is only the shape.

// Body is the statements of a logic or an automation, in source order.
type Body struct {
	BeforeWrite bool
	Statements  []BodyStatement
	Span        Span
}

// BodyStatement is one statement of a body. The concrete types below are the
// whole set; nothing outside this file implements it.
type BodyStatement interface {
	Node
	bodyStatement()
	StatementSpan() Span
}

// ConstructCall is `<kind> <name>(<named args>)`, optionally followed, for an
// action, by `on surface("<name>")`. A call is always a statement of its own:
// the whole right-hand side of `:=`, the whole value of `return`, or a bare
// call statement.
type ConstructCall struct {
	// Kind is one of query, mutation, logic, builtin, automation, action.
	Kind string
	Name string
	// Args are the named arguments, in source order. There are no positional
	// arguments: the bare-argument pun is retired.
	Args []NamedArg
	// Surface is the `on surface("...")` binding of an action call; "" when
	// the source wrote none.
	Surface string
	Span    Span
}

// StatementMods are a statement's trailing clauses.
type StatementMods struct {
	// Retry is the n of `retry(n)`; 0 means none. Legal on a call only.
	Retry int
	// OnError is "continue" for `on error continue`, "" for the default,
	// which is to stop. Legal on a call, a `for` and a `parallel`.
	OnError string
}

// AssignStatement is `name := <call>` or `name := <expression>`. Exactly one
// of Call and Value is set.
type AssignStatement struct {
	Name     string
	NameSpan Span
	Call     *ConstructCall
	Value    ExpressionNode
	Mods     StatementMods
	Span     Span
}

// CallStatement is a construct call whose value is not bound.
type CallStatement struct {
	Call *ConstructCall
	Mods StatementMods
	Span Span
}

// IfBranch is one arm of an if statement. Cond is nil for the final `else`.
type IfBranch struct {
	Cond ExpressionNode
	Body []BodyStatement
	Span Span
}

// IfStatement is `if c { } else if d { } else { }`. Branches[0] is the `if`.
type IfStatement struct {
	Branches []IfBranch
	Span     Span
}

// ForStatement is `for <var> in <source> [if <filter>] { }`. The author names
// the loop variable; it exists only inside the body.
type ForStatement struct {
	Var     string
	VarSpan Span
	Source  ExpressionNode
	// Filter is the `if` condition; nil when none is written.
	Filter ExpressionNode
	Body   []BodyStatement
	// Mods carries OnError only.
	Mods StatementMods
	Span Span
}

// CaseArm is one `case <label>[, <label>] { }`, or the `default { }`.
type CaseArm struct {
	// Labels are literal expressions; empty for default.
	Labels  []ExpressionNode
	Default bool
	Body    []BodyStatement
	Span    Span
}

// SwitchStatement is `switch <subject> { case ... { } default { } }`.
type SwitchStatement struct {
	Subject ExpressionNode
	Cases   []CaseArm
	Span    Span
}

// ParallelBranch is one `branch <label> { }` of a parallel statement. The
// label names the branch in the run record; it binds nothing.
type ParallelBranch struct {
	Label string
	Body  []BodyStatement
	Span  Span
}

// ParallelStatement is `parallel { branch a { } branch b { } } [wait any]`.
type ParallelStatement struct {
	Branches []ParallelBranch
	// Wait is "all" (the default, never written) or "any".
	Wait string
	// Mods carries OnError only.
	Mods StatementMods
	Span Span
}

// PublishStatement is `publish "<topic>" { <payload> }`, legal in an
// automation only. The topic is a string literal, so the topics a body can
// publish are known at load.
type PublishStatement struct {
	Topic   string
	Payload *MapExpr
	Span    Span
}

// ReturnStatement is `return [<value>]`. It ends the body; in an automation,
// the run. The value is an expression or, as on the right of `:=`, a whole
// construct call (`return builtin ensureDailySpace(userId: args.id)`): at most
// one of Call and Value is set, and neither for a bare `return`.
type ReturnStatement struct {
	Call  *ConstructCall
	Value ExpressionNode
	// Mods carries Retry only, and only with a Call.
	Mods StatementMods
	Span Span
}

func (*AssignStatement) node()            {}
func (*CallStatement) node()              {}
func (*IfStatement) node()                {}
func (*ForStatement) node()               {}
func (*SwitchStatement) node()            {}
func (*ParallelStatement) node()          {}
func (*PublishStatement) node()           {}
func (*ReturnStatement) node()            {}
func (*AssignStatement) bodyStatement()   {}
func (*CallStatement) bodyStatement()     {}
func (*IfStatement) bodyStatement()       {}
func (*ForStatement) bodyStatement()      {}
func (*SwitchStatement) bodyStatement()   {}
func (*ParallelStatement) bodyStatement() {}
func (*PublishStatement) bodyStatement()  {}
func (*ReturnStatement) bodyStatement()   {}

func (s *AssignStatement) StatementSpan() Span   { return s.Span }
func (s *CallStatement) StatementSpan() Span     { return s.Span }
func (s *IfStatement) StatementSpan() Span       { return s.Span }
func (s *ForStatement) StatementSpan() Span      { return s.Span }
func (s *SwitchStatement) StatementSpan() Span   { return s.Span }
func (s *ParallelStatement) StatementSpan() Span { return s.Span }
func (s *PublishStatement) StatementSpan() Span  { return s.Span }
func (s *ReturnStatement) StatementSpan() Span   { return s.Span }

// BodyStatementKinds lists the statement kinds, in the words StatementKind
// returns. It is the closed set every exhaustive walker is tested against.
func BodyStatementKinds() []string {
	return []string{"fieldWrite", "assign", "call", "if", "for", "switch", "parallel", "publish", "return"}
}

// StatementKind names a statement's kind: one of BodyStatementKinds.
func StatementKind(s BodyStatement) string {
	switch s.(type) {
	case *FieldWriteStatement:
		return "fieldWrite"
	case *AssignStatement:
		return "assign"
	case *CallStatement:
		return "call"
	case *IfStatement:
		return "if"
	case *ForStatement:
		return "for"
	case *SwitchStatement:
		return "switch"
	case *ParallelStatement:
		return "parallel"
	case *PublishStatement:
		return "publish"
	case *ReturnStatement:
		return "return"
	default:
		return ""
	}
}

// Children returns the statements nested directly inside s, in source order:
// every branch of an if, a for's body, every case of a switch, every branch of
// a parallel. A statement with no block returns nil.
func Children(s BodyStatement) []BodyStatement {
	var out []BodyStatement
	switch t := s.(type) {
	case *IfStatement:
		for _, b := range t.Branches {
			out = append(out, b.Body...)
		}
	case *ForStatement:
		out = append(out, t.Body...)
	case *SwitchStatement:
		for _, c := range t.Cases {
			out = append(out, c.Body...)
		}
	case *ParallelStatement:
		for _, b := range t.Branches {
			out = append(out, b.Body...)
		}
	}
	return out
}

// WalkBody visits every statement depth first, in source order. Returning
// false from visit skips that statement's children.
func WalkBody(stmts []BodyStatement, visit func(BodyStatement) bool) {
	for _, s := range stmts {
		if s == nil {
			continue
		}
		if !visit(s) {
			continue
		}
		WalkBody(Children(s), visit)
	}
}

// StatementExpressions returns every expression a statement itself holds --
// conditions, sources, filters, subjects, case labels, values, call arguments,
// a published payload -- in source order, and none of its nested statements'.
func StatementExpressions(s BodyStatement) []ExpressionNode {
	var out []ExpressionNode
	add := func(e ExpressionNode) {
		if e != nil {
			out = append(out, e)
		}
	}
	addCall := func(c *ConstructCall) {
		if c == nil {
			return
		}
		for _, a := range c.Args {
			add(a.Value)
		}
	}
	switch t := s.(type) {
	case *FieldWriteStatement:
		add(t.Value)
	case *AssignStatement:
		addCall(t.Call)
		add(t.Value)
	case *CallStatement:
		addCall(t.Call)
	case *IfStatement:
		for _, b := range t.Branches {
			add(b.Cond)
		}
	case *ForStatement:
		add(t.Source)
		add(t.Filter)
	case *SwitchStatement:
		add(t.Subject)
		for _, c := range t.Cases {
			for _, l := range c.Labels {
				add(l)
			}
		}
	case *PublishStatement:
		if t.Payload != nil {
			add(t.Payload)
		}
	case *ReturnStatement:
		addCall(t.Call)
		add(t.Value)
	}
	return out
}

// FieldWriteStatement adjusts one declared payload field before persistence.
type FieldWriteStatement struct {
	Field string
	Value ExpressionNode
	Span  Span
}

func (*FieldWriteStatement) node()                 {}
func (*FieldWriteStatement) bodyStatement()        {}
func (s *FieldWriteStatement) StatementSpan() Span { return s.Span }
