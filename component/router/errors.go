package router

import (
	"context"
	"errors"
	"strings"
)

// CategorizeError maps an error to a short classifier string for the
// v1:router:call.errorCategory field. Intentionally coarse -- fine-grained
// error diagnostics live in the full message; this column is for grouping
// in dashboards.
func CategorizeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "rate_limit"), strings.Contains(lower, "429"):
		return "rate_limit"
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "deadline"):
		return "timeout"
	case strings.Contains(lower, "unauthor"), strings.Contains(lower, "forbidden"), strings.Contains(lower, "api key"), strings.Contains(lower, "401"), strings.Contains(lower, "403"):
		return "auth"
	case strings.Contains(lower, "5") && (strings.Contains(lower, "server error") || strings.Contains(lower, "internal")):
		return "upstream"
	case strings.Contains(lower, "canceled"), strings.Contains(lower, "cancelled"):
		return "cancelled"
	default:
		return "other"
	}
}

// TruncateError truncates an error message to the given maximum length,
// appending an ellipsis marker when truncation occurred. Used to keep
// CallRecord.ErrorMessage at a reasonable size for the ledger row.
func TruncateError(msg string, max int) string {
	if len(msg) <= max {
		return msg
	}
	if max <= 3 {
		return msg[:max]
	}
	return msg[:max-3] + "..."
}
