package memql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/id"
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

// EvaluateCoalesce resolves a coalesce(a, b, ...) expression: it returns
// the first non-nil / non-empty argument, falling back to the FINAL
// argument -- the ultimate fallback -- which is returned even when it is
// an empty string (memql#1614). That makes an explicit empty-string
// fallback expressible: coalesce(absent, "") -> "". A non-final arg that
// resolves to nil or "" is still treated as missing and skipped, so
// coalesce("", fallback) -> fallback and coalesce(emptyField, fallback) ->
// fallback are unchanged.
func (e *RuntimeEvaluator) EvaluateCoalesce(args ...any) any {
	for i, arg := range args {
		if i == len(args)-1 {
			return arg
		}
		if arg == nil {
			continue
		}
		if s, ok := arg.(string); ok && s == "" {
			continue
		}
		return arg
	}
	return nil
}

// EvaluateIf resolves an if(cond, then, else) expression.
func (e *RuntimeEvaluator) EvaluateIf(cond, thenVal, elseVal any) any {
	if IsTruthy(cond) {
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

// EvaluateShortId resolves shortId(value) to the bare short id by
// stripping any `<concept>:` prefix. The inverse of EvaluateCanonicalId.
// An already-bare value passes through unchanged; empty / nil input
// yields "". NOT idempotent in general -- it strips ONE prefix, so
// "v1:a:b:v2:c:d:e" -> "v2:c:d:e" -> "e" (memql#2981). Mirrors the
// mutation-template builtin so automations + queries normalize ids to
// one short form. See #1859.
//
// NOTE: the no-engine fallback below re-implements the structural split
// inline rather than calling BareShortId. The two agree today because
// both strip exactly once; if the primitive is ever changed to loop to a
// fixpoint, THIS is the path that silently keeps the old behaviour
// (memql#2981).
func (e *RuntimeEvaluator) EvaluateShortId(value any) string {
	if value == nil {
		return ""
	}
	v := strings.TrimSpace(fmt.Sprintf("%v", value))
	if v == "" {
		return ""
	}
	if e.ctx != nil && e.ctx.Engine != nil {
		return e.ctx.Engine.shortIdValue(v)
	}
	// No engine wired: do the structural split inline so the builtin
	// still works in evaluation contexts without a full engine.
	_, short, err := id.ParseNodeId(v)
	if err != nil || short == "" {
		return v
	}
	return short
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
		if !IsTruthy(arg) {
			return false
		}
	}
	return true
}

// EvaluateOr resolves an or(a, b, ...) expression.
// Returns true if any argument is truthy.
func (e *RuntimeEvaluator) EvaluateOr(args ...any) bool {
	for _, arg := range args {
		if IsTruthy(arg) {
			return true
		}
	}
	return false
}

// EvaluateNot resolves a not(value) expression.
// Returns the boolean negation.
func (e *RuntimeEvaluator) EvaluateNot(value any) bool {
	return !IsTruthy(value)
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
	// A leading sign negates the whole duration (ISO-8601 "-P1D"). This is
	// the documented subtractTimestamps replacement (#2707): addDuration
	// with a negative duration subtracts.
	negative := false
	if len(s) > 0 && s[0] == '-' {
		negative = true
		s = s[1:]
	}
	if len(s) < 2 || s[0] != 'P' {
		return 0, fmt.Errorf("duration must start with P (optionally -P for a negative duration)")
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

	if negative {
		d = -d
	}
	return d, nil
}

// IsTruthy is THE truthiness rule for the language: one implementation, used
// by every path that has to decide whether a value counts as true (memql#2963).
//
// It used to be two. This package's copy read a string as truthy whenever it
// was non-empty; component/automations' copy also excluded "false" and "0". The
// same authored source therefore produced different answers depending on the
// shape of the body it sat in -- measured through both paths with
// `cond(args.allowed, "Y", "N")`:
//
//	input        single-statement   multi-statement
//	"false"      "Y"                "N"     <- diverged
//	"0"          "Y"                "N"     <- diverged
//	nil "" false 0                  agreed (falsy)
//	true 1 2.5 "true" "nonempty"    agreed (truthy)
//
// The STRICT rule is canonical, for two reasons that point the same way.
//
// The safety one: the divergence only ever appeared on a gate, and the
// permissive rule is the one that fails OPEN. `return cond(args.allowed, true,
// false)` handed the string "false" -- exactly what a JSON, HTTP or MCP caller
// sends for a boolean it stringified -- opened the gate. When two
// implementations disagree and one of them fails open, the closed one wins.
//
// The plain one: an author who writes `"false"` means false. Reading it as true
// because the string is non-empty is a Go type-switch artifact, not a language
// decision anyone made.
//
// The cost is stated rather than hidden: a string value of literally "false" or
// "0" is now falsy everywhere this is consulted -- `.any()`, `.all()`, `&&`,
// `||`, `!`, the mutation-template conditionals -- not only in cond. That is
// the point of having one rule, and it is the direction that surprises fewer
// people.
func IsTruthy(v any) bool {
	if v == nil {
		return false
	}
	switch val := v.(type) {
	case bool:
		return val
	case string:
		return val != "" && val != "false" && val != "0"
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
