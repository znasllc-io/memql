package memql

// embedder_autobind_test.go -- the automatic first memory-search model is an
// OWNER'S MACHINE model (design record 2026-11-23, sections 1 and 2): never a
// vendor model by itself, never one only a non-owner's machine offers, and a
// machine lent to an owner counts as that owner's. Also who indexes and who
// chose, and the resolver's re-check that the indexer is still an owner.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
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

// An unfinished first binding -- its plan recorded, its activation never
// reached, as when a replica dies between the two writes -- is NAMED to whoever
// chooses another model: the plan write never moves a row's model (@createOnly),
// so only its own model can finish it, and the refusal says which, where it
// used to answer "changed meanwhile, try again" for ever. Nothing is probed for
// the refused model. Choosing the named one finishes the binding.
func TestAnUnfinishedPlanIsNamedSoItCanBeFinished(t *testing.T) {
	f := newAutobindFixture(t)
	model := "embed-stuck-" + uniqueSuffix("p")
	stuck := FleetReferencePrefix + model
	call, err := parser.RenderCall("platform.recordEmbedderBindingPlan", map[string]any{"bindingId": ActiveEmbedderBindingID,
		"providerRef": stuck, "dimensions": 1024, "versionTime": time.Now().UTC().Format(time.RFC3339Nano)})
	require.NoError(t, err)
	_, err = f.eng.Execute(auth.ContextWithInternalOrigin(f.ctx), "mutation "+call)
	require.NoError(t, err)
	_, err = f.binding(t)
	require.ErrorIs(t, err, ErrNoEmbedderBound, "a plan is not a binding")

	ownerCtx := auth.ContextWithAccess(f.ctx, &auth.AccessContext{UserId: f.owner, Role: auth.RoleOwner})
	_, err = f.eng.bindEmbedderBuiltin(ownerCtx, map[string]any{"provider": FleetReferencePrefix + "embed-" + uniqueSuffix("q")}, 0)
	require.EqualError(t, err, "an unfinished binding of "+model+" is in the way; choose "+model+" to finish it")
	require.Zero(t, f.probe.calls, "nothing probed for the refused model")

	table, _ := EmbeddingVectorTable(stuck, 1024)
	t.Cleanup(func() { _, _ = f.db.DB.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+table) })
	_, err = f.eng.bindEmbedderBuiltin(ownerCtx, map[string]any{"provider": stuck}, 0)
	require.NoError(t, err)
	b, err := f.binding(t)
	require.NoError(t, err)
	require.Equal(t, stuck, b.ProviderRef)
	require.Equal(t, f.owner, b.ChosenBy)
}

// The resolver re-checks that the indexer is still an active owner, and a
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

// A refused indexer is a WAIT: it satisfies the fleet-unavailable check every
// indexing caller already makes.
func TestARefusedIndexerReadsAsUnavailable(t *testing.T) {
	require.ErrorIs(t, ErrEmbeddingIndexerNotOwner, ErrFleetUnavailable)
	require.ErrorIs(t, fmt.Errorf("wrapped: %w", ErrEmbeddingIndexerNotOwner), ErrEmbeddingIndexerNotOwner)
}

// A failed read keeps the last good set, is retried after a short backoff and
// not on every call, and a replica with no earlier answer refuses.
func TestTheOwnerLookupSurvivesAFailedRead(t *testing.T) {
	e := &MemQLEngine{}
	reads, fail := 0, false
	e.indexerOwners.read = func(context.Context) ([]string, error) {
		reads++
		if fail {
			return nil, errors.New("database unavailable")
		}
		return []string{"v1:identity:user:o1"}, nil
	}
	ctx := context.Background()
	// No earlier answer and the read fails: refuse, and do not hammer.
	fail = true
	require.False(t, e.IndexerIsActiveOwner(ctx, "o1"))
	require.False(t, e.IndexerIsActiveOwner(ctx, "o1"))
	require.Equal(t, 1, reads, "a failed read is retried after a backoff, not on every call")
	// Recovered: the set is read once the backoff passes.
	fail = false
	e.indexerOwners.retryAt = time.Time{}
	require.True(t, e.IndexerIsActiveOwner(ctx, "o1"))
	require.Equal(t, 2, reads)
	// Stale, then the database fails: the last good set keeps answering.
	e.indexerOwners.at = time.Now().Add(-2 * indexerOwnerTTL)
	fail = true
	require.True(t, e.IndexerIsActiveOwner(ctx, "o1"))
	require.True(t, e.IndexerIsActiveOwner(ctx, "o1"))
	require.Equal(t, 3, reads)
}

// A read of the owners that panics does not stop later reads. The panic goes on
// to whoever recovers it, but the replica must read again on its next call:
// left marked as reading, it would answer from the set it had -- or refuse
// every indexer, with none -- for good.
func TestAnOwnerReadThatPanicsDoesNotStopLaterReads(t *testing.T) {
	e := &MemQLEngine{}
	reads, panics := 0, true
	e.indexerOwners.read = func(context.Context) ([]string, error) {
		reads++
		if panics {
			panic("the owner read panicked")
		}
		return []string{"v1:identity:user:o1"}, nil
	}
	ctx := context.Background()
	require.Panics(t, func() { e.IndexerIsActiveOwner(ctx, "o1") })
	panics = false
	require.True(t, e.IndexerIsActiveOwner(ctx, "o1"), "the next call reads again")
	require.Equal(t, 2, reads)
}

// With no set to answer from, the owner check says why rather than answering
// no (indexerOwnerAnswer): an owner's act on Memory search then reports a check
// that could not be made, where "you are not an active cluster owner" would be
// a guess. IndexerIsActiveOwner still reads that state as false. Once a set is
// read it answers, a no included, and a stale one keeps answering through an
// outage, as the resolver's does.
func TestAnOwnerCheckWithNoSetToAnswerFromSaysWhy(t *testing.T) {
	e := &MemQLEngine{}
	fail := true
	e.indexerOwners.read = func(context.Context) ([]string, error) {
		if fail {
			return nil, errors.New("database unavailable")
		}
		return []string{"v1:identity:user:o1"}, nil
	}
	ctx := context.Background()
	for range 2 { // the read, then the backoff after it
		owner, err := e.indexerOwnerAnswer(ctx, "o1")
		require.ErrorContains(t, err, "database unavailable")
		require.False(t, owner)
	}
	require.False(t, e.IndexerIsActiveOwner(ctx, "o1"))

	fail = false
	e.indexerOwners.retryAt = time.Time{}
	owner, err := e.indexerOwnerAnswer(ctx, "o1")
	require.NoError(t, err)
	require.True(t, owner)
	owner, err = e.indexerOwnerAnswer(ctx, "o2")
	require.NoError(t, err)
	require.False(t, owner, "a set that was read says no without an error")

	e.indexerOwners.at = time.Now().Add(-2 * indexerOwnerTTL)
	fail = true
	owner, err = e.indexerOwnerAnswer(ctx, "o1")
	require.NoError(t, err)
	require.True(t, owner)
}
