package memql

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// dslCalendarMutationsSource returns the on-disk content of
// dsl/calendar/mutations.memql, resolved relative to this test file's
// directory so the path stays correct regardless of where `go test` is
// invoked.
func dslCalendarMutationsSource(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	// This file lives at component/memql/; repo root is two levels up.
	repoRoot := filepath.Join(filepath.Dir(file), "..", "..")
	src, err := os.ReadFile(filepath.Join(repoRoot, "dsl", "calendar", "mutations.memql"))
	if err != nil {
		t.Fatalf("read dsl/calendar/mutations.memql: %v", err)
	}
	return string(src)
}

// TestCalendarCreate1673_AllDayBoolDefault is the #1673 regression guard:
// calendarCreate.allDay is a boolean field. Before the fix the tool schema had
// `"default": "false"` (a string) because @default("false") stored the literal
// string, and the handler used the pre-quoted form `"$args.allDay"` which
// produced a MemQL string literal instead of a bare bool.
//
// Two classes of bug were fixed:
//  1. Tool-field @default("false") → JSON schema had `"default": "false"` (string
//     in a boolean field), which caused some LLMs / MCP clients to call the tool
//     with allDay="false" (string) instead of allDay=false (bool).
//  2. Handler `"allDay": "$args.allDay"` (pre-quoted form) → when allDay arrived
//     as a string the MemQL query got `"allDay": "false"` (string literal),
//     rejected by mutationCreateCalendarEvent with "expected bool, got string".
//
// After the fix: @default(false) (no string default in schema) + bare handler
// form `allDay: $args.allDay` (direct bool expression, not a quoted string slot).
func TestCalendarCreate1673_AllDayBoolDefault(t *testing.T) {
	registry := newToolRegistry()
	if _, err := LoadUnifiedTools(slog.Default(), registry); err != nil {
		t.Fatalf("LoadUnifiedTools: %v", err)
	}

	tool, err := registry.Get("calendarCreate")
	if err != nil {
		t.Fatalf("calendarCreate not found in tool registry: %v", err)
	}

	// Decode the tool's JSON input schema.
	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatalf("unmarshal InputSchema: %v", err)
	}

	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("InputSchema.properties missing or wrong type: %T %v", schema["properties"], schema)
	}

	allDayProp, ok := props["allDay"].(map[string]any)
	if !ok {
		t.Fatalf("calendarCreate.allDay missing from InputSchema.properties; got keys: %v", propKeys(props))
	}

	// The field type MUST be "boolean" (not "string").
	if got := allDayProp["type"]; got != "boolean" {
		t.Errorf("calendarCreate.allDay type = %q, want \"boolean\"", got)
	}

	// After @default(false) (unquoted bool), attrStringValue returns "" so no
	// "default" key lands in the JSON schema. This prevents a string-typed default
	// from confusing LLMs or MCP clients into passing allDay as a string. The
	// mutation handles the absent field via coalesce(args.allDay, false).
	if defVal, exists := allDayProp["default"]; exists {
		t.Errorf("calendarCreate.allDay schema has \"default\": %v (%T); want no default key (a string default confuses LLM into passing \"false\" as a string instead of false as a bool)", defVal, defVal)
	}

	// The handler query template must use the BARE form $args.allDay (not the
	// pre-quoted form "$args.allDay") so a bool value flows through as a MemQL
	// bool expression rather than a string literal.
	if tool.Handler == nil {
		t.Fatal("calendarCreate.Handler is nil; expected a query handler")
	}
	handlerQuery := tool.Handler.Query

	if strings.Contains(handlerQuery, `"$args.allDay"`) {
		t.Errorf("calendarCreate handler uses pre-quoted form \"$args.allDay\" -- must use bare form allDay: $args.allDay to preserve bool type;\nquery=%s", handlerQuery)
	}
	if !strings.Contains(handlerQuery, `$args.allDay`) {
		t.Errorf("calendarCreate handler missing $args.allDay placeholder;\nquery=%s", handlerQuery)
	}
}

// TestCalendarCreate1673_HandlerQueryBoolSubstitution verifies that
// substituteArgsInMemqlQuery, when applied to the calendarCreate handler
// template, produces a bare bool (not a quoted string literal) for allDay in
// both the timed-event (allDay=false) and all-day-event (allDay=true) cases.
func TestCalendarCreate1673_HandlerQueryBoolSubstitution(t *testing.T) {
	registry := newToolRegistry()
	if _, err := LoadUnifiedTools(slog.Default(), registry); err != nil {
		t.Fatalf("LoadUnifiedTools: %v", err)
	}

	tool, err := registry.Get("calendarCreate")
	if err != nil {
		t.Fatalf("calendarCreate not found: %v", err)
	}
	if tool.Handler == nil {
		t.Fatal("calendarCreate.Handler is nil")
	}
	tmpl := tool.Handler.Query

	t.Run("timed event allDay=false produces bare false", func(t *testing.T) {
		args := map[string]any{
			"title":    "Karate class",
			"startsAt": "2026-06-18T09:00:00Z",
			"allDay":   false, // Go bool -- what the engine receives from the LLM
		}
		got := substituteArgsInMemqlQuery(tmpl, args)

		// The rendered query must contain bare `false` (not the JSON string
		// `"false"`). encodeForMemqlSubstitution for a Go bool produces
		// the bare token via fmt.Sprintf("%t", v), so the result CANNOT be
		// wrapped in JSON quotes when the bare-substitution path ran correctly.
		if strings.Contains(got, `"false"`) {
			t.Errorf("timed event: allDay rendered as quoted string literal \"false\" in MemQL query; want bare bool false\nquery=%s", got)
		}
		if !strings.Contains(got, "false") {
			t.Errorf("timed event: value false missing from rendered query\nquery=%s", got)
		}
	})

	t.Run("all-day event allDay=true produces bare true", func(t *testing.T) {
		args := map[string]any{
			"title":    "Birthday",
			"startsAt": "2026-06-18T00:00:00Z",
			"allDay":   true,
		}
		got := substituteArgsInMemqlQuery(tmpl, args)

		if strings.Contains(got, `"true"`) {
			t.Errorf("all-day event: allDay rendered as quoted string literal \"true\" in MemQL query; want bare bool true\nquery=%s", got)
		}
		if !strings.Contains(got, "true") {
			t.Errorf("all-day event: value true missing from rendered query\nquery=%s", got)
		}
	})

	t.Run("omitted allDay resolves to null not a placeholder", func(t *testing.T) {
		// When the LLM omits allDay entirely, the placeholder cleanup in
		// substituteArgsInMemqlQuery must resolve $args.allDay → null.
		// The mutation body handles null via coalesce(args.allDay, false).
		args := map[string]any{
			"title":    "Meeting",
			"startsAt": "2026-06-18T14:00:00Z",
			// allDay intentionally absent
		}
		got := substituteArgsInMemqlQuery(tmpl, args)

		if strings.Contains(got, `"$args.allDay"`) || strings.Contains(got, `$args.allDay`) {
			t.Errorf("omitted allDay left an unreplaced placeholder in the rendered query\nquery=%s", got)
		}
	})
}

// TestCalendarCreate1673_MutationNoReservedFields guards against #1673:
// the insert block of mutationCreateCalendarEvent must NOT declare ANY
// engine-reserved payload field. createdAt + createdBy are both reserved
// (component/database/memory-nodes/constants.go reservedPayloadFields);
// the executeInsert guard rejects any reserved key in the payload at
// mutation-execution time with "insert() payload ... declares reserved
// field". The engine auto-stamps these intrinsics. `id` is exempt -- it is
// extracted from the insert block as the node identity before the guard,
// so it is the one reserved name an insert block legitimately assigns.
//
// The original fix (#1691) only dropped createdBy and left createdAt: now,
// so calendar create still failed on createdAt -- this generalized guard
// would have caught that. A DSL-text check rather than a real-engine insert
// because the latter is Postgres-gated; #1694 carries the real-engine path.
func TestCalendarCreate1673_MutationNoReservedFields(t *testing.T) {
	content := dslCalendarMutationsSource(t)

	insert := mutationInsertBlock(content, "mutationCreateCalendarEvent")
	if insert == "" {
		t.Fatal("could not locate mutationCreateCalendarEvent insert block in dsl/calendar/mutations.memql")
	}

	// reserved-minus-id: id is extracted as the node identity before the
	// reserved-field guard, so an insert block may assign it; the rest must
	// never appear as a payload assignment.
	reserved := []string{"createdAt", "createdBy", "concept", "partition", "payload", "schema", "type"}
	for _, f := range reserved {
		if strings.Contains(insert, f+":") {
			t.Errorf("mutationCreateCalendarEvent insert block declares reserved field %q -- remove it; the engine auto-stamps it. The executeInsert guard rejects reserved keys at execution time.\ninsert block:\n%s", f, insert)
		}
	}
}

// mutationInsertBlock returns the text of the insert { ... } block for the
// named mutation, or "" if not found.
func mutationInsertBlock(content, mutationName string) string {
	start := strings.Index(content, mutationName)
	if start < 0 {
		return ""
	}
	ins := strings.Index(content[start:], "insert {")
	if ins < 0 {
		return ""
	}
	ins += start
	end := strings.Index(content[ins:], "}")
	if end < 0 {
		return content[ins:]
	}
	return content[ins : ins+end+1]
}

// propKeys returns the string keys of a map, for diagnostic messages.
func propKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
