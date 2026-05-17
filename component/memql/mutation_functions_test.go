package memql

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

func TestParseObjectLiteral_Basic(t *testing.T) {
	obj := parseObjectLiteral(`{
		"name": "Test",
		active: true,
		count: 123,
		ratio: 1.5,
		nested: { enabled: false },
		arr: ["a", 2, {x: "y"}],
		expr: args.userId
	}`)
	require.NotNil(t, obj)
	require.Equal(t, "Test", obj["name"])
	require.Equal(t, true, obj["active"])
	require.Equal(t, int64(123), obj["count"])
	require.Equal(t, 1.5, obj["ratio"])
	require.Equal(t, `args.userId`, obj["expr"])
}

// TestParseObjectLiteral_CtxShorthand covers the `ctx.ident` shorthand
// that infers the map key from the path. `{ctx.name}` is equivalent
// to `{name: ctx.name}`. parseObjectLiteral runs after the mutation
// rewriter has translated `args.X` -> `ctx.X`, so the engine-internal
// view of an author-written `args.name` is `ctx.name`. The verbose
// form must keep working in the same object.
func TestParseObjectLiteral_CtxShorthand(t *testing.T) {
	obj := parseObjectLiteral(`{
		ctx.name,
		ctx.region,
		environment: ctx.environment,
		active: true
	}`)
	require.NotNil(t, obj)
	require.Equal(t, `ctx.name`, obj["name"])
	require.Equal(t, `ctx.region`, obj["region"])
	require.Equal(t, `ctx.environment`, obj["environment"])
	require.Equal(t, true, obj["active"])
}

// Shorthand is rejected when the path is not a simple identifier
// (dotted paths, empty string, etc.) so we don't invent garbage field
// names. The caller should write the verbose form in those cases.
func TestParseObjectLiteral_CtxShorthandRejectsDottedPath(t *testing.T) {
	// Dotted paths must still be written as `key: ctx.user.id`.
	obj := parseObjectLiteral(`{ userId: ctx.user.id }`)
	require.NotNil(t, obj)
	require.Equal(t, `ctx.user.id`, obj["userId"])

	// Shorthand with a dotted path is malformed (no `key:`) and parses
	// as an invalid entry -> nil map.
	bad := parseObjectLiteral(`{ ctx.user.id }`)
	require.Nil(t, bad)
}

func TestMutationFunctionTemplate_LoadAndRender_CreateSpace(t *testing.T) {
	t.Skip("legacy dsl/v1 tree retired; unified-tree coverage lives in component/memql/unified_*_test.go and dsl/embed_test.go.")
	path := filepath.Join("..", "..", "dsl", "v1", "mutations", "v1", "cognition", "createSpace.memql")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	// `mutationCreateSpace` binds via `@useConcept(space)`; the loader
	// resolves the bare name against the registry, so the test needs a
	// registry seeded with the canonical concept id.
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cognition:space": {Name: "v1:cognition:space"},
	})

	fn, err := tryParseNewFunctionSyntax("mutationCreateSpace", "mutation", string(raw), path, registry)
	require.NoError(t, err)
	require.NotNil(t, fn)
	require.Equal(t, "mutation", fn.FunctionKind)
	require.NotNil(t, fn.MutationTemplate)

	engine := &MemQLEngine{}
	mutation, err := engine.renderMutationTemplate(context.Background(), fn.MutationTemplate, map[string]any{
		"spaceId": "space-123",
		"name":    "My Space",
	})
	require.NoError(t, err)
	require.Equal(t, "v1:cognition:space", mutation.Concept)
	require.Equal(t, "space-123", mutation.ID)
	require.NotEmpty(t, mutation.PayloadRaw)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(mutation.PayloadRaw), &payload))
	require.Equal(t, "My Space", payload["name"])
	require.Equal(t, true, payload["active"])
	require.Equal(t, "active", payload["status"])

	// Optional fields should be absent when args are missing.
	_, hasDescription := payload["description"]
	require.False(t, hasDescription)
}

// Locks in the bare `args.<name>` shorthand inside an `insert <X> {
// ... }` block: the engine's `tryParseShorthandCtx` (after the
// rewriter's `args.X` -> `ctx.X` translation) infers the payload key
// from the path. `args.name` alone produces `{ name: <arg-value> }`.
func TestMutationInsertShorthand_ArgsRefInfersKey(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cognition:space": {Name: "v1:cognition:space"},
	})
	src := `@useConcept(space)
mutation testCreateSpaceShorthand {
  args {
    spaceId  string  @required
    name     string  @required
    status   string
  }
  insert space {
    id: args.spaceId
    args.name
    args.status
    active: true
  }
}`
	fn, err := tryParseNewFunctionSyntax("testCreateSpaceShorthand", "mutation", src, "test.memql", registry)
	require.NoError(t, err)
	require.NotNil(t, fn.MutationTemplate)

	engine := &MemQLEngine{}
	mutation, err := engine.renderMutationTemplate(context.Background(), fn.MutationTemplate, map[string]any{
		"spaceId": "space-7",
		"name":    "Space Seven",
		"status":  "active",
	})
	require.NoError(t, err)
	require.Equal(t, "space-7", mutation.ID)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(mutation.PayloadRaw), &payload))
	// Shorthand entries infer key from the args path:
	require.Equal(t, "Space Seven", payload["name"])
	require.Equal(t, "active", payload["status"])
	// Verbose entries continue to work alongside the shorthand:
	require.Equal(t, true, payload["active"])
}

func TestResolvePlanFunctions_TopLevelMutationCall(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:         "createSpace",
		FunctionKind: "mutation",
		Enabled:      true,
		MutationTemplate: &FunctionMutationTemplate{
			Concept:         "v1:cognition:space",
			IDTemplate:      &languageParser.ArgRefExpr{Path: "spaceId"},
			PayloadTemplate: map[string]any{"name": &languageParser.ArgRefExpr{Path: "name"}},
		},
	}))

	plan := &QueryPlan{
		Root: &FunctionCallExpression{
			Name: "createSpace",
			Args: map[string]any{"name": "X"},
		},
	}
	require.NoError(t, resolvePlanFunctions(plan, reg, nil))
	require.Nil(t, plan.Root)
	require.NotNil(t, plan.MutationCall)
	require.Equal(t, "createSpace", plan.MutationCall.Name)
}

func TestResolvePlanFunctions_SpecCallExpands(t *testing.T) {
	specs := newSpecRegistry()
	require.NoError(t, specs.add(&Spec{
		Name: "specIsOpen",
		Expr: &ComparisonExpression{
			Field: FieldReference{
				Raw:   "payload.status",
				Parts: []string{"payload", "status"},
			},
			Operator: OpEq,
			Value:    "open",
		},
	}))

	plan := &QueryPlan{
		Root: &FunctionCallExpression{
			Name: "specIsOpen",
			Args: map[string]any{},
		},
	}

	require.NoError(t, resolvePlanFunctions(plan, nil, specs))
	_, isCall := plan.Root.(*FunctionCallExpression)
	require.False(t, isCall, "spec call should be expanded before execution")
}

func TestResolvePlanFunctions_SpecCallRejectsArgs(t *testing.T) {
	specs := newSpecRegistry()
	require.NoError(t, specs.add(&Spec{
		Name: "specIsOpen",
		Expr: &ComparisonExpression{
			Field: FieldReference{
				Raw:   "payload.status",
				Parts: []string{"payload", "status"},
			},
			Operator: OpEq,
			Value:    "open",
		},
	}))

	plan := &QueryPlan{
		Root: &FunctionCallExpression{
			Name: "specIsOpen",
			Args: map[string]any{"unexpected": true},
		},
	}

	err := resolvePlanFunctions(plan, nil, specs)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not accept arguments")
}

func TestParseStandaloneExpression_BareSpecRejected(t *testing.T) {
	_, err := parseStandaloneExpression("specIsOpen")
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be invoked with parentheses")
}
