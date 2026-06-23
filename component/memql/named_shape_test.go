package memql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests cover the named-shape resolution path end to end, the piece of
// machinery that a production regression ("empty `data` on every query*() that
// uses a named shape") showed was entirely untested. The chain is:
//
//	shape(filter, "name") -> parser -> ShapeExpression{TemplateName}
//	                        -> parseShapeExpressionString (string -> shapeTemplate)
//	                        -> convertShapeDefinitionTemplate (map -> shapeTemplate)
//	                        -> resolveNamedShape (registry lookup + convert)
//	                        -> applyShapeTemplate at runtime

// --- parseShapeExpressionString ----------------------------------------------

// parseShapeExpressionString receives the output of the relaxed shape parser,
// which stores node() calls as escaped strings (e.g., `node(\"id\")`). If the
// JSON-strict fallback fires instead, it receives the unescaped form
// (`node("id")`). Both have to round-trip to a shapeNodeFunc with the right
// FieldReference parts -- else every field quietly renders as nil at runtime.
func TestParseShapeExpressionString_EscapedFromRelaxedParser(t *testing.T) {
	tmpl, err := parseShapeExpressionString(`node(\"id\")`)
	require.NoError(t, err)
	fn, ok := tmpl.(*shapeNodeFunc)
	require.Truef(t, ok, "expected shapeNodeFunc, got %T", tmpl)
	require.Len(t, fn.Fields, 1)
	require.Equal(t, "id", fn.Fields[0].Raw)
	require.Equal(t, []string{"id"}, fn.Fields[0].Parts)
}

func TestParseShapeExpressionString_UnescapedFromJSONFallback(t *testing.T) {
	tmpl, err := parseShapeExpressionString(`node("id")`)
	require.NoError(t, err)
	fn, ok := tmpl.(*shapeNodeFunc)
	require.True(t, ok)
	require.Len(t, fn.Fields, 1)
	require.Equal(t, "id", fn.Fields[0].Raw)
}

func TestParseShapeExpressionString_NestedPath(t *testing.T) {
	tmpl, err := parseShapeExpressionString(`node(\"payload.partitionId\")`)
	require.NoError(t, err)
	fn, ok := tmpl.(*shapeNodeFunc)
	require.True(t, ok)
	require.Len(t, fn.Fields, 1)
	require.Equal(t, "payload.partitionId", fn.Fields[0].Raw)
	require.Equal(t, []string{"payload", "partitionId"}, fn.Fields[0].Parts)
}

func TestParseShapeExpressionString_MultipleFields(t *testing.T) {
	tmpl, err := parseShapeExpressionString(`node(\"id\", \"payload.name\")`)
	require.NoError(t, err)
	fn, ok := tmpl.(*shapeNodeFunc)
	require.True(t, ok)
	require.Len(t, fn.Fields, 2)
	require.Equal(t, "id", fn.Fields[0].Raw)
	require.Equal(t, "payload.name", fn.Fields[1].Raw)
}

func TestParseShapeExpressionString_EmptyFallsThroughToLiteral(t *testing.T) {
	tmpl, err := parseShapeExpressionString("")
	require.NoError(t, err)
	lit, ok := tmpl.(*shapeLiteral)
	require.True(t, ok)
	require.Equal(t, "", lit.Value)
}

func TestParseShapeExpressionString_NonNodeCallBecomesLiteral(t *testing.T) {
	// Strings that aren't `node(...)` calls are preserved as literal values.
	tmpl, err := parseShapeExpressionString("production")
	require.NoError(t, err)
	lit, ok := tmpl.(*shapeLiteral)
	require.True(t, ok)
	require.Equal(t, "production", lit.Value)
}

// --- convertShapeDefinitionTemplate ------------------------------------------

// convertShapeDefinitionTemplate is the bridge between the loaded shape
// definition (a map[string]any from either relaxed or JSON parsing) and the
// compiled runtime shapeTemplate tree. The test covers the three input flavors
// it handles: function-call strings, literal strings, and nested objects.
func TestConvertShapeDefinitionTemplate_MixedFieldTypes(t *testing.T) {
	tmpl := map[string]any{
		"id":             `node(\"id\")`,
		"displayName":    `node(\"payload.displayName\")`,
		"lifecycleState": "production", // literal fallback
		"nested": map[string]any{
			"partitionId": `node(\"payload.partitionId\")`,
		},
	}

	converted, err := convertShapeDefinitionTemplate(tmpl)
	require.NoError(t, err)

	obj, ok := converted.(*shapeObject)
	require.True(t, ok)
	require.Len(t, obj.Fields, 4)

	idNode, ok := obj.Fields["id"].(*shapeNodeFunc)
	require.True(t, ok)
	require.Equal(t, "id", idNode.Fields[0].Raw)

	nameNode, ok := obj.Fields["displayName"].(*shapeNodeFunc)
	require.True(t, ok)
	require.Equal(t, "payload.displayName", nameNode.Fields[0].Raw)

	lit, ok := obj.Fields["lifecycleState"].(*shapeLiteral)
	require.True(t, ok)
	require.Equal(t, "production", lit.Value)

	nestedObj, ok := obj.Fields["nested"].(*shapeObject)
	require.True(t, ok)
	spaceNode, ok := nestedObj.Fields["partitionId"].(*shapeNodeFunc)
	require.True(t, ok)
	require.Equal(t, "payload.partitionId", spaceNode.Fields[0].Raw)
}

// --- resolveNamedShape + shape loader -----------------------------------------

// TestResolveNamedShape_EndToEnd exercises the engine-level resolver against a
// minimal in-memory ShapeRegistry. This is the exact lookup path hit at
// execution time via engine.Execute when plan.ShapeTemplateName is non-empty.
func TestResolveNamedShape_EndToEnd(t *testing.T) {
	registry := newShapeRegistry()
	require.NoError(t, registry.add(&ShapeDefinition{
		Name: "utteranceFull",
		Template: map[string]any{
			"id":      `node(\"id\")`,
			"partitionId": `node(\"payload.partitionId\")`,
			"text":    `node(\"payload.text\")`,
		},
	}))

	engine := &MemQLEngine{shapes: registry}
	resolved, err := engine.resolveNamedShape("utteranceFull")
	require.NoError(t, err)
	require.NotNil(t, resolved)

	obj, ok := resolved.(*shapeObject)
	require.True(t, ok)
	require.Contains(t, obj.Fields, "id")
	require.Contains(t, obj.Fields, "partitionId")
	require.Contains(t, obj.Fields, "text")

	_, ok = obj.Fields["id"].(*shapeNodeFunc)
	require.True(t, ok, "id should be shapeNodeFunc, not shapeLiteral")
}

func TestResolveNamedShape_MissingShapeReturnsError(t *testing.T) {
	engine := &MemQLEngine{shapes: newShapeRegistry()}
	_, err := engine.resolveNamedShape("doesNotExist")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestResolveNamedShape_NilRegistryReturnsError(t *testing.T) {
	engine := &MemQLEngine{}
	_, err := engine.resolveNamedShape("anything")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not initialized")
}

// --- embedded .memql smoke tests ---------------------------------------------

// TestAllEmbeddedShapesParseAndConvert loads every shape .memql shipped in
// the binary and confirms each one: parses cleanly, produces a non-empty
// template, and converts into the compiled runtime form without any field
// silently degrading to a shapeLiteral. The last check catches regressions
// where a shape's expression format drifts and starts round-tripping as a
// literal string (which is exactly the failure mode the frontend reported).
func TestAllEmbeddedShapesParseAndConvert(t *testing.T) {
	t.Skip("legacy dsl/v1 tree retired; unified-tree coverage lives in component/memql/unified_*_test.go and dsl/embed_test.go.")
	registry, err := loadEmbeddedShapes(nil, nil)
	require.NoError(t, err)
	require.Greater(t, registry.Count(), 0, "no shapes loaded from embedded FS")

	for _, shape := range registry.List() {
		t.Run(shape.Name, func(t *testing.T) {
			require.NotEmpty(t, shape.Name)
			// Trait shapes (concept-agnostic @row scaffolds for
			// cross-concept predicates) and caller-only shapes don't
			// carry @concepts. Only concept-bound row shapes do.
			require.NotEmpty(t, shape.Template, "shape %q has empty template", shape.Name)

			converted, convErr := convertShapeDefinitionTemplate(shape.Template)
			require.NoErrorf(t, convErr, "shape %q failed to convert", shape.Name)
			obj, ok := converted.(*shapeObject)
			require.Truef(t, ok, "shape %q root must be an object", shape.Name)
			require.NotEmptyf(t, obj.Fields, "shape %q produced empty object", shape.Name)

			// Any field whose source value is a string starting with "node("
			// must convert to a shapeNodeFunc, not a shapeLiteral. A literal
			// here means the escape/unescape contract between the relaxed
			// parser and parseShapeExpressionString is broken -- i.e., the
			// exact bug that shipped as empty `data`.
			for key, raw := range shape.Template {
				s, isString := raw.(string)
				if !isString {
					continue
				}
				trimmed := strings.TrimSpace(s)
				// Accept both the escaped form from the relaxed parser and
				// the unescaped form from the JSON fallback.
				if !strings.HasPrefix(trimmed, `node(\"`) && !strings.HasPrefix(trimmed, `node("`) {
					continue
				}
				_, isNodeFunc := obj.Fields[key].(*shapeNodeFunc)
				require.Truef(t, isNodeFunc,
					"shape %q field %q: node() expression %q collapsed to %T; "+
						"expected shapeNodeFunc -- relaxed-parser / parseShapeExpressionString contract broken",
					shape.Name, key, s, obj.Fields[key])
			}
		})
	}
}

// TestAllEmbeddedFunctionsLoad loads every query and mutation .memql shipped
// in the binary via the same loader the engine uses at startup. Any parse or
// validation failure will surface here long before it reaches a user. A
// passing engine start is not a substitute -- the production startup tolerates
// and logs per-file errors rather than failing the process.
func TestAllEmbeddedFunctionsLoad(t *testing.T) {
	t.Skip("legacy dsl/v1 tree retired; unified-tree coverage lives in component/memql/unified_*_test.go and dsl/embed_test.go.")
	reg, err := loadEmbeddedFunctions(nil, nil)
	require.NoError(t, err)
	require.Greater(t, reg.Count(), 0, "no functions loaded from embedded FS")

	// Spot-check a few well-known queries and mutations so a silent rename
	// shows up as a test failure.
	expected := []string{
		"querySpaceUtterances",
		"querySpaceParticipants",
		"queryActiveSpaces",
		"mutationSendSpeechUtterance",
	}
	for _, name := range expected {
		fn, err := reg.Get(name)
		require.NoErrorf(t, err, "function %q must be registered", name)
		require.NotNil(t, fn)
	}
}

// TestEmbeddedNamedShapeFunctionsRetainShapeReference is the end-to-end
// regression guard for the original shipped bug. For every query function
// whose body is a ShapeExpression referencing a named shape, running it
// through the validator (clone + expand) must produce a ShapeExpression
// with both TemplateName and a non-nil Target preserved. The earlier bug
// dropped TemplateName here and shipped empty `data` on every named-shape
// query.
//
// Stub values are supplied for required args so the assertion exercises
// the entire substitution path, not just the no-arg case.
func TestEmbeddedNamedShapeFunctionsRetainShapeReference(t *testing.T) {
	t.Skip("legacy dsl/v1 tree retired; unified-tree coverage lives in component/memql/unified_*_test.go and dsl/embed_test.go.")
	reg, err := loadEmbeddedFunctions(nil, nil)
	require.NoError(t, err)

	checked := 0
	for _, name := range reg.Names() {
		fn, err := reg.Get(name)
		require.NoError(t, err)
		if fn == nil || fn.Expr == nil {
			continue
		}
		shape, ok := fn.Expr.(*ShapeExpression)
		if !ok || shape.TemplateName == "" {
			continue
		}
		checked++

		args := stubArgsForFunction(fn)
		plan := &QueryPlan{Root: &FunctionCallExpression{Name: name, Args: args}}
		require.NoErrorf(t, resolvePlanFunctions(plan, reg, nil),
			"resolvePlanFunctions failed for %q", name)

		expanded, ok := plan.Root.(*ShapeExpression)
		require.Truef(t, ok, "function %q: expected ShapeExpression after expansion, got %T", name, plan.Root)
		require.Equalf(t, shape.TemplateName, expanded.TemplateName,
			"function %q: TemplateName must be preserved through expansion", name)
		require.NotNilf(t, expanded.Target, "function %q: Target must be preserved", name)
	}
	require.Greater(t, checked, 0, "expected at least one embedded named-shape query")
}

// stubArgsForFunction produces a stub value for every field declared in
// the function's args block, required or not. Function bodies routinely
// call args.X via hard filters even for optional args, and expansion
// errors when the arg isn't supplied. Passing a value for every declared
// field sidesteps that without caring which specific args matter for any
// given function -- the test's only concern is TemplateName preservation.
//
// Enum-constrained fields receive their first allowed value so the stub
// satisfies @enum(...) validation as well.
func stubArgsForFunction(fn *Function) map[string]any {
	args := make(map[string]any)
	if fn == nil || fn.ArgsSchema == nil {
		return args
	}
	for _, field := range fn.ArgsSchema.Fields {
		if field == nil {
			continue
		}
		if len(field.Enum) > 0 {
			args[field.Name] = field.Enum[0]
			continue
		}
		args[field.Name] = stubValueForType(field.Type)
	}
	return args
}

func stubValueForType(typeName string) any {
	switch typeName {
	case "string":
		return "stub"
	case "number", "integer", "int", "float":
		return 1
	case "bool", "boolean":
		return true
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	default:
		return "stub"
	}
}
