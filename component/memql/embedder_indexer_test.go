package memql

// embedder_indexer_test.go -- memory search embeds AS ITS INDEXER, whoever
// asks (design record 2026-11-23, section 1).
//
// This file pins the engine's half: the binding row carrying its indexer, the
// order in which a call's indexer is decided, the retired row's id, the
// assertion an indexer's embedding carries across a replica hop, and the
// binding's writes. The resolver every plug-in embeds through is pinned in
// app/embedding_indexer_test.go, against the real router; the hop itself in
// integrations/agent/worker/memory_search_hop_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// THE ROW IS HOW THE INDEXER REACHES EVERY REPLICA: each one reads the binding
// fresh (ReadEmbedderBinding), so an indexer that did not decode would send
// every embedding back to whoever asked, on every replica at once.
func TestABindingRowCarriesItsIndexer(t *testing.T) {
	// The shape a row arrives in from MaterializeRows: JSON-decoded, so the
	// width is a float64.
	row := map[string]any{
		"id":          "active",
		"providerRef": "fleet:qwen3-embedding:0.6b",
		"dimensions":  float64(1024),
		"activatedAt": "2026-11-24T10:00:00Z",
		"previousRef": "embedding3Small",
		"indexedBy":   "v1:identity:user:owner-1",
		"chosenBy":    "v1:identity:user:owner-2",
		"retireAt":    "2026-12-24T10:00:00Z",
		"createdAt":   "2026-11-24T10:00:00Z",
	}
	b, err := embedderBindingFromRow(row)
	require.NoError(t, err)
	require.Equal(t, EmbedderBinding{
		ProviderRef: "fleet:qwen3-embedding:0.6b",
		Dimensions:  1024,
		ActivatedAt: "2026-11-24T10:00:00Z",
		PreviousRef: "embedding3Small",
		IndexedBy:   "v1:identity:user:owner-1",
		ChosenBy:    "v1:identity:user:owner-2",
		RetireAt:    "2026-12-24T10:00:00Z",
	}, b)

	indexer, fleet := IndexerFor(b)
	require.True(t, fleet)
	require.Equal(t, "v1:identity:user:owner-1", indexer)

	// A vendor has no machines, so a vendor binding has no indexer whatever
	// its row says: the cluster's account pays for it instead.
	vendor := b
	vendor.ProviderRef = "embedding3Small"
	indexer, fleet = IndexerFor(vendor)
	require.False(t, fleet)
	require.Empty(t, indexer)

	// A row written before indexers existed decodes with none: a state to
	// repair by adopting an owner as its indexer, not an error.
	delete(row, "indexedBy")
	delete(row, "chosenBy")
	delete(row, "retireAt")
	legacy, err := embedderBindingFromRow(row)
	require.NoError(t, err)
	indexer, fleet = IndexerFor(legacy)
	require.True(t, fleet)
	require.Empty(t, indexer)
}

// A retired model's row is keyed by its index's own table hash, so the row and
// the table it keeps can never name two different models.
func TestRetiredIdsFollowTheTableHash(t *testing.T) {
	id := RetiredEmbedderBindingID("fleet:a", 1024)
	table, err := EmbeddingVectorTable("fleet:a", 1024)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(id, "retired-"), "id %q", id)
	hex := strings.TrimPrefix(id, "retired-")
	require.Len(t, hex, 16)
	require.True(t, strings.HasSuffix(table, "_"+hex), "table %q does not end in %q", table, hex)

	require.NotEqual(t, id, RetiredEmbedderBindingID("fleet:b", 1024), "two models must not share a retired row")
	// A ref or width the table refuses has no retired row either.
	require.Empty(t, RetiredEmbedderBindingID("", 1024))
	require.Empty(t, RetiredEmbedderBindingID("fleet:a", 0))
}

// countingBinding installs a durable reader that answers b and counts reads,
// as production's ReadEmbedderBinding is a database read on every call.
// indexerOf is EmbeddingIndexerForCall for tests with no owner check installed,
// where it cannot refuse.
func indexerOf(t *testing.T, ctx context.Context, ref string) string {
	t.Helper()
	got, err := EmbeddingIndexerForCall(ctx, ref)
	require.NoError(t, err)
	return got
}

func countingBinding(t *testing.T, b EmbedderBinding, err error) *int {
	t.Helper()
	reads := 0
	SetEmbedderBindingReader(func(context.Context) (EmbedderBinding, error) {
		reads++
		return b, err
	})
	t.Cleanup(func() { SetEmbedderBindingReader(nil) })
	return &reads
}

// THE ORDER A CALL'S INDEXER IS DECIDED IN: the context's (the index job's),
// then the ACTIVE binding's for its own model, then nobody -- the caller, as
// every other fleet call. The binding is read only when the first answer is
// not on the context, so a caller that already holds the indexer costs no
// second read.
func TestTheIndexerIsTheContextsThenTheActiveBindingsThenNobody(t *testing.T) {
	const bound = "fleet:qwen3-embedding:0.6b"
	reads := countingBinding(t, EmbedderBinding{ProviderRef: bound, Dimensions: 1024, IndexedBy: "owner-1"}, nil)
	caller := auth.ContextWithUserActor(context.Background(), "writer-1")

	require.Equal(t, "owner-1", indexerOf(t, caller, bound))
	require.Equal(t, 1, *reads)

	job := ContextWithEmbeddingIndexer(caller, "owner-2")
	require.Equal(t, "owner-2", indexerOf(t, job, bound))
	// The job's model need not be the active one: a switch fills its target.
	require.Equal(t, "owner-2", indexerOf(t, job, "fleet:nomic-embed-text"))
	require.Equal(t, 1, *reads, "a context that names the indexer must not read the binding again")

	// A fleet model that is not the bound one is nobody's index: its caller's.
	require.Empty(t, indexerOf(t, caller, "fleet:nomic-embed-text"))
	// A vendor has no machines, whoever the context names.
	require.Empty(t, indexerOf(t, job, "embedding3Small"))
	require.Empty(t, indexerOf(t, caller, "embedding3Small"))
	require.Equal(t, 2, *reads, "a vendor ref must not read the binding at all")

	// An unbound cluster, and a read that failed, borrow nobody's machines.
	countingBinding(t, EmbedderBinding{}, ErrNoEmbedderBound)
	require.Empty(t, indexerOf(t, caller, bound))
	countingBinding(t, EmbedderBinding{}, errors.New("database unavailable"))
	require.Empty(t, indexerOf(t, caller, bound))
}

// The engine's attribution fills a request's caller from the context only when
// the request named none. The indexer is a caller the request FIXED, on a
// context that names somebody else -- the writer, and a run owned by them --
// and filling over it would send the embedding back to the asker's machines.
func TestTheIndexerOutlivesTheCallersOwnAttribution(t *testing.T) {
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "writer-1"),
		common.RunContext{RunId: "v1:work:run:r", OwnerUserId: "writer-1", StepKey: "s"})
	req := applyCallAttribution(ctx, airoute.ResolveRequest{Level: airoute.LevelEmbeddings, Modality: airoute.ModalityEmbedding,
		ExplicitProvider: "fleet:qwen3-embedding:0.6b", UserId: "owner-1"})
	require.Equal(t, "owner-1", req.UserId)
	// The rest of the attribution is still the call's own.
	require.Equal(t, "v1:work:run:r", req.RunId)
	require.Equal(t, auth.CallerKindUser, req.CallerKind)
}

func TestAnEmptyIndexerStampsNothing(t *testing.T) {
	outer := ContextWithEmbeddingIndexer(context.Background(), "owner-1")
	// An empty stamp would shadow the outer one with "nobody".
	require.Equal(t, outer, ContextWithEmbeddingIndexer(outer, "  "))
	got, ok := EmbeddingIndexerFromContext(outer)
	require.True(t, ok)
	require.Equal(t, "owner-1", got)
	_, ok = EmbeddingIndexerFromContext(context.Background())
	require.False(t, ok)
}

// hopWitness records what the context of each embedding call carried.
type hopWitness struct {
	asserted []auth.ForwardedAuthority
	actors   []string
}

func (w *hopWitness) note(ctx context.Context) {
	a, _ := auth.ForwardedAuthorityFromContext(ctx)
	w.asserted = append(w.asserted, a)
	actor := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		actor = ac.UserId
	}
	w.actors = append(w.actors, actor)
}

func (w *hopWitness) Embed(ctx context.Context, _ string) ([]float32, error) {
	w.note(ctx)
	return []float32{1, 0}, nil
}

func (w *hopWitness) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	w.note(ctx)
	return make([][]float32, len(texts)), nil
}

func (*hopWitness) Dimensions() int { return 2 }

// THE INDEXER'S EMBEDDING CARRIES THE INDEXER'S ASSERTION, AND ONLY THAT. The
// replica holding a machine's stream checks the machine against the assertion's
// subject, so without it a call resolved for the indexer is refused the moment
// it has to cross a replica (memory_search_hop_test.go). The actor the call
// runs as is left alone: a work run's admission asks it for the run's owner,
// and the caller's own context is not changed for anything after the call.
func TestAnIndexerEmbeddingCarriesTheIndexersAssertionAndNothingElse(t *testing.T) {
	inner := &hopWitness{}
	p := EmbedAsIndexer(inner, "v1:identity:user:owner-1")
	caller := auth.ContextWithUserActor(context.Background(), "v1:identity:user:writer-1")

	_, err := p.Embed(caller, "a note the writer wrote")
	require.NoError(t, err)
	_, err = p.EmbedBatch(caller, []string{"a", "b"})
	require.NoError(t, err)
	require.Equal(t, 2, p.Dimensions())

	require.Len(t, inner.asserted, 2)
	for i, a := range inner.asserted {
		require.Equal(t, "v1:identity:user:owner-1", a.Subject)
		require.Equal(t, auth.ForwardedPrincipalUser, a.Kind)
		require.Equal(t, auth.ForwardedClassUser, a.CredentialClass)
		// Writer, never the owner's own rung: the hop needs a subject to check
		// a machine against, and nothing more.
		require.Equal(t, auth.RoleWriter, a.Role)
		_, err := auth.VerifyForwardedAuthority(a, time.Now())
		require.NoError(t, err, "the receiving replica must be able to verify it")
		require.Equal(t, "v1:identity:user:writer-1", inner.actors[i], "the call still runs as its caller")
	}
	_, borrowed := auth.ForwardedAuthorityFromContext(caller)
	require.False(t, borrowed, "the caller's own context must not keep the indexer's assertion")

	// A test persona never borrows anybody, the indexer included.
	persona := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: auth.RoleTestUserPrefix + "writer", Role: auth.RoleWriter})
	_, err = p.Embed(persona, "text")
	require.ErrorContains(t, err, "test persona")
	require.Len(t, inner.asserted, 2, "a refused call must not reach the model")

	// Nobody to act as means nothing to wrap.
	require.Same(t, inner, EmbedAsIndexer(inner, " ").(*hopWitness))
	require.Nil(t, EmbedAsIndexer(nil, "owner-1"))
}

// THE BINDING'S WRITES, against a real database: the indexer and the chooser
// read back through the same query every replica reads the binding with, a
// flip states the whole new model (nothing of the previous one's indexer or
// chooser survives it), and a retired model's row is never read as the active
// one.
func TestTheBindingWritesRecordWhoIndexesAndWhatWasRetired(t *testing.T) {
	e, db, _ := sharedReadMergeEngine(t)
	ctx := askTestActor()
	access, _ := auth.AccessFromContext(ctx)
	owner := *access
	owner.Role = auth.RoleOwner
	internal := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, &owner))
	// Preserve any other fixture's binding. This suite runs only against
	// dbtest's disposable database, and never reaches a running cluster's.
	var saved string
	require.NoError(t, db.DB.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(n)),'[]')::text FROM "MemoryNodes" n WHERE concept=$1`, EmbedderBindingConceptID).Scan(&saved))
	_, err := db.DB.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept=$1`, EmbedderBindingConceptID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept=$1`, EmbedderBindingConceptID)
		_, _ = db.DB.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" SELECT * FROM jsonb_populate_recordset(NULL::"MemoryNodes",$1::jsonb)`, saved)
	})
	// Every write of the binding and the retired rows is stamped with a
	// version (ruling R12); this test writes them directly, so it stamps each
	// itself.
	write := func(name string, args map[string]any) {
		t.Helper()
		args["versionTime"] = time.Now().UTC().Format(time.RFC3339Nano)
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		_, err = e.Execute(internal, "mutation "+call)
		require.NoError(t, err, name)
	}
	stored := func(id string) map[string]any {
		t.Helper()
		var raw string
		require.NoError(t, db.DB.QueryRowContext(ctx, `SELECT payload::text FROM "MemoryNodes" WHERE concept=$1 AND id=$2 ORDER BY "createdAt" DESC LIMIT 1`,
			EmbedderBindingConceptID, EmbedderBindingConceptID+":"+id).Scan(&raw))
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &payload))
		return payload
	}
	at := func(d time.Duration) string { return time.Now().UTC().Add(d).Format(time.RFC3339Nano) }

	write("platform.recordEmbedderBindingPlan", map[string]any{"bindingId": "active", "providerRef": "fleet:indexer-test-a", "dimensions": 1024})
	write("platform.activateEmbedderBinding", map[string]any{"bindingId": "active", "activatedAt": at(0)})
	write("platform.setEmbedderBindingChoice", map[string]any{"bindingId": "active", "indexedBy": "owner-1", "chosenBy": "owner-1"})
	b, err := e.ReadEmbedderBinding(ctx)
	require.NoError(t, err)
	require.Equal(t, "fleet:indexer-test-a", b.ProviderRef)
	require.Equal(t, "owner-1", b.IndexedBy)
	require.Equal(t, "owner-1", b.ChosenBy)

	// Each writer names only its own field: adopting an indexer leaves the
	// chooser as it was.
	write("platform.setEmbedderBindingChoice", map[string]any{"bindingId": "active", "indexedBy": "owner-3"})
	b, err = e.ReadEmbedderBinding(ctx)
	require.NoError(t, err)
	require.Equal(t, "owner-3", b.IndexedBy)
	require.Equal(t, "owner-1", b.ChosenBy)

	write("platform.flipEmbedderBinding", map[string]any{"bindingId": "active", "providerRef": "fleet:indexer-test-b", "dimensions": 768,
		"previousRef": "fleet:indexer-test-a", "activatedAt": at(time.Second), "indexedBy": "owner-2"})
	retired := RetiredEmbedderBindingID("fleet:indexer-test-a", 1024)
	write("platform.recordRetiredEmbedder", map[string]any{"bindingId": retired, "providerRef": "fleet:indexer-test-a", "dimensions": 1024,
		"indexedBy": "owner-3", "retireAt": at(30 * 24 * time.Hour)})
	b, err = e.ReadEmbedderBinding(ctx)
	require.NoError(t, err)
	require.Equal(t, EmbedderBinding{ProviderRef: "fleet:indexer-test-b", Dimensions: 768, ActivatedAt: b.ActivatedAt, PreviousRef: "fleet:indexer-test-a", IndexedBy: "owner-2"}, b,
		"a flip states the whole new model; the previous model's chooser must not survive it")

	row := stored(retired)
	require.Equal(t, "fleet:indexer-test-a", row["providerRef"])
	require.EqualValues(t, 1024, row["dimensions"])
	require.Equal(t, "owner-3", row["indexedBy"])
	require.NotEmpty(t, row["retireAt"])

	// Retiring the same model again later states its whole row again: a
	// dropped date left from an earlier retirement does not survive it, or a
	// table that exists again would read as dropped.
	write("platform.recordRetiredEmbedder", map[string]any{"bindingId": retired, "providerRef": "fleet:indexer-test-a", "dimensions": 1024,
		"retireAt": at(-time.Hour), "droppedAt": at(0)})
	require.NotEmpty(t, stored(retired)["droppedAt"])
	write("platform.recordRetiredEmbedder", map[string]any{"bindingId": retired, "providerRef": "fleet:indexer-test-a", "dimensions": 1024,
		"retireAt": at(30 * 24 * time.Hour)})
	require.Empty(t, stored(retired)["droppedAt"])

	// A retired row's id is the only kind recordRetiredEmbedder writes: it
	// cannot be pointed at the active binding.
	call, err := parser.RenderCall("platform.recordRetiredEmbedder", map[string]any{"bindingId": "active", "providerRef": "fleet:indexer-test-c", "dimensions": 1024,
		"versionTime": at(0)})
	require.NoError(t, err)
	_, err = e.Execute(internal, "mutation "+call)
	require.Error(t, err)

	// Every one of them is the engine's: a client cannot flip the binding.
	call, err = parser.RenderCall("platform.flipEmbedderBinding", map[string]any{"bindingId": "active", "providerRef": "fleet:indexer-test-c", "dimensions": 1024, "activatedAt": at(0),
		"versionTime": at(time.Minute)})
	require.NoError(t, err)
	_, err = e.Execute(auth.ContextWithClientOrigin(ctx), "mutation "+call)
	require.Error(t, err)
	b, err = e.ReadEmbedderBinding(ctx)
	require.NoError(t, err)
	require.Equal(t, "fleet:indexer-test-b", b.ProviderRef)
}
