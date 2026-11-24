package memql

// memory_index_job_test.go -- the index job's DECISIONS, over a world of maps
// (design record 2026-11-23, sections 4 and 6). What a tick does with Postgres
// -- the lock, the fenced run rows, the flip's writes, two replicas -- is
// memory_index_job_db_test.go's.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// memoryIndexFake is a memoryIndexWorld over maps. Every save and every embed
// is appended to events in the order the tick made it, so a test can say what
// happened before what.
type memoryIndexFake struct {
	clock      time.Time
	active     EmbedderBinding
	runs       map[string]MemoryIndexRun
	srcs       []MemorySource
	rows       map[string][]MemoryItem
	sourceErr  map[string]error
	tables     map[string]map[string]bool
	embedErr   func(run MemoryIndexRun, item MemoryItem) error
	owners     map[string]bool
	unsourcedN int
	events     []string
	saves      []MemoryIndexRun
	attempts   map[string]int
	embedded   map[string]int
	flips      int
}

func memoryIndexItemId(n int) string { return fmt.Sprintf("v1:test:item:%03d", n) }

const (
	fakeOldRef = "fleet:old-embedder"
	fakeNewRef = "fleet:new-embedder"
	fakeOwner  = "v1:identity:user:owner-1"
)

// newMemoryIndexFake is one source, "alpha", of n items, an active fleet
// binding indexed by an owner, and a switch toward another fleet model that
// the same owner started.
func newMemoryIndexFake(n int) *memoryIndexFake {
	f := &memoryIndexFake{
		clock:     time.Now().UTC(),
		active:    EmbedderBinding{ProviderRef: fakeOldRef, Dimensions: 4, IndexedBy: fakeOwner, ActivatedAt: "2026-11-01T00:00:00Z"},
		runs:      map[string]MemoryIndexRun{},
		rows:      map[string][]MemoryItem{},
		sourceErr: map[string]error{},
		tables:    map[string]map[string]bool{},
		owners:    map[string]bool{fakeOwner: true},
		attempts:  map[string]int{},
		embedded:  map[string]int{},
	}
	f.addSource("alpha", n)
	f.runs[memoryIndexKindSwitch] = MemoryIndexRun{
		Kind: memoryIndexKindSwitch, State: memoryIndexStateIndexing,
		TargetRef: fakeNewRef, TargetDimensions: 4, IndexedBy: fakeOwner, StartedBy: fakeOwner,
		StartedAt: f.clock.Add(-time.Minute), Cursor: map[string]string{},
	}
	return f
}

func (f *memoryIndexFake) addSource(name string, n int) {
	f.srcs = append(f.srcs, MemorySource{Name: name, Concept: "v1:test:item", VectorField: "content",
		Items: func(context.Context, string, int) ([]MemoryItem, string, error) { return nil, "", nil }})
	sort.Slice(f.srcs, func(a, b int) bool { return f.srcs[a].Name < f.srcs[b].Name })
	for i := 1; i <= n; i++ {
		f.rows[name] = append(f.rows[name], MemoryItem{Id: memoryIndexItemId(i), Text: fmt.Sprintf("item %d", i)})
	}
}

func (f *memoryIndexFake) table(ref string, dims int) string {
	t, _ := EmbeddingVectorTable(ref, dims)
	return t
}

// fill marks every item of every source present in a model's table.
func (f *memoryIndexFake) fill(ref string, dims int) {
	t := f.table(ref, dims)
	if f.tables[t] == nil {
		f.tables[t] = map[string]bool{}
	}
	for _, items := range f.rows {
		for _, item := range items {
			f.tables[t]["content|"+item.Id] = true
		}
	}
}

func (f *memoryIndexFake) activeBinding(context.Context) (EmbedderBinding, error) {
	if !f.active.Valid() {
		return EmbedderBinding{}, ErrNoEmbedderBound
	}
	return f.active, nil
}

func (f *memoryIndexFake) readRun(_ context.Context, kind string) (MemoryIndexRun, bool, error) {
	run, ok := f.runs[kind]
	return cloneMemoryIndexRun(run), ok, nil
}

func (f *memoryIndexFake) saveRun(_ context.Context, run MemoryIndexRun) error {
	run = cloneMemoryIndexRun(run)
	f.runs[run.Kind] = run
	f.saves = append(f.saves, run)
	f.events = append(f.events, "save "+run.State+" "+fmt.Sprint(run.Cursor))
	return nil
}

func (f *memoryIndexFake) isActiveOwner(_ context.Context, userId string) bool {
	return f.owners[userId]
}

func (f *memoryIndexFake) sources() []MemorySource { return f.srcs }

func (f *memoryIndexFake) items(_ context.Context, s MemorySource, after string, limit int) ([]MemoryItem, string, error) {
	if err := f.sourceErr[s.Name]; err != nil {
		return nil, "", err
	}
	var rest []MemoryItem
	for _, item := range f.rows[s.Name] {
		if item.Id > after {
			rest = append(rest, item)
		}
	}
	if len(rest) <= limit {
		return rest, "", nil
	}
	return rest[:limit], rest[limit-1].Id, nil
}

func (f *memoryIndexFake) ensureTable(_ context.Context, ref string, dims int) (string, error) {
	t := f.table(ref, dims)
	if f.tables[t] == nil {
		f.tables[t] = map[string]bool{}
	}
	return t, nil
}

func (f *memoryIndexFake) vectorTimes(_ context.Context, table, field string, ids []string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	for _, id := range ids {
		if f.tables[table][field+"|"+id] {
			out[id] = f.clock
		}
	}
	return out, nil
}

func (f *memoryIndexFake) embed(_ context.Context, run MemoryIndexRun, s MemorySource, item MemoryItem) error {
	f.attempts[item.Id]++
	f.events = append(f.events, "embed "+item.Id)
	if f.embedErr != nil {
		if err := f.embedErr(run, item); err != nil {
			return err
		}
	}
	f.embedded[item.Id]++
	t := f.table(run.TargetRef, run.TargetDimensions)
	if f.tables[t] == nil {
		f.tables[t] = map[string]bool{}
	}
	f.tables[t][s.VectorField+"|"+item.Id] = true
	return nil
}

func (f *memoryIndexFake) unsourced(context.Context, string, []MemorySource) (int, error) {
	return f.unsourcedN, nil
}

func (f *memoryIndexFake) flip(_ context.Context, run MemoryIndexRun) (MemoryIndexRun, error) {
	f.flips++
	previous := f.active.ProviderRef
	f.active = EmbedderBinding{ProviderRef: run.TargetRef, Dimensions: run.TargetDimensions, IndexedBy: run.IndexedBy, ActivatedAt: f.clock.Format(time.RFC3339Nano)}
	run = cloneMemoryIndexRun(run)
	run.State, run.PreviousRef = memoryIndexStateFinishing, previous
	run.Cursor, run.Counts = map[string]string{}, MemoryIndexCounts{}
	f.runs[run.Kind] = run
	f.events = append(f.events, "flip "+run.TargetRef)
	return run, nil
}

func (f *memoryIndexFake) now() time.Time { return f.clock }

func (f *memoryIndexFake) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// savesIn returns the saves made while the run was in one state.
func (f *memoryIndexFake) savesIn(state string) []MemoryIndexRun {
	var out []MemoryIndexRun
	for _, s := range f.saves {
		if s.State == state {
			out = append(out, s)
		}
	}
	return out
}

func indexOfEvent(events []string, prefix string) int {
	for i, e := range events {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

// A batch is 32 items, and a batch that embedded saves its cursor and counts
// BEFORE the next batch starts: a restart, or another replica, resumes after
// the last item embedded and embeds nothing twice. A batch that only found its
// items already present is the exception: losing its cursor costs a re-read of
// presence and no embedding, so it is saved with the tick's end rather than
// writing a row version per 32 present items on every backfill pass.
func TestAWalkSavesItsCursorEveryBatch(t *testing.T) {
	f := newMemoryIndexFake(70)
	require.NoError(t, runMemoryIndexTick(context.Background(), f, time.Hour))

	walk := f.savesIn(memoryIndexStateIndexing)
	require.Len(t, walk, 2, "batches one and two each saved; the third ends the walk")
	require.Equal(t, map[string]string{"alpha": memoryIndexItemId(32)}, walk[0].Cursor)
	require.Equal(t, MemoryIndexCounts{Total: 32, Done: 32}, walk[0].Counts)
	require.Equal(t, map[string]string{"alpha": memoryIndexItemId(64)}, walk[1].Cursor)
	require.Equal(t, MemoryIndexCounts{Total: 64, Done: 64}, walk[1].Counts)

	// The save of a batch comes after its last item and before the next batch.
	saved := indexOfEvent(f.events, "save "+memoryIndexStateIndexing)
	require.Greater(t, saved, indexOfEvent(f.events, "embed "+memoryIndexItemId(32)))
	require.Less(t, saved, indexOfEvent(f.events, "embed "+memoryIndexItemId(33)))

	// The end of the walk is saved as the start of the catch-up walk.
	catchUp := f.savesIn(memoryIndexStateCatchingUp)
	require.NotEmpty(t, catchUp)
	require.Empty(t, catchUp[0].Cursor, "the catch-up walk starts from the first source")
	require.Equal(t, MemoryIndexCounts{}, catchUp[0].Counts)
	require.False(t, catchUp[0].LastPassAt.IsZero(), "the first walk's end is recorded")

	for i := 1; i <= 70; i++ {
		require.Equal(t, 1, f.embedded[memoryIndexItemId(i)], "item %d embedded once over the whole switch", i)
	}
	require.Equal(t, memoryIndexStateDone, f.runs[memoryIndexKindSwitch].State)

	// A batch of items already present saves nothing of its own; the tick's end
	// keeps its cursor.
	f = newMemoryIndexFake(70)
	f.fill(fakeNewRef, 4)
	run := f.runs[memoryIndexKindSwitch]
	run.State = memoryIndexStateCatchingUp
	f.runs[memoryIndexKindSwitch] = run
	require.NoError(t, runMemoryIndexTick(context.Background(), f, 0))
	require.Len(t, f.saves, 1, "one batch, nothing embedded: one save, at the tick's end")
	require.Equal(t, map[string]string{"alpha": memoryIndexItemId(32)}, f.saves[0].Cursor)
	require.Equal(t, MemoryIndexCounts{Total: 32, Skipped: 32}, f.saves[0].Counts)
	require.Empty(t, f.embedded)
}

// The indexer's machines all offline is a WAIT: the run says why, keeps its
// cursor, counts nothing failed, and the next tick picks up where it stopped.
func TestAnOfflineIndexerMakesTheRunWaitNotFail(t *testing.T) {
	refusal := errors.New("every_door_shut: no door to a model is open for this call [pinned to fleet:new-embedder: a pinned source is never substituted]; fleet:new-embedder (local): no machine offering new-embedder is online")
	f := newMemoryIndexFake(10)
	offline := true
	f.embedErr = func(run MemoryIndexRun, item MemoryItem) error {
		if offline && item.Id >= memoryIndexItemId(4) {
			return fmt.Errorf("store: embed: resolve provider %q: %w", run.TargetRef, refusal)
		}
		return nil
	}
	require.NoError(t, runMemoryIndexTick(context.Background(), f, time.Hour))
	run := f.runs[memoryIndexKindSwitch]
	require.Equal(t, memoryIndexStateWaiting, run.State)
	require.Equal(t, "none of the indexer's machines offering new-embedder is online", run.WaitingReason)
	require.Equal(t, MemoryIndexCounts{Total: 3, Done: 3}, run.Counts, "nothing is counted failed while it waits")
	require.Equal(t, map[string]string{"alpha": memoryIndexItemId(3)}, run.Cursor)
	require.Equal(t, 1, f.attempts[memoryIndexItemId(4)], "a wait is not retried within the tick")

	// Still offline: the next tick tries again and writes nothing new.
	saves := len(f.saves)
	require.NoError(t, runMemoryIndexTick(context.Background(), f, time.Hour))
	require.Len(t, f.saves, saves, "an unchanged wait is not written again")
	require.Equal(t, 2, f.attempts[memoryIndexItemId(4)])

	// The machine is back: the walk resumes at item 4 and embeds nothing twice.
	offline = false
	require.NoError(t, runMemoryIndexTick(context.Background(), f, time.Hour))
	for i := 1; i <= 10; i++ {
		require.Equal(t, 1, f.embedded[memoryIndexItemId(i)], "item %d", i)
	}
	require.Equal(t, memoryIndexStateDone, f.runs[memoryIndexKindSwitch].State)
	require.Empty(t, f.runs[memoryIndexKindSwitch].WaitingReason)
	require.Equal(t, 1, f.flips)

	// The resolver's own re-check of the indexer (ErrEmbeddingIndexerNotOwner)
	// is the same wait, with the owner's reason.
	f = newMemoryIndexFake(3)
	f.embedErr = func(MemoryIndexRun, MemoryItem) error { return fmt.Errorf("store: %w", ErrEmbeddingIndexerNotOwner) }
	require.NoError(t, runMemoryIndexTick(context.Background(), f, time.Hour))
	run = f.runs[memoryIndexKindSwitch]
	require.Equal(t, memoryIndexStateWaiting, run.State)
	require.Equal(t, memoryIndexOwnerReason, run.WaitingReason)
	require.Zero(t, run.Counts.Failed)

	// An indexer who is no longer an active owner is noticed at the start of
	// the tick, before anything is embedded.
	f = newMemoryIndexFake(3)
	delete(f.owners, fakeOwner)
	require.NoError(t, runMemoryIndexTick(context.Background(), f, time.Hour))
	run = f.runs[memoryIndexKindSwitch]
	require.Equal(t, memoryIndexStateWaiting, run.State)
	require.Equal(t, "the owner who chose this model is no longer an owner; an owner can choose again", run.WaitingReason)
	require.Empty(t, f.attempts, "nothing is embedded on a former owner's machines")

	// The plain words are the classification's, whatever wraps them.
	for _, err := range []error{
		fmt.Errorf("embed: %w", ErrFleetUnavailable),
		errors.New("store: embed: resolve provider \"fleet:new-embedder\": no machine offering new-embedder is online"),
		refusal,
	} {
		reason, wait := memoryIndexWaitReason(err, fakeNewRef)
		require.True(t, wait, "%v", err)
		require.Equal(t, "none of the indexer's machines offering new-embedder is online", reason)
	}
	_, wait := memoryIndexWaitReason(errors.New("embedding model returned a zero vector"), fakeNewRef)
	require.False(t, wait, "a bad answer is a failure, not a wait")
}

// The flip is automatic only when the catch-up walk left nothing failed and
// the active index holds nothing a source cannot rebuild. Otherwise the switch
// holds for the owner (needsDecision) and search keeps the active model.
func TestFailuresOrUnsourcedItemsHoldTheFlip(t *testing.T) {
	for _, tc := range []struct {
		name          string
		arrange       func(f *memoryIndexFake)
		held          bool
		failed, unsrc int
	}{
		{name: "an item that keeps failing", held: true, failed: 1, arrange: func(f *memoryIndexFake) {
			f.embedErr = func(_ MemoryIndexRun, item MemoryItem) error {
				if item.Id == memoryIndexItemId(2) {
					return errors.New("embedding model returned a zero vector")
				}
				return nil
			}
		}},
		{name: "items the active index holds and no source rebuilds", held: true, unsrc: 2, arrange: func(f *memoryIndexFake) {
			f.unsourcedN = 2
		}},
		{name: "an item its source cannot rebuild", held: true, failed: 1, arrange: func(f *memoryIndexFake) {
			f.rows["alpha"][4] = MemoryItem{Id: f.rows["alpha"][4].Id, Failure: "its stored payload does not decode"}
		}},
		{name: "a source that cannot be read", held: true, failed: 1, arrange: func(f *memoryIndexFake) {
			f.addSource("beta", 2)
			f.sourceErr["beta"] = errors.New("Training lessons cannot be read: the Library plug-in has no database")
		}},
		{name: "every item indexed", held: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMemoryIndexFake(6)
			if tc.arrange != nil {
				tc.arrange(f)
			}
			require.NoError(t, runMemoryIndexTick(context.Background(), f, time.Hour))
			run := f.runs[memoryIndexKindSwitch]
			if !tc.held {
				require.Equal(t, 1, f.flips)
				require.Equal(t, fakeNewRef, f.active.ProviderRef)
				require.Equal(t, memoryIndexStateDone, run.State)
				return
			}
			require.Zero(t, f.flips, "no automatic flip")
			require.Equal(t, fakeOldRef, f.active.ProviderRef, "search keeps the active model")
			require.Equal(t, memoryIndexStateNeedsDecision, run.State)
			require.Equal(t, tc.failed, run.Counts.Failed)
			require.Equal(t, tc.unsrc, run.Counts.Unsourced)
			require.Equal(t, run.Counts.Total, run.Counts.Done+run.Counts.Skipped+run.Counts.Failed)
		})
	}

	// A failing item is retried a few times in each walk, then counted.
	f := newMemoryIndexFake(3)
	f.embedErr = func(_ MemoryIndexRun, item MemoryItem) error {
		if item.Id == memoryIndexItemId(2) {
			return errors.New("embedding model returned a zero vector")
		}
		return nil
	}
	require.NoError(t, runMemoryIndexTick(context.Background(), f, time.Hour))
	require.Equal(t, 2*memoryIndexAttempts, f.attempts[memoryIndexItemId(2)], "the first walk and the catch-up walk each try it")
	require.Equal(t, 1, f.attempts[memoryIndexItemId(3)], "the job goes on past it")
}

// A needsDecision switch is the owner's move: the job leaves it alone and keeps
// the ACTIVE index whole with the backfill, at most once every ten minutes.
func TestAHeldSwitchLeavesTheBackfillRunning(t *testing.T) {
	f := newMemoryIndexFake(4)
	f.fill(fakeOldRef, 4)
	delete(f.tables[f.table(fakeOldRef, 4)], "content|"+memoryIndexItemId(3))
	run := f.runs[memoryIndexKindSwitch]
	run.State = memoryIndexStateNeedsDecision
	f.runs[memoryIndexKindSwitch] = run
	require.NoError(t, runMemoryIndexTick(context.Background(), f, time.Hour))
	require.Equal(t, memoryIndexStateNeedsDecision, f.runs[memoryIndexKindSwitch].State)
	backfill := f.runs[memoryIndexKindBackfill]
	require.Equal(t, memoryIndexStateDone, backfill.State)
	require.Equal(t, fakeOldRef, backfill.TargetRef)
	require.Equal(t, fakeOwner, backfill.IndexedBy, "the backfill embeds as the active model's indexer")
	require.Equal(t, MemoryIndexCounts{Total: 4, Done: 1, Skipped: 3}, backfill.Counts)
	require.Equal(t, map[string]int{memoryIndexItemId(3): 1}, f.embedded)

	// Inside ten minutes nothing is walked again.
	saves := len(f.saves)
	f.clock = f.clock.Add(9 * time.Minute)
	require.NoError(t, runMemoryIndexTick(context.Background(), f, time.Hour))
	require.Len(t, f.saves, saves)
	f.clock = f.clock.Add(2 * time.Minute)
	require.NoError(t, runMemoryIndexTick(context.Background(), f, time.Hour))
	require.Greater(t, len(f.saves), saves, "past ten minutes the backfill walks again")
}

// The step is the job's own: a client cannot run it.
func TestTheIndexStepRefusesACallWithoutInternalOrigin(t *testing.T) {
	e := auditTestEngine(t)
	_, err := e.memoryIndexStepBuiltin(auth.ContextWithClientOrigin(auth.ContextWithUserActor(context.Background(), "v1:identity:user:somebody")), map[string]any{"budgetSeconds": 45}, 0)
	require.ErrorContains(t, err, "memory index job")
}

// THE JOB'S BOOKKEEPING EXPIRES. A run row is written after every batch of 32
// and nothing reads a superseded version, so only its newest version outlives
// a day (the v1:pipelines:run precedent, status_row_retention_test.go).
func TestTheIndexRunKeepsOnlyItsNewestVersionPastADay(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	c, err := memorynodes.Get(MemoryIndexRunConceptID)
	require.NoError(t, err)
	require.NotNil(t, c.Retention)
	require.Equal(t, memorynodes.RetentionPolicy{TTL: 24 * time.Hour, KeepLatest: true, Archive: "none"}, *c.Retention)
}

// A vector counts as its row's while it is not older than the row's newest
// version, give or take the clock slack between the writing replica and the
// database; an item with no version (content-addressed) is current whenever
// its vector is present (ruling R7).
func TestAVectorOlderThanItsRowIsMissing(t *testing.T) {
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	written := map[string]time.Time{"v": at}
	for _, tc := range []struct {
		name    string
		item    MemoryItem
		current bool
	}{
		{"absent", MemoryItem{Id: "absent"}, false},
		{"no version", MemoryItem{Id: "v"}, true},
		{"row written before its vector", MemoryItem{Id: "v", Version: at.Add(-time.Minute)}, true},
		{"row stamped by a clock a little ahead", MemoryItem{Id: "v", Version: at.Add(memoryIndexClockSlack)}, true},
		{"row rewritten after its vector", MemoryItem{Id: "v", Version: at.Add(memoryIndexClockSlack + time.Millisecond)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.current, memoryIndexCurrent(tc.item, written))
		})
	}
}
