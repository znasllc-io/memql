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
// calendarCreate.allDay is a boolean field with NO schema default.
//
// Original bug + the now-superseded partial fix: @default("false") put a
// string "default" into the boolean field's JSON schema, so some LLM/MCP
// clients called the tool with allDay="false" (string); and the handler
// interpolated `"$args.allDay"` into a MemQL query string, so allDay arrived
// as the string literal "false" -> createCalendarEvent rejected it
// with "expected bool, got string". An interim fix kept the type="query"
// handler with a bare `$args.allDay` token, but that still left the OTHER
// optionals (endsAt/location/notes/recurrence) as quoted "$args.x" slots that
// render the literal string "null" when omitted (the #1683 class), and it
// still relied on the malformed string default.
//
// Shipped fix (memql#1672): calendarCreate dispatches via a typed
// type="function" handler -> the structural args map is passed straight to
// createCalendarEvent (no $args string interpolation at all), and the
// allDay tool field carries NO @default, so the schema advertises a clean
// boolean with no string default. Omitted optionals simply stay absent.
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

	// No "default" key may land in the JSON schema -- a string default
	// ("false") confuses LLMs / MCP clients into passing allDay as a string.
	// The mutation handles the absent field via coalesce(args.allDay, false).
	if defVal, exists := allDayProp["default"]; exists {
		t.Errorf("calendarCreate.allDay schema has \"default\": %v (%T); want no default key", defVal, defVal)
	}

	// The handler must be a typed function handler targeting the mutation --
	// NOT a type="query" string-interpolation handler. This eliminates the
	// $args string-substitution slot entirely (the root of both the bool-as-
	// string and the omitted-optional-as-"null" bug classes).
	if tool.Handler == nil {
		t.Fatal("calendarCreate.Handler is nil; expected a function handler")
	}
	if tool.Handler.Type != "function" {
		t.Errorf("calendarCreate handler type = %q, want \"function\" (typed dispatch, no $args string interpolation)", tool.Handler.Type)
	}
	if tool.Handler.FunctionName != "createCalendarEvent" {
		t.Errorf("calendarCreate handler function = %q, want \"createCalendarEvent\"", tool.Handler.FunctionName)
	}
	// A function handler carries no query template -- there is no string slot
	// for allDay (or any optional) to be coerced to a string / "null".
	if strings.Contains(tool.Handler.Query, "$args") {
		t.Errorf("calendarCreate function handler must not carry a $args query template; query=%q", tool.Handler.Query)
	}
}

// TestCalendarCreate1673_HandlerIsTypedFunction verifies the shipped fix
// routes calendarCreate through a typed function handler so the args map is
// passed structurally to the mutation. With no string-interpolation template,
// an omitted optional (allDay / endsAt / location / notes / recurrence) is
// simply absent from the call rather than rendered as the literal "null" /
// "false" string -- the bug class that the prior type="query" handler left
// open for every optional except allDay.
func TestCalendarCreate1673_HandlerIsTypedFunction(t *testing.T) {
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
	if tool.Handler.Type != "function" || tool.Handler.FunctionName != "createCalendarEvent" {
		t.Fatalf("calendarCreate handler = {type:%q, fn:%q}; want a function handler targeting createCalendarEvent",
			tool.Handler.Type, tool.Handler.FunctionName)
	}
}

// TestCalendarCreate1673_MutationNoReservedFields guards against #1673
// blocker 2: the insert block of createCalendarEvent must NOT declare
// the engine-reserved fields "createdBy" OR "createdAt". Both are stamped by
// the engine (createdBy from actor.userId, createdAt = now); declaring either
// in the payload triggers "insert() payload declares reserved field" at
// mutation-execution time and blocks calendar create entirely. (An interim
// fix removed only createdBy but left createdAt: now, which is also reserved.)
func TestCalendarCreate1673_MutationNoReservedFields(t *testing.T) {
	content := dslCalendarMutationsSource(t)

	for _, reserved := range []string{"createdBy: actor.userId", "createdAt: now"} {
		if strings.Contains(content, reserved) {
			excerpt := content
			if idx := strings.Index(content, "mutation calendarEvent createCalendarEvent"); idx >= 0 {
				end := idx + 600
				if end > len(content) {
					end = len(content)
				}
				excerpt = content[idx:end]
			}
			t.Errorf("createCalendarEvent insert block declares reserved field %q -- remove it; the engine stamps createdAt/createdBy automatically\nexcerpt:\n%s", reserved, excerpt)
		}
	}
}

// propKeys returns the string keys of a map, for diagnostic messages.
func propKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
