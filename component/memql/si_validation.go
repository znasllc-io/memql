package memql

import "fmt"

func validateSIContext(expr ExpressionNode) error {
	if expr == nil {
		return nil
	}
	switch node := expr.(type) {
	case *SIExpression:
		return fmt.Errorf("si() cannot be used in filter, join, sort, or group expressions; use it only in projection")
	case *LogicalExpression:
		if err := validateSIContext(node.Left); err != nil {
			return err
		}
		return validateSIContext(node.Right)
	case *RelationshipExpression:
		return validateSIContext(node.Target)
	default:
		return nil
	}
}
