package memql

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// newRealDSLEngine boots a real MemQLEngine with the full embedded DSL loaded
// (no DB). The parser + named-query resolution are fully live; only DB-backed
// Execute needs a database.
//
// It exists because a handler's query string is only ever checked by the REAL
// parser: a suite that feeds a mock engine records the string and parses
// nothing, which is how a retired call form survived three fixes before
// memql#4256. It lived in voice_agent_real_engine_test.go, which went with the
// voice node (epic memql#4988); deploy_control_parse_test.go and
// render_query_args_parse_test.go are its callers now.
func newRealDSLEngine(t *testing.T) *memqlengine.MemQLEngine {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (dsl/ domain-first tree): %v", err)
	}
	registry := concept.DefaultRegistry()
	require.NotNil(t, registry)
	eng, err := memqlengine.New(nil)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(registry))
	return eng
}
