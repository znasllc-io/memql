package knowledge

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/similarity"
)

type seedTestProvider struct{}

func (seedTestProvider) Embed(context.Context, string) ([]float32, error) {
	v := make([]float32, 1024)
	v[0] = 1
	return v, nil
}
func (p seedTestProvider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	var v [][]float32
	for _, text := range texts {
		one, _ := p.Embed(ctx, text)
		v = append(v, one)
	}
	return v, nil
}
func (seedTestProvider) Dimensions() int { return 1024 }

type seedFixture struct {
	a, b             *Integration
	ea, eb           *memql.MemQLEngine
	db               *sql.DB
	ctx              context.Context
	domain, provider string
	seeds            []canonicalSeedChunk
}

func newSeedFixture(t *testing.T) *seedFixture {
	t.Helper()
	return seedFixtureForCase(t, newSeedReplicas(t))
}

func newSeedReplicas(t *testing.T) *seedFixture {
	t.Helper()
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "canonical seed publication", dbtest.DSN(), err)
		return nil
	}
	_, err := memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	f := &seedFixture{db: db}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	open := func() (*Integration, *memql.MemQLEngine) {
		pool := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
		t.Cleanup(func() { _ = pool.Close() })
		e, err := memql.New(pool)
		require.NoError(t, err)
		e.Logger = logger
		require.NoError(t, e.Init(memorynodes.DefaultRegistry()))
		i := New(logger)
		i.SetEngine(e)
		i.SetDBGetter(func() *sql.DB { return db })
		i.SetStagedConceptPredicate(func(string) bool { return false })
		i.SetEmbeddingProvider(func(context.Context, string) (memql.EmbeddingAIProvider, error) { return seedTestProvider{}, nil })
		return i, e
	}
	f.a, f.ea = open()
	f.b, f.eb = open()
	return f
}

func seedFixtureForCase(t *testing.T, shared *seedFixture) *seedFixture {
	t.Helper()
	// A parent owns only the fixed replicas and pools. Every sequential case
	// registers a new corpus and uses new caller and vector/cache identities.
	prefix := fmt.Sprintf("seed-review-%d", time.Now().UnixNano())
	f := *shared
	f.domain, f.provider = prefix, prefix
	f.ctx = auth.ContextWithUserActor(context.Background(), prefix)
	f.seeds = nil
	for _, i := range []*Integration{f.a, f.b} {
		engine, dbGetter, gateDB := i.engine, i.dbGetter, i.gateDB
		embeddingProvider, stagedConcept := i.embeddingProvider, i.stagedConcept
		t.Cleanup(func() {
			i.SetEngine(engine)
			i.SetDBGetter(dbGetter)
			i.SetGateDB(gateDB)
			i.SetEmbeddingProvider(embeddingProvider)
			i.SetStagedConceptPredicate(stagedConcept)
		})
	}
	entries := []SeedCorpusEntry{}
	for n := 0; n < 10; n++ {
		entries = append(entries, SeedCorpusEntry{SourceRef: fmt.Sprintf("manual-%d", n), Text: fmt.Sprintf("Registered general capability guidance %d.", n)})
	}
	// A long registered entry proves identity is based on actual chunked bytes,
	// not the whole entry's hash (which is different for split corpora).
	entries = append(entries, SeedCorpusEntry{SourceRef: "long-manual", Text: strings.Repeat("Long registered capability guidance. ", 100)})
	RegisterSeedDomain(SeedDomainRegistration{Domain: StandardDomain{ID: prefix}, Corpus: entries})
	for _, entry := range entries {
		for seq, text := range Chunk(entry.Text, defaultChunkSize, defaultOverlap) {
			key := chunkIdFor(prefix, entry.SourceRef, seq, text)
			f.seeds = append(f.seeds, canonicalSeedChunk{key, prefix, entry.SourceRef, text, seq})
		}
	}
	t.Cleanup(func() {
		_, _ = f.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept='v1:knowledge:documentChunk' AND payload->>'domainId'=$1`, prefix)
		_, _ = f.db.ExecContext(context.Background(), `DELETE FROM embedding_cache WHERE provider=$1`, prefix)
		table, _ := memql.EmbeddingVectorTable(prefix, 1024)
		_, _ = f.db.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+table)
		seedDomainMu.Lock()
		defer seedDomainMu.Unlock()
		delete(seedDomainIDs, prefix)
		for n, reg := range seedDomainRegs {
			if reg.Domain.ID == prefix {
				seedDomainRegs = append(seedDomainRegs[:n], seedDomainRegs[n+1:]...)
				break
			}
		}
	})
	return &f
}
func (f *seedFixture) write(t *testing.T, seed canonicalSeedChunk, source string) {
	t.Helper()
	_, err := f.ea.Execute(f.ctx, fmt.Sprintf(`mutation createDocumentChunk(chunkId:%s,domainId:%s,text:%s,source:%s,sourceRef:%s,seq:%d)`, quoteString(seed.ID), quoteString(seed.Domain), quoteString(seed.Text), quoteString(source), quoteString(seed.SourceRef), seed.Seq))
	require.NoError(t, err)
}
func (f *seedFixture) status(t *testing.T, seed canonicalSeedChunk) string {
	t.Helper()
	row, err := f.b.seedChunkSnapshot(f.ctx, seed.ID)
	require.NoError(t, err)
	require.NotNil(t, row)
	status, _ := row.Payload["validationStatus"].(string)
	return status
}
func (f *seedFixture) decide(t *testing.T, seed canonicalSeedChunk, status string) {
	t.Helper()
	_, err := f.eb.Execute(f.ctx, fmt.Sprintf(`mutation setChunkValidationStatus(chunkId:%s,status:%s)`, quoteString(seed.ID), quoteString(status)))
	require.NoError(t, err)
}

func TestCanonicalKnowledgeDB(t *testing.T) {
	f := newSeedReplicas(t)
	t.Run("CanonicalSeedRepairPreservesReviewAndOnlyPublishesExactRegisteredContent", func(t *testing.T) { testCanonicalSeedRepairPreservesReviewAndOnlyPublishesExactRegisteredContent(t, f) })
	t.Run("CanonicalSeedRepairCannotOverwriteAnotherReplicasDecision", func(t *testing.T) { testCanonicalSeedRepairCannotOverwriteAnotherReplicasDecision(t, f) })
	t.Run("CanonicalIngestPublishesOnlyServerSelectedCorpusAndPreservesRejection", func(t *testing.T) { testCanonicalIngestPublishesOnlyServerSelectedCorpusAndPreservesRejection(t, f) })
	t.Run("TrainerRefreshUsesOnlyFreshPublishedKnowledge", func(t *testing.T) { testTrainerRefreshUsesOnlyFreshPublishedKnowledge(t, f) })
}

func testCanonicalSeedRepairPreservesReviewAndOnlyPublishesExactRegisteredContent(t *testing.T, shared *seedFixture) {
	f := seedFixtureForCase(t, shared)
	for _, seed := range f.seeds[:8] {
		f.write(t, seed, "appStructure")
	}
	f.decide(t, f.seeds[1], "rejected")
	f.decide(t, f.seeds[2], "validated")
	_, err := f.eb.Execute(f.ctx, fmt.Sprintf(`mutation markChunkSuperseded(chunkId:%s,supersededAt:"2026-10-12T00:00:00Z")`, quoteString(f.seeds[3].ID)))
	require.NoError(t, err)
	// An unknown source with the trusted-looking label is not in the registry.
	unknown := canonicalSeedChunk{ID: f.domain + "-unknown", Domain: f.domain, SourceRef: "manual", Text: "Caller content", Seq: 0}
	f.write(t, unknown, "appStructure")
	// Same ID but altered bytes, and exact registered bytes from a user upload,
	// are both refused. The immutable registration, not the label, is authority.
	forged := f.seeds[8]
	forged.Text = "Caller altered text"
	f.write(t, forged, "appStructure")
	f.write(t, f.seeds[9], "fileUpload")
	for _, seed := range f.seeds[10:] {
		f.write(t, seed, "appStructure")
	}
	table, err := memql.EnsureEmbeddingVectorTable(f.ctx, f.db, f.provider, 1024)
	require.NoError(t, err)
	vector, _ := seedTestProvider{}.Embed(f.ctx, "")
	_, err = f.db.ExecContext(f.ctx, `INSERT INTO `+table+`(id,concept,vector_field,embedding) VALUES($1,'v1:knowledge:documentChunk','content',$2::vector)`, f.seeds[0].ID, vectorLiteral(vector))
	require.NoError(t, err)
	require.ErrorContains(t, f.a.ReconcileCanonicalSeeds(f.ctx), "requires internal origin")
	require.Empty(t, f.status(t, f.seeds[0]), "an inbound context must not publish a seed")
	require.ErrorContains(t, f.a.ReconcileCanonicalSeeds(auth.ContextWithInternalOrigin(f.ctx)), "does not match registered content")
	require.Equal(t, "validated", f.status(t, f.seeds[0]))
	require.Equal(t, "rejected", f.status(t, f.seeds[1]))
	require.Equal(t, "validated", f.status(t, f.seeds[2]))
	require.Empty(t, f.status(t, f.seeds[3]))
	require.Empty(t, f.status(t, unknown))
	require.Empty(t, f.status(t, forged))
	require.Empty(t, f.status(t, f.seeds[9]))
	for _, seed := range f.seeds[10:] {
		require.Equal(t, "validated", f.status(t, seed))
	}
	// Another replica consumes the repaired legacy vector through the actual
	// generic similarity capability, without any model call or local seed cache.
	retrieve := similarity.New(nil)
	retrieve.SetDBGetter(func() *sql.DB { return f.db })
	retrieve.SetStagedConceptPredicate(func(string) bool { return false })
	retrieve.SetEmbeddingProvider(func(context.Context, string) (memql.EmbeddingAIProvider, error) { return seedTestProvider{}, nil })
	for _, cap := range retrieve.Capabilities() {
		if cap.Name == "similarTo" {
			rows, err := cap.Handler(f.ctx, map[string]any{"text": "capability guidance", "concept": conceptKnowledgeDocumentChunk, "domains": []string{f.domain}, "provider": f.provider}, 0)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Equal(t, conceptKnowledgeDocumentChunk+":"+f.seeds[0].ID, rows[0].ID)
		}
	}
	before, err := f.b.seedChunkSnapshot(f.ctx, f.seeds[0].ID)
	require.NoError(t, err)
	require.Error(t, f.b.ReconcileCanonicalSeeds(auth.ContextWithInternalOrigin(f.ctx))) // forged entries still fail closed
	after, err := f.b.seedChunkSnapshot(f.ctx, f.seeds[0].ID)
	require.NoError(t, err)
	require.Equal(t, before.At, after.At, "a repeated repair must not add versions")
}

type beforeSeedWrite struct {
	memql.IntegrationEngineAccess
	before func()
}

func (e beforeSeedWrite) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.Contains(q, "setChunkValidationStatus") {
		e.before()
	}
	return e.IntegrationEngineAccess.Execute(ctx, q)
}
func testCanonicalSeedRepairCannotOverwriteAnotherReplicasDecision(t *testing.T, shared *seedFixture) {
	f := seedFixtureForCase(t, shared)
	seed := f.seeds[0]
	f.write(t, seed, "appStructure")
	// Deterministically put rejection after the repair read but before commit.
	f.a.SetEngine(beforeSeedWrite{f.ea, func() { f.decide(t, seed, "rejected") }})
	_, err := f.a.reconcileCanonicalSeed(f.ctx, seed)
	require.ErrorIs(t, err, memql.ErrRowVersionChanged)
	require.Equal(t, "rejected", f.status(t, seed))
	f.a.SetEngine(f.ea)
	// Two replicas repairing the same pending record converge; the stale
	// transaction refuses and can safely re-read on the next bounded pass.
	seed = f.seeds[1]
	f.write(t, seed, "appStructure")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, i := range []*Integration{f.a, f.b} {
		wg.Add(1)
		go func(i *Integration) { defer wg.Done(); _, err := i.reconcileCanonicalSeed(f.ctx, seed); errs <- err }(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			require.ErrorIs(t, err, memql.ErrRowVersionChanged)
		}
	}
	_, err = f.b.reconcileCanonicalSeed(f.ctx, seed)
	require.NoError(t, err)
	require.Equal(t, "validated", f.status(t, seed))
}

func testCanonicalIngestPublishesOnlyServerSelectedCorpusAndPreservesRejection(t *testing.T, shared *seedFixture) {
	f := seedFixtureForCase(t, shared)
	seed := f.seeds[0]
	args := map[string]any{"domainId": seed.Domain, "text": seed.Text, "sourceRef": seed.SourceRef, "source": "appStructure", "provider": f.provider}
	newSeed := f.seeds[1]
	_, err := f.a.ingest(f.ctx, map[string]any{"domainId": newSeed.Domain, "text": newSeed.Text, "sourceRef": newSeed.SourceRef, "source": "appStructure", "provider": f.provider}, true)
	require.NoError(t, err)
	require.Equal(t, "validated", f.status(t, newSeed), "new canonical content is published by the trusted writer")
	_, err = f.a.ingestHandler(f.ctx, args, 0)
	require.NoError(t, err)
	require.Empty(t, f.status(t, seed), "a caller's label is not approval")
	_, err = f.a.ingest(f.ctx, args, true)
	require.NoError(t, err)
	require.Equal(t, "validated", f.status(t, seed))
	f.decide(t, seed, "rejected")
	_, err = f.b.ingest(f.ctx, args, true)
	require.NoError(t, err)
	require.Equal(t, "rejected", f.status(t, seed))
	has, err := f.a.hasVector(f.ctx, conceptKnowledgeDocumentChunk+":"+seed.ID, f.provider, 1024)
	require.NoError(t, err)
	require.True(t, has, "ingestion must index the same canonical ID retrieval joins")
}

func testTrainerRefreshUsesOnlyFreshPublishedKnowledge(t *testing.T, shared *seedFixture) {
	f := seedFixtureForCase(t, shared)
	for _, seed := range f.seeds[:4] {
		f.write(t, seed, "fileUpload")
	}
	f.decide(t, f.seeds[1], "rejected")
	f.decide(t, f.seeds[2], "validated")
	f.decide(t, f.seeds[3], "validated")
	_, err := f.eb.Execute(f.ctx, fmt.Sprintf(`mutation markChunkSuperseded(chunkId:%s,supersededAt:"2026-10-12T00:00:00Z")`, quoteString(f.seeds[3].ID)))
	require.NoError(t, err)
	rows := f.a.loadExistingCorpus(f.ctx, f.domain)
	require.Len(t, rows, 1)
	require.Equal(t, f.seeds[2].Text, rows[0]["text"])
	// Warm the first engine's read then reject on the second engine. Neither
	// the review queue's broader read nor its cached projection may leak here.
	f.decide(t, f.seeds[2], "rejected")
	require.Empty(t, f.a.loadExistingCorpus(f.ctx, f.domain))
	raw, err := f.ea.Execute(memql.ContextWithFreshRead(f.ctx), fmt.Sprintf(`query documentChunksForDomain(domainId:%s)`, quoteString(f.domain)))
	require.NoError(t, err)
	require.Len(t, memql.MaterializeRows(raw), 4, "Training must still be able to review pending/rejected history")
}
