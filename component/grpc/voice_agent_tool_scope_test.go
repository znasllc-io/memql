package memql

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestToolSlugsFromAgentRows covers the #1419 row-shape handling: the agent
// row's tools list arrives as []any (JSON-decoded) or []string, an existing
// row without a tools field is a real empty answer (found=true, scope to
// nothing), and no rows at all means the lookup failed (found=false, callers
// fail open to the unscoped registry).
func TestToolSlugsFromAgentRows(t *testing.T) {
	t.Run("json-decoded any list", func(t *testing.T) {
		slugs, found := toolSlugsFromAgentRows([]map[string]any{
			{"id": "a1", "tools": []any{"webSearch", "workbench_use", "", 42}},
		})
		assert.True(t, found)
		assert.Equal(t, []string{"webSearch", "workbench_use"}, slugs, "non-strings and empties are dropped")
	})

	t.Run("typed string list", func(t *testing.T) {
		slugs, found := toolSlugsFromAgentRows([]map[string]any{
			{"id": "a1", "tools": []string{"produceArtifact"}},
		})
		assert.True(t, found)
		assert.Equal(t, []string{"produceArtifact"}, slugs)
	})

	t.Run("row without tools is a real empty answer", func(t *testing.T) {
		slugs, found := toolSlugsFromAgentRows([]map[string]any{{"id": "a1"}})
		assert.True(t, found, "the agent exists; it just has no tools")
		assert.Empty(t, slugs)
	})

	t.Run("no rows fails open", func(t *testing.T) {
		_, found := toolSlugsFromAgentRows(nil)
		assert.False(t, found, "no row -> caller must fall back to the unscoped registry")
	})
}
