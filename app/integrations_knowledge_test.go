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
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/knowledge/seeddata"
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

func TestKnowledgePublicationStartupOwnsFreshOriginAndCancellation(t *testing.T) {
	type requestKey struct{}
	inbound, cancel := context.WithCancel(auth.ContextWithUserActor(context.WithValue(context.Background(), requestKey{}, "request-only"), "person"))
	cancel()
	entered := make(chan context.Context, 1)
	finished := make(chan struct{})
	s := &knowledgeSeedMaintenance{ready: make(chan struct{}), warn: func(error) { t.Error("cancelled maintenance warned") }, reconcile: func(ctx context.Context) error {
		entered <- ctx
		<-ctx.Done()
		close(finished)
		return ctx.Err()
	}}
	s.Start(inbound)
	t.Cleanup(func() { s.Stop(context.Background()) })
	select {
	case ctx := <-entered:
		require.Equal(t, auth.OriginInternal, auth.OriginFromContext(ctx))
		require.Nil(t, auth.TokenInfoFromContext(ctx))
		require.Nil(t, ctx.Value(requestKey{}))
		require.NoError(t, ctx.Err(), "caller cancellation must not become the maintenance lifetime")
		_, bounded := ctx.Deadline()
		require.True(t, bounded)
	case <-time.After(time.Second):
		t.Fatal("maintenance did not start")
	}
	s.Stop(context.Background())
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the maintenance pass")
	}
}

// Exercise the installed factory and app registration, with an empty isolated
// schema so another test's registered corpus cannot supply accidental evidence.
func TestKnowledgePublicationStartupSuppliesSelectedReviewedCorpusWithoutOwnerOrEmbedder(t *testing.T) {
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
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	require.NoError(t, err)
	t.Cleanup(func() { gates.Close() })
	for _, plugin := range memql.RegisteredPlugins() {
		if plugin.Name == "knowledge" {
			provider, err := plugin.Factory(memql.PluginContext{Logger: logger, Engine: engine, GateDB: func() *sql.DB { return gates }, BunDB: func() *bun.DB { return db },
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
	// The production phase is 30 seconds, with at most one retained 15-second
	// commit on cancellation; allow that same bound under race instrumentation.
	case <-time.After(45 * time.Second):
		t.Fatal("startup publication pass did not finish")
	}
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept='v1:knowledge:documentChunk'`).Scan(&count))
	require.Equal(t, len(seeddata.Release()), count, "fresh startup publishes only the explicit checked-in release selection")
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT DISTINCT ON (id) payload FROM "MemoryNodes" WHERE concept='v1:knowledge:documentChunk' ORDER BY id,"createdAt" DESC) latest WHERE payload->>'validationStatus'='validated'`).Scan(&count))
	require.Equal(t, len(seeddata.Release()), count, "ownerless publication approves only registered exact bytes")
	// These are the existing Training UI reads: domain cards are chunk-derived,
	// and opening one reads full reviewable passages without catalog rows.
	viewer := auth.ContextWithUserActor(context.Background(), "fresh-owner")
	resultRows, err := engine.Execute(viewer, "query knowledgeDomainsAll()")
	require.NoError(t, err)
	require.Empty(t, memql.MaterializeRows(resultRows))
	resultRows, err = engine.Execute(viewer, "query allDocumentChunkDomains()")
	require.NoError(t, err)
	domains := map[string]bool{}
	for _, row := range memql.MaterializeRows(resultRows) {
		domains[row["domainId"].(string)] = true
	}
	require.Equal(t, map[string]bool{
		"computer-use":         true,
		"workbench":            true,
		"work-guidance":        true,
		"software-development": true,
		"research-methodology": true,
	}, domains, "fresh startup exposes exactly the reviewed release domains")
	for _, seed := range seeddata.Release() {
		require.True(t, domains[seed.Domain])
	}
	resultRows, err = engine.Execute(viewer, `query documentChunksForDomain(domainId:"software-development")`)
	require.NoError(t, err)
	lessons := memql.MaterializeRows(resultRows)
	require.NotEmpty(t, lessons)
	for _, row := range lessons {
		require.NotEmpty(t, row["text"])
		require.Contains(t, row["sourceRef"], "training:")
		require.Equal(t, "validated", row["validationStatus"])
	}
}
