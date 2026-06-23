package memql

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// cache_ttl_load_test.go proves the struct-form `@cache(ttl=N)` annotation
// survives the full per-construct load path (struct rewriter -> langparser
// -> AST converter -> applyQueryCacheTTL) and ends up as a cache hint the
// engine consumes. This is the end-to-end proof for 5.5 adoption: before
// the wiring, `@cache` was parsed onto fn.CacheTTL but never stamped onto
// the executable expression, so caching never engaged on any struct query.

func cacheLoadRegistry() memoryNodes.Registry {
	return newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:agents:agentRole": {Name: "v1:agents:agentRole"},
	})
}

// loadCachedQueryHints loads a struct query carrying the given cache
// annotation and returns the per-concept cache hints the engine would see.
func loadCachedQueryHints(t *testing.T, cacheAnnotation string) map[string]int64 {
	t.Helper()
	src := "use agents.concepts.{ agentRole }\n\n" +
		"@enabled\n" +
		cacheAnnotation + "\n" +
		"query agentRole queryRolesCached {\n" +
		"  filter  payload.active == true\n" +
		"  shape   agentRole\n" +
		"}"
	fn, err := tryParseNewFunctionSyntax("queryRolesCached", "query", src, "agents.queries.memql", cacheLoadRegistry())
	require.NoError(t, err)
	require.NotNil(t, fn)
	require.Equal(t, cacheAnnotationTTL(cacheAnnotation), fn.CacheTTL)

	hints := map[string]int64{}
	collectCacheHints(fn.Expr, hints)
	return hints
}

func cacheAnnotationTTL(annotation string) string {
	switch annotation {
	case `@cache(ttl="300")`:
		return "300"
	case `@cache(ttl="0")`:
		return "0"
	default:
		return ""
	}
}

func TestLoad_CacheTTLStampsHint(t *testing.T) {
	hints := loadCachedQueryHints(t, `@cache(ttl="300")`)
	got, ok := hints["v1:agents:agentrole"]
	if !ok {
		// collectCacheHints lowercases the concept key.
		got, ok = hints["v1:agents:agentRole"]
	}
	require.Truef(t, ok, "expected a cache hint keyed by the bound concept, got %v", hints)
	require.Equal(t, int64(300), got)
}

func TestLoad_CacheTTLZeroIsNeverCache(t *testing.T) {
	hints := loadCachedQueryHints(t, `@cache(ttl="0")`)
	// A 0 hint must be present (explicit never-cache), not absent.
	var found bool
	for _, v := range hints {
		found = true
		require.Equal(t, int64(0), v)
	}
	require.Truef(t, found, "@cache(ttl=0) must record an explicit 0 hint, got %v", hints)
}

func TestLoad_NoCacheAnnotationNoHint(t *testing.T) {
	hints := loadCachedQueryHints(t, `@description("no cache")`)
	require.Empty(t, hints, "a query without @cache must produce no cache hint (cache stays off)")
}

// TestLoad_EmbeddedCachedQueriesCarryHint is the regression guard for the
// 5.5 adoption set: it loads the REAL embedded DSL through the same
// unified loader the engine runs and asserts every query we annotated with
// @cache(ttl=N) actually carries the per-concept cache hint. If a future
// edit drops the @cache annotation, the wiring, or the annotation
// allow-list entry, the corresponding query stops caching silently -- this
// test fails instead.
func TestLoad_EmbeddedCachedQueriesCarryHint(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nullWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))

	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	concepts := memoryNodes.DefaultRegistry()

	functionRegistry, err := loadEmbeddedFunctions(logger, concepts)
	if err != nil {
		t.Fatalf("loadEmbeddedFunctions: %v", err)
	}
	if _, _, err := LoadUnifiedFunctions(logger, functionRegistry, concepts); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}

	// query name -> expected TTL seconds (the values authored in dsl/).
	want := map[string]int64{
		"activeAgentRoles": 300,
		"agentRoleBySlug":  300,
		"activeSkills":     300,
		"activeSkillsFull": 300,
		"skillBySlug":      300,
		"routerBudgets":    120,
		"spaceUtterances":  30,
	}

	for name, ttl := range want {
		fn, err := functionRegistry.Get(name)
		if err != nil {
			t.Errorf("%s: not registered: %v", name, err)
			continue
		}
		hints := map[string]int64{}
		collectCacheHints(fn.Expr, hints)
		if len(hints) == 0 {
			t.Errorf("%s: @cache hint missing -- caching is silently OFF for this query", name)
			continue
		}
		for _, got := range hints {
			if got != ttl {
				t.Errorf("%s: cache hint = %ds, want %ds", name, got, ttl)
			}
		}
	}
}

type nullWriter struct{}

func (nullWriter) Write(p []byte) (int, error) { return len(p), nil }
