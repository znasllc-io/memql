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
// AN ITEM NOBODY ANSWERS. A machine can take a call and never answer it, which
// no refusal says. TRIGGER: an attempt given the whole per-item timeout
// (memoryIndexItemTimeout) runs out of it. Such an attempt is not retried in
// the walk: a retry costs another whole timeout of a tick that has under two,
// and the old loop retried it until the tick's own limit stopped the walk
// BEFORE the item -- never counted, never waited for, tried first again by
// every tick for ever, with no reason shown and no backlog counted. So an item
// whose every attempt ran out the timeout is UNANSWERED: counted failed, and
// the walk goes on (the next walk tries it again). The SECOND unanswered item
// in a row (memoryIndexUnansweredInARow, the streak kept on the run as
// Counts.Unanswered, so it survives the tick between them) is no longer two
// slow items but a machine that is not answering: the run waits at it, with a
// plain reason (memoryIndexNotAnsweringReason), the item not counted, as for a
// machine that is offline; a waiting backfill counts its backlog meanwhile,
// and every tick tries that item again first, once. An answer of any kind --
// written, or refused -- ends the streak. An attempt that the TICK cut short,
// given less than the whole timeout, is not unanswered: the tick's time ran
// out, and the item is tried again, first, by the next tick. Cost: the first
// unanswered item of a hung machine is counted failed, which one item cannot
// tell apart from a slow one.
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
// that. A row whose id hashes its own text is exempt
// (MemorySource.ContentAddressed, asked of each row): its text never changes
// under the id.
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
//     records `finishing`, taking the previous model from the binding. A run
//     parked as `waiting` here (a tick that lost the job lock wrote its wait
//     meanwhile) resumes to `switching` and does the same.
//  4. The cache emptied, `finishing` not yet recorded: as 3, and the DELETE
//     runs again.
//  5. `finishing` recorded: the flip is over, and the walk after it follows.
//
// # No graph routing rule (ruling R3)
//
// The run rows have no graph.node.* routing rule (component/node/routing.go).
// Nothing subscribes to their row events: the OS polls memorySearchStatus, and
// forwarding one on every batch's write would wake every node's graph
// subscribers per 32 items. R3 removed only that rule. Like every graph write,
// a run-row write still emits cache.invalidate.v1:platform:memoryIndexRun,
// which the cache.invalidate.* rule carries to every node, where the result
// cache's evictor alone reads it: one small mesh event per write. The
// binding's own rule already tells every replica about a flip.
//
// # The files
//
// This file holds the job's decisions: the tick, its walks, and when it
// waits. memory_index_world.go is its world over Postgres (the step builtin,
// the one-runner lock, the store path), memory_index_flip.go the flip, and
// memory_index_run.go the run rows as stored, read and written.
//
// Regression tests: memory_index_job_test.go (the decisions, over maps) and
// memory_index_job_db_test.go (Postgres, two engines over one database).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/work"
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
	// tick's budget: one hung machine must not hold the job past its tick. An
	// attempt that runs the whole of it out is not retried (the header's "An
	// item nobody answers").
	memoryIndexItemTimeout = 30 * time.Second
	// memoryIndexUnansweredInARow is how many items in a row, each unanswered
	// for the whole per-item timeout, make the run wait for a machine that is
	// not answering; the last of them is not counted failed.
	memoryIndexUnansweredInARow = 2
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
// An item with no Version -- a row whose id hashes its text, or an item of a
// source that pages its own items -- is current whenever it is present.
func memoryIndexCurrent(item MemoryItem, written map[string]time.Time) bool {
	at, ok := written[item.Id]
	if !ok {
		return false
	}
	return item.Version.IsZero() || !item.Version.After(at.Add(memoryIndexClockSlack))
}

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
//
// Unanswered is not a count of the walk but a streak: the items in a row,
// ending with the last one the walk tried to embed, whose every attempt ran
// out the whole per-item timeout (the header's "An item nobody answers"). An
// item that answered, written or refused, sets it back to zero; one the walk
// did not try (already present, or its source cannot rebuild it) leaves it.
type MemoryIndexCounts struct{ Total, Done, Skipped, Unsourced, Failed, Missing, Unanswered int }

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
//
// A SWITCH WHOSE BINDING NAMES ITS TARGET BUT WHOSE RUN NAMES NO PREVIOUS MODEL
// resumes to `switching`. The flip records the previous model together with
// `finishing` (flipMemoryIndex), so such a run never recorded the end of its
// flip: a tick that had lost the job lock wrote its wait between another
// tick's binding write and that `finishing`, and the other tick's write was
// refused by the fence. Resumed as `finishing`, the rest of the flip -- the
// semantic answer cache emptied, the previous model recorded -- would never
// run; resumed as `switching`, it runs (ruling R6), and work takes it without
// the indexer check.
func memoryIndexResumeState(run MemoryIndexRun, active EmbedderBinding) string {
	switch {
	case run.Kind != memoryIndexKindSwitch:
		return memoryIndexStateIndexing
	case active.ProviderRef == run.TargetRef && active.Dimensions == run.TargetDimensions && strings.TrimSpace(run.PreviousRef) == "":
		return memoryIndexStateSwitching
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

// memoryIndexNotAnsweringReason is the wait of a run whose machine took items
// and answered none in the whole per-item timeout, memoryIndexUnansweredInARow
// in a row: a fleet model's indexer's machines, in the words the screen says
// from the reader's side, or a vendor model in its own name.
func memoryIndexNotAnsweringReason(ref string) string {
	if model, fleet := IsFleetReference(ref); fleet {
		return fmt.Sprintf("the indexer's machines offering %s are not answering", model)
	}
	return fmt.Sprintf("%s is not answering", strings.TrimSpace(ref))
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
// indexer's standing. Parked instead, those steps would wait for an owner
// they do not need. So a run in `switching` with the binding moved takes them
// at once, and so does a run parked as `waiting` in that state, which resumes
// to `switching` (memoryIndexResumeState). The walk after the flip is checked
// as usual, and waits for an owner.
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
		resume := memoryIndexResumeState(t.run, t.active)
		// Still refused: the run waits on, unchanged (save writes nothing). The
		// rest of a flip whose binding moved is not refused (above).
		if resume != memoryIndexStateSwitching {
			if reason, wait := t.indexerWait(ctx); wait {
				return t.wait(ctx, reason)
			}
		}
		t.run.State, t.run.WaitingReason = resume, ""
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
			tried := t.embed(ctx, s, item)
			if tried.unanswered && t.run.Counts.Unanswered+1 >= memoryIndexUnansweredInARow {
				// Not one slow item but a machine that is not answering: a
				// wait, and this item is not counted (the header's "An item
				// nobody answers").
				tried = memoryIndexAttempt{reason: memoryIndexNotAnsweringReason(t.run.TargetRef)}
			}
			if tried.stopped || tried.reason != "" {
				// Resume at this item: everything before it is counted.
				t.run.Cursor = map[string]string{s.Name: last}
				if tried.reason != "" {
					return true, t.wait(ctx, tried.reason)
				}
				_, serr := t.save(ctx)
				return true, serr
			}
			t.run.Counts.Total++
			switch {
			case tried.unanswered:
				t.log.Warn("memory index: an item was not answered in the whole per-item timeout; it counts as failed and the next walk tries it again",
					"source", s.Name, "id", item.Id, "target", t.run.TargetRef, "timeout", memoryIndexItemTimeout)
				t.run.Counts.Failed++
				t.run.Counts.Missing++
				t.run.Counts.Unanswered++
			case tried.err != nil:
				t.log.Warn("memory index: an item could not be indexed; it counts as failed and the next walk tries it again",
					"source", s.Name, "id", item.Id, "target", t.run.TargetRef, "error", tried.err)
				t.run.Counts.Failed++
				t.run.Counts.Missing++
				t.run.Counts.Unanswered = 0
			default:
				t.run.Counts.Done++
				t.run.Counts.Unanswered = 0
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

// memoryIndexAttempt is how embedding one item ended (embed).
type memoryIndexAttempt struct {
	// reason is set when the model cannot be reached at all: the run waits.
	reason string
	// stopped is the tick's own time, or its context, running out: the item
	// is tried again, first, by the next tick.
	stopped bool
	// unanswered is every attempt having run out the whole per-item timeout
	// (the header's "An item nobody answers"); err is then that timeout.
	unanswered bool
	// err is the last attempt's error; nil when the item was written.
	err error
}

// embed tries one item up to memoryIndexAttempts times, each attempt bounded
// by memoryIndexItemTimeout and by what is left of the tick. An attempt that
// ran out the WHOLE per-item timeout is not retried; an attempt the tick cut
// short is the tick's time running out (the header's "An item nobody
// answers").
func (t *memoryIndexTick) embed(ctx context.Context, s MemorySource, item MemoryItem) memoryIndexAttempt {
	var err error
	answered := false
	for attempt := 1; attempt <= memoryIndexAttempts; attempt++ {
		remaining := t.hard.Sub(t.w.now())
		if remaining <= 0 {
			return memoryIndexAttempt{stopped: true}
		}
		limit := min(remaining, memoryIndexItemTimeout)
		call, cancel := context.WithTimeout(ctx, limit)
		err = t.w.embed(call, t.run, s, item)
		// The call's own deadline, or a store that says it ran out of time.
		timedOut := errors.Is(call.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
		cancel()
		switch {
		case err == nil:
			return memoryIndexAttempt{}
		case ctx.Err() != nil:
			return memoryIndexAttempt{stopped: true}
		}
		if reason, wait := memoryIndexWaitReason(err, t.run.TargetRef); wait {
			return memoryIndexAttempt{reason: reason}
		}
		if timedOut && limit == memoryIndexItemTimeout {
			return memoryIndexAttempt{unanswered: !answered, err: err}
		}
		if !t.w.now().Before(t.hard) {
			return memoryIndexAttempt{stopped: true}
		}
		answered = answered || !timedOut
	}
	return memoryIndexAttempt{err: err}
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
