package knowledge

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/knowledge/seeddata"
	"github.com/znasllc-io/memql/integrations/similarity"
)

type releaseEmbedding struct {
	mu    sync.Mutex
	calls map[string]int
}

func (p *releaseEmbedding) Embed(ctx context.Context, s string) ([]float32, error) {
	p.mu.Lock()
	p.calls[s]++
	p.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	return seedTestProvider{}.Embed(ctx, s)
}
func (p *releaseEmbedding) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	var out [][]float32
	for _, s := range texts {
		v, e := p.Embed(ctx, s)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}
func (*releaseEmbedding) Dimensions() int { return 1024 }

func releaseFixture(t *testing.T) (*seedFixture, context.Context) {
	t.Helper()
	f := newSeedFixture(t)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	require.NoError(t, err)
	t.Cleanup(func() { gates.Close() })
	for _, i := range []*Integration{f.a, f.b} {
		i.SetGateDB(func() *sql.DB { return gates })
	}
	keys := []string{}
	for _, s := range seeddata.Release() {
		keys = append(keys, id.BuildNodeId(conceptKnowledgeDocumentChunk, s.ID))
	}
	clean := func() {
		for _, key := range keys {
			_, e := f.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept='v1:knowledge:documentChunk' AND id=$1`, key)
			require.NoError(t, e)
		}
	}
	clean()
	t.Cleanup(clean)
	return f, auth.ContextWithInternalOrigin(context.Background())
}

func TestReleaseSeedsFirstBootAndConcurrentIndexingAcrossReplicas(t *testing.T) {
	f, ctx := releaseFixture(t)
	require.ErrorContains(t, f.a.ReconcileReleaseSeeds(context.Background(), false), "internal origin")
	// Publication is independent of an owner and model. Both replicas race from
	// a genuinely empty corpus using the installed compiled DSL workflow.
	f.a.SetEmbeddingProvider(nil)
	f.b.SetEmbeddingProvider(nil)
	errs := make(chan error, 2)
	go func() { errs <- f.a.ReconcileReleaseSeeds(ctx, false) }()
	go func() { errs <- f.b.ReconcileReleaseSeeds(ctx, false) }()
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	for _, seed := range seeddata.Release() {
		row, e := f.b.seedChunkSnapshot(ctx, seed.ID)
		require.NoError(t, e)
		require.NotNil(t, row)
		require.Equal(t, "validated", row.Payload["validationStatus"])
		require.True(t, seed.Matches(row.Payload))
	}
	p := &releaseEmbedding{calls: map[string]int{}}
	for _, i := range []*Integration{f.a, f.b} {
		i.SetEmbeddingProvider(func(context.Context, string) (memql.EmbeddingAIProvider, error) { return p, nil })
	}
	old, had := memql.ActiveEmbedderBinding()
	memql.SetActiveEmbedderBinding(memql.EmbedderBinding{ProviderRef: f.provider, Dimensions: 1024})
	t.Cleanup(func() {
		if had {
			memql.SetActiveEmbedderBinding(old)
		} else {
			memql.ClearActiveEmbedderBinding()
		}
	})
	for n := 0; n < (len(seeddata.Release())+7)/8; n++ {
		go func() { errs <- f.a.ReconcileReleaseSeeds(ctx, true) }()
		go func() { errs <- f.b.ReconcileReleaseSeeds(ctx, true) }()
		require.NoError(t, <-errs)
		require.NoError(t, <-errs)
	}
	require.Len(t, p.calls, len(seeddata.Release()))
	for _, count := range p.calls {
		require.Equal(t, 1, count, "concurrent replicas must not duplicate embedding work")
	}
	for _, seed := range seeddata.Release() {
		present, e := f.b.hasVector(ctx, id.BuildNodeId(conceptKnowledgeDocumentChunk, seed.ID), f.provider, 1024)
		require.NoError(t, e)
		require.True(t, present)
	}
	require.NoError(t, f.a.ReconcileReleaseSeeds(ctx, false))
	var versions int
	require.NoError(t, f.db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE concept='v1:knowledge:documentChunk' AND id=$1`, id.BuildNodeId(conceptKnowledgeDocumentChunk, seeddata.Release()[0].ID)).Scan(&versions))
	require.Equal(t, 2, versions, "create plus reviewed publication only; upgrades/restarts do not append duplicate versions")
	// The actual peer similarity path excludes older release revisions before
	// top-K and refuses tampered current IDs even if their source fields change.
	retrieve := similarity.New(nil)
	retrieve.SetDBGetter(func() *sql.DB { return f.db })
	retrieve.SetStagedConceptPredicate(func(string) bool { return false })
	retrieve.SetEmbeddingProvider(func(context.Context, string) (memql.EmbeddingAIProvider, error) { return seedTestProvider{}, nil })
	search := func() int {
		for _, cap := range retrieve.Capabilities() {
			if cap.Name == "similarTo" {
				rows, e := cap.Handler(f.ctx, map[string]any{"text": "manual", "concept": conceptKnowledgeDocumentChunk, "domains": []string{"computer-use"}, "provider": f.provider, "limit": 100}, 0)
				require.NoError(t, e)
				return len(rows)
			}
		}
		t.Fatal("similarity capability absent")
		return 0
	}
	count := search()
	require.Positive(t, count)
	current := seeddata.Release()[0]
	prior := canonicalSeedChunk{ID: f.domain + "old", Domain: current.Domain, SourceRef: current.SourceRef, Text: "Previous revision", Seq: current.Seq}
	f.write(t, prior, "appStructure")
	f.decide(t, prior, "validated")
	t.Cleanup(func() {
		f.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id=$1`, id.BuildNodeId(conceptKnowledgeDocumentChunk, prior.ID))
	})
	vector, _ := seedTestProvider{}.Embed(ctx, "")
	require.NoError(t, f.a.storeVector(ctx, f.provider, prior.ID, conceptKnowledgeDocumentChunk, vector))
	require.Equal(t, count, search(), "older reviewed source does not displace the release revision")
	tampered := canonicalSeedChunk{current.ID, current.Domain, "unselected-source", current.Text, current.Seq}
	f.write(t, tampered, "fileUpload")
	f.decide(t, tampered, "validated")
	require.Equal(t, count-1, search(), "a known current ID cannot escape exact provenance by changing source labels")

}

func TestReleaseSeedsPreserveSourceDecisionsAndAcceptedReplacement(t *testing.T) {
	for _, decision := range []string{"rejected", "superseded"} {
		t.Run(decision, func(t *testing.T) {
			f, ctx := releaseFixture(t)
			var chosen seeddata.ChunkRecord
			for _, s := range seeddata.Release() {
				if s.SourceRef == "computerUse:fileReads" {
					chosen = s
					break
				}
			}
			require.NotEmpty(t, chosen.ID)
			previous := canonicalSeedChunk{Domain: chosen.Domain, SourceRef: chosen.SourceRef, Text: "Previous reviewed file-read manual.", Seq: chosen.Seq}
			previous.ID = chunkIdFor(previous.Domain, previous.SourceRef, previous.Seq, previous.Text)
			query := fmt.Sprintf(`mutation createDocumentChunk(chunkId:%s,domainId:%s,text:%s,source:"appStructure",sourceRef:%s,seq:%d)`, quoteString(previous.ID), quoteString(previous.Domain), quoteString(previous.Text), quoteString(previous.SourceRef), previous.Seq)
			_, err := f.ea.Execute(f.ctx, query)
			require.NoError(t, err)
			t.Cleanup(func() {
				f.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id=$1`, id.BuildNodeId(conceptKnowledgeDocumentChunk, previous.ID))
			})
			if decision == "rejected" {
				_, err = f.eb.Execute(f.ctx, fmt.Sprintf(`mutation setChunkValidationStatus(chunkId:%s,status:"rejected")`, quoteString(previous.ID)))
			} else {
				_, err = f.eb.Execute(f.ctx, fmt.Sprintf(`mutation markChunkSuperseded(chunkId:%s,supersededAt:%s)`, quoteString(previous.ID), quoteString(time.Now().UTC().Format(time.RFC3339Nano))))
			}
			require.NoError(t, err)
			require.NoError(t, f.a.ReconcileReleaseSeeds(ctx, false))
			row, err := f.b.seedChunkSnapshot(ctx, chosen.ID)
			require.NoError(t, err)
			require.NotEqual(t, "validated", row.Payload["validationStatus"], "release upgrade cannot resurrect a disabled source")
			require.NoError(t, f.b.ReconcileReleaseSeeds(ctx, false))
			row, _ = f.a.seedChunkSnapshot(ctx, chosen.ID)
			require.NotEqual(t, "validated", row.Payload["validationStatus"])
			// Existing public review is the explicit way to accept a replacement. The
			// legacy fileReads case superseded the old revision after accepting the new.
			_, err = f.eb.Execute(f.ctx, fmt.Sprintf(`mutation setChunkValidationStatus(chunkId:%s,status:"validated")`, quoteString(chosen.ID)))
			require.NoError(t, err)
			_, err = f.eb.Execute(f.ctx, fmt.Sprintf(`mutation markChunkSuperseded(chunkId:%s,supersededAt:%s,reason:"Reviewed replacement")`, quoteString(previous.ID), quoteString(time.Now().UTC().Format(time.RFC3339Nano))))
			require.NoError(t, err)
			require.NoError(t, f.a.ReconcileReleaseSeeds(ctx, false))
			row, _ = f.b.seedChunkSnapshot(ctx, chosen.ID)
			require.Equal(t, "validated", row.Payload["validationStatus"])
			_, err = f.eb.Execute(f.ctx, fmt.Sprintf(`mutation markChunkSuperseded(chunkId:%s,supersededAt:%s)`, quoteString(chosen.ID), quoteString(time.Now().UTC().Format(time.RFC3339Nano))))
			require.NoError(t, err)
			require.NoError(t, f.a.ReconcileReleaseSeeds(ctx, false))
			row, _ = f.b.seedChunkSnapshot(ctx, chosen.ID)
			require.Equal(t, true, row.Payload["superseded"])
		})
	}

}

func TestReleaseSeedsInstalledScheduledExecutorIndexesAndRefusesOwnedInvocation(t *testing.T) {
	f, ctx := releaseFixture(t)
	require.NoError(t, f.ea.RegisterIntegration(f.a))
	old, had := memql.ActiveEmbedderBinding()
	memql.SetActiveEmbedderBinding(memql.EmbedderBinding{ProviderRef: f.provider, Dimensions: 1024})
	t.Cleanup(func() {
		if had {
			memql.SetActiveEmbedderBinding(old)
		} else {
			memql.ClearActiveEmbedderBinding()
		}
	})
	p := &releaseEmbedding{calls: map[string]int{}}
	f.a.SetEmbeddingProvider(func(context.Context, string) (memql.EmbeddingAIProvider, error) { return p, nil })
	scheduled, err := workflowhost.Load("reconcileReleaseKnowledge")
	require.NoError(t, err)
	require.True(t, scheduled.Trusted)
	require.True(t, scheduled.IsScheduled())
	registry := steps.NewRegistry()
	registry.Register(automations.StepTypeFunction, &steps.FunctionExecutor{})
	executor := automations.NewExecutor(automations.ExecutorOptions{Engine: f.ea, StepRegistry: registry, Logger: f.ea.Logger})
	defer executor.Close()
	memql.ClearActiveEmbedderBinding()
	_, err = executor.Execute(context.Background(), scheduled, "schedule")
	require.Error(t, err, "missing indexing dependency is reported honestly")
	require.Empty(t, p.calls)
	for _, seed := range seeddata.Release() {
		row, readErr := f.b.seedChunkSnapshot(ctx, seed.ID)
		require.NoError(t, readErr)
		require.NotNil(t, row, "missing embedder must not prevent any publication")
		require.Equal(t, "validated", row.Payload["validationStatus"])
	}
	memql.SetActiveEmbedderBinding(memql.EmbedderBinding{ProviderRef: f.provider, Dimensions: 1024})
	_, err = executor.Execute(context.Background(), scheduled, "schedule")
	require.NoError(t, err)
	require.Len(t, p.calls, 8, "the real ownerless schedule reaches the bounded indexer")
	for _, ownerCtx := range []context.Context{f.ctx, common.ContextWithRun(ctx, common.RunContext{RunId: "owned", GoalId: "goal", OwnerUserId: "person", Mode: common.RunModeLive})} {
		_, err = executor.Execute(ownerCtx, scheduled, "manual")
		require.ErrorContains(t, err, "server lifecycle")
	}
}
