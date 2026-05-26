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
