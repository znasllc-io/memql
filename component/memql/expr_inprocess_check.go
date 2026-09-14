package memql

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// expr_inprocess_check.go -- the LOAD-time half of an in-process position
// (epic memql#5363, memql#5367).
//
// EvalExpr refuses at run time whatever it cannot evaluate: an unknown name,
// an unknown function, a call of the wrong shape, a construct call where the
// position hands no hook. For a value that is only ever evaluated when a
// caller calls, "at run time" means "on the first call, in production": the
// mutation templates' history is a list of exactly that -- a construct that
// passed memqllint and strict boot and died at render (memql#2909's class:
// memql#2925's shortId in an `id:` slot, memql#4746's actor reference inside
// a call). So a position that evaluates in process runs this check on every
// expression when the tree is read, and refuses there what EvalExpr would
// refuse on the first call:
//
//   - a node kind the position does not admit (the tier manifest's row);
//   - a bare name the position's scope does not bind -- where the string
//     evaluator wrote an unknown name out as a literal string, so a mistyped
//     reference stored its own spelling;
//   - a field of a closed-set root that does not exist (actor.userld);
//   - a root that has no whole value read whole (`actor` alone);
//   - a call to a name that is not a catalog function with an in-process
//     implementation, or to one with the wrong number or kind of arguments;
//   - a method name no receiver has;
//   - a static cost estimate above tiers.MaxStaticCost.
//
// What it cannot see is left to EvalExpr, which refuses it on the call: a
// value's runtime TYPE (whether `args.x` holds a string or a list is the
// caller's to decide), and a method whose receiver turns out not to have it.

// inProcessExprCheck is one in-process position's load-time check.
type inProcessExprCheck struct {
	// where names the position in a refusal ("a mutation value").
	where string
	// position is the tier manifest row node kinds and functions are judged
	// against.
	position tiers.Position
	// roots are the names the position's scope binds. `now`, the clock, is
	// bound everywhere EvalExpr runs and needs no entry.
	roots map[string]bool
	// fields validates `<root>.<field>` for a root whose fields are a closed
	// set; a root with no entry admits any field.
	fields map[string]func(field string) error
	// fieldOnly marks a root with no whole value: it is read one field at a
	// time and a bare read of it is refused.
	fieldOnly map[string]bool
}

// check runs the whole check on one expression.
func (c *inProcessExprCheck) check(n ast.ExpressionNode) error {
	if n == nil {
		return fmt.Errorf("%s has no expression", c.where)
	}
	if cost := EstimateCost(n); cost > tiers.MaxStaticCost {
		return fmt.Errorf("`%s` in %s has a static cost estimate of %d evaluations, above the limit of %d (tiers.MaxStaticCost): a lambda nested in another scans a list once per element of the other -- name the inner result as a separate step",
			ast.FormatExpr(n), c.where, cost, tiers.MaxStaticCost)
	}
	return c.walk(n, nil)
}

// walk checks n with params the lambda parameters in scope.
func (c *inProcessExprCheck) walk(n ast.ExpressionNode, params []string) error {
	kind := ast.KindOf(n)
	if kind == ast.KindUnknown {
		return fmt.Errorf("%s holds a %T, which is not an edition-2026 expression node", c.where, n)
	}
	if tiers.KindAdmission(c.position, kind) == tiers.Refused {
		return fmt.Errorf("`%s` is a %s, which %s does not admit", ast.FormatExpr(n), kind, c.where)
	}
	switch e := n.(type) {
	case *ast.IdentExpr:
		return c.name(e.Name, params, false)
	case *ast.MemberExpr:
		// A member read directly on a root is where a closed-set root's
		// field is judged; deeper reads are judged at their root.
		if id, ok := e.Object.(*ast.IdentExpr); ok && !exprCheckIsParam(id.Name, params) {
			if err := c.name(id.Name, params, true); err != nil {
				return err
			}
			if validate := c.fields[id.Name]; validate != nil {
				if err := validate(e.Field); err != nil {
					return fmt.Errorf("`%s` in %s: %w", ast.FormatExpr(e), c.where, err)
				}
			}
			return nil
		}
		return c.walk(e.Object, params)
	case *ast.CallExpr:
		return c.call(e, params)
	case *ast.UnaryExpr:
		return c.walk(e.Operand, params)
	case *ast.BinaryExpr:
		if err := c.walk(e.Left, params); err != nil {
			return err
		}
		return c.walk(e.Right, params)
	case *ast.TernaryExpr:
		for _, part := range []ast.ExpressionNode{e.Condition, e.Then, e.Else} {
			if err := c.walk(part, params); err != nil {
				return err
			}
		}
		return nil
	case *ast.ListExpr:
		for _, el := range e.Elems {
			if err := c.walk(el, params); err != nil {
				return err
			}
		}
		return nil
	case *ast.MapExpr:
		for _, en := range e.Entries {
			if err := c.walk(en.Value, params); err != nil {
				return err
			}
		}
		return nil
	case *ast.ParenExpr:
		return c.walk(e.Inner, params)
	case *ast.LambdaExpr:
		// A lambda is legal only as a method argument, which call() walks
		// itself; reaching one here means it stands on its own, and EvalExpr
		// refuses that (lambda_not_callable).
		return fmt.Errorf("`%s` in %s is a lambda standing on its own: a lambda is a method's argument, as in xs.where(x => ...)", ast.FormatExpr(e), c.where)
	}
	return nil
}

// name judges a bare name: a lambda parameter in scope, `now`, or a root the
// position binds. asObject is true when the name is the object of a member
// read, which is the only way a field-only root may be read.
func (c *inProcessExprCheck) name(name string, params []string, asObject bool) error {
	if exprCheckIsParam(name, params) || name == "now" {
		return nil
	}
	if !c.roots[name] {
		return fmt.Errorf("%s reads `%s`, which it does not bind: %s reads %s -- a payload or event field is always read through its root",
			c.where, name, c.where, c.rootList())
	}
	if c.fieldOnly[name] && !asObject {
		return fmt.Errorf("%s reads `%s` whole, and it has no whole value: read one field at a time, as in %s.userId", c.where, name, name)
	}
	return nil
}

// rootList names the roots a refusal offers, `now` included, sorted.
func (c *inProcessExprCheck) rootList() string {
	names := make([]string, 0, len(c.roots)+1)
	for r := range c.roots {
		names = append(names, r)
	}
	names = append(names, "now")
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// call judges a call: a construct call is refused wherever the manifest
// refuses it (checked by walk), a method must exist on some receiver, and a
// function must be a catalog function EvalExpr implements, called with the
// shape its signature takes.
func (c *inProcessExprCheck) call(e *ast.CallExpr, params []string) error {
	if e.Receiver != nil {
		_, onList := exprMethodEntry(functions.TypeList, e.Name)
		_, onString := exprMethodEntry(functions.TypeString, e.Name)
		if !onList && !onString {
			if replacement, retired := functions.RetiredMethods()[functions.TypeList+"."+e.Name]; retired {
				return fmt.Errorf("`%s` in %s: .%s() is retired in edition 2026: write %s", ast.FormatExpr(e), c.where, e.Name, replacement)
			}
			return fmt.Errorf("`%s` in %s: .%s() is not a method", ast.FormatExpr(e), c.where, e.Name)
		}
		if err := c.walk(e.Receiver, params); err != nil {
			return err
		}
		return c.callArgs(e, params)
	}
	if e.Kind != "" {
		// walk refuses a construct call where the manifest does; a position
		// that admits one runs it, and its arguments are values.
		return c.callArgs(e, params)
	}
	fn, inCatalog := exprCatalog[e.Name]
	if !inCatalog || fn.Receiver != "" {
		if replacement, retired := functions.RetiredFunctions()[e.Name]; retired {
			return fmt.Errorf("`%s` in %s: %s() is retired in edition 2026: write %s", ast.FormatExpr(e), c.where, e.Name, replacement)
		}
		return fmt.Errorf("`%s` in %s: %s() is not a function %s can call -- the catalog has no %s, and no spec or trait is applied here",
			ast.FormatExpr(e), c.where, e.Name, c.where, e.Name)
	}
	if _, implemented := exprFunctionImpls[e.Name]; !implemented {
		return fmt.Errorf("`%s` in %s: %s is a relationship traversal, which selects rows where a query filter runs and has no in-process value",
			ast.FormatExpr(e), c.where, fn.Signature())
	}
	if tiers.FunctionAdmission(c.position, fn.Key()) == tiers.Refused {
		return fmt.Errorf("`%s` in %s: %s() is not admitted here", ast.FormatExpr(e), c.where, e.Name)
	}
	if err := exprCheckShape(e, fn); err != nil {
		return fmt.Errorf("`%s` in %s: %w", ast.FormatExpr(e), c.where, err)
	}
	return c.callArgs(e, params)
}

// callArgs walks a call's arguments; a lambda argument's body is walked with
// its parameters in scope.
func (c *inProcessExprCheck) callArgs(e *ast.CallExpr, params []string) error {
	for _, a := range e.Args {
		if lam, ok := ast.Unparen(a).(*ast.LambdaExpr); ok && lam != nil {
			if e.Receiver == nil {
				return fmt.Errorf("`%s` in %s: only a method takes a lambda here", ast.FormatExpr(e), c.where)
			}
			inner := append(append([]string(nil), params...), lam.Params...)
			if err := c.walk(lam.Body, inner); err != nil {
				return err
			}
			continue
		}
		if err := c.walk(a, params); err != nil {
			return err
		}
	}
	for _, a := range e.Named {
		if err := c.walk(a.Value, params); err != nil {
			return err
		}
	}
	return nil
}

func exprCheckIsParam(name string, params []string) bool {
	for _, p := range params {
		if p == name {
			return true
		}
	}
	return false
}
