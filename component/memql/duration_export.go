package memql

import "time"

// ParseISO8601Duration parses an ISO 8601 duration string (e.g. "PT24H",
// "P30D", "PT30M"). It is the exported entry point onto the same parser the
// RuntimeEvaluator's addDuration uses, so the automation/logic condition
// evaluator (component/automations) shares one duration semantics rather than
// re-implementing its own (#2256).
func ParseISO8601Duration(s string) (time.Duration, error) {
	return parseISO8601Duration(s)
}

// ParseFlexibleTimestamp parses an RFC3339 / date-only timestamp string,
// accepting the same format set as the RuntimeEvaluator's date builtins
// (RFC3339, "2006-01-02T15:04:05Z", "2006-01-02T15:04:05", "2006-01-02").
// Exported so the condition evaluator can compare date-window operands as
// timestamps instead of coercing them to 0 via toNumber (#2254).
func ParseFlexibleTimestamp(s string) (time.Time, error) {
	return parseDate(s)
}
