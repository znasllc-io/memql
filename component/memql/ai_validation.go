package memql

import "fmt"

func validateAIContext(expr ExpressionNode) error {
	if expr == nil {
		return nil
	}
	switch node := expr.(type) {
	case *AIExpression:
		return fmt.Errorf("ai() cannot be used in filter, join, sort, or group expressions; use it only in projection")
	case *LogicalExpression:
		if err := validateAIContext(node.Left); err != nil {
			return err
		}
		return validateAIContext(node.Right)
	case *RelationshipExpression:
		return validateAIContext(node.Target)
	default:
		return nil
	}
}
