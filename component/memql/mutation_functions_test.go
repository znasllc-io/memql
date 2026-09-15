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
// ... }` block: the rewriter expands it to the explicit entry
// (expandBareMirror, component/language/parser/rewriter.go), so
// `args.name` alone produces `{ name: <arg-value> }`.
func TestMutationInsertShorthand_ArgsRefInfersKey(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cognition:space": {Name: "v1:cognition:space"},
	})
	src := `mutation space mutationCreateSpaceShorthand {
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

mutation space mutationCreateDailySpace {
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
