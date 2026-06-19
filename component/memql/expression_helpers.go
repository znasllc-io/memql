package memql

import (
	"fmt"
	"strings"
)

// literalTruthy reports the boolean value of a literal in predicate
// position. Mirrors specTruthy (expression_evaluator.go) so the SQL
// post-filter (nodeMatchesExpression) and the spec evaluator agree on
// what a bare literal means as a boolean constant (#1705): nil/false/""/0
// are falsy, everything else truthy.
func literalTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return strings.TrimSpace(t) != ""
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return fmt.Sprintf("%v", t) != "0"
	}
	return true
}

// ensureBooleanExpression walks an expression tree and rejects nodes
// that cannot evaluate to a boolean -- the contract every spec body
// must satisfy. Allowed leaves are comparison expressions, spec
// references, and SI expressions; logical (and/or) and relationship
// expressions are allowed structurally as long as their sub-trees
// are boolean.
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

// cloneExpressionNode produces a deep copy of an ExpressionNode tree.
// Used by the engine when handing AST fragments across registry /
// execution boundaries that need isolated ownership.
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
	case *CountExpression:
		return &CountExpression{
			Target: cloneExpressionNode(node.Target),
		}
	case *LiteralValueNode:
		// A literal value substituted in for an arg reference (#1705). The
		// Value is an opaque `any` -- a shallow copy is correct (the runtime
		// treats it as immutable, the same as ComparisonExpression.Value).
		// Before this case the default returned nil, silently nil-ing out
		// plan.Root whenever a logic body resolved to a literal -- the
		// memql#1090 / #1705 class of "query must include at least one
		// filter" / "unsupported expression node" failures.
		return &LiteralValueNode{Value: node.Value}
	case *constantBoolExpression:
		return &constantBoolExpression{value: node.value}
	case *CallerRefExpression:
		return &CallerRefExpression{}
	case *ErrorRefExpression:
		return &ErrorRefExpression{}
	case *ErrorExpression:
		return &ErrorExpression{Message: cloneExpressionNode(node.Message)}
	default:
		// EXHAUSTIVE over every ExpressionNode implementer (asserted by
		// TestCloneExpressionNode_ExhaustiveNodeCoverage, which builds one of
		// each and fails if clone returns nil). A nil here would silently drop
		// the node from plan.Root / a sub-tree and surface much later as an
		// opaque downstream error, so a new node type added without a case is
		// caught at test time rather than at runtime (#1705).
		return nil
	}
}
