package memql

import (
	"testing"
)

// cache_cursor_key_test.go proves the result-cache key isolates keyset
// cursor pages (epic 5, issue 5.5). Before the fix e.cacheKey omitted the
// cursor, so two distinct continuation pages of the same @cache'd query
// collided on a single key and page 2 was served the cached page-1 rows.
// This is the HARD PREREQUISITE that has to land together with the first
// @cache adoption -- it is latent today (no @cache in use) but activates
// the instant caching is turned on for any paginated query.

func TestCacheKey_CursorIsolatesPages(t *testing.T) {
	e := &MemQLEngine{}

	const (
		query    = "concept==v1:cognition:utterance"
		sortSig  = "createdAt:desc"
		selSig   = "none"
		shapeSig = "graph-bundle"
		limit    = 50
		offset   = 0
		depth    = 0
	)

	// Page 1 of a keyset-paginated query: no inbound cursor (empty token).
	page1 := e.cacheKey(query, nil, limit, offset, depth, sortSig, selSig, shapeSig, "")
	// Page 2: the continuation cursor minted from page 1's last row.
	page2 := e.cacheKey(query, nil, limit, offset, depth, sortSig, selSig, shapeSig, "cursor-token-page-2")
	// Page 3: a different continuation cursor again.
	page3 := e.cacheKey(query, nil, limit, offset, depth, sortSig, selSig, shapeSig, "cursor-token-page-3")

	if page1 == page2 {
		t.Fatalf("page 1 and page 2 collide on the same cache key %q -- a cached page 1 would be served as page 2 (stale-page bug)", page1)
	}
	if page2 == page3 {
		t.Fatalf("page 2 and page 3 collide on the same cache key %q -- distinct continuation pages must key independently", page2)
	}
	if page1 == page3 {
		t.Fatalf("page 1 and page 3 collide on the same cache key %q", page1)
	}
}

func TestCacheKey_SameCursorIsStable(t *testing.T) {
	e := &MemQLEngine{}
	a := e.cacheKey("concept==v1:x:y", nil, 50, 0, 0, "createdAt:desc", "none", "graph-bundle", "tok-1")
	b := e.cacheKey("concept==v1:x:y", nil, 50, 0, 0, "createdAt:desc", "none", "graph-bundle", "tok-1")
	if a != b {
		t.Fatalf("identical inputs must produce a stable cache key: %q != %q", a, b)
	}
	// And the empty-cursor (non-paginated) key is itself stable and distinct
	// from any populated-cursor key.
	empty1 := e.cacheKey("concept==v1:x:y", nil, 50, 0, 0, "createdAt:desc", "none", "graph-bundle", "")
	empty2 := e.cacheKey("concept==v1:x:y", nil, 50, 0, 0, "createdAt:desc", "none", "graph-bundle", "")
	if empty1 != empty2 {
		t.Fatalf("empty-cursor key must be stable: %q != %q", empty1, empty2)
	}
	if empty1 == a {
		t.Fatalf("empty-cursor key must differ from a populated-cursor key")
	}
}
