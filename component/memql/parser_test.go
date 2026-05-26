package memql

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var (
	parserTestRegistryOnce sync.Once
	parserTestRegistry     *FunctionRegistry
	parserTestRegistryErr  error
)

func TestTokenizeEqualityOperator(t *testing.T) {
	// The shared lexer merges `v1:conversation` into a single identifier
	// token because `:` followed by alphanumeric is part of the concept
	// literal form. The query parser's readIdentifierLiteral still
	// accepts the legacy three-token form (`v1`, `:`, `conversation`)
	// for hand-written whitespace like `v1 : conversation`.
	query := "concept==v1:conversation"
	tokens, err := tokenize(query)
	require.NoError(t, err)
	require.Len(t, tokens, 4)
	require.Equal(t, tokIdentifier, tokens[0].typ)
	require.Equal(t, "concept", tokens[0].literal)
	require.Equal(t, tokOperator, tokens[1].typ)
	require.Equal(t, "==", tokens[1].literal)
	require.Equal(t, tokIdentifier, tokens[2].typ)
	require.Equal(t, "v1:conversation", tokens[2].literal)
	require.Equal(t, tokEOF, tokens[3].typ)

	plan := mustParse(t, query)
	require.NotNil(t, plan)
}

func TestParseBasicComparison(t *testing.T) {
	plan := mustParse(t, "concept==v1:conversation")
	comp := assertComparison(t, plan.Root)
	require.Equal(t, "concept", comp.Field.Raw)
	require.Equal(t, OpEq, comp.Operator)
	require.Equal(t, "v1:conversation", comp.Value)
}

func TestParseLogicalExpressions(t *testing.T) {
	plan := mustParse(t, "concept==v1:conversation;payload.active==true,concept==v1:message")

	orNode := assertLogical(t, plan.Root, LogicalOr)
	andNode := assertLogical(t, orNode.Left, LogicalAnd)

	leftMost := assertComparison(t, andNode.Left)
	require.Equal(t, "concept", leftMost.Field.Raw)
	require.Equal(t, "v1:conversation", leftMost.Value)

	rightOfAnd := assertComparison(t, andNode.Right)
	require.Equal(t, "payload.active", rightOfAnd.Field.Raw)
	require.Equal(t, true, rightOfAnd.Value)

	rightOfOr := assertComparison(t, orNode.Right)
	require.Equal(t, "concept", rightOfOr.Field.Raw)
	require.Equal(t, "v1:message", rightOfOr.Value)
}

func TestParseParenthesizedPrecedence(t *testing.T) {
	plan := mustParse(t, "(concept==v1:conversation,concept==v1:message);payload.active==true")
	andNode := assertLogical(t, plan.Root, LogicalAnd)
	orNode := assertLogical(t, andNode.Left, LogicalOr)

	assertComparison(t, orNode.Left)
	assertComparison(t, orNode.Right)

	active := assertComparison(t, andNode.Right)
	require.Equal(t, "payload.active", active.Field.Raw)
	require.Equal(t, true, active.Value)
}

func TestParseRelationshipFunctions(t *testing.T) {
	functions := []RelationshipFunction{
		RelParentOf,
		RelChildOf,
		RelAliasOf,
		RelEquals,
		RelInteractsWith,
		RelContains,
		RelOwns,
		RelCreatedBy,
		RelIds,
	}

	for _, fn := range functions {
		query := string(fn) + "(concept==v1:conversation)"
		t.Run(string(fn), func(t *testing.T) {
			plan := mustParse(t, query)
			rel, ok := plan.Root.(*RelationshipExpression)
			require.Truef(t, ok, "expected RelationshipExpression, got %T", plan.Root)
			require.Equal(t, fn, rel.Function)
			assertComparison(t, rel.Target)
		})
	}
}

func TestParseSortDirective(t *testing.T) {
	plan := mustParse(t, `sort(concept==v1:conversation,"createdAt","asc","id","desc")`)
	want := []SortField{
		{Field: "createdAt", Direction: SortDirectionAsc},
		{Field: "id", Direction: SortDirectionDesc},
	}
	require.Equal(t, want, plan.Sort)
	assertComparison(t, plan.Root)
}

func TestParsePaginateDirective(t *testing.T) {
	plan := mustParse(t, "paginate(concept==v1:conversation, 50)")
	require.NotNil(t, plan.Limit)
	require.Equal(t, 50, *plan.Limit)
	require.Nil(t, plan.Offset)

	plan = mustParse(t, "paginate(concept==v1:conversation, 25, 10)")
	require.NotNil(t, plan.Offset)
	require.Equal(t, 10, *plan.Offset)
}

func TestParseAsOfDirective(t *testing.T) {
	const ts = "2025-01-01T00:00:00Z"
	plan := mustParse(t, `asOf(concept==v1:conversation, "`+ts+`")`)
	require.NotNil(t, plan.Timestamp)
	require.Equal(t, ts, plan.Timestamp.Format(time.RFC3339))
	require.False(t, plan.UseLatest)

	plan = mustParse(t, "asOf(concept==v1:conversation, latest)")
	require.True(t, plan.UseLatest)
}

func TestParseLatestDirectiveRemoved(t *testing.T) {
	// latest() directive has been removed - queries should now fail if latest() is used
	_, err := newParserTestEngine(t).Parse("latest(concept==v1:conversation)")
	require.Error(t, err, "latest() should no longer be a valid directive")
}

func TestParseWithDepthDirective(t *testing.T) {
	plan := mustParse(t, "withDepth(concept==v1:conversation, 3)")
	require.NotNil(t, plan.Depth)
	require.Equal(t, 3, *plan.Depth)
}

func TestParseNestedDirectives(t *testing.T) {
	query := `sort(paginate(concept==v1:conversation;payload.active==true, 100), "createdAt","desc")`
	plan := mustParse(t, query)

	require.NotNil(t, plan.Limit)
	require.Equal(t, 100, *plan.Limit)
	require.Len(t, plan.Sort, 1)
	require.Equal(t, "createdAt", plan.Sort[0].Field)

	andExpr := assertLogical(t, plan.Root, LogicalAnd)
	assertComparison(t, andExpr.Left)
	assertComparison(t, andExpr.Right)
}

func TestParseDocsAndConceptsRequests(t *testing.T) {
	plan := mustParse(t, "memqlDocs()")
	require.NotNil(t, plan.Root)
	docsExpr, ok := plan.Root.(*BuiltinFunctionExpression)
	require.True(t, ok, "expected BuiltinFunctionExpression, got %T", plan.Root)
	require.Equal(t, "memqlDocs", docsExpr.Name)
	require.Equal(t, BuiltinExecutorMemqlDocs, docsExpr.Executor)

	plan = mustParse(t, "concepts()")
	require.NotNil(t, plan.Root)
	conceptsExpr, ok := plan.Root.(*BuiltinFunctionExpression)
	require.True(t, ok, "expected BuiltinFunctionExpression, got %T", plan.Root)
	require.Equal(t, "concepts", conceptsExpr.Name)
	require.Equal(t, BuiltinExecutorConcepts, conceptsExpr.Executor)
	require.Nil(t, conceptsExpr.Args, "concepts() without pattern should have nil args")

	// Test concepts() with pattern filter
	plan = mustParse(t, `concepts("crm")`)
	require.NotNil(t, plan.Root)
	conceptsExpr, ok = plan.Root.(*BuiltinFunctionExpression)
	require.True(t, ok, "expected BuiltinFunctionExpression, got %T", plan.Root)
	require.Equal(t, "concepts", conceptsExpr.Name)
	require.Equal(t, BuiltinExecutorConcepts, conceptsExpr.Executor)
	require.NotNil(t, conceptsExpr.Args, "concepts() with pattern should have args")
	require.Equal(t, "crm", conceptsExpr.Args["pattern"], "pattern should be 'crm'")
}

func TestParseValidateFunction(t *testing.T) {
	// Valid validate() call with concept and payload
	plan := mustParse(t, `validate({"concept": "v1:crm:lead", "payload": {"email": "test@example.com"}})`)
	require.NotNil(t, plan.Root)
	validateExpr, ok := plan.Root.(*BuiltinFunctionExpression)
	require.True(t, ok, "expected BuiltinFunctionExpression, got %T", plan.Root)
	require.Equal(t, "validate", validateExpr.Name)
	require.Equal(t, BuiltinExecutorValidate, validateExpr.Executor)
	require.NotNil(t, validateExpr.Args, "Args should not be nil, got validateExpr: %+v", validateExpr)
	require.Equal(t, "v1:crm:lead", validateExpr.Args["concept"])
	payload, ok := validateExpr.Args["payload"].(map[string]any)
	require.True(t, ok, "expected payload to be map[string]any")
	require.Equal(t, "test@example.com", payload["email"])
}

// TestParseFunctionArgs_UnquotedKeys pins the canonical function-call
// argument syntax: identifier keys are bare (not quoted), nested objects
// recurse with the same rule, and quoted string keys still parse so
// JSON-serialized tool calls remain compatible. This is the regression
// guard for "cognition integration silently silently never finds SI
// participants because querySpaceParticipants({spaceId: ...}) fails to
// parse with 'function argument keys must be strings'."
func TestParseFunctionArgs_UnquotedKeys(t *testing.T) {
	engine := newParserTestEngine(t)

	cases := []struct {
		name  string
		query string
	}{
		{
			name:  "canonical unquoted keys",
			query: `validate({concept: "v1:crm:lead", payload: {email: "x@y.z"}})`,
		},
		{
			name:  "quoted keys still accepted",
			query: `validate({"concept": "v1:crm:lead", "payload": {"email": "x@y.z"}})`,
		},
		{
			name:  "mixed: top-level unquoted, nested quoted",
			query: `validate({concept: "v1:crm:lead", payload: {"email": "x@y.z"}})`,
		},
		{
			name:  "mixed: top-level quoted, nested unquoted",
			query: `validate({"concept": "v1:crm:lead", payload: {email: "x@y.z"}})`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := engine.Parse(tc.query)
			require.NoError(t, err)
			expr, ok := plan.Root.(*BuiltinFunctionExpression)
			require.True(t, ok)
			require.Equal(t, "v1:crm:lead", expr.Args["concept"])
			payload, ok := expr.Args["payload"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "x@y.z", payload["email"])
		})
	}
}

func TestParseValidateFunctionErrors(t *testing.T) {
	engine := newParserTestEngine(t)

	// Every validate() arg-shape failure wraps ErrInvalidArgument
	// (the sentinel category). Pre-#257 these tests asserted
	// specific message substrings, which coupled the test surface
	// to a single parser's error wording. errors.Is decouples the
	// category from the message; the parser-side wording can change
	// without breaking the test.
	cases := []struct {
		name  string
		query string
	}{
		{"missing concept field", `validate({"payload": {}})`},
		{"missing payload field", `validate({"concept": "v1:test"})`},
		{"non-object argument", `validate("not-an-object")`},
		{"no arguments", `validate()`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := engine.Parse(tc.query)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrInvalidArgument,
				"validate() arg-shape errors must wrap ErrInvalidArgument; got %v", err)
		})
	}
}

func TestParseFunctionsBuiltin(t *testing.T) {
	plan := mustParse(t, `functions()`)
	require.NotNil(t, plan.Root)
	expr, ok := plan.Root.(*BuiltinFunctionExpression)
	require.True(t, ok, "expected BuiltinFunctionExpression, got %T", plan.Root)
	require.Equal(t, "functions", expr.Name)
	require.Equal(t, BuiltinExecutorFunctions, expr.Executor)
	require.Nil(t, expr.Args)
}

func TestParseToolsBuiltin(t *testing.T) {
	plan := mustParse(t, `tools()`)
	require.NotNil(t, plan.Root)
	expr, ok := plan.Root.(*BuiltinFunctionExpression)
	require.True(t, ok, "expected BuiltinFunctionExpression, got %T", plan.Root)
	require.Equal(t, "tools", expr.Name)
	require.Equal(t, BuiltinExecutorTools, expr.Executor)
	require.Nil(t, expr.Args)
}

func TestParseBuiltinFromRegistryMetadata(t *testing.T) {
	reg := newFunctionRegistry()
	require.NoError(t, reg.add(&Function{
		Name:     "customBuiltin",
		Type:     FunctionTypeBuiltin,
		Executor: "help",
		BuiltinArgs: &BuiltinArgContract{
			Profile:   BuiltinArgProfileStringOrObject,
			StringKey: "name",
			Required:  []string{"name"},
			Properties: map[string]string{
				"name": "string",
			},
		},
	}))
	engine := &MemQLEngine{initialized: true, functions: reg}
	plan, err := engine.Parse(`customBuiltin("x")`)
	require.NoError(t, err)
	expr, ok := plan.Root.(*BuiltinFunctionExpression)
	require.True(t, ok)
	require.Equal(t, "customBuiltin", expr.Name)
	require.Equal(t, "help", expr.Executor)
	require.Equal(t, "x", expr.Args["name"])
}

func TestParseServiceVersionBuiltins(t *testing.T) {
	plan := mustParse(t, `memqlVersion()`)
	require.NotNil(t, plan.Root)
	expr, ok := plan.Root.(*BuiltinFunctionExpression)
	require.True(t, ok, "expected BuiltinFunctionExpression, got %T", plan.Root)
	require.Equal(t, "memqlVersion", expr.Name)
	require.Equal(t, BuiltinExecutorServiceVersion, expr.Executor)
	require.Nil(t, expr.Args)

	plan = mustParse(t, `serviceVersion()`)
	require.NotNil(t, plan.Root)
	expr, ok = plan.Root.(*BuiltinFunctionExpression)
	require.True(t, ok, "expected BuiltinFunctionExpression, got %T", plan.Root)
	require.Equal(t, "memqlVersion", expr.Name)
	require.Equal(t, BuiltinExecutorServiceVersion, expr.Executor)
	require.Nil(t, expr.Args)
}

func TestParseHelpBuiltin(t *testing.T) {
	// JSON object argument
	plan := mustParse(t, `help({"name": "myFunction"})`)
	require.NotNil(t, plan.Root)
	expr, ok := plan.Root.(*BuiltinFunctionExpression)
	require.True(t, ok, "expected BuiltinFunctionExpression, got %T", plan.Root)
	require.Equal(t, "help", expr.Name)
	require.Equal(t, BuiltinExecutorHelp, expr.Executor)
	require.NotNil(t, expr.Args)
	require.Equal(t, "myFunction", expr.Args["name"])

	// String shorthand argument
	plan = mustParse(t, `help("otherFunction")`)
	require.NotNil(t, plan.Root)
	expr, ok = plan.Root.(*BuiltinFunctionExpression)
	require.True(t, ok, "expected BuiltinFunctionExpression, got %T", plan.Root)
	require.Equal(t, "help", expr.Name)
	require.Equal(t, "otherFunction", expr.Args["name"])
}

func TestParseHelpBuiltinErrors(t *testing.T) {
	engine := newParserTestEngine(t)

	// help() arg-shape failures wrap ErrInvalidArgument (#257
	// rationale -- see TestParseValidateFunctionErrors above).
	for _, query := range []string{
		`help()`,
		`help({"other": "value"})`,
	} {
		t.Run(query, func(t *testing.T) {
			_, err := engine.Parse(query)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrInvalidArgument,
				"help() arg-shape errors must wrap ErrInvalidArgument; got %v", err)
		})
	}
}

func TestParseInsertMutation(t *testing.T) {
	plan := mustParse(t, `insert("v1:conversation", id="conv-123", payload={"active":true})`)
	require.Len(t, plan.Mutations, 1)
	m := plan.Mutations[0]
	require.Equal(t, "v1:conversation", m.Concept)
	require.Equal(t, "conv-123", m.ID)
	require.NotEmpty(t, strings.TrimSpace(m.PayloadRaw))
}

func TestParseLiteralTypes(t *testing.T) {
	cases := []struct {
		query string
		want  any
	}{
		{`payload.active==true`, true},
		{`payload.count==42`, int64(42)},
		{`payload.ratio==3.14`, 3.14},
		{`payload.note==null`, nil},
		{`payload.status==pending`, "pending"},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			plan := mustParse(t, tc.query)
			comp := assertComparison(t, plan.Root)
			switch want := tc.want.(type) {
			case int64:
				got, ok := comp.Value.(int64)
				require.True(t, ok)
				require.Equal(t, want, got)
			case float64:
				got, ok := comp.Value.(float64)
				require.True(t, ok)
				require.Equal(t, want, got)
			default:
				require.Equal(t, want, comp.Value)
			}
		})
	}
}

func mustParse(t *testing.T, query string) *QueryPlan {
	t.Helper()
	plan, err := newParserTestEngine(t).Parse(query)
	require.NoErrorf(t, err, "parse failed for %q", query)
	return plan
}

func newParserTestEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	parserTestRegistryOnce.Do(func() {
		parserTestRegistry, parserTestRegistryErr = loadEmbeddedFunctions(nil, nil)
		if parserTestRegistryErr != nil {
			return
		}
		// Pass 2 of the DSL restructure: overlay functions + builtins
		// from the new unified tree so this test engine sees the same
		// registry the runtime engine sees. Without this, tests that
		// reference unified-only entries (or run after the legacy
		// tree is retired) fail with "function not found".
		if _, _, err := LoadUnifiedFunctions(nil, parserTestRegistry, nil); err != nil {
			parserTestRegistryErr = err
			return
		}
		if _, err := LoadUnifiedBuiltins(nil, parserTestRegistry); err != nil {
			parserTestRegistryErr = err
			return
		}
	})
	require.NoError(t, parserTestRegistryErr)
	return &MemQLEngine{
		initialized: true,
		functions:   parserTestRegistry,
	}
}

func assertComparison(t *testing.T, node ExpressionNode) *ComparisonExpression {
	t.Helper()
	comp, ok := node.(*ComparisonExpression)
	require.Truef(t, ok, "expected ComparisonExpression, got %T", node)
	return comp
}

func assertLogical(t *testing.T, node ExpressionNode, op LogicalOp) *LogicalExpression {
	t.Helper()
	logical, ok := node.(*LogicalExpression)
	require.Truef(t, ok, "expected LogicalExpression, got %T", node)
	require.Equal(t, op, logical.Op)
	return logical
}
