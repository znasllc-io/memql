package worktrace

// fields.go -- the two payload readers the trace reader needs, moved here
// with the assembler when the harness module was retired (work spine A1).
// A node's payload arrives as decoded JSON, so every read is a type
// assertion; both helpers answer the zero value rather than panicking,
// because one malformed row must not break a whole timeline.

import "strings"

// stringField reads a string field, trimmed. Missing, null or wrongly
// typed all read as "".
func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// objectField reads a nested object field. Missing or wrongly typed reads
// as nil, which the renderer treats as "no structured data".
func objectField(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

// firstNonEmpty returns the first value that is not blank. The renderer
// uses it to fall back through a header's candidate labels.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
