package memql

// memory_search_builtins_test.go -- the cluster owner's memory-search builtins
// and the daily cleanup, as DECISIONS over a world of maps (design record
// 2026-11-23, sections 3, 6, 7 and 8). The world shares its binding, run rows
// and tables with the index job's map world (memoryIndexFake), so a test can make
// an owner's act and then run the job's tick over the same state. What the
// builtins do with Postgres -- the fence, the retained table reused, the drop --
// is memory_search_builtins_db_test.go's.

import (
	"context"
	"encoding/json"
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
)

// memorySearchPart is the capability resource every owner builtin names.
const memorySearchPart = "app:cluster/memorysearch"

// memorySearchFake is a memorySearchWorld over the index job's map world.
type memorySearchFake struct {
	job         *memoryIndexFake
	widths      map[string]int
	retiredRows map[string]retiredMemoryIndex
	catalogs    map[string][]FleetModel
	vendors     []vendorEmbedderRecord
	names       map[string]string

	locked   bool
	writeErr error

	events      []string
	runAttempts int
	probes      []string
	binds       []string
	choices     []embedderChoice
}

func newMemorySearchFake(n int) *memorySearchFake {
	return &memorySearchFake{
		job:         newMemoryIndexFake(n),
		widths:      map[string]int{fakeOldRef: 4, fakeNewRef: 4},
		retiredRows: map[string]retiredMemoryIndex{},
		catalogs:    map[string][]FleetModel{},
		names:       map[string]string{},
	}
}

// noSwitch removes the switch the job's world starts with.
func (f *memorySearchFake) noSwitch() *memorySearchFake {
	delete(f.job.runs, memoryIndexKindSwitch)
	return f
}

func (f *memorySearchFake) setSwitchState(state string) {
	run := f.job.runs[memoryIndexKindSwitch]
	run.State = state
	f.job.runs[memoryIndexKindSwitch] = run
}

func (f *memorySearchFake) binding(context.Context) (EmbedderBinding, error) {
	return f.job.activeBinding(context.Background())
}

func (f *memorySearchFake) lockBinding(context.Context) (func(), error) {
	if f.locked {
		return nil, errors.New("MemQL is already working in this conversation")
	}
	f.locked = true
	f.events = append(f.events, "lock")
	return func() {
		f.locked = false
		f.events = append(f.events, "unlock")
	}, nil
}

func (f *memorySearchFake) readRun(_ context.Context, kind string) (MemoryIndexRun, time.Time, bool, error) {
	run, ok := f.job.runs[kind]
	return cloneMemoryIndexRun(run), f.job.clock, ok, nil
}

func (f *memorySearchFake) writeRun(_ context.Context, run MemoryIndexRun, _ time.Time) error {
	f.runAttempts++
	if f.writeErr != nil {
		return f.writeErr
	}
	f.job.runs[run.Kind] = cloneMemoryIndexRun(run)
	f.events = append(f.events, fmt.Sprintf("writeRun %s locked=%t", run.State, f.locked))
	return nil
}

func (f *memorySearchFake) writeChoice(_ context.Context, c embedderChoice) error {
	f.choices = append(f.choices, c)
	f.events = append(f.events, fmt.Sprintf("writeChoice locked=%t", f.locked))
	if c.IndexedBy != "" {
		f.job.active.IndexedBy = c.IndexedBy
	}
	if c.ChosenBy != "" {
		f.job.active.ChosenBy = c.ChosenBy
	}
	return nil
}

func (f *memorySearchFake) bind(_ context.Context, provider string, c embedderChoice) error {
	f.binds = append(f.binds, provider)
	dims, err := f.probe(context.Background(), provider, c.IndexedBy)
	if err != nil {
		return err
	}
	f.job.active = EmbedderBinding{ProviderRef: provider, Dimensions: dims, IndexedBy: c.IndexedBy, ChosenBy: c.ChosenBy,
		ActivatedAt: f.job.clock.Format(time.RFC3339Nano)}
	_, _ = f.job.ensureTable(context.Background(), provider, dims)
	return nil
}

func (f *memorySearchFake) probe(_ context.Context, provider, indexer string) (int, error) {
	f.probes = append(f.probes, provider+" as "+indexer)
	dims, ok := f.widths[provider]
	if !ok {
		return 0, fmt.Errorf("no machine offering %s is online", provider)
	}
	return dims, nil
}

func (f *memorySearchFake) ensureTable(ctx context.Context, ref string, dims int) (string, error) {
	return f.job.ensureTable(ctx, ref, dims)
}

func (f *memorySearchFake) vectors(_ context.Context, table string) (int, error) {
	return len(f.job.tables[table]), nil
}

func (f *memorySearchFake) retired(context.Context) ([]retiredMemoryIndex, error) {
	out := make([]retiredMemoryIndex, 0, len(f.retiredRows))
	for _, r := range f.retiredRows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProviderRef < out[j].ProviderRef })
	return out, nil
}

func (f *memorySearchFake) recordRetired(_ context.Context, r retiredMemoryIndex) error {
	f.retiredRows[RetiredEmbedderBindingID(r.ProviderRef, r.Dimensions)] = r
	f.events = append(f.events, fmt.Sprintf("recordRetired %s locked=%t", r.ProviderRef, f.locked))
	return nil
}

func (f *memorySearchFake) dropTable(_ context.Context, table string) error {
	delete(f.job.tables, table)
	return nil
}

func (f *memorySearchFake) isActiveOwner(_ context.Context, userId string) bool {
	return f.job.owners[userId]
}

func (f *memorySearchFake) catalog(_ context.Context, userId string) ([]FleetModel, error) {
	return f.catalogs[userId], nil
}

func (f *memorySearchFake) vendorEmbedders() []vendorEmbedderRecord { return f.vendors }

func (f *memorySearchFake) displayName(_ context.Context, userId string) string {
	return f.names[userId]
}

func (f *memorySearchFake) now() time.Time { return f.job.clock }

func (f *memorySearchFake) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (f *memorySearchFake) retiredOf(ref string, dims int) (retiredMemoryIndex, bool) {
	r, ok := f.retiredRows[RetiredEmbedderBindingID(ref, dims)]
	return r, ok
}

const memorySearchOtherOwner = "v1:identity:user:owner-2"

// TestOnlyTheOwnerHoldsTheMemorySearchParts: `read` and `execute` on
// app:cluster/memorysearch are seeded on the owner and nobody else, each
// builtin names the part it needs, and a developer is refused before any
// handler runs. The cleanup has no part: only its schedule runs it.
func TestOnlyTheOwnerHoldsTheMemorySearchParts(t *testing.T) {
	cat, _ := seededCatalog(t)
	for _, verb := range []string{auth.VerbRead, auth.VerbExecute} {
		for _, role := range []string{"owner", "developer", "admin", "user", "viewer"} {
			require.Equal(t, role == "owner", cat.Holds(role, verb, memorySearchPart), "%s holds %s %s", role, verb, memorySearchPart)
		}
	}

	installSeededCatalog(t)
	e := treeBuiltinEngine(t)
	for name, verb := range map[string]string{
		BuiltinExecutorMemorySearchStatus:       auth.VerbRead,
		BuiltinExecutorMemorySearchChoices:      auth.VerbRead,
		BuiltinExecutorMemorySearchSwitch:       auth.VerbExecute,
		BuiltinExecutorMemorySearchCancel:       auth.VerbExecute,
		BuiltinExecutorMemorySearchSwitchAnyway: auth.VerbExecute,
		BuiltinExecutorMemorySearchConfirm:      auth.VerbExecute,
	} {
		fn, err := e.functions.Get(name)
		require.NoError(t, err, name)
		require.NotNil(t, fn, name)
		// THE CONTROL COMES FROM THE TREE: a builtin that lost its annotation
		// would otherwise be admitted for everybody below.
		require.Equal(t, CapabilityRequirement{Verb: verb, Resource: memorySearchPart}, fn.RequiresCapability, name)

		_, err = e.evaluateBuiltinFunctionExpression(asCaller("developer"), builtinCall(name), 0)
		require.Error(t, err, "%s answered a developer", name)
		require.True(t, strings.HasPrefix(err.Error(), CodeCapabilityNotHeld+": "), "%s refused a developer with %q", name, err)
		require.Contains(t, err.Error(), memorySearchPart)

		// The reachable positive: the owner passes the gate. What the handler
		// then does is the rest of this file's.
		require.NoError(t, e.refuseBuiltinBelowRequiredCapability(asCaller("owner"), builtinCall(name)), name)
	}

	_, err := e.memoryIndexRetireBuiltin(auth.ContextWithClientOrigin(asCaller("owner")), nil, 0)
	require.ErrorContains(t, err, "daily cleanup")
}

// A cluster with no model binds the first one at once: nothing is indexed under
// another model, so there is nothing to rebuild. A fleet model is indexed on the
// chooser's machines; a vendor model on nobody's. An active model whose index
// holds no vector is the same case for search: the builtin writes the switch as
// `switching`, moves nothing itself, and the job's next tick flips it.
func TestAFirstBindingOnAnEmptyCorpusActivatesAtOnce(t *testing.T) {
	ctx := context.Background()

	f := newMemorySearchFake(3).noSwitch()
	f.job.active = EmbedderBinding{}
	require.NoError(t, memorySearchSwitch(ctx, f, fakeOwner, fakeNewRef))
	require.Equal(t, []string{fakeNewRef}, f.binds)
	require.Equal(t, fakeNewRef, f.job.active.ProviderRef)
	require.Equal(t, fakeOwner, f.job.active.IndexedBy, "a machine model is indexed on the chooser's machines")
	require.Equal(t, fakeOwner, f.job.active.ChosenBy)
	_, has := f.job.runs[memoryIndexKindSwitch]
	require.False(t, has, "no switch: there is nothing to re-index")

	const vendor = "embedding3Small"
	f = newMemorySearchFake(3).noSwitch()
	f.job.active = EmbedderBinding{}
	f.widths[vendor] = 1536
	require.NoError(t, memorySearchSwitch(ctx, f, fakeOwner, vendor))
	require.Equal(t, vendor, f.job.active.ProviderRef)
	require.Empty(t, f.job.active.IndexedBy, "a vendor model runs on nobody's machines")
	require.Equal(t, fakeOwner, f.job.active.ChosenBy)

	// An active model with nothing in its index.
	f = newMemorySearchFake(3).noSwitch()
	f.widths[fakeNewRef] = 6
	require.NoError(t, memorySearchSwitch(ctx, f, fakeOwner, fakeNewRef))
	require.Empty(t, f.binds, "a binding is never changed by the builtin")
	run := f.job.runs[memoryIndexKindSwitch]
	require.Equal(t, memoryIndexStateSwitching, run.State)
	require.Equal(t, fakeNewRef, run.TargetRef)
	require.Equal(t, 6, run.TargetDimensions)
	require.Equal(t, f.job.table(fakeNewRef, 6), run.TargetTable)
	require.Equal(t, fakeOwner, run.IndexedBy)
	require.Equal(t, fakeOwner, run.StartedBy)
	require.True(t, run.StartedAt.Equal(f.job.clock))
	require.Contains(t, f.job.tables, f.job.table(fakeNewRef, 6), "the target's table is ensured")
	require.Equal(t, fakeOldRef, f.job.active.ProviderRef, "the builtin does not flip")
	require.Zero(t, f.job.flips)

	require.NoError(t, runMemoryIndexTick(ctx, f.job, time.Hour))
	require.Equal(t, 1, f.job.flips)
	require.Equal(t, fakeNewRef, f.job.active.ProviderRef, "the next tick flips it")
	require.Equal(t, memoryIndexStateDone, f.job.runs[memoryIndexKindSwitch].State)
	require.Len(t, f.job.tables[f.job.table(fakeNewRef, 6)], 3, "the walk after the flip indexes every item")
}

// One switch at a time: while one is in progress -- needsDecision, waiting and
// finishing included -- another is refused before anything is probed or
// written. A switch that is over leaves the way open.
func TestASecondSwitchIsRefusedWhileOneRuns(t *testing.T) {
	ctx := context.Background()
	const third = "fleet:third-embedder"
	for _, state := range []string{memoryIndexStateIndexing, memoryIndexStateCatchingUp, memoryIndexStateNeedsDecision,
		memoryIndexStateSwitching, memoryIndexStateFinishing, memoryIndexStateWaiting} {
		t.Run(state, func(t *testing.T) {
			f := newMemorySearchFake(3)
			f.job.fill(fakeOldRef, 4)
			f.widths[third] = 4
			f.setSwitchState(state)
			err := memorySearchSwitch(ctx, f, fakeOwner, third)
			require.Error(t, err)
			require.ErrorContains(t, err, "new-embedder", "the refusal names the switch in progress")
			require.ErrorContains(t, err, "cancel it or let it finish")
			require.Zero(t, f.runAttempts)
			require.Empty(t, f.probes)
			require.Equal(t, state, f.job.runs[memoryIndexKindSwitch].State)
		})
	}
	for _, state := range []string{memoryIndexStateDone, memoryIndexStateCancelled} {
		t.Run(state, func(t *testing.T) {
			f := newMemorySearchFake(3)
			f.job.fill(fakeOldRef, 4)
			f.widths[third] = 4
			f.setSwitchState(state)
			require.NoError(t, memorySearchSwitch(ctx, f, fakeOwner, third))
			run := f.job.runs[memoryIndexKindSwitch]
			require.Equal(t, memoryIndexStateIndexing, run.State)
			require.Equal(t, third, run.TargetRef)
			require.Equal(t, []string{third + " as " + fakeOwner}, f.probes, "the new model is checked on the chooser's machines")
			require.Equal(t, []string{"lock", "writeRun indexing locked=true", "unlock"}, f.events, "decided again and written under the binding lock")
		})
	}

	// A machine model is indexed on an active owner's machines, so a caller who
	// is not one (a grant of the part, say) cannot start one.
	f := newMemorySearchFake(3).noSwitch()
	f.job.fill(fakeOldRef, 4)
	f.widths[third] = 4
	err := memorySearchSwitch(ctx, f, memorySearchOtherOwner, third)
	require.ErrorContains(t, err, "not an active cluster owner")
	require.Zero(t, f.runAttempts)

	// A write refused by the fence is answered, never retried.
	f = newMemorySearchFake(3).noSwitch()
	f.job.fill(fakeOldRef, 4)
	f.widths[third] = 4
	f.writeErr = errMemoryIndexRunChanged
	err = memorySearchSwitch(ctx, f, fakeOwner, third)
	require.ErrorIs(t, err, errMemorySearchChanged)
	require.ErrorContains(t, err, "changed meanwhile, try again")
	require.Equal(t, 1, f.runAttempts)
}

// Choosing the model already in use moves nothing, rebuilds nothing and probes
// nothing; under the binding lock it records who chose it where nobody is
// recorded, and makes the chooser the indexer of a machine model nobody indexes
// or whose indexer is no longer an owner (the job's wait says "an owner can
// choose again"). An indexer who is still an owner is kept.
func TestChoosingTheModelInUseRecordsWhoChoseIt(t *testing.T) {
	ctx := context.Background()

	f := newMemorySearchFake(3).noSwitch()
	f.job.active.IndexedBy, f.job.active.ChosenBy = "", ""
	f.job.owners[memorySearchOtherOwner] = true
	require.NoError(t, memorySearchSwitch(ctx, f, memorySearchOtherOwner, fakeOldRef))
	require.Equal(t, memorySearchOtherOwner, f.job.active.IndexedBy)
	require.Equal(t, memorySearchOtherOwner, f.job.active.ChosenBy)
	require.Equal(t, []string{"lock", "writeChoice locked=true", "unlock"}, f.events)
	require.Empty(t, f.probes)
	require.Zero(t, f.runAttempts)

	// An indexer who is still an owner stays; only the empty chooser is filled.
	f = newMemorySearchFake(3).noSwitch()
	f.job.active.ChosenBy = ""
	f.job.owners[memorySearchOtherOwner] = true
	require.NoError(t, memorySearchSwitch(ctx, f, memorySearchOtherOwner, fakeOldRef))
	require.Equal(t, fakeOwner, f.job.active.IndexedBy)
	require.Equal(t, memorySearchOtherOwner, f.job.active.ChosenBy)

	// An indexer who is no longer an owner is replaced by the owner choosing again.
	f = newMemorySearchFake(3).noSwitch()
	f.job.active.ChosenBy = fakeOwner
	delete(f.job.owners, fakeOwner)
	f.job.owners[memorySearchOtherOwner] = true
	require.NoError(t, memorySearchSwitch(ctx, f, memorySearchOtherOwner, fakeOldRef))
	require.Equal(t, memorySearchOtherOwner, f.job.active.IndexedBy)
	require.Equal(t, memorySearchOtherOwner, f.job.active.ChosenBy)

	// Everything already recorded: nothing to write.
	f = newMemorySearchFake(3).noSwitch()
	f.job.active.ChosenBy = fakeOwner
	require.NoError(t, memorySearchSwitch(ctx, f, fakeOwner, fakeOldRef))
	require.Equal(t, fakeOwner, f.job.active.IndexedBy)
	require.Empty(t, f.choices)
}

// Cancel stops the switch and keeps the partial index under the 30-day
// retention, so choosing the same model again resumes rather than restarts. It
// runs under the binding lock, and the index is dated BEFORE the run says
// cancelled. A switch that already happened is not cancelled: switching back
// is a new switch.
func TestCancelKeepsThePartialIndexForThirtyDays(t *testing.T) {
	ctx := context.Background()
	f := newMemorySearchFake(5)
	f.job.fill(fakeOldRef, 4)
	partial := f.job.table(fakeNewRef, 4)
	f.job.tables[partial] = map[string]bool{"content|" + memoryIndexItemId(1): true, "content|" + memoryIndexItemId(2): true}
	f.setSwitchState(memoryIndexStateWaiting)
	run := f.job.runs[memoryIndexKindSwitch]
	run.WaitingReason = "none of the indexer's machines offering new-embedder is online"
	f.job.runs[memoryIndexKindSwitch] = run

	require.NoError(t, memorySearchCancel(ctx, f))
	cancelled := f.job.runs[memoryIndexKindSwitch]
	require.Equal(t, memoryIndexStateCancelled, cancelled.State)
	require.Empty(t, cancelled.WaitingReason)
	kept, ok := f.retiredOf(fakeNewRef, 4)
	require.True(t, ok, "the target is recorded retired")
	require.True(t, kept.RetireAt.Equal(f.job.clock.Add(30*24*time.Hour)), "kept for 30 days: %v", kept.RetireAt)
	require.True(t, kept.DroppedAt.IsZero())
	require.Equal(t, fakeOwner, kept.IndexedBy)
	require.Len(t, f.job.tables[partial], 2, "the partial index is kept")
	require.Equal(t, []string{"lock", "recordRetired " + fakeNewRef + " locked=true", "writeRun cancelled locked=true", "unlock"}, f.events)

	// The job leaves a cancelled switch alone; the backfill keeps the active one.
	require.NoError(t, runMemoryIndexTick(ctx, f.job, time.Hour))
	require.Equal(t, memoryIndexStateCancelled, f.job.runs[memoryIndexKindSwitch].State)
	require.Equal(t, fakeOldRef, f.job.active.ProviderRef)

	// Nothing to cancel any more.
	require.ErrorContains(t, memorySearchCancel(ctx, f), "no switch")

	// A switch whose binding already names its target has happened.
	f = newMemorySearchFake(3)
	f.setSwitchState(memoryIndexStateFinishing)
	f.job.active = EmbedderBinding{ProviderRef: fakeNewRef, Dimensions: 4, IndexedBy: fakeOwner, ActivatedAt: "2026-11-24T00:00:00Z"}
	err := memorySearchCancel(ctx, f)
	require.ErrorContains(t, err, "already happened")
	require.Zero(t, f.runAttempts)
	require.Empty(t, f.retiredRows)

	// A run changed meanwhile is answered, never retried.
	f = newMemorySearchFake(3)
	f.writeErr = errMemoryIndexRunChanged
	err = memorySearchCancel(ctx, f)
	require.ErrorIs(t, err, errMemorySearchChanged)
	require.Equal(t, 1, f.runAttempts)

	// The binding lock held elsewhere is a plain "try again".
	f = newMemorySearchFake(3)
	f.locked = true
	require.ErrorContains(t, memorySearchCancel(ctx, f), "try again")
	require.Zero(t, f.runAttempts)
}

// Switch anyway is the owner's answer to a switch held for failed or unsourced
// items: it writes `switching` and moves nothing itself; the job's next tick
// flips, and the held items stay out of search until they are written again or a
// later walk picks them up.
func TestSwitchAnywayFlipsWithTheHeldItems(t *testing.T) {
	ctx := context.Background()
	f := newMemorySearchFake(4)
	f.job.fill(fakeOldRef, 4)
	held := memoryIndexItemId(2)
	f.job.embedErr = func(_ MemoryIndexRun, item MemoryItem) error {
		if item.Id == held {
			return errors.New("embedding model returned a zero vector")
		}
		return nil
	}
	require.NoError(t, runMemoryIndexTick(ctx, f.job, time.Hour))
	require.Equal(t, memoryIndexStateNeedsDecision, f.job.runs[memoryIndexKindSwitch].State)
	require.Equal(t, 1, f.job.runs[memoryIndexKindSwitch].Counts.Failed)

	require.NoError(t, memorySearchSwitchAnyway(ctx, f))
	require.Equal(t, memoryIndexStateSwitching, f.job.runs[memoryIndexKindSwitch].State)
	require.Equal(t, fakeOldRef, f.job.active.ProviderRef, "the builtin does not flip")
	require.Zero(t, f.job.flips)

	require.NoError(t, runMemoryIndexTick(ctx, f.job, time.Hour))
	require.Equal(t, 1, f.job.flips)
	require.Equal(t, fakeNewRef, f.job.active.ProviderRef)
	require.Equal(t, memoryIndexStateDone, f.job.runs[memoryIndexKindSwitch].State)
	require.False(t, f.job.tables[f.job.table(fakeNewRef, 4)]["content|"+held], "the held item is not in the new index")

	for _, state := range []string{memoryIndexStateIndexing, memoryIndexStateCatchingUp, memoryIndexStateSwitching,
		memoryIndexStateFinishing, memoryIndexStateWaiting, memoryIndexStateDone, memoryIndexStateCancelled} {
		f := newMemorySearchFake(2)
		f.setSwitchState(state)
		require.ErrorContains(t, memorySearchSwitchAnyway(ctx, f), "waiting for your decision", state)
		require.Zero(t, f.runAttempts, state)
	}
	require.ErrorContains(t, memorySearchSwitchAnyway(ctx, newMemorySearchFake(2).noSwitch()), "waiting for your decision")

	f = newMemorySearchFake(2)
	f.setSwitchState(memoryIndexStateNeedsDecision)
	f.writeErr = errMemoryIndexRunChanged
	require.ErrorIs(t, memorySearchSwitchAnyway(ctx, f), errMemorySearchChanged)
	require.Equal(t, 1, f.runAttempts)
}

// The status says what is in use and who indexes it, how many items the index
// holds, the backlog the backfill could not index and why it waits, the switch
// in progress with an ESTIMATED total, and the indexes kept for switching back.
func TestStatusReportsTheBacklogAndWhyItWaits(t *testing.T) {
	ctx := context.Background()
	f := newMemorySearchFake(10).noSwitch()
	f.job.fill(fakeOldRef, 4)
	f.job.active.ChosenBy = fakeOwner
	f.names[fakeOwner] = "Ana Owner"
	const reason = "none of the indexer's machines offering old-embedder is online"
	f.job.runs[memoryIndexKindBackfill] = MemoryIndexRun{Kind: memoryIndexKindBackfill, State: memoryIndexStateWaiting,
		TargetRef: fakeOldRef, TargetDimensions: 4, IndexedBy: fakeOwner, WaitingReason: reason,
		Counts: MemoryIndexCounts{Total: 6, Done: 1, Skipped: 3, Failed: 2}, Cursor: map[string]string{"alpha": memoryIndexItemId(6)}}

	row, err := memorySearchStatus(ctx, f)
	require.NoError(t, err)
	require.Equal(t, fakeOldRef, row["providerRef"])
	require.Equal(t, "machine", row["kind"])
	require.Equal(t, "old-embedder", row["label"])
	require.Equal(t, fakeOwner, row["indexedBy"])
	require.Equal(t, "Ana Owner", row["indexedByName"])
	require.Equal(t, fakeOwner, row["chosenBy"])
	require.Equal(t, false, row["needsConfirmation"])
	require.Equal(t, 10, row["indexed"])
	require.Equal(t, 2, row["backlog"], "the items the backfill found missing and could not index")
	require.Equal(t, reason, row["waitingReason"])
	require.Nil(t, row["switch"])
	require.Equal(t, []any{}, row["retained"])

	// A switch in progress, and what is kept.
	started := f.job.clock.Add(-5 * time.Minute)
	f.job.runs[memoryIndexKindSwitch] = MemoryIndexRun{Kind: memoryIndexKindSwitch, State: memoryIndexStateIndexing,
		TargetRef: fakeNewRef, TargetDimensions: 4, IndexedBy: fakeOwner, StartedBy: fakeOwner, StartedAt: started,
		Counts: MemoryIndexCounts{Total: 4, Done: 3, Skipped: 1}, Cursor: map[string]string{"alpha": memoryIndexItemId(4)}}
	const third = "fleet:third-embedder"
	keptUntil := f.job.clock.Add(29 * 24 * time.Hour)
	f.retiredRows[RetiredEmbedderBindingID(third, 8)] = retiredMemoryIndex{ProviderRef: third, Dimensions: 8, RetireAt: keptUntil}
	// The active model's own dated row (a flip that stopped part way), the
	// running switch's target and a dropped index are not "kept for switching back".
	f.retiredRows[RetiredEmbedderBindingID(fakeOldRef, 4)] = retiredMemoryIndex{ProviderRef: fakeOldRef, Dimensions: 4, RetireAt: keptUntil}
	f.retiredRows[RetiredEmbedderBindingID(fakeNewRef, 4)] = retiredMemoryIndex{ProviderRef: fakeNewRef, Dimensions: 4, RetireAt: keptUntil}
	f.retiredRows[RetiredEmbedderBindingID("fleet:gone-embedder", 4)] = retiredMemoryIndex{ProviderRef: "fleet:gone-embedder", Dimensions: 4,
		RetireAt: f.job.clock.Add(-time.Hour), DroppedAt: f.job.clock.Add(-time.Minute)}

	row, err = memorySearchStatus(ctx, f)
	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"state": memoryIndexStateIndexing, "targetRef": fakeNewRef, "targetLabel": "new-embedder",
		"counts":         map[string]any{"total": 4, "done": 3, "skipped": 1, "unsourced": 0, "failed": 0},
		"startedAt":      started.Format(time.RFC3339Nano),
		"startedBy":      fakeOwner,
		"waitingReason":  "",
		"estimatedTotal": 12,
	}, row["switch"])
	require.Equal(t, []any{map[string]any{"providerRef": third, "retireAt": keptUntil.Format(time.RFC3339Nano)}}, row["retained"])

	// A backfill row about another model (the model before a flip) is no
	// backlog of this one.
	bf := f.job.runs[memoryIndexKindBackfill]
	bf.TargetRef = "fleet:previous-embedder"
	f.job.runs[memoryIndexKindBackfill] = bf
	row, err = memorySearchStatus(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 0, row["backlog"])
	require.Equal(t, "", row["waitingReason"])

	// Nothing bound: one row, empty.
	f = newMemorySearchFake(1).noSwitch()
	f.job.active = EmbedderBinding{}
	row, err = memorySearchStatus(ctx, f)
	require.NoError(t, err)
	require.Equal(t, "", row["providerRef"])
	require.Equal(t, "", row["kind"])
	require.Equal(t, 0, row["indexed"])
}

// A vendor model nobody chose -- bound by the fallback this design removed --
// asks an owner once to confirm it; confirming records who did, under the
// binding lock, and asks no more. A machine model never asks.
func TestAVendorBindingPickedBeforeAsksForConfirmationOnce(t *testing.T) {
	ctx := context.Background()
	const vendor = "embedding3Small"
	f := newMemorySearchFake(2).noSwitch()
	f.job.active = EmbedderBinding{ProviderRef: vendor, Dimensions: 1536, ActivatedAt: "2026-10-01T00:00:00Z"}
	f.vendors = []vendorEmbedderRecord{{Name: vendor, Model: "text-embedding-3-small"}}

	row, err := memorySearchStatus(ctx, f)
	require.NoError(t, err)
	require.Equal(t, "vendor", row["kind"])
	require.Equal(t, "text-embedding-3-small", row["label"])
	require.Equal(t, true, row["needsConfirmation"])
	require.Equal(t, "", row["indexedBy"])

	require.NoError(t, memorySearchConfirm(ctx, f, fakeOwner))
	require.Equal(t, fakeOwner, f.job.active.ChosenBy)
	require.Equal(t, []string{"lock", "writeChoice locked=true", "unlock"}, f.events)
	row, err = memorySearchStatus(ctx, f)
	require.NoError(t, err)
	require.Equal(t, false, row["needsConfirmation"])
	require.Equal(t, fakeOwner, row["chosenBy"])

	// Once: a second confirmation writes nothing.
	require.NoError(t, memorySearchConfirm(ctx, f, memorySearchOtherOwner))
	require.Len(t, f.choices, 1)
	require.Equal(t, fakeOwner, f.job.active.ChosenBy)

	// A machine model nobody chose (an adopted indexer) is not a vendor's to confirm.
	f = newMemorySearchFake(2).noSwitch()
	f.job.active.ChosenBy = ""
	row, err = memorySearchStatus(ctx, f)
	require.NoError(t, err)
	require.Equal(t, false, row["needsConfirmation"])
	require.ErrorContains(t, memorySearchConfirm(ctx, f, fakeOwner), "machine model")
	require.Empty(t, f.choices)

	f.job.active = EmbedderBinding{}
	require.ErrorContains(t, memorySearchConfirm(ctx, f, fakeOwner), "no memory-search model")
}

// The choices are the caller's machine embedding models -- their own machines
// and the ones lent to them, embeddings only -- and the vendor embedding models
// the cluster's own account is set up for, each marked as paid by it. Read
// through the engine and a real provider registry, so the vendor half is the
// vendor-account predicate itself (federationEmbedderRecords).
func TestChoicesMarkVendorModelsAsPaidByTheCluster(t *testing.T) {
	const caller = "v1:identity:user:caller"
	e := &MemQLEngine{providers: NewProviderRegistryForTest()}
	machine := func(id string, online, embeds bool) FleetMachine {
		return FleetMachine{RegistrationId: id, Online: online, Capabilities: FleetModelCapabilities{Embeddings: embeds}}
	}
	e.providers.SetFleetCatalog(ownerCatalogs{caller: {
		{ModelId: "nomic-embed-text", Embeddings: true, Machines: []FleetMachine{machine("m3", false, true)}},
		{ModelId: "qwen3-embedding:0.6b", Embeddings: true, Machines: []FleetMachine{machine("m1", true, true), machine("m2", false, true), machine("m4", true, false)}},
		{ModelId: "llama3.1:8b", Machines: []FleetMachine{machine("m1", true, false)}},
	}})
	federated := map[string]string{authKeyIdentityProviderID: "idp", authKeyOpenAIServiceAccountID: "sa", authKeyOpenAITokenFile: "/var/run/secrets/token"}
	probe := vendorProbe{&activationProbe{}}
	e.providers.setEntry(&ProviderConfigEntry{Config: ProviderConfig{Name: "embedding3Small", Type: "OpenAIEmbedding", Model: "text-embedding-3-small", Auth: federated}, Client: probe, Available: true})
	// Not set up: the cluster has no account to pay for it.
	e.providers.setEntry(&ProviderConfigEntry{Config: ProviderConfig{Name: "embedding3Large", Type: "OpenAIEmbedding", Model: "text-embedding-3-large"}, Client: probe, Available: true})
	// Set up, but not an embedder.
	e.providers.setEntry(&ProviderConfigEntry{Config: ProviderConfig{Name: "chat54Mini", Type: "OpenAI", Model: "gpt-5.4-mini", Auth: federated}, Client: chatOnlyProvider{}, Available: true})

	nodes, err := e.memorySearchChoicesBuiltin(asCaller("owner"), nil, 0)
	require.NoError(t, err)
	type choice struct {
		ProviderRef   string `json:"providerRef"`
		Kind          string `json:"kind"`
		Label         string `json:"label"`
		Machines      int    `json:"machines"`
		PaidByCluster bool   `json:"paidByCluster"`
		Available     bool   `json:"available"`
		Hint          string `json:"hint"`
	}
	var got []choice
	for _, n := range nodes {
		require.Equal(t, MemorySearchChoiceConcept, n.Concept)
		var c choice
		require.NoError(t, json.Unmarshal(n.Payload, &c))
		require.Equal(t, c.ProviderRef, n.ID)
		require.NotEmpty(t, c.Hint, c.ProviderRef)
		c.Hint = ""
		got = append(got, c)
	}
	require.Equal(t, []choice{
		{ProviderRef: "fleet:nomic-embed-text", Kind: "machine", Label: "nomic-embed-text", Machines: 1},
		{ProviderRef: "fleet:qwen3-embedding:0.6b", Kind: "machine", Label: "qwen3-embedding:0.6b", Machines: 2, Available: true},
		{ProviderRef: "embedding3Small", Kind: "vendor", Label: "text-embedding-3-small", PaidByCluster: true, Available: true},
	}, got)

	_, err = e.memorySearchChoicesBuiltin(auth.ContextWithSystemActor(context.Background(), "someSweep"), nil, 0)
	require.Error(t, err, "the cluster acting has no machines to choose from")
}

// chatOnlyProvider is a vendor client that embeds nothing.
type chatOnlyProvider struct{}

func (chatOnlyProvider) Call(context.Context, string) (any, error) { return nil, nil }
