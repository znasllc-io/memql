package compiler

import (
	"fmt"

	"github.com/znasllc-io/memql/component/language/ast"
)

// CodeArgsUndeclared is the stable rule id of a read of `args.<name>` that the
// construct's args block does not declare (memql#3626): an undeclared argument
// reads absent when nothing supplies it, and bypasses @required, the type and
// every value annotation when something does.
const CodeArgsUndeclared = "args_undeclared"

// ArgsUndeclaredError is the refusal of an undeclared `args.<name>` read.
type ArgsUndeclaredError struct {
	Automation string
	// Where is the position the read sits in: "body", "trigger filter" or
	// "@loop until".
	Where string
	Field string
}

// Error names the construct, the position and the argument, with the fix and
// the rule id last in brackets.
func (e *ArgsUndeclaredError) Error() string {
	return fmt.Sprintf("automation %q: %s reads args.%s, which is not declared in its args block -- declare it there (`%s <type>`), or read an argument the block declares [%s]",
		e.Automation, e.Where, e.Field, e.Field, CodeArgsUndeclared)
}

// RuleCode is the refusal's stable rule id (baseloader.CodedRefusal).
func (e *ArgsUndeclaredError) RuleCode() string { return CodeArgsUndeclared }

// checkAutomationBindings checks the parsed statements, including nested
// bodies and modifier expressions, and the two lambdas an automation's header
// carries -- the trigger @filter and @loop's until -- which read the bound
// `args` exactly as a statement does (memql#5426: an undeclared `args.x` in a
// trigger filter loaded, and read absent on every fire). Lambda parameters are
// local bindings; comments and string contents are not reads. A precondition's
// check is the fourth such position; the automation loader lifts it out of the
// source before the parse, so it asks CheckArgsReads.
//
// The @actor rule is the body's: a trigger filter always sees the no-caller
// envelope (an event has no caller, memql#2801), so it is not a capability the
// automation exercises.
func checkAutomationBindings(def *ast.FunctionDef, automation *ast.AutomationDef) error {
	c := &bindingCheck{automation: def.Name, declared: declaredArgs(def.ArgsSchema)}
	for _, attr := range def.Attributes {
		if attr != nil && attr.Name == "actor" {
			c.actorDeclared = true
		}
	}
	if automation.Trigger != nil && automation.Trigger.FilterLambda != nil {
		c.expression("trigger filter", automation.Trigger.FilterLambda, nil)
	}
	if c.problem == nil && automation.Loop != nil && automation.Loop.Until != nil {
		c.expression("@loop until", automation.Loop.Until, nil)
	}
	if c.problem != nil || automation.Body == nil {
		return c.problem
	}
	ast.WalkBody(automation.Body.Statements, func(statement ast.BodyStatement) bool {
		for _, expr := range ast.StatementExpressions(statement) {
			c.expression("body", expr, nil)
		}
		return c.problem == nil
	})
	return c.problem
}

// CheckArgsReads holds one expression of an automation -- read at where, with
// the args block declaring the names in declared -- to the rule
// checkAutomationBindings holds its statements and header lambdas to: an
// `args.<name>` read the block does not declare is an ArgsUndeclaredError. It
// is for a position the parser does not hold, a precondition's check
// (memql#5426 review), so that every place an automation reads its bound args
// from answers to one rule and one code.
func CheckArgsReads(automation, where string, expr ast.ExpressionNode, declared map[string]bool) error {
	c := &bindingCheck{automation: automation, declared: declared, actorDeclared: true}
	c.expression(where, expr, nil)
	return c.problem
}

// declaredArgs is the set of names an args block declares.
func declaredArgs(schema *ast.ArgsSchema) map[string]bool {
	declared := map[string]bool{}
	if schema != nil {
		for _, field := range schema.Fields {
			if field != nil {
				declared[field.Name] = true
			}
		}
	}
	return declared
}

// bindingCheck is one automation's binding rules, and the first problem found.
type bindingCheck struct {
	automation    string
	declared      map[string]bool
	actorDeclared bool
	problem       error
}

// expression checks expr, read at where, with locals the lambda parameters in
// scope around it.
func (c *bindingCheck) expression(where string, expr ast.ExpressionNode, locals map[string]bool) {
	ast.WalkV1(expr, func(node ast.ExpressionNode) bool {
		if c.problem != nil {
			return false
		}
		switch n := node.(type) {
		case *ast.LambdaExpr:
			inner := map[string]bool{}
			for name := range locals {
				inner[name] = true
			}
			for _, name := range n.Params {
				inner[name] = true
			}
			c.expression(where, n.Body, inner)
			return false
		case *ast.IdentExpr:
			if n.Name == "actor" && !locals[n.Name] && !c.actorDeclared && where == "body" {
				c.problem = fmt.Errorf("automation %q reads the auth envelope but does not declare @actor; add @actor above the declaration", c.automation)
			}
		case *ast.MemberExpr:
			root, ok := ast.Unparen(n.Object).(*ast.IdentExpr)
			if ok && root.Name == "args" && !locals[root.Name] && !c.declared[n.Field] {
				c.problem = &ArgsUndeclaredError{Automation: c.automation, Where: where, Field: n.Field}
			}
		}
		return true
	})
}
