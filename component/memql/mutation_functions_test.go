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
	t.Skip("legacy dsl/v1 tree retired; unified-tree coverage lives in component/memql/unified_*_test.go and test/dslconformance/embed_test.go.")
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
		"partitionId": "space-123",
		"name":        "My Space",
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
	src := `mutate space mutationCreateSpaceShorthand {
  args {
    partitionId  string  @required
    name     string  @required
    status   string
  }
  insert {
    id: args.partitionId
    args.name
    args.status
    active: true
  }
}`
	fn, err := tryParseNewFunctionSyntax("mutationCreateSpaceShorthand", "mutation", src, "test.memql", registry)
	require.NoError(t, err)
	require.NotNil(t, fn.MutationTemplate)

	engine := &MemQLEngine{}
	mutation, err := engine.renderMutationTemplate(context.Background(), fn.MutationTemplate, map[string]any{
		"partitionId": "space-7",
		"name":        "Space Seven",
		"status":      "active",
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

// In the multi-construct file layout, the slicer prepends ALL file-top
// `use ...` declarations to every emitted slice so the per-construct
// parser has its imports. The signature-bound concept (`mutation
// <Concept> <name>`) names the construct's single concept directly,
// so the file-top use count is unrelated to "one concept per
// mutation." Counting file-top uses against signature-bound
// constructs was rejecting every mutation in
// `cognition/mutations.memql` after the multi-construct consolidation
// -- surfaced as memql-cockpit#49 (daily-space never created because
// `mutationCreateDailySpace` was unloadable).
func TestSignatureBoundMutationAcceptsMultipleFileTopUses(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cognition:space":  {Name: "v1:cognition:space"},
		"v1:identity:request": {Name: "v1:identity:request"},
	})
	src := `use cognition.concepts.{ space }
use identity.concepts.{ request }

mutate space mutationCreateDailySpace {
  args {
    partitionId       string  @required
    name          string  @required
    dailyDateKey  string  @required
  }
  insert {
    id: args.partitionId
    args.name
    args.dailyDateKey
    kind: "daily"
    private: true
    status: "active"
    active: true
  }
}`
	fn, err := tryParseNewFunctionSyntax("mutationCreateDailySpace", "mutation", src, "test.memql", registry)
	require.NoError(t, err)
	require.NotNil(t, fn)
	require.NotNil(t, fn.MutationTemplate)
	require.Equal(t, "v1:cognition:space", fn.MutationTemplate.Concept)
}

// Legacy procedural-form queries / mutations (no signature-bound
// concept) still get the single-use rule -- their `use` declaration
// IS the concept binding, so two uses is genuinely ambiguous.
func TestLegacyProceduralMutationRejectsMultipleUses(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cognition:space":  {Name: "v1:cognition:space"},
		"v1:identity:request": {Name: "v1:identity:request"},
	})
	src := `use cognition.concepts.{ space }
use identity.concepts.{ request }

func (Mutation) mutationLegacyForm(ctx any) (any, error) {
  return insert { id: ctx.input.id }, nil
}`
	_, err := tryParseNewFunctionSyntax("mutationLegacyForm", "mutation", src, "test.memql", registry)
	require.Error(t, err)
}

func TestResolvePlanFunctions_TopLevelMutationCall(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:         "createSpace",
		FunctionKind: "mutation",
		Enabled:      true,
		MutationTemplate: &FunctionMutationTemplate{
			Concept:         "v1:cognition:space",
			IDTemplate:      &languageParser.ArgRefExpr{Path: "partitionId"},
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

// TestResolvePlanFunctions_LogicReturningMutationDispatches pins
// F.6 of the ctx-envelope purge: a top-level call to a Logic
// function whose body returns a mutation call is hoisted to
// plan.MutationCall (with caller args substituted into the
// mutation's args map) so the engine dispatches it through
// executeMutationFunctionCall instead of the query-expression
// path. Without this, logicBootstrapDefaultPartition and the
// identity logics hit the "function X is a mutation and cannot
// be used inside query expressions" guard at call time.
// A Logic whose body returns a BUILTIN call must substitute ArgRef
// nodes in the builtin call's args before the builtin executor runs.
// The Logic→Mutation path already does this via the F.6 hoist; the
// Logic→Builtin path goes through the regular expandFunctionCall
// path and must do the same. Without this, the builtin executor
// receives `args["userId"] = *ArgReference{Path: "event.payload.
// userId"}` and `asString(...)` returns empty -- which is exactly
// how memql-cockpit#49's daily-space chain ended up calling
// `dailyspace.ensureForUser` with empty userId even after the
// trigger event fired correctly.
func TestExpandExpression_SubstitutesArgRefsInBuiltinCallArgs(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:         "ensureDailySpaceForUser",
		FunctionKind: "builtin",
		Enabled:      true,
		Executor:     "integration.dailyspace.ensureForUser",
		ArgsSchema: &ArgsSchemaConfig{
			Fields: []*FunctionArgsField{
				{Name: "userId", Type: "string"},
			},
		},
	}))
	require.NoError(t, reg.add(&Function{
		Name:         "logicEnsureDailySpaceOnAuthSession",
		FunctionKind: "logic",
		Enabled:      true,
		ArgsSchema: &ArgsSchemaConfig{
			Fields: []*FunctionArgsField{
				{Name: "event", Type: "object"},
			},
		},
		Expr: &FunctionCallExpression{
			Name: "ensureDailySpaceForUser",
			Args: map[string]any{
				"userId": &ArgReference{Path: "event.payload.userId"},
			},
		},
	}))

	eventMap := map[string]any{
		"topic": "graph.node.created.v1:identity:authSession",
		"kind":  "NodeCreated",
		"payload": map[string]any{
			"userId": "v1:identity:user:abc123",
		},
	}

	// Two input shapes that arrive at this layer in practice:
	//   - flat {event: ...} (engine's own expression parser produces
	//     this for tests / programmatic dispatch).
	//   - positional {"0": {event: ...}} (the language parser produces
	//     this for `logicX({event: ...})` -- which is what the function
	//     step emits when an automation fires the Logic).
	// Both must reach the builtin with the userId resolved to the
	// nested string. The positional shape was the production-hit one
	// in memql-cockpit#49 -- the automation step rendered to a query
	// string parsed back into positional form, and ArgRef
	// substitution failed because the substitution helper looked up
	// `event.payload.userId` against `{"0": {event: ...}}` instead of
	// the flat shape.
	cases := []struct {
		name string
		args map[string]any
	}{
		{
			name: "flat args (engine parser)",
			args: map[string]any{"event": eventMap},
		},
		{
			name: "positional-wrapped args (language parser, automation step path)",
			args: map[string]any{"0": map[string]any{"event": eventMap}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := &QueryPlan{
				Root: &FunctionCallExpression{
					Name: "logicEnsureDailySpaceOnAuthSession",
					Args: tc.args,
				},
			}
			require.NoError(t, resolvePlanFunctions(plan, reg, nil))

			require.NotNil(t, plan.Root, "expected expanded builtin expression")
			builtin, ok := plan.Root.(*BuiltinFunctionExpression)
			require.True(t, ok, "expected BuiltinFunctionExpression, got %T", plan.Root)
			require.Equal(t, "ensureDailySpaceForUser", builtin.Name)
			require.Equal(t, "v1:identity:user:abc123", builtin.Args["userId"],
				"ArgRef substitution must produce the resolved string -- without it the builtin executor sees an *ArgReference and asString() returns empty")
		})
	}
}

func TestResolvePlanFunctions_LogicReturningMutationDispatches(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:         "mutationCreatePartition",
		FunctionKind: "mutation",
		Enabled:      true,
		MutationTemplate: &FunctionMutationTemplate{
			Concept:    "v1:platform:partition",
			IDTemplate: &languageParser.ArgRefExpr{Path: "name"},
			PayloadTemplate: map[string]any{
				"name":          &languageParser.ArgRefExpr{Path: "name"},
				"partitionType": &languageParser.ArgRefExpr{Path: "partitionType"},
			},
		},
	}))
	// Logic whose return expression is a mutation call. The Logic's
	// own arg (event) is unused -- this mirrors the real
	// logicBootstrapDefaultPartition shape where the cron-fired
	// logic ignores its event and just dispatches the mutation
	// with constants.
	require.NoError(t, reg.add(&Function{
		Name:         "logicBootstrapDefaultPartition",
		FunctionKind: "logic",
		Enabled:      true,
		Expr: &FunctionCallExpression{
			Name: "mutationCreatePartition",
			Args: map[string]any{
				"name":          "default",
				"partitionType": "standard",
			},
		},
	}))

	plan := &QueryPlan{
		Root: &FunctionCallExpression{
			Name: "logicBootstrapDefaultPartition",
			Args: map[string]any{},
		},
	}
	require.NoError(t, resolvePlanFunctions(plan, reg, nil))
	require.Nil(t, plan.Root, "Logic-returning-mutation should be hoisted to plan.MutationCall")
	require.NotNil(t, plan.MutationCall, "plan.MutationCall must be set")
	require.Equal(t, "mutationCreatePartition", plan.MutationCall.Name)
	require.Equal(t, "default", plan.MutationCall.Args["name"])
	require.Equal(t, "standard", plan.MutationCall.Args["partitionType"])
}

// TestResolvePlanFunctions_LogicReturningMutationPositionalArgs pins
// the F.6 hoist normalisation for the shape the language parser
// actually produces. `mutationCreatePartition({name: "default", ...})`
// in a Logic body is parsed as a FunctionCallExpr whose Args map has
// a single positional key "0" carrying the object literal. The engine's
// own expression parser produces flat args ({name: "default"}); the
// hoist must flatten the language-parser shape so the downstream
// mutation validator sees the canonical form. Without the flatten step
// logicBootstrapDefaultPartition fails at startup with
// `mutationCreatePartition: argument validation failed: required
// argument "name" is missing`.
func TestResolvePlanFunctions_LogicReturningMutationPositionalArgs(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:         "mutationCreatePartition",
		FunctionKind: "mutation",
		Enabled:      true,
		MutationTemplate: &FunctionMutationTemplate{
			Concept:    "v1:platform:partition",
			IDTemplate: &languageParser.ArgRefExpr{Path: "name"},
			PayloadTemplate: map[string]any{
				"name":          &languageParser.ArgRefExpr{Path: "name"},
				"partitionType": &languageParser.ArgRefExpr{Path: "partitionType"},
			},
		},
	}))
	// Logic body where the object literal is wrapped under positional
	// key "0" -- exactly the shape extractLogicReturnExpression hands
	// over to the AST converter when the body is parsed by the
	// language parser.
	require.NoError(t, reg.add(&Function{
		Name:         "logicBootstrapDefaultPartition",
		FunctionKind: "logic",
		Enabled:      true,
		Expr: &FunctionCallExpression{
			Name: "mutationCreatePartition",
			Args: map[string]any{
				"0": map[string]any{
					"name":          "default",
					"partitionType": "standard",
				},
			},
		},
	}))

	plan := &QueryPlan{
		Root: &FunctionCallExpression{
			Name: "logicBootstrapDefaultPartition",
			Args: map[string]any{},
		},
	}
	require.NoError(t, resolvePlanFunctions(plan, reg, nil))
	require.NotNil(t, plan.MutationCall, "Logic-returning-mutation should be hoisted to plan.MutationCall")
	require.Equal(t, "mutationCreatePartition", plan.MutationCall.Name)
	require.Equal(t, "default", plan.MutationCall.Args["name"],
		"positional-arg wrap must flatten so the mutation validator sees `name`")
	require.Equal(t, "standard", plan.MutationCall.Args["partitionType"])
	_, hasPositional := plan.MutationCall.Args["0"]
	require.False(t, hasPositional, "positional `0` key should be unwrapped after the hoist")
}

// TestResolvePlanFunctions_LogicReturningMutationSubstitutesArgs
// pins that caller-passed args to the Logic propagate into the
// inner mutation call's args via the ArgReference substitution path.
func TestResolvePlanFunctions_LogicReturningMutationSubstitutesArgs(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:         "mutationCreatePartition",
		FunctionKind: "mutation",
		Enabled:      true,
		MutationTemplate: &FunctionMutationTemplate{
			Concept:    "v1:platform:partition",
			IDTemplate: &languageParser.ArgRefExpr{Path: "name"},
			PayloadTemplate: map[string]any{
				"name": &languageParser.ArgRefExpr{Path: "name"},
			},
		},
	}))
	// Logic whose body forwards a caller-supplied arg into the
	// mutation: `return mutationCreatePartition({ name: args.partitionName })`.
	require.NoError(t, reg.add(&Function{
		Name:         "logicProvisionNamedPartition",
		FunctionKind: "logic",
		Enabled:      true,
		ArgsSchema: &ArgsSchemaConfig{
			Fields: []*FunctionArgsField{{Name: "partitionName", Type: "string"}},
		},
		Expr: &FunctionCallExpression{
			Name: "mutationCreatePartition",
			Args: map[string]any{
				"name": &ArgReference{Path: "partitionName"},
			},
		},
	}))

	plan := &QueryPlan{
		Root: &FunctionCallExpression{
			Name: "logicProvisionNamedPartition",
			Args: map[string]any{"partitionName": "acme"},
		},
	}
	require.NoError(t, resolvePlanFunctions(plan, reg, nil))
	require.NotNil(t, plan.MutationCall)
	require.Equal(t, "mutationCreatePartition", plan.MutationCall.Name)
	require.Equal(t, "acme", plan.MutationCall.Args["name"], "args.partitionName should resolve to caller-passed value")
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

// TestParseStandaloneExpression_BareSpecRejected was retired in
// #328 alongside the recursive-descent parser. The legacy parser
// had a bespoke check that rejected bare spec names with "must be
// invoked with parentheses" -- a low-value error-message guard for
// a typo class engineers don't actually hit (specs are always
// called via the typed generated method on QueryClient, not by
// hand-written bare-name strings). The langparser parses
// `specIsOpen` as an identifier reference and surfaces a different
// error downstream (undefined identifier) which is just as
// actionable. Nothing else relied on the specific message text.
