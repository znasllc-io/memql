package memql

import "fmt"

// narrowExpandedQueryPage composes a caller's page size with the one cap in
// a named query's OUTER directive chain. expandFunctionCall has already
// cloned this tree and checked the construct's access/argument gates. Replace
// the cap in that clone before planning so SQL LIMIT, cursor emission and the
// cache key all see the same minimum; never trim a larger page after reading.
//
// A second cap inside the declaration remains an error. Do not traverse into
// predicates or relationship targets: those are not outer page directives.
func narrowExpandedQueryPage(expr ExpressionNode, outer *int) (bool, error) {
	if outer == nil || *outer <= 0 {
		return false, fmt.Errorf("paginate() requires a positive limit")
	}
	var page *PaginateExpression
	for expr != nil {
		switch node := expr.(type) {
		case *PaginateExpression:
			if page != nil {
				return false, nil // preserve applyDirectiveWrappers' duplicate error
			}
			if node.Limit == nil || *node.Limit <= 0 {
				return false, fmt.Errorf("paginate() requires a positive limit")
			}
			page, expr = node, node.Target
		case *SortExpression:
			expr = node.Target
		case *SelectExpression:
			expr = node.Target
		case *TimestampExpression:
			expr = node.Target
		case *DepthExpression:
			expr = node.Target
		case *CountExpression:
			expr = node.Target
		case *ShapeExpression:
			expr = node.Target
		case *RefineExpression:
			expr = node.Target
		default:
			expr = nil
		}
	}
	if page == nil {
		return false, nil
	}
	limit := min(*outer, *page.Limit)
	page.Limit = &limit
	return true, nil
}
