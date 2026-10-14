package memql

import (
	"fmt"
	"strings"
)

// ResultCursor lifts the continuation token off whatever the engine seam
// handed back. Two shapes, and both are real: the in-process engine returns
// an *ExecuteResult carrying ResultMeta, and the package's tests fake flat row
// envelopes (the seam is `Execute(ctx, string) (any, error)` precisely so they
// can). A result with neither is an exhausted page, which is the safe reading
// -- it ends the walk rather than continuing from a position nobody supplied.
func ResultCursor(res any) string {
	type metaCarrier interface{ GetMeta() *ResultMeta }
	if carrier, ok := res.(metaCarrier); ok {
		if meta := carrier.GetMeta(); meta != nil {
			return strings.TrimSpace(meta.Cursor)
		}
	}
	if envelope, ok := res.(map[string]any); ok {
		if raw, ok := envelope["cursor"].(string); ok {
			return strings.TrimSpace(raw)
		}
		if meta, ok := envelope["meta"].(map[string]any); ok {
			if raw, ok := meta["cursor"].(string); ok {
				return strings.TrimSpace(raw)
			}
		}
	}
	return ""
}

// ResultPageCursor refuses a truncated page with no continuation. Treating it as
// exhaustion would silently turn a bounded read into a complete audience.
func ResultPageCursor(res any) (string, error) {
	cursor := ResultCursor(res)
	more := false
	if carrier, ok := res.(interface{ GetMeta() *ResultMeta }); ok {
		if meta := carrier.GetMeta(); meta != nil {
			more = meta.HasMore
		}
	}
	if envelope, ok := res.(map[string]any); ok {
		more, _ = envelope["hasMore"].(bool)
		if meta, ok := envelope["meta"].(map[string]any); ok {
			if value, exists := meta["hasMore"].(bool); exists {
				more = value
			}
		}
	}
	if more && cursor == "" {
		return "", fmt.Errorf("incomplete page has no continuation cursor")
	}
	return cursor, nil
}
