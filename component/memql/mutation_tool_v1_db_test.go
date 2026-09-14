package memql

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/provenance"
	"github.com/znasllc-io/memql/core/id"
)

// mutation_tool_v1_db_test.go -- the edition-2026 mutation values and tool
// handlers against a REAL engine and a REAL Postgres (epic memql#5363,
// memql#5367). The DB-free tests pin the rendering; these pin that what is
// rendered is written, validated against the real concept, read back and
// served to a tool call -- beside the shipped constructs the tree loads, which
// before the flip were the string evaluator's and now are the same kind.
//
// Postgres-gated through the package's shared engine (sharedReadMergeEngine):
// they only read and write rows, under a user minted per run, so they leave
// the engine stock for every other borrower.

// v1ToolFromDecl builds a tool the way the loader does, from a `tool` block
// whose @handler is written in edition 2026.
func v1ToolFromDecl(t *testing.T, src string) *Tool {
	t.Helper()
	decl, err := languageParser.ParseToolDecl(src)
	require.NoError(t, err)
	tools, err := toolDeclToTool(decl, "v1-probe.memql")
	require.NoError(t, err)
	require.Len(t, tools, 1)
	require.NotNil(t, tools[0].Handler.queryV1, "a v1 handler is parsed at load")
	return tools[0]
}

// toolRows runs a tool and returns the rows it served, keyed by short id.
// todos is shaped (todoFull), so its rows arrive under `data`, projected,
// with bare ids (a tool result is bare-ified for the model).
func toolRows(t *testing.T, eng *MemQLEngine, ctx context.Context, tool *Tool, args map[string]any) (map[string]map[string]any, string) {
	t.Helper()
	res, err := eng.ExecuteTool(ctx, tool, args)
	require.NoError(t, err)
	require.False(t, res.IsError, "tool %s errored: %v", tool.Name, res.Content)
	var out struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
	rows := map[string]map[string]any{}
	for _, row := range out.Data {
		short, _ := row["id"].(string)
		rows[BareShortId(short)] = row
	}
	return rows, res.Content[0].Text
}

// rowIDs is the sorted key set of toolRows' result.
func rowIDs(rows map[string]map[string]any) []string {
	ids := make([]string, 0, len(rows))
	for k := range rows {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	return ids
}

// TestV1MutationTemplateWritesThroughTheEngine renders createTodo's values in
// edition 2026, executes the node through the engine's real write path
// (reserved-field guard, concept validation, @relationship canonicalisation,
// row-authz), and reads the row back beside one the shipped createTodo wrote
// from the same arguments: the two stored payloads are one payload, so the
// block below is what the shipped mutation writes.
func TestV1MutationTemplateWritesThroughTheEngine(t *testing.T) {
	eng, _, base := sharedReadMergeEngine(t)
	user := "user-v1mut-" + id.NewShortId()
	ctx := auth.ContextWithUserActor(base, user)

	// createTodo's insert block (dsl/todos/mutations.memql) as it reads in
	// edition 2026: `accept { title, dueAt, priority,
	// sourceResponsibilityId }` plus its stamp block.
	v1 := mustV1Mutation(t, "v1:todos:todo", v1MutationSrc{block: `{
		title: args.title, dueAt: args.dueAt, priority: args.priority,
		sourceResponsibilityId: args.sourceResponsibilityId,
		id: args.todoId, ownerUserId: actor.userId, done: false
	}`})
	shippedFn, err := eng.functions.Get("createTodo")
	require.NoError(t, err)

	// The Execute path stamps a named mutation's provenance before the
	// write; a node executed directly carries it the same way.
	ctx = provenance.ContextWithProvenance(ctx, provenance.Mutation("createTodo"))
	v1ID, shippedID := "todo-v1-"+id.NewShortId(), "todo-shipped-"+id.NewShortId()
	args := func(todoID string) map[string]any {
		return map[string]any{"todoId": todoID, "title": "Buy milk", "priority": "high"}
	}
	var written []string
	for _, w := range []struct {
		tmpl   *FunctionMutationTemplate
		todoID string
	}{{v1, v1ID}, {shippedFn.MutationTemplate, shippedID}} {
		node, err := eng.renderMutationTemplate(ctx, w.tmpl, args(w.todoID))
		require.NoError(t, err)
		_, err = eng.executeMutation(ctx, node)
		require.NoError(t, err, "the rendered node must pass the engine's real write path")
		written = append(written, node.PayloadRaw)
	}
	require.JSONEq(t, written[1], written[0], "the block and the shipped createTodo wrote different payloads")
	require.NotContains(t, written[0], `"dueAt"`, "a missing optional argument writes no key")

	read := func(todoID string) map[string]any {
		res, err := eng.Execute(ctx, `query todoById(todoId: `+languageParser.QuoteString(todoID)+`)`)
		require.NoError(t, err)
		// todoById is shaped (todoFull), so its row is projected into data.
		_, data, err := res.ToAPIResult()
		require.NoError(t, err)
		require.Len(t, data, 1, "the owner reads the row back")
		row, ok := data[0].AsInterface().(map[string]any)
		require.True(t, ok)
		delete(row, "id")
		delete(row, "createdAt")
		return row
	}
	// Read back through the owner-gated query: the stored rows, owner
	// canonicalised on insert, are one row but for their ids.
	gotV1, gotShipped := read(v1ID), read(shippedID)
	require.Equal(t, gotShipped, gotV1, "the block and the shipped createTodo stored different payloads")
	require.Equal(t, "Buy milk", gotV1["title"])
	require.Equal(t, false, gotV1["done"])
}

// TestV1ToolHandlersExecuteAgainstTheEngine runs the todos tools with their
// handlers written in edition 2026 through ExecuteTool: parsed at load,
// arguments evaluated, the call rendered from values and executed. For every
// filled call the rows served equal the shipped tool's, and an unfilled filter
// is omitted from the call, so every row is served. An explicit JSON null for
// a typed argument never reaches either handler: the tool's input schema
// refuses it first, on both.
func TestV1ToolHandlersExecuteAgainstTheEngine(t *testing.T) {
	eng, _, base := sharedReadMergeEngine(t)
	user := "user-v1tool-" + id.NewShortId()
	ctx := WithActingAgentRole(auth.ContextWithUserActor(base, user), "specialist")

	list := v1ToolFromDecl(t, "/// List to-dos\n@handler(type=\"query\", query=\"query todos(done: args.done)\")\ntool zzTodosListV1 {\n  done boolean @description(\"d\")\n}\n")
	complete := v1ToolFromDecl(t, "/// Complete a to-do\n@handler(type=\"query\", query=\"mutation completeTodo(todoId: args.todoId, payload: args.payload)\")\ntool zzTodosCompleteV1 {\n  todoId string! @description(\"d\")\n  payload object! @description(\"d\")\n}\n")
	shippedList, err := eng.tools.Get("todosList")
	require.NoError(t, err)
	require.NotNil(t, shippedList.Handler.queryV1, "the shipped todosList is parsed at load like every query handler")

	openID, doneID := "todo-open-"+id.NewShortId(), "todo-done-"+id.NewShortId()
	for _, todoID := range []string{openID, doneID} {
		_, err := eng.Execute(ctx, `mutation createTodo(todoId: `+languageParser.QuoteString(todoID)+`, title: "t")`)
		require.NoError(t, err)
	}

	// Complete one through the v1 handler: a map argument rendered as a
	// literal, a hostile title passed through as data.
	const hostile = "done \"now\" \\ $args.todoId\nline two"
	res, err := eng.ExecuteTool(ctx, complete, map[string]any{
		"todoId":  doneID,
		"payload": map[string]any{"title": hostile, "done": true},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)

	sorted := func(ids ...string) []string {
		sort.Strings(ids)
		return ids
	}
	for _, tc := range []struct {
		name   string
		args   map[string]any
		want   []string
		filled bool
	}{
		{"done=true", map[string]any{"done": true}, sorted(doneID), true},
		{"done=false", map[string]any{"done": false}, sorted(openID), true},
		{"done omitted", map[string]any{}, sorted(doneID, openID), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, v1Text := toolRows(t, eng, ctx, list, tc.args)
			require.Equal(t, tc.want, rowIDs(rows))
			if tc.filled {
				_, shippedText := toolRows(t, eng, ctx, shippedList, tc.args)
				require.JSONEq(t, shippedText, v1Text, "a filled call serves what the shipped handler serves")
			}
		})
	}

	// An explicit null is refused by the input schema before either handler.
	for _, tool := range []*Tool{list, shippedList} {
		res, err := eng.ExecuteTool(ctx, tool, map[string]any{"done": nil})
		require.NoError(t, err)
		require.True(t, res.IsError, "%s: a null boolean is not a boolean", tool.Name)
		require.Contains(t, res.Content[0].Text, "expected boolean, but got null")
	}

	// The hostile title arrived as data, byte for byte.
	rows, _ := toolRows(t, eng, ctx, list, map[string]any{"done": true})
	require.Equal(t, hostile, rows[doneID]["title"])
	require.Equal(t, true, rows[doneID]["done"])
}
