package compiler

// value_leaf.go -- how a value expression is stored in a compiled step: an
// argument, a payload, a published event's body.

import "github.com/znasllc-io/memql/component/language/ast"

// exprLeafKey is the one key of a compiled value leaf that is an expression.
const exprLeafKey = "$expr"

// EncodeValueLeaf encodes one edition-2026 value expression as it is stored
// in a compiled value map -- a call's args, a payload, a body. It is THE
// encoding: the body compiler (body_compile.go) emits it and the automations
// runtime decodes it (component/automations: PrepareExpressions parses each
// leaf once, ResolveV1Value evaluates it).
//
// The rule, applied after stripping any ParenExpr wrappers:
//
//   - a LiteralExpr is its plain JSON value (a string, an int64 or float64, a
//     bool);
//   - a NilExpr is JSON null;
//   - a MapExpr is a JSON object whose every value is EncodeValueLeaf of the
//     entry, recursively -- a map with one expression value is still an object,
//     with a `$expr` leaf inside, never a whole-map `$expr`;
//   - a ListExpr is a JSON array of EncodeValueLeaf of each element;
//   - anything else is `{"$expr": ast.FormatExpr(n)}`.
//
// The decoder walks the maps and arrays and evaluates each `$expr` leaf with
// EvalExpr, applying EvalExpr's own container rule: a leaf that evaluates to
// the Absent sentinel omits its key or element, and an explicit nil stays
// null. Decoding EncodeValueLeaf(e) therefore yields what EvalExpr(e) yields
// (TestEncodeValueLeafRoundTrip, component/automations).
//
// n must be an edition-2026 node: a node of the internal query form has no
// canonical source. A nil node encodes as null.
func EncodeValueLeaf(n ast.ExpressionNode) any {
	if n == nil {
		return nil
	}
	switch e := ast.Unparen(n).(type) {
	case *ast.LiteralExpr:
		return e.Value
	case *ast.NilExpr:
		return nil
	case *ast.MapExpr:
		obj := make(map[string]any, len(e.Entries))
		for _, en := range e.Entries {
			obj[en.Key] = EncodeValueLeaf(en.Value)
		}
		return obj
	case *ast.ListExpr:
		arr := make([]any, len(e.Elems))
		for i, el := range e.Elems {
			arr[i] = EncodeValueLeaf(el)
		}
		return arr
	default:
		return map[string]any{exprLeafKey: ast.FormatExpr(e)}
	}
}
