package memql

// memory_index_job.go -- ONE job keeps memory search's index whole (design
// record docs/superpowers/specs/2026-11-23-memory-search-model-switch-design.md,
// sections 4, 6 and 9).
//
// # What it does
//
// The index stores vectors and no text, so a vector that is missing can only
// be made again from its row: the sources (memory_sources.go) rebuild each
// item's text by its writer's own rule. One scheduled tick (memoryIndexTick,
// dsl/platform/automations.memql, every minute) works on ONE run row of
// v1:platform:memoryIndexRun, at a literal id:
//
//   - `switch`, while an owner's switch is in progress. The tick walks every
//     source in name order and fills the TARGET model's table with what it is
//     missing, while search keeps reading the active model's table; then walks
//     again (the catch-up) for what arrived meanwhile; then flips the binding;
//     then walks the now-active table once more for what was written between
//     the last walk and the flip.
//   - otherwise `backfill`: the same walk over the ACTIVE table, at most once
//     every ten minutes. That is what catches up an item a writer could not
//     embed (its indexer's machines offline, the indexer demoted): the writers
//     log "until backfilled", and this is the backfill.
//
// A switch's states: indexing -> catchingUp -> switching -> finishing -> done.
// A catch-up walk that left an item failed, or found the active table holding
// items no source rebuilds, goes to needsDecision instead, and stays there: the
// owner decides, with memorySearchSwitchAnyway (which writes `switching`; this
// job flips) or memorySearchCancel (memory_search_builtins.go). Any walking
// state can go to waiting and back. A backfill: indexing -> done, and waiting.
//
// # One replica at a time
//
// TRIGGER: the schedule fires on one agent replica (placed, with its own
// lease), but a lease can be held twice while it changes hands, and an owner's
// act runs on whichever replica took the request. CHOSEN PATH: a tick first
// takes pg_try_advisory_xact_lock(hashtextextended('memql:memory-index', 0))
// and returns at once when another holds it (tryMemoryIndexLock). It is the
// TRANSACTION-scoped form, held on a connection of the write-gate pool: a
// session lock on the request pool would be released under a transaction
// pooler between two statements (db-connection-budget.md), and the request
// pool ends a session idle in a transaction after 60 s. The gate pool's
// sessions may idle five minutes, and a tick stops by its budget plus
// memoryIndexGrace (under a minute). A replica that dies drops its connection,
// and PostgreSQL releases the lock with it.
//
// The lock is not what keeps the run row right; the fence is. Every write of a
// run row carries the version it read (writeMemoryIndexRun), so a write after
// somebody else's -- an owner's Cancel, a lock that was lost -- is refused and
// the tick stops, rather than reverting what the other wrote (memql#5431). A
// vector written twice is the same vector, so a doubled tick costs embeddings,
// never correctness.
//
// # The cursor, and when it is saved
//
// The cursor names ONE source and the last item done in it; sources before it
// in name order are done for this walk (memoryIndexPosition). A batch is 32
// items. A batch that embedded or failed anything saves the run (cursor and
// counts) before the next batch starts, so a restart or another replica
// resumes after the last item embedded and embeds nothing twice. A batch whose
// items were all present saves nothing of its own and is saved with the
// tick's end: losing it costs a re-read of presence, never an embedding, and
// the backfill walks every indexed item every ten minutes -- a row version per
// 32 present items would be tens of thousands of versions a day for nothing.
// Rows that do get written keep only their newest version past a day
// (@retention on the concept).
//
// # Waiting is not failing
//
// An item whose embedding fails is tried memoryIndexAttempts times, then
// counted failed, and the walk goes on; the next walk tries it again. But when
// the model cannot be reached at all -- every door shut, no machine offering
// it online, the indexer no longer an owner (memoryIndexWaitReason) -- every
// item would fail the same way, and counting them would make a switch look
// broken and offer the owner "Switch anyway" over an empty index. So the run
// goes to `waiting` with a plain reason, keeps its cursor, counts nothing, and
// the next tick tries again. STOP CONDITION: the indexer's machine comes back
// or an owner chooses the model again. The indexer's role is checked before a
// tick's first step that embeds or moves the binding (work).
//
// # The backlog: what is missing, counted (ruling R10)
//
// Every walk that ends records what it LEFT missing -- items absent from the
// target or older there than their row whose embedding failed
// (Counts.Missing) -- on the run as Missing, with MissingAt: the backlog Memory
// search shows. A waiting walk cannot say, since it stops at its first missing
// item; so a waiting BACKFILL still takes a counting walk at the backfill's
// cadence (count): every source from the first, presence and staleness reads
// only, nothing embedded, on its own cursor (Counting) so the embed walk's
// cursor and counts stay where the wait left them. It is saved with the wait,
// at the tick's end, and resumes on the next tick, on any replica, when it does
// not fit in one. A fresh walk of the same model keeps the last count until its
// own end. A switch's waits count nothing: its backlog is not the screen's.
//
// # What counts as missing
//
// An item is missing from the target when its vector is absent OR older than
// its row's newest version (memoryIndexCurrent, ruling R7): a rewritten row's
// old vector no longer says what the row says, and presence alone cannot see
// that. Content-addressed sources are exempt; their text never changes under
// an id.
//
// # The flip: ordered steps, every prefix finished (ruling R6)
//
// flipMemoryIndex, under the binding lock. It is ORDERED STEPS, not one
// transaction: the engine writes rows only through its mutations, each its own
// statement, and the lock lives on another connection. `switching` is saved
// first, and the next tick finishes whatever prefix a crash left; the steps
// after the binding moves embed nothing, so they are finished even when the
// indexer is no longer an owner (work). What each prefix leaves:
//
//  1. `switching` saved, nothing else: search reads the active model; the next
//     tick runs the whole flip.
//  2. The active model recorded retired (retireAt now + 30 days), the binding
//     not moved: search still reads it, and its index is dated though in use,
//     which the daily cleanup never drops while it is active; the next tick
//     records it again, with a later date, and goes on.
//  3. The binding moved (one row version: readers see one model or the other,
//     never half): search reads the new model, the semantic answer cache may
//     hold the old model's answers, which no lookup matches (every cached row
//     is scoped to the model that embedded it, ai_semantic_cache.go), and the
//     run still says `switching` with no previous model; the next tick sees
//     the binding already moved, writes no binding, empties the cache and
//     records `finishing`, taking the previous model from the binding.
//  4. The cache emptied, `finishing` not yet recorded: as 3, and the DELETE
//     runs again.
//  5. `finishing` recorded: the flip is over, and the walk after it follows.
//
// # Nothing is broadcast (ruling R3)
//
// The run rows have no routing rule (component/node/routing.go). Nothing
// subscribes to them: the OS polls memorySearchStatus, and a broadcast on every
// batch's write would put a mesh event on every node per 32 items. The
// binding's own rule already tells every replica about a flip.
//
// Regression tests: memory_index_job_test.go (the decisions, over maps) and
// memory_index_job_db_test.go (Postgres, two engines over one database).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/num"
)

// MemoryIndexRunConceptID is the concept the index job's run rows live on.
const MemoryIndexRunConceptID = "v1:platform:memoryIndexRun"

// The two run rows, at literal ids, and their states.
const (
	memoryIndexKindSwitch   = "switch"
	memoryIndexKindBackfill = "backfill"

	memoryIndexStateIndexing      = "indexing"
	memoryIndexStateCatchingUp    = "catchingUp"
	memoryIndexStateNeedsDecision = "needsDecision"
	memoryIndexStateSwitching     = "switching"
	memoryIndexStateFinishing     = "finishing"
	memoryIndexStateDone          = "done"
	memoryIndexStateCancelled     = "cancelled"
	memoryIndexStateWaiting       = "waiting"
)

const (
	// memoryIndexBatch is how many items one step reads, checks and embeds.
	memoryIndexBatch = 32
	// memoryIndexAttempts is how often one walk tries one item before it
	// counts it failed. The next walk tries it again.
	memoryIndexAttempts = 3
	// memoryIndexItemTimeout bounds one embedding call, distinct from the
	// tick's budget: one hung machine must not hold the job past its tick.
	memoryIndexItemTimeout = 30 * time.Second
	// memoryIndexGrace is how far past its budget a tick may run to finish the
	// batch it is in. Budget plus grace stays under the minute between ticks
	// and far under the gate session's five-minute idle limit.
	memoryIndexGrace = 10 * time.Second
	// memoryIndexBackfillEvery is how often the backfill walks the active index.
	memoryIndexBackfillEvery = 10 * time.Minute
	// memoryIndexRetention is how long a replaced model's index is kept, so
	// switching back reuses it (design record section 7).
	memoryIndexRetention = 30 * 24 * time.Hour
	// memoryIndexDefaultBudget and memoryIndexMaxBudget bound the budget the
	// automation passes (45 s): a tick must end before the next one fires.
	memoryIndexDefaultBudget = 45 * time.Second
	memoryIndexMaxBudget     = 50 * time.Second
)

// memoryIndexClockSlack is how much older than its row's newest version a
// vector may be and still count as that version's (memoryIndexCurrent). A
// variable only so a test can shorten it.
var memoryIndexClockSlack = 2 * time.Second

// memoryIndexCurrent reports whether the target already holds item's vector AS
// ITS ROW NOW IS: present, and not older than the row's newest version.
//
// WHY A PRESENT VECTOR CAN BE MISSING (ruling R7). A vector is keyed by (id,
// vector_field) and carries no text, so presence alone cannot see a row
// rewritten after it was embedded. During a switch the writers write only the
// active table, so a row the first walk embedded into the target and someone
// then rewrote would be skipped by every later walk, and after the flip search
// would answer from text the row no longer has; the backfill would never repair
// an edit whose live re-embed failed either (embedCatalogConstruct swallows its
// failure and counts on a backfill). So a vector older than its row's newest
// version (MemoryItem.Version) is missing, in every walk.
//
// THE GUARD: a row its writer embedded when it was written is not touched.
// Every registered writer writes the row version first and its vector after
// (embedWorkObservation runs after the mutation, indexConversationEvidence,
// the acknowledgement and the catalog promotion write the row and then call
// the store, the Library writes its chunk and then embeds it), so its vector is
// the newer of the two. The version is stamped by the writing replica's clock
// and the vector's updated_at by the database's, so within memoryIndexClockSlack
// a vector still counts as its row's: replicas drift by milliseconds, not
// seconds. The cost of the slack is one rare miss: a row rewritten within it
// after the job embedded the row keeps the older vector until its next write.
// An item with no Version -- a content-addressed source, or one that pages its
// own items -- is current whenever it is present.
func memoryIndexCurrent(item MemoryItem, written map[string]time.Time) bool {
	at, ok := written[item.Id]
	if !ok {
		return false
	}
	return item.Version.IsZero() || !item.Version.After(at.Add(memoryIndexClockSlack))
}

// memoryIndexLockSQL takes the job's one-runner lock, without waiting, for the
// transaction it runs in (see the header).
const memoryIndexLockSQL = `SELECT pg_try_advisory_xact_lock(hashtextextended('memql:memory-index', 0))`

// memoryIndexActorName names the job when it reads and writes: every row
// version it writes says `system:memoryIndex` made it, whoever fired the tick.
const memoryIndexActorName = "memoryIndex"

// The reasons a run waits, in words an owner reads on Memory search.
const (
	memoryIndexOwnerReason     = "the owner who chose this model is no longer an owner; an owner can choose again"
	memoryIndexNoIndexerReason = "nobody is named to index this model yet; an owner can choose it in Cluster > Settings > Memory search"
	memoryIndexNoWriterReason  = "the replica running the index job has no embedding writer, so nothing can be indexed there"
)

var (
	// errMemoryIndexRunChanged is a run row written by somebody else since
	// this tick read it. The tick stops and the next one reads it again.
	errMemoryIndexRunChanged = errors.New("memory index: the run was changed elsewhere since it was read")
	// errMemoryIndexBusy is the embedder binding's lock held by another writer.
	errMemoryIndexBusy = errors.New("memory index: the embedder binding is being changed; the flip is tried again next tick")
	// errMemoryIndexNoWriter is a replica without the embedding writer.
	errMemoryIndexNoWriter = errors.New(memoryIndexNoWriterReason)
)

// MemoryIndexCounts are one walk's counts; a new walk starts from zero. Every
// item the walk visited is in Total, and Total = Done + Skipped + Failed:
//
//   - Done: embedded and written by this walk;
//   - Skipped: already in the target table, nothing to do;
//   - Failed: not written after memoryIndexAttempts tries, or an item its
//     source cannot rebuild (MemoryItem.Failure), or a source that could not
//     be read at all, which counts once.
//
// Unsourced is not a visited item. It is set when a walk ends: the entries of
// the ACTIVE table whose (concept, field) no registered source rebuilds, which
// a switch would drop from search.
//
// Missing is not a fourth kind of visited item either: it is the items the walk
// found absent from the target, or older there than their row
// (memoryIndexCurrent), and LEFT so -- the ones whose embedding failed. A walk
// that ends records it on the run (MemoryIndexRun.Missing).
type MemoryIndexCounts struct{ Total, Done, Skipped, Unsourced, Failed, Missing int }

// MemoryIndexRun is the Go mirror of v1:platform:memoryIndexRun.
type MemoryIndexRun struct {
	Kind, State, TargetRef, TargetTable, PreviousRef, IndexedBy, StartedBy, WaitingReason string
	TargetDimensions                                                                      int
	StartedAt, FinishedAt, LastPassAt                                                     time.Time
	// Cursor names at most one source: the one the walk is in, mapped to the
	// last id done in it ("" for its start). Empty is the walk's beginning.
	Cursor map[string]string
	Counts MemoryIndexCounts
	// Missing is what the last COMPLETED walk left missing (its Counts.Missing),
	// or what a counting walk found missing, and MissingAt when that walk
	// ended: the backlog Memory search shows (ruling R10). It is kept across
	// the walks of one model, so the last count stands while the next is taken.
	Missing   int
	MissingAt time.Time
	// Counting is a waiting backfill's counting walk in progress (count).
	Counting memoryIndexCounting
}

// memoryIndexCounting is a counting walk in progress: where it stands, in the
// shape of MemoryIndexRun.Cursor, and how many items it has found missing so
// far. An empty Cursor is no counting walk in progress.
type memoryIndexCounting struct {
	Cursor  map[string]string
	Missing int
}

// memoryIndexWorld is everything a tick touches outside its own decisions:
// the binding, the run rows, the sources, the vector tables, the store path
// and the flip. engineMemoryIndexWorld implements it over Postgres; the unit
// tests implement it over maps, so the state machine is tested without a
// database.
type memoryIndexWorld interface {
	activeBinding(ctx context.Context) (EmbedderBinding, error)
	readRun(ctx context.Context, kind string) (MemoryIndexRun, bool, error)
	// saveRun writes the whole row, and answers errMemoryIndexRunChanged when
	// somebody else wrote it since this tick read or wrote it.
	saveRun(ctx context.Context, run MemoryIndexRun) error
	isActiveOwner(ctx context.Context, userId string) bool
	sources() []MemorySource
	items(ctx context.Context, s MemorySource, after string, limit int) ([]MemoryItem, string, error)
	ensureTable(ctx context.Context, ref string, dims int) (string, error)
	// vectorTimes answers, for each of ids the table holds a vector of, when
	// that vector was last written.
	vectorTimes(ctx context.Context, table, vectorField string, ids []string) (map[string]time.Time, error)
	embed(ctx context.Context, run MemoryIndexRun, s MemorySource, item MemoryItem) error
	unsourced(ctx context.Context, table string, sources []MemorySource) (int, error)
	// flip makes the run's target the active model and answers the run as it
	// was saved afterwards, in `finishing`.
	flip(ctx context.Context, run MemoryIndexRun) (MemoryIndexRun, error)
	now() time.Time
	logger() *slog.Logger
}

// cloneMemoryIndexRun copies a run, so a map held by one copy is never written
// through another. A nil cursor becomes an empty one.
func cloneMemoryIndexRun(run MemoryIndexRun) MemoryIndexRun {
	run.Cursor = cloneMemoryIndexCursor(run.Cursor)
	run.Counting.Cursor = cloneMemoryIndexCursor(run.Counting.Cursor)
	return run
}

func cloneMemoryIndexCursor(cursor map[string]string) map[string]string {
	out := make(map[string]string, len(cursor))
	for k, v := range cursor {
		out[k] = v
	}
	return out
}

// memoryIndexRunsEqual reports whether writing a would change the row b.
func memoryIndexRunsEqual(a, b MemoryIndexRun) bool {
	return a.Kind == b.Kind && a.State == b.State && a.TargetRef == b.TargetRef && a.TargetTable == b.TargetTable &&
		a.PreviousRef == b.PreviousRef && a.IndexedBy == b.IndexedBy && a.StartedBy == b.StartedBy &&
		a.WaitingReason == b.WaitingReason && a.TargetDimensions == b.TargetDimensions && a.Counts == b.Counts &&
		a.StartedAt.Equal(b.StartedAt) && a.FinishedAt.Equal(b.FinishedAt) && a.LastPassAt.Equal(b.LastPassAt) &&
		a.Missing == b.Missing && a.MissingAt.Equal(b.MissingAt) && a.Counting.Missing == b.Counting.Missing &&
		memoryIndexCursorsEqual(a.Cursor, b.Cursor) && memoryIndexCursorsEqual(a.Counting.Cursor, b.Counting.Cursor)
}

func memoryIndexCursorsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// memoryIndexSwitchActive reports whether a switch in this state still has the
// job's work to do. needsDecision is the owner's move; done and cancelled are over.
func memoryIndexSwitchActive(state string) bool {
	switch state {
	case memoryIndexStateIndexing, memoryIndexStateCatchingUp, memoryIndexStateSwitching,
		memoryIndexStateFinishing, memoryIndexStateWaiting:
		return true
	}
	return false
}

// memoryIndexResumeState is the walk a waiting run returns to. A backfill has
// one walk. A switch that has flipped (its target is the active model) was
// finishing; one whose first walk ended (lastPassAt) was catching up, and a
// switch that waited on its way to the flip walks once more before it, which
// costs one walk of presence checks; otherwise it was indexing.
func memoryIndexResumeState(run MemoryIndexRun, active EmbedderBinding) string {
	switch {
	case run.Kind != memoryIndexKindSwitch:
		return memoryIndexStateIndexing
	case active.ProviderRef == run.TargetRef && active.Dimensions == run.TargetDimensions:
		return memoryIndexStateFinishing
	case !run.LastPassAt.IsZero():
		return memoryIndexStateCatchingUp
	default:
		return memoryIndexStateIndexing
	}
}

// memoryIndexPosition is where a walk stands: the index of the source it is in
// and the id to resume after. A cursor naming a source that is no longer
// registered resumes at the next one by name; one that names nothing is the
// walk's beginning.
func memoryIndexPosition(sources []MemorySource, cursor map[string]string) (int, string) {
	name, after := "", ""
	for k, v := range cursor {
		if name == "" || k < name {
			name, after = k, v
		}
	}
	if name == "" {
		return 0, ""
	}
	for i, s := range sources {
		if s.Name == name {
			return i, after
		}
		if s.Name > name {
			return i, ""
		}
	}
	return len(sources), ""
}

// memoryIndexWaitReason says whether an embedding error means the model cannot
// be reached at all (a wait) rather than this item failing, and the plain
// reason the run shows. The triggers are what the router and the resolver say:
// the indexer is no longer an owner (ErrEmbeddingIndexerNotOwner); no machine
// offering the model is online (ErrFleetUnavailable, its words, or a refusal
// code -- every_door_shut is a pinned source with every door shut); this
// replica cannot write vectors at all. A refusal for a vendor model waits too:
// counting every item failed during a vendor's outage would offer the owner
// "Switch anyway" over an empty index.
func memoryIndexWaitReason(err error, ref string) (string, bool) {
	if err == nil {
		return "", false
	}
	if errors.Is(err, ErrEmbeddingIndexerNotOwner) {
		return memoryIndexOwnerReason, true
	}
	if errors.Is(err, errMemoryIndexNoWriter) {
		return memoryIndexNoWriterReason, true
	}
	msg := err.Error()
	_, _, refused := work.DoorsFrom(err)
	if !refused && !errors.Is(err, ErrFleetUnavailable) &&
		!strings.Contains(msg, "no machine offering") && !strings.Contains(msg, "no machine offers") {
		return "", false
	}
	if model, fleet := IsFleetReference(ref); fleet {
		return fmt.Sprintf("none of the indexer's machines offering %s is online", model), true
	}
	return fmt.Sprintf("%s cannot be reached right now", strings.TrimSpace(ref)), true
}

// memoryIndexTick is one tick's working state.
type memoryIndexTick struct {
	w          memoryIndexWorld
	log        *slog.Logger
	soft, hard time.Time
	active     EmbedderBinding
	run        MemoryIndexRun // the run being worked on
	stored     MemoryIndexRun // the row as this tick last read or wrote it
	sources    []MemorySource
	table      string // the run's target table
}

// runMemoryIndexTick is one bounded tick: pick the run, re-check its indexer,
// then take steps -- a batch of a walk, or the flip -- until the run rests or
// the budget is spent. The first step always runs, so a tick with a tiny
// budget still makes progress; a step past the budget never starts.
func runMemoryIndexTick(ctx context.Context, w memoryIndexWorld, budget time.Duration) error {
	t := &memoryIndexTick{w: w, log: w.logger()}
	if t.log == nil {
		t.log = slog.Default()
	}
	start := w.now()
	t.soft = start.Add(budget)
	t.hard = t.soft.Add(memoryIndexGrace)
	active, err := w.activeBinding(ctx)
	if err != nil && !errors.Is(err, ErrNoEmbedderBound) {
		return err
	}
	t.active = active
	picked, err := t.pick(ctx)
	if err != nil || !picked {
		return err
	}
	return t.work(ctx)
}

// pick chooses the run this tick works on: a switch while one is in progress,
// otherwise the backfill when it is due.
func (t *memoryIndexTick) pick(ctx context.Context) (bool, error) {
	sw, has, err := t.w.readRun(ctx, memoryIndexKindSwitch)
	if err != nil {
		return false, err
	}
	if has && memoryIndexSwitchActive(sw.State) {
		t.run, t.stored = cloneMemoryIndexRun(sw), cloneMemoryIndexRun(sw)
		return true, nil
	}
	if !t.active.Valid() {
		return false, nil
	}
	bf, has, err := t.w.readRun(ctx, memoryIndexKindBackfill)
	if err != nil {
		return false, err
	}
	t.stored = cloneMemoryIndexRun(bf)
	now := t.w.now()
	sameTarget := has && bf.TargetRef == t.active.ProviderRef && bf.TargetDimensions == t.active.Dimensions
	switch {
	case sameTarget && (bf.State == memoryIndexStateIndexing || bf.State == memoryIndexStateWaiting):
		t.run = cloneMemoryIndexRun(bf)
	case sameTarget && !bf.LastPassAt.IsZero() && now.Sub(bf.LastPassAt) < memoryIndexBackfillEvery:
		return false, nil
	default:
		// A fresh walk: the first, one due again, or one for a model a flip made
		// active (the previous walk's cursor is for another table).
		next := MemoryIndexRun{Kind: memoryIndexKindBackfill, State: memoryIndexStateIndexing,
			TargetRef: t.active.ProviderRef, TargetDimensions: t.active.Dimensions,
			StartedAt: now, Cursor: map[string]string{}}
		if sameTarget {
			next.LastPassAt, next.FinishedAt = bf.LastPassAt, bf.FinishedAt
			next.Missing, next.MissingAt = bf.Missing, bf.MissingAt
		}
		t.run = next
	}
	// The backfill embeds as the ACTIVE model's indexer, read fresh each tick,
	// so an owner choosing the model again is honoured at the next tick.
	t.run.IndexedBy = strings.TrimSpace(t.active.IndexedBy)
	return true, nil
}

// work takes the run's steps.
//
// The indexer is re-checked before the first step of the tick that embeds or
// moves the binding (indexerWait). ONE STEP IS EXEMPT: the rest of a flip whose
// binding already names its target (ruling R6). A flip that moved the binding
// and stopped before recording `finishing` has left only steps that embed
// nothing and choose no machine -- emptying the semantic answer cache,
// recording `finishing` and the model replaced -- so they run whatever the
// indexer's standing. Parked instead, the run would resume as `finishing`
// (memoryIndexResumeState) and those steps would never run. The walk after the
// flip is checked as usual, and waits for an owner.
func (t *memoryIndexTick) work(ctx context.Context) error {
	// The sources and the table first: a waiting backfill still counts its
	// backlog over them (count).
	t.sources = append([]MemorySource(nil), t.w.sources()...)
	sort.SliceStable(t.sources, func(a, b int) bool { return t.sources[a].Name < t.sources[b].Name })
	table, err := t.w.ensureTable(ctx, t.run.TargetRef, t.run.TargetDimensions)
	if err != nil {
		return err
	}
	t.table, t.run.TargetTable = table, table
	if t.run.State == memoryIndexStateWaiting {
		// Still refused: the run waits on, unchanged (save writes nothing).
		if reason, wait := t.indexerWait(ctx); wait {
			return t.wait(ctx, reason)
		}
		t.run.State = memoryIndexResumeState(t.run, t.active)
		t.run.WaitingReason = ""
	}
	checked := false
	check := func() (string, bool) {
		if checked {
			return "", false
		}
		checked = true
		return t.indexerWait(ctx)
	}
	for steps := 0; ; steps++ {
		if steps > 0 && !t.w.now().Before(t.soft) {
			_, err := t.save(ctx)
			return err
		}
		var stop bool
		switch t.run.State {
		case memoryIndexStateSwitching:
			if !t.bindingNamesTarget() {
				if reason, wait := check(); wait {
					return t.wait(ctx, reason)
				}
			}
			stop, err = t.flipStep(ctx)
		case memoryIndexStateIndexing, memoryIndexStateCatchingUp, memoryIndexStateFinishing:
			if reason, wait := check(); wait {
				return t.wait(ctx, reason)
			}
			stop, err = t.batch(ctx)
		default:
			_, err = t.save(ctx)
			return err
		}
		if err != nil || stop {
			return err
		}
	}
}

// bindingNamesTarget reports whether the active binding already is the run's
// target: a switch whose flip has moved the binding.
func (t *memoryIndexTick) bindingNamesTarget() bool {
	return t.active.ProviderRef == t.run.TargetRef && t.active.Dimensions == t.run.TargetDimensions
}

// indexerWait is the indexer re-check of every tick (R5 at the job's level),
// made before the tick's first step that embeds or moves the binding. A fleet
// model is embedded on its indexer's machines, and the
// indexer's machines see every person's memory text, so an indexer who is no
// longer an active owner indexes nothing; a fleet model with no indexer named
// would resolve as the job itself and reach no one's machines. A vendor model
// has no indexer: the cluster's account pays, as it does for every embedding.
func (t *memoryIndexTick) indexerWait(ctx context.Context) (string, bool) {
	indexer, fleet := IndexerFor(EmbedderBinding{ProviderRef: t.run.TargetRef, IndexedBy: t.run.IndexedBy})
	if !fleet {
		return "", false
	}
	if indexer == "" {
		return memoryIndexNoIndexerReason, true
	}
	if !t.w.isActiveOwner(ctx, indexer) {
		return memoryIndexOwnerReason, true
	}
	return "", false
}

// wait parks the run with a reason, keeping its cursor. An unchanged wait is
// not written again (save compares with the stored row).
func (t *memoryIndexTick) wait(ctx context.Context, reason string) error {
	t.run.State, t.run.WaitingReason = memoryIndexStateWaiting, reason
	if t.run.Kind == memoryIndexKindBackfill {
		if err := t.count(ctx); err != nil {
			_, serr := t.save(ctx)
			return errors.Join(err, serr)
		}
	}
	_, err := t.save(ctx)
	return err
}

// count takes the steps of a waiting backfill's COUNTING WALK (ruling R10).
// While the model cannot be reached nothing is embedded, but Memory search must
// still say how many items wait ("37 items waiting: ..."), and a wait stops the
// embed walk at its first missing item, so its counts cannot say. TRIGGER: a
// backfill waiting, and either a counting walk in progress or the last count
// memoryIndexBackfillEvery old (or none taken). The walk covers every source
// from the first, reading presence and staleness only (memoryIndexCurrent, as
// the embed walk does), and keeps its own cursor and tally on the run
// (Counting), so the embed walk's Cursor and Counts stay where the wait left
// them and a walk too long for one tick resumes on the next, on any replica.
// It runs one batch at least and then while the budget lasts. Its end records
// Missing and MissingAt. It is saved with the wait, at the tick's end: losing a
// batch of it costs a re-read of presence, never an embedding.
func (t *memoryIndexTick) count(ctx context.Context) error {
	r := &t.run
	if len(r.Counting.Cursor) == 0 {
		if !r.MissingAt.IsZero() && t.w.now().Sub(r.MissingAt) < memoryIndexBackfillEvery {
			return nil
		}
		r.Counting.Missing = 0
	}
	for first := true; first || t.w.now().Before(t.soft); first = false {
		ended, err := t.countBatch(ctx)
		if err != nil || ended {
			return err
		}
	}
	return nil
}

// countBatch counts up to memoryIndexBatch items of the source the counting
// walk is in. It answers true when counting stops for this tick: the walk
// ended, or the tick's context did.
func (t *memoryIndexTick) countBatch(ctx context.Context) (bool, error) {
	r := &t.run
	pos, after := memoryIndexPosition(t.sources, r.Counting.Cursor)
	if pos >= len(t.sources) {
		t.endCount()
		return true, nil
	}
	s := t.sources[pos]
	items, next, err := t.w.items(ctx, s, after, memoryIndexBatch)
	if err != nil {
		if ctx.Err() != nil {
			return true, nil
		}
		t.log.Warn("memory index: a source could not be read while counting the backlog; its items are not in this count",
			"source", s.Name, "error", err)
		return t.countMoveOn(pos), nil
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item.Failure == "" {
			ids = append(ids, item.Id)
		}
	}
	written := map[string]time.Time{}
	if len(ids) > 0 {
		if written, err = t.w.vectorTimes(ctx, t.table, s.VectorField, ids); err != nil {
			return true, err
		}
	}
	for _, item := range items {
		if item.Failure == "" && !memoryIndexCurrent(item, written) {
			r.Counting.Missing++
		}
	}
	if next != "" {
		r.Counting.Cursor = map[string]string{s.Name: next}
		return false, nil
	}
	return t.countMoveOn(pos), nil
}

// countMoveOn leaves source pos counted: the counting walk goes to the next
// source's start, or, after the last, ends. It answers true when it ended.
func (t *memoryIndexTick) countMoveOn(pos int) bool {
	if pos+1 < len(t.sources) {
		t.run.Counting.Cursor = map[string]string{t.sources[pos+1].Name: ""}
		return false
	}
	t.endCount()
	return true
}

// endCount records a finished counting walk as the backlog.
func (t *memoryIndexTick) endCount() {
	r := &t.run
	r.Missing, r.MissingAt = r.Counting.Missing, t.w.now()
	r.Counting = memoryIndexCounting{Cursor: map[string]string{}}
}

// save writes the run when it differs from the stored row. It answers false
// when the tick must stop: somebody else wrote the row since (an owner's act),
// and the next tick reads what they wrote.
func (t *memoryIndexTick) save(ctx context.Context) (bool, error) {
	if memoryIndexRunsEqual(t.run, t.stored) {
		return true, nil
	}
	if err := t.w.saveRun(ctx, t.run); err != nil {
		if errors.Is(err, errMemoryIndexRunChanged) {
			t.log.Info("memory index: the run was changed elsewhere since this tick read it; the next tick reads it again",
				"kind", t.run.Kind, "state", t.run.State)
			return false, nil
		}
		return false, err
	}
	t.stored = cloneMemoryIndexRun(t.run)
	return true, nil
}

// batch is one step of a walk: up to memoryIndexBatch items of the source the
// cursor is in. It answers true when the tick must stop.
func (t *memoryIndexTick) batch(ctx context.Context) (bool, error) {
	pos, after := memoryIndexPosition(t.sources, t.run.Cursor)
	if pos >= len(t.sources) {
		return t.endWalk(ctx)
	}
	s := t.sources[pos]
	items, next, err := t.w.items(ctx, s, after, memoryIndexBatch)
	if err != nil {
		if ctx.Err() != nil {
			_, serr := t.save(ctx)
			return true, serr
		}
		// A source that cannot be read is counted, never skipped silently, and
		// the walk goes on; the next walk reads it again.
		t.log.Warn("memory index: a source could not be read; it counts as one failed item and the next walk reads it again",
			"source", s.Name, "error", err)
		t.run.Counts.Total++
		t.run.Counts.Failed++
		if ended, stop, err := t.moveOn(ctx, pos); ended || stop || err != nil {
			return stop, err
		}
		ok, err := t.save(ctx)
		return !ok, err
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item.Failure == "" {
			ids = append(ids, item.Id)
		}
	}
	written := map[string]time.Time{}
	if len(ids) > 0 {
		if written, err = t.w.vectorTimes(ctx, t.table, s.VectorField, ids); err != nil {
			_, serr := t.save(ctx)
			return true, errors.Join(err, serr)
		}
	}
	changed, last := false, after
	for _, item := range items {
		switch {
		case item.Failure != "":
			t.log.Warn("memory index: an item its source cannot rebuild counts as failed",
				"source", s.Name, "id", item.Id, "why", item.Failure)
			t.run.Counts.Total++
			t.run.Counts.Failed++
			changed = true
		case memoryIndexCurrent(item, written):
			t.run.Counts.Total++
			t.run.Counts.Skipped++
		default:
			reason, stopped, err := t.embed(ctx, s, item)
			if stopped || reason != "" {
				// Resume at this item: everything before it is counted.
				t.run.Cursor = map[string]string{s.Name: last}
				if reason != "" {
					return true, t.wait(ctx, reason)
				}
				_, serr := t.save(ctx)
				return true, serr
			}
			t.run.Counts.Total++
			if err != nil {
				t.log.Warn("memory index: an item could not be indexed; it counts as failed and the next walk tries it again",
					"source", s.Name, "id", item.Id, "target", t.run.TargetRef, "error", err)
				t.run.Counts.Failed++
				t.run.Counts.Missing++
			} else {
				t.run.Counts.Done++
			}
			changed = true
		}
		last = item.Id
	}
	if next != "" {
		t.run.Cursor = map[string]string{s.Name: next}
	} else if ended, stop, err := t.moveOn(ctx, pos); ended || stop || err != nil {
		return stop, err // the walk's end saved the run with everything this batch did
	}
	if !changed {
		return false, nil
	}
	ok, err := t.save(ctx)
	return !ok, err
}

// moveOn leaves source pos done for this walk: the cursor names the next
// source's start, or, after the last source, the walk ends (ended), in which
// case endWalk has saved the run and stop says whether the tick rests.
func (t *memoryIndexTick) moveOn(ctx context.Context, pos int) (ended, stop bool, err error) {
	if pos+1 < len(t.sources) {
		t.run.Cursor = map[string]string{t.sources[pos+1].Name: ""}
		return false, false, nil
	}
	stop, err = t.endWalk(ctx)
	return true, stop, err
}

// embed tries one item up to memoryIndexAttempts times. It answers the wait
// reason when the model cannot be reached at all, stopped when the tick ran
// out of time, and otherwise the last error (nil when written).
func (t *memoryIndexTick) embed(ctx context.Context, s MemorySource, item MemoryItem) (string, bool, error) {
	var err error
	for attempt := 1; attempt <= memoryIndexAttempts; attempt++ {
		remaining := t.hard.Sub(t.w.now())
		if remaining <= 0 {
			return "", true, nil
		}
		call, cancel := context.WithTimeout(ctx, min(remaining, memoryIndexItemTimeout))
		err = t.w.embed(call, t.run, s, item)
		cancel()
		if err == nil {
			return "", false, nil
		}
		if reason, wait := memoryIndexWaitReason(err, t.run.TargetRef); wait {
			return reason, false, nil
		}
		if ctx.Err() != nil || !t.w.now().Before(t.hard) {
			return "", true, nil
		}
	}
	return "", false, err
}

// endWalk decides what follows a walk that reached the end of the last source.
// It answers true when the run rests (done, needsDecision) or the tick must stop.
func (t *memoryIndexTick) endWalk(ctx context.Context) (bool, error) {
	now := t.w.now()
	r := &t.run
	// Every walk that ends records what it left missing: the backlog. A
	// counting walk it interrupted is over, since this count is newer.
	r.Missing, r.MissingAt = r.Counts.Missing, now
	r.Counting = memoryIndexCounting{Cursor: map[string]string{}}
	if r.Kind == memoryIndexKindSwitch && r.State == memoryIndexStateIndexing {
		// The first walk filled the target; the catch-up walk picks up what
		// arrived meanwhile, and decides about the flip.
		r.State, r.LastPassAt = memoryIndexStateCatchingUp, now
		r.Cursor, r.Counts = map[string]string{}, MemoryIndexCounts{}
	} else {
		unsourced, err := t.unsourced(ctx)
		if err != nil {
			_, serr := t.save(ctx)
			return true, errors.Join(err, serr)
		}
		r.Counts.Unsourced, r.LastPassAt, r.Cursor = unsourced, now, map[string]string{}
		switch {
		case r.Kind == memoryIndexKindBackfill || r.State == memoryIndexStateFinishing:
			r.State, r.FinishedAt = memoryIndexStateDone, now
		case r.Counts.Failed == 0 && r.Counts.Unsourced == 0:
			// Nothing is left missing and nothing would drop out of search. An
			// item written after this walk passed its place is caught by the
			// walk after the flip: it went to the active table, which is kept.
			r.State = memoryIndexStateSwitching
		default:
			// The flip is automatic only when every item is in. The owner sees
			// how many failed and how many no source rebuilds, and decides.
			r.State = memoryIndexStateNeedsDecision
		}
	}
	ok, err := t.save(ctx)
	if !ok || err != nil {
		return true, err
	}
	return r.State == memoryIndexStateDone || r.State == memoryIndexStateNeedsDecision, nil
}

// unsourced counts the active table's entries no registered source rebuilds:
// what a switch would drop from search. Nothing is active: nothing to drop.
func (t *memoryIndexTick) unsourced(ctx context.Context) (int, error) {
	if !t.active.Valid() {
		return 0, nil
	}
	table, err := t.w.ensureTable(ctx, t.active.ProviderRef, t.active.Dimensions)
	if err != nil {
		return 0, err
	}
	return t.w.unsourced(ctx, table, t.sources)
}

// flipStep makes the switch's target active, then walks on in `finishing`.
func (t *memoryIndexTick) flipStep(ctx context.Context) (bool, error) {
	// `switching` is on the row before the binding moves, so a tick that dies
	// mid-flip leaves the next one the rest of it.
	if ok, err := t.save(ctx); !ok || err != nil {
		return true, err
	}
	flipped, err := t.w.flip(ctx, t.run)
	if err != nil {
		if errors.Is(err, errMemoryIndexBusy) || errors.Is(err, errMemoryIndexRunChanged) {
			t.log.Info("memory index: the flip waits for the next tick", "target", t.run.TargetRef, "why", err)
			return true, nil
		}
		return true, err
	}
	t.run, t.stored = cloneMemoryIndexRun(flipped), cloneMemoryIndexRun(flipped)
	active, err := t.w.activeBinding(ctx)
	if err != nil {
		return true, err
	}
	t.active = active
	return false, nil
}

// --- the engine's world: Postgres, the store path, the binding --------------

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

// tryMemoryIndexLock takes the job's one-runner lock without waiting, in a
// transaction on a write-gate connection held until release (see the header).
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

// flipMemoryIndex makes a switch's target the active memory-search model: the
// active model's row is kept as retired for memoryIndexRetention, the binding
// names the target, the semantic answer cache is emptied (its vectors are the
// old model's), and the run goes to `finishing` for the walk that covers what
// was written meanwhile. See the header for why this is ordered steps under
// one lock rather than one transaction, and why a crash anywhere in it is
// finished by the next tick.
//
// run is the switch the caller read; the flip happens only for that switch
// (same target, same start) in `switching` -- the job's -- or `needsDecision`
// (an owner's Switch anyway). The binding lock (lockAskConversationKind
// "active", "embedder-binding") is held around the fresh reads and every
// write, and the binding write is fenced to the version read under it, so a
// concurrent binding writer on another replica is never reverted and the new
// version is stamped after the one read (memql#5431). Every id it writes is
// derived here (RetiredEmbedderBindingID), never taken from input.
func (e *MemQLEngine) flipMemoryIndex(ctx context.Context, run MemoryIndexRun) error {
	if run.Kind != memoryIndexKindSwitch {
		return fmt.Errorf("memory index: only a switch flips the model, not a %q run", run.Kind)
	}
	unlock, err := e.lockAskConversationKind(ctx, ActiveEmbedderBindingID, "embedder-binding")
	if err != nil {
		return fmt.Errorf("%w: %v", errMemoryIndexBusy, err)
	}
	defer unlock()
	current, version, ok, err := e.readMemoryIndexRun(ctx, memoryIndexKindSwitch)
	if err != nil {
		return err
	}
	if !ok || current.TargetRef != run.TargetRef || current.TargetDimensions != run.TargetDimensions || !current.StartedAt.Equal(run.StartedAt) {
		return errMemoryIndexRunChanged
	}
	if current.State != memoryIndexStateSwitching && current.State != memoryIndexStateNeedsDecision {
		return fmt.Errorf("memory index: a switch flips from switching or needsDecision, not from %s", current.State)
	}
	if _, err := EmbeddingVectorTable(current.TargetRef, current.TargetDimensions); err != nil {
		return err
	}
	db := e.database()
	if db == nil {
		return fmt.Errorf("memory index: no database")
	}
	bindingVersion, err := e.latestRowVersion(ctx, EmbedderBindingConceptID, ActiveEmbedderBindingID)
	if err != nil {
		return err
	}
	fenced := ContextWithRowVersionFence(ctx, EmbedderBindingConceptID, ActiveEmbedderBindingID, bindingVersion)
	active, err := e.ReadEmbedderBinding(ctx)
	if err != nil {
		return fmt.Errorf("memory index: there is no active model to switch from: %w", err)
	}
	now := time.Now().UTC()
	previous := current.PreviousRef
	if active.ProviderRef == current.TargetRef && active.Dimensions == current.TargetDimensions {
		// A resumed flip: an earlier attempt moved the binding and stopped
		// before the run said so. The binding names what it replaced.
		if previous == "" {
			previous = active.PreviousRef
		}
	} else {
		retired := RetiredEmbedderBindingID(active.ProviderRef, active.Dimensions)
		if retired == "" {
			return fmt.Errorf("memory index: the active model %q has no index to keep", active.ProviderRef)
		}
		// Retired FIRST: once the binding has moved, the old model's index
		// must already be dated for keeping, or the daily cleanup could not
		// tell it from an index nobody needs.
		// Each write is stamped after the version it read (ruling R12): the
		// retired row's own, and the binding's read under this lock.
		retiredVersion, err := e.latestRowVersion(ctx, EmbedderBindingConceptID, retired)
		if err != nil {
			return err
		}
		retireArgs := map[string]any{"bindingId": retired, "providerRef": active.ProviderRef, "dimensions": active.Dimensions,
			"retireAt":    now.Add(memoryIndexRetention).Format(time.RFC3339Nano),
			"versionTime": VersionTimeAfter(retiredVersion, now).Format(time.RFC3339Nano)}
		if v := strings.TrimSpace(active.IndexedBy); v != "" {
			retireArgs["indexedBy"] = v
		}
		if err := e.memoryIndexMutation(auth.ContextWithInternalOrigin(ctx), "platform.recordRetiredEmbedder", retireArgs); err != nil {
			return err
		}
		flipArgs := map[string]any{"bindingId": ActiveEmbedderBindingID, "providerRef": current.TargetRef,
			"dimensions": current.TargetDimensions, "activatedAt": now.Format(time.RFC3339Nano), "previousRef": active.ProviderRef,
			"versionTime": VersionTimeAfter(bindingVersion, now).Format(time.RFC3339Nano)}
		if indexer, fleet := IndexerFor(EmbedderBinding{ProviderRef: current.TargetRef, IndexedBy: current.IndexedBy}); fleet && indexer != "" {
			flipArgs["indexedBy"] = indexer
		}
		if v := strings.TrimSpace(current.StartedBy); v != "" {
			flipArgs["chosenBy"] = v
		}
		if err := e.memoryIndexMutation(auth.ContextWithInternalOrigin(fenced), "platform.flipEmbedderBinding", flipArgs); err != nil {
			return err
		}
		previous = active.ProviderRef
	}
	// The semantic answer cache holds the old model's vectors: dropped, and it
	// refills in the bound model on its own (design record, non-goals). No
	// lookup could match them anyway -- every cached row is scoped to the model
	// that embedded it, so neither these nor a row a lagging replica writes after
	// this DELETE answer a lookup made with the new model (ai_semantic_cache.go).
	if _, err := db.DB.ExecContext(ctx, `DELETE FROM semantic_ai_cache`); err != nil {
		return fmt.Errorf("memory index: emptying the semantic answer cache: %w", err)
	}
	next := cloneMemoryIndexRun(current)
	next.State, next.PreviousRef, next.WaitingReason = memoryIndexStateFinishing, previous, ""
	next.Cursor, next.Counts = map[string]string{}, MemoryIndexCounts{}
	if _, err := e.writeMemoryIndexRun(ctx, next, version); err != nil {
		return err
	}
	e.logOrDefault().Info("memory index: memory search switched to its new model",
		"provider", current.TargetRef, "dimensions", current.TargetDimensions, "previous", previous)
	return nil
}

// memoryIndexMutation runs one of the job's @serverOnly mutations.
func (e *MemQLEngine) memoryIndexMutation(ctx context.Context, name string, args map[string]any) error {
	_, err := e.memoryIndexMutationResult(ctx, name, args)
	return err
}

// memoryIndexMutationResult runs one of the job's @serverOnly mutations and
// answers what it wrote.
func (e *MemQLEngine) memoryIndexMutationResult(ctx context.Context, name string, args map[string]any) (*ExecuteResult, error) {
	call, err := parser.RenderCall(name, args)
	if err != nil {
		return nil, err
	}
	return e.Execute(ctx, "mutation "+call)
}

// memoryIndexWrittenVersion is the version a write of row id stored, as the
// write's own result reports it, or zero when the result does not name it.
func memoryIndexWrittenVersion(res *ExecuteResult, id string) time.Time {
	if res == nil || res.Bundle == nil {
		return time.Time{}
	}
	for _, n := range res.Bundle.Nodes {
		if n != nil && n.GetId() == id && n.GetCreatedAt() != nil {
			return n.GetCreatedAt().AsTime().UTC()
		}
	}
	return time.Time{}
}

func (e *MemQLEngine) logOrDefault() *slog.Logger {
	if l := e.safeLogger(); l != nil {
		return l
	}
	return slog.Default()
}

// memoryIndexRunPayload is the row as stored.
type memoryIndexRunPayload struct {
	Kind             string            `json:"kind"`
	State            string            `json:"state"`
	TargetRef        string            `json:"targetRef"`
	TargetDimensions int               `json:"targetDimensions"`
	TargetTable      string            `json:"targetTable"`
	PreviousRef      string            `json:"previousRef"`
	IndexedBy        string            `json:"indexedBy"`
	StartedBy        string            `json:"startedBy"`
	StartedAt        string            `json:"startedAt"`
	FinishedAt       string            `json:"finishedAt"`
	Cursor           map[string]string `json:"cursor"`
	Counts           struct {
		Total     int `json:"total"`
		Done      int `json:"done"`
		Skipped   int `json:"skipped"`
		Unsourced int `json:"unsourced"`
		Failed    int `json:"failed"`
		Missing   int `json:"missing"`
	} `json:"counts"`
	WaitingReason string `json:"waitingReason"`
	LastPassAt    string `json:"lastPassAt"`
	Missing       int    `json:"missing"`
	MissingAt     string `json:"missingAt"`
	Counting      struct {
		Cursor  map[string]string `json:"cursor"`
		Missing int               `json:"missing"`
	} `json:"counting"`
}

func memoryIndexTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func memoryIndexTimeArg(args map[string]any, key string, t time.Time) {
	if !t.IsZero() {
		args[key] = t.UTC().Format(time.RFC3339Nano)
	}
}

// readMemoryIndexRun returns the newest version of one run row, when that
// version was written (the witness writeMemoryIndexRun fences on), and whether
// the row exists. Read straight off the store: it is the job's own state, read
// fresh by construction, and never answered from a result cache.
//
// staged-data: MUST-NOT-GATE -- the job's own state row (a core platform
// concept, never promoted). Read as staged, a switch would read as absent: the
// job would never see an owner's switch, or would start one over from its
// first item and embed everything again.
func (e *MemQLEngine) readMemoryIndexRun(ctx context.Context, kind string) (MemoryIndexRun, time.Time, bool, error) {
	db := e.database()
	if db == nil {
		return MemoryIndexRun{}, time.Time{}, false, fmt.Errorf("memory index: no database")
	}
	var node memorynodes.MemoryNode
	q := db.NewSelect().Model(&node).Column("createdAt", "payload").Where("concept = ?", MemoryIndexRunConceptID)
	err := versionsOfOneID(q, MemoryIndexRunConceptID+":"+kind, false).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return MemoryIndexRun{}, time.Time{}, false, nil
	}
	if err != nil {
		return MemoryIndexRun{}, time.Time{}, false, fmt.Errorf("memory index: reading the %s run: %w", kind, err)
	}
	var p memoryIndexRunPayload
	if err := json.Unmarshal(node.Payload, &p); err != nil {
		return MemoryIndexRun{}, time.Time{}, false, fmt.Errorf("memory index: the %s run does not decode: %w", kind, err)
	}
	run := MemoryIndexRun{
		Kind: p.Kind, State: p.State, TargetRef: p.TargetRef, TargetDimensions: p.TargetDimensions,
		TargetTable: p.TargetTable, PreviousRef: p.PreviousRef, IndexedBy: p.IndexedBy, StartedBy: p.StartedBy,
		WaitingReason: p.WaitingReason, StartedAt: memoryIndexTime(p.StartedAt), FinishedAt: memoryIndexTime(p.FinishedAt),
		LastPassAt: memoryIndexTime(p.LastPassAt), Cursor: p.Cursor,
		Counts: MemoryIndexCounts{Total: p.Counts.Total, Done: p.Counts.Done, Skipped: p.Counts.Skipped,
			Unsourced: p.Counts.Unsourced, Failed: p.Counts.Failed, Missing: p.Counts.Missing},
		Missing: p.Missing, MissingAt: memoryIndexTime(p.MissingAt),
		Counting: memoryIndexCounting{Cursor: p.Counting.Cursor, Missing: p.Counting.Missing},
	}
	if run.Kind == "" {
		run.Kind = kind
	}
	return cloneMemoryIndexRun(run), node.CreatedAt, true, nil
}

// writeMemoryIndexRun writes the WHOLE run row through recordMemoryIndexRun,
// fenced to version: the time of the newest version the caller read, or zero
// when it read none (the row must then still be absent). A write after anybody
// else's answers errMemoryIndexRunChanged and writes nothing; the caller reads
// again and decides again (memql#5431). It answers the version it stored, which
// is the witness for the caller's NEXT write of the row: a caller that wrote
// keeps that, and never a version read afterwards, which may be somebody
// else's (saveRun).
//
// EVERY WRITER OF THE RUN ROWS GOES THROUGH HERE -- the job, its flip, and the
// owner's builtins -- because the fence protects a row only when all its
// writers carry one: an unfenced write would silently revert a cancel, or a
// cancel would roll back a cursor.
//
// THE NEW VERSION IS STAMPED AFTER THE ONE READ (ruling R12, memql#5431's third
// part): VersionTimeAfter(version, now) is the write's versionTime, so a
// replica whose clock lags the last writer's still writes the newest version
// rather than one that reads as older and is lost. (The fence's own clock,
// fenceWriteClock, makes the same guarantee for a fenced write; the stamp here
// states it on the write, the way every other writer of memory search's rows
// does.)
func (e *MemQLEngine) writeMemoryIndexRun(ctx context.Context, run MemoryIndexRun, version time.Time) (time.Time, error) {
	if run.Kind != memoryIndexKindSwitch && run.Kind != memoryIndexKindBackfill {
		return time.Time{}, fmt.Errorf("memory index: no run of kind %q", run.Kind)
	}
	fenced := ContextWithRowVersionFence(auth.ContextWithInternalOrigin(ctx), MemoryIndexRunConceptID, run.Kind, version)
	res, err := e.memoryIndexMutationResult(fenced, "platform.recordMemoryIndexRun", memoryIndexRunArgs(run, VersionTimeAfter(version, time.Now())))
	if err != nil {
		if errors.Is(err, ErrRowVersionChanged) || strings.Contains(err.Error(), ErrRowVersionChanged.Error()) {
			return time.Time{}, errMemoryIndexRunChanged
		}
		return time.Time{}, err
	}
	if written := memoryIndexWrittenVersion(res, MemoryIndexRunConceptID+":"+run.Kind); !written.IsZero() {
		return written, nil
	}
	// The result did not name the version it stored: read it back, and take it
	// only as THIS write. Anything else is another writer landing after it,
	// and its version must not become this writer's witness (saveRun).
	stored, at, ok, err := e.readMemoryIndexRun(ctx, run.Kind)
	if err != nil {
		return time.Time{}, err
	}
	if !ok || !memoryIndexRunsEqual(stored, cloneMemoryIndexRun(run)) {
		return time.Time{}, errMemoryIndexRunChanged
	}
	return at, nil
}

// memoryIndexRunArgs is run as recordMemoryIndexRun's arguments, written as
// the row version versionTime. A blank field is left out, and the mutation
// stamps it blank: every write states the row.
func memoryIndexRunArgs(run MemoryIndexRun, versionTime time.Time) map[string]any {
	counts := map[string]any{"total": run.Counts.Total, "done": run.Counts.Done, "skipped": run.Counts.Skipped,
		"unsourced": run.Counts.Unsourced, "failed": run.Counts.Failed, "missing": run.Counts.Missing}
	cursor := map[string]any{}
	for k, v := range run.Cursor {
		cursor[k] = v
	}
	counting := map[string]any{}
	for k, v := range run.Counting.Cursor {
		counting[k] = v
	}
	args := map[string]any{"kind": run.Kind, "state": run.State, "cursor": cursor, "counts": counts, "missing": run.Missing,
		"counting": map[string]any{"cursor": counting, "missing": run.Counting.Missing}}
	for key, v := range map[string]string{"targetRef": run.TargetRef, "targetTable": run.TargetTable, "previousRef": run.PreviousRef,
		"indexedBy": run.IndexedBy, "startedBy": run.StartedBy, "waitingReason": run.WaitingReason} {
		if strings.TrimSpace(v) != "" {
			args[key] = v
		}
	}
	if run.TargetDimensions > 0 {
		args["targetDimensions"] = run.TargetDimensions
	}
	memoryIndexTimeArg(args, "startedAt", run.StartedAt)
	memoryIndexTimeArg(args, "finishedAt", run.FinishedAt)
	memoryIndexTimeArg(args, "lastPassAt", run.LastPassAt)
	memoryIndexTimeArg(args, "missingAt", run.MissingAt)
	memoryIndexTimeArg(args, "versionTime", versionTime)
	return args
}
