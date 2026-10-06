package memql

import "github.com/znasllc-io/memql/component/actions/capability"

// ReplayReadOnly proves that a registered query or builtin only reads. It is
// deliberately stricter than authorization: unknown executors, missing
// callees, cycles, model calls and unclassified IR nodes are not proof. Logic
// bodies are classified by their statement executor, not by their name.
// This is admission evidence for the loaded definitions, not an idempotency
// receipt or a guarantee that a second read returns the same value.
func ReplayReadOnly(fns *FunctionRegistry, specs *SpecRegistry, name string) bool {
	if fns == nil {
		return false
	}
	c := replayReadCheck{functions: fns.LookupIndex(), visiting: map[string]bool{}}
	if specs != nil {
		c.specs = specs.LookupIndex()
	}
	return c.function(name)
}

type replayReadCheck struct {
	functions map[string]*Function
	specs     map[string]*Spec
	visiting  map[string]bool
}

func (c *replayReadCheck) function(name string) bool {
	fn := c.functions[name]
	key := "function:" + name
	if fn == nil || c.visiting[key] {
		return false
	}
	c.visiting[key] = true
	defer delete(c.visiting, key)
	if fn.IsBuiltin() {
		return replayReadExecutor(fn.Executor)
	}
	if fn.FunctionKind != "" && fn.FunctionKind != "query" {
		return false
	}
	return fn.Expr != nil && c.expression(fn.Expr)
}

func replayReadExecutor(executor string) bool {
	if CheckBuiltinPreview(executor) == nil {
		return true
	}
	class, known := capability.CapabilityClass(executor)
	return known && class == capability.ClassRead
}

func (c *replayReadCheck) expression(expr ExpressionNode) bool {
	switch n := expr.(type) {
	case nil:
		return true // optional child; a missing function body is refused above
	case *FunctionCallExpression:
		return c.function(n.Name) && c.value(n.Args)
	case *BuiltinFunctionExpression:
		return replayReadExecutor(n.Executor) && c.value(n.Args)
	case *SpecReferenceExpression:
		spec := c.specs[n.Name]
		key := "spec:" + n.Name
		if spec == nil || spec.UsesAI || spec.Expr == nil || c.visiting[key] {
			return false
		}
		c.visiting[key] = true
		defer delete(c.visiting, key)
		return c.expression(spec.Expr)
	case *LogicalExpression:
		return c.expression(n.Left) && c.expression(n.Right)
	case *NotExpression:
		return c.expression(n.Target)
	case *ComparisonExpression:
		return c.value(n.Value)
	case *ArrayPredicateExpression:
		return c.expression(n.Pred) && c.value(n.CountValue)
	case *RelationshipExpression:
		return c.expression(n.Target)
	case *SortExpression:
		return c.expression(n.Target)
	case *PaginateExpression:
		return c.expression(n.Target)
	case *SelectExpression:
		return c.expression(n.Target)
	case *TimestampExpression:
		return c.expression(n.Target)
	case *DepthExpression:
		return c.expression(n.Target)
	case *CountExpression:
		return c.expression(n.Target)
	case *ConditionalFilterExpression:
		return c.expression(n.Filter)
	case *ArithmeticExpression:
		return c.expression(n.Left) && c.expression(n.Right)
	case *BinaryComparisonExpression:
		return c.expression(n.Left) && c.expression(n.Right)
	case *DotAccessExpression:
		return c.expression(n.Object)
	case *LambdaExpression:
		return c.expression(n.Body)
	case *CollectionMethodExpression:
		if !c.expression(n.Receiver) {
			return false
		}
		for _, arg := range n.Args {
			if !c.expression(arg) {
				return false
			}
		}
		return true
	case *ErrorExpression:
		return c.expression(n.Message)
	case *LiteralValueNode:
		return c.value(n.Value)
	case *ArgRefExpression, *CallerRefExpression, *constantBoolExpression,
		*AccountScopeExpression, *ReadFloorExpression:
		return true
	case *PlanConstExpression:
		// A plan constant admits only the in-process expression catalog.
		return true
	default:
		// In particular, an AI predicate may call tools. Refine and shape
		// templates require their own transitive proof before admission.
		return false
	}
}

func (c *replayReadCheck) value(v any) bool {
	switch x := v.(type) {
	case ExpressionNode:
		return c.expression(x)
	case map[string]any:
		for _, item := range x {
			if !c.value(item) {
				return false
			}
		}
	case []any:
		for _, item := range x {
			if !c.value(item) {
				return false
			}
		}
	}
	return true
}
