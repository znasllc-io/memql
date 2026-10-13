package app

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"

	"errors"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKnowledgePublicationMaintenanceIsOneBoundedPass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls, warnings atomic.Int32
	done := make(chan struct{})
	s := &knowledgeSeedMaintenance{
		reconcile: func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 30*time.Second {
				t.Error("repair has no bounded phase")
			}
			calls.Add(1)
			return errors.New("temporary database failure")
		}, warn: func(error) { warnings.Add(1) },
	}
	go func() { defer close(done); s.run(ctx) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not stop")
	}
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, int32(1), warnings.Load())
}

// Exercise the installed factory and app registration, with an empty isolated
// schema so another test's registered corpus cannot supply accidental evidence.
func TestKnowledgePublicationStartupRegistersWithoutCreatingCorpus(t *testing.T) {
	ctx := context.Background()
	admin := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "knowledge startup", dbtest.DSN(), err)
		return
	}
	schema := fmt.Sprintf("knowledge_boot_%d", time.Now().UnixNano())
	_, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`) })
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithConnParams(map[string]any{"search_path": schema}))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `CREATE TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`)
	require.NoError(t, err)
	_, err = memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	engine, err := memql.New(db)
	require.NoError(t, err)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine.Logger = logger
	require.NoError(t, engine.Init(memorynodes.DefaultRegistry()))
	for _, plugin := range memql.RegisteredPlugins() {
		if plugin.Name == "knowledge" {
			provider, err := plugin.Factory(memql.PluginContext{Logger: logger, Engine: engine, BunDB: func() *bun.DB { return db },
				ResolvePartitionFromContext: func(context.Context) string { return "default" }, ConceptDataIsStaged: func(string) bool { return false },
				ResolveEmbeddingProvider: func(context.Context, string) (memql.EmbeddingAIProvider, error) {
					return nil, fmt.Errorf("startup must not resolve an embedder")
				},
			})
			require.NoError(t, err)
			require.NoError(t, engine.RegisterIntegration(provider))
		}
	}
	a := &App{Logger: logger, engine: engine}
	a.registerKnowledgeSeedMaintenance()
	require.Len(t, a.Dependencies, 1)
	lifecycle, ok := a.Dependencies[0].(*knowledgeSeedMaintenance)
	require.True(t, ok)
	original := lifecycle.reconcile
	result := make(chan error, 1)
	lifecycle.reconcile = func(ctx context.Context) error { err := original(ctx); result <- err; return err }
	lifecycle.Start(ctx)
	t.Cleanup(func() { lifecycle.Stop(ctx) })
	select {
	case <-lifecycle.Ready():
	case <-time.After(time.Second):
		t.Fatal("startup readiness was blocked by knowledge")
	}
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("startup publication pass did not finish")
	}
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes"`).Scan(&count))
	require.Zero(t, count, "empty startup must not install or generate a corpus")
}
