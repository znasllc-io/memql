package memql

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

// run3_tool_handlers_test.go guards the tool-surface half of the run-3 MCP QA
// fixes (memql#1672): the create tools that used $args string interpolation
// (which rendered an omitted optional as the literal string "null"/"false")
// are now typed function handlers that pass the structural args map straight to
// the mutation, so an omitted optional stays absent. No DB required -- this
// asserts the loaded tool definitions.
func TestRun3_CreateTools_AreTypedFunctionHandlers(t *testing.T) {
	registry := newToolRegistry()
	if _, err := LoadUnifiedTools(slog.Default(), registry); err != nil {
		t.Fatalf("LoadUnifiedTools failed: %v", err)
	}

	cases := []struct {
		tool string
		fn   string
	}{
		{"calendarCreate", "mutationCreateCalendarEvent"}, // #1673 / #1683
		{"notesCreate", "mutationCreateNote"},             // #1683
		{"todosCreate", "mutationCreateTodo"},             // #1683
	}
	for _, c := range cases {
		tool, err := registry.Get(c.tool)
		require.NoError(t, err, "tool %s must load", c.tool)
		require.NotNil(t, tool.Handler, "tool %s must have a handler", c.tool)
		require.Equal(t, "function", tool.Handler.Type,
			"tool %s must dispatch via a typed function handler, not $args string interpolation (memql#1672)", c.tool)
		require.Equal(t, c.fn, tool.Handler.FunctionName,
			"tool %s must target mutation %s", c.tool, c.fn)
	}

	// #1673: the calendarCreate allDay field must NOT carry a string default
	// ("false") in its input schema -- that default rendered as the string
	// "false" and failed the mutation's bool arg validation. Boolean type, no
	// default, is the fix.
	cal, err := registry.Get("calendarCreate")
	require.NoError(t, err)
	var schema struct {
		Properties map[string]struct {
			Type    string `json:"type"`
			Default any    `json:"default"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(cal.InputSchema, &schema))
	allDay, ok := schema.Properties["allDay"]
	require.True(t, ok, "calendarCreate must declare allDay")
	require.Equal(t, "boolean", allDay.Type, "allDay must be a boolean")
	require.Nil(t, allDay.Default, "allDay must not carry a (string) default in the schema (#1673)")
}
