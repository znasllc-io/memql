package cognition

import (
	"context"
	"fmt"
	"strings"
)

// getSpaceContextForPrompt returns the latest space context object for prompt inputs.
// The returned object is JSON-friendly and safe to pass into si() prompt templates.
func (c *CognitionIntegration) getSpaceContextForPrompt(ctx context.Context, spaceId string) map[string]any {
	if c == nil || c.engine == nil {
		return nil
	}
	spaceId = strings.TrimSpace(spaceId)
	if spaceId == "" {
		return nil
	}

	// memql#288 (sub-epic #286): migrated from a handwritten
	// `shape(paginate(sort(filter, "createdAt", "desc"), 1),
	// "spaceContextFull")` runtime query to the DSL-defined
	// queryLatestSpaceContextForSpace -- added in memql#294 as the
	// in-tree exerciser for the wired sort/paginate struct-query
	// directives. Downstream extractDataFromResult sees the same
	// spaceContextFull-shaped result so the type-switch below is
	// unchanged. Unblocks #250.
	query := fmt.Sprintf(`queryLatestSpaceContextForSpace({spaceId: %s})`, escapeJSONString(spaceId))
	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		return nil
	}
	data, err := extractDataFromResult(result)
	if err != nil {
		return nil
	}
	switch v := data.(type) {
	case []any:
		if len(v) > 0 {
			if m, ok := v[0].(map[string]any); ok {
				return m
			}
		}
	case map[string]any:
		return v
	}
	return nil
}
