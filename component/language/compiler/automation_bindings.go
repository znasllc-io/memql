package compiler

import (
	"fmt"

	"github.com/znasllc-io/memql/component/language/ast"
)

// checkAutomationBindings checks the parsed statements, including nested
// bodies and modifier expressions. Lambda parameters are local bindings;
// comments and string contents are not reads.
func checkAutomationBindings(def *ast.FunctionDef, body *ast.Body) error {
	actorDeclared := false
	for _, attr := range def.Attributes {
		if attr != nil && attr.Name == "actor" {
			actorDeclared = true
		}
	}
	declared := map[string]bool{}
	if def.ArgsSchema != nil {
		for _, field := range def.ArgsSchema.Fields {
			if field != nil {
				declared[field.Name] = true
			}
		}
	}
	var problem error
	var expression func(ast.ExpressionNode, map[string]bool)
	expression = func(expr ast.ExpressionNode, locals map[string]bool) {
		ast.WalkV1(expr, func(node ast.ExpressionNode) bool {
			if problem != nil {
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
				expression(n.Body, inner)
				return false
			case *ast.IdentExpr:
				if n.Name == "actor" && !locals[n.Name] && !actorDeclared {
					problem = fmt.Errorf("automation %q reads the auth envelope but does not declare @actor; add @actor above the declaration", def.Name)
				}
			case *ast.MemberExpr:
				root, ok := ast.Unparen(n.Object).(*ast.IdentExpr)
				if ok && root.Name == "args" && !locals[root.Name] && !declared[n.Field] {
					problem = fmt.Errorf("automation %q: body reads args.%s, which is not declared in its args block", def.Name, n.Field)
				}
			}
			return true
		})
	}
	ast.WalkBody(body.Statements, func(statement ast.BodyStatement) bool {
		for _, expr := range ast.StatementExpressions(statement) {
			expression(expr, nil)
		}
		return problem == nil
	})
	return problem
}
