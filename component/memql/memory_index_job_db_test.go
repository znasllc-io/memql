package memql

// memory_index_job_db_test.go -- the memory index job against Postgres, with
// TWO ENGINES OVER ONE DATABASE (design record 2026-11-23, sections 4, 6 and 9):
// the lock, the fenced run rows, the flip's writes, and a walk that one replica
// starts and another finishes. Nothing crosses between the engines but the
// database.
//
// THE STORE PATH IS A STUB, and deliberately the same contract as the real one
// (integrations/embedding storeHandler): embed with the provider named or else
// the active binding's, ensure that model's table, upsert on (id, vector_field).
// Its vector has the width the test gives each model. component/memql cannot
// import the integration (it imports this package); the integration's own
// tests hold its half. The search is a stub of similarTo's table choice, which
// is the half a flip changes: the active binding's model and width name the
// table it reads.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
)

type memoryIndexStoreCall struct {
	engine, provider, nodeId, indexer string
	job, ok                           bool
}

type memoryIndexDBFixture struct {
	t       *testing.T
	a, b    *MemQLEngine
	db      *bun.DB
	ctx     context.Context
	token   string
	concept string
	owner   string
	widths  map[string]int

	mu      sync.Mutex
	owners  []string
	calls   []memoryIndexStoreCall
	offline map[string]bool
	failing map[string]bool
	block   chan struct{}
	entered chan struct{}
	tables  map[string]bool
}

// newMemoryIndexDBFixture boots two engines that walk only this test's own
// source, saves and clears the cluster-wide rows a switch touches (the binding
// and both runs) and restores them afterwards, and points the process's binding
// reader at the database, as every replica's is.
func newMemoryIndexDBFixture(t *testing.T) *memoryIndexDBFixture {
	t.Helper()
	a, db, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%d", t.Name(), os.Getpid(), time.Now().UnixNano())))
	token := hex.EncodeToString(sum[:5])
	f := &memoryIndexDBFixture{t: t, a: a, b: b, db: db, token: token,
		concept: "v1:memidx" + token + ":item",
		owner:   "v1:identity:user:owner-" + token,
		widths:  map[string]int{},
		offline: map[string]bool{}, failing: map[string]bool{}, tables: map[string]bool{},
		entered: make(chan struct{}, 1),
		ctx:     auth.ContextWithSystemActor(context.Background(), "memoryIndexTest"),
	}
	f.owners = []string{f.owner}
	source := MemorySource{Name: "testItems", Concept: f.concept, VectorField: "content",
		Rows: func(p map[string]any) (string, bool) {
			text, _ := p["text"].(string)
			return text, strings.TrimSpace(text) != ""
		}}
	for name, e := range map[string]*MemQLEngine{"a": a, "b": b} {
		e.memoryIndexSources = func() []MemorySource { return []MemorySource{source} }
		e.indexerOwners.read = func(context.Context) ([]string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]string(nil), f.owners...), nil
		}
		require.NoError(t, e.integrations.Register(memoryIndexTestIntegration{handler: f.store(name, e)}))
	}
	for _, c := range []string{EmbedderBindingConceptID, MemoryIndexRunConceptID} {
		var saved string
		require.NoError(t, db.DB.QueryRowContext(f.ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(n)),'[]')::text FROM "MemoryNodes" n WHERE concept=$1`, c).Scan(&saved))
		_, err := db.DB.ExecContext(f.ctx, `DELETE FROM "MemoryNodes" WHERE concept=$1`, c)
		require.NoError(t, err)
		concept := c
		t.Cleanup(func() {
			bg := context.Background()
			_, _ = db.DB.ExecContext(bg, `DELETE FROM "MemoryNodes" WHERE concept=$1`, concept)
			_, _ = db.DB.ExecContext(bg, `INSERT INTO "MemoryNodes" SELECT * FROM jsonb_populate_recordset(NULL::"MemoryNodes",$1::jsonb)`, saved)
		})
	}
	bindingReader.RLock()
	previous := bindingReader.read
	bindingReader.RUnlock()
	SetEmbedderBindingReader(b.ReadEmbedderBinding)
	t.Cleanup(func() {
		SetEmbedderBindingReader(previous)
		bg := context.Background()
		_, _ = db.DB.ExecContext(bg, `DELETE FROM "MemoryNodes" WHERE concept=$1`, f.concept)
		f.mu.Lock()
		defer f.mu.Unlock()
		for table := range f.tables {
			_, _ = db.DB.ExecContext(bg, `DROP TABLE IF EXISTS `+table)
		}
	})
	return f
}

// ref names a model of this test, with its width.
func (f *memoryIndexDBFixture) ref(name string, width int) string {
	ref := "fleet:memidx-" + name + "-" + f.token
	f.widths[ref] = width
	table, err := EmbeddingVectorTable(ref, width)
	require.NoError(f.t, err)
	f.mu.Lock()
	f.tables[table] = true
	f.mu.Unlock()
	return ref
}

// memoryIndexVector is a fixed, nonzero vector of a width for an id.
func memoryIndexVector(id string, width int) string {
	parts := make([]string, width)
	for i := range parts {
		parts[i] = "0"
	}
	parts[0] = "1"
	sum := sha256.Sum256([]byte(id))
	parts[int(sum[0])%width] = "2"
	return "[" + strings.Join(parts, ",") + "]"
}

// store is the stub of the embedding store path on one engine.
func (f *memoryIndexDBFixture) store(engine string, e *MemQLEngine) builtinExecutorHandler {
	return func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		provider, _ := args["provider"].(string)
		job := provider != ""
		if !job {
			bound, err := ResolveEmbedderProvider(ctx)
			if err != nil {
				return nil, err
			}
			provider = bound
		}
		nodeId, _ := args["nodeId"].(string)
		field, _ := args["vectorField"].(string)
		concept, _ := args["concept"].(string)
		indexer, _ := EmbeddingIndexerFromContext(ctx)
		f.mu.Lock()
		offline, failing, block := f.offline[provider], f.failing[nodeId], f.block
		f.block = nil
		f.mu.Unlock()
		if block != nil {
			f.entered <- struct{}{}
			<-block
		}
		record := func(ok bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls = append(f.calls, memoryIndexStoreCall{engine: engine, provider: provider, nodeId: nodeId, indexer: indexer, job: job, ok: ok})
		}
		if offline {
			record(false)
			model := strings.TrimPrefix(provider, FleetReferencePrefix)
			return nil, fmt.Errorf("store: embed: resolve provider %q: every_door_shut: no door to a model is open for this call [pinned to %s: a pinned source is never substituted]; %s (local): no machine offering %s is online", provider, provider, provider, model)
		}
		if failing {
			record(false)
			return nil, errors.New("store: embed: embedding model returned a zero vector")
		}
		width := f.widths[provider]
		table, err := EnsureEmbeddingVectorTable(ctx, e.database().DB, provider, width)
		if err != nil {
			return nil, err
		}
		if _, err = e.database().DB.ExecContext(ctx, `INSERT INTO `+table+` (id, concept, vector_field, embedding) VALUES ($1, $2, $3, $4::vector)
			ON CONFLICT (id, vector_field) DO UPDATE SET embedding = EXCLUDED.embedding, concept = EXCLUDED.concept, updated_at = NOW()`,
			nodeId, concept, field, memoryIndexVector(nodeId, width)); err != nil {
			return nil, err
		}
		record(true)
		raw, _ := json.Marshal(map[string]any{"status": "stored", "nodeId": nodeId, "provider": provider, "dimensions": width})
		return []memorynodes.MemoryNode{{ID: "embedding:store", Payload: raw}}, nil
	}
}

// bind makes ref the active binding, indexed and chosen by indexer, as
// bindEmbedder leaves it: plan, activation, choice, and the model's table.
func (f *memoryIndexDBFixture) bind(ref string, indexer string) {
	f.t.Helper()
	width := f.widths[ref]
	writes := []struct {
		name string
		args map[string]any
	}{
		{"platform.recordEmbedderBindingPlan", map[string]any{"bindingId": "active", "providerRef": ref, "dimensions": width}},
		{"platform.activateEmbedderBinding", map[string]any{"bindingId": "active", "activatedAt": time.Now().UTC().Format(time.RFC3339Nano)}},
		{"platform.setEmbedderBindingChoice", map[string]any{"bindingId": "active", "indexedBy": indexer, "chosenBy": indexer}},
	}
	for _, w := range writes {
		call, err := parser.RenderCall(w.name, w.args)
		require.NoError(f.t, err)
		_, err = f.a.Execute(auth.ContextWithInternalOrigin(f.ctx), "mutation "+call)
		require.NoError(f.t, err, w.name)
	}
	_, err := EnsureEmbeddingVectorTable(f.ctx, f.db.DB, ref, width)
	require.NoError(f.t, err)
}

// addItems writes rows of this test's source.
func (f *memoryIndexDBFixture) addItems(names ...string) []string {
	f.t.Helper()
	var ids []string
	for _, name := range names {
		id := f.concept + ":" + name
		payload, _ := json.Marshal(map[string]any{"text": "a note about " + name})
		row := []*memorynodes.MemoryNode{{ID: id, Concept: f.concept, CreatedBy: "memory-index-test", CreatedAt: time.Now().UTC(),
			Payload: payload, Provenance: json.RawMessage(`{"kind":"direct","name":"memory-index-test"}`)}}
		_, err := f.db.NewInsert().Model(&row).Exec(f.ctx)
		require.NoError(f.t, err)
		ids = append(ids, id)
	}
	return ids
}

func memoryIndexNames(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%03d", prefix, i+1)
	}
	return out
}

// write is an everyday writer: the store path with no provider, so the item
// goes to whichever model the binding names at that moment.
func (f *memoryIndexDBFixture) write(e *MemQLEngine, ids ...string) {
	f.t.Helper()
	store, ok := e.integrations.Get("integration.embedding.store")
	require.True(f.t, ok)
	for _, id := range ids {
		_, err := store(f.ctx, map[string]any{"nodeId": id, "text": "a note", "concept": f.concept, "vectorField": "content"}, 1)
		require.NoError(f.t, err)
	}
}

// startSwitch writes the switch run as an owner's switch does (Task 5's
// memorySearchSwitch): indexing toward ref, indexed by and started by the
// indexer, with the target's table ensured.
func (f *memoryIndexDBFixture) startSwitch(ref, indexer string) MemoryIndexRun {
	f.t.Helper()
	run := MemoryIndexRun{Kind: memoryIndexKindSwitch, State: memoryIndexStateIndexing, TargetRef: ref, TargetDimensions: f.widths[ref],
		IndexedBy: indexer, StartedBy: indexer, StartedAt: time.Now().UTC().Truncate(time.Microsecond), Cursor: map[string]string{}}
	_, version, _, err := f.a.readMemoryIndexRun(f.ctx, memoryIndexKindSwitch)
	require.NoError(f.t, err)
	require.NoError(f.t, f.a.writeMemoryIndexRun(f.ctx, run, version))
	_, err = EnsureEmbeddingVectorTable(f.ctx, f.db.DB, ref, f.widths[ref])
	require.NoError(f.t, err)
	return run
}

func (f *memoryIndexDBFixture) tick(e *MemQLEngine, budget time.Duration) {
	f.t.Helper()
	require.NoError(f.t, e.memoryIndexStep(f.ctx, budget))
}

// settle ticks with a tiny budget, one step a tick, until the switch is done.
func (f *memoryIndexDBFixture) settle(e *MemQLEngine) {
	f.t.Helper()
	for i := 0; i < 40; i++ {
		if f.run(memoryIndexKindSwitch).State == memoryIndexStateDone {
			return
		}
		f.tick(e, time.Nanosecond)
	}
	f.t.Fatalf("the switch did not finish: %+v", f.run(memoryIndexKindSwitch))
}

func (f *memoryIndexDBFixture) run(kind string) MemoryIndexRun {
	f.t.Helper()
	run, _, ok, err := f.a.readMemoryIndexRun(f.ctx, kind)
	require.NoError(f.t, err)
	require.True(f.t, ok, "no %s run", kind)
	return run
}

func (f *memoryIndexDBFixture) binding() EmbedderBinding {
	f.t.Helper()
	b, err := f.a.ReadEmbedderBinding(f.ctx)
	require.NoError(f.t, err)
	return b
}

// present is which of this test's items a model's table holds.
func (f *memoryIndexDBFixture) present(ref string) map[string]bool {
	f.t.Helper()
	table, err := EmbeddingVectorTable(ref, f.widths[ref])
	require.NoError(f.t, err)
	rows, err := f.db.DB.QueryContext(f.ctx, `SELECT id FROM `+table+` WHERE concept = $1 AND vector_field = 'content'`, f.concept)
	require.NoError(f.t, err)
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		require.NoError(f.t, rows.Scan(&id))
		out[id] = true
	}
	require.NoError(f.t, rows.Err())
	return out
}

// jobStores counts the job's successful stores toward a model, by item.
func (f *memoryIndexDBFixture) jobStores(ref string) map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for _, c := range f.calls {
		if c.job && c.ok && c.provider == ref {
			out[c.nodeId]++
		}
	}
	return out
}

func (f *memoryIndexDBFixture) callsBy(engine string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.engine == engine {
			n++
		}
	}
	return n
}

func (f *memoryIndexDBFixture) setOffline(ref string, offline bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offline[ref] = offline
}

// setOwners replaces who the owners are and makes both replicas read it again.
func (f *memoryIndexDBFixture) setOwners(owners ...string) {
	f.mu.Lock()
	f.owners = owners
	f.mu.Unlock()
	for _, e := range []*MemQLEngine{f.a, f.b} {
		e.indexerOwners.mu.Lock()
		e.indexerOwners.owners = nil
		e.indexerOwners.mu.Unlock()
	}
}

// search is similarTo's table choice, through engine b's builtin dispatch: the
// active binding names the model, the query is embedded at its width, and that
// model's table answers.
func (f *memoryIndexDBFixture) search(e *MemQLEngine) (string, map[string]bool) {
	f.t.Helper()
	e.builtinExecutorHandlers["integration.similarity.similarTo"] = func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		provider, err := ResolveEmbedderProvider(ctx)
		if err != nil {
			return nil, err
		}
		table, err := EmbeddingVectorTable(provider, f.widths[provider])
		if err != nil {
			return nil, err
		}
		// The distance is computed for every row, so a query embedded at
		// another width than the table's fails here; the rows are read in
		// id order rather than through the HNSW index, whose approximate
		// search may skip rows among this test's near-identical vectors.
		rows, err := e.database().DB.QueryContext(ctx, `SELECT id, embedding <=> $2::vector FROM `+table+`
			WHERE concept = $1 AND vector_field = 'content' ORDER BY id`, args["concept"], memoryIndexVector("query", f.widths[provider]))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []memorynodes.MemoryNode
		for rows.Next() {
			var id string
			var distance float64
			if err := rows.Scan(&id, &distance); err != nil {
				return nil, err
			}
			raw, _ := json.Marshal(map[string]any{"hit": id, "table": table})
			out = append(out, memorynodes.MemoryNode{ID: id, Payload: raw})
		}
		return out, rows.Err()
	}
	// A fresh read: the same call before and after the flip must not be
	// answered from a result cache.
	rows, err := e.memoryCall(ContextWithFreshRead(f.ctx), "builtin", "common.similarTo", map[string]any{"text": "a note", "concept": f.concept, "limit": 200})
	require.NoError(f.t, err)
	table, hits := "", map[string]bool{}
	for _, row := range rows {
		table, _ = row["table"].(string)
		hit, _ := row["hit"].(string)
		hits[hit] = true
	}
	return table, hits
}

func (f *memoryIndexDBFixture) tableOf(ref string) string {
	table, err := EmbeddingVectorTable(ref, f.widths[ref])
	require.NoError(f.t, err)
	return table
}

// TestTheRowSourceWalkReadsTheLatestVersionOnPostgres holds memorySourceItems'
// statement to Postgres (it had never run against one): the newest version of
// each row, a deleted row and a blank one skipped, a row whose payload does not
// decode reported as a failure, the cursor resumed after, and paging that ends.
func TestTheRowSourceWalkReadsTheLatestVersionOnPostgres(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	concept := fmt.Sprintf("v1:memidxwalk%d:item", os.Getpid())
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept=$1`, concept)
	})
	base := time.Now().UTC().Add(-time.Hour)
	insert := func(name string, at time.Duration, payload string) {
		t.Helper()
		row := []*memorynodes.MemoryNode{{ID: concept + ":" + name, Concept: concept, CreatedBy: "walk-test", CreatedAt: base.Add(at),
			Payload: json.RawMessage(payload), Provenance: json.RawMessage(`{"kind":"direct","name":"walk-test"}`)}}
		_, err := db.NewInsert().Model(&row).Exec(ctx)
		require.NoError(t, err)
	}
	insert("a", 0, `{"text":"old a"}`)
	insert("a", time.Second, `{"text":"new a"}`)
	insert("b", 0, `{"text":"b"}`)
	insert("b", time.Second, `{"id":"`+concept+`:b","deleted":true}`)
	insert("c", 0, `{"text":"  "}`)
	insert("d", 0, `"not a row"`)
	insert("e", 0, `{"text":"e"}`)
	insert("f", 0, `{"text":"f"}`)
	source := MemorySource{Name: "walkProbe", Concept: concept, VectorField: "content", Rows: func(p map[string]any) (string, bool) {
		text, _ := p["text"].(string)
		return text, strings.TrimSpace(text) != ""
	}}
	items, next, err := eng.memorySourceItems(ctx, source, "", 2)
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, MemoryItem{Id: concept + ":a", Text: "new a"}, items[0], "the newest version of a row, once")
	require.Equal(t, concept+":d", items[1].Id)
	require.Contains(t, items[1].Failure, "its stored payload does not decode")
	require.Empty(t, items[1].Text)
	require.Equal(t, concept+":d", next)

	items, next, err = eng.memorySourceItems(ctx, source, next, 2)
	require.NoError(t, err)
	require.Equal(t, []MemoryItem{{Id: concept + ":e", Text: "e"}, {Id: concept + ":f", Text: "f"}}, items)
	items, next, err = eng.memorySourceItems(ctx, source, next, 2)
	require.NoError(t, err)
	require.Empty(t, items)
	require.Empty(t, next, "the walk ends")
}

// A switch rebuilds every item and flips only when every one is in. An item
// that keeps failing holds it for the owner; the same switch started again
// resumes, embedding only what is missing, and then flips.
func TestASwitchFlipsOnlyWhenEveryItemIsIndexed(t *testing.T) {
	f := newMemoryIndexDBFixture(t)
	oldRef, newRef := f.ref("old", 8), f.ref("new", 8)
	f.bind(oldRef, f.owner)
	ids := f.addItems(memoryIndexNames("item", 6)...)
	f.write(f.a, ids...)
	f.mu.Lock()
	f.failing[ids[2]] = true
	f.mu.Unlock()
	f.startSwitch(newRef, f.owner)

	f.tick(f.a, 45*time.Second)
	run := f.run(memoryIndexKindSwitch)
	require.Equal(t, memoryIndexStateNeedsDecision, run.State)
	require.Equal(t, 1, run.Counts.Failed)
	require.Equal(t, oldRef, f.binding().ProviderRef, "nothing flips while an item is missing")
	require.Len(t, f.present(newRef), 5)
	require.Equal(t, f.tableOf(newRef), run.TargetTable)

	// The item can be embedded again; the owner starts the same switch again.
	f.mu.Lock()
	f.failing = map[string]bool{}
	f.mu.Unlock()
	before := f.jobStores(newRef)
	f.startSwitch(newRef, f.owner)
	f.tick(f.b, 45*time.Second)
	run = f.run(memoryIndexKindSwitch)
	require.Equal(t, memoryIndexStateDone, run.State)
	require.Equal(t, oldRef, run.PreviousRef)
	after := f.jobStores(newRef)
	require.Equal(t, before[ids[2]]+1, after[ids[2]], "the missing item is embedded")
	for _, id := range ids {
		if id != ids[2] {
			require.Equal(t, before[id], after[id], "%s was already in and is not embedded again", id)
		}
	}
	bound := f.binding()
	require.Equal(t, newRef, bound.ProviderRef)
	require.Equal(t, f.owner, bound.IndexedBy)
	require.Equal(t, f.owner, bound.ChosenBy)
	require.Equal(t, oldRef, bound.PreviousRef)
	require.Empty(t, bound.ReembedRunId)
	// The model it replaced is kept for thirty days, at the id its table derives.
	var retiredRef, retireAt string
	require.NoError(t, f.db.DB.QueryRowContext(f.ctx, `SELECT payload->>'providerRef', payload->>'retireAt' FROM "MemoryNodes" WHERE id = $1 ORDER BY "createdAt" DESC LIMIT 1`,
		EmbedderBindingConceptID+":"+RetiredEmbedderBindingID(oldRef, 8)).Scan(&retiredRef, &retireAt))
	require.Equal(t, oldRef, retiredRef)
	at, err := time.Parse(time.RFC3339Nano, retireAt)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(memoryIndexRetention), at, time.Hour)
}

// Search reads the old model's table until the flip, and the new one's after.
func TestSearchUsesTheOldIndexUntilTheFlip(t *testing.T) {
	f := newMemoryIndexDBFixture(t)
	oldRef, newRef := f.ref("old", 8), f.ref("new", 8)
	f.bind(oldRef, f.owner)
	ids := f.addItems(memoryIndexNames("item", 40)...)
	f.write(f.a, ids...)
	f.startSwitch(newRef, f.owner)

	f.tick(f.a, time.Nanosecond) // one batch of 32
	require.Equal(t, memoryIndexStateIndexing, f.run(memoryIndexKindSwitch).State)
	require.Len(t, f.present(newRef), 32, "the new index is half built")
	table, hits := f.search(f.b)
	require.Equal(t, f.tableOf(oldRef), table, "search keeps the active model while the rebuild runs")
	require.Len(t, hits, 40)

	f.settle(f.a)
	table, hits = f.search(f.b)
	require.Equal(t, f.tableOf(newRef), table, "after the flip search reads the new index")
	require.Len(t, hits, 40)
}

// An interrupted switch resumes on another replica from the saved cursor, and
// no item is embedded twice.
func TestAnInterruptedSwitchResumesOnAnotherReplica(t *testing.T) {
	f := newMemoryIndexDBFixture(t)
	oldRef, newRef := f.ref("old", 8), f.ref("new", 8)
	f.bind(oldRef, f.owner)
	ids := f.addItems(memoryIndexNames("item", 40)...)
	f.write(f.a, ids...)
	f.startSwitch(newRef, f.owner)

	f.tick(f.a, time.Nanosecond)
	run := f.run(memoryIndexKindSwitch)
	require.Equal(t, map[string]string{"testItems": ids[31]}, run.Cursor)
	require.Equal(t, MemoryIndexCounts{Total: 32, Done: 32}, run.Counts)

	f.tick(f.b, 45*time.Second)
	require.Equal(t, memoryIndexStateDone, f.run(memoryIndexKindSwitch).State)
	stores := f.jobStores(newRef)
	require.Len(t, stores, 40)
	for _, id := range ids {
		require.Equal(t, 1, stores[id], "%s embedded once", id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	byEngine := map[string]int{}
	for _, c := range f.calls {
		if c.job && c.provider == newRef {
			byEngine[c.engine]++
			require.Equal(t, f.owner, c.indexer, "embedded as the indexer")
		}
	}
	require.Equal(t, map[string]int{"a": 32, "b": 8}, byEngine)
}

// An item written during the rebuild, behind the walk's cursor, is caught by
// the catch-up walk; one written after the last walk and before the flip goes
// to the old index and is caught by the walk after the flip.
func TestItemsWrittenDuringTheSwitchReachTheNewIndex(t *testing.T) {
	f := newMemoryIndexDBFixture(t)
	oldRef, newRef := f.ref("old", 8), f.ref("new", 8)
	f.bind(oldRef, f.owner)
	ids := f.addItems(memoryIndexNames("item", 40)...)
	f.write(f.a, ids...)
	f.startSwitch(newRef, f.owner)

	f.tick(f.a, time.Nanosecond)
	early := f.addItems("item-000-early")[0] // sorts before the cursor
	f.write(f.b, early)
	require.True(t, f.present(oldRef)[early], "written to the active model")

	for i := 0; i < 20 && f.run(memoryIndexKindSwitch).State != memoryIndexStateSwitching; i++ {
		f.tick(f.a, time.Nanosecond)
	}
	require.Equal(t, memoryIndexStateSwitching, f.run(memoryIndexKindSwitch).State)
	require.True(t, f.present(newRef)[early], "the catch-up walk caught it")
	require.Equal(t, oldRef, f.binding().ProviderRef)

	late := f.addItems("item-000-late")[0]
	f.write(f.b, late) // after the last walk, before the flip
	require.True(t, f.present(oldRef)[late])
	require.False(t, f.present(newRef)[late])

	f.settle(f.b)
	require.Equal(t, newRef, f.binding().ProviderRef)
	present := f.present(newRef)
	require.True(t, present[late], "the walk after the flip caught it")
	require.Len(t, present, 42)
}

// An item an everyday writer could not embed (the indexer's machines offline)
// is caught up by the next backfill; nothing is counted failed while it waits.
func TestAnItemMissedWhileTheIndexerWasOfflineIsBackfilled(t *testing.T) {
	f := newMemoryIndexDBFixture(t)
	ref := f.ref("active", 8)
	f.bind(ref, f.owner)
	ids := f.addItems("first", "second", "missed")
	f.write(f.a, ids[0], ids[1]) // the third was written while every machine was off
	f.setOffline(ref, true)

	f.tick(f.a, 45*time.Second)
	run := f.run(memoryIndexKindBackfill)
	require.Equal(t, memoryIndexStateWaiting, run.State)
	require.Equal(t, "none of the indexer's machines offering memidx-active-"+f.token+" is online", run.WaitingReason)
	require.Zero(t, run.Counts.Failed)
	require.False(t, f.present(ref)[ids[2]])

	f.setOffline(ref, false)
	f.tick(f.b, 45*time.Second)
	run = f.run(memoryIndexKindBackfill)
	require.Equal(t, memoryIndexStateDone, run.State)
	require.True(t, f.present(ref)[ids[2]], "backfilled")
	require.Equal(t, MemoryIndexCounts{Total: 3, Done: 1, Skipped: 2}, run.Counts)
	require.Equal(t, map[string]int{ids[2]: 1}, f.jobStores(ref))

	// Inside ten minutes the backfill does not walk again.
	calls := f.callsBy("a") + f.callsBy("b")
	f.tick(f.a, 45*time.Second)
	require.Equal(t, calls, f.callsBy("a")+f.callsBy("b"))
}

// Two replicas tick at once: one takes the lock and works, the other returns
// without touching anything.
func TestOneReplicaRunsTheJobAtATime(t *testing.T) {
	f := newMemoryIndexDBFixture(t)
	oldRef, newRef := f.ref("old", 8), f.ref("new", 8)
	f.bind(oldRef, f.owner)
	ids := f.addItems(memoryIndexNames("item", 3)...)
	f.write(f.a, ids...)
	f.startSwitch(newRef, f.owner)
	release := make(chan struct{})
	f.mu.Lock()
	f.block = release
	f.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- f.a.memoryIndexStep(f.ctx, 45*time.Second) }()
	select {
	case <-f.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("replica a never reached the store")
	}
	before := f.callsBy("b")
	started := time.Now()
	require.NoError(t, f.b.memoryIndexStep(f.ctx, 45*time.Second))
	require.Less(t, time.Since(started), 10*time.Second, "the second replica does not wait for the first")
	require.Equal(t, before, f.callsBy("b"), "the second replica embedded nothing")
	require.Equal(t, memoryIndexStateIndexing, f.run(memoryIndexKindSwitch).State, "and wrote nothing")
	close(release)
	require.NoError(t, <-done)
	require.Equal(t, memoryIndexStateDone, f.run(memoryIndexKindSwitch).State)
}

// A width change, 1024 to 768: writers and readers use the 1024 table until
// the flip, and the 768 table after it; the job fills the 768 table all along.
func TestAWidthChangeMovesTablesOnlyAtTheFlip(t *testing.T) {
	f := newMemoryIndexDBFixture(t)
	oldRef, newRef := f.ref("wide", 1024), f.ref("narrow", 768)
	f.bind(oldRef, f.owner)
	ids := f.addItems(memoryIndexNames("item", 34)...)
	f.write(f.a, ids...)
	run := f.startSwitch(newRef, f.owner)
	require.Equal(t, 768, run.TargetDimensions)

	f.tick(f.a, time.Nanosecond)
	require.Equal(t, f.tableOf(newRef), f.run(memoryIndexKindSwitch).TargetTable)
	during := f.addItems("item-during")[0]
	f.write(f.b, during)
	require.True(t, f.present(oldRef)[during], "a writer still writes the 1024 table")
	require.False(t, f.present(newRef)[during])
	table, _ := f.search(f.b)
	require.Equal(t, f.tableOf(oldRef), table, "a reader still reads the 1024 table")

	f.settle(f.a)
	bound := f.binding()
	require.Equal(t, newRef, bound.ProviderRef)
	require.Equal(t, 768, bound.Dimensions)
	after := f.addItems("item-after")[0]
	f.write(f.b, after)
	require.True(t, f.present(newRef)[after], "after the flip a writer writes the 768 table")
	require.False(t, f.present(oldRef)[after])
	table, hits := f.search(f.b)
	require.Equal(t, f.tableOf(newRef), table)
	require.Len(t, hits, 36)
}

// An indexer who is no longer an active owner makes the run wait with a plain
// reason, and nothing is embedded on their machines; an owner again, it goes on.
func TestAnIndexerWhoIsNoLongerAnOwnerMakesTheRunWait(t *testing.T) {
	f := newMemoryIndexDBFixture(t)
	oldRef, newRef := f.ref("old", 8), f.ref("new", 8)
	f.bind(oldRef, f.owner)
	ids := f.addItems(memoryIndexNames("item", 3)...)
	f.write(f.a, ids...)
	f.startSwitch(newRef, f.owner)
	f.setOwners()

	before := f.callsBy("a") + f.callsBy("b")
	f.tick(f.a, 45*time.Second)
	run := f.run(memoryIndexKindSwitch)
	require.Equal(t, memoryIndexStateWaiting, run.State)
	require.Equal(t, "the owner who chose this model is no longer an owner; an owner can choose again", run.WaitingReason)
	require.Equal(t, before, f.callsBy("a")+f.callsBy("b"), "nothing is embedded")
	require.Equal(t, oldRef, f.binding().ProviderRef)

	f.setOwners(f.owner)
	f.tick(f.b, 45*time.Second)
	require.Equal(t, memoryIndexStateDone, f.run(memoryIndexKindSwitch).State)
	require.Equal(t, newRef, f.binding().ProviderRef)
}
