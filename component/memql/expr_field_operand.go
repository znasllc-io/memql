package memql

import (
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// expr_field_operand.go -- a comparison between two fields of the same row
// (epic memql#5363, task memql#5366): `row.overage > row.reportedToStripe`.
//
// # Why it exists
//
// The pre-v1 filter grammar had no way to say it. A bare name on the right of a
// comparison was read as a value, so deploy/fleet's hasUnreportedOverage --
// `overage > reportedToStripe` -- has compared every meter's overage against
// the LITERAL STRING "reportedToStripe" since it was written, and a number is
// never ordered against a string, so the billing sweep's input set has always
// been empty. Edition 2026 names every row read through the lambda parameter,
// so `row.reportedToStripe` is unambiguously a field, and this is its lowering.
//
// # Why it is a comparison VALUE and not a new node
//
// Every walker in this package already knows ComparisonExpression, and almost
// none of them look at its value beyond "is it an argument or an actor
// reference". A new node kind would have to be taught to each of them (the
// forty-four the walker table lists); a new value type is only visible where a
// value is COMPILED or MATCHED, and those are the places below. A walker that
// clones or prints a comparison carries the operand along untouched.
//
// # The rules, which are EvalExpr's `==` and `<` stated over two stored values
//
//   - `==`: ONE NOTION OF UNSET, then typed deep equality. Two unset sides are
//     equal (absent, JSON null and "" are one value), one unset side is not,
//     and two set sides are equal when their JSON values are: numbers
//     numerically across int and float, strings verbatim, lists and objects
//     deeply. SQL: jsonb equality, which is exactly that.
//   - `!=`: the two-valued negation of `==` (compileNotSQL), as everywhere.
//   - `< <= > >=`: two numbers numerically, two strings by byte (COLLATE "C"),
//     anything else -- an unset side, a type mismatch, two booleans -- false.
//     The casts sit behind CASE for the evaluation-order reason
//     compileJSONValueComparison gives.
//
// Both sides are payload paths or paths under the collection element in scope;
// an intrinsic column is compared with a value, never with another field, and
// the lowering refuses that shape at load.

// FieldOperand is a comparison value that is another field: the right-hand
// side of `row.a > row.b`. Field is a payload path (`payload.b`) or a path under
// the collection element in scope (`$elem.b`).
type FieldOperand struct {
	Field FieldReference
}

// fieldOperandSQL renders one side of a field comparison: its text extraction
// and its jsonb value. The shapes are exactly a collection predicate's source
// shapes, so this is arraySourceSQL under a name that says what it is for here.
func fieldOperandSQL(field FieldReference, scope *sqlElementScope) (string, string, error) {
	if !isArrayElementField(field) && !(len(field.Parts) >= 2 && strings.EqualFold(strings.TrimSpace(field.Parts[0]), "payload")) {
		return "", "", fmt.Errorf("a field comparison reads two payload fields (or two fields of the collection element); %q is neither", planConstantFieldLabel(field))
	}
	return arraySourceSQL(field, scope)
}

// compileFieldOperandComparison lowers `<field> <op> <field>`. scope is the
// collection element in scope (nil at row level).
func compileFieldOperandComparison(cmp *ComparisonExpression, fo *FieldOperand, scope *sqlElementScope) (compiledExpression, error) {
	lt, lj, err := fieldOperandSQL(cmp.Field, scope)
	if err != nil {
		return compiledExpression{}, err
	}
	rt, rj, err := fieldOperandSQL(fo.Field, scope)
	if err != nil {
		return compiledExpression{}, err
	}
	switch cmp.Operator {
	case OpEq:
		return compileFieldEquality(lt, lj, rt, rj), nil
	case OpNe:
		return compileNotSQL(compileFieldEquality(lt, lj, rt, rj)), nil
	case OpGt, OpGe, OpLt, OpLe:
		sqlOp, err := sqlOperatorForComparison(cmp.Operator)
		if err != nil {
			return compiledExpression{}, err
		}
		// Each WHEN names BOTH types, so a CAST only runs when both sides
		// are numbers; `COLLATE "C"` on the left decides the collation of the
		// comparison, as in compileTypedComparison.
		return compiledExpression{sql: fmt.Sprintf(
			"(CASE WHEN jsonb_typeof(%[1]s) = 'number' AND jsonb_typeof(%[2]s) = 'number' THEN (%[3]s)::numeric %[5]s (%[4]s)::numeric "+
				"WHEN jsonb_typeof(%[1]s) = 'string' AND jsonb_typeof(%[2]s) = 'string' THEN (%[3]s) COLLATE \"C\" %[5]s (%[4]s) "+
				"ELSE FALSE END)",
			lj, rj, lt, rt, sqlOp)}, nil
	default:
		return compiledExpression{}, fmt.Errorf("operator %q does not compare two fields; a field comparison is ==, !=, <, <=, > or >=", cmp.Operator)
	}
}

// compileFieldEquality is `==` between two fields: both unset, or equal as JSON.
// jsonb `=` is typed deep equality -- '1' = '1.0' is true, '"1"' = '1' is not,
// objects compare by key and lists by position -- which is exprStrictEqual. The
// COALESCE keeps the term two-valued when one side is absent (jsonb NULL = x is
// NULL), so it reads the same inside an OR as under compileNotSQL.
func compileFieldEquality(lt, lj, rt, rj string) compiledExpression {
	return compiledExpression{sql: fmt.Sprintf(
		"((COALESCE(%[1]s, '') = '' AND COALESCE(%[3]s, '') = '') OR COALESCE(%[2]s = %[4]s, FALSE))",
		lt, lj, rt, rj)}
}

// fieldOperandMatches is the in-process twin of compileFieldOperandComparison:
// both sides read from the row (or the element in scope), then EvalExpr's own
// `==` and ordering (exprEqual, exprOrdered) -- the SAME functions the
// in-process evaluator runs, so a refine clause and a filter cannot disagree
// about what two stored values compare to.
func fieldOperandMatches(node memorynodes.MemoryNode, cmp *ComparisonExpression, fo *FieldOperand, elem *arrayElementFrame, payloadCache map[string]map[string]any) (bool, error) {
	left, err := fieldOperandValue(node, cmp.Field, elem, payloadCache)
	if err != nil {
		return false, err
	}
	right, err := fieldOperandValue(node, fo.Field, elem, payloadCache)
	if err != nil {
		return false, err
	}
	switch cmp.Operator {
	case OpEq:
		return exprEqual(nil, nil, left, right), nil
	case OpNe:
		return !exprEqual(nil, nil, left, right), nil
	case OpGt, OpGe, OpLt, OpLe:
		return exprOrdered(string(cmp.Operator), left, right), nil
	default:
		return false, fmt.Errorf("operator %q does not compare two fields; a field comparison is ==, !=, <, <=, > or >=", cmp.Operator)
	}
}

// fieldOperandValue reads one side of a field comparison, absent as nil, and
// normalised into the value domain exprEqual and exprOrdered compare in.
func fieldOperandValue(node memorynodes.MemoryNode, field FieldReference, elem *arrayElementFrame, payloadCache map[string]map[string]any) (any, error) {
	if !isArrayElementField(field) && !(len(field.Parts) >= 2 && strings.EqualFold(strings.TrimSpace(field.Parts[0]), "payload")) {
		return nil, fmt.Errorf("a field comparison reads two payload fields (or two fields of the collection element); %q is neither", planConstantFieldLabel(field))
	}
	value, present, err := arrayPredicateSource(node, field, elem, payloadCache)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	normalized, err := exprNormalize(value)
	if err != nil {
		return nil, fmt.Errorf("field %s: %w", planConstantFieldLabel(field), err)
	}
	return normalized, nil
}

// canonicalFieldOperand renders a field operand for the result-cache signature:
// `field(payload.b)`, distinct from the string literal "payload.b" -- which
// canonicalValue quotes -- so a comparison against a field and one against the
// field's NAME never share a signature.
func canonicalFieldOperand(fo *FieldOperand) string {
	if fo == nil {
		return "field()"
	}
	return "field(" + canonicalField(fo.Field) + ")"
}
