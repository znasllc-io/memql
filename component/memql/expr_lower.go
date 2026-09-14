package memql

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// expr_lower.go -- Lower, THE lowering of edition 2026 (epic memql#5363, task
// memql#5366; D1, D8, D9 and D11 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// A v1 predicate body at a pushdown position -- a query filter, a spec or
// trait body -- becomes the executor's IR here, and from then on it is an
// ordinary tree: the SQL compiler (tryCompileCombinedFilter) and the in-process
// twin (nodeMatchesExpression) consume it exactly as they consume a tree the
// legacy grammar produced, and they already agree row for row. So there is
// no second SQL compiler in this file and no second set of comparison rules:
// Lower's only job is to decide, per node, WHICH IR node the v1 node is --
// and to refuse, at load, the nodes that have none.
//
// # The mapping
//
//	param.<intrinsic>          FieldReference{<canonical intrinsic>}
//	param.<field>[.<more>]     FieldReference{payload, <field>, ...}
//	                           (checked against the bound concept; mapped
//	                           through the shape for a shape-bound spec;
//	                           unchecked for a trait or a traversal's row)
//	actor.<f>, args.<f>        ActorReference / ArgReference, as today's IR
//	v OP param.f               flipped to param.f OP' v
//	v in param.<array>         OpHas
//	param.f in <list | args>   OpIn
//	param.f == nil / == ""     the one notion of unset, via the value
//	param.f startsWith p       OpStartsWith
//	param.f.includes(s)        OpIncludes
//	param.a OP param.b         a FieldOperand value (expr_field_operand.go)
//	!e                         NotExpression
//	isX(param), isX(actor)     SpecReferenceExpression, kind-checked
//	childOf(p => ...) etc.     RelationshipExpression, lowered on its own row
//	param.xs.any/all(x => ...) ArrayPredicateExpression
//	param.xs.count() OP n      ArrayPredicateExpression (count)
//	c ? p : q (boolean)        (c && p) || (!c && q)
//	a subtree reading no       PlanConstExpression, evaluated once per call by
//	  parameter                EvalExpr during argument expansion
//
// # What is refused, and how
//
// A row-dependent node with no IR form -- an in-process function or method
// over the row, arithmetic over the row, `??` over the row, a value ternary
// over the row, a construct call, a negated traversal -- is a LOAD refusal.
// Every refusal names three things (D11, D24): the node as written
// (ast.FormatExpr), the position, and the nearest pushdown spelling, so the
// author reads the fix rather than a description of the failure. LowerError
// carries them as fields and prints them in one sentence.
//
// A condition must be boolean (D8): a declared string or number field used as
// a condition refuses naming its type, statically; an untyped value is decided
// at run time, where the pushdown's reading of a non-boolean is "not true".

// ArgType is a declared argument's type as its args block spells it: string,
// int, float, bool, datetime, array, object, any, ... An empty ArgType is an
// argument whose type the lowering does not know, which it treats as untyped.
type ArgType string

// LowerEnv is what a v1 predicate body is lowered against.
type LowerEnv struct {
	// Position is the pushdown position the body sits in -- normally
	// tiers.PositionQueryFilter or tiers.PositionSpecBody. It names the
	// position in every refusal and decides what the tier manifest admits.
	Position tiers.Position
	// Param is the lambda's parameter: the row, or -- for a spec over an
	// @actor shape -- the actor envelope, spelled `actor`.
	Param string
	// Concept is the bound concept. Nil for a trait, a traversal's row and a
	// shape-bound spec: fields are then unchecked (a trait is validated at the
	// call site, as it always was).
	Concept *memoryNodes.Concept
	// ShapeKeys maps a shape-bound spec's projected keys to the stored paths
	// they read (`status` -> `payload.status`, `userId` -> `actor.userId`).
	// Non-nil means the parameter reads through the shape and only its keys
	// are fields.
	ShapeKeys map[string]string
	// Args are the declared arguments and their types. Nil means the position
	// takes no arguments (a spec or trait body): an `args.x` read refuses. A
	// query with no args block passes an empty, non-nil map.
	Args map[string]ArgType
	// Predicate resolves a spec or trait by name, for the kind check of a
	// predicate application. Nil defers the check: a query is lowered while
	// the specs are still loading, and the engine's Init pass lowers it again
	// with the registry in hand (lowerAllPushdownPositions).
	Predicate func(name string) (*Spec, bool)
}

// LowerError is a lowering refusal. Its three parts are the contract the
// corpus pins: Node is the refused node as written (ast.FormatExpr), Position
// the position it sits in, and Fix the nearest pushdown spelling -- a sentence
// carrying the rewritten expression in backticks.
type LowerError struct {
	Node     string
	Position tiers.Position
	Reason   string
	Fix      string
	// Span locates the refused node in the source it was parsed from, when
	// the node carries one (a v1 node the parser built). It is what lets an
	// authoring diagnostic point at the author's line and column rather than
	// at the construct as a whole; the zero Span means "no position", never
	// line 1.
	Span ast.Span
	// Clause and Anchor place a refusal inside a struct query's clause
	// ("filter" or "refine"). The struct rewriter folds a clause -- every
	// continuation line of it -- into the one `return` line the parser reads,
	// so Span there is a column on a line the author never wrote. Anchor is
	// the span of the clause's lambda BODY on that same line, which makes the
	// refusal's offset into the body independent of what the rewriter put in
	// front of it (the concept binding, the directive wrappers), and the
	// authoring diagnostic maps that offset back through the clause's own
	// fold onto the author's line and column.
	Clause string
	Anchor ast.Span
}

// Error prints the refusal as one sentence:
//
//	`lower(row.email)` does not lower in a query filter: `lower` runs in
//	process and reads the row. Compare against a computed value instead:
//	`row.email == lower("x")`
func (e *LowerError) Error() string {
	var b strings.Builder
	b.WriteString("`")
	b.WriteString(e.Node)
	b.WriteString("` does not lower in ")
	b.WriteString(positionPhrase(e.Position))
	b.WriteString(": ")
	b.WriteString(e.Reason)
	if e.Fix != "" {
		b.WriteString(". ")
		b.WriteString(e.Fix)
	}
	return b.String()
}

// positionPhrase names a position in a refusal's words.
func positionPhrase(p tiers.Position) string {
	switch p {
	case tiers.PositionQueryFilter:
		return "a query filter"
	case tiers.PositionSpecBody:
		return "a spec or trait body"
	case tiers.PositionQueryRefine:
		return "a refine clause"
	case "":
		return "a pushdown position"
	}
	return "position " + string(p)
}

// lowerReservedRoots are the reserved names a predicate's parameter may not
// take: naming the parameter `args` would make every argument read a row read.
// `actor` is reserved too, except as the parameter of a spec over an @actor
// shape, where it IS the envelope (D1).
var lowerReservedRoots = map[string]bool{
	"args": true, "actor": true, "now": true, "config": true, "event": true,
	"steps": true, "item": true, "index": true, "input": true, "trace": true, "partition": true,
}

// lowerPlanConstantRoots are the reserved roots a plan constant may read in a
// pushdown position: what argument expansion binds (planConstantBindings).
var lowerPlanConstantRoots = map[string]bool{"args": true, "actor": true, "now": true, "config": true}

// Lower turns a v1 predicate body into the executor's IR. body is the
// lambda's body; env.Param is its parameter.
func Lower(body ast.ExpressionNode, env LowerEnv) (ExpressionNode, error) {
	l, err := newLowerer(env, nil)
	if err != nil {
		return nil, err
	}
	if body == nil {
		return nil, &LowerError{Node: env.Param + " => ", Position: env.Position,
			Reason: "the predicate has no body", Fix: "Write the condition after the arrow: `" + env.Param + " => " + env.Param + ".status == \"open\"`"}
	}
	return l.pred(body)
}

// lowerScope is one lambda parameter in scope: the row (the first scope of a
// lowerer), or a collection element pushed by any() / all().
type lowerScope struct {
	param string
	elem  bool
	// elemType is an element's declared type ("string", "object", ...) when
	// the array's declaration names one.
	elemType string
}

type lowerer struct {
	env    LowerEnv
	fields map[string]conceptFieldShape
	// closed names the nested blocks whose keys the declaration closes: only
	// under one of them is an undeclared key a load error.
	closed map[string]bool
	scopes []lowerScope
	// outer names the parameters of the ENCLOSING lowerers, for a traversal's
	// body: it reads only its own row, and a read of the outer row is refused
	// by name rather than as an unknown one.
	outer []string
}

func newLowerer(env LowerEnv, outer []string) (*lowerer, error) {
	param := strings.TrimSpace(env.Param)
	if param == "" {
		return nil, &LowerError{Node: "=>", Position: env.Position, Reason: "the lambda has no parameter",
			Fix: "Name the row: `row => row.status == \"open\"`"}
	}
	actorParam := param == "actor" && env.ShapeKeys != nil
	if lowerReservedRoots[param] && !actorParam {
		fix := "Name the parameter `row`: `row => ...`"
		if param == "actor" {
			fix = "The parameter `actor` is the actor envelope, and only a spec over an @actor shape takes it; a predicate over rows names its parameter `row`: `row => ...`"
		}
		return nil, &LowerError{Node: param + " => ...", Position: env.Position,
			Reason: fmt.Sprintf("`%s` is a reserved name, so a parameter spelled that way would hide it", param), Fix: fix}
	}
	l := &lowerer{env: env, outer: outer, scopes: []lowerScope{{param: param}}}
	if env.Concept != nil {
		fields, err := flattenConceptFields(env.Concept)
		if err != nil {
			return nil, fmt.Errorf("read the declared fields of %s: %w", env.Concept.Name, err)
		}
		l.fields = fields
		closed, err := closedObjectPaths(env.Concept)
		if err != nil {
			return nil, fmt.Errorf("read the declared blocks of %s: %w", env.Concept.Name, err)
		}
		l.closed = closed
	}
	return l, nil
}

// refuse builds a LowerError for n.
func (l *lowerer) refuse(n ast.ExpressionNode, reason, fix string) error {
	return &LowerError{Node: ast.FormatExpr(n), Position: l.env.Position, Reason: reason, Fix: fix, Span: nodeSpan(n)}
}

// rowParam is the parameter of this lowerer's row.
func (l *lowerer) rowParam() string { return l.scopes[0].param }

// innermostElem is the element in scope, or nil at row level.
func (l *lowerer) innermostElem() *lowerScope {
	last := &l.scopes[len(l.scopes)-1]
	if last.elem {
		return last
	}
	return nil
}

// scopeOf finds the scope a name is the parameter of: the innermost binding
// wins, so a nested lambda's parameter shadows an outer one of the same name.
func (l *lowerer) scopeOf(name string) (int, bool) {
	for i := len(l.scopes) - 1; i >= 0; i-- {
		if l.scopes[i].param == name {
			return i, true
		}
	}
	return 0, false
}

func (l *lowerer) isOuter(name string) bool {
	for _, p := range l.outer {
		if p == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Row dependence: what makes a subtree a plan constant
// ---------------------------------------------------------------------------

// rowDependent reports whether a subtree depends on anything a plan constant
// cannot hold: a parameter in scope (the row, an element, an outer row), a
// predicate application (it lowers to a spec reference, which the executor
// inlines -- a plan constant would have to evaluate the spec in process), a
// relationship traversal (a row set) or a construct call (refused). Anything
// else reads only the call -- its arguments, the actor, the clock -- and is
// one value for the whole scan.
func (l *lowerer) rowDependent(n ast.ExpressionNode) bool {
	return l.dependsIn(n, nil)
}

func (l *lowerer) dependsIn(n ast.ExpressionNode, local map[string]bool) bool {
	switch e := n.(type) {
	case nil:
		return false
	case *ast.IdentExpr:
		if e == nil || local[e.Name] {
			return false
		}
		_, inScope := l.scopeOf(e.Name)
		return inScope || l.isOuter(e.Name)
	case *ast.MemberExpr:
		return e != nil && l.dependsIn(e.Object, local)
	case *ast.CallExpr:
		if e == nil {
			return false
		}
		if e.Kind != "" {
			return true
		}
		if e.Receiver == nil {
			if isTraversalName(e.Name) {
				return true
			}
			if _, catalogued := functions.Lookup(e.Name); !catalogued {
				return true // a predicate application
			}
		} else if l.dependsIn(e.Receiver, local) {
			return true
		}
		for _, a := range e.Args {
			if l.dependsIn(a, local) {
				return true
			}
		}
		for _, a := range e.Named {
			if l.dependsIn(a.Value, local) {
				return true
			}
		}
		return false
	case *ast.UnaryExpr:
		return e != nil && l.dependsIn(e.Operand, local)
	case *ast.BinaryExpr:
		return e != nil && (l.dependsIn(e.Left, local) || l.dependsIn(e.Right, local))
	case *ast.TernaryExpr:
		return e != nil && (l.dependsIn(e.Condition, local) || l.dependsIn(e.Then, local) || l.dependsIn(e.Else, local))
	case *ast.ListExpr:
		if e == nil {
			return false
		}
		for _, el := range e.Elems {
			if l.dependsIn(el, local) {
				return true
			}
		}
		return false
	case *ast.MapExpr:
		if e == nil {
			return false
		}
		for _, en := range e.Entries {
			if l.dependsIn(en.Value, local) {
				return true
			}
		}
		return false
	case *ast.ParenExpr:
		return e != nil && l.dependsIn(e.Inner, local)
	case *ast.LambdaExpr:
		if e == nil {
			return false
		}
		inner := make(map[string]bool, len(local)+len(e.Params))
		for k, v := range local {
			inner[k] = v
		}
		for _, p := range e.Params {
			inner[p] = true
		}
		return l.dependsIn(e.Body, inner)
	}
	return false
}

// isTraversalName reports whether a call names a relationship traversal: the
// catalog's row-set functions (parentOf, childOf, ..., ids).
func isTraversalName(name string) bool {
	fn, ok := functions.Lookup(name)
	return ok && fn.Returns == functions.TypeRows
}

// ---------------------------------------------------------------------------
// Predicates
// ---------------------------------------------------------------------------

// pred lowers a node in CONDITION position: the body, an operand of && || !,
// a ternary's condition or branch, a collection predicate's body.
func (l *lowerer) pred(n ast.ExpressionNode) (ExpressionNode, error) {
	if n == nil {
		return nil, &LowerError{Node: "", Position: l.env.Position, Reason: "a condition is missing", Fix: "Write a comparison, as in `row.status == \"open\"`"}
	}
	if !l.rowDependent(n) {
		return l.planConstPred(n)
	}
	switch e := n.(type) {
	case *ast.ParenExpr:
		return l.pred(e.Inner)
	case *ast.BinaryExpr:
		switch e.Op {
		case "&&", "||":
			left, err := l.pred(e.Left)
			if err != nil {
				return nil, err
			}
			right, err := l.pred(e.Right)
			if err != nil {
				return nil, err
			}
			op := LogicalAnd
			if e.Op == "||" {
				op = LogicalOr
			}
			return &LogicalExpression{Op: op, Left: left, Right: right}, nil
		case "==", "!=", "<", "<=", ">", ">=":
			return l.comparison(e)
		case "in":
			return l.membership(e)
		case "startsWith":
			return l.startsWith(e)
		case "??":
			return nil, l.coalesceConditionRefusal(e)
		default:
			return nil, l.refuse(e, fmt.Sprintf("`%s` is arithmetic over the row: it runs in process, and a number is not a condition", e.Op),
				"Compare the field with a value instead, as in `"+ast.FormatExpr(e.Left)+" > 0`")
		}
	case *ast.UnaryExpr:
		if e.Op != "!" {
			return nil, l.refuse(e, "a negated number is not a condition", "Compare the field with a value, as in `"+ast.FormatExpr(e.Operand)+" < 0`")
		}
		target, err := l.pred(e.Operand)
		if err != nil {
			return nil, err
		}
		if lowersToTraversal(target) {
			return nil, l.negatedTraversalRefusal(e)
		}
		return &NotExpression{Target: target}, nil
	case *ast.TernaryExpr:
		return l.ternaryPred(e)
	case *ast.CallExpr:
		return l.callPred(e)
	case *ast.MemberExpr, *ast.IdentExpr:
		return l.barePred(n)
	case *ast.LambdaExpr:
		return nil, l.refuse(e, "a lambda is an argument -- of a traversal or a collection method -- not a condition",
			"Apply it through a method, as in `row.tags.any("+ast.FormatExpr(e)+")`")
	case *ast.ListExpr:
		return nil, l.refuse(e, "a list is not a condition", "Test membership instead: `row.status in "+ast.FormatExpr(e)+"`")
	case *ast.MapExpr:
		return nil, l.refuse(e, "a map is not a condition", "Compare a field with a value instead, as in `row.status == \"open\"`")
	}
	return nil, l.refuse(n, fmt.Sprintf("%T has no pushdown form", n), "Write a comparison, as in `row.status == \"open\"`")
}

// lowersToTraversal reports whether an IR node is a relationship traversal,
// the one predicate that cannot be negated (expr_not.go: set complement is
// unsupported by design). Directly or behind nested negations of one.
func lowersToTraversal(n ExpressionNode) bool {
	switch e := n.(type) {
	case *RelationshipExpression:
		return true
	case *NotExpression:
		return lowersToTraversal(e.Target)
	}
	return false
}

// planConstPred lowers a row-independent node in condition position: a
// boolean literal becomes the constant, anything else a plan constant that
// argument expansion evaluates once per call. The static type is checked here
// (D8): a node whose type is known and is not boolean is refused at load
// rather than at every call.
func (l *lowerer) planConstPred(n ast.ExpressionNode) (ExpressionNode, error) {
	if lit, ok := ast.Unparen(n).(*ast.LiteralExpr); ok && lit != nil {
		if b, isBool := lit.Value.(bool); isBool {
			return &constantBoolExpression{value: b, planConstant: true}, nil
		}
	}
	if err := l.checkPlanConstant(n); err != nil {
		return nil, err
	}
	if t := l.staticType(n); t != "" && t != "bool" {
		return nil, l.notBooleanRefusal(n, t)
	}
	return &PlanConstExpression{Expr: n}, nil
}

// barePred lowers a field used as a condition, `row.urgent`: a boolean field
// is true exactly when it holds true, which is the comparison `== true` --
// false, absent and every non-boolean value are "not true" on both halves.
func (l *lowerer) barePred(n ast.ExpressionNode) (ExpressionNode, error) {
	op, err := l.operand(n)
	if err != nil {
		return nil, err
	}
	switch op.kind {
	case operandField:
		if op.typ != "" && op.typ != "bool" {
			return nil, l.notBooleanRefusal(n, op.typ)
		}
		return &ComparisonExpression{Field: op.field, Operator: OpEq, Value: true}, nil
	case operandValue:
		// A shape key mapped to an actor path: row-independent after all.
		if t := op.typ; t != "" && t != "bool" {
			return nil, l.notBooleanRefusal(n, t)
		}
		if ref, ok := op.value.(*ActorReference); ok {
			return &ComparisonExpression{Field: FieldReference{Raw: "actor." + ref.Path, Parts: []string{"actor", ref.Path}}, Operator: OpEq, Value: true}, nil
		}
		return &PlanConstExpression{Expr: n}, nil
	case operandRow:
		return nil, l.refuse(n, fmt.Sprintf("`%s` is the row itself, not a condition", ast.FormatExpr(n)),
			"Compare one of its fields, as in `"+ast.FormatExpr(n)+".status == \"open\"`, or apply a predicate to it: `isActiveRecord("+ast.FormatExpr(n)+")`")
	}
	return nil, l.refuse(n, "it is not a condition", "Write a comparison, as in `"+ast.FormatExpr(n)+" == true`")
}

// notBooleanRefusal is D8's refusal: a condition whose type is known and is
// not boolean, naming the type and the comparison that was probably meant.
func (l *lowerer) notBooleanRefusal(n ast.ExpressionNode, typ string) error {
	text := ast.FormatExpr(n)
	fix := "Compare it with a value, as in `" + text + " == true`"
	switch typ {
	case "string", "datetime":
		fix = "Test whether it is set, `" + text + " != nil`, or compare it: `" + text + " == \"...\"`"
	case "number":
		fix = "Compare it with a number, as in `" + text + " > 0`"
	case "list":
		fix = "Test its elements, as in `" + text + ".any(x => x == \"...\")`, or its size: `" + text + ".count() > 0`"
	case "map":
		fix = "Compare one of its fields, as in `" + text + ".status == \"...\"`"
	case "nil":
		fix = "A condition is `true` or `false`; `nil` is neither"
	}
	return l.refuse(n, fmt.Sprintf("`%s` is a %s, and a condition must be boolean", text, typ), fix)
}

// ternaryPred lowers a BOOLEAN ternary, `c ? p : q`, to `(c && p) || (!c &&
// q)`: exactly the ternary for a two-valued condition, and c may itself be a
// plan constant, in which case argument expansion folds the whole term to p or
// to q (TRUE && p -> p, FALSE || x -> x). A VALUE ternary -- its branches are
// strings, numbers, fields -- has no pushdown form, and is refused naming the
// boolean one.
func (l *lowerer) ternaryPred(e *ast.TernaryExpr) (ExpressionNode, error) {
	for _, branch := range []ast.ExpressionNode{e.Then, e.Else} {
		if t := l.branchType(branch); t != "" && t != "bool" {
			return nil, l.refuse(e, "a value ternary over the row runs in process, and its branches are not conditions",
				"Write the boolean form, with each branch a comparison: `"+ast.FormatExpr(&ast.BinaryExpr{Op: "||",
					Left:  &ast.BinaryExpr{Op: "&&", Left: e.Condition, Right: &ast.BinaryExpr{Op: "==", Left: e.Then, Right: &ast.IdentExpr{Name: "v"}}},
					Right: &ast.BinaryExpr{Op: "&&", Left: &ast.UnaryExpr{Op: "!", Operand: e.Condition}, Right: &ast.BinaryExpr{Op: "==", Left: e.Else, Right: &ast.IdentExpr{Name: "v"}}}})+"`")
		}
	}
	cond, err := l.pred(e.Condition)
	if err != nil {
		return nil, err
	}
	if lowersToTraversal(cond) {
		return nil, l.negatedTraversalRefusal(e.Condition)
	}
	// The condition is lowered a second time rather than shared: an IR node
	// appears once in a tree, so nothing that rewrites one branch can reach
	// into the other.
	negCond, err := l.pred(e.Condition)
	if err != nil {
		return nil, err
	}
	then, err := l.pred(e.Then)
	if err != nil {
		return nil, err
	}
	els, err := l.pred(e.Else)
	if err != nil {
		return nil, err
	}
	return &LogicalExpression{
		Op:    LogicalOr,
		Left:  &LogicalExpression{Op: LogicalAnd, Left: cond, Right: then},
		Right: &LogicalExpression{Op: LogicalAnd, Left: &NotExpression{Target: negCond}, Right: els},
	}, nil
}

// branchType is a ternary branch's type as far as the load can tell: a field's
// declared type, else the static type of a value.
func (l *lowerer) branchType(n ast.ExpressionNode) string {
	if !l.rowDependent(n) {
		return l.staticType(n)
	}
	switch ast.Unparen(n).(type) {
	case *ast.MemberExpr, *ast.IdentExpr:
		if op, err := l.operand(n); err == nil && (op.kind == operandField || op.kind == operandValue) {
			return op.typ
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Operands
// ---------------------------------------------------------------------------

type lowOperandKind int

const (
	// operandValue is row-independent: a literal, a list of literals, an
	// argument or actor reference, or a plan constant.
	operandValue lowOperandKind = iota
	// operandField is a field of the row or of the element in scope.
	operandField
	// operandCount is `<array field>.count()`.
	operandCount
	// operandRow is the row parameter itself.
	operandRow
	// operandOther is row-dependent and none of the above: refused by the
	// node that uses it, which knows what spelling to suggest.
	operandOther
)

type lowOperand struct {
	kind  lowOperandKind
	node  ast.ExpressionNode
	field FieldReference
	value any
	// typ is the static type word: string, number, bool, datetime, list,
	// map, nil, or "" when the load cannot know it.
	typ string
	// declType is a field's declared type as flattenConceptFields spells it
	// ("[]string", "object", ...), "" when undeclared or unknown.
	declType string
	// intrinsic is the row intrinsic a field reads, "" for a payload path.
	intrinsic string
}

// operand classifies one side of a comparison.
func (l *lowerer) operand(n ast.ExpressionNode) (lowOperand, error) {
	if !l.rowDependent(n) {
		return l.valueOperand(n)
	}
	switch e := n.(type) {
	case *ast.ParenExpr:
		return l.operand(e.Inner)
	case *ast.IdentExpr:
		if i, ok := l.scopeOf(e.Name); ok {
			s := l.scopes[i]
			if !s.elem {
				return lowOperand{kind: operandRow, node: n}, nil
			}
			if &l.scopes[i] != l.innermostElem() {
				return lowOperand{}, l.outerElementRefusal(e)
			}
			return lowOperand{kind: operandField, node: n,
				field: FieldReference{Raw: arrayElementRoot, Parts: []string{arrayElementRoot}},
				typ:   declTypeWord(s.elemType), declType: s.elemType}, nil
		}
		if l.isOuter(e.Name) {
			return lowOperand{}, l.outerRowRefusal(e)
		}
	case *ast.MemberExpr:
		return l.memberOperand(e)
	case *ast.CallExpr:
		if e.Receiver != nil && e.Kind == "" && e.Name == "count" && len(e.Args) == 0 && len(e.Named) == 0 {
			return l.countOperand(e)
		}
	}
	return lowOperand{kind: operandOther, node: n}, nil
}

// valueOperand lowers a row-independent operand.
func (l *lowerer) valueOperand(n ast.ExpressionNode) (lowOperand, error) {
	if err := l.checkPlanConstant(n); err != nil {
		return lowOperand{}, err
	}
	inner := ast.Unparen(n)
	switch e := inner.(type) {
	case *ast.LiteralExpr:
		return lowOperand{kind: operandValue, node: n, value: e.Value, typ: literalTypeWord(e.Value)}, nil
	case *ast.NilExpr:
		return lowOperand{kind: operandValue, node: n, value: nil, typ: "nil"}, nil
	case *ast.ListExpr:
		values := make([]any, 0, len(e.Elems))
		allLiteral := true
		for _, el := range e.Elems {
			switch v := ast.Unparen(el).(type) {
			case *ast.LiteralExpr:
				values = append(values, v.Value)
			case *ast.NilExpr:
				values = append(values, nil)
			default:
				allLiteral = false
			}
		}
		if allLiteral {
			return lowOperand{kind: operandValue, node: n, value: values, typ: "list"}, nil
		}
		return lowOperand{kind: operandValue, node: n, value: &PlanConstExpression{Expr: n}, typ: "list"}, nil
	case *ast.MemberExpr:
		if root, path, ok := simpleRootPath(e); ok {
			switch root {
			case "args":
				return lowOperand{kind: operandValue, node: n, value: &ArgReference{Path: strings.Join(path, ".")},
					typ: l.argPathType(path)}, nil
			case "actor":
				return lowOperand{kind: operandValue, node: n, value: &ActorReference{Path: strings.Join(path, ".")},
					typ: actorTypeWord(path[0])}, nil
			}
		}
	}
	return lowOperand{kind: operandValue, node: n, value: &PlanConstExpression{Expr: n}, typ: l.staticType(n)}, nil
}

// simpleRootPath reads a member chain rooted at an identifier with no optional
// hops: `args.a.b` -> ("args", [a, b]). A `.?` hop makes it a plan constant
// instead, which reads through absence exactly as EvalExpr does.
func simpleRootPath(e *ast.MemberExpr) (string, []string, bool) {
	var path []string
	var cur ast.ExpressionNode = e
	for {
		switch m := cur.(type) {
		case *ast.MemberExpr:
			if m.Optional {
				return "", nil, false
			}
			path = append([]string{m.Field}, path...)
			cur = m.Object
		case *ast.IdentExpr:
			return m.Name, path, len(path) > 0
		default:
			return "", nil, false
		}
	}
}

// memberSeg is one hop of a member chain.
type memberSeg struct {
	name     string
	optional bool
}

// memberChain splits a member chain into its root identifier and its hops.
func memberChain(e *ast.MemberExpr) (*ast.IdentExpr, []memberSeg, bool) {
	var segs []memberSeg
	var cur ast.ExpressionNode = e
	for {
		switch m := cur.(type) {
		case *ast.MemberExpr:
			segs = append([]memberSeg{{name: m.Field, optional: m.Optional}}, segs...)
			cur = m.Object
		case *ast.ParenExpr:
			cur = m.Inner
		case *ast.IdentExpr:
			return m, segs, true
		default:
			return nil, nil, false
		}
	}
}

// memberOperand lowers a row-dependent member chain: a field of the row or of
// the element in scope.
func (l *lowerer) memberOperand(e *ast.MemberExpr) (lowOperand, error) {
	root, segs, ok := memberChain(e)
	if !ok {
		return lowOperand{kind: operandOther, node: e}, nil
	}
	i, inScope := l.scopeOf(root.Name)
	if !inScope {
		if l.isOuter(root.Name) {
			return lowOperand{}, l.outerRowRefusal(e)
		}
		return lowOperand{kind: operandOther, node: e}, nil
	}
	s := l.scopes[i]
	if s.elem {
		if &l.scopes[i] != l.innermostElem() {
			return lowOperand{}, l.outerElementRefusal(e)
		}
		parts := []string{arrayElementRoot}
		for _, seg := range segs {
			parts = append(parts, seg.name)
		}
		return lowOperand{kind: operandField, node: e, field: FieldReference{Raw: strings.Join(parts, "."), Parts: parts}}, nil
	}
	return l.rowField(e, segs)
}

// rowField resolves `<row>.<segs...>`.
func (l *lowerer) rowField(e *ast.MemberExpr, segs []memberSeg) (lowOperand, error) {
	first := segs[0].name
	if l.env.ShapeKeys != nil {
		return l.shapeField(e, segs)
	}
	if info, ok := resolveIntrinsicField(first); ok {
		canonical, _ := canonicalIntrinsicFieldName(first)
		if info.kind == intrinsicFieldProvenance {
			if len(segs) != 2 {
				return lowOperand{}, l.refuse(e, "provenance is compared on one of its leaves",
					"Write `"+l.rowParam()+".provenance.kind`, `.name`, `.trigger` or `.via`")
			}
			leaf := strings.TrimSpace(segs[1].name)
			return lowOperand{kind: operandField, node: e, intrinsic: canonical,
				field: FieldReference{Raw: canonical + "." + leaf, Parts: []string{canonical, leaf}}, typ: "string"}, nil
		}
		if len(segs) > 1 {
			return lowOperand{}, l.refuse(e, fmt.Sprintf("`%s.%s` is a row column and has no fields", l.rowParam(), first),
				"Compare the column itself, as in `"+l.rowParam()+"."+canonical+" == \"...\"`")
		}
		typ := "string"
		if info.kind == intrinsicFieldCreatedAt {
			typ = "datetime"
		}
		return lowOperand{kind: operandField, node: e, intrinsic: canonical,
			field: FieldReference{Raw: canonical, Parts: []string{canonical}}, typ: typ}, nil
	}
	if isUnfilterableRowIntrinsic(first) {
		return lowOperand{}, l.refuse(e, fmt.Sprintf("`%s.%s` is a row intrinsic with no pushdown form", l.rowParam(), first),
			"Filter on id, concept, type, createdAt, createdBy or a provenance leaf")
	}

	parts := []string{"payload"}
	for _, seg := range segs {
		parts = append(parts, seg.name)
	}
	field := FieldReference{Raw: strings.Join(parts, "."), Parts: parts}
	if l.fields == nil {
		return lowOperand{kind: operandField, node: e, field: field}, nil
	}

	// Bound: every hop must be declared (down to the first hop through a
	// type whose keys the declaration does not close), and `.?` is required
	// on the hop after an OPTIONAL object field.
	path := ""
	var shape conceptFieldShape
	declared := true
	for idx, seg := range segs {
		if path == "" {
			path = seg.name
		} else {
			path = path + "." + seg.name
		}
		if !declared {
			continue
		}
		s, ok := l.fields[path]
		if !ok {
			return lowOperand{}, l.undeclaredFieldRefusal(e, segs[:idx+1])
		}
		shape = s
		if idx < len(segs)-1 {
			if s.Type == "any" || s.Type == "" {
				// An UNTYPED field: its members are the author's to answer
				// for, exactly as a trait's are, and `.` and `.?` read it
				// alike -- the `.?` rule binds a DECLARED optional object,
				// which an untyped value is not.
				declared = false
				continue
			}
			if !objectLikeType(s.Type) {
				if strings.HasPrefix(s.Type, "[]") {
					return lowOperand{}, l.refuse(e, fmt.Sprintf("`%s` is a list, and a list has no fields", l.rowParam()+"."+path),
						"Test its elements instead: `"+l.rowParam()+"."+path+".any(x => x."+segs[idx+1].name+" == \"...\")`")
				}
				return lowOperand{}, l.refuse(e, fmt.Sprintf("`%s` is a %s, and a %s has no fields", l.rowParam()+"."+path, declTypeWord(s.Type), declTypeWord(s.Type)),
					"Compare the field itself: `"+l.rowParam()+"."+path+" == \"...\"`")
			}
			if !s.Required && !seg.optional {
				return lowOperand{}, l.optionalHopRefusal(e, segs, idx)
			}
			if s.Type != "object" || !l.closed[path] {
				// A map, a union, or an OPEN object -- declared `object` with
				// no block, or an `@open` block: its keys are not all
				// declared, so the rest of the path is the author's to answer
				// for, as in a trait. Only a CLOSED block (the default for a
				// declared block, memql#3641) makes an undeclared key a
				// mistake worth refusing, because the write path would have
				// refused to store it.
				declared = false
			}
		}
	}
	if !declared {
		return lowOperand{kind: operandField, node: e, field: field}, nil
	}
	return lowOperand{kind: operandField, node: e, field: field, typ: declTypeWord(shape.Type), declType: shape.Type}, nil
}

// closedObjectPaths walks a concept's definition schema for the nested blocks
// whose key set is CLOSED (`additionalProperties: false`). flattenConceptFields
// cannot answer this: it records a block's declared keys, and a block that
// declares keys may still be `@open` -- keys as data -- where reading an
// undeclared one is legitimate and refusing it would be wrong.
func closedObjectPaths(c *memoryNodes.Concept) (map[string]bool, error) {
	out := map[string]bool{}
	raw, err := c.DefinitionSchema()
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var walk func(prefix string, doc map[string]any)
	walk = func(prefix string, doc map[string]any) {
		props, ok := doc["properties"].(map[string]any)
		if !ok {
			return
		}
		for name, value := range props {
			sub, ok := value.(map[string]any)
			if !ok {
				continue
			}
			path := joinFieldPath(prefix, name)
			if extra, ok := sub["additionalProperties"].(bool); ok && !extra {
				out[path] = true
			}
			walk(path, sub)
		}
	}
	walk("", doc)
	return out, nil
}

// shapeField resolves a read through a shape binding: only projected keys are
// fields, and each reads the stored path the shape names.
func (l *lowerer) shapeField(e *ast.MemberExpr, segs []memberSeg) (lowOperand, error) {
	key := segs[0].name
	path, ok := l.env.ShapeKeys[key]
	if !ok {
		return lowOperand{}, l.refuse(e, fmt.Sprintf("`%s` is not a projected key of the bound shape (keys: %s)", key, sortedShapeKeys(l.env.ShapeKeys)),
			"Read one of its keys, or bind a shape that projects it")
	}
	if len(segs) > 1 {
		return lowOperand{}, l.refuse(e, "a shape binding is read by projected key only", "Write `"+l.rowParam()+"."+key+"`")
	}
	ref := fieldReferenceFromPath(path)
	if len(ref.Parts) == 2 && strings.EqualFold(ref.Parts[0], "actor") {
		if l.rowParam() == "actor" {
			// A context spec: the envelope's fields are its fields, in the
			// form the actor-comparison fold reads (actor.<f>).
			return lowOperand{kind: operandField, node: e, field: ref, typ: actorTypeWord(ref.Parts[1])}, nil
		}
		// A mixed shape's actor key inside a row predicate: row-independent.
		return lowOperand{kind: operandValue, node: e, value: &ActorReference{Path: ref.Parts[1]}, typ: actorTypeWord(ref.Parts[1])}, nil
	}
	if len(ref.Parts) == 1 {
		if info, ok := resolveIntrinsicField(ref.Parts[0]); ok {
			canonical, _ := canonicalIntrinsicFieldName(ref.Parts[0])
			typ := "string"
			if info.kind == intrinsicFieldCreatedAt {
				typ = "datetime"
			}
			return lowOperand{kind: operandField, node: e, intrinsic: canonical, field: FieldReference{Raw: canonical, Parts: []string{canonical}}, typ: typ}, nil
		}
	}
	return lowOperand{kind: operandField, node: e, field: ref}, nil
}

// countOperand lowers `<array field>.count()`.
func (l *lowerer) countOperand(e *ast.CallExpr) (lowOperand, error) {
	recv, err := l.operand(e.Receiver)
	if err != nil {
		return lowOperand{}, err
	}
	if isConstructCall(e.Receiver) {
		return lowOperand{}, l.unboundedSourceRefusal(e)
	}
	if recv.kind != operandField || recv.intrinsic != "" {
		return lowOperand{kind: operandOther, node: e}, nil
	}
	if recv.typ != "" && recv.typ != "list" {
		if recv.typ == "string" {
			return lowOperand{}, l.refuse(e, "`.count()` on a string counts its characters in process, and has no pushdown form",
				"Compare the string itself, or evaluate the count over a page: `paginate 50` then `refine row => "+ast.FormatExpr(e)+" > 3`")
		}
		return lowOperand{}, l.refuse(e, fmt.Sprintf("`%s` is a %s, and `.count()` counts a list", ast.FormatExpr(e.Receiver), recv.typ),
			"Compare the field itself")
	}
	return lowOperand{kind: operandCount, node: e, field: recv.field}, nil
}

// isConstructCall reports whether a node is (or wraps) a construct call.
func isConstructCall(n ast.ExpressionNode) bool {
	c, ok := ast.Unparen(n).(*ast.CallExpr)
	return ok && c != nil && c.Kind != ""
}

// ---------------------------------------------------------------------------
// Comparisons, membership, prefixes
// ---------------------------------------------------------------------------

func comparisonOperatorOf(op string) ComparisonOperator {
	switch op {
	case "==":
		return OpEq
	case "!=":
		return OpNe
	case "<":
		return OpLt
	case "<=":
		return OpLe
	case ">":
		return OpGt
	case ">=":
		return OpGe
	}
	return ComparisonOperator(op)
}

// flipOperator is the operator after the operands swap sides: `3 < x` is
// `x > 3`.
func flipOperator(op ComparisonOperator) ComparisonOperator {
	switch op {
	case OpLt:
		return OpGt
	case OpLe:
		return OpGe
	case OpGt:
		return OpLt
	case OpGe:
		return OpLe
	}
	return op
}

func isOrderingOp(op ComparisonOperator) bool {
	switch op {
	case OpLt, OpLe, OpGt, OpGe:
		return true
	}
	return false
}

// comparison lowers `==` `!=` `<` `<=` `>` `>=`.
func (l *lowerer) comparison(e *ast.BinaryExpr) (ExpressionNode, error) {
	left, err := l.operand(e.Left)
	if err != nil {
		return nil, err
	}
	right, err := l.operand(e.Right)
	if err != nil {
		return nil, err
	}
	op := comparisonOperatorOf(e.Op)
	switch {
	case left.kind == operandField && right.kind == operandValue:
		return l.fieldComparison(e, left, op, right)
	case left.kind == operandValue && right.kind == operandField:
		return l.fieldComparison(e, right, flipOperator(op), left)
	case left.kind == operandField && right.kind == operandField:
		return l.fieldFieldComparison(e, left, op, right)
	case left.kind == operandCount && right.kind == operandValue:
		return l.countComparison(left, op, right)
	case left.kind == operandValue && right.kind == operandCount:
		return l.countComparison(right, flipOperator(op), left)
	case left.kind == operandValue && right.kind == operandValue:
		// Both sides are row-independent and something else (a predicate
		// application, a traversal) made the whole node row-dependent.
		return nil, l.refuse(e, "neither side reads the row", "Apply the predicate directly instead of comparing it, as in `isActiveRecord("+l.rowParam()+")`")
	}
	for _, side := range []lowOperand{left, right} {
		if side.kind == operandOther || side.kind == operandRow || side.kind == operandCount {
			other := right
			if side.node == right.node {
				other = left
			}
			return nil, l.valueRefusal(e, side, other)
		}
	}
	return nil, l.refuse(e, "it compares two things neither of which is a field and a value", "Compare a field with a value, as in `"+l.rowParam()+".status == \"open\"`")
}

// fieldComparison is `<field> op <value>`.
func (l *lowerer) fieldComparison(e *ast.BinaryExpr, f lowOperand, op ComparisonOperator, v lowOperand) (ExpressionNode, error) {
	if isOrderingOp(op) && (v.typ == "bool" || f.typ == "bool") {
		return nil, l.refuse(e, "booleans are not ordered", "Compare with `==` or `!=`: `"+ast.FormatExpr(f.node)+" == true`")
	}
	if isOrderingOp(op) && v.typ == "nil" {
		// Nothing orders against nil: false on both evaluators (EvalExpr's
		// exprOrdered answers false for an absent side), decided here rather
		// than handed to a compiler that would refuse the nil.
		return &constantBoolExpression{value: false, planConstant: true}, nil
	}
	if v.typ == "list" || v.typ == "map" {
		return nil, l.refuse(e, fmt.Sprintf("`%s` is a %s, and `%s` compares one value", ast.FormatExpr(v.node), v.typ, op),
			"Test membership instead: `"+ast.FormatExpr(f.node)+" in "+ast.FormatExpr(v.node)+"`")
	}
	if f.typ == "list" || f.typ == "map" {
		return nil, l.refuse(e, fmt.Sprintf("`%s` is a %s, and `%s` compares one value", ast.FormatExpr(f.node), f.typ, op),
			"Test its elements instead: `"+ast.FormatExpr(f.node)+".any(x => x "+string(op)+" "+ast.FormatExpr(v.node)+")`")
	}
	if f.intrinsic != "" {
		return l.intrinsicComparison(e, f, op, v)
	}
	return &ComparisonExpression{Field: f.field, Operator: op, Value: v.value}, nil
}

// intrinsicComparison is a comparison on a row column. The columns are NOT
// NULL, so an unset comparison is decided outright; the string columns are
// compared by equality and membership only; createdAt orders.
func (l *lowerer) intrinsicComparison(e *ast.BinaryExpr, f lowOperand, op ComparisonOperator, v lowOperand) (ExpressionNode, error) {
	if isUnsetLiteral(v.value) {
		if f.intrinsic == "provenance" {
			return nil, l.refuse(e, "a provenance leaf has no unset comparison in the pushdown",
				"Compare the leaf with a value: `"+ast.FormatExpr(f.node)+" == \"...\"`")
		}
		switch op {
		case OpEq:
			return &constantBoolExpression{value: false, planConstant: true}, nil
		case OpNe:
			return &constantBoolExpression{value: true, planConstant: true}, nil
		}
		return nil, l.refuse(e, "nothing orders against nil", "Compare with a value instead")
	}
	if isOrderingOp(op) && f.intrinsic != "createdAt" {
		return nil, l.refuse(e, fmt.Sprintf("`%s` is compared by `==`, `!=` and `in` only", ast.FormatExpr(f.node)),
			"Compare it with a value: `"+ast.FormatExpr(f.node)+" == "+ast.FormatExpr(v.node)+"`")
	}
	return &ComparisonExpression{Field: f.field, Operator: op, Value: v.value}, nil
}

// isUnsetLiteral reports whether a lowered VALUE is an unset literal (nil or
// ""); an argument or plan constant is decided at run time, not here.
func isUnsetLiteral(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	}
	return false
}

// fieldFieldComparison is `<field> op <field>` on one row (or one element).
func (l *lowerer) fieldFieldComparison(e *ast.BinaryExpr, left lowOperand, op ComparisonOperator, right lowOperand) (ExpressionNode, error) {
	if left.intrinsic != "" || right.intrinsic != "" {
		return nil, l.refuse(e, "a row column is compared with a value, not with another field",
			"Compare each field with a value, or compare the two payload fields")
	}
	if isOrderingOp(op) && (left.typ == "bool" || right.typ == "bool") {
		return nil, l.refuse(e, "booleans are not ordered", "Compare with `==` or `!=`")
	}
	for _, side := range []lowOperand{left, right} {
		if side.typ == "list" || side.typ == "map" {
			return nil, l.refuse(e, fmt.Sprintf("`%s` is a %s, and a field comparison compares two values", ast.FormatExpr(side.node), side.typ),
				"Compare two scalar fields, or test elements with `.any(...)`")
		}
	}
	return &ComparisonExpression{Field: left.field, Operator: op, Value: &FieldOperand{Field: right.field}}, nil
}

// countComparison is `<array>.count() op <value>`. The operand is a number
// literal or a plan constant (an argument included: ArrayPredicateExpression
// takes a plan constant as its count value, which expansion replaces).
func (l *lowerer) countComparison(c lowOperand, op ComparisonOperator, v lowOperand) (ExpressionNode, error) {
	if v.typ != "" && v.typ != "number" {
		return nil, l.refuse(c.node, fmt.Sprintf("`.count()` is a number, and `%s` is a %s", ast.FormatExpr(v.node), v.typ),
			"Compare it with a number: `"+ast.FormatExpr(c.node)+" > 0`")
	}
	var value any = v.value
	switch v.value.(type) {
	case *ArgReference, *ActorReference:
		value = &PlanConstExpression{Expr: v.node}
	}
	return &ArrayPredicateExpression{Field: c.field, Method: ArrayMethodCount, CountOp: op, CountValue: value}, nil
}

// membership lowers `in`.
func (l *lowerer) membership(e *ast.BinaryExpr) (ExpressionNode, error) {
	left, err := l.operand(e.Left)
	if err != nil {
		return nil, err
	}
	right, err := l.operand(e.Right)
	if err != nil {
		return nil, err
	}
	switch {
	case left.kind == operandField && right.kind == operandValue:
		// `row.f in <list>`.
		if right.typ != "" && right.typ != "list" && right.typ != "nil" {
			return nil, l.refuse(e, fmt.Sprintf("the right side of `in` must be a list, and `%s` is a %s", ast.FormatExpr(right.node), right.typ),
				"Compare with `==` instead: `"+ast.FormatExpr(left.node)+" == "+ast.FormatExpr(right.node)+"`")
		}
		if list, ok := right.value.([]any); ok {
			if len(list) == 0 {
				// `x in []` has no members: false, on both evaluators.
				return &constantBoolExpression{value: false, planConstant: true}, nil
			}
			// The SQL half binds one typed list per membership test
			// (compileTypedMembership), so a literal list of two types has
			// no single guard; refused at load rather than at the first call.
			if kinds := literalListKinds(list); len(kinds) > 1 {
				return nil, l.refuse(e, fmt.Sprintf("a membership list holds one type, and `%s` mixes %s", ast.FormatExpr(right.node), strings.Join(kinds, " and ")),
					"Split it into one `in` per type, joined with `||`")
			}
		}
		if right.value == nil {
			return &constantBoolExpression{value: false, planConstant: true}, nil
		}
		if left.intrinsic == "createdAt" {
			return nil, l.refuse(e, "`createdAt` is compared by `==` and ordered, not tested for membership",
				"Compare it with an instant: `"+ast.FormatExpr(left.node)+" >= "+"\"2026-01-01T00:00:00Z\"`")
		}
		return &ComparisonExpression{Field: left.field, Operator: OpIn, Value: right.value}, nil
	case left.kind == operandValue && right.kind == operandField:
		// `v in row.<array>`.
		if right.intrinsic != "" {
			return nil, l.refuse(e, fmt.Sprintf("`%s` is a row column, not a list", ast.FormatExpr(right.node)),
				"Compare it with the value: `"+ast.FormatExpr(right.node)+" == "+ast.FormatExpr(left.node)+"`")
		}
		if right.typ == "string" || right.typ == "datetime" {
			return nil, l.refuse(e, fmt.Sprintf("`%s` is a string, and `in` tests list membership", ast.FormatExpr(right.node)),
				"For a substring write `"+ast.FormatExpr(right.node)+".includes("+ast.FormatExpr(left.node)+")`")
		}
		if right.typ != "" && right.typ != "list" {
			return nil, l.refuse(e, fmt.Sprintf("`%s` is a %s, and `in` tests list membership", ast.FormatExpr(right.node), right.typ),
				"Compare it with the value: `"+ast.FormatExpr(right.node)+" == "+ast.FormatExpr(left.node)+"`")
		}
		return &ComparisonExpression{Field: right.field, Operator: OpHas, Value: left.value}, nil
	case left.kind == operandField && right.kind == operandField:
		return nil, l.refuse(e, "membership of one field in another has no pushdown form",
			"Test the list's elements instead: `"+ast.FormatExpr(e.Right)+".any(x => x == "+ast.FormatExpr(e.Left)+")`")
	}
	for _, side := range []lowOperand{left, right} {
		if side.kind == operandOther || side.kind == operandRow || side.kind == operandCount {
			other := right
			if side.node == right.node {
				other = left
			}
			return nil, l.valueRefusal(e, side, other)
		}
	}
	return nil, l.refuse(e, "neither side is a field of the row", "Write `"+l.rowParam()+".status in [\"a\", \"b\"]`")
}

// startsWith lowers `<field> startsWith <prefix>`.
func (l *lowerer) startsWith(e *ast.BinaryExpr) (ExpressionNode, error) {
	left, err := l.operand(e.Left)
	if err != nil {
		return nil, err
	}
	right, err := l.operand(e.Right)
	if err != nil {
		return nil, err
	}
	switch {
	case left.kind == operandField && right.kind == operandValue:
		if left.intrinsic != "" {
			return nil, l.refuse(e, fmt.Sprintf("`%s` is a row column, and a prefix test runs on a payload string", ast.FormatExpr(left.node)),
				"Compare the column with `==` or `in`")
		}
		if left.typ != "" && left.typ != "string" && left.typ != "datetime" {
			return nil, l.refuse(e, fmt.Sprintf("`%s` is a %s, and startsWith tests a string", ast.FormatExpr(left.node), left.typ),
				"Compare it with `==` instead")
		}
		switch right.typ {
		case "", "string", "datetime", "list":
		case "nil":
			// An absent prefix is a blank one: it matches nothing.
			return &constantBoolExpression{value: false, planConstant: true}, nil
		default:
			return nil, l.refuse(e, fmt.Sprintf("startsWith takes a string or a list of strings, and `%s` is a %s", ast.FormatExpr(right.node), right.typ),
				"Write a string prefix: `"+ast.FormatExpr(left.node)+" startsWith \"...\"`")
		}
		if lit, ok := right.value.(string); ok && strings.TrimSpace(lit) == "" {
			return &constantBoolExpression{value: false, planConstant: true}, nil
		}
		return &ComparisonExpression{Field: left.field, Operator: OpStartsWith, Value: right.value}, nil
	case left.kind == operandValue && right.kind == operandField:
		return nil, l.refuse(e, "the stored field is the subject of startsWith, not the prefix",
			"Put the field first: `"+ast.FormatExpr(e.Right)+" startsWith "+ast.FormatExpr(e.Left)+"` -- or, to ask whether a value starts with a stored prefix, evaluate it over a page with `refine`")
	case left.kind == operandField && right.kind == operandField:
		return nil, l.refuse(e, "a prefix test between two fields has no pushdown form", "Evaluate it over a page with `refine row => "+ast.FormatExpr(e)+"` after `paginate`")
	}
	for _, side := range []lowOperand{left, right} {
		if side.kind == operandOther || side.kind == operandRow || side.kind == operandCount {
			other := right
			if side.node == right.node {
				other = left
			}
			return nil, l.valueRefusal(e, side, other)
		}
	}
	return nil, l.refuse(e, "neither side is a field of the row", "Write `"+l.rowParam()+".name startsWith \"prefix\"`")
}

// ---------------------------------------------------------------------------
// Calls: traversals, predicates, methods
// ---------------------------------------------------------------------------

// callPred lowers a call in condition position.
func (l *lowerer) callPred(e *ast.CallExpr) (ExpressionNode, error) {
	if e.Kind != "" {
		if e.Receiver == nil {
			return nil, l.refuse(e, "a construct call has no place in a predicate: over the row it would run once per row, and as a plan constant it brings a whole result into the filter",
				"Select the rows with the query's own filter, or a traversal such as `childOf(p => ...)`")
		}
		return nil, l.unboundedSourceRefusal(e)
	}
	if e.Receiver != nil {
		return l.methodPred(e)
	}
	if isTraversalName(e.Name) {
		return l.traversal(e)
	}
	if fn, catalogued := functions.Lookup(e.Name); catalogued {
		return nil, l.inProcessFunctionRefusal(e, fn, nil, nil)
	}
	return l.predicateApplication(e)
}

// traversal lowers `childOf(p => ...)`, `contains("label", p => ...)`: the
// lambda selects the rows the traversal starts from, lowered as a filter over
// an UNBOUND row of its own -- which concept it reads is the lambda's to say
// (`p.concept == "v1:..."`), exactly as the legacy traversal's target was a
// filter of its own. It reads only its row: the traversed rows are selected
// by a separate scan, so a reference to the outer row has nothing to bind to.
func (l *lowerer) traversal(e *ast.CallExpr) (ExpressionNode, error) {
	fnName := RelationshipFunction(e.Name)
	if len(e.Named) > 0 {
		return nil, l.refuse(e, "a traversal takes positional arguments", "Write `"+e.Name+"(p => ...)`")
	}
	var label string
	args := e.Args
	switch len(args) {
	case 1:
	case 2:
		lit, ok := ast.Unparen(args[0]).(*ast.LiteralExpr)
		s, isString := "", false
		if ok && lit != nil {
			s, isString = lit.Value.(string)
		}
		if !isString || strings.TrimSpace(s) == "" {
			return nil, l.refuse(e, "a traversal's label is a string literal naming the edges' `as` label",
				"Write `"+e.Name+"(\"respondsAs\", p => ...)`")
		}
		if fnName == RelIds {
			return nil, l.refuse(e, "ids() follows no edge, so it takes no label", "Write `ids(p => ...)`")
		}
		label = strings.TrimSpace(s)
		args = args[1:]
	default:
		return nil, l.refuse(e, fmt.Sprintf("%s() takes an optional label and a lambda, got %d arguments", e.Name, len(e.Args)),
			"Write `"+e.Name+"(p => p.id == args.id)`")
	}
	lam, ok := ast.Unparen(args[0]).(*ast.LambdaExpr)
	if !ok || lam == nil || len(lam.Params) != 1 {
		return nil, l.refuse(e, "a traversal selects its starting rows with a lambda of one parameter",
			"Write `"+e.Name+"(p => p.id == args.id)`")
	}
	outer := append([]string(nil), l.outer...)
	for _, s := range l.scopes {
		outer = append(outer, s.param)
	}
	sub, err := newLowerer(LowerEnv{Position: l.env.Position, Param: lam.Params[0], Args: l.env.Args, Predicate: l.env.Predicate}, outer)
	if err != nil {
		return nil, err
	}
	target, err := sub.pred(lam.Body)
	if err != nil {
		return nil, err
	}
	return &RelationshipExpression{Function: fnName, Target: target, Label: label}, nil
}

// predicateApplication lowers `isX(row)` / `isX(actor)` to a spec reference,
// after checking that the argument is one a predicate can be applied to and --
// when the registry is at hand -- that the spec's kind matches it: a row spec
// or trait reads the row, a context spec reads the actor.
func (l *lowerer) predicateApplication(e *ast.CallExpr) (ExpressionNode, error) {
	if tiers.PredicateAdmission(l.env.Position) != tiers.Admitted {
		return nil, l.refuse(e, "a predicate cannot be applied here", "Write the condition inline")
	}
	if len(e.Args) != 1 || len(e.Named) > 0 {
		return nil, l.refuse(e, fmt.Sprintf("a predicate is applied to exactly one argument, and %s() has %d", e.Name, len(e.Args)+len(e.Named)),
			"Apply it to the row: `"+e.Name+"("+l.rowParam()+")`")
	}
	arg, ok := ast.Unparen(e.Args[0]).(*ast.IdentExpr)
	if !ok || arg == nil {
		return nil, l.refuse(e, "a predicate is applied to the row or to the actor, not to a value",
			"Apply it to the row: `"+e.Name+"("+l.rowParam()+")`")
	}
	appliedToRow := false
	if i, inScope := l.scopeOf(arg.Name); inScope {
		if l.scopes[i].elem {
			return nil, l.refuse(e, fmt.Sprintf("a predicate reads a row, and `%s` is an element of a list", arg.Name),
				"Write the element condition inline: `... .any("+arg.Name+" => "+arg.Name+" == \"...\")`")
		}
		appliedToRow = l.scopes[i].param != "actor" || l.env.ShapeKeys == nil
	} else if l.isOuter(arg.Name) {
		return nil, l.outerRowRefusal(e)
	} else if arg.Name != "actor" {
		return nil, l.refuse(e, fmt.Sprintf("`%s` is neither the row nor the actor", arg.Name),
			"Apply it to the row, `"+e.Name+"("+l.rowParam()+")`, or to the actor, `"+e.Name+"(actor)`")
	}
	if l.env.Predicate != nil {
		spec, found := l.env.Predicate(e.Name)
		if !found || spec == nil {
			return nil, l.refuse(e, fmt.Sprintf("`%s` is not a spec, trait or catalog function known here", e.Name),
				"Check the name and the file-top `use` import of the spec or trait")
		}
		isContext := spec.Kind == SpecKindContext
		switch {
		case appliedToRow && isContext:
			return nil, l.refuse(e, fmt.Sprintf("`%s` is a context spec over the actor, not a predicate over rows", e.Name),
				"Apply it to the actor: `"+e.Name+"(actor)`")
		case !appliedToRow && !isContext:
			kind := "row spec"
			if spec.IsTrait {
				kind = "trait"
			}
			return nil, l.refuse(e, fmt.Sprintf("`%s` is a %s, a predicate over rows, not over the actor", e.Name, kind),
				"Apply it to the row: `"+e.Name+"("+l.rowParam()+")`")
		}
	}
	return &SpecReferenceExpression{Name: e.Name}, nil
}

// methodPred lowers a method call in condition position.
func (l *lowerer) methodPred(e *ast.CallExpr) (ExpressionNode, error) {
	switch e.Name {
	case "any", "all":
		return l.arrayPredicate(e)
	case "includes":
		return l.includes(e)
	case "count":
		return nil, l.refuse(e, "`.count()` is a number, and a condition must be boolean",
			"Compare it: `"+ast.FormatExpr(e)+" > 0`")
	}
	if _, onList := functions.Method(functions.TypeList, e.Name); onList {
		return nil, l.inProcessMethodRefusal(e, nil, nil)
	}
	if _, onString := functions.Method(functions.TypeString, e.Name); onString {
		return nil, l.inProcessMethodRefusal(e, nil, nil)
	}
	return nil, l.refuse(e, fmt.Sprintf("`.%s()` is not a method", e.Name), "See the function catalog for the methods a list and a string have")
}

// arrayPredicate lowers `<array>.any(x => ...)` / `.all(x => ...)` over a row
// array field (or an array field of the element in scope): the one bounded
// collection a pushdown position may scan.
func (l *lowerer) arrayPredicate(e *ast.CallExpr) (ExpressionNode, error) {
	if isConstructCall(e.Receiver) {
		return nil, l.unboundedSourceRefusal(e)
	}
	if len(e.Args) != 1 || len(e.Named) > 0 {
		return nil, l.refuse(e, fmt.Sprintf(".%s() takes one lambda", e.Name), "Write `"+ast.FormatExpr(e.Receiver)+"."+e.Name+"(x => x == \"...\")`")
	}
	lam, ok := ast.Unparen(e.Args[0]).(*ast.LambdaExpr)
	if !ok || lam == nil || len(lam.Params) != 1 {
		return nil, l.refuse(e, fmt.Sprintf(".%s() takes a lambda of one parameter, the element", e.Name),
			"Write `"+ast.FormatExpr(e.Receiver)+"."+e.Name+"(x => x == \"...\")`")
	}
	recv, err := l.operand(e.Receiver)
	if err != nil {
		return nil, err
	}
	switch recv.kind {
	case operandField:
	case operandValue:
		return nil, l.refuse(e, "the collection is a plan constant, so its elements are not the row's, and the predicate reads the row",
			"Test the row field for membership instead, as in `"+l.rowParam()+".status in "+ast.FormatExpr(e.Receiver)+"`")
	default:
		return nil, l.refuse(e, fmt.Sprintf("`%s` is not an array field of the row", ast.FormatExpr(e.Receiver)),
			"Scan a row array field: `"+l.rowParam()+".tags."+e.Name+"(t => t == \"...\")`")
	}
	if recv.intrinsic != "" {
		return nil, l.refuse(e, fmt.Sprintf("`%s` is a row column, not a list", ast.FormatExpr(e.Receiver)), "Scan a payload array field")
	}
	if recv.typ != "" && recv.typ != "list" {
		return nil, l.refuse(e, fmt.Sprintf("`%s` is a %s, not a list", ast.FormatExpr(e.Receiver), recv.typ),
			"Compare the field itself: `"+ast.FormatExpr(e.Receiver)+" == \"...\"`")
	}
	param := lam.Params[0]
	if _, shadows := l.scopeOf(param); shadows || lowerReservedRoots[param] {
		return nil, l.refuse(lam, fmt.Sprintf("the element parameter `%s` hides a name already in scope", param),
			"Name the element after the list, as in `t => t == \"...\"`")
	}
	elemType := ""
	if strings.HasPrefix(recv.declType, "[]") {
		elemType = strings.TrimPrefix(recv.declType, "[]")
	}
	l.scopes = append(l.scopes, lowerScope{param: param, elem: true, elemType: elemType})
	body, err := l.pred(lam.Body)
	l.scopes = l.scopes[:len(l.scopes)-1]
	if err != nil {
		return nil, err
	}
	method := ArrayMethodAny
	if e.Name == "all" {
		method = ArrayMethodAll
	}
	return &ArrayPredicateExpression{Field: recv.field, Method: method, Param: param, Pred: body}, nil
}

// includes lowers `<field>.includes(<sub>)`.
func (l *lowerer) includes(e *ast.CallExpr) (ExpressionNode, error) {
	if len(e.Args) != 1 || len(e.Named) > 0 {
		return nil, l.refuse(e, ".includes() takes one string", "Write `"+ast.FormatExpr(e.Receiver)+".includes(\"...\")`")
	}
	recv, err := l.operand(e.Receiver)
	if err != nil {
		return nil, err
	}
	if recv.kind != operandField {
		return nil, l.inProcessMethodRefusal(e, nil, nil)
	}
	if recv.intrinsic != "" {
		return nil, l.refuse(e, fmt.Sprintf("`%s` is a row column, and a substring test runs on a payload string", ast.FormatExpr(e.Receiver)),
			"Compare the column with `==` or `in`")
	}
	if recv.typ == "list" {
		return nil, l.refuse(e, fmt.Sprintf("`%s` is a list, and `.includes()` tests a substring", ast.FormatExpr(e.Receiver)),
			"For list membership write `"+ast.FormatExpr(e.Args[0])+" in "+ast.FormatExpr(e.Receiver)+"`")
	}
	if recv.typ != "" && recv.typ != "string" && recv.typ != "datetime" {
		return nil, l.refuse(e, fmt.Sprintf("`%s` is a %s, and `.includes()` tests a string", ast.FormatExpr(e.Receiver), recv.typ),
			"Compare it with `==` instead")
	}
	arg, err := l.operand(e.Args[0])
	if err != nil {
		return nil, err
	}
	if arg.kind != operandValue {
		return nil, l.refuse(e, "the substring is a value, and this one reads the row", "Search for a value: `"+ast.FormatExpr(e.Receiver)+".includes(args.q)`")
	}
	switch arg.typ {
	case "", "string", "datetime":
	case "nil":
		return &constantBoolExpression{value: false, planConstant: true}, nil
	default:
		return nil, l.refuse(e, fmt.Sprintf(".includes() takes a string, and `%s` is a %s", ast.FormatExpr(e.Args[0]), arg.typ),
			"Search for a string: `"+ast.FormatExpr(e.Receiver)+".includes(\"...\")`")
	}
	if lit, ok := arg.value.(string); ok && strings.TrimSpace(lit) == "" {
		return &constantBoolExpression{value: false, planConstant: true}, nil
	}
	return &ComparisonExpression{Field: recv.field, Operator: OpIncludes, Value: arg.value}, nil
}

// ---------------------------------------------------------------------------
// Plan constants: what they may read
// ---------------------------------------------------------------------------

// checkPlanConstant validates a row-independent subtree before it becomes a
// plan constant: every name is one argument expansion binds, every argument
// is declared, every actor member exists, every call is a catalog function or
// method, and every node kind is one the position admits. A plan constant is
// evaluated at CALL time, so what is not caught here is an error on every
// call instead of one refusal at load.
func (l *lowerer) checkPlanConstant(n ast.ExpressionNode) error {
	var err error
	l.walkPlanConstant(n, map[string]bool{}, &err)
	return err
}

func (l *lowerer) walkPlanConstant(n ast.ExpressionNode, local map[string]bool, errp *error) {
	if *errp != nil || n == nil {
		return
	}
	if kind := ast.KindOf(n); kind != "" && tiers.KindAdmission(l.env.Position, kind) == tiers.Refused {
		*errp = l.refuse(n, fmt.Sprintf("%s is not admitted in %s", kind, positionPhrase(l.env.Position)), "Remove it, or evaluate it in a logic body and pass the result as an argument")
		return
	}
	switch e := n.(type) {
	case *ast.IdentExpr:
		if local[e.Name] {
			return
		}
		switch {
		case e.Name == "args":
			if l.env.Args == nil && l.env.Position == tiers.PositionSpecBody {
				*errp = l.refuse(e, "a spec or trait takes no arguments", "Read the row's fields, or move the comparison into the query that applies the predicate")
			}
		case lowerPlanConstantRoots[e.Name]:
		default:
			fix := "A predicate reads its parameter (" + l.rowParam() + "), args, actor, now and config"
			if _, isField := l.fields[e.Name]; isField {
				// The pre-v1 filter's bare payload field (D1): the fix is
				// mechanical, so the refusal carries it (D24).
				fix = "A payload field is read through the parameter: write `" + l.rowParam() + "." + e.Name + "`"
			}
			*errp = l.refuse(e, fmt.Sprintf("`%s` is not defined here", e.Name), fix)
		}
	case *ast.MemberExpr:
		if root, path, ok := simpleRootPath(e); ok && !local[root] {
			switch root {
			case "args":
				if l.env.Args == nil {
					if l.env.Position == tiers.PositionSpecBody {
						*errp = l.refuse(e, "a spec or trait takes no arguments", "Read the row's fields, or move the comparison into the query that applies the predicate")
					}
					return
				}
				if _, declared := l.env.Args[path[0]]; !declared {
					*errp = l.refuse(e, fmt.Sprintf("`args.%s` is not a declared argument", path[0]),
						"Declare it in the query's args block: `args { "+path[0]+" string }`")
				}
				return
			case "actor":
				if _, ok := auth.ActorEnvelopeCanonicalName(path[0]); !ok || len(path) > 1 {
					*errp = l.refuse(e, fmt.Sprintf("`actor.%s` is not a field of the actor envelope", strings.Join(path, ".")),
						"Read one of: "+auth.ActorEnvelopeValidNames())
				}
				return
			}
		}
		l.walkPlanConstant(e.Object, local, errp)
	case *ast.CallExpr:
		switch {
		case e.Kind != "":
			*errp = l.refuse(e, "a construct call has no place in a predicate", "Pass the value it computes as an argument")
			return
		case e.Receiver == nil:
			if _, ok := functions.Lookup(e.Name); !ok {
				*errp = l.refuse(e, fmt.Sprintf("`%s` is not a catalog function", e.Name), "See the function catalog for the functions a condition may call")
				return
			}
		default:
			_, onList := functions.Method(functions.TypeList, e.Name)
			_, onString := functions.Method(functions.TypeString, e.Name)
			if !onList && !onString {
				*errp = l.refuse(e, fmt.Sprintf("`.%s()` is not a method", e.Name), "See the function catalog for the methods a list and a string have")
				return
			}
			l.walkPlanConstant(e.Receiver, local, errp)
		}
		for _, a := range e.Args {
			l.walkPlanConstant(a, local, errp)
		}
	case *ast.LambdaExpr:
		inner := make(map[string]bool, len(local)+len(e.Params))
		for k, v := range local {
			inner[k] = v
		}
		for _, p := range e.Params {
			inner[p] = true
		}
		l.walkPlanConstant(e.Body, inner, errp)
	case *ast.UnaryExpr:
		l.walkPlanConstant(e.Operand, local, errp)
	case *ast.BinaryExpr:
		l.walkPlanConstant(e.Left, local, errp)
		l.walkPlanConstant(e.Right, local, errp)
	case *ast.TernaryExpr:
		l.walkPlanConstant(e.Condition, local, errp)
		l.walkPlanConstant(e.Then, local, errp)
		l.walkPlanConstant(e.Else, local, errp)
	case *ast.ListExpr:
		for _, el := range e.Elems {
			l.walkPlanConstant(el, local, errp)
		}
	case *ast.MapExpr:
		for _, en := range e.Entries {
			l.walkPlanConstant(en.Value, local, errp)
		}
	case *ast.ParenExpr:
		l.walkPlanConstant(e.Inner, local, errp)
	}
}

// ---------------------------------------------------------------------------
// Static types
// ---------------------------------------------------------------------------

// staticType is a row-independent node's type as far as the load can tell.
func (l *lowerer) staticType(n ast.ExpressionNode) string {
	switch e := n.(type) {
	case *ast.ParenExpr:
		return l.staticType(e.Inner)
	case *ast.LiteralExpr:
		return literalTypeWord(e.Value)
	case *ast.NilExpr:
		return "nil"
	case *ast.ListExpr:
		return "list"
	case *ast.MapExpr:
		return "map"
	case *ast.IdentExpr:
		switch e.Name {
		case "now":
			return "datetime"
		case "args", "actor", "config":
			return "map"
		}
	case *ast.MemberExpr:
		if root, path, ok := simpleRootPath(e); ok {
			switch root {
			case "args":
				return l.argPathType(path)
			case "actor":
				if len(path) == 1 {
					return actorTypeWord(path[0])
				}
			}
		}
	case *ast.CallExpr:
		if e.Kind != "" {
			return ""
		}
		if e.Receiver == nil {
			if fn, ok := functions.Lookup(e.Name); ok {
				return catalogTypeWord(fn.Returns)
			}
			return ""
		}
		listFn, onList := functions.Method(functions.TypeList, e.Name)
		strFn, onString := functions.Method(functions.TypeString, e.Name)
		switch {
		case onList && onString:
			if catalogTypeWord(listFn.Returns) == catalogTypeWord(strFn.Returns) {
				return catalogTypeWord(listFn.Returns)
			}
		case onList:
			return catalogTypeWord(listFn.Returns)
		case onString:
			return catalogTypeWord(strFn.Returns)
		}
	case *ast.UnaryExpr:
		if e.Op == "!" {
			return "bool"
		}
		return "number"
	case *ast.BinaryExpr:
		switch e.Op {
		case "==", "!=", "<", "<=", ">", ">=", "in", "startsWith", "&&", "||":
			return "bool"
		case "-", "*", "/", "%":
			return "number"
		case "+":
			lt, rt := l.staticType(e.Left), l.staticType(e.Right)
			if lt == "number" && rt == "number" {
				return "number"
			}
			if lt == "string" || rt == "string" {
				return "string"
			}
		case "??":
			if lt, rt := l.staticType(e.Left), l.staticType(e.Right); lt == rt {
				return lt
			}
		}
	case *ast.TernaryExpr:
		if tt, et := l.staticType(e.Then), l.staticType(e.Else); tt == et {
			return tt
		}
	}
	return ""
}

// argPathType is the type of `args.<path>`: the declared type for a top-level
// argument, unknown below it.
func (l *lowerer) argPathType(path []string) string {
	if len(path) != 1 || l.env.Args == nil {
		return ""
	}
	return argTypeWord(l.env.Args[path[0]])
}

// literalTypeWord is a literal value's type word.
func literalTypeWord(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "bool"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return "number"
	case nil:
		return "nil"
	}
	return ""
}

// declTypeWord maps a declared concept field type (flattenConceptFields'
// vocabulary) to a type word.
func declTypeWord(t string) string {
	switch {
	case t == "string" || t == "enum":
		return "string"
	case t == "datetime":
		return "datetime"
	case t == "int" || t == "float":
		return "number"
	case t == "bool":
		return "bool"
	case strings.HasPrefix(t, "[]"):
		return "list"
	case objectLikeType(t):
		return "map"
	}
	return ""
}

// objectLikeType reports whether a declared type is a keyed value a member
// read goes through: a closed block, a map, or a union.
func objectLikeType(t string) bool {
	return t == "object" || t == "variant" || strings.HasPrefix(t, "map[")
}

// argTypeWord maps an args-block type to a type word.
func argTypeWord(t ArgType) string {
	s := strings.ToLower(strings.TrimSpace(string(t)))
	switch {
	case s == "string" || s == "enum" || s == "id":
		return "string"
	case s == "datetime" || s == "date" || s == "timestamp":
		return "datetime"
	case s == "int" || s == "integer" || s == "float" || s == "number":
		return "number"
	case s == "bool" || s == "boolean":
		return "bool"
	case s == "array" || s == "list" || strings.HasPrefix(s, "[]"):
		return "list"
	case s == "object" || s == "map":
		return "map"
	}
	return ""
}

// actorTypeWord is an actor envelope field's type word.
func actorTypeWord(field string) string {
	canonical, ok := auth.ActorEnvelopeCanonicalName(field)
	if !ok {
		return ""
	}
	switch canonical {
	case "isClusterOwner":
		return "bool"
	case "now":
		return "datetime"
	}
	return "string"
}

// catalogTypeWord maps a catalog type word to the lowering's.
func catalogTypeWord(t string) string {
	switch t {
	case functions.TypeString, functions.TypeDuration:
		return "string"
	case functions.TypeDatetime:
		return "datetime"
	case functions.TypeNumber:
		return "number"
	case functions.TypeBool:
		return "bool"
	case functions.TypeList:
		return "list"
	case functions.TypeMap:
		return "map"
	}
	return ""
}
