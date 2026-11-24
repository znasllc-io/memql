package memql

// embedder_autobind_test.go -- the automatic first memory-search model is an
// OWNER'S MACHINE model (design record 2026-11-23, sections 1 and 2): never a
// vendor model by itself, never one only a non-owner's machine offers, and a
// machine lent to an owner counts as that owner's. Also who indexes and who
// chose, and the resolver's re-check that the indexer is still an owner.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/airoute"
)

// ownerCatalogs answers FleetCatalogReader per person, as the installed reader
// does: a person's own machines and the ones lent to them, in one list.
type ownerCatalogs map[string][]FleetModel

func (c ownerCatalogs) Catalog(_ context.Context, person string) ([]FleetModel, error) {
	return c[person], nil
}

func embedderModel(id string, online bool) FleetModel {
	return FleetModel{ModelId: id, Embeddings: true, Machines: []FleetMachine{{RegistrationId: "m-" + id, Online: online}}}
}

// vendorProbe is a vendor embedder the registry could serve.
type vendorProbe struct{ *activationProbe }

func (vendorProbe) Call(context.Context, string) (any, error) { return nil, nil }

type autobindFixture struct {
	eng      *MemQLEngine
	db       *bun.DB
	ctx      context.Context
	probe    *activationProbe
	catalogs ownerCatalogs
	owner    string
	writer   string
}

// newAutobindFixture boots a private engine with no binding, an owner and a
// writer in v1:identity:user, and a resolver that records every embedder it is
// asked for. Like the activation test it runs only against dbtest's disposable
// database and restores any binding it found.
func newAutobindFixture(t *testing.T) *autobindFixture {
	t.Helper()
	eng, db, ctx := readMergeTestEngine(t)
	var saved string
	require.NoError(t, db.DB.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(n)),'[]')::text FROM "MemoryNodes" n WHERE concept=$1`, EmbedderBindingConceptID).Scan(&saved))
	_, err := db.DB.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept=$1`, EmbedderBindingConceptID)
	require.NoError(t, err)
	sfx := uniqueSuffix("autobind")
	f := &autobindFixture{eng: eng, db: db, ctx: ctx, catalogs: ownerCatalogs{},
		owner: "v1:identity:user:owner-" + sfx, writer: "v1:identity:user:writer-" + sfx,
		probe: &activationProbe{vector: make([]float32, 1024)}}
	f.probe.vector[0] = 1
	marker := "autobind:" + sfx
	for id, role := range map[string]string{f.owner: "owner", f.writer: "writer"} {
		payload, _ := json.Marshal(map[string]any{"id": id, "displayName": id, "primaryEmail": id + "@example.invalid", "role": role, "active": true})
		rows := []*memorynodes.MemoryNode{{ID: id, Concept: "v1:identity:user", CreatedBy: marker, CreatedAt: time.Now().UTC(), Payload: payload,
			Provenance: json.RawMessage(`{"kind":"direct","name":"autobind-test"}`)}}
		_, err := db.NewInsert().Model(&rows).Exec(ctx)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = db.DB.ExecContext(bg, `DELETE FROM "MemoryNodes" WHERE concept=$1`, EmbedderBindingConceptID)
		_, _ = db.DB.ExecContext(bg, `INSERT INTO "MemoryNodes" SELECT * FROM jsonb_populate_recordset(NULL::"MemoryNodes",$1::jsonb)`, saved)
		_, _ = db.DB.ExecContext(bg, `DELETE FROM "MemoryNodes" WHERE concept='v1:identity:user' AND "createdBy"=$1`, marker)
	})
	eng.providers = NewProviderRegistryForTest()
	eng.providers.SetFleetCatalog(f.catalogs)
	eng.SetAIResolver(testAIResolver{fn: func(_ context.Context, req airoute.ResolveRequest) (ResolvedProvider, error) {
		return ResolvedProvider{Client: f.probe}, nil
	}})
	return f
}

func (f *autobindFixture) binding(t *testing.T) (EmbedderBinding, error) {
	t.Helper()
	return f.eng.ReadEmbedderBinding(f.ctx)
}

// A vendor embedder reachable here must not bind by itself: it spends the
// cluster's account on a model nobody chose.
func TestTheAutomaticChoiceNeverBindsAVendorModel(t *testing.T) {
	f := newAutobindFixture(t)
	f.eng.providers.RegisterForTest("vendorEmbed", "openai", "text-embedding-3-small", vendorProbe{f.probe})
	f.eng.autoBindEmbedder(f.ctx)
	_, err := f.binding(t)
	require.ErrorIs(t, err, ErrNoEmbedderBound)
	require.Zero(t, f.probe.calls, "nothing may even be probed for a vendor model")
}

func TestTheAutomaticChoiceTakesOnlyAnOwnersMachineModel(t *testing.T) {
	f := newAutobindFixture(t)
	model := "embed-" + uniqueSuffix("w")
	f.catalogs[f.writer] = []FleetModel{embedderModel(model, true)}
	f.eng.autoBindEmbedder(f.ctx)
	_, err := f.binding(t)
	require.ErrorIs(t, err, ErrNoEmbedderBound, "a writer's machine is not memory search's to use")

	// An owner's OFFLINE machine is not a candidate either.
	f.catalogs[f.owner] = []FleetModel{embedderModel(model, false)}
	f.eng.autoBindEmbedder(f.ctx)
	_, err = f.binding(t)
	require.ErrorIs(t, err, ErrNoEmbedderBound)

	f.catalogs[f.owner] = []FleetModel{embedderModel(model, true)}
	f.eng.autoBindEmbedder(f.ctx)
	b, err := f.binding(t)
	require.NoError(t, err)
	require.Equal(t, FleetReferencePrefix+model, b.ProviderRef)
	require.Equal(t, f.owner, b.IndexedBy)
	require.Empty(t, b.ChosenBy, "the automatic path is nobody's choice")
	table, _ := EmbeddingVectorTable(b.ProviderRef, b.Dimensions)
	t.Cleanup(func() { _, _ = f.db.DB.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+table) })
}

// A person's catalog is their own machines AND the ones lent to them, so a
// model that arrives only by being lent to the owner is the owner's.
func TestAMachineLentToAnOwnerCounts(t *testing.T) {
	f := newAutobindFixture(t)
	model := "embed-" + uniqueSuffix("l")
	lent := embedderModel(model, true)
	lent.Machines[0].OwnerUserId = "v1:identity:user:somebody-else"
	f.catalogs[f.owner] = []FleetModel{lent}
	cands, err := f.eng.ownerEmbeddingCandidates(f.ctx)
	require.NoError(t, err)
	require.Contains(t, cands, embedderCandidate{ProviderRef: FleetReferencePrefix + model, OwnerUserId: f.owner})
	for _, c := range cands {
		require.NotEqual(t, f.writer, c.OwnerUserId)
	}
}

// A binding made before indexers existed names nobody; boot adopts the first
// active owner whose catalog offers the model -- online or not -- and leaves
// who chose it empty. An owner already recorded is never replaced.
func TestAnExistingFleetBindingAdoptsAnOwnerAsIndexer(t *testing.T) {
	f := newAutobindFixture(t)
	model := "embed-" + uniqueSuffix("a")
	ref := FleetReferencePrefix + model
	f.catalogs[f.owner] = []FleetModel{embedderModel(model, false)}
	_, err := f.eng.bindEmbedder(f.ctx, ref, embedderChoice{})
	require.NoError(t, err)
	table, _ := EmbeddingVectorTable(ref, 1024)
	t.Cleanup(func() { _, _ = f.db.DB.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+table) })
	b, err := f.binding(t)
	require.NoError(t, err)
	require.Empty(t, b.IndexedBy)

	require.NoError(t, f.eng.adoptEmbedderIndexer(f.ctx))
	b, err = f.binding(t)
	require.NoError(t, err)
	require.Equal(t, f.owner, b.IndexedBy)
	require.Empty(t, b.ChosenBy)

	// Idempotent, and a recorded indexer is kept.
	require.NoError(t, f.eng.adoptEmbedderIndexer(f.ctx))
	again, _ := f.binding(t)
	require.Equal(t, b, again)
}

func TestBindEmbedderRecordsWhoChoseIt(t *testing.T) {
	f := newAutobindFixture(t)
	ownerCtx := auth.ContextWithAccess(f.ctx, &auth.AccessContext{UserId: f.owner, Role: auth.RoleOwner})
	model := "embed-" + uniqueSuffix("c")
	ref := FleetReferencePrefix + model
	_, err := f.eng.bindEmbedderBuiltin(ownerCtx, map[string]any{"provider": ref}, 0)
	require.NoError(t, err)
	table, _ := EmbeddingVectorTable(ref, 1024)
	t.Cleanup(func() { _, _ = f.db.DB.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+table) })
	b, err := f.binding(t)
	require.NoError(t, err)
	require.Equal(t, f.owner, b.ChosenBy)
	require.Equal(t, f.owner, b.IndexedBy, "a fleet model is indexed on the chooser's machines")
}

// R5: the resolver re-checks that the indexer is still an active owner, and a
// refusal reaches the writer as an error so its usual "until backfilled" line
// is logged.
func TestAnIndexerWhoIsNoLongerAnOwnerIsRefused(t *testing.T) {
	const bound = "fleet:qwen3-embedding:0.6b"
	countingBinding(t, EmbedderBinding{ProviderRef: bound, Dimensions: 1024, IndexedBy: "owner-1"}, nil)
	caller := auth.ContextWithUserActor(context.Background(), "writer-1")
	owners := map[string]bool{"owner-1": true}
	SetEmbeddingIndexerOwnerCheck(func(_ context.Context, id string) bool { return owners[id] })
	t.Cleanup(func() { SetEmbeddingIndexerOwnerCheck(nil) })

	got, err := EmbeddingIndexerForCall(caller, bound)
	require.NoError(t, err)
	require.Equal(t, "owner-1", got)

	owners["owner-1"] = false
	_, err = EmbeddingIndexerForCall(caller, bound)
	require.ErrorIs(t, err, ErrEmbeddingIndexerNotOwner)
	require.ErrorContains(t, err, "Cluster > Settings > Memory search")
	// The index job's named indexer is held to the same rule.
	_, err = EmbeddingIndexerForCall(ContextWithEmbeddingIndexer(caller, "owner-1"), "fleet:other")
	require.ErrorIs(t, err, ErrEmbeddingIndexerNotOwner)

	// An empty IndexedBy is production's binding until adoption: the caller's.
	countingBinding(t, EmbedderBinding{ProviderRef: bound, Dimensions: 1024}, nil)
	got, err = EmbeddingIndexerForCall(caller, bound)
	require.NoError(t, err)
	require.Empty(t, got)
}

// The engine's lookup is cached per replica for 60 seconds.
func TestTheOwnerLookupIsCached(t *testing.T) {
	f := newAutobindFixture(t)
	require.True(t, f.eng.IndexerIsActiveOwner(f.ctx, f.owner))
	require.False(t, f.eng.IndexerIsActiveOwner(f.ctx, f.writer))
	_, err := f.db.DB.ExecContext(f.ctx, `DELETE FROM "MemoryNodes" WHERE id=$1`, f.owner)
	require.NoError(t, err)
	require.True(t, f.eng.IndexerIsActiveOwner(f.ctx, f.owner), "served from the cache inside the TTL")
}
