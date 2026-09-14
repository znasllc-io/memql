package memql

import (
	"fmt"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// expr_condition_check.go -- D8's "a condition must be boolean", checked at
// LOAD for the in-process predicate positions that read a row (memql#5366).
//
// At a pushdown position Lower refuses a bare field of a declared non-boolean
// type used as a condition (`row => row.title` over a string), naming the
// type and the comparison that was probably meant. The in-process positions
// that also read a typed row -- a query's refine clause, an automation
// trigger's @filter -- had no such check, and at run time a STORED
// non-boolean in condition position is ruled "not true" (the corpus ruling):
// correct for data, silent for a mistake. So `refine row => row.title` on a
// string field emptied every page, and a trigger filter written that way
// never fired, and nothing said why. CheckConditionFields is that check, run
// where those positions load: the query lowering pass (checkQueryConstruct ->
// validateRefineIn) for a refine, the automation expression preparer for a
// trigger filter.
//
// It checks exactly what Lower's refusal covers, and no more: a plain read of
// a field of the row parameter whose type Lower would know (see
// conditionFieldType) and which is not boolean, in condition position -- the
// lambda body itself, an operand of `!`, `&&` or `||`, a ternary's condition,
// and a ternary's branches when the ternary is in condition position. A value
// position (a comparison's side, a method's argument) holds a value, and a
// read whose type the load cannot know is left to the runtime rule.

// CheckConditionFields refuses, at load, a bare field of lam's row parameter
// whose declared type on concept is not boolean and which is used as a
// condition. position names where the lambda sits (tiers.PositionQueryRefine,
// tiers.PositionTriggerFilter). A nil concept, or a lambda that is not one of
// one parameter, has nothing to check against and passes.
//
// The refusal is a *LowerError whose Span is the field's span in the lambda as
// parsed. A caller whose lambda was re-parsed from compiled text rather than
// from the author's file clears it, so a diagnostic never points at a column
// of text the author did not write.
func CheckConditionFields(lam *ast.LambdaExpr, concept *memoryNodes.Concept, position tiers.Position) error {
	if lam == nil || len(lam.Params) != 1 || concept == nil {
		return nil
	}
	fields, err := flattenConceptFields(concept)
	if err != nil {
		return nil
	}
	closed, err := closedObjectPaths(concept)
	if err != nil {
		return nil
	}
	param := lam.Params[0]
	var refusal error
	var visit func(n ast.ExpressionNode)
	visit = func(n ast.ExpressionNode) {
		if refusal != nil || n == nil {
			return
		}
		switch x := ast.Unparen(n).(type) {
		case *ast.UnaryExpr:
			if x.Op == "!" {
				visit(x.Operand)
			}
			return
		case *ast.BinaryExpr:
			if x.Op == "&&" || x.Op == "||" {
				visit(x.Left)
				visit(x.Right)
			}
			return
		case *ast.TernaryExpr:
			visit(x.Condition)
			visit(x.Then)
			visit(x.Else)
			return
		}
		segs, ok := rowFieldPath(n, param)
		if !ok {
			return
		}
		typ, source := conditionFieldType(segs, fields, closed, concept.Name)
		if typ == "" || typ == "bool" {
			return
		}
		text := ast.FormatExpr(n)
		refusal = &LowerError{Node: text, Position: position,
			Reason: fmt.Sprintf("`%s` is a %s (%s), and a condition must be boolean", text, typ, source),
			Fix:    notBooleanFix(text, typ), Span: nodeSpan(n)}
	}
	visit(lam.Body)
	return refusal
}

// rowFieldPath is the hops of a plain read of param's fields -- `row.title`,
// `row.?lineage.planId` -- or ok=false for anything else: a call, a method,
// another root, the row itself.
func rowFieldPath(n ast.ExpressionNode, param string) ([]string, bool) {
	m, ok := ast.Unparen(n).(*ast.MemberExpr)
	if !ok {
		return nil, false
	}
	root, segs, ok := memberChain(m)
	if !ok || root.Name != param || len(segs) == 0 {
		return nil, false
	}
	path := make([]string, len(segs))
	for i, s := range segs {
		path[i] = s.name
	}
	return path, true
}

// conditionFieldType is the type word of a row field read (string, number,
// datetime, list, map, bool) and where that type comes from, or "" when the
// load cannot know it. It answers as Lower's rowField types a read, so the
// two refusals cover the same reads: a row intrinsic has its column's type (a
// provenance leaf is a string; provenance itself, and a hop below any other
// column, is not a typed read); a payload path is typed only when every hop is
// declared and every hop but the last is a CLOSED object block. A hop through
// an untyped value, a map, a union or an open block leaves the rest of the
// path the author's to answer for, as Lower does; a hop through a scalar or a
// list, or a field the concept does not declare, is a different mistake that
// is not this check's to name.
func conditionFieldType(segs []string, fields map[string]conceptFieldShape, closed map[string]bool, conceptName string) (typ, source string) {
	if len(segs) == 0 {
		return "", ""
	}
	if info, ok := resolveIntrinsicField(segs[0]); ok {
		switch {
		case info.kind == intrinsicFieldProvenance && len(segs) == 2:
			return "string", "the row's provenance." + segs[1]
		case info.kind == intrinsicFieldProvenance || len(segs) > 1:
			return "", ""
		case info.kind == intrinsicFieldCreatedAt:
			return "datetime", "the row's createdAt column"
		}
		return "string", "the row's " + info.canonical + " column"
	}
	path := ""
	for i, seg := range segs {
		path = joinFieldPath(path, seg)
		shape, ok := fields[path]
		if !ok {
			return "", ""
		}
		if i == len(segs)-1 {
			return declTypeWord(shape.Type), fmt.Sprintf("declared `%s` on %s", shape.Type, conceptName)
		}
		if shape.Type != "object" || !closed[path] {
			return "", ""
		}
	}
	return "", ""
}
