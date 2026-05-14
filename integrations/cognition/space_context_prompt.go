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

	query := fmt.Sprintf(`shape(
  paginate(sort(concept==v1:cognition:space:context;payload.spaceId==%s, "createdAt", "desc"), 1),
  "spaceContextFull"
)`, escapeJSONString(spaceId))

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
