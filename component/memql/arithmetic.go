package memql

import "fmt"

// arithmetic.go implements the engine side of #2316: binary arithmetic
// (`+ - * / %`) evaluated IN-MEMORY only. The parser produces a language
// ArithmeticExpr; the AST converter admits it ONLY in the logic /
// collection-lambda context (WithCollectionMethods) and rejects it in specs
// and query filters (no SQL pushdown). At evaluation time the operands are
// coerced numerically: when both sides are integers the result is int64
// (integer division / modulo); if either side is a float the result is
// float64. Division and modulo by zero are clear errors; `%` requires integer
// operands. `+` is numeric only here -- string concatenation stays the
// dedicated concat(...) builtin.

// ArithmeticExpression is the engine-side binary arithmetic node (#2316).
type ArithmeticExpression struct {
	Op    string
	Left  ExpressionNode
	Right ExpressionNode
}

func (*ArithmeticExpression) isExpressionNode() {}

// runtimeToInt64 coerces a value to int64, reporting ok only for the integer
// families (NOT float64 -- a float operand forces the float arithmetic path).
func runtimeToInt64(v any) (int64, bool) {
	switch val := v.(type) {
	case int:
		return int64(val), true
	case int8:
		return int64(val), true
	case int16:
		return int64(val), true
	case int32:
		return int64(val), true
	case int64:
		return val, true
	case uint:
		return int64(val), true
	case uint8:
		return int64(val), true
	case uint16:
		return int64(val), true
	case uint32:
		return int64(val), true
	case uint64:
		return int64(val), true
	default:
		return 0, false
	}
}

// evalArithmetic computes `lhs <op> rhs` under the #2316 numeric rules.
func evalArithmetic(op string, lhs, rhs any) (any, error) {
	if li, lok := runtimeToInt64(lhs); lok {
		if ri, rok := runtimeToInt64(rhs); rok {
			return intArithmetic(op, li, ri)
		}
	}
	lf, lok := runtimeToFloat64(lhs)
	if !lok {
		return nil, fmt.Errorf("left operand of %q is not numeric (%T)", op, lhs)
	}
	rf, rok := runtimeToFloat64(rhs)
	if !rok {
		return nil, fmt.Errorf("right operand of %q is not numeric (%T)", op, rhs)
	}
	return floatArithmetic(op, lf, rf)
}

func intArithmetic(op string, l, r int64) (any, error) {
	switch op {
	case "+":
		return l + r, nil
	case "-":
		return l - r, nil
	case "*":
		return l * r, nil
	case "/":
		if r == 0 {
			return nil, fmt.Errorf("division by zero")
		}
		return l / r, nil
	case "%":
		if r == 0 {
			return nil, fmt.Errorf("modulo by zero")
		}
		return l % r, nil
	default:
		return nil, fmt.Errorf("unsupported arithmetic operator %q", op)
	}
}

func floatArithmetic(op string, l, r float64) (any, error) {
	switch op {
	case "+":
		return l + r, nil
	case "-":
		return l - r, nil
	case "*":
		return l * r, nil
	case "/":
		if r == 0 {
			return nil, fmt.Errorf("division by zero")
		}
		return l / r, nil
	case "%":
		return nil, fmt.Errorf("modulo (%%) requires integer operands")
	default:
		return nil, fmt.Errorf("unsupported arithmetic operator %q", op)
	}
}
