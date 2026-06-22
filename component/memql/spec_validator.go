package memql

import (
	"fmt"
	"strings"
)

type specScope string

const (
	scopeGlobal specScope = "global"
	scopeInline specScope = "inline"
)

type specKey struct {
	scope specScope
	name  string
}

type specValidator struct {
	schemaIdx *schemaIndex
	globals   map[string]*Spec
	inline    map[string]*Spec
	resolved  map[specKey]ExpressionNode
	resolving map[specKey]struct{}
}

func newSpecValidator(idx *schemaIndex, globals, inline map[string]*Spec) *specValidator {
	return &specValidator{
		schemaIdx: idx,
		globals:   globals,
		inline:    inline,
		resolved:  make(map[specKey]ExpressionNode),
		resolving: make(map[specKey]struct{}),
	}
}

func (v *specValidator) validateGlobalSpec(name string) error {
	if v == nil {
		return fmt.Errorf("spec validator is not initialized")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("spec name is required")
	}
	_, err := v.resolveSpec(scopeGlobal, name)
	return err
}

func (v *specValidator) validateInlineSpec(name string) error {
	if v == nil {
		return fmt.Errorf("spec validator is not initialized")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("spec name is required")
	}
	_, err := v.resolveSpec(scopeInline, name)
	return err
}

func (v *specValidator) resolveExpression(expr ExpressionNode, allowInline bool) (ExpressionNode, error) {
	if v == nil {
		return cloneExpressionNode(expr), nil
	}
	return v.expandExpression(expr, allowInline)
}

func (v *specValidator) resolveSpec(scope specScope, name string) (ExpressionNode, error) {
	key := specKey{scope: scope, name: strings.TrimSpace(name)}
	if key.name == "" {
		return nil, fmt.Errorf("spec name is required")
	}
	if resolved, ok := v.resolved[key]; ok {
		return resolved, nil
	}
	if _, cycle := v.resolving[key]; cycle {
		return nil, fmt.Errorf("circular spec reference detected for %q", key.name)
	}
	v.resolving[key] = struct{}{}
	spec, err := v.lookupSpec(scope, key.name)
	if err != nil {
		delete(v.resolving, key)
		return nil, err
	}
	allowInline := scope == scopeInline
	resolvedExpr, err := v.expandExpression(spec.Expr, allowInline)
	if err != nil {
		delete(v.resolving, key)
		return nil, err
	}
	if err := ensureBooleanExpression(resolvedExpr); err != nil {
		delete(v.resolving, key)
		return nil, fmt.Errorf("spec %q must resolve to a boolean expression: %w", spec.Name, err)
	}
	if err := v.validateSchema(spec.Name, resolvedExpr); err != nil {
		delete(v.resolving, key)
		return nil, err
	}
	resolvedClone := cloneExpressionNode(resolvedExpr)
	v.resolved[key] = resolvedClone
	spec.Expr = cloneExpressionNode(resolvedExpr)
	delete(v.resolving, key)
	return resolvedClone, nil
}

func (v *specValidator) expandExpression(expr ExpressionNode, allowInline bool) (ExpressionNode, error) {
	if expr == nil {
		return nil, nil
	}
	switch node := expr.(type) {
	case *LogicalExpression:
		left, err := v.expandExpression(node.Left, allowInline)
		if err != nil {
			return nil, err
		}
		right, err := v.expandExpression(node.Right, allowInline)
		if err != nil {
			return nil, err
		}
		return &LogicalExpression{
			Op:    node.Op,
			Left:  left,
			Right: right,
		}, nil
	case *ComparisonExpression:
		return cloneExpressionNode(node), nil
	case *RelationshipExpression:
		target, err := v.expandExpression(node.Target, allowInline)
		if err != nil {
			return nil, err
		}
		return &RelationshipExpression{
			Function: node.Function,
			Target:   target,
		}, nil
	case *SpecReferenceExpression:
		scope, spec, err := v.lookupSpecForExpansion(node.Name, allowInline)
		if err != nil {
			return nil, err
		}
		if spec != nil && spec.UsesSI {
			return nil, fmt.Errorf("spec %q uses ai() and cannot be used in filter expressions", spec.Name)
		}
		resolved, err := v.resolveSpec(scope, node.Name)
		if err != nil {
			return nil, err
		}
		return cloneExpressionNode(resolved), nil
	default:
		return cloneExpressionNode(expr), nil
	}
}

func (v *specValidator) lookupSpec(scope specScope, name string) (*Spec, error) {
	key := strings.TrimSpace(name)
	if key == "" {
		return nil, fmt.Errorf("spec name is required")
	}
	switch scope {
	case scopeInline:
		if spec, ok := v.inline[key]; ok && spec != nil {
			return spec, nil
		}
		return nil, fmt.Errorf("inline spec %q not found", key)
	default:
		if spec, ok := v.globals[key]; ok && spec != nil {
			return spec, nil
		}
		return nil, fmt.Errorf("spec %q not found", key)
	}
}

func (v *specValidator) lookupSpecForExpansion(name string, allowInline bool) (specScope, *Spec, error) {
	key := strings.TrimSpace(name)
	if key == "" {
		return "", nil, fmt.Errorf("spec name is required")
	}
	if allowInline {
		if spec, ok := v.inline[key]; ok && spec != nil {
			return scopeInline, spec, nil
		}
	}
	if spec, ok := v.globals[key]; ok && spec != nil {
		return scopeGlobal, spec, nil
	}
	return "", nil, fmt.Errorf("unknown spec %q", key)
}

func (v *specValidator) validateSchema(specName string, expr ExpressionNode) error {
	if v == nil || expr == nil {
		return nil
	}
	usage := newSpecUsage()
	usage.collect(expr)

	// Specs must be concept-agnostic: reject any concept constraints.
	if len(usage.concepts) > 0 {
		return fmt.Errorf("spec %q must not constrain concept; specs are concept-agnostic and apply to any concept with matching fields", specName)
	}

	// Payload paths cannot be validated at load time since specs are concept-agnostic.
	// Validation happens at query time when the spec is applied to a specific concept.
	return nil
}

type specUsage struct {
	payloadPaths map[string][]string
	concepts     map[string]struct{}
}

func newSpecUsage() *specUsage {
	return &specUsage{
		payloadPaths: make(map[string][]string),
		concepts:     make(map[string]struct{}),
	}
}

func (u *specUsage) collect(expr ExpressionNode) {
	if expr == nil {
		return
	}
	switch node := expr.(type) {
	case *ComparisonExpression:
		u.observeComparison(node)
	case *LogicalExpression:
		u.collect(node.Left)
		u.collect(node.Right)
	case *RelationshipExpression:
		u.collect(node.Target)
	}
}

func (u *specUsage) observeComparison(expr *ComparisonExpression) {
	if expr == nil || len(expr.Field.Parts) == 0 {
		return
	}
	first := strings.ToLower(strings.TrimSpace(expr.Field.Parts[0]))
	switch first {
	case "concept":
		u.observeConceptConstraint(expr)
	case "payload":
		if len(expr.Field.Parts) > 1 {
			path := make([]string, len(expr.Field.Parts)-1)
			for i := 1; i < len(expr.Field.Parts); i++ {
				path[i-1] = strings.TrimSpace(expr.Field.Parts[i])
			}
			key := strings.Join(path, ".")
			if key != "" {
				u.payloadPaths[key] = path
			}
		}
	}
}

func (u *specUsage) observeConceptConstraint(expr *ComparisonExpression) {
	if expr == nil {
		return
	}
	switch expr.Operator {
	case OpEq:
		if value, ok := expr.Value.(string); ok {
			name := strings.TrimSpace(value)
			if name != "" {
				u.concepts[name] = struct{}{}
			}
		}
	case OpIn:
		values, ok := expr.Value.([]any)
		if !ok {
			return
		}
		for _, raw := range values {
			if text, ok := raw.(string); ok {
				name := strings.TrimSpace(text)
				if name != "" {
					u.concepts[name] = struct{}{}
				}
			}
		}
	}
}
