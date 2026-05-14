package memql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// RuntimeContext provides the context for evaluating accessor expressions.
// This is used during automation execution and mutation function evaluation.
type RuntimeContext struct {
	// Engine is the MemQL engine for executing queries and resolving variables.
	Engine *MemQLEngine

	// Args contains function arguments (for ctx.name resolution).
	Args map[string]any

	// Steps contains results from previous automation steps (for step("id") resolution).
	Steps map[string]*StepResult

	// Input is the automation input result (for input() resolution).
	Input any

	// Item is the current forEach item (for item() resolution).
	Item any

	// Index is the current forEach index (for index() resolution).
	Index int

	// Event is the trigger event (for event() resolution).
	Event map[string]any

	// Error is the current error message (for error() resolution).
	Error string
}

// StepResult represents the result of an automation step.
type StepResult struct {
	Result   any
	Metadata map[string]any
	Status   string
}

// RuntimeEvaluator evaluates accessor expressions at runtime.
type RuntimeEvaluator struct {
	ctx *RuntimeContext
}

// NewRuntimeEvaluator creates a new runtime evaluator with the given context.
func NewRuntimeEvaluator(ctx *RuntimeContext) *RuntimeEvaluator {
	return &RuntimeEvaluator{ctx: ctx}
}

// EvaluateArg resolves an ctx.name expression.
func (e *RuntimeEvaluator) EvaluateArg(name string) (any, error) {
	if e.ctx == nil || e.ctx.Args == nil {
		return nil, fmt.Errorf("no argument context available")
	}

	// Support nested paths like "options.limit"
	parts := strings.Split(name, ".")
	var current any = e.ctx.Args

	for _, part := range parts {
		if m, ok := current.(map[string]any); ok {
			val, exists := m[part]
			if !exists {
				return nil, nil // Return nil for missing args (optional)
			}
			current = val
		} else {
			return nil, nil
		}
	}

	return current, nil
}

// EvaluateVar resolves a var("NAME") expression. Partition-scoped
// plaintext; falls back to v1:platform:globalVariable when the partition
// lookup misses.
func (e *RuntimeEvaluator) EvaluateVar(ctx context.Context, name string) (string, error) {
	if e.ctx == nil || e.ctx.Engine == nil {
		return "", fmt.Errorf("no engine context available")
	}
	return e.ctx.Engine.ResolveVariable(ctx, name)
}

// EvaluateSystemVar resolves a systemVar("NAME") expression. Global
// plaintext from v1:platform:globalVariable. No fallback.
func (e *RuntimeEvaluator) EvaluateSystemVar(ctx context.Context, name string) (string, error) {
	if e.ctx == nil || e.ctx.Engine == nil {
		return "", fmt.Errorf("no engine context available")
	}
	return e.ctx.Engine.ResolveSystemVariable(ctx, name)
}

// EvaluateSecret resolves a secret("NAME") expression. Returns the
// decrypted plaintext from the partition-scoped v1:platform:partitionSecret,
// falling back to v1:platform:globalSecret on miss. Requires
// MEMQL_MASTER_KEY. Callers must not log the returned value.
func (e *RuntimeEvaluator) EvaluateSecret(ctx context.Context, name string) (string, error) {
	if e.ctx == nil || e.ctx.Engine == nil {
		return "", fmt.Errorf("no engine context available")
	}
	return e.ctx.Engine.ResolveSecret(ctx, name)
}

// EvaluateSystemSecret resolves a systemSecret("NAME") expression.
// Returns decrypted plaintext from the global v1:platform:globalSecret. No
// fallback.
func (e *RuntimeEvaluator) EvaluateSystemSecret(ctx context.Context, name string) (string, error) {
	if e.ctx == nil || e.ctx.Engine == nil {
		return "", fmt.Errorf("no engine context available")
	}
	return e.ctx.Engine.ResolveSystemSecret(ctx, name)
}

// EvaluateStep resolves a step("id") expression.
func (e *RuntimeEvaluator) EvaluateStep(stepId string) (any, error) {
	if e.ctx == nil || e.ctx.Steps == nil {
		return nil, fmt.Errorf("no step context available")
	}

	result, exists := e.ctx.Steps[stepId]
	if !exists {
		return nil, fmt.Errorf("step %q not found", stepId)
	}

	return result.Result, nil
}

// EvaluateStepMetadata resolves step("id").metadata access.
func (e *RuntimeEvaluator) EvaluateStepMetadata(stepId string) (map[string]any, error) {
	if e.ctx == nil || e.ctx.Steps == nil {
		return nil, fmt.Errorf("no step context available")
	}

	result, exists := e.ctx.Steps[stepId]
	if !exists {
		return nil, fmt.Errorf("step %q not found", stepId)
	}

	return result.Metadata, nil
}

// EvaluateInput resolves an input() expression.
func (e *RuntimeEvaluator) EvaluateInput() any {
	if e.ctx == nil {
		return nil
	}
	return e.ctx.Input
}

// EvaluateItem resolves an item() expression.
func (e *RuntimeEvaluator) EvaluateItem() any {
	if e.ctx == nil {
		return nil
	}
	return e.ctx.Item
}

// EvaluateIndex resolves an index() expression.
func (e *RuntimeEvaluator) EvaluateIndex() int {
	if e.ctx == nil {
		return 0
	}
	return e.ctx.Index
}

// EvaluateEvent resolves an event() expression.
func (e *RuntimeEvaluator) EvaluateEvent() map[string]any {
	if e.ctx == nil {
		return nil
	}
	return e.ctx.Event
}

// EvaluateError resolves an error() expression.
func (e *RuntimeEvaluator) EvaluateError() string {
	if e.ctx == nil {
		return ""
	}
	return e.ctx.Error
}

// EvaluateTimestamp resolves a timestamp() or now() expression.
func (e *RuntimeEvaluator) EvaluateTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// EvaluateField resolves a field(obj, "key") expression.
func (e *RuntimeEvaluator) EvaluateField(obj any, key string) any {
	if obj == nil {
		return nil
	}

	// Support nested paths
	parts := strings.Split(key, ".")
	current := obj

	for _, part := range parts {
		if m, ok := current.(map[string]any); ok {
			val, exists := m[part]
			if !exists {
				return nil
			}
			current = val
		} else {
			return nil
		}
	}

	return current
}

// EvaluateConcat resolves a concat(a, b, ...) expression.
func (e *RuntimeEvaluator) EvaluateConcat(args ...any) string {
	var builder strings.Builder
	for _, arg := range args {
		builder.WriteString(fmt.Sprintf("%v", arg))
	}
	return builder.String()
}

// EvaluateCoalesce resolves a coalesce(a, b, ...) expression.
func (e *RuntimeEvaluator) EvaluateCoalesce(args ...any) any {
	for _, arg := range args {
		if arg != nil {
			// Check for empty strings too
			if s, ok := arg.(string); ok && s == "" {
				continue
			}
			return arg
		}
	}
	return nil
}

// EvaluateIf resolves an if(cond, then, else) expression.
func (e *RuntimeEvaluator) EvaluateIf(cond, thenVal, elseVal any) any {
	if isTruthy(cond) {
		return thenVal
	}
	return elseVal
}

// EvaluateFirst resolves a first(collection) expression.
func (e *RuntimeEvaluator) EvaluateFirst(collection any) any {
	if collection == nil {
		return nil
	}
	switch c := collection.(type) {
	case []any:
		if len(c) > 0 {
			return c[0]
		}
	case []map[string]any:
		if len(c) > 0 {
			return c[0]
		}
	}
	return nil
}

// EvaluateLast resolves a last(collection) expression.
func (e *RuntimeEvaluator) EvaluateLast(collection any) any {
	if collection == nil {
		return nil
	}
	switch c := collection.(type) {
	case []any:
		if len(c) > 0 {
			return c[len(c)-1]
		}
	case []map[string]any:
		if len(c) > 0 {
			return c[len(c)-1]
		}
	}
	return nil
}

// EvaluateLower resolves a lower(str) expression.
func (e *RuntimeEvaluator) EvaluateLower(str any) string {
	if str == nil {
		return ""
	}
	return strings.ToLower(fmt.Sprintf("%v", str))
}

// EvaluateUpper resolves an upper(str) expression.
func (e *RuntimeEvaluator) EvaluateUpper(str any) string {
	if str == nil {
		return ""
	}
	return strings.ToUpper(fmt.Sprintf("%v", str))
}

// EvaluateTrim resolves a trim(str) expression.
func (e *RuntimeEvaluator) EvaluateTrim(str any) string {
	if str == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", str))
}

// EvaluateHash resolves a hash(str) expression (SHA256).
func (e *RuntimeEvaluator) EvaluateHash(str any) string {
	if str == nil {
		return ""
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%v", str)))
	return hex.EncodeToString(hash[:])
}

// EvaluateCanonicalId resolves canonicalId(value, "<conceptType>") to
// the canonical id form (`<partition>:<conceptType>:<bareSlug>`).
// Mirrors the mutation-template builtin so automations + queries can
// produce stable derived ids regardless of input shape (bare vs
// already-canonical). See MemQLEngine.canonicalizeIdValue for the
// behavior matrix.
func (e *RuntimeEvaluator) EvaluateCanonicalId(ctx context.Context, value any, conceptType any) (string, error) {
	if value == nil {
		return "", nil
	}
	concept := strings.TrimSpace(fmt.Sprintf("%v", conceptType))
	if concept == "" {
		return "", fmt.Errorf("canonicalId: concept name is required")
	}
	v := strings.TrimSpace(fmt.Sprintf("%v", value))
	if v == "" {
		return "", nil
	}
	if e.ctx == nil || e.ctx.Engine == nil {
		return "", fmt.Errorf("canonicalId: engine not available")
	}
	return e.ctx.Engine.canonicalizeIdValue(ctx, v, concept)
}

// EvaluateContains resolves a contains(str, substr) expression.
func (e *RuntimeEvaluator) EvaluateContains(str, substr any) bool {
	if str == nil || substr == nil {
		return false
	}
	return strings.Contains(fmt.Sprintf("%v", str), fmt.Sprintf("%v", substr))
}

// EvaluateAnd resolves an and(a, b, ...) expression.
// Returns true if all arguments are truthy.
func (e *RuntimeEvaluator) EvaluateAnd(args ...any) bool {
	for _, arg := range args {
		if !isTruthy(arg) {
			return false
		}
	}
	return true
}

// EvaluateOr resolves an or(a, b, ...) expression.
// Returns true if any argument is truthy.
func (e *RuntimeEvaluator) EvaluateOr(args ...any) bool {
	for _, arg := range args {
		if isTruthy(arg) {
			return true
		}
	}
	return false
}

// EvaluateNot resolves a not(value) expression.
// Returns the boolean negation.
func (e *RuntimeEvaluator) EvaluateNot(value any) bool {
	return !isTruthy(value)
}

// EvaluateEq resolves an eq(a, b) expression.
// Returns true if a == b.
func (e *RuntimeEvaluator) EvaluateEq(a, b any) bool {
	return runtimeCompareValues(a, b) == 0
}

// EvaluateLt resolves a lt(a, b) expression.
// Returns true if a < b.
func (e *RuntimeEvaluator) EvaluateLt(a, b any) bool {
	return runtimeCompareValues(a, b) < 0
}

// EvaluateGt resolves a gt(a, b) expression.
// Returns true if a > b.
func (e *RuntimeEvaluator) EvaluateGt(a, b any) bool {
	return runtimeCompareValues(a, b) > 0
}

// EvaluateLte resolves a lte(a, b) expression.
// Returns true if a <= b.
func (e *RuntimeEvaluator) EvaluateLte(a, b any) bool {
	return runtimeCompareValues(a, b) <= 0
}

// EvaluateGte resolves a gte(a, b) expression.
// Returns true if a >= b.
func (e *RuntimeEvaluator) EvaluateGte(a, b any) bool {
	return runtimeCompareValues(a, b) >= 0
}

// EvaluateToString resolves a toString(value) expression.
// Converts any value to its string representation.
func (e *RuntimeEvaluator) EvaluateToString(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%v", value)
}

// EvaluateAddDuration resolves an addDuration(timestamp, duration) expression.
// Adds an ISO 8601 duration to a timestamp.
// Returns the resulting ISO 8601 timestamp string.
func (e *RuntimeEvaluator) EvaluateAddDuration(timestamp, duration string) (string, error) {
	t, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return "", fmt.Errorf("invalid timestamp %q: %w", timestamp, err)
	}

	d, err := parseISO8601Duration(duration)
	if err != nil {
		return "", fmt.Errorf("invalid duration %q: %w", duration, err)
	}

	return t.Add(d).Format(time.RFC3339), nil
}

// EvaluateDaysBetween resolves a daysBetween(date1, date2) expression.
// Returns the number of days between two dates (date2 - date1).
func (e *RuntimeEvaluator) EvaluateDaysBetween(date1, date2 string) (int, error) {
	t1, err := parseDate(date1)
	if err != nil {
		return 0, fmt.Errorf("invalid date1 %q: %w", date1, err)
	}

	t2, err := parseDate(date2)
	if err != nil {
		return 0, fmt.Errorf("invalid date2 %q: %w", date2, err)
	}

	// Calculate difference in days
	hours := t2.Sub(t1).Hours()
	return int(hours / 24), nil
}

// EvaluateSubtractTimestamps resolves a subtractTimestamps(t1, t2) expression.
// Calculates the duration between two timestamps (t1 - t2).
// Returns an ISO 8601 duration string.
func (e *RuntimeEvaluator) EvaluateSubtractTimestamps(t1, t2 string) (string, error) {
	time1, err := time.Parse(time.RFC3339, t1)
	if err != nil {
		return "", fmt.Errorf("invalid timestamp t1 %q: %w", t1, err)
	}

	time2, err := time.Parse(time.RFC3339, t2)
	if err != nil {
		return "", fmt.Errorf("invalid timestamp t2 %q: %w", t2, err)
	}

	d := time1.Sub(time2)
	return formatISO8601Duration(d), nil
}

// EvaluateYear resolves a year(timestamp) expression.
// Returns the year as an integer.
func (e *RuntimeEvaluator) EvaluateYear(timestamp string) (int, error) {
	t, err := parseDate(timestamp)
	if err != nil {
		return 0, fmt.Errorf("invalid timestamp %q: %w", timestamp, err)
	}
	return t.Year(), nil
}

// EvaluateQuarter resolves a quarter(timestamp) expression.
// Returns the calendar quarter (1-4).
func (e *RuntimeEvaluator) EvaluateQuarter(timestamp string) (int, error) {
	t, err := parseDate(timestamp)
	if err != nil {
		return 0, fmt.Errorf("invalid timestamp %q: %w", timestamp, err)
	}
	return (int(t.Month())-1)/3 + 1, nil
}

// EvaluateMonth resolves a month(timestamp) expression.
// Returns the month (1-12).
func (e *RuntimeEvaluator) EvaluateMonth(timestamp string) (int, error) {
	t, err := parseDate(timestamp)
	if err != nil {
		return 0, fmt.Errorf("invalid timestamp %q: %w", timestamp, err)
	}
	return int(t.Month()), nil
}

// EvaluateDayOfMonth resolves a dayOfMonth(timestamp) expression.
// Returns the day of the month (1-31).
func (e *RuntimeEvaluator) EvaluateDayOfMonth(timestamp string) (int, error) {
	t, err := parseDate(timestamp)
	if err != nil {
		return 0, fmt.Errorf("invalid timestamp %q: %w", timestamp, err)
	}
	return t.Day(), nil
}

// EvaluateIsAnniversary resolves an isAnniversary(startDate, checkDate) expression.
// Returns true if checkDate is an anniversary of startDate (same month and day).
func (e *RuntimeEvaluator) EvaluateIsAnniversary(startDate, checkDate string) (bool, error) {
	start, err := parseDate(startDate)
	if err != nil {
		return false, fmt.Errorf("invalid startDate %q: %w", startDate, err)
	}

	check, err := parseDate(checkDate)
	if err != nil {
		return false, fmt.Errorf("invalid checkDate %q: %w", checkDate, err)
	}

	return start.Month() == check.Month() && start.Day() == check.Day(), nil
}

// EvaluateIsFirstDayOfQuarter resolves an isFirstDayOfQuarter(timestamp) expression.
// Returns true if the date is Jan 1, Apr 1, Jul 1, or Oct 1.
func (e *RuntimeEvaluator) EvaluateIsFirstDayOfQuarter(timestamp string) (bool, error) {
	t, err := parseDate(timestamp)
	if err != nil {
		return false, fmt.Errorf("invalid timestamp %q: %w", timestamp, err)
	}

	// First day of quarter: Jan 1, Apr 1, Jul 1, Oct 1
	month := t.Month()
	day := t.Day()
	return day == 1 && (month == time.January || month == time.April || month == time.July || month == time.October), nil
}

// runtimeCompareValues compares two values and returns:
// -1 if a < b, 0 if a == b, 1 if a > b
func runtimeCompareValues(a, b any) int {
	// Handle nil cases
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}

	// Convert to float64 for numeric comparison
	fa, aIsNum := runtimeToFloat64(a)
	fb, bIsNum := runtimeToFloat64(b)

	if aIsNum && bIsNum {
		if fa < fb {
			return -1
		}
		if fa > fb {
			return 1
		}
		return 0
	}

	// String comparison
	sa := fmt.Sprintf("%v", a)
	sb := fmt.Sprintf("%v", b)
	if sa < sb {
		return -1
	}
	if sa > sb {
		return 1
	}
	return 0
}

// runtimeToFloat64 attempts to convert a value to float64.
func runtimeToFloat64(v any) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case int32:
		return float64(val), true
	case uint:
		return float64(val), true
	case uint64:
		return float64(val), true
	case uint32:
		return float64(val), true
	default:
		return 0, false
	}
}

// parseDate parses a date string in various formats.
func parseDate(s string) (time.Time, error) {
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, format := range formats {
		if t, err := time.Parse(format, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date format")
}

// parseISO8601Duration parses an ISO 8601 duration string (e.g., "PT24H", "P1D", "PT30M").
func parseISO8601Duration(s string) (time.Duration, error) {
	if len(s) < 2 || s[0] != 'P' {
		return 0, fmt.Errorf("duration must start with P")
	}

	var d time.Duration
	s = s[1:] // Remove 'P'

	// Handle days before 'T'
	if idx := strings.Index(s, "D"); idx != -1 {
		days := 0
		if _, err := fmt.Sscanf(s[:idx], "%d", &days); err == nil {
			d += time.Duration(days) * 24 * time.Hour
		}
		s = s[idx+1:]
	}

	// Handle time components after 'T'
	if len(s) > 0 && s[0] == 'T' {
		s = s[1:]

		// Hours
		if idx := strings.Index(s, "H"); idx != -1 {
			hours := 0
			if _, err := fmt.Sscanf(s[:idx], "%d", &hours); err == nil {
				d += time.Duration(hours) * time.Hour
			}
			s = s[idx+1:]
		}

		// Minutes
		if idx := strings.Index(s, "M"); idx != -1 {
			minutes := 0
			if _, err := fmt.Sscanf(s[:idx], "%d", &minutes); err == nil {
				d += time.Duration(minutes) * time.Minute
			}
			s = s[idx+1:]
		}

		// Seconds
		if idx := strings.Index(s, "S"); idx != -1 {
			seconds := 0
			if _, err := fmt.Sscanf(s[:idx], "%d", &seconds); err == nil {
				d += time.Duration(seconds) * time.Second
			}
		}
	}

	return d, nil
}

// formatISO8601Duration formats a duration as an ISO 8601 duration string.
func formatISO8601Duration(d time.Duration) string {
	if d < 0 {
		d = -d
	}

	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if hours >= 24 {
		days := hours / 24
		hours = hours % 24
		if hours == 0 && minutes == 0 && seconds == 0 {
			return fmt.Sprintf("P%dD", days)
		}
		return fmt.Sprintf("P%dDT%dH%dM%dS", days, hours, minutes, seconds)
	}

	if hours == 0 && minutes == 0 {
		return fmt.Sprintf("PT%dS", seconds)
	}
	if hours == 0 {
		return fmt.Sprintf("PT%dM%dS", minutes, seconds)
	}
	return fmt.Sprintf("PT%dH%dM%dS", hours, minutes, seconds)
}

// isTruthy determines if a value is truthy.
func isTruthy(v any) bool {
	if v == nil {
		return false
	}
	switch val := v.(type) {
	case bool:
		return val
	case string:
		return val != ""
	case int:
		return val != 0
	case int64:
		return val != 0
	case float64:
		return val != 0
	case []any:
		return len(val) > 0
	case map[string]any:
		return len(val) > 0
	default:
		return true
	}
}
