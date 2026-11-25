package memql

// memory_index_world.go -- the memory index job's world over Postgres: the
// step builtin its schedule calls, the one-runner lock, and
// engineMemoryIndexWorld, which reads the binding, the run rows, the sources
// and the vector tables, and embeds through the store path every writer uses.
// The job's decisions, and why each is made, are memory_index_job.go's (its
// header); the flip is memory_index_flip.go's, and the run rows as stored
// memory_index_run.go's.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/lib/pq"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/num"
)

// memoryIndexStepBuiltin is builtin memoryIndexStep: one tick of the job, for
// the shipped schedule (memoryIndexTick) and nothing else.
//
// THE GATE IS HERE, because a builtin takes no @serverOnly and any signed-in
// client's query can name one: a call without internal origin -- which the
// automation executor stamps on a tree-loaded automation's step and on nothing
// a client sends -- is refused before anything is read. The step takes no
// argument but its budget, which is bounded here.
func (e *MemQLEngine) memoryIndexStepBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return nil, fmt.Errorf("memoryIndexStep is the memory index job's own step: only its schedule runs it")
	}
	budget := time.Duration(workHistoryInt(args, "budgetSeconds", int(memoryIndexDefaultBudget/time.Second))) * time.Second
	budget = max(time.Second, min(budget, memoryIndexMaxBudget))
	return nil, e.memoryIndexStep(ctx, budget)
}

// memoryIndexStep is one bounded tick of the memory index job: it takes the
// job's lock or returns at once, and takes no step past budget.
//
// IT REPLACES THE CALLER'S ACTOR with the job's own (`system:memoryIndex`, the
// deployment acting): the job reads and writes on the cluster's behalf, so
// every row it writes says the job wrote it, and a vendor model's embedding is
// the cluster's own work (auth.ActsForTheCluster). Only the embedding step
// changes hands, to the indexer (ContextWithEmbeddingIndexer, in embed).
func (e *MemQLEngine) memoryIndexStep(ctx context.Context, budget time.Duration) error {
	ctx = auth.ContextWithSystemActor(ctx, memoryIndexActorName)
	release, held, err := e.tryMemoryIndexLock(ctx)
	if err != nil {
		if IsWriteGateBusy(err) {
			return nil // every gate connection is held: nothing attempted, the next tick tries
		}
		return err
	}
	if !held {
		return nil // another replica is running the job
	}
	defer release()
	return runMemoryIndexTick(ctx, &engineMemoryIndexWorld{e: e, versions: map[string]time.Time{}}, budget)
}

// memoryIndexLockSQL takes the job's one-runner lock, without waiting, for the
// transaction it runs in (memory_index_job.go's header).
const memoryIndexLockSQL = `SELECT pg_try_advisory_xact_lock(hashtextextended('memql:memory-index', 0))`

// tryMemoryIndexLock takes the job's one-runner lock without waiting, in a
// transaction on a write-gate connection held until release
// (memory_index_job.go's header).
func (e *MemQLEngine) tryMemoryIndexLock(ctx context.Context) (func(), bool, error) {
	db, err := ResolveWriteGatePool(e.askCoordinationPool, "the memory index job", "wire SetAskCoordinationPool with database.Database.GateDB")
	if err != nil {
		return nil, false, err
	}
	acquire, cancel := context.WithTimeout(ctx, writeGateWait)
	defer cancel()
	if err := checkGatePool(acquire, db); err != nil {
		return nil, false, gateWaitError(err, acquire)
	}
	conn, err := db.Conn(acquire)
	if err != nil {
		return nil, false, gateWaitError(err, acquire)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = conn.Close()
		return nil, false, err
	}
	var held bool
	if err := tx.QueryRowContext(acquire, memoryIndexLockSQL).Scan(&held); err != nil || !held {
		_ = tx.Rollback()
		_ = conn.Close()
		return nil, false, err
	}
	return func() {
		_ = tx.Rollback()
		_ = conn.Close()
	}, true, nil
}

// engineMemoryIndexWorld is the job's world over Postgres. It remembers, per
// run, the version it last read or wrote: the witness the next write is fenced
// on.
type engineMemoryIndexWorld struct {
	e        *MemQLEngine
	versions map[string]time.Time
}

func (w *engineMemoryIndexWorld) activeBinding(ctx context.Context) (EmbedderBinding, error) {
	return w.e.ReadEmbedderBinding(ctx)
}

func (w *engineMemoryIndexWorld) readRun(ctx context.Context, kind string) (MemoryIndexRun, bool, error) {
	run, version, ok, err := w.e.readMemoryIndexRun(ctx, kind)
	if err != nil {
		return MemoryIndexRun{}, false, err
	}
	w.versions[kind] = version
	return run, ok, nil
}

// saveRun writes the run fenced to the version this tick last read or wrote,
// and keeps THE VERSION OF ITS OWN WRITE, as writeMemoryIndexRun answers it, as
// the next write's witness.
//
// Never whatever a read after the write returns: a write by anybody else
// landing between this write and that read -- an owner's Cancel on the bff, a
// second tick after a lost lock -- would become the witness, the job's next
// batch would pass the fence, and the job would write `indexing` over it, which
// is the revert the fence exists to prevent. With its own version as the
// witness, the job's next write is refused instead (errMemoryIndexRunChanged)
// and the tick stops.
func (w *engineMemoryIndexWorld) saveRun(ctx context.Context, run MemoryIndexRun) error {
	version, err := w.e.writeMemoryIndexRun(ctx, run, w.versions[run.Kind])
	if err != nil {
		return err
	}
	w.versions[run.Kind] = version
	if w.e.memoryIndexAfterRunWrite != nil {
		w.e.memoryIndexAfterRunWrite(run)
	}
	return nil
}

func (w *engineMemoryIndexWorld) isActiveOwner(ctx context.Context, userId string) bool {
	return w.e.IndexerIsActiveOwner(ctx, userId)
}

func (w *engineMemoryIndexWorld) sources() []MemorySource {
	if w.e.memoryIndexSources != nil {
		return w.e.memoryIndexSources()
	}
	return MemorySources()
}

func (w *engineMemoryIndexWorld) items(ctx context.Context, s MemorySource, after string, limit int) ([]MemoryItem, string, error) {
	return w.e.memorySourceItems(ctx, s, after, limit)
}

func (w *engineMemoryIndexWorld) ensureTable(ctx context.Context, ref string, dims int) (string, error) {
	db := w.e.database()
	if db == nil {
		return "", fmt.Errorf("memory index: no database")
	}
	return EnsureEmbeddingVectorTable(ctx, db.DB, ref, dims)
}

// vectorTimes reads, for each of ids the table holds a vector of for one
// field, when that vector was last written (updated_at, which every upsert
// sets). The table name comes from EmbeddingVectorTable, never from a row, and
// is checked again before it enters the statement.
func (w *engineMemoryIndexWorld) vectorTimes(ctx context.Context, table, vectorField string, ids []string) (map[string]time.Time, error) {
	db := w.e.database()
	if db == nil || !embeddingTableName.MatchString(table) {
		return nil, fmt.Errorf("memory index: cannot read vector table %q", table)
	}
	rows, err := db.DB.QueryContext(ctx, `SELECT id, updated_at FROM `+table+` WHERE vector_field = $1 AND id = ANY($2::text[])`, vectorField, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = at.UTC()
	}
	return out, rows.Err()
}

// embed writes one item through the store path every writer uses
// (integration.embedding.store: embed, ensure the table, upsert on (id,
// vector_field)), naming the run's model explicitly, so the item lands in the
// TARGET table while search still reads the active one. A fleet model is
// embedded as the run's indexer: their machines and the ones lent to them,
// whoever wrote the item.
func (w *engineMemoryIndexWorld) embed(ctx context.Context, run MemoryIndexRun, s MemorySource, item MemoryItem) error {
	if w.e.integrations == nil {
		return errMemoryIndexNoWriter
	}
	store, ok := w.e.integrations.Get("integration.embedding.store")
	if !ok {
		return errMemoryIndexNoWriter
	}
	if indexer, fleet := IndexerFor(EmbedderBinding{ProviderRef: run.TargetRef, IndexedBy: run.IndexedBy}); fleet {
		ctx = ContextWithEmbeddingIndexer(ctx, indexer)
	}
	nodes, err := store(ctx, map[string]any{
		"nodeId": item.Id, "text": item.Text, "concept": s.Concept, "vectorField": s.VectorField, "provider": run.TargetRef,
	}, 1)
	if err != nil {
		return err
	}
	// The store writes to the table of the width the model ANSWERED. A model
	// answering another width than the switch recorded wrote elsewhere, and
	// the item is still missing from the target: a failure, said plainly.
	for _, n := range nodes {
		var stored struct {
			Dimensions int `json:"dimensions"`
		}
		if json.Unmarshal(n.Payload, &stored) == nil && stored.Dimensions > 0 && stored.Dimensions != run.TargetDimensions {
			return fmt.Errorf("the model answered %d dimensions where this index holds %d", stored.Dimensions, run.TargetDimensions)
		}
	}
	return nil
}

// unsourced counts the table's entries whose (concept, field) no source
// rebuilds. The pairs are joined with a unit separator, which no concept name
// or field carries.
func (w *engineMemoryIndexWorld) unsourced(ctx context.Context, table string, sources []MemorySource) (int, error) {
	db := w.e.database()
	if db == nil || !embeddingTableName.MatchString(table) {
		return 0, fmt.Errorf("memory index: cannot read vector table %q", table)
	}
	pairs := make([]string, 0, len(sources))
	for _, s := range sources {
		pairs = append(pairs, s.Concept+"\x1f"+s.VectorField)
	}
	var n int64
	err := db.DB.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE (concept || chr(31) || vector_field) <> ALL($1::text[])`, pq.Array(pairs)).Scan(&n)
	return num.ClampInt64(n), err
}

// flip runs flipMemoryIndex and then reads the run back. The read takes the
// row's payload AND its version together, so the tick goes on with whatever
// the row now is -- the `finishing` it wrote, or a later write by somebody else,
// which the tick then obeys -- never its own idea of the row under another
// writer's version (saveRun).
func (w *engineMemoryIndexWorld) flip(ctx context.Context, run MemoryIndexRun) (MemoryIndexRun, error) {
	if err := w.e.flipMemoryIndex(ctx, run); err != nil {
		return MemoryIndexRun{}, err
	}
	flipped, ok, err := w.readRun(ctx, run.Kind)
	if err != nil {
		return MemoryIndexRun{}, err
	}
	if !ok {
		return MemoryIndexRun{}, errMemoryIndexRunChanged
	}
	return flipped, nil
}

func (w *engineMemoryIndexWorld) now() time.Time { return time.Now().UTC() }

func (w *engineMemoryIndexWorld) logger() *slog.Logger {
	if l := w.e.safeLogger(); l != nil {
		return l
	}
	return slog.Default()
}

func (e *MemQLEngine) logOrDefault() *slog.Logger {
	if l := e.safeLogger(); l != nil {
		return l
	}
	return slog.Default()
}
