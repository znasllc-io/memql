package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/similarity"
)

type installTestEmbedder struct {
	seedTestProvider
	calls              int
	fail               bool
	declaredDimensions int
	vector             []float32
}

func (p *installTestEmbedder) Dimensions() int { return p.declaredDimensions }

func (p *installTestEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	p.calls++
	if p.fail {
		return nil, errors.New("embedding unavailable")
	}
	if p.vector != nil {
		return p.vector, nil
	}
	return p.seedTestProvider.Embed(ctx, text)
}
func capabilityInstallFixture(t *testing.T, shared *seedFixture) (*seedFixture, *installTestEmbedder) {
	t.Helper()
	f := seedFixtureForCase(t, shared)
	f.ctx = auth.ContextWithAccess(f.ctx, &auth.AccessContext{UserId: f.domain, Role: auth.RoleOwner})
	seedDomainMu.Lock()
	for n := range seedDomainRegs {
		if seedDomainRegs[n].Domain.ID == f.domain {
			seedDomainRegs[n].Domain.Source = "appStructure"
			seedDomainRegs[n].Domain.RequiredByToolSlugs = []string{"syntheticCapability"}
		}
	}
	seedDomainMu.Unlock()
	p := &installTestEmbedder{declaredDimensions: 1024}
	resolve := func(context.Context, string) (memql.EmbeddingAIProvider, error) { return p, nil }
	f.a.SetEmbeddingProvider(resolve)
	f.b.SetEmbeddingProvider(resolve)
	old, had := memql.ActiveEmbedderBinding()
	memql.SetActiveEmbedderBinding(memql.EmbedderBinding{ProviderRef: f.provider, Dimensions: 1024})
	t.Cleanup(func() {
		if had {
			memql.SetActiveEmbedderBinding(old)
		} else {
			memql.ClearActiveEmbedderBinding()
		}
	})
	return f, p
}
func installFixtureCorpus(t *testing.T, f *seedFixture, i *Integration) capabilityInstallReceipt {
	t.Helper()
	nodes, err := i.installCapabilityCorporaHandler(f.ctx, map[string]any{"domainIds": []string{f.domain}}, 0)
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	var result struct {
		Domains []capabilityInstallReceipt `json:"domains"`
	}
	require.NoError(t, json.Unmarshal(nodes[0].Payload, &result))
	require.Len(t, result.Domains, 1)
	return result.Domains[0]
}

func TestCapabilityInstallationDB(t *testing.T) {
	f := newSeedReplicas(t)
	require.NoError(t, f.ea.RegisterIntegration(f.a))
	require.NoError(t, f.eb.RegisterIntegration(f.b))
	t.Run("AcrossReplicasPublishesOnlySelectedAndRepairsVectors", func(t *testing.T) {
		testCapabilityInstallationAcrossReplicasPublishesOnlySelectedAndRepairsVectors(t, f)
	})
	t.Run("PreservesReviewAndReportsOlderPublishedRevisions", func(t *testing.T) { testCapabilityInstallationPreservesReviewAndReportsOlderPublishedRevisions(t, f) })
	t.Run("FailsStrictlyAndPreservesPeerReview", func(t *testing.T) { testCapabilityInstallationFailsStrictlyAndPreservesPeerReview(t, f) })
	t.Run("RetryRepairsPartialPublicationOnAnotherReplica", func(t *testing.T) { testCapabilityInstallationRetryRepairsPartialPublicationOnAnotherReplica(t, f) })
	t.Run("UsesBoundWidthForUnknownProviderAcrossReplicas", func(t *testing.T) { testCapabilityInstallationUsesBoundWidthForUnknownProviderAcrossReplicas(t, f) })
	t.Run("RejectsInvalidVectorsBeforePublication", func(t *testing.T) { testCapabilityInstallationRejectsInvalidVectorsBeforePublication(t, f) })
}

func testCapabilityInstallationAcrossReplicasPublishesOnlySelectedAndRepairsVectors(t *testing.T, shared *seedFixture) {
	f, p := capabilityInstallFixture(t, shared)
	// Entry through the installed public builtin, not a test-only ingestion path.
	_, err := f.ea.Execute(f.ctx, fmt.Sprintf(`builtin knowledgeInstallCapabilityCorpora(domainIds:[%s])`, quoteString(f.domain)))
	require.NoError(t, err)
	require.Equal(t, len(f.seeds), p.calls)
	before, err := f.a.seedChunkSnapshot(f.ctx, f.seeds[0].ID)
	require.NoError(t, err)
	r := installFixtureCorpus(t, f, f.b)
	require.Equal(t, len(f.seeds), r.Ready)
	require.Zero(t, r.Embedded)
	require.Zero(t, r.Preserved)
	require.Equal(t, len(f.seeds), p.calls)
	after, err := f.b.seedChunkSnapshot(f.ctx, f.seeds[0].ID)
	require.NoError(t, err)
	require.Equal(t, before.At, after.At)
	var domains int
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT count(*) FROM "MemoryNodes" WHERE concept='v1:knowledge:knowledgeDomain' AND (id=$1 OR payload->>'id'=$1)`, id.BuildNodeId("v1:knowledge:knowledgeDomain", f.domain)).Scan(&domains))
	require.Zero(t, domains)
	// A missing vector is repaired even though its published chunk already exists.
	table, err := memql.EmbeddingVectorTable(f.provider, 1024)
	require.NoError(t, err)
	_, err = f.db.ExecContext(f.ctx, `DELETE FROM `+table+` WHERE id=$1`, id.BuildNodeId(conceptKnowledgeDocumentChunk, f.seeds[0].ID))
	require.NoError(t, err)
	r = installFixtureCorpus(t, f, f.b)
	require.Equal(t, 1, r.Embedded)
	require.Equal(t, len(f.seeds), r.Ready)
	// Another node's real retrieval sees the installed text. No domain catalog row
	// is necessary for a tool's already-declared required domain to be searched.
	retrieve := similarity.New(nil)
	retrieve.SetDBGetter(func() *sql.DB { return f.db })
	retrieve.SetStagedConceptPredicate(func(string) bool { return false })
	retrieve.SetEmbeddingProvider(func(context.Context, string) (memql.EmbeddingAIProvider, error) { return seedTestProvider{}, nil })
	for _, cap := range retrieve.Capabilities() {
		if cap.Name == "similarTo" {
			rows, err := cap.Handler(f.ctx, map[string]any{"text": "capability guidance", "concept": conceptKnowledgeDocumentChunk, "domains": []string{f.domain}, "provider": f.provider}, 0)
			require.NoError(t, err)
			require.NotEmpty(t, rows)
		}
	}
}

func testCapabilityInstallationPreservesReviewAndReportsOlderPublishedRevisions(t *testing.T, shared *seedFixture) {
	f, p := capabilityInstallFixture(t, shared)
	f.write(t, f.seeds[0], "appStructure")
	f.decide(t, f.seeds[0], "rejected")
	f.write(t, f.seeds[1], "appStructure")
	_, err := f.eb.Execute(f.ctx, fmt.Sprintf(`mutation markChunkSuperseded(chunkId:%s,supersededAt:"2026-10-13T00:00:00Z")`, quoteString(f.seeds[1].ID)))
	require.NoError(t, err)
	old := f.seeds[2]
	old.ID = f.domain + "-old"
	old.Text = "Older approved manual remains a review decision."
	f.write(t, old, "appStructure")
	f.decide(t, old, "validated")
	pending := old
	pending.ID = f.domain + "-pending"
	pending.Text = "Caller prose with same source reference."
	f.write(t, pending, "appStructure")
	r := installFixtureCorpus(t, f, f.a)
	require.Equal(t, 2, r.Preserved)
	require.Equal(t, len(f.seeds)-2, r.Ready)
	require.Equal(t, 1, r.OlderPublished)
	require.Equal(t, len(f.seeds)-2, p.calls)
	require.Equal(t, "rejected", f.status(t, f.seeds[0]))
	require.Empty(t, f.status(t, f.seeds[1]))
	require.Equal(t, "validated", f.status(t, old))
	require.Empty(t, f.status(t, pending))
	r = installFixtureCorpus(t, f, f.b)
	require.Zero(t, r.Embedded)
	require.Equal(t, 1, r.OlderPublished)
}

func testCapabilityInstallationFailsStrictlyAndPreservesPeerReview(t *testing.T, shared *seedFixture) {
	f, p := capabilityInstallFixture(t, shared)
	p.fail = true
	_, err := f.a.installCapabilityCorporaHandler(f.ctx, map[string]any{"domainIds": []string{f.domain}}, 0)
	require.ErrorContains(t, err, "embedding unavailable")
	require.Equal(t, 1, p.calls)
	var written int
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT count(*) FROM "MemoryNodes" WHERE concept='v1:knowledge:documentChunk' AND payload->>'domainId'=$1`, f.domain).Scan(&written))
	require.Zero(t, written)
	p.fail = false
	seed := f.seeds[0]
	f.write(t, seed, "appStructure")
	f.a.SetEngine(beforeSeedWrite{IntegrationEngineAccess: f.ea, before: func() { f.decide(t, seed, "rejected") }})
	binding := memql.EmbedderBinding{ProviderRef: f.provider, Dimensions: 1024}
	_, err = f.a.installCapabilityChunk(f.ctx, seed, binding, p)
	require.ErrorIs(t, err, memql.ErrRowVersionChanged)
	require.Equal(t, "rejected", f.status(t, seed))
	r := installFixtureCorpus(t, f, f.b)
	require.Equal(t, 1, r.Preserved)
	require.Equal(t, len(f.seeds)-1, r.Ready)
}

func testCapabilityInstallationRetryRepairsPartialPublicationOnAnotherReplica(t *testing.T, shared *seedFixture) {
	f, p := capabilityInstallFixture(t, shared)
	table, err := memql.EnsureEmbeddingVectorTable(f.ctx, f.db, f.provider, 1024)
	require.NoError(t, err)
	_, err = f.db.ExecContext(f.ctx, `ALTER TABLE `+table+` ADD CONSTRAINT fixture_refuse_vector CHECK (false)`)
	require.NoError(t, err)
	_, err = f.a.installCapabilityCorporaHandler(f.ctx, map[string]any{"domainIds": []string{f.domain}}, 0)
	require.ErrorContains(t, err, "fixture_refuse_vector")
	require.Equal(t, 1, p.calls)
	var published int
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT count(*) FROM (SELECT DISTINCT ON (id) payload FROM "MemoryNodes" WHERE concept='v1:knowledge:documentChunk' ORDER BY id,"createdAt" DESC) latest WHERE payload->>'domainId'=$1 AND payload->>'validationStatus'='validated'`, f.domain).Scan(&published))
	require.Equal(t, 1, published, "vector failure can leave one exact registered publication, not an acknowledged complete install")
	_, err = f.db.ExecContext(f.ctx, `ALTER TABLE `+table+` DROP CONSTRAINT fixture_refuse_vector`)
	require.NoError(t, err)
	r := installFixtureCorpus(t, f, f.b)
	require.Equal(t, len(f.seeds), r.Ready)
	require.Equal(t, len(f.seeds), r.Embedded)
	require.Equal(t, len(f.seeds)+1, p.calls)
}

func testCapabilityInstallationUsesBoundWidthForUnknownProviderAcrossReplicas(t *testing.T, shared *seedFixture) {
	f, p := capabilityInstallFixture(t, shared)
	p.declaredDimensions = 0 // Fleet models outside the catalogue report unknown.
	_, err := f.ea.Execute(f.ctx, fmt.Sprintf(`builtin knowledgeInstallCapabilityCorpora(domainIds:[%s])`, quoteString(f.domain)))
	require.NoError(t, err)
	require.Equal(t, len(f.seeds), p.calls)
	r := installFixtureCorpus(t, f, f.b)
	require.Equal(t, len(f.seeds), r.Ready)
	require.Zero(t, r.Embedded)
	require.Equal(t, len(f.seeds), p.calls, "peer retry reuses vectors in the bound model space")
}

func testCapabilityInstallationRejectsInvalidVectorsBeforePublication(t *testing.T, shared *seedFixture) {
	f, p := capabilityInstallFixture(t, shared)
	valid := make([]float32, 1024)
	valid[0] = 1
	nan := append([]float32(nil), valid...)
	nan[0] = float32(math.NaN())
	infinite := append([]float32(nil), valid...)
	infinite[0] = float32(math.Inf(1))
	for _, tc := range []struct {
		name     string
		declared int
		vector   []float32
		want     string
		calls    int
	}{
		{"explicit width conflicts", 512, valid, "do not match", 0},
		{"declared width wrong response", 1024, []float32{1, 0, 0}, "expected 1024", 1},
		{"unknown width wrong response", 0, []float32{1, 0, 0}, "expected 1024", 1},
		{"empty response", 0, []float32{}, "expected 1024", 1},
		{"zero response", 0, make([]float32, 1024), "zero vector", 1},
		{"non-finite response", 0, nan, "non-finite", 1},
		{"infinite response", 0, infinite, "non-finite", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.declaredDimensions, p.vector, p.calls = tc.declared, tc.vector, 0
			_, err := f.ea.Execute(f.ctx, fmt.Sprintf(`builtin knowledgeInstallCapabilityCorpora(domainIds:[%s])`, quoteString(f.domain)))
			require.ErrorContains(t, err, tc.want)
			require.Equal(t, tc.calls, p.calls)
			var written int
			require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT count(*) FROM "MemoryNodes" WHERE concept='v1:knowledge:documentChunk' AND payload->>'domainId'=$1`, f.domain).Scan(&written))
			require.Zero(t, written, "invalid embedding cannot publish even an exact registered chunk")
		})
	}
}
