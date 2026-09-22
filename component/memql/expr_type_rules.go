package memql

import (
	"fmt"
	"sort"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// expr_type_rules.go -- the TYPE RULES, checked where the IN-PROCESS
// positions load (memql#5522).
//
// # The question this answers
//
// The differential lane holds memql.Lower and memql.EvalExpr equal on what
// they ANSWER. It never held them equal on what they ACCEPT: an expression
// the lowering refused at load was logged and skipped, on the stated position
// that "the refusal IS its answer, and the corpus pins refusals".
//
// That position is right for most of what Lower refuses and wrong for two
// cases, and the codes now carry the difference. LowerCodeRefused means "no
// form at this position" -- an in-process function, arithmetic over the row --
// and those are exactly what a refine clause exists to evaluate, so the
// in-process side must accept them. A TYPE RULE means the expression is
// wrong: booleans have no order, and one `in` tests one type. Those are facts
// about the language, true wherever the expression is written.
//
// Measured before it was fixed, and the measurement is why this file exists
// rather than a comment saying the position holds:
//
//	filter row => row.value in [1, "1"]   ->  refused at load
//	refine row => row.value in [1, "1"]   ->  loaded, and answered TRUE for
//	                                          the stored number 1 AND for the
//	                                          stored string "1"
//	refine row => row.?obj.a > true       ->  loaded, and answered false with
//	                                          no error, on every row
//
// So one source text meant two different things depending on which clause it
// sat in, and only the SQL side's reading was pinned anywhere.
//
// # Why this is a SECOND implementation and not a shared helper
//
// The obvious tidy-up -- one function both Lower and this call -- would make
// the lane's new arm vacuous. The whole value of a differential lane is two
// implementations arriving at one answer independently; a single shared
// function turns "the two agree" into "x == x". Lower's arms also read types
// the lowerer has already resolved through its operand machinery, which this
// file reaches a different way (conditionFieldType, the resolver
// CheckConditionFields uses). Held equal by the lane over generated
// expressions, exactly as D7 holds the answers equal.
//
// # What it checks, and no more
//
// The same two rules Lower's refusals cover:
//
//   - an ordering comparison (`<` `<=` `>` `>=`) where either side is a
//     boolean: a `true`/`false` literal, or a field of the row whose declared
//     type on the bound concept is boolean;
//   - an `in` whose right-hand list LITERAL mixes literal types.
//
// A side whose type the load cannot know is left alone, and so is a list
// built at run time: this refuses what is knowable statically, which is the
// same bound Lower works under. Unset members (`nil`, `""`) are not a type --
// literalListKinds skips them -- so `[1, nil]` is one type, not two.

// CheckTypeRules refuses, at load, an expression that breaks one of the
// language's type rules. lam is the lambda as parsed; concept is the bound
// concept, or nil when there is none (a trait, an engine-free validation) --
// with nil, only literal-typed sides are decidable and field-typed ones pass,
// the same way CheckConditionFields degrades.
//
// position names where the lambda sits (tiers.PositionQueryRefine,
// tiers.PositionTriggerFilter), so a diagnostic says which clause it is about.
//
// The refusal is a *LowerError carrying the type-rule Code, which is what the
// differential lane's accept-parity arm reads.
func CheckTypeRules(lam *ast.LambdaExpr, concept *memoryNodes.Concept, position tiers.Position) error {
	if lam == nil || len(lam.Params) != 1 {
		return nil
	}
	c := newTypeRuleChecker(lam.Params[0], concept, position)
	c.visit(lam.Body)
	return c.refusal
}

// CheckTypeRulesIn is CheckTypeRules over a bare expression rather than a
// lambda, for a caller that already holds the body and the parameter name --
// the differential lane, which generates both.
func CheckTypeRulesIn(body ast.ExpressionNode, param string, concept *memoryNodes.Concept, position tiers.Position) error {
	if body == nil || strings.TrimSpace(param) == "" {
		return nil
	}
	c := newTypeRuleChecker(param, concept, position)
	c.visit(body)
	return c.refusal
}

type typeRuleChecker struct {
	param    string
	position tiers.Position
	// fields and closed are the concept's flattened shape, empty when there
	// is no concept -- which makes every field-typed side undecidable and
	// therefore passed over.
	fields      map[string]conceptFieldShape
	closed      map[string]bool
	conceptName string
	refusal     error
}

func newTypeRuleChecker(param string, concept *memoryNodes.Concept, position tiers.Position) *typeRuleChecker {
	c := &typeRuleChecker{param: param, position: position}
	if concept == nil {
		return c
	}
	fields, err := flattenConceptFields(concept)
	if err != nil {
		return c
	}
	closed, err := closedObjectPaths(concept)
	if err != nil {
		return c
	}
	c.fields, c.closed, c.conceptName = fields, closed, concept.Name
	return c
}

// visit walks every node, because a type rule is broken wherever it is
// written -- inside a `!`, a ternary branch, a method argument or a nested
// lambda alike. That is the difference from CheckConditionFields, which
// descends only through CONDITION positions because its rule is about what a
// condition may be.
func (c *typeRuleChecker) visit(n ast.ExpressionNode) {
	if c.refusal != nil || n == nil {
		return
	}
	switch x := ast.Unparen(n).(type) {
	case *ast.BinaryExpr:
		c.checkBinary(x)
		c.visit(x.Left)
		c.visit(x.Right)
	case *ast.UnaryExpr:
		c.visit(x.Operand)
	case *ast.TernaryExpr:
		c.visit(x.Condition)
		c.visit(x.Then)
		c.visit(x.Else)
	case *ast.CallExpr:
		c.visit(x.Receiver)
		for _, a := range x.Args {
			c.visit(a)
		}
		for _, a := range x.Named {
			c.visit(a.Value)
		}
	case *ast.MemberExpr:
		c.visit(x.Object)
	case *ast.LambdaExpr:
		c.visit(x.Body)
	case *ast.ListExpr:
		for _, e := range x.Elems {
			c.visit(e)
		}
	case *ast.MapExpr:
		for _, en := range x.Entries {
			c.visit(en.Value)
		}
	}
}

func (c *typeRuleChecker) checkBinary(x *ast.BinaryExpr) {
	switch x.Op {
	case "<", "<=", ">", ">=":
		c.checkOrdering(x)
	case "in":
		c.checkMembership(x)
	}
}

// checkOrdering refuses an ordering comparison against a boolean.
func (c *typeRuleChecker) checkOrdering(x *ast.BinaryExpr) {
	for _, side := range []ast.ExpressionNode{x.Left, x.Right} {
		if !c.isBoolean(side) {
			continue
		}
		text := ast.FormatExpr(x)
		other := ast.FormatExpr(x.Left)
		if c.isBoolean(x.Left) {
			other = ast.FormatExpr(x.Right)
		}
		c.refusal = &LowerError{
			Node:     text,
			Position: c.position,
			Reason:   "booleans are not ordered",
			Fix:      "Compare with `==` or `!=`: `" + other + " == true`",
			Code:     LowerCodeNotOrdered,
			Span:     nodeSpan(x),
		}
		return
	}
}

// checkMembership refuses an `in` over a mixed-type list literal.
func (c *typeRuleChecker) checkMembership(x *ast.BinaryExpr) {
	list, ok := ast.Unparen(x.Right).(*ast.ListExpr)
	if !ok {
		return
	}
	seen := map[string]bool{}
	for _, el := range list.Elems {
		lit, ok := ast.Unparen(el).(*ast.LiteralExpr)
		if !ok {
			// A member the load cannot type makes the whole list
			// undecidable: refusing on a partial reading would refuse lists
			// Lower accepts.
			return
		}
		if isUnsetLiteral(lit.Value) {
			// Not a type. `[1, nil]` holds one.
			continue
		}
		seen[literalTypeWord(lit.Value)] = true
	}
	if len(seen) <= 1 {
		return
	}
	kinds := make([]string, 0, len(seen))
	for k := range seen {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	c.refusal = &LowerError{
		Node:     ast.FormatExpr(x),
		Position: c.position,
		Reason:   fmt.Sprintf("a membership list holds one type, and `%s` mixes %s", ast.FormatExpr(x.Right), strings.Join(kinds, " and ")),
		Fix:      "Split it into one `in` per type, joined with `||`",
		Code:     LowerCodeMixedMembershipList,
		Span:     nodeSpan(x),
	}
}

// isBoolean reports whether the load can tell this side is a boolean: a
// literal `true`/`false`, or a field of the row parameter whose declared type
// on the bound concept is boolean.
//
// Undecidable answers false, deliberately. Refusing what cannot be typed
// would refuse expressions Lower lowers, and the two have to accept the same
// set for the lane's arm to mean anything.
func (c *typeRuleChecker) isBoolean(n ast.ExpressionNode) bool {
	switch x := ast.Unparen(n).(type) {
	case *ast.LiteralExpr:
		_, ok := x.Value.(bool)
		return ok
	}
	if c.fields == nil {
		return false
	}
	segs, ok := rowFieldPath(n, c.param)
	if !ok {
		return false
	}
	typ, _ := conditionFieldType(segs, c.fields, c.closed, c.conceptName)
	return typ == "bool"
}
