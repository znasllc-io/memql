package memql

import (
	"fmt"
	"strings"
)

func parseStandaloneExpression(input string) (ExpressionNode, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return nil, fmt.Errorf("expression is empty")
	}
	tokens, err := tokenize(trimmed)
	if err != nil {
		return nil, err
	}
	p := newParser(tokens, nil)
	expr, err := p.parse()
	if err != nil {
		return nil, err
	}
	if p.peek().typ != tokEOF {
		return nil, p.errorf(p.peek(), "unexpected token %q after expression", p.peek().literal)
	}
	return expr, nil
}

func ensureBooleanExpression(expr ExpressionNode) error {
	if expr == nil {
		return fmt.Errorf("expression is required")
	}
	switch node := expr.(type) {
	case *LogicalExpression:
		if err := ensureBooleanExpression(node.Left); err != nil {
			return err
		}
		return ensureBooleanExpression(node.Right)
	case *ComparisonExpression:
		return nil
	case *RelationshipExpression:
		return ensureBooleanExpression(node.Target)
	case *SpecReferenceExpression:
		return nil
	case *SIExpression:
		return nil
	default:
		return fmt.Errorf("expression node %T is not allowed inside a spec", expr)
	}
}

func detectSIUsage(expr ExpressionNode) bool {
	if expr == nil {
		return false
	}
	switch node := expr.(type) {
	case *SIExpression:
		return true
	case *LogicalExpression:
		return detectSIUsage(node.Left) || detectSIUsage(node.Right)
	case *RelationshipExpression:
		return detectSIUsage(node.Target)
	default:
		return false
	}
}

func cloneExpressionNode(expr ExpressionNode) ExpressionNode {
	if expr == nil {
		return nil
	}
	switch node := expr.(type) {
	case *LogicalExpression:
		return &LogicalExpression{
			Op:    node.Op,
			Left:  cloneExpressionNode(node.Left),
			Right: cloneExpressionNode(node.Right),
		}
	case *ComparisonExpression:
		clone := *node
		clone.Field = FieldReference{
			Raw:      node.Field.Raw,
			Parts:    append([]string(nil), node.Field.Parts...),
			Wildcard: node.Field.Wildcard,
		}
		if node.CacheHintSeconds != nil {
			value := *node.CacheHintSeconds
			clone.CacheHintSeconds = &value
		}
		if len(node.FieldSelections) > 0 {
			clone.FieldSelections = copyFieldReferences(node.FieldSelections)
		}
		return &clone
	case *RelationshipExpression:
		return &RelationshipExpression{
			Function: node.Function,
			Target:   cloneExpressionNode(node.Target),
		}
	case *SpecReferenceExpression:
		return &SpecReferenceExpression{Name: node.Name}
	case *BuiltinFunctionExpression:
		clone := &BuiltinFunctionExpression{
			Name:     node.Name,
			Executor: node.Executor,
		}
		if len(node.Args) > 0 {
			clone.Args = make(map[string]any, len(node.Args))
			for k, v := range node.Args {
				clone.Args[k] = v
			}
		}
		return clone
	case *SIExpression:
		return &SIExpression{
			Invocation: cloneSIInvocation(node.Invocation),
		}
	case *FunctionCallExpression:
		argsCopy := make(map[string]any)
		for k, v := range node.Args {
			argsCopy[k] = v
		}
		return &FunctionCallExpression{
			Name: node.Name,
			Args: argsCopy,
		}
	case *ArgRefExpression:
		return &ArgRefExpression{Path: node.Path}
	case *ConditionalFilterExpression:
		return &ConditionalFilterExpression{
			ArgPath: node.ArgPath,
			Filter:  cloneExpressionNode(node.Filter),
		}
	case *ShapeExpression:
		return &ShapeExpression{
			Target:        cloneExpressionNode(node.Target),
			Template:      node.Template, // Templates are typically immutable data structures
			TemplateName:  node.TemplateName,
			IncludeBundle: node.IncludeBundle,
		}
	case *SortExpression:
		fieldsCopy := make([]SortField, len(node.Fields))
		copy(fieldsCopy, node.Fields)
		return &SortExpression{
			Target: cloneExpressionNode(node.Target),
			Fields: fieldsCopy,
		}
	case *PaginateExpression:
		clone := &PaginateExpression{
			Target: cloneExpressionNode(node.Target),
		}
		if node.Limit != nil {
			limit := *node.Limit
			clone.Limit = &limit
		}
		if node.Offset != nil {
			offset := *node.Offset
			clone.Offset = &offset
		}
		return clone
	case *SelectExpression:
		return &SelectExpression{
			Target: cloneExpressionNode(node.Target),
			Fields: copyFieldReferences(node.Fields),
		}
	case *TimestampExpression:
		clone := &TimestampExpression{
			Target:    cloneExpressionNode(node.Target),
			UseLatest: node.UseLatest,
		}
		if node.Timestamp != nil {
			ts := *node.Timestamp
			clone.Timestamp = &ts
		}
		return clone
	case *DepthExpression:
		return &DepthExpression{
			Target: cloneExpressionNode(node.Target),
			Depth:  node.Depth,
		}
	default:
		return nil
	}
}

func inlineDefinitionsToSpecs(defs []inlineSpecDefinition) (map[string]*Spec, error) {
	if len(defs) == 0 {
		return nil, nil
	}
	specs := make(map[string]*Spec, len(defs))
	for _, def := range defs {
		name := strings.TrimSpace(def.Name)
		if err := validateSpecName(name); err != nil {
			return nil, err
		}
		if def.Expr == nil {
			return nil, fmt.Errorf("inline spec %q expression is required", name)
		}
		if err := ensureBooleanExpression(def.Expr); err != nil {
			return nil, fmt.Errorf("inline spec %q must be boolean: %w", name, err)
		}
		if _, exists := specs[name]; exists {
			return nil, fmt.Errorf("inline spec %q defined multiple times", name)
		}
		source := canonicalExpression(def.Expr)
		specs[name] = &Spec{
			Name:       name,
			ExprSource: source,
			Expr:       def.Expr,
			UsesSI:     detectSIUsage(def.Expr),
			Origin:     fmt.Sprintf("inline:%s", name),
		}
	}
	return specs, nil
}
