package memql

import (
	"fmt"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// expr_position_admission_check.go -- the LOAD-time half of the tier
// manifest's node-kind admission rule, for a position whose body is
// EVALUATED rather than lowered to SQL (memql#5582).
//
// Lower enforces node-kind admission for the two pushdown positions (a query
// filter, a spec or trait body) at load, walking the tree once and refusing
// the first node its position does not admit; validateRefine does the same
// for a refine clause (component/memql/refine.go). A trigger filter -- also
// an in-process, tier-M position, also governed by tiers.inProcessKinds --
// had no such walk: EvalCondition enforces the SAME rule (constructCall is
// Refused there, same as here), but only when a hook is missing at the first
// FIRE (construct_call_not_allowed, expr_eval.go's constructCall), which
// means a refusal, and the scheduler's warning, on every matching event
// instead of once at load.
//
// CheckPositionAdmission is that walk. It descends a lambda's whole body and
// refuses the first node whose kind the position's tier-manifest row refuses
// (tiers.KindAdmission) -- named the same way Lower and validateRefine name
// one: the node as the author wrote it, the position, and the nearest fix,
// as a *LowerError. Every kind tiers.inProcessKinds and tiers.bodyKinds admit
// is Admitted except a construct call (manifest.go: "a construct call is the
// work of a statement... and these positions are not statements"), so in
// practice the node this refuses is always a construct call; the check stays
// driven by the manifest rather than hardcoded to that one kind, so it tracks
// the manifest if that ever changes.
func CheckPositionAdmission(lam *ast.LambdaExpr, position tiers.Position) error {
	if lam == nil {
		return nil
	}
	var refusal error
	var walk func(n ast.ExpressionNode)
	walk = func(n ast.ExpressionNode) {
		if refusal != nil || n == nil {
			return
		}
		if kind := ast.KindOf(n); kind != "" && tiers.KindAdmission(position, kind) == tiers.Refused {
			refusal = positionAdmissionRefusal(n, position, kind)
			return
		}
		switch x := n.(type) {
		case *ast.MemberExpr:
			walk(x.Object)
		case *ast.CallExpr:
			if x.Receiver != nil {
				walk(x.Receiver)
			}
			for _, a := range x.Args {
				walk(a)
			}
			for _, a := range x.Named {
				walk(a.Value)
			}
		case *ast.UnaryExpr:
			walk(x.Operand)
		case *ast.BinaryExpr:
			walk(x.Left)
			walk(x.Right)
		case *ast.TernaryExpr:
			walk(x.Condition)
			walk(x.Then)
			walk(x.Else)
		case *ast.ListExpr:
			for _, el := range x.Elems {
				walk(el)
			}
		case *ast.MapExpr:
			for _, en := range x.Entries {
				walk(en.Value)
			}
		case *ast.ParenExpr:
			walk(x.Inner)
		case *ast.LambdaExpr:
			walk(x.Body)
		}
	}
	walk(lam.Body)
	return refusal
}

// positionAdmissionRefusal builds the refusal for node n of kind at
// position. For the construct-call case -- the only one inProcessKinds and
// bodyKinds ever refuse today -- its wording deliberately echoes EvalExpr's
// own construct_call_not_allowed ("%s %s(...) cannot be called from this
// position", e.Kind, e.Name) up front: the same phrase, so a reader who has
// seen the fire-time refusal recognises the load-time one. Node already
// carries the construct itself ("<kind> <name>(...)", via ast.FormatExpr),
// so the sentence adds what the fire-time refusal cannot: the position it
// was called from and what to do instead. A kind manifest.go does not refuse
// this way today falls back to the manifest's own vocabulary, so the
// function stays honest if that ever changes.
//
// The Fix text names a STEP, which assumes an automation-shaped position
// (a trigger filter, an automation condition, a before-write value): the
// only caller today is the trigger filter. A future caller for a position
// with no step of its own (a prompt input) would need its own Fix wording.
func positionAdmissionRefusal(n ast.ExpressionNode, position tiers.Position, kind ast.NodeKind) error {
	reason := fmt.Sprintf("%s is not admitted in %s", kind, positionPhrase(position))
	if call, ok := n.(*ast.CallExpr); ok && call.Kind != "" {
		reason = fmt.Sprintf("cannot be called from this position: a construct call is the work of a statement, and %s is not one -- a call there would read or write with no step of its own",
			positionPhrase(position))
	}
	return &LowerError{
		Node:     ast.FormatExpr(n),
		Position: position,
		Reason:   reason,
		Fix:      "Move the call into a step of the automation's body",
		Code:     LowerCodeRefused,
		Span:     nodeSpan(n),
	}
}
