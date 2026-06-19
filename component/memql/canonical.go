package memql

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func canonicalExpression(expr ExpressionNode) string {
	if expr == nil {
		return ""
	}

	switch node := expr.(type) {
	case *LogicalExpression:
		return canonicalLogicalExpression(node)
	case *ComparisonExpression:
		return canonicalComparisonExpression(node)
	case *RelationshipExpression:
		target := canonicalExpression(node.Target)
		return fmt.Sprintf("%s(%s)", strings.ToLower(string(node.Function)), target)
	case *SortExpression:
		target := canonicalExpression(node.Target)
		fields := canonicalSortFields(node.Fields)
		return fmt.Sprintf("sort(%s|%s)", target, strings.Join(fields, ","))
	case *SelectExpression:
		target := canonicalExpression(node.Target)
		fields := canonicalFieldReferenceStrings(node.Fields)
		return fmt.Sprintf("select(%s|%s)", target, strings.Join(fields, ","))
	case *CountExpression:
		return fmt.Sprintf("count(%s)", canonicalExpression(node.Target))
	case *SpecReferenceExpression:
		return fmt.Sprintf("spec(%s)", strings.ToLower(strings.TrimSpace(node.Name)))
	case *SIExpression:
		return canonicalSIExpression(node)
	case *FunctionCallExpression:
		return fmt.Sprintf("fn(%s)", strings.ToLower(strings.TrimSpace(node.Name)))
	case *BuiltinFunctionExpression:
		return fmt.Sprintf("builtin(%s)", strings.ToLower(strings.TrimSpace(node.Name)))
	default:
		return ""
	}
}

func canonicalLogicalExpression(expr *LogicalExpression) string {
	if expr == nil {
		return ""
	}

	operands := collectLogicalOperands(expr.Op, expr)
	canonicalChildren := make([]string, 0, len(operands))
	for _, operand := range operands {
		canonicalChildren = append(canonicalChildren, canonicalExpression(operand))
	}
	sort.Strings(canonicalChildren)
	return fmt.Sprintf("%s(%s)", strings.ToUpper(string(expr.Op)), strings.Join(canonicalChildren, ","))
}

func collectLogicalOperands(op LogicalOp, expr ExpressionNode) []ExpressionNode {
	logical, ok := expr.(*LogicalExpression)
	if !ok || logical.Op != op {
		return []ExpressionNode{expr}
	}
	left := collectLogicalOperands(op, logical.Left)
	right := collectLogicalOperands(op, logical.Right)
	return append(left, right...)
}

func canonicalComparisonExpression(expr *ComparisonExpression) string {
	if expr == nil {
		return ""
	}

	field := canonicalField(expr.Field)
	if expr.CacheHintSeconds != nil && strings.EqualFold(field, "concept") {
		field = fmt.Sprintf("%s@cache(%d)", field, *expr.CacheHintSeconds)
	}
	value := canonicalValue(expr.Value)
	return fmt.Sprintf("%s%s%s", field, string(expr.Operator), value)
}

func canonicalSIExpression(expr *SIExpression) string {
	if expr == nil || expr.Invocation == nil {
		return "si()"
	}
	args := []string{strconv.Quote(strings.TrimSpace(expr.Invocation.TemplateId))}
	if expr.Invocation.ProviderOverride != nil {
		if provider := strings.TrimSpace(*expr.Invocation.ProviderOverride); provider != "" {
			args = append(args, strconv.Quote(provider))
		}
	}
	if expr.Invocation.CacheSeconds != nil {
		args = append(args, fmt.Sprintf("%d", *expr.Invocation.CacheSeconds))
	}
	return fmt.Sprintf("si(%s)", strings.Join(args, ","))
}

func canonicalField(field FieldReference) string {
	if len(field.Parts) == 0 {
		if canonical, ok := canonicalIntrinsicFieldName(field.Raw); ok {
			return canonical
		}
		return strings.ToLower(strings.TrimSpace(field.Raw))
	}

	first := strings.TrimSpace(field.Parts[0])
	if canonical, ok := canonicalIntrinsicFieldName(first); ok {
		if len(field.Parts) == 1 {
			return canonical
		}
		parts := []string{canonical}
		for _, part := range field.Parts[1:] {
			parts = append(parts, strings.ToLower(strings.TrimSpace(part)))
		}
		return strings.Join(parts, ".")
	}

	parts := make([]string, len(field.Parts))
	for i, part := range field.Parts {
		parts[i] = strings.ToLower(strings.TrimSpace(part))
	}
	return strings.Join(parts, ".")
}

func canonicalValue(value any) string {
	switch v := value.(type) {
	case string:
		return strconv.Quote(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case int, int8, int16, int32, int64:
		return fmt.Sprintf("%v", v)
	case uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%v", v)
	case float32:
		return strconv.FormatFloat(float64(v), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case []any:
		return canonicalArrayValue(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func canonicalArrayValue(values []any) string {
	if len(values) == 0 {
		return "[]"
	}

	if collection, err := normalizeCollectionValues(values); err == nil && collection != nil {
		switch collection.kind {
		case valueKindString:
			items := append([]string(nil), collection.strings...)
			sort.Strings(items)
			quoted := make([]string, len(items))
			for i, item := range items {
				quoted[i] = strconv.Quote(item)
			}
			return fmt.Sprintf("[%s]", strings.Join(quoted, ","))
		case valueKindNumber:
			items := append([]float64(nil), collection.numbers...)
			sort.Float64s(items)
			stringified := make([]string, len(items))
			for i, item := range items {
				stringified[i] = strconv.FormatFloat(item, 'g', -1, 64)
			}
			return fmt.Sprintf("[%s]", strings.Join(stringified, ","))
		case valueKindBool:
			items := append([]bool(nil), collection.bools...)
			sort.Slice(items, func(i, j int) bool { return !items[i] && items[j] })
			stringified := make([]string, len(items))
			for i, item := range items {
				if item {
					stringified[i] = "true"
				} else {
					stringified[i] = "false"
				}
			}
			return fmt.Sprintf("[%s]", strings.Join(stringified, ","))
		}
	}

	canonical := make([]string, 0, len(values))
	for _, item := range values {
		canonical = append(canonical, canonicalValue(item))
	}
	sort.Strings(canonical)
	return fmt.Sprintf("[%s]", strings.Join(canonical, ","))
}

func canonicalSortFields(fields []SortField) []string {
	if len(fields) == 0 {
		return []string{"createdAt:desc"}
	}
	out := make([]string, len(fields))
	for i, field := range fields {
		name := canonicalSortFieldName(field.Field)
		dir := strings.ToLower(strings.TrimSpace(string(field.Direction)))
		if dir != string(SortDirectionAsc) {
			dir = string(SortDirectionDesc)
		}
		out[i] = fmt.Sprintf("%s:%s", name, dir)
	}
	return out
}

func canonicalSortFieldName(field string) string {
	if canonical, ok := canonicalIntrinsicFieldName(field); ok {
		return canonical
	}
	return strings.ToLower(strings.TrimSpace(field))
}

func canonicalFieldReferenceStrings(fields []FieldReference) []string {
	if len(fields) == 0 {
		return nil
	}
	result := make([]string, len(fields))
	for i, field := range fields {
		result[i] = canonicalField(field)
	}
	sort.Strings(result)
	return result
}
