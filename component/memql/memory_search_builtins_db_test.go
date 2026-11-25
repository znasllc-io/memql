package memql

// memory_search_builtins_db_test.go -- the owner's memory-search builtins and the
// daily cleanup against Postgres, on the index job's fixture: TWO ENGINES OVER
// ONE DATABASE, with the job's stub store path (memory_index_job_db_test.go).
// The decisions themselves are memory_search_builtins_test.go's; what is held
// here is what only a database shows: a retained table reused by a switch back,
// tables actually dropped, and the ones that never are.

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/airoute"
)

// memorySearchWidthProbe answers a readiness probe as a model of one width does.
type memorySearchWidthProbe struct{ width int }

func (p memorySearchWidthProbe) Embed(context.Context, string) ([]float32, error) {
	v := make([]float32, p.width)
	v[0] = 1
	return v, nil
}
func (memorySearchWidthProbe) EmbedBatch(context.Context, []string) ([][]float32, error) {
	return nil, nil
}
func (memorySearchWidthProbe) Dimensions() int                           { return 0 }
func (memorySearchWidthProbe) Call(context.Context, string) (any, error) { return nil, nil }

// probeModels lets both engines check a model of this test: each answers at
// the width the test gave it.
func (f *memoryIndexDBFixture) probeModels() {
	for _, e := range []*MemQLEngine{f.a, f.b} {
		e.SetAIResolver(testAIResolver{fn: func(_ context.Context, req airoute.ResolveRequest) (ResolvedProvider, error) {
			return ResolvedProvider{Client: memorySearchWidthProbe{width: f.widths[req.ExplicitProvider]}}, nil
		}})
	}
}

// ownerCall is the fixture's owner, signed in.
func (f *memoryIndexDBFixture) ownerCall() context.Context {
	return auth.ContextWithAccess(f.ctx, &auth.AccessContext{UserId: f.owner, Role: auth.RoleOwner})
}

// ownerRow writes the fixture's owner as a v1:identity:user row with a display
// name, so the status can name whose machines index.
func (f *memoryIndexDBFixture) ownerRow(name string) {
	f.t.Helper()
	marker := "memory-search-test:" + f.token
	payload, _ := json.Marshal(map[string]any{"id": f.owner, "displayName": name, "primaryEmail": "owner-" + f.token + "@example.invalid",
		"role": "owner", "active": true})
	rows := []*memorynodes.MemoryNode{{ID: f.owner, Concept: "v1:identity:user", CreatedBy: marker, CreatedAt: time.Now().UTC(),
		Payload: payload, Provenance: json.RawMessage(`{"kind":"direct","name":"memory-search-test"}`)}}
	_, err := f.db.NewInsert().Model(&rows).Exec(f.ctx)
	require.NoError(f.t, err)
	f.t.Cleanup(func() {
		_, _ = f.db.DB.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept='v1:identity:user' AND "createdBy"=$1`, marker)
	})
}

// plantVersion writes a newer version of a row the way a replica whose clock
// runs ahead of this process's would: its newest payload (an empty one when the
// row has none), changed by edit, stamped at.
func (f *memoryIndexDBFixture) plantVersion(concept, id string, at time.Time, edit func(map[string]any)) {
	f.t.Helper()
	payload := map[string]any{}
	var raw []byte
	err := f.db.DB.QueryRowContext(f.ctx, `SELECT payload FROM "MemoryNodes" WHERE id = $1 ORDER BY "createdAt" DESC LIMIT 1`, id).Scan(&raw)
	if err == nil {
		require.NoError(f.t, json.Unmarshal(raw, &payload))
	} else {
		require.ErrorIs(f.t, err, sql.ErrNoRows)
	}
	if edit != nil {
		edit(payload)
	}
	body, err := json.Marshal(payload)
	require.NoError(f.t, err)
	rows := []*memorynodes.MemoryNode{{ID: id, Concept: concept, CreatedBy: "memory-search-test:" + f.token, CreatedAt: at.UTC(), Payload: body,
		Provenance: json.RawMessage(`{"kind":"direct","name":"memory-search-test"}`)}}
	_, err = f.db.NewInsert().Model(&rows).Exec(f.ctx)
	require.NoError(f.t, err)
}

// retire writes a retired model's row the way a flip or a cancel does, dated at.
func (f *memoryIndexDBFixture) retire(ref string, at time.Time) {
	f.t.Helper()
	_, err := EnsureEmbeddingVectorTable(f.ctx, f.db.DB, ref, f.widths[ref])
	require.NoError(f.t, err)
	call, err := parser.RenderCall("platform.recordRetiredEmbedder", map[string]any{"bindingId": RetiredEmbedderBindingID(ref, f.widths[ref]),
		"providerRef": ref, "dimensions": f.widths[ref], "retireAt": at.UTC().Format(time.RFC3339Nano),
		"versionTime": time.Now().UTC().Format(time.RFC3339Nano)})
	require.NoError(f.t, err)
	_, err = f.a.Execute(auth.ContextWithInternalOrigin(f.ctx), "mutation "+call)
	require.NoError(f.t, err)
}

// retiredRow is a retired model's row as stored: its payload and its newest
// version.
func (f *memoryIndexDBFixture) retiredRow(ref string) (map[string]any, time.Time) {
	f.t.Helper()
	var raw []byte
	var at time.Time
	require.NoError(f.t, f.db.DB.QueryRowContext(f.ctx, `SELECT payload, "createdAt" FROM "MemoryNodes" WHERE id = $1 ORDER BY "createdAt" DESC LIMIT 1`,
		EmbedderBindingConceptID+":"+RetiredEmbedderBindingID(ref, f.widths[ref])).Scan(&raw, &at))
	var payload map[string]any
	require.NoError(f.t, json.Unmarshal(raw, &payload))
	return payload, at
}

func (f *memoryIndexDBFixture) tableExists(ref string) bool {
	f.t.Helper()
	var exists bool
	require.NoError(f.t, f.db.DB.QueryRowContext(f.ctx, `SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = current_schema() AND tablename = $1)`,
		f.tableOf(ref)).Scan(&exists))
	return exists
}

// sweep runs the daily cleanup as its schedule does.
func (f *memoryIndexDBFixture) sweep(e *MemQLEngine) {
	f.t.Helper()
	_, err := e.memoryIndexRetireBuiltin(auth.ContextWithInternalOrigin(f.ctx), nil, 0)
	require.NoError(f.t, err)
}

func (f *memoryIndexDBFixture) memorySearchStatus(e *MemQLEngine) map[string]any {
	f.t.Helper()
	nodes, err := e.memorySearchStatusBuiltin(f.ownerCall(), nil, 0)
	require.NoError(f.t, err)
	require.Len(f.t, nodes, 1)
	require.Equal(f.t, "current", nodes[0].ID)
	var row map[string]any
	require.NoError(f.t, json.Unmarshal(nodes[0].Payload, &row))
	return row
}

// mayAct asks the gates an act passes, on a real engine: `execute` on the part
// through the capability check the builtin path makes, and the owner set. An
// owner holding the part may act; a caller whose role holds the part but who is
// not an active owner may not, and neither may an owner signed in under a role
// that does not hold it.
func TestTheStatusAsksAnActsOwnGatesWhetherTheCallerMayAct(t *testing.T) {
	f := newMemoryIndexDBFixture(t)
	f.bind(f.ref("model", 3), f.owner)
	mayAct := func(ac *auth.AccessContext) any {
		t.Helper()
		nodes, err := f.a.memorySearchStatusBuiltin(auth.ContextWithAccess(f.ctx, ac), nil, 0)
		require.NoError(t, err)
		require.Len(t, nodes, 1)
		var row map[string]any
		require.NoError(t, json.Unmarshal(nodes[0].Payload, &row))
		return row["mayAct"]
	}
	assert.Equal(t, true, mayAct(&auth.AccessContext{UserId: f.owner, Role: auth.RoleOwner}))
	assert.Equal(t, false, mayAct(&auth.AccessContext{UserId: "v1:identity:user:not-an-owner-" + f.token, Role: auth.RoleOwner}))
	assert.Equal(t, false, mayAct(&auth.AccessContext{UserId: f.owner, Role: auth.RoleDeveloper}))
}

// Switching back within the 30 days reuses the kept index: the job embeds only
// what was written since the switch away, and every item ends up in it.
func TestSwitchingBackReusesTheKeptIndex(t *testing.T) {
	f := newMemoryIndexDBFixture(t)
	f.probeModels()
	first, second := f.ref("first", 3), f.ref("second", 5)
	f.bind(first, f.owner)
	old := f.addItems("one", "two", "three", "four", "five")
	f.write(f.a, old...)

	_, err := f.a.memorySearchSwitchBuiltin(f.ownerCall(), map[string]any{"provider": second}, 0)
	require.NoError(t, err)
	run := f.run(memoryIndexKindSwitch)
	require.Equal(t, memoryIndexStateIndexing, run.State)
	require.Equal(t, f.owner, run.StartedBy)
	require.Equal(t, f.owner, run.IndexedBy)
	require.Equal(t, 5, run.TargetDimensions, "the width the model answered")
	f.tick(f.b, time.Minute)
	require.Equal(t, memoryIndexStateDone, f.run(memoryIndexKindSwitch).State, "one tick carries a small switch through")
	require.Equal(t, second, f.binding().ProviderRef)

	f.ownerRow("Ana Owner")
	status := f.memorySearchStatus(f.a)
	require.Equal(t, f.owner, status["indexedBy"])
	require.Equal(t, "Ana Owner", status["indexedByName"], "the status names whose machines index")
	retained, _ := status["retained"].([]any)
	require.Len(t, retained, 1, "the first model's index is kept")
	kept, _ := retained[0].(map[string]any)
	require.Equal(t, first, kept["providerRef"])
	until, err := time.Parse(time.RFC3339Nano, kept["retireAt"].(string))
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(30*24*time.Hour), until, time.Minute)

	fresh := f.addItems("six", "seven")
	f.write(f.a, fresh...)
	require.Empty(t, f.jobStores(first), "nothing was embedded into the kept index meanwhile")

	_, err = f.b.memorySearchSwitchBuiltin(f.ownerCall(), map[string]any{"provider": first}, 0)
	require.NoError(t, err)
	f.tick(f.a, time.Minute)
	require.Equal(t, memoryIndexStateDone, f.run(memoryIndexKindSwitch).State)
	require.Equal(t, first, f.binding().ProviderRef)
	require.Equal(t, map[string]int{fresh[0]: 1, fresh[1]: 1}, f.jobStores(first), "only the items written since are embedded")
	require.Len(t, f.present(first), 7)
}

// A retired index is dropped once its date has passed, and its row then says
// when; one still inside its 30 days is kept. A second sweep changes nothing.
func TestARetiredIndexIsDroppedAfterItsDate(t *testing.T) {
	f := newMemoryIndexDBFixture(t)
	active, expired, kept := f.ref("active", 3), f.ref("expired", 4), f.ref("kept", 5)
	f.bind(active, f.owner)
	f.retire(expired, time.Now().Add(-time.Hour))
	f.retire(kept, time.Now().Add(time.Hour))

	f.sweep(f.a)
	require.False(t, f.tableExists(expired), "past its date: dropped")
	require.True(t, f.tableExists(kept), "inside its 30 days: kept")
	require.True(t, f.tableExists(active))
	dropped, version := f.retiredRow(expired)
	require.NotEmpty(t, dropped["droppedAt"], "the row records the drop")
	require.NotEmpty(t, dropped["retireAt"], "and keeps its date")
	stillKept, _ := f.retiredRow(kept)
	require.Empty(t, stillKept["droppedAt"])

	f.sweep(f.b)
	_, again := f.retiredRow(expired)
	require.True(t, version.Equal(again), "a dropped index is not dropped again")
	retained, _ := f.memorySearchStatus(f.b)["retained"].([]any)
	require.Len(t, retained, 1)
	require.Equal(t, kept, retained[0].(map[string]any)["providerRef"])
}

// The active model's table and a running switch's target are never dropped,
// even when a dated retired row names them: a flip that stopped part way dates
// the active model before the binding moves, and a cancelled switch's target
// can be chosen again. Once the switch is over, its target is an ordinary
// retired index.
func TestTheActiveAndTargetTablesAreNeverDropped(t *testing.T) {
	f := newMemoryIndexDBFixture(t)
	active, target, other := f.ref("active", 3), f.ref("target", 4), f.ref("other", 5)
	f.bind(active, f.owner)
	f.startSwitch(target, f.owner)
	past := time.Now().Add(-time.Hour)
	f.retire(active, past)
	f.retire(target, past)
	f.retire(other, past)

	f.sweep(f.a)
	require.True(t, f.tableExists(active), "the active model's table is never dropped")
	require.True(t, f.tableExists(target), "a running switch's target is never dropped")
	require.False(t, f.tableExists(other), "the reachable positive: an ordinary retired index past its date")
	for _, ref := range []string{active, target} {
		row, _ := f.retiredRow(ref)
		require.Empty(t, row["droppedAt"], ref)
	}

	f.setRun(f.b, func(r *MemoryIndexRun) { r.State = memoryIndexStateCancelled })
	f.sweep(f.b)
	require.False(t, f.tableExists(target), "the switch is over: its target is an ordinary retired index")
	require.True(t, f.tableExists(active))
}

// Every write of the binding, the retired rows and the run rows is stamped
// after the version it read (ruling R12; memql#5431's third part), so a writer
// whose clock lags the last writer's still writes the CURRENT version rather
// than an older one that reads as lost. Here each last version was stamped an
// hour ahead of this process's clock, as a replica whose clock runs ahead
// leaves it.
func TestEveryWriteIsStampedAfterTheVersionItRead(t *testing.T) {
	f := newMemoryIndexDBFixture(t)
	f.probeModels()
	first, second := f.ref("first", 3), f.ref("second", 4)
	f.bind(first, f.owner)
	f.write(f.a, f.addItems("one", "two")...)
	ahead := time.Now().Add(time.Hour)
	retiredRow := func(ref string, dims int) string {
		return EmbedderBindingConceptID + ":" + RetiredEmbedderBindingID(ref, dims)
	}
	monthAhead := func(row map[string]any) time.Time {
		at, _ := time.Parse(time.RFC3339Nano, stringFromAny(row["retireAt"]))
		return at
	}

	// The binding: choosing the model in use records its chooser.
	f.plantVersion(EmbedderBindingConceptID, EmbedderBindingConceptID+":"+ActiveEmbedderBindingID, ahead,
		func(p map[string]any) { p["chosenBy"] = "" })
	_, err := f.a.memorySearchSwitchBuiltin(f.ownerCall(), map[string]any{"provider": first}, 0)
	require.NoError(t, err)
	assert.Equal(t, f.owner, f.binding().ChosenBy, "the binding: the chooser written after a version stamped ahead is current")

	// The run row and a retired row: Cancel dates the target's index for 30
	// days over an older retirement, and records the run cancelled.
	_, err = f.a.memorySearchSwitchBuiltin(f.ownerCall(), map[string]any{"provider": second}, 0)
	require.NoError(t, err)
	f.plantVersion(MemoryIndexRunConceptID, MemoryIndexRunConceptID+":"+memoryIndexKindSwitch, ahead, nil)
	f.plantVersion(EmbedderBindingConceptID, retiredRow(second, 4), ahead, func(p map[string]any) {
		p["providerRef"], p["dimensions"], p["retireAt"] = second, 4, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	})
	_, err = f.b.memorySearchCancelBuiltin(f.ownerCall(), nil, 0)
	require.NoError(t, err)
	assert.Equal(t, memoryIndexStateCancelled, f.run(memoryIndexKindSwitch).State, "the run row: the cancel is current")
	kept, _ := f.retiredRow(second)
	assert.WithinDuration(t, time.Now().Add(30*24*time.Hour), monthAhead(kept), time.Minute,
		"a retired row: the cancel's 30 days are current, not the older date")

	// A retired row written by the flip: the model it replaces, over an older
	// row that said its index was dropped.
	f.plantVersion(EmbedderBindingConceptID, retiredRow(first, 3), ahead, func(p map[string]any) {
		p["providerRef"], p["dimensions"] = first, 3
		p["retireAt"] = time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
		p["droppedAt"] = time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
	})
	_, err = f.a.memorySearchSwitchBuiltin(f.ownerCall(), map[string]any{"provider": second}, 0)
	require.NoError(t, err)
	f.tick(f.b, time.Minute)
	require.Equal(t, memoryIndexStateDone, f.run(memoryIndexKindSwitch).State)
	require.Equal(t, second, f.binding().ProviderRef)
	replaced, _ := f.retiredRow(first)
	assert.Empty(t, replaced["droppedAt"], "the flip's retired row: its retirement is current, not the older drop")
	assert.WithinDuration(t, time.Now().Add(30*24*time.Hour), monthAhead(replaced), time.Minute)
}

// A cluster's first model is bound by bindEmbedder, whose own writes -- the
// plan and the activation -- are stamped after the binding's newest version,
// as every other binding write is (ruling R12). Here a plan row was left an
// hour ahead of this process's clock, as a replica whose clock runs ahead
// leaves it: stamped at this clock, the activation would be an older version
// than that plan, and the binding would read as never activated.
func TestAFirstBindingIsStampedAfterTheVersionItRead(t *testing.T) {
	f := newMemoryIndexDBFixture(t)
	f.probeModels()
	ref := f.ref("first", 3)
	f.plantVersion(EmbedderBindingConceptID, EmbedderBindingConceptID+":"+ActiveEmbedderBindingID, time.Now().Add(time.Hour),
		func(p map[string]any) { p["providerRef"], p["dimensions"] = ref, 3 })

	_, err := f.a.memorySearchSwitchBuiltin(f.ownerCall(), map[string]any{"provider": ref}, 0)
	require.NoError(t, err)
	bound := f.binding()
	require.Equal(t, ref, bound.ProviderRef)
	require.NotEmpty(t, bound.ActivatedAt, "the activation is the current version")
	require.Equal(t, f.owner, bound.IndexedBy)
	require.Equal(t, f.owner, bound.ChosenBy)
}

// The switch asks the active index only whether it holds anything
// (hasVectors, an EXISTS): a table that does not exist and an empty one hold
// nothing, one entry is enough, and a name that is not a vector table's never
// reaches a statement.
func TestTheSwitchAsksTheActiveIndexOnlyWhetherItHoldsAnything(t *testing.T) {
	f := newMemoryIndexDBFixture(t)
	w := engineMemorySearchWorld{e: f.a}
	ref := f.ref("probe", 8)
	table, err := EmbeddingVectorTable(ref, 8)
	require.NoError(t, err)

	held, err := w.hasVectors(f.ctx, table)
	require.NoError(t, err)
	require.False(t, held, "a table that does not exist holds nothing")

	_, err = EnsureEmbeddingVectorTable(f.ctx, f.db.DB, ref, 8)
	require.NoError(t, err)
	held, err = w.hasVectors(f.ctx, table)
	require.NoError(t, err)
	require.False(t, held, "an empty table holds nothing")

	id := f.concept + ":one"
	_, err = f.db.DB.ExecContext(f.ctx, `INSERT INTO `+table+` (id, concept, vector_field, embedding) VALUES ($1, $2, 'content', $3::vector)`,
		id, f.concept, memoryIndexVector(id, 8))
	require.NoError(t, err)
	held, err = w.hasVectors(f.ctx, table)
	require.NoError(t, err)
	require.True(t, held, "one entry is enough")
	n, err := w.vectors(f.ctx, table)
	require.NoError(t, err)
	require.Equal(t, 1, n, "and the count agrees")

	_, err = w.hasVectors(f.ctx, "node_vectors_8; DROP TABLE x")
	require.ErrorContains(t, err, "cannot read vector table")
}
