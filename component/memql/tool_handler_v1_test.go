package memql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// tool_handler_v1_test.go pins the edition-2026 query handler (epic
// memql#5363, memql#5367; tool_handler_v1.go): parsed once at load, its
// arguments evaluated through EvalExpr, and the call rendered from their
// values -- never substituted into text.

// toolHandlerCorpus is every query handler the tree ships, as it loads, and
// the call it renders when every argument is filled (filledToolArgs). The
// rendered calls were measured against the retired `$args.` substitution
// before the flip deleted it, and were the same bytes -- so a handler renders
// the call it always has. TestToolHandlerV1CorpusIsComplete holds this table
// to the loaded tree, so a handler added to the tree without a row here fails.
var toolHandlerCorpus = []struct {
	tool, v1, rendered string
}{
	{"recallWorkHistory", `query workRecallHistory(search: args.search)`, `query workRecallHistory(search: "v-search")`},
	{"discoverCapabilities", `query workCapabilities(search: args.search)`, `query workCapabilities(search: "v-search")`},
	{"executeCapability", `query workExecute(name: args.name, arguments: args.arguments)`, `query workExecute(name: "v-name", arguments: {"done":true,"n":2,"title":"T arguments"})`},
	{"navigateOS", `query workNavigate(app: args.app, section: args.section, record: args.record)`, `query workNavigate(app: "v-app", section: "v-section", record: "v-record")`},
	{"todosList", `query todos(done: args.done)`, `query todos(done: true)`},
	{"todosComplete", `mutation completeTodo(todoId: args.todoId, payload: args.payload)`, `mutation completeTodo(todoId: "v-todoId", payload: {"done":true,"n":2,"title":"T payload"})`},
	{"todosUpdate", `mutation updateTodo(todoId: args.todoId, payload: args.payload)`, `mutation updateTodo(todoId: "v-todoId", payload: {"done":true,"n":2,"title":"T payload"})`},
	{"forgeActiveProjects", `query activeProjects()`, `query activeProjects()`},
	{"forgeResolveProject", `query projectBySlug(slug: args.slug)`, `query projectBySlug(slug: "v-slug")`},
	{"forgeMyRequests", `query myRequests()`, `query myRequests()`},
	{"forgeRequestById", `query requestById(requestId: args.requestId)`, `query requestById(requestId: "v-requestId")`},
	{"forgeRequestHistory", `query requestEvents(requestId: args.requestId)`, `query requestEvents(requestId: "v-requestId")`},
	{"forgeValidationQueue", `query validationQueue()`, `query validationQueue()`},
	{"forgeApprovalQueue", `query approvalQueue()`, `query approvalQueue()`},
	{"describeFunction", `builtin help(name: args.name)`, `builtin help(name: "v-name")`},
	{"searchUsers", `paginate(query searchUsers(active: args.active), args.limit)`, `paginate(query searchUsers(active: true), 7)`},
	{"calendarList", `query upcomingEvents(windowStart: args.windowStart, windowEnd: args.windowEnd)`, `query upcomingEvents(windowStart: "v-windowStart", windowEnd: "v-windowEnd")`},
	{"calendarFind", `query findEvents(title: args.title)`, `query findEvents(title: "v-title")`},
	{"calendarUpdate", `mutation updateCalendarEvent(eventId: args.eventId, payload: args.payload)`, `mutation updateCalendarEvent(eventId: "v-eventId", payload: {"done":true,"n":2,"title":"T payload"})`},
	{"calendarDelete", `mutation deleteCalendarEvent(eventId: args.eventId)`, `mutation deleteCalendarEvent(eventId: "v-eventId")`},
	{"notesList", `query notes()`, `query notes()`},
	{"notesUpdate", `mutation updateNote(noteId: args.noteId, payload: args.payload)`, `mutation updateNote(noteId: "v-noteId", payload: {"done":true,"n":2,"title":"T payload"})`},
	{"notesSearch", `query notesByTag(tag: args.tag)`, `query notesByTag(tag: "v-tag")`},
}

// loadCorpusQueryTools loads the tree's tools and returns its query-handler
// tools by name.
func loadCorpusQueryTools(t *testing.T) map[string]*Tool {
	t.Helper()
	tools := newToolRegistry()
	n, err := LoadUnifiedTools(discardLogger(), tools)
	require.NoError(t, err)
	require.NotZero(t, n)
	// The index carries each tool under its bare and its namespace-qualified
	// name; keyed by the tool's own name, each appears once.
	out := map[string]*Tool{}
	for _, tool := range tools.LookupIndex() {
		if tool != nil && tool.Handler != nil && strings.EqualFold(tool.Handler.Type, "query") {
			out[tool.Name] = tool
		}
	}
	return out
}

// filledToolArgs builds a value for every property the tool's input schema
// declares, by type -- the arguments of a call that fills every slot.
func filledToolArgs(t *testing.T, tool *Tool) map[string]any {
	t.Helper()
	var schema struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(tool.InputSchema, &schema))
	args := map[string]any{}
	for name, p := range schema.Properties {
		switch p.Type {
		case "boolean":
			args[name] = true
		case "integer", "number":
			args[name] = float64(7)
		case "object":
			args[name] = map[string]any{"title": "T " + name, "done": true, "n": float64(2)}
		case "array":
			args[name] = []any{"a", "b"}
		default:
			args[name] = "v-" + name
		}
	}
	return args
}

// TestToolHandlerV1CorpusIsComplete: the table above is the tree's query
// handlers, exactly, each parsed when it loaded.
func TestToolHandlerV1CorpusIsComplete(t *testing.T) {
	loaded := loadCorpusQueryTools(t)
	require.Len(t, loaded, len(toolHandlerCorpus), "a query handler was added to or removed from the tree without a row in toolHandlerCorpus")
	for _, row := range toolHandlerCorpus {
		tool, ok := loaded[row.tool]
		require.Truef(t, ok, "tool %s is in the table and not in the tree", row.tool)
		require.Equal(t, row.v1, strings.TrimSpace(tool.Handler.Query), "tool %s", row.tool)
		require.NotNilf(t, tool.Handler.queryV1, "%s is parsed at load", row.tool)
	}
}

// TestToolHandlerV1RendersThePinnedCall: for every handler in the tree, with
// every argument filled, the handler renders the call pinned in the table --
// the call the retired substitution rendered, byte for byte.
func TestToolHandlerV1RendersThePinnedCall(t *testing.T) {
	loaded := loadCorpusQueryTools(t)
	for _, row := range toolHandlerCorpus {
		t.Run(row.tool, func(t *testing.T) {
			tool := loaded[row.tool]
			args := applyToolDefaults(context.Background(), tool, filledToolArgs(t, tool), nil)
			got, err := tool.Handler.queryV1.render(context.Background(), args)
			require.NoError(t, err)
			require.Equal(t, row.rendered, got, "the executed query moved")

			// And through the handler itself, the way ExecuteTool asks.
			viaHandler, err := tool.Handler.renderToolQuery(context.Background(), args)
			require.NoError(t, err)
			require.Equal(t, row.rendered, viaHandler)
		})
	}
}

// TestToolHandlerV1UnfilledArgumentsAreOmitted: an argument the caller did
// not supply is absent, and its named argument is left out of the call.
// CHANGED: the substitution wrote `null` for a bare placeholder and the
// four-character string "null" for a quoted one (pinned as a quirk by
// TestSubstituteArgs_UnfilledPlaceholdersCollapseToNull).
func TestToolHandlerV1UnfilledArgumentsAreOmitted(t *testing.T) {
	loaded := loadCorpusQueryTools(t)
	for _, row := range toolHandlerCorpus {
		t.Run(row.tool, func(t *testing.T) {
			plan, err := prepareToolQueryV1(row.v1)
			require.NoError(t, err)
			// ExecuteTool applies the schema's @default values before the
			// handler runs, so an unfilled argument with a default is filled.
			args := applyToolDefaults(context.Background(), loaded[row.tool], map[string]any{}, nil)
			got, err := plan.render(context.Background(), args)
			require.NoError(t, err)
			call := row.v1
			if strings.HasPrefix(call, "paginate(") {
				require.Equal(t, `paginate(query searchUsers(), 10)`, got, "the default limit fills the count; the unfilled filter is omitted")
				return
			}
			require.Equal(t, call[:strings.Index(call, "(")]+"()", got, "every unfilled argument is omitted")
		})
	}
}

// TestToolHandlerV1ExplicitNilIsNull: an argument the caller sent as JSON
// null is kept, as `null` -- the internal query form's nil in every value
// position. CHANGED: the substitution rendered an explicit nil as the empty
// string `""`, which a boolean or object argument then refused.
func TestToolHandlerV1ExplicitNilIsNull(t *testing.T) {
	plan, err := prepareToolQueryV1(`query todos(done: args.done)`)
	require.NoError(t, err)
	got, err := plan.render(context.Background(), map[string]any{"done": nil})
	require.NoError(t, err)
	require.Equal(t, `query todos(done: null)`, got)

	// The rendered call parses, and the argument is a plain nil.
	parsed, err := languageParser.ParseExpression(got)
	require.NoError(t, err)
	call, ok := parsed.(*languageParser.FunctionCallExpr)
	require.True(t, ok, "got %T", parsed)
	v, present := call.Args["done"]
	require.True(t, present)
	require.Nil(t, v)
}

// TestMemqlCallLiteralNilIsNullNotNil records why nil renders as `null`:
// inside an object literal the internal query form reads `nil` as a nil NODE,
// and `null` as a nil value.
func TestMemqlCallLiteralNilIsNullNotNil(t *testing.T) {
	parsed, err := languageParser.ParseExpression(`query q(p: {a: nil, b: null})`)
	require.NoError(t, err)
	obj := parsed.(*languageParser.FunctionCallExpr).Args["p"].(map[string]any)
	require.IsType(t, &languageParser.NilExpr{}, obj["a"], "`nil` in an object is a node")
	require.Nil(t, obj["b"], "`null` in an object is nil")
	lit, err := memqlCallLiteral(map[string]any{"a": nil, "b": []any{nil}})
	require.NoError(t, err)
	require.Equal(t, `{"a":null,"b":[null]}`, lit)
}

// TestToolHandlerV1ValueIsNeverTemplate is the injection test: a string
// argument carrying a quote, a backslash, a newline and a `$args.` reference
// of its own is one literal from the caller to the construct, and the
// sibling argument's value cannot reach its slot.
func TestToolHandlerV1ValueIsNeverTemplate(t *testing.T) {
	const hostile = "a \"quoted\" \\ back\nslash $args.other and args.other) , x: 1"
	plan, err := prepareToolQueryV1(`mutation createTodo(todoId: args.todoId, title: args.title)`)
	require.NoError(t, err)
	args := map[string]any{"todoId": hostile, "title": "$args.todoId", "other": "SECRET"}
	got, err := plan.render(context.Background(), args)
	require.NoError(t, err)
	require.NotContains(t, got, "SECRET", "a value never reads another argument")

	parsed, err := languageParser.ParseExpression(got)
	require.NoError(t, err, "the rendered call must parse: %s", got)
	call, ok := parsed.(*languageParser.FunctionCallExpr)
	require.True(t, ok)
	require.Equal(t, hostile, call.Args["todoId"], "the hostile value arrives verbatim")
	require.Equal(t, "$args.todoId", call.Args["title"], "a `$args.` in a value is data")
	require.Len(t, call.Args, 2, "no argument was injected")

	// And through a map argument, keys included.
	plan, err = prepareToolQueryV1(`mutation updateNote(noteId: args.noteId, payload: args.payload)`)
	require.NoError(t, err)
	payload := map[string]any{"k\"ey": hostile, "nested": map[string]any{"x": "$args.noteId"}}
	got, err = plan.render(context.Background(), map[string]any{"noteId": "n-1", "payload": payload})
	require.NoError(t, err)
	parsed, err = languageParser.ParseExpression(got)
	require.NoError(t, err, "the rendered call must parse: %s", got)
	require.Equal(t, payload, parsed.(*languageParser.FunctionCallExpr).Args["payload"])
}

// TestToolHandlerV1RenderedValuesRoundTrip: every value shape a tool call
// carries renders as a literal the internal query form reads back to the
// same value.
func TestToolHandlerV1RenderedValuesRoundTrip(t *testing.T) {
	plan, err := prepareToolQueryV1(`query q(v: args.v)`)
	require.NoError(t, err)
	for _, v := range []any{
		"plain", "", "unicode é ☃", "control \x00 \x1b", "<html> & amp", float64(3), 2.5, float64(-4), 1e21, int64(1) << 60,
		true, false, []any{"a", float64(1), true, nil}, map[string]any{"a": "b", "n": float64(1), "l": []any{}, "m": map[string]any{}},
	} {
		got, err := plan.render(context.Background(), map[string]any{"v": v})
		require.NoError(t, err)
		parsed, err := languageParser.ParseExpression(got)
		require.NoErrorf(t, err, "%s", got)
		back := parsed.(*languageParser.FunctionCallExpr).Args["v"]
		// Numbers come back as the literal's type (int64 for a whole
		// number), so compare through JSON.
		wantJSON, _ := json.Marshal(v)
		gotJSON, _ := json.Marshal(back)
		require.JSONEqf(t, string(wantJSON), string(gotJSON), "%#v rendered as %s", v, got)
	}
}

// TestToolHandlerV1ArgumentsAreExpressions: an argument is any in-process
// expression over args, evaluated before the call.
func TestToolHandlerV1ArgumentsAreExpressions(t *testing.T) {
	plan, err := prepareToolQueryV1(`query findEvents(title: trim(args.title) ?? "untitled", day: args.day ?? now)`)
	require.NoError(t, err)
	got, err := plan.render(context.Background(), map[string]any{"title": "  Karate  ", "day": "2026-09-13"})
	require.NoError(t, err)
	require.Equal(t, `query findEvents(title: "Karate", day: "2026-09-13")`, got)
	got, err = plan.render(context.Background(), map[string]any{"title": "   "})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(got, `query findEvents(title: "untitled", day: "`), got)
}

// TestToolHandlerV1PaginateCount: paginate's count is a positive whole
// number, or the call is refused before it is executed.
func TestToolHandlerV1PaginateCount(t *testing.T) {
	plan, err := prepareToolQueryV1(`paginate(query searchUsers(active: args.active), args.limit)`)
	require.NoError(t, err)
	for _, tc := range []struct {
		limit any
		want  string
	}{
		{float64(10), `paginate(query searchUsers(active: true), 10)`},
		{int64(3), `paginate(query searchUsers(active: true), 3)`},
	} {
		got, err := plan.render(context.Background(), map[string]any{"active": true, "limit": tc.limit})
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
	for _, bad := range []any{float64(0), float64(-1), 2.5, "10", nil} {
		_, err := plan.render(context.Background(), map[string]any{"active": true, "limit": bad})
		require.ErrorContainsf(t, err, "paginate takes a positive whole number", "limit %#v", bad)
	}
	_, err = plan.render(context.Background(), map[string]any{"active": true})
	require.ErrorContains(t, err, "is absent")
}

// TestToolHandlerV1RefusedAtLoad: a v1 handler that is not a handler is a
// load refusal, through the loader's own conversion.
func TestToolHandlerV1RefusedAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name, query, want string
	}{
		{"does not parse", `query todos(done: args.done`, "does not parse"},
		{"not a call", `args.done`, "is not a handler"},
		{"a raw filter", `concept == "v1:todos:todo"`, "is not a handler"},
		{"a bare function", `hash(args.x)`, "is not a handler"},
		{"an unsupported kind", `action deploy(target: args.t)`, "a handler calls a query, mutation, logic, builtin or automation"},
		{"an unbound pun", `query todos(done)`, "reads `done`"},
		{"the retired ctx root", `query todos(done: ctx.done)`, "reads `ctx`"},
		{"the actor", `query todos(owner: actor.userId)`, "reads `actor`"},
		{"a nested construct call", `query todos(done: query other())`, "constructCall"},
		{"an unknown function", `query todos(done: frob(args.done))`, "frob() is not a function"},
		{"paginate with one argument", `paginate(query searchUsers())`, "paginate takes the call and a count"},
		{"paginate over a non-call", `paginate(args.x, 10)`, "paginate's first argument is the construct call"},
		{"paginate's count reads ctx", `paginate(query searchUsers(), ctx.limit)`, "reads `ctx`"},
		{"the retired dollar form inside a v1 handler", `query todos(done: $done)`, "does not parse"},
		{"the retired placeholder", `query todos(done: $args.done)`, "$args.x is retired in edition 2026"},
		{"the retired placeholder, quoted", `query projectBySlug(slug: "$args.slug")`, "$args.x is retired in edition 2026"},
		{"the retired placeholder, paged", `paginate(query searchUsers(active: args.active), $args.limit)`, "memqlmigrate --rewrite=expressions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "/// probe\n@handler(type=\"query\", query=" + languageParser.QuoteString(tc.query) + ")\ntool zzProbe {\n  done boolean @description(\"d\")\n}\n"
			decl, err := languageParser.ParseToolDecl(src)
			require.NoError(t, err)
			_, err = toolDeclToTool(decl, "probe.memql")
			require.Error(t, err, "%s must refuse at load", tc.query)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestToolHandlerV1LoadsThroughTheLoader: a v1 handler declared in `.memql`
// is parsed when it loads, and a registry clone keeps the parse.
func TestToolHandlerV1LoadsThroughTheLoader(t *testing.T) {
	src := "/// List to-dos\n@handler(type=\"query\", query=\"query todos(done: args.done)\")\ntool zzTodos {\n  done boolean @description(\"d\")\n}\n"
	decl, err := languageParser.ParseToolDecl(src)
	require.NoError(t, err)
	tools, err := toolDeclToTool(decl, "probe.memql")
	require.NoError(t, err)
	require.Len(t, tools, 1)
	require.NotNil(t, tools[0].Handler.queryV1, "parsed once, at load")

	registry := newToolRegistry()
	require.NoError(t, registry.Upsert(tools[0]))
	got, err := registry.Get("zzTodos")
	require.NoError(t, err)
	require.Same(t, tools[0].Handler.queryV1, got.Handler.queryV1, "a registry clone shares the parse")
	require.Equal(t, []string{"todos"}, toolHandlerTargets(got))
}

// TestToolHandlerV1TargetsResolve: the resolver reads a v1 handler's target
// from its AST, and a dead one is still caught.
func TestToolHandlerV1TargetsResolve(t *testing.T) {
	for _, tc := range []struct{ query, want string }{
		{`query todos(done: args.done)`, "todos"},
		{`mutation updateNote(noteId: args.noteId)`, "updateNote"},
		{`builtin help(name: args.name)`, "help"},
		{`paginate(query searchUsers(active: args.active), args.limit)`, "searchUsers"},
		{`query activeProjects()`, "activeProjects"},
	} {
		plan, err := prepareToolQueryV1(tc.query)
		require.NoError(t, err)
		got := toolHandlerTargets(&Tool{Name: "zzProbe", Handler: &ToolHandler{Type: "query", Query: tc.query, queryV1: plan}})
		require.Equal(t, []string{tc.want}, got, tc.query)
	}

	plan, err := prepareToolQueryV1(`query zzNoSuchQuery(a: args.a)`)
	require.NoError(t, err)
	tools := newToolRegistry()
	mustUpsertTool(t, tools, &Tool{
		Name: "zzDeadV1", Description: "d", Origin: "test.memql",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler:     &ToolHandler{Type: "query", Query: `query zzNoSuchQuery(a: args.a)`, queryV1: plan},
	})
	errs := validateToolHandlerTargets(tools, newFunctionRegistry())
	require.Len(t, errs, 1)
	require.Contains(t, errs[0].Error(), "zzNoSuchQuery")
}

// TestToolHandlerV1RefusesTheRetiredPlaceholder: a handler built in Go that
// still carries `$args.` is refused on its call, naming the spelling that
// replaces it, and nothing reaches the engine. The QUOTED form is the one that
// matters: read as v1 it is a string literal, and would have handed the
// construct the text "$args.id" in place of the caller's value.
func TestToolHandlerV1RefusesTheRetiredPlaceholder(t *testing.T) {
	for _, query := range []string{
		`mutation m(id: "$args.id", key: "$args.idempotencyKey")`,
		`mutation m(id: $args.id)`,
	} {
		h := &ToolHandler{Type: "query", Query: query}
		_, err := h.renderToolQuery(context.Background(), map[string]any{"id": "abc", "idempotencyKey": "K-1"})
		require.ErrorContains(t, err, "$args.x is retired in edition 2026: write args.x", query)
		require.Empty(t, toolHandlerTargets(&Tool{Name: "zzProbe", Handler: h}), "a handler that does not load names no target")
	}
}

// TestToolHandlerV1GoBuiltHandlerIsParsedPerCall: a tool built in Go was never
// loaded, so its v1 handler is parsed on the call -- and refused there when it
// is not a handler.
func TestToolHandlerV1GoBuiltHandlerIsParsedPerCall(t *testing.T) {
	h := &ToolHandler{Type: "query", Query: `query todos(done: args.done)`}
	got, err := h.renderToolQuery(context.Background(), map[string]any{"done": false})
	require.NoError(t, err)
	require.Equal(t, `query todos(done: false)`, got)

	_, err = (&ToolHandler{Type: "query", Query: `concept==v1:examples:world`}).renderToolQuery(context.Background(), nil)
	require.ErrorContains(t, err, "does not parse")
}

// TestExecuteToolV1RefusesBeforeExecuting: a handler whose arguments refuse
// is refused by ExecuteTool before anything reaches the engine (no database
// is needed to see it).
func TestExecuteToolV1RefusesBeforeExecuting(t *testing.T) {
	plan, err := prepareToolQueryV1(`paginate(query searchUsers(active: args.active), args.limit)`)
	require.NoError(t, err)
	tool := &Tool{
		Name:        "zzPaged",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"active":{"type":"boolean"},"limit":{"type":"integer"}}}`),
		Handler:     &ToolHandler{Type: "query", Query: `paginate(query searchUsers(active: args.active), args.limit)`, queryV1: plan},
	}
	_, err = (&MemQLEngine{}).ExecuteTool(agentCtxForTest(), tool, map[string]any{"active": true, "limit": float64(0)})
	require.ErrorContains(t, err, `tool "zzPaged": paginate takes a positive whole number`)
}

// TestMemqlCallLiteral_IsTheOneDefinition is the v1 twin of
// memql_literal_single_definition_test.go: a string is rendered by
// QuoteString, the one definition of a MemQL string literal, and reads back.
func TestMemqlCallLiteral_IsTheOneDefinition(t *testing.T) {
	for _, s := range memqlLiteralFixtures {
		got, err := memqlCallLiteral(s)
		require.NoError(t, err)
		require.Equal(t, languageParser.QuoteString(s), got)
		toks, err := languageParser.NewLexer(got).Tokenize()
		require.NoError(t, err)
		require.Equal(t, languageParser.TokenString, toks[0].Type)
		require.Equal(t, s, toks[0].Literal)
	}
	for _, bad := range []any{func() {}, make(chan int)} {
		_, err := memqlCallLiteral(bad)
		require.Error(t, err)
	}
}

// TestToolHandlerV1ArgumentOrderIsTheSource: arguments render in the order the
// handler writes them, which is the order the substitution kept.
func TestToolHandlerV1ArgumentOrderIsTheSource(t *testing.T) {
	plan, err := prepareToolQueryV1(`query upcomingEvents(windowStart: args.windowStart, windowEnd: args.windowEnd)`)
	require.NoError(t, err)
	got, err := plan.render(context.Background(), map[string]any{"windowEnd": "E", "windowStart": "S"})
	require.NoError(t, err)
	require.Equal(t, `query upcomingEvents(windowStart: "S", windowEnd: "E")`, got)
}
