package memql

import (
	"fmt"
	"strconv"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
)

// textConcatOperand is the narrow string receiver admitted by includes:
// declared string/datetime payload fields joined with string literals. Each
// authored + subtree has a guaranteed string side, preserving + on missing
// and null fields. Numeric/list/object coercion and untyped fields are not
// admitted. Equivalence is defined for schema-valid string/null/absent values;
// this does not introduce a SQL serializer for corrupt arbitrary JSON values.
//
// It belongs to the existing comparison node so argument expansion, actor
// resolution and authorization walkers retain their ordinary behavior. Parts
// are immutable after lowering; neither receiver fields nor literals are
// caller expressions evaluated later.
type textConcatOperand struct {
	parts []textConcatPart
}

type textConcatPart struct {
	field   *FieldReference
	literal string
}

func (l *lowerer) textConcatReceiver(node ast.ExpressionNode) (*textConcatOperand, error) {
	var parts []textConcatPart
	var collect func(ast.ExpressionNode) (bool, error)
	collect = func(node ast.ExpressionNode) (bool, error) {
		node = ast.Unparen(node)
		if binary, ok := node.(*ast.BinaryExpr); ok {
			if binary.Op != "+" {
				return false, l.refuse(node, "includes() joins text with +, not row arithmetic", "Join declared text fields with a string separator")
			}
			left, err := collect(binary.Left)
			if err != nil {
				return false, err
			}
			right, err := collect(binary.Right)
			if err != nil {
				return false, err
			}
			if !left && !right {
				return false, l.refuse(node, "a text join needs a guaranteed string side so two missing fields do not become arithmetic", `Write (row.firstName + " " + row.lastName).includes(args.search, true)`)
			}
			return true, nil
		}
		if literal, ok := node.(*ast.LiteralExpr); ok {
			if value, ok := literal.Value.(string); ok {
				parts = append(parts, textConcatPart{literal: value})
				return true, nil
			}
			return false, l.refuse(node, "a text join accepts only string literals and declared text fields", "Use a string literal for the separator")
		}
		operand, err := l.operand(node)
		if err != nil {
			return false, err
		}
		if operand.kind != operandField || operand.intrinsic != "" || (operand.typ != "string" && operand.typ != "datetime") || len(operand.field.Parts) < 2 || operand.field.Parts[0] != "payload" {
			return false, l.refuse(node, "a text join accepts only declared string/datetime payload fields and string literals", "Join fields declared as text; keep numeric, list, object and untyped values out of this filter")
		}
		field := operand.field
		parts = append(parts, textConcatPart{field: &field})
		return false, nil // an optional text field may be absent at runtime
	}
	if _, err := collect(node); err != nil {
		return nil, err
	}
	return &textConcatOperand{parts: parts}, nil
}

func (value *textConcatOperand) canonical() string {
	parts := make([]string, 0, len(value.parts))
	for _, part := range value.parts {
		if part.field != nil {
			parts = append(parts, canonicalField(*part.field))
		} else {
			parts = append(parts, strconv.Quote(part.literal))
		}
	}
	return "textConcat(" + strings.Join(parts, ",") + ")"
}

// mapFields uses copy-on-write because clones of a registered predicate share
// this immutable operand. Even a mapper that edits Parts in place must not
// change the original plan or another caller's receiver.
func (value *textConcatOperand) mapFields(mapper func(FieldReference) (FieldReference, error)) (*textConcatOperand, error) {
	parts := append([]textConcatPart(nil), value.parts...)
	for i := range parts {
		if parts[i].field == nil {
			continue
		}
		field := *parts[i].field
		field.Parts = append([]string(nil), field.Parts...)
		mapped, err := mapper(field)
		if err != nil {
			return nil, err
		}
		parts[i].field = &mapped
	}
	return &textConcatOperand{parts: parts}, nil
}

func compileTextConcatIncludes(cmp *ComparisonExpression) (compiledExpression, error) {
	if cmp.Operator != OpIncludes && cmp.Operator != OpIncludesFold {
		return compiledExpression{}, fmt.Errorf("a joined text receiver is supported only by includes")
	}
	needle, ok, err := includesNeedle(cmp.Value)
	if err != nil {
		return compiledExpression{}, err
	}
	if !ok {
		return compiledExpression{sql: "FALSE"}, nil
	}
	parts := make([]string, 0, len(cmp.TextConcat.parts))
	var args []any
	for _, part := range cmp.TextConcat.parts {
		if part.field == nil {
			parts = append(parts, "?::text")
			args = append(args, part.literal)
			continue
		}
		text, _, err := fieldOperandSQL(*part.field, nil)
		if err != nil {
			return compiledExpression{}, err
		}
		parts = append(parts, "COALESCE("+text+", '')")
	}
	receiver, search := "("+strings.Join(parts, " || ")+")", "?::text"
	if cmp.Operator == OpIncludesFold {
		receiver, search = "lower("+receiver+")", "lower("+search+")"
	}
	return compiledExpression{sql: "(strpos(" + receiver + ", " + search + ") > 0)", args: append(args, needle)}, nil
}

func matchTextConcatIncludes(node memorynodes.MemoryNode, cmp *ComparisonExpression, payloadCache map[string]map[string]any) (bool, error) {
	payload, err := cachedPayloadMap(node, payloadCache)
	if err != nil {
		return false, err
	}
	var text strings.Builder
	for _, part := range cmp.TextConcat.parts {
		if part.field == nil {
			text.WriteString(part.literal)
			continue
		}
		value, _ := valueAtPath(payload, part.field.Parts[1:])
		text.WriteString(exprText(value))
	}
	return compareScalarValues(text.String(), cmp.Operator, cmp.Value)
}
