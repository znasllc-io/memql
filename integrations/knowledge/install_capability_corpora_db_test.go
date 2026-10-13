package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/similarity"
)

type installTestEmbedder struct {
	seedTestProvider
	calls     int
	fail      bool
	badVector bool
}

func (p *installTestEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	p.calls++
	if p.fail {
		return nil, errors.New("embedding unavailable")
	}
	if p.badVector {
		return []float32{1, 0, 0}, nil
	}
	return p.seedTestProvider.Embed(ctx, text)
}
func capabilityInstallFixture(t *testing.T) (*seedFixture, *installTestEmbedder) {
	t.Helper()
	f := newSeedFixture(t)
	f.ctx = auth.ContextWithAccess(f.ctx, &auth.AccessContext{UserId: f.domain, Role: auth.RoleOwner})
	seedDomainMu.Lock()
	for n := range seedDomainRegs {
		if seedDomainRegs[n].Domain.ID == f.domain {
			seedDomainRegs[n].Domain.Source = "appStructure"
			seedDomainRegs[n].Domain.RequiredByToolSlugs = []string{"syntheticCapability"}
		}
	}
	seedDomainMu.Unlock()
	p := &installTestEmbedder{}
	resolve := func(context.Context, string) (memql.EmbeddingAIProvider, error) { return p, nil }
	f.a.SetEmbeddingProvider(resolve)
	f.b.SetEmbeddingProvider(resolve)
	memql.SetActiveEmbedderBinding(memql.EmbedderBinding{ProviderRef: f.provider, Dimensions: 1024})
	t.Cleanup(memql.ClearActiveEmbedderBinding)
	require.NoError(t, f.ea.RegisterIntegration(f.a))
	require.NoError(t, f.eb.RegisterIntegration(f.b))
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

func TestCapabilityInstallationAcrossReplicasPublishesOnlySelectedAndRepairsVectors(t *testing.T) {
	f, p := capabilityInstallFixture(t)
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

func TestCapabilityInstallationPreservesReviewAndReportsOlderPublishedRevisions(t *testing.T) {
	f, p := capabilityInstallFixture(t)
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

func TestCapabilityInstallationFailsStrictlyAndPreservesPeerReview(t *testing.T) {
	f, p := capabilityInstallFixture(t)
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

func TestCapabilityInstallationRetryRepairsPartialPublicationOnAnotherReplica(t *testing.T) {
	f, p := capabilityInstallFixture(t)
	p.badVector = true
	_, err := f.a.installCapabilityCorporaHandler(f.ctx, map[string]any{"domainIds": []string{f.domain}}, 0)
	require.Error(t, err)
	require.Equal(t, 1, p.calls)
	var published int
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT count(*) FROM (SELECT DISTINCT ON (id) payload FROM "MemoryNodes" WHERE concept='v1:knowledge:documentChunk' ORDER BY id,"createdAt" DESC) latest WHERE payload->>'domainId'=$1 AND payload->>'validationStatus'='validated'`, f.domain).Scan(&published))
	require.Equal(t, 1, published, "vector failure can leave one exact registered publication, not an acknowledged complete install")
	p.badVector = false
	r := installFixtureCorpus(t, f, f.b)
	require.Equal(t, len(f.seeds), r.Ready)
	require.Equal(t, len(f.seeds), r.Embedded)
	require.Equal(t, len(f.seeds)+1, p.calls)
}
