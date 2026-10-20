package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrivateReadMergeEngineClosesItsCache(t *testing.T) {
	var cache *resultCache
	t.Run("private engine lifetime", func(t *testing.T) {
		engine, _, _ := readMergeTestEngine(t)
		cache = engine.cache
		require.NotNil(t, cache)
		require.True(t, cache.cache.Set("cleanup-probe", "before", 1))
		// Some fixtures disable caching after boot. Cleanup must retain the
		// original cache, rather than consulting the modified engine field.
		engine.cache = nil
	})
	if cache == nil {
		return // The database-gated child reports an unavailable test database.
	}
	require.False(t, cache.cache.Set("cleanup-probe", "after", 1), "private fixture cache workers must stop with their test")
}
