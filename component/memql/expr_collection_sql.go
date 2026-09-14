package memql

import (
	"context"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// expr_collection_sql.go -- ArrayPredicateExpression: `.any(x => p)`,
// `.all(x => p)` and `.count() <op> n` over a ROW ARRAY FIELD, lowered to SQL
// (epic memql#5363, task memql#5366, design record D11).
//
// # Why this is its own node and not a CollectionMethodExpression
//
// CollectionMethodExpression is the IN-MEMORY collection surface: it runs a
// method over a value a logic body already holds. A filter cannot do that --
// the rows are in the database and the point of a pushdown position is that
// the database decides which ones come back. So a collection method over a row
// array field in a P-tier position lowers to `jsonb_array_elements` inside the
// WHERE clause, and this node is that lowering's IR. It is BOUNDED by
// construction (one row's array), which is what D11 requires of any collection
// method in the pushdown tier; a method over a query result is unbounded and
// never reaches here.
//
// # The element namespace
//
// Inside the element predicate the lambda's parameter is the ELEMENT, not the
// row, and an IR comparison names its operand with a FieldReference. So the
// element gets a pseudo-root, arrayElementRoot: `x == "a"` is a comparison on
// `$elem`, and `x.qty > 0` one on `$elem.qty`. `$` cannot begin a DSL
// identifier or a payload property, so the root can never collide with a real
// field, and the two passes that would otherwise rewrite it -- the bare-field
// payload prefixing (filterFieldRef) and the spec-binding rewrite
// (rewriteSpecFields) -- leave it alone by name. It always means the INNERMOST
// enclosing element: a nested predicate's Field is read in the enclosing scope
// and its Pred in its own, which is ordinary lexical scoping and the reason the
// SQL aliases the elements per depth (e, e1, e2, ...).
//
// # The rules, which the SQL and the in-process twin must reproduce row for row
//
// "Absent" is the absence table's: the key is missing OR its value is JSON
// null.
//
//	            absent   present non-array   array
//	any(p)      false    false               some element satisfies p
//	all(p)      true     false               every element satisfies p ([] -> true)
//	count()     0        0                   its length
//
// all() over an absent field is vacuously true -- there are no elements to
// fail -- while a present value that is not an array is a type mismatch, and a
// mismatch satisfies nothing. An element predicate that is unknown for an
// element (an absent element field) counts as false, in both directions: the
// element does not satisfy any(), and it DOES fail all(). SQL spells that with
// COALESCE, exactly as NotExpression does.
//
// # Evaluation order, which SQL does not promise
//
// jsonb_array_elements RAISES on a scalar ("cannot extract elements from a
// scalar"), and Postgres documents that it does not guarantee the order in
// which a WHERE clause evaluates the operands of AND. So the guard is not
// written `jsonb_typeof(p) = 'array' AND EXISTS (...)` -- a plan that ran the
// EXISTS first would fail the whole read on one malformed row. It is a CASE,
// the construct the documentation names for forcing evaluation order: the
// THEN branch, and the set-returning function in it, is only evaluated for an
// array.

// arrayElementRoot is the pseudo-field that names the current element of a
// collection predicate. See the file header.
const arrayElementRoot = "$elem"

// The three collection predicates the pushdown tier admits over a row array.
const (
	ArrayMethodAny   = "any"
	ArrayMethodAll   = "all"
	ArrayMethodCount = "count"
)

// ArrayPredicateExpression is `.any(x => Pred)`, `.all(x => Pred)` or
// `.count() CountOp CountValue` over the array at Field.
//
// Field is a payload path (`payload.tags`) or, inside another collection
// predicate, a path under the enclosing element (`$elem.tags`). Pred reads
// the element through arrayElementRoot and may also read the row (a payload
// or intrinsic comparison, which the SQL correlates to the outer row). Param
// is the lambda parameter as authored, kept for messages only: it is not part
// of the predicate's identity, so `t => t == "a"` and `x => x == "a"` render
// to one canonical signature. CountValue holds a number, or a
// *PlanConstExpression until argument expansion replaces it.
type ArrayPredicateExpression struct {
	Field      FieldReference
	Method     string
	Param      string
	Pred       ExpressionNode
	CountOp    ComparisonOperator
	CountValue any
}

func (*ArrayPredicateExpression) isExpressionNode() {}

// isArrayElementField reports whether a field names the current collection
// element rather than a row field.
func isArrayElementField(field FieldReference) bool {
	return len(field.Parts) > 0 && strings.TrimSpace(field.Parts[0]) == arrayElementRoot
}

// sqlElementScope is the element in scope while compiling an element
// predicate. A nil scope is the row itself.
type sqlElementScope struct {
	alias string
	depth int
}

// child returns the scope for a predicate nested one level deeper. The first
// level is `e`, matching the spelling the design record uses; deeper levels
// number themselves so an inner predicate can never shadow the outer alias.
func (s *sqlElementScope) child() *sqlElementScope {
	depth := 0
	if s != nil {
		depth = s.depth + 1
	}
	alias := "e"
	if depth > 0 {
		alias = fmt.Sprintf("e%d", depth)
	}
	return &sqlElementScope{alias: alias, depth: depth}
}

// jsonTextPathOn renders `<base> #>> '{a,b}'` -- the TEXT extraction every
// comparison compiler reads -- over an arbitrary jsonb base. An empty path is
// the base itself as text (`#>> '{}'`), which is how an element that is a
// scalar is read: a jsonb string extracts unquoted, a number as its digits,
// and JSON null as SQL NULL, exactly as `payload #>> '{f}'` does for a field.
func jsonTextPathOn(base string, path []string) (string, error) {
	segments := make([]string, len(path))
	for i, segment := range path {
		trimmed := strings.TrimSpace(segment)
		if trimmed == "" || !isSafePathSegment(trimmed) {
			return "", fmt.Errorf("path segment %q is invalid", segment)
		}
		// Escaped even though isSafePathSegment rejects quotes, for the same
		// reason buildJSONPathExpression does: it is what CodeQL recognises.
		segments[i] = strings.ReplaceAll(trimmed, "'", "''")
	}
	return fmt.Sprintf("%s #>> '{%s}'", base, strings.Join(segments, ",")), nil
}

// jsonbPathOn renders `<base>->'a'->'b'` -- the jsonb navigation the array
// and containment operators need -- over an arbitrary base.
func jsonbPathOn(base string, path []string) (string, error) {
	if len(path) == 0 {
		return base, nil
	}
	segments := make([]string, len(path))
	for i, segment := range path {
		trimmed := strings.TrimSpace(segment)
		if trimmed == "" || !isSafePathSegment(trimmed) {
			return "", fmt.Errorf("path segment %q is invalid", segment)
		}
		segments[i] = sqlStringLiteral(trimmed)
	}
	return base + "->" + strings.Join(segments, "->"), nil
}

// arrayPredicateSourceError is the refusal for a Field that is neither a
// payload path nor a path under the enclosing element. Shared by both halves
// so they refuse the same shapes in the same words.
func arrayPredicateSourceError(field FieldReference) error {
	return fmt.Errorf("a collection predicate lowers over a payload array field (or a field of the enclosing "+
		"element); %q is neither", planConstantFieldLabel(field))
}

// arraySourceSQL renders the text and jsonb spellings of a collection
// predicate's array.
func arraySourceSQL(field FieldReference, scope *sqlElementScope) (string, string, error) {
	switch {
	case isArrayElementField(field):
		if scope == nil {
			return "", "", fmt.Errorf("%q names a collection element outside a collection predicate", planConstantFieldLabel(field))
		}
		base := scope.alias + ".v"
		text, err := jsonTextPathOn(base, field.Parts[1:])
		if err != nil {
			return "", "", err
		}
		jsonb, err := jsonbPathOn(base, field.Parts[1:])
		if err != nil {
			return "", "", err
		}
		return text, jsonb, nil
	case len(field.Parts) >= 2 && strings.EqualFold(strings.TrimSpace(field.Parts[0]), "payload"):
		text, err := buildJSONPathExpression(field.Parts[1:])
		if err != nil {
			return "", "", err
		}
		jsonb, err := buildJSONBPathExpression(field.Parts[1:])
		if err != nil {
			return "", "", err
		}
		return text, jsonb, nil
	default:
		return "", "", arrayPredicateSourceError(field)
	}
}

// compileArrayPredicate lowers one collection predicate. scope is the element
// in scope where the predicate sits (nil at row level); its Pred compiles in a
// child scope.
func (e *MemQLEngine) compileArrayPredicate(ctx context.Context, n *ArrayPredicateExpression, conceptContext string, scope *sqlElementScope) (compiledExpression, error) {
	if n == nil {
		return compiledExpression{}, fmt.Errorf("collection predicate is nil")
	}
	textExpr, jsonbExpr, err := arraySourceSQL(n.Field, scope)
	if err != nil {
		return compiledExpression{}, err
	}
	switch n.Method {
	case ArrayMethodAny, ArrayMethodAll:
		if n.Pred == nil {
			return compiledExpression{}, fmt.Errorf("%s.%s() needs an element predicate", planConstantFieldLabel(n.Field), n.Method)
		}
		inner := scope.child()
		pred, ok := e.tryCompileCombinedFilterIn(ctx, n.Pred, conceptContext, inner)
		if !ok {
			return compiledExpression{}, fmt.Errorf("the element predicate of %s does not compile to SQL", canonicalExpression(n))
		}
		var sql string
		if n.Method == ArrayMethodAny {
			sql = fmt.Sprintf(
				"(CASE WHEN jsonb_typeof(%[1]s) = 'array' THEN EXISTS (SELECT 1 FROM jsonb_array_elements(%[1]s) AS %[2]s(v) WHERE COALESCE((%[3]s), FALSE)) ELSE FALSE END)",
				jsonbExpr, inner.alias, pred.sql)
		} else {
			// The ELSE is the absence rule: vacuously true when the field is
			// absent (the text extraction is NULL for a missing key AND for
			// JSON null), false when it holds something that is not an array.
			sql = fmt.Sprintf(
				"(CASE WHEN jsonb_typeof(%[1]s) = 'array' THEN NOT EXISTS (SELECT 1 FROM jsonb_array_elements(%[1]s) AS %[2]s(v) WHERE NOT COALESCE((%[3]s), FALSE)) ELSE (%[4]s) IS NULL END)",
				jsonbExpr, inner.alias, pred.sql, textExpr)
		}
		return compiledExpression{sql: sql, args: pred.args}, nil
	case ArrayMethodCount:
		sqlOp, err := sqlOperatorForComparison(n.CountOp)
		if err != nil {
			return compiledExpression{}, fmt.Errorf("%s.count(): %w", planConstantFieldLabel(n.Field), err)
		}
		count, err := arrayCountOperand(n.CountValue)
		if err != nil {
			return compiledExpression{}, fmt.Errorf("%s.count(): %w", planConstantFieldLabel(n.Field), err)
		}
		// jsonb_array_length raises on a non-array exactly as
		// jsonb_array_elements does, so the CASE hands it NULL instead, and
		// COALESCE turns that NULL into the 0 the rules table promises.
		return compiledExpression{
			sql: fmt.Sprintf("(COALESCE(jsonb_array_length(CASE WHEN jsonb_typeof(%[1]s) = 'array' THEN %[1]s END), 0) %[2]s ?)",
				jsonbExpr, sqlOp),
			args: []any{count},
		}, nil
	default:
		return compiledExpression{}, fmt.Errorf("collection predicate method %q is not one of any, all, count", n.Method)
	}
}

// compileElementComparison lowers a comparison on the element in scope. The
// operand is the element's text extraction and its jsonb value, and from there
// it is the SAME compiler a payload field uses (compileJSONValueComparison):
// one notion of unset, comparisons typed by the element's JSON type, `!=` as
// the two-valued negation of `==`, and string ordering by byte. An element is
// not a second kind of value.
func compileElementComparison(cmp *ComparisonExpression, scope *sqlElementScope) (compiledExpression, error) {
	if scope == nil {
		return compiledExpression{}, fmt.Errorf("%q names a collection element outside a collection predicate", planConstantFieldLabel(cmp.Field))
	}
	base := scope.alias + ".v"
	textExpr, err := jsonTextPathOn(base, cmp.Field.Parts[1:])
	if err != nil {
		return compiledExpression{}, err
	}
	jsonbExpr, err := jsonbPathOn(base, cmp.Field.Parts[1:])
	if err != nil {
		return compiledExpression{}, err
	}
	return compileJSONValueComparison(textExpr, jsonbExpr, cmp.Operator, cmp.Value)
}

// arrayCountOperand reads the number a count compares against.
func arrayCountOperand(value any) (float64, error) {
	kind, normalized, err := normalizeScalarValue(value)
	if err != nil {
		return 0, err
	}
	if kind != valueKindNumber {
		return 0, fmt.Errorf("count() compares with a number, got %T", value)
	}
	return normalized.(float64), nil
}

// ---------------------------------------------------------------------
// The in-process twin.
// ---------------------------------------------------------------------

// arrayElementFrame is the element in scope while matching an element
// predicate in process. A nil frame is the row itself.
type arrayElementFrame struct {
	value any
}

// nodeMatchesArrayPredicate is the in-process twin of compileArrayPredicate.
func nodeMatchesArrayPredicate(node memorynodes.MemoryNode, n *ArrayPredicateExpression, elem *arrayElementFrame, payloadCache map[string]map[string]any) (bool, error) {
	if n == nil {
		return false, fmt.Errorf("collection predicate is nil")
	}
	value, present, err := arrayPredicateSource(node, n.Field, elem, payloadCache)
	if err != nil {
		return false, err
	}
	return evalArrayPredicate(n, value, present, func(item any) (bool, error) {
		return nodeMatchesExpressionIn(node, n.Pred, &arrayElementFrame{value: item}, payloadCache)
	})
}

// arrayPredicateSource reads the value at a collection predicate's Field.
// present is false for both halves of "absent": a missing key and JSON null.
func arrayPredicateSource(node memorynodes.MemoryNode, field FieldReference, elem *arrayElementFrame, payloadCache map[string]map[string]any) (any, bool, error) {
	switch {
	case isArrayElementField(field):
		if elem == nil {
			return nil, false, fmt.Errorf("%q names a collection element outside a collection predicate", planConstantFieldLabel(field))
		}
		value, exists := elementValueAt(elem.value, field.Parts[1:])
		return value, exists && value != nil, nil
	case len(field.Parts) >= 2 && strings.EqualFold(strings.TrimSpace(field.Parts[0]), "payload"):
		payloadMap, err := cachedPayloadMap(node, payloadCache)
		if err != nil {
			return nil, false, err
		}
		if payloadMap == nil {
			return nil, false, nil
		}
		value, exists := valueAtPath(payloadMap, field.Parts[1:])
		return value, exists && value != nil, nil
	default:
		return nil, false, arrayPredicateSourceError(field)
	}
}

// evalArrayPredicate applies the rules table in the file header to a value
// already read. It is shared by every in-process evaluator of the node, so the
// table is written down exactly once; elemMatch decides one element.
func evalArrayPredicate(n *ArrayPredicateExpression, value any, present bool, elemMatch func(item any) (bool, error)) (bool, error) {
	var items []any
	isArray := false
	if present {
		items, isArray = payloadArrayElements(value)
	}
	switch n.Method {
	case ArrayMethodAny:
		if n.Pred == nil {
			return false, fmt.Errorf("%s.any() needs an element predicate", planConstantFieldLabel(n.Field))
		}
		if !isArray {
			return false, nil
		}
		for _, item := range items {
			match, err := elemMatch(item)
			if err != nil {
				return false, err
			}
			if match {
				return true, nil
			}
		}
		return false, nil
	case ArrayMethodAll:
		if n.Pred == nil {
			return false, fmt.Errorf("%s.all() needs an element predicate", planConstantFieldLabel(n.Field))
		}
		if !present {
			return true, nil
		}
		if !isArray {
			return false, nil
		}
		for _, item := range items {
			match, err := elemMatch(item)
			if err != nil {
				return false, err
			}
			if !match {
				return false, nil
			}
		}
		return true, nil
	case ArrayMethodCount:
		count := 0
		if isArray {
			count = len(items)
		}
		return compareArrayCount(count, n.CountOp, n.CountValue)
	default:
		return false, fmt.Errorf("collection predicate method %q is not one of any, all, count", n.Method)
	}
}

// compareArrayCount is the in-process twin of the count SQL.
func compareArrayCount(count int, op ComparisonOperator, value any) (bool, error) {
	want, err := arrayCountOperand(value)
	if err != nil {
		return false, err
	}
	have := float64(count)
	switch op {
	case OpEq:
		return have == want, nil
	case OpNe:
		return have != want, nil
	case OpGt:
		return have > want, nil
	case OpGe:
		return have >= want, nil
	case OpLt:
		return have < want, nil
	case OpLe:
		return have <= want, nil
	default:
		return false, fmt.Errorf("operator %q is not supported for count()", op)
	}
}

// elementMatchesComparison is the in-process twin of compileElementComparison:
// the element read at the path, then the same absence-and-comparison rules a
// payload field gets (matchJSONValue).
func elementMatchesComparison(elem *arrayElementFrame, cmp *ComparisonExpression) (bool, error) {
	if elem == nil {
		return false, fmt.Errorf("%q names a collection element outside a collection predicate", planConstantFieldLabel(cmp.Field))
	}
	value, exists := elementValueAt(elem.value, cmp.Field.Parts[1:])
	return matchJSONValue(value, exists, cmp.Operator, cmp.Value)
}

// elementValueAt reads a path under an element. The element itself always
// exists (a JSON-null element is present-and-null, which matchJSONValue reads
// as absent, as `#>> '{}'` does); a path through something that is not an
// object is absent, as `#>> '{a}'` on a scalar is NULL.
func elementValueAt(elem any, path []string) (any, bool) {
	if len(path) == 0 {
		return elem, true
	}
	current := elem
	for _, segment := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		value, ok := m[strings.TrimSpace(segment)]
		if !ok {
			return nil, false
		}
		current = value
	}
	return current, true
}
