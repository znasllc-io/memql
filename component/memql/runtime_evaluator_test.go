package memql

import (
	"testing"
)

func TestRuntimeEvaluator_EvaluateArg(t *testing.T) {
	ctx := &RuntimeContext{
		Args: map[string]any{
			"name": "John",
			"options": map[string]any{
				"limit": 10,
			},
		},
	}
	eval := NewRuntimeEvaluator(ctx)

	tests := []struct {
		name     string
		argName  string
		expected any
	}{
		{"simple arg", "name", "John"},
		{"nested arg", "options.limit", 10},
		{"missing arg", "unknown", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := eval.EvaluateArg(tt.argName)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestRuntimeEvaluator_EvaluateStep(t *testing.T) {
	ctx := &RuntimeContext{
		Steps: map[string]*StepResult{
			"checkUser": {
				Result: map[string]any{
					"id":   "user-123",
					"name": "John",
				},
				Metadata: map[string]any{
					"itemCount": 1,
				},
			},
		},
	}
	eval := NewRuntimeEvaluator(ctx)

	// Test step result
	result, err := eval.EvaluateStep("checkUser")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", result)
	}
	if m["id"] != "user-123" {
		t.Errorf("expected id 'user-123', got %v", m["id"])
	}

	// Test missing step
	_, err = eval.EvaluateStep("unknown")
	if err == nil {
		t.Error("expected error for missing step")
	}

	// Test step metadata
	metadata, err := eval.EvaluateStepMetadata("checkUser")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metadata["itemCount"] != 1 {
		t.Errorf("expected itemCount 1, got %v", metadata["itemCount"])
	}
}

func TestRuntimeEvaluator_EvaluateInput(t *testing.T) {
	ctx := &RuntimeContext{
		Input: []map[string]any{
			{"id": "1", "name": "Item 1"},
			{"id": "2", "name": "Item 2"},
		},
	}
	eval := NewRuntimeEvaluator(ctx)

	result := eval.EvaluateInput()
	items, ok := result.([]map[string]any)
	if !ok {
		t.Fatalf("expected []map[string]any, got %T", result)
	}
	if len(items) != 2 {
		t.Errorf("expected 2 items, got %d", len(items))
	}
}

func TestRuntimeEvaluator_EvaluateItemAndIndex(t *testing.T) {
	ctx := &RuntimeContext{
		Item: map[string]any{
			"id":   "lead-123",
			"name": "Lead Name",
		},
		Index: 5,
	}
	eval := NewRuntimeEvaluator(ctx)

	// Test item
	item := eval.EvaluateItem()
	m, ok := item.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", item)
	}
	if m["id"] != "lead-123" {
		t.Errorf("expected id 'lead-123', got %v", m["id"])
	}

	// Test index
	index := eval.EvaluateIndex()
	if index != 5 {
		t.Errorf("expected index 5, got %d", index)
	}
}

func TestRuntimeEvaluator_EvaluateTimestamp(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	ts := eval.EvaluateTimestamp()
	if ts == "" {
		t.Error("expected non-empty timestamp")
	}
	// Should be in RFC3339 format
	if len(ts) < 20 {
		t.Errorf("timestamp too short: %s", ts)
	}
}

func TestRuntimeEvaluator_EvaluateField(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	obj := map[string]any{
		"id": "123",
		"nested": map[string]any{
			"value": "deep",
		},
	}

	tests := []struct {
		name     string
		key      string
		expected any
	}{
		{"simple field", "id", "123"},
		{"nested field", "nested.value", "deep"},
		{"missing field", "unknown", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := eval.EvaluateField(obj, tt.key)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestRuntimeEvaluator_EvaluateConcat(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	result := eval.EvaluateConcat("user-", "123", "-test")
	if result != "user-123-test" {
		t.Errorf("expected 'user-123-test', got %s", result)
	}

	// Test with numbers
	result = eval.EvaluateConcat("count:", 42)
	if result != "count:42" {
		t.Errorf("expected 'count:42', got %s", result)
	}
}

func TestRuntimeEvaluator_EvaluateCoalesce(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	tests := []struct {
		name     string
		args     []any
		expected any
	}{
		{"first non-nil", []any{nil, "value", "other"}, "value"},
		{"all nil", []any{nil, nil, nil}, nil},
		{"first value wins", []any{"first", "second"}, "first"},

		// memql#1614: coalesce returns the first NON-NIL arg; a literal
		// empty string is a present value and is returned, not skipped.
		{"nil then empty string yields empty string", []any{nil, ""}, ""},
		{"nil then nil yields nil", []any{nil, nil}, nil},
		{"a then b yields a", []any{"a", "b"}, "a"},
		{"nil then x yields x", []any{nil, "x"}, "x"},
		// Pins the new first-non-nil semantics: an empty-string FIRST arg
		// is a real value and wins over a later non-empty fallback.
		{"empty string then y yields empty string", []any{"", "y"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := eval.EvaluateCoalesce(tt.args...)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestRuntimeEvaluator_EvaluateIf(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	tests := []struct {
		name     string
		cond     any
		then     any
		els      any
		expected any
	}{
		{"true bool", true, "yes", "no", "yes"},
		{"false bool", false, "yes", "no", "no"},
		{"nil is falsy", nil, "yes", "no", "no"},
		{"non-empty string is truthy", "text", "yes", "no", "yes"},
		{"empty string is falsy", "", "yes", "no", "no"},
		{"zero is falsy", 0, "yes", "no", "no"},
		{"non-zero is truthy", 42, "yes", "no", "yes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := eval.EvaluateIf(tt.cond, tt.then, tt.els)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestRuntimeEvaluator_EvaluateFirstLast(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	items := []any{"a", "b", "c"}

	// First
	first := eval.EvaluateFirst(items)
	if first != "a" {
		t.Errorf("expected 'a', got %v", first)
	}

	// Last
	last := eval.EvaluateLast(items)
	if last != "c" {
		t.Errorf("expected 'c', got %v", last)
	}

	// Empty
	empty := []any{}
	if eval.EvaluateFirst(empty) != nil {
		t.Error("expected nil for empty first")
	}
	if eval.EvaluateLast(empty) != nil {
		t.Error("expected nil for empty last")
	}
}

func TestRuntimeEvaluator_StringFunctions(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	// Lower
	if result := eval.EvaluateLower("HELLO"); result != "hello" {
		t.Errorf("lower: expected 'hello', got %s", result)
	}

	// Upper
	if result := eval.EvaluateUpper("hello"); result != "HELLO" {
		t.Errorf("upper: expected 'HELLO', got %s", result)
	}

	// Trim
	if result := eval.EvaluateTrim("  hello  "); result != "hello" {
		t.Errorf("trim: expected 'hello', got %s", result)
	}

	// Hash (just verify it returns something)
	hash := eval.EvaluateHash("test")
	if len(hash) != 64 { // SHA256 produces 64 hex chars
		t.Errorf("hash: expected 64 chars, got %d", len(hash))
	}

	// Contains
	if !eval.EvaluateContains("hello world", "world") {
		t.Error("contains: expected true")
	}
	if eval.EvaluateContains("hello", "world") {
		t.Error("contains: expected false")
	}
}

func TestRuntimeEvaluator_EvaluateEvent(t *testing.T) {
	event := map[string]any{
		"topic":   "graph.node.created",
		"payload": map[string]any{"nodeId": "123"},
	}
	ctx := &RuntimeContext{Event: event}
	eval := NewRuntimeEvaluator(ctx)

	result := eval.EvaluateEvent()
	if result["topic"] != "graph.node.created" {
		t.Errorf("expected topic 'graph.node.created', got %v", result["topic"])
	}
}

func TestRuntimeEvaluator_EvaluateError(t *testing.T) {
	ctx := &RuntimeContext{Error: "something went wrong"}
	eval := NewRuntimeEvaluator(ctx)

	result := eval.EvaluateError()
	if result != "something went wrong" {
		t.Errorf("expected error message, got %s", result)
	}
}

func TestRuntimeEvaluator_BooleanFunctions(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	// And
	tests := []struct {
		name     string
		fn       string
		args     []any
		expected bool
	}{
		{"and all true", "and", []any{true, true, true}, true},
		{"and one false", "and", []any{true, false, true}, false},
		{"and empty", "and", []any{}, true},
		{"and truthy values", "and", []any{"hello", 1, true}, true},
		{"and with nil", "and", []any{true, nil}, false},
		{"or all false", "or", []any{false, false, false}, false},
		{"or one true", "or", []any{false, true, false}, true},
		{"or empty", "or", []any{}, false},
		{"or truthy values", "or", []any{"", 0, "hello"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result bool
			if tt.fn == "and" {
				result = eval.EvaluateAnd(tt.args...)
			} else {
				result = eval.EvaluateOr(tt.args...)
			}
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}

	// Not
	notTests := []struct {
		name     string
		arg      any
		expected bool
	}{
		{"not true", true, false},
		{"not false", false, true},
		{"not nil", nil, true},
		{"not empty string", "", true},
		{"not non-empty string", "hello", false},
	}

	for _, tt := range notTests {
		t.Run(tt.name, func(t *testing.T) {
			result := eval.EvaluateNot(tt.arg)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestRuntimeEvaluator_ComparisonFunctions(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	tests := []struct {
		name     string
		fn       string
		a, b     any
		expected bool
	}{
		// Equality
		{"eq int equal", "eq", 5, 5, true},
		{"eq int not equal", "eq", 5, 3, false},
		{"eq float equal", "eq", 3.14, 3.14, true},
		{"eq string equal", "eq", "hello", "hello", true},
		{"eq string not equal", "eq", "hello", "world", false},
		{"eq nil equal", "eq", nil, nil, true},

		// Less than
		{"lt int true", "lt", 3, 5, true},
		{"lt int false", "lt", 5, 3, false},
		{"lt int equal", "lt", 5, 5, false},
		{"lt float", "lt", 3.14, 3.5, true},

		// Greater than
		{"gt int true", "gt", 5, 3, true},
		{"gt int false", "gt", 3, 5, false},

		// Less than or equal
		{"lte int less", "lte", 3, 5, true},
		{"lte int equal", "lte", 5, 5, true},
		{"lte int greater", "lte", 6, 5, false},

		// Greater than or equal
		{"gte int greater", "gte", 5, 3, true},
		{"gte int equal", "gte", 5, 5, true},
		{"gte int less", "gte", 3, 5, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result bool
			switch tt.fn {
			case "eq":
				result = eval.EvaluateEq(tt.a, tt.b)
			case "lt":
				result = eval.EvaluateLt(tt.a, tt.b)
			case "gt":
				result = eval.EvaluateGt(tt.a, tt.b)
			case "lte":
				result = eval.EvaluateLte(tt.a, tt.b)
			case "gte":
				result = eval.EvaluateGte(tt.a, tt.b)
			}
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestRuntimeEvaluator_ToString(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	tests := []struct {
		name     string
		input    any
		expected string
	}{
		{"int", 42, "42"},
		{"float", 3.14, "3.14"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"string", "hello", "hello"},
		{"nil", nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := eval.EvaluateToString(tt.input)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestRuntimeEvaluator_AddDuration(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	tests := []struct {
		name      string
		timestamp string
		duration  string
		expected  string
		hasError  bool
	}{
		{
			name:      "add 24 hours",
			timestamp: "2025-01-15T12:00:00Z",
			duration:  "PT24H",
			expected:  "2025-01-16T12:00:00Z",
		},
		{
			name:      "add 1 day",
			timestamp: "2025-01-15T12:00:00Z",
			duration:  "P1D",
			expected:  "2025-01-16T12:00:00Z",
		},
		{
			name:      "add 30 minutes",
			timestamp: "2025-01-15T12:00:00Z",
			duration:  "PT30M",
			expected:  "2025-01-15T12:30:00Z",
		},
		{
			name:      "invalid timestamp",
			timestamp: "not-a-date",
			duration:  "PT1H",
			hasError:  true,
		},
		{
			name:      "invalid duration",
			timestamp: "2025-01-15T12:00:00Z",
			duration:  "not-a-duration",
			hasError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := eval.EvaluateAddDuration(tt.timestamp, tt.duration)
			if tt.hasError {
				if err == nil {
					t.Error("expected error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// TestClusterNodeStalePruneWindow models the per-row staleness guard the
// pruneStaleClusterNodes cron uses (#1061):
//
//	if addDuration(item.payload.lastSeen, "PT{N}M") < now { ...mark stopped... }
//
// It pins the selection boundary: a node whose lastSeen is older than the
// MEMQL_NODE_STALE_PRUNE_MINUTES window is pruned; one inside the window
// is left alone. Uses the same EvaluateAddDuration builtin the logic body
// resolves at runtime, so the unit test exercises the real comparison.
func TestClusterNodeStalePruneWindow(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})
	now := "2026-06-07T12:00:00Z"
	const windowDuration = "PT30M" // MEMQL_NODE_STALE_PRUNE_MINUTES default 30

	cases := []struct {
		name       string
		lastSeen   string
		wantPruned bool
	}{
		{"fresh heartbeat 1m ago -> keep", "2026-06-07T11:59:00Z", false},
		{"inside window 29m ago -> keep", "2026-06-07T11:31:00Z", false},
		{"just past window 31m ago -> prune", "2026-06-07T11:29:00Z", true},
		{"long departed 3h ago -> prune", "2026-06-07T09:00:00Z", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deadline, err := eval.EvaluateAddDuration(c.lastSeen, windowDuration)
			if err != nil {
				t.Fatalf("EvaluateAddDuration: %v", err)
			}
			// The logic guard fires (prunes) when deadline < now.
			pruned := deadline < now
			if pruned != c.wantPruned {
				t.Errorf("lastSeen=%s deadline=%s now=%s: pruned=%v, want %v",
					c.lastSeen, deadline, now, pruned, c.wantPruned)
			}
		})
	}
}

func TestRuntimeEvaluator_DaysBetween(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	tests := []struct {
		name     string
		date1    string
		date2    string
		expected int
		hasError bool
	}{
		{"same day", "2025-01-15", "2025-01-15", 0, false},
		{"one day", "2025-01-15", "2025-01-16", 1, false},
		{"one week", "2025-01-15", "2025-01-22", 7, false},
		{"negative", "2025-01-22", "2025-01-15", -7, false},
		{"with timestamps", "2025-01-15T00:00:00Z", "2025-01-22T00:00:00Z", 7, false},
		{"invalid date1", "invalid", "2025-01-15", 0, true},
		{"invalid date2", "2025-01-15", "invalid", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := eval.EvaluateDaysBetween(tt.date1, tt.date2)
			if tt.hasError {
				if err == nil {
					t.Error("expected error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("expected %d, got %d", tt.expected, result)
			}
		})
	}
}

func TestRuntimeEvaluator_SubtractTimestamps(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	result, err := eval.EvaluateSubtractTimestamps("2025-01-15T14:00:00Z", "2025-01-15T12:00:00Z")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should be PT2H0M0S or similar
	if result != "PT2H0M0S" {
		t.Errorf("expected PT2H0M0S, got %s", result)
	}
}

func TestRuntimeEvaluator_DatePartFunctions(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	timestamp := "2025-07-15T12:00:00Z"

	// Year
	year, err := eval.EvaluateYear(timestamp)
	if err != nil {
		t.Fatalf("year error: %v", err)
	}
	if year != 2025 {
		t.Errorf("year: expected 2025, got %d", year)
	}

	// Quarter
	quarter, err := eval.EvaluateQuarter(timestamp)
	if err != nil {
		t.Fatalf("quarter error: %v", err)
	}
	if quarter != 3 { // July is Q3
		t.Errorf("quarter: expected 3, got %d", quarter)
	}

	// Month
	month, err := eval.EvaluateMonth(timestamp)
	if err != nil {
		t.Fatalf("month error: %v", err)
	}
	if month != 7 {
		t.Errorf("month: expected 7, got %d", month)
	}

	// Day of month
	day, err := eval.EvaluateDayOfMonth(timestamp)
	if err != nil {
		t.Fatalf("dayOfMonth error: %v", err)
	}
	if day != 15 {
		t.Errorf("dayOfMonth: expected 15, got %d", day)
	}
}

func TestRuntimeEvaluator_QuarterCalculation(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	tests := []struct {
		month    string
		expected int
	}{
		{"2025-01-15", 1}, // January
		{"2025-03-15", 1}, // March
		{"2025-04-15", 2}, // April
		{"2025-06-15", 2}, // June
		{"2025-07-15", 3}, // July
		{"2025-09-15", 3}, // September
		{"2025-10-15", 4}, // October
		{"2025-12-15", 4}, // December
	}

	for _, tt := range tests {
		t.Run(tt.month, func(t *testing.T) {
			result, err := eval.EvaluateQuarter(tt.month)
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("expected Q%d, got Q%d", tt.expected, result)
			}
		})
	}
}

func TestRuntimeEvaluator_IsAnniversary(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	tests := []struct {
		name      string
		startDate string
		checkDate string
		expected  bool
	}{
		{"same month/day", "2020-07-15", "2025-07-15", true},
		{"different day", "2020-07-15", "2025-07-16", false},
		{"different month", "2020-07-15", "2025-08-15", false},
		{"same date", "2020-07-15", "2020-07-15", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := eval.EvaluateIsAnniversary(tt.startDate, tt.checkDate)
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestRuntimeEvaluator_IsFirstDayOfQuarter(t *testing.T) {
	eval := NewRuntimeEvaluator(&RuntimeContext{})

	tests := []struct {
		date     string
		expected bool
	}{
		{"2025-01-01", true},  // Jan 1
		{"2025-01-02", false}, // Jan 2
		{"2025-04-01", true},  // Apr 1
		{"2025-07-01", true},  // Jul 1
		{"2025-10-01", true},  // Oct 1
		{"2025-02-01", false}, // Feb 1
		{"2025-05-01", false}, // May 1
	}

	for _, tt := range tests {
		t.Run(tt.date, func(t *testing.T) {
			result, err := eval.EvaluateIsFirstDayOfQuarter(tt.date)
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}
