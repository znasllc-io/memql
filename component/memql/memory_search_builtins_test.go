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

	locked     bool
	lockErr    error
	catalogErr error
	writeErr   error
	ensureErr  error
	bindErr    error
	ownerErr   error
	noExecute  bool // the caller does not hold `execute` on the part
	counted    int

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
	if f.lockErr != nil {
		return nil, f.lockErr
	}
	if f.locked {
		return nil, errAskConversationHeld
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

func (f *memorySearchFake) writeRun(_ context.Context, run MemoryIndexRun, _ time.Time) (time.Time, error) {
	f.runAttempts++
	if f.writeErr != nil {
		return time.Time{}, f.writeErr
	}
	f.job.runs[run.Kind] = cloneMemoryIndexRun(run)
	f.events = append(f.events, fmt.Sprintf("writeRun %s locked=%t", run.State, f.locked))
	return f.job.clock, nil
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
	if f.bindErr != nil {
		return f.bindErr
	}
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
	if f.ensureErr != nil {
		return "", f.ensureErr
	}
	return f.job.ensureTable(ctx, ref, dims)
}

func (f *memorySearchFake) vectors(_ context.Context, table string) (int, error) {
	f.counted++
	return len(f.job.tables[table]), nil
}

func (f *memorySearchFake) hasVectors(_ context.Context, table string) (bool, error) {
	return len(f.job.tables[table]) > 0, nil
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

func (f *memorySearchFake) isActiveOwner(_ context.Context, userId string) (bool, error) {
	if f.ownerErr != nil {
		return false, f.ownerErr
	}
	return f.job.owners[userId], nil
}

func (f *memorySearchFake) mayExecute(context.Context) bool { return !f.noExecute }

func (f *memorySearchFake) catalog(_ context.Context, userId string) ([]FleetModel, error) {
	if f.catalogErr != nil {
		return nil, f.catalogErr
	}
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

// Every act is an ACTIVE CLUSTER OWNER's (design record, decision 2 and
// sections 2 and 8: a vendor model is bound only by an owner choosing it, and
// Memory search is owners only), on top of the part the builtin path checks. A
// person granted the part who is not an owner -- or an owner who stopped being
// one -- chooses no model of either kind, takes nothing over, confirms nothing,
// cancels nothing and switches nothing anyway, and is told why in plain words.
func TestEveryActIsAnActiveOwners(t *testing.T) {
	ctx := context.Background()
	const vendor, granted = "embedding3Small", "v1:identity:user:granted"
	refused := func(t *testing.T, err error) {
		t.Helper()
		require.ErrorIs(t, err, errMemorySearchNotOwner)
		require.EqualError(t, err, "only an active cluster owner can change memory search, and you are not one")
	}
	nothingDone := func(t *testing.T, f *memorySearchFake) {
		t.Helper()
		require.Zero(t, f.runAttempts, "no run written")
		require.Empty(t, f.probes, "nothing probed, so nothing spent")
		require.Empty(t, f.binds, "nothing bound")
		require.Empty(t, f.choices, "no indexer or chooser recorded")
		require.Empty(t, f.retiredRows, "no index dated")
	}

	// A vendor model, which the cluster's account would pay for on a choice no
	// owner made: as a switch, and as a cluster's first model.
	f := newMemorySearchFake(3).noSwitch()
	f.job.fill(fakeOldRef, 4)
	f.widths[vendor] = 1536
	refused(t, memorySearchSwitch(ctx, f, granted, vendor))
	nothingDone(t, f)
	f = newMemorySearchFake(3).noSwitch()
	f.job.active = EmbedderBinding{}
	f.widths[vendor] = 1536
	refused(t, memorySearchSwitch(ctx, f, granted, vendor))
	nothingDone(t, f)

	// A machine model, and choosing the model in use again where an owner's
	// choice would take its indexing over.
	f = newMemorySearchFake(3).noSwitch()
	f.job.fill(fakeOldRef, 4)
	f.job.active.IndexedBy = ""
	refused(t, memorySearchSwitch(ctx, f, granted, fakeNewRef))
	refused(t, memorySearchSwitch(ctx, f, granted, fakeOldRef))
	nothingDone(t, f)
	require.Empty(t, f.job.active.IndexedBy)

	// Keep: a vendor model nobody chose stays unconfirmed.
	f = newMemorySearchFake(2).noSwitch()
	f.job.active = EmbedderBinding{ProviderRef: vendor, Dimensions: 1536, ActivatedAt: "2026-10-01T00:00:00Z"}
	refused(t, memorySearchConfirm(ctx, f, granted))
	nothingDone(t, f)
	require.Empty(t, f.job.active.ChosenBy)

	// Cancel and Switch anyway, by a grantee and by an owner who was demoted.
	f = newMemorySearchFake(3)
	refused(t, memorySearchCancel(ctx, f, granted))
	f.setSwitchState(memoryIndexStateNeedsDecision)
	refused(t, memorySearchSwitchAnyway(ctx, f, granted))
	delete(f.job.owners, fakeOwner)
	refused(t, memorySearchCancel(ctx, f, fakeOwner))
	refused(t, memorySearchSwitchAnyway(ctx, f, fakeOwner))
	nothingDone(t, f)
	require.Equal(t, memoryIndexStateNeedsDecision, f.job.runs[memoryIndexKindSwitch].State)
}

// The status says from the caller's side whether they may act at all
// (`mayAct`): an active cluster owner who holds the part's `execute`, and
// nobody else, so the screen offers nobody else an act. Neither take-over is
// offered where acting is not: choosing again would be refused.
func TestStatusSaysWhetherTheCallerMayAct(t *testing.T) {
	ctx := context.Background()
	const granted = "v1:identity:user:granted"
	for _, tc := range []struct {
		name, caller string
		arrange      func(f *memorySearchFake)
		want         bool
	}{
		{"an active owner holding the part", memorySearchOtherOwner, nil, true},
		{"an owner whose part lacks execute", memorySearchOtherOwner, func(f *memorySearchFake) { f.noExecute = true }, false},
		{"somebody granted the part who is not an owner", granted, nil, false},
		{"an owner who stopped being one", memorySearchOtherOwner, func(f *memorySearchFake) { delete(f.job.owners, memorySearchOtherOwner) }, false},
		{"an owner check that cannot be made", memorySearchOtherOwner, func(f *memorySearchFake) {
			f.ownerErr = errors.New("the cluster's owners could not be read: database unavailable")
		}, false},
		{"the cluster asking", "", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Nobody indexes the model in use or the switch's target, so an owner
			// choosing either again would take it over.
			f := newMemorySearchFake(2)
			f.job.active.IndexedBy = ""
			run := f.job.runs[memoryIndexKindSwitch]
			run.IndexedBy = ""
			f.job.runs[memoryIndexKindSwitch] = run
			f.job.owners[memorySearchOtherOwner] = true
			if tc.arrange != nil {
				tc.arrange(f)
			}
			row, err := memorySearchStatus(ctx, f, tc.caller)
			require.NoError(t, err)
			require.Equal(t, tc.want, row["mayAct"])
			require.Equal(t, tc.want, row["takeOver"], "the take-over of the model in use")
			require.Equal(t, tc.want, row["switch"].(map[string]any)["takeOver"], "the take-over of the switch")
			require.Zero(t, f.runAttempts, "a status writes nothing")
		})
	}
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

// One switch at a time. Before the flip, another model is refused before
// anything is probed or written: a switch held for the owner's decision names
// Switch anyway and Cancel, any other says to cancel it or let it finish.
// Choosing the running switch's own target again is not a second switch, and
// with its indexer able to index changes nothing. A switch that is over leaves
// the way open.
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
			if state == memoryIndexStateNeedsDecision {
				require.ErrorContains(t, err, "Switch anyway")
				require.ErrorContains(t, err, "Cancel")
				require.NotContains(t, err.Error(), "let it finish", "a held switch does not finish by itself")
			} else {
				require.ErrorContains(t, err, "cancel it or let it finish")
			}
			require.Zero(t, f.runAttempts)
			require.Empty(t, f.probes)
			require.Equal(t, state, f.job.runs[memoryIndexKindSwitch].State)

			require.NoError(t, memorySearchSwitch(ctx, f, fakeOwner, fakeNewRef), "the running switch's own target")
			require.Zero(t, f.runAttempts)
			require.Empty(t, f.probes)
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

	// A caller who is not an active owner (a grant of the part, say) starts
	// nothing (TestEveryActIsAnActiveOwners has every act).
	f := newMemorySearchFake(3).noSwitch()
	f.job.fill(fakeOldRef, 4)
	f.widths[third] = 4
	err := memorySearchSwitch(ctx, f, memorySearchOtherOwner, third)
	require.ErrorIs(t, err, errMemorySearchNotOwner)
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

// offeredFor is a catalog with one model, embedding or not, on one machine
// that is offline.
func offeredFor(model string, embeds bool) []FleetModel {
	return []FleetModel{{ModelId: model, Embeddings: embeds, Machines: []FleetMachine{{RegistrationId: "m-" + model,
		Capabilities: FleetModelCapabilities{Embeddings: embeds}}}}}
}

// onlineFor is a catalog with one embedding model on one machine that is
// online: a replica holds its stream (the fleet catalog's online rule).
func onlineFor(model string) []FleetModel {
	return []FleetModel{{ModelId: model, Embeddings: true, Machines: []FleetMachine{{RegistrationId: "m-" + model,
		Capabilities: FleetModelCapabilities{Embeddings: true}, Online: true}}}}
}

// tickUntil runs the index job one step a tick until its switch is in state.
func tickUntil(t *testing.T, job *memoryIndexFake, state string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if job.runs[memoryIndexKindSwitch].State == state {
			return
		}
		require.NoError(t, runMemoryIndexTick(context.Background(), job, 0))
	}
	t.Fatalf("the switch never reached %s: %+v", state, job.runs[memoryIndexKindSwitch])
}

// Choosing the model in use again moves nothing, rebuilds nothing and probes
// nothing. Under the binding lock it is the TAKE-OVER (design section 1, ruling
// R9): the caller becomes the indexer and the chooser of a machine model whose
// indexer is missing, is no longer an active owner, or has no machine, own or
// lent, offering the model for embeddings while one of the caller's does.
// Otherwise only an empty chooser is filled. A vendor model has no indexer: only
// an empty chooser is filled.
func TestChoosingTheModelInUseTakesOverAnIndexerWhoCannotIndex(t *testing.T) {
	ctx := context.Background()
	const other, model = memorySearchOtherOwner, "old-embedder"
	for _, tc := range []struct {
		name                string
		arrange             func(f *memorySearchFake)
		indexedBy, chosenBy string
	}{
		{"nobody indexes it", func(f *memorySearchFake) { f.job.active.IndexedBy = "" }, other, other},
		{"the indexer is no longer an owner", func(f *memorySearchFake) { delete(f.job.owners, fakeOwner) }, other, other},
		{"no machine of the indexer's offers it, and one of the caller's does", func(f *memorySearchFake) {
			f.catalogs[other] = offeredFor(model, true)
		}, other, other},
		{"the indexer's machine offers it for something other than embeddings", func(f *memorySearchFake) {
			f.catalogs[fakeOwner] = offeredFor(model, false)
			f.catalogs[other] = offeredFor(model, true)
		}, other, other},
		{"no machine of either offers it, so taking over would not help", nil, fakeOwner, fakeOwner},
		{"the indexer's machines offer it", func(f *memorySearchFake) {
			f.catalogs[fakeOwner] = offeredFor(model, true)
			f.catalogs[other] = offeredFor(model, true)
		}, fakeOwner, fakeOwner},
		// Design section 1's pause and section 8's example: while the
		// indexer's laptop sleeps, search waits for everybody, so an owner
		// whose machine is awake takes over.
		{"the indexer's machines offering it are all offline, and one of the caller's is online", func(f *memorySearchFake) {
			f.catalogs[fakeOwner] = offeredFor(model, true)
			f.catalogs[other] = onlineFor(model)
		}, other, other},
		{"the indexer's machine offering it is online", func(f *memorySearchFake) {
			f.catalogs[fakeOwner] = onlineFor(model)
			f.catalogs[other] = onlineFor(model)
		}, fakeOwner, fakeOwner},
		{"an online machine of the indexer's that offers it for something other than embeddings is not one offering it", func(f *memorySearchFake) {
			f.catalogs[fakeOwner] = []FleetModel{{ModelId: model, Embeddings: true, Machines: []FleetMachine{
				{RegistrationId: "m-embeds", Capabilities: FleetModelCapabilities{Embeddings: true}},
				{RegistrationId: "m-chats", Online: true}}}}
			f.catalogs[other] = onlineFor(model)
		}, other, other},
		{"the indexer's machines offer it and nobody chose it", func(f *memorySearchFake) {
			f.job.active.ChosenBy = ""
			f.catalogs[fakeOwner] = offeredFor(model, true)
		}, fakeOwner, other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMemorySearchFake(3).noSwitch()
			f.job.active.ChosenBy = fakeOwner
			f.job.owners[other] = true
			if tc.arrange != nil {
				tc.arrange(f)
			}
			require.NoError(t, memorySearchSwitch(ctx, f, other, fakeOldRef))
			require.Equal(t, tc.indexedBy, f.job.active.IndexedBy)
			require.Equal(t, tc.chosenBy, f.job.active.ChosenBy)
			require.Equal(t, fakeOldRef, f.job.active.ProviderRef)
			require.Empty(t, f.probes)
			require.Zero(t, f.runAttempts)
			for _, e := range f.events {
				if strings.HasPrefix(e, "writeChoice") {
					require.Equal(t, "writeChoice locked=true", e)
				}
			}
			if tc.indexedBy == fakeOwner && tc.chosenBy == fakeOwner {
				require.Empty(t, f.choices, "nothing to record")
			}
		})
	}

	const vendor = "embedding3Small"
	f := newMemorySearchFake(2).noSwitch()
	f.job.active = EmbedderBinding{ProviderRef: vendor, Dimensions: 1536, ActivatedAt: "2026-10-01T00:00:00Z"}
	f.job.owners[other] = true
	require.NoError(t, memorySearchSwitch(ctx, f, other, vendor))
	require.Equal(t, other, f.job.active.ChosenBy)
	require.Empty(t, f.job.active.IndexedBy, "a vendor model runs on nobody's machines")
	require.NoError(t, memorySearchSwitch(ctx, f, fakeOwner, vendor))
	require.Equal(t, other, f.job.active.ChosenBy, "a recorded chooser of a vendor model is kept")
	require.Len(t, f.choices, 1)
}

// A switch whose binding already names its target (ruling R11) has a way out.
// Its walk after the flip embeds as the run's indexer, and when that indexer
// cannot index, an owner choosing the model in use again takes over both the
// binding and the run, and the next tick finishes. Cancel stays refused: the
// switch happened.
func TestAFlippedSwitchWhoseIndexerCannotIndexIsTakenOverByChoosingItAgain(t *testing.T) {
	ctx := context.Background()
	const other = memorySearchOtherOwner
	written := MemoryItem{Id: memoryIndexItemId(9), Text: "written after the flip"}

	t.Run("the indexer is no longer an owner", func(t *testing.T) {
		f := newMemorySearchFake(4)
		f.job.fill(fakeOldRef, 4)
		f.job.owners[other] = true
		tickUntil(t, f.job, memoryIndexStateFinishing)
		require.Equal(t, fakeNewRef, f.job.active.ProviderRef, "flipped")
		f.job.rows["alpha"] = append(f.job.rows["alpha"], written)
		delete(f.job.owners, fakeOwner)
		require.NoError(t, runMemoryIndexTick(ctx, f.job, time.Hour))
		require.Equal(t, memoryIndexStateWaiting, f.job.runs[memoryIndexKindSwitch].State)
		require.Equal(t, memoryIndexOwnerReason, f.job.runs[memoryIndexKindSwitch].WaitingReason)
		require.ErrorContains(t, memorySearchCancel(ctx, f, other), "already happened")

		require.NoError(t, memorySearchSwitch(ctx, f, other, fakeNewRef))
		require.Equal(t, other, f.job.active.IndexedBy)
		require.Equal(t, other, f.job.active.ChosenBy)
		require.Equal(t, other, f.job.runs[memoryIndexKindSwitch].IndexedBy, "the walk after the flip resumes on the new indexer's machines")
		require.Empty(t, f.probes)

		require.NoError(t, runMemoryIndexTick(ctx, f.job, time.Hour))
		require.Equal(t, memoryIndexStateDone, f.job.runs[memoryIndexKindSwitch].State)
		require.True(t, f.job.tables[f.job.table(fakeNewRef, 4)]["content|"+written.Id], "the walk after the flip finished")
	})

	t.Run("no machine of the indexer's offers the model", func(t *testing.T) {
		f := newMemorySearchFake(4)
		f.job.fill(fakeOldRef, 4)
		f.job.owners[other] = true
		tickUntil(t, f.job, memoryIndexStateFinishing)
		f.job.rows["alpha"] = append(f.job.rows["alpha"], written)
		f.job.embedErr = func(run MemoryIndexRun, _ MemoryItem) error {
			if run.IndexedBy == fakeOwner {
				return errors.New("store: embed: resolve provider \"fleet:new-embedder\": no machine offering new-embedder is online")
			}
			return nil
		}
		require.NoError(t, runMemoryIndexTick(ctx, f.job, time.Hour))
		require.Equal(t, memoryIndexStateWaiting, f.job.runs[memoryIndexKindSwitch].State)
		require.Equal(t, "none of the indexer's machines offering new-embedder is online", f.job.runs[memoryIndexKindSwitch].WaitingReason)

		f.catalogs[other] = offeredFor("new-embedder", true)
		require.NoError(t, memorySearchSwitch(ctx, f, other, fakeNewRef))
		require.Equal(t, other, f.job.active.IndexedBy)
		require.Equal(t, other, f.job.runs[memoryIndexKindSwitch].IndexedBy)

		require.NoError(t, runMemoryIndexTick(ctx, f.job, time.Hour))
		require.Equal(t, memoryIndexStateDone, f.job.runs[memoryIndexKindSwitch].State)
		require.True(t, f.job.tables[f.job.table(fakeNewRef, 4)]["content|"+written.Id])
	})
}

// Before the flip, choosing the running switch's target again takes over the
// RUN's indexer under the same triggers (ruling R11 b): the switch goes on, and
// flips with the new indexer.
func TestChoosingTheRunningSwitchsTargetAgainTakesItOver(t *testing.T) {
	ctx := context.Background()
	const other = memorySearchOtherOwner
	f := newMemorySearchFake(4)
	f.job.fill(fakeOldRef, 4)
	f.job.owners[other] = true
	delete(f.job.owners, fakeOwner)
	require.NoError(t, runMemoryIndexTick(ctx, f.job, time.Hour))
	require.Equal(t, memoryIndexStateWaiting, f.job.runs[memoryIndexKindSwitch].State)

	require.NoError(t, memorySearchSwitch(ctx, f, other, fakeNewRef))
	require.Equal(t, other, f.job.runs[memoryIndexKindSwitch].IndexedBy)
	require.Equal(t, fakeOldRef, f.job.active.ProviderRef, "nothing flips")
	require.Empty(t, f.probes)

	require.NoError(t, runMemoryIndexTick(ctx, f.job, time.Hour))
	require.Equal(t, memoryIndexStateDone, f.job.runs[memoryIndexKindSwitch].State)
	require.Equal(t, fakeNewRef, f.job.active.ProviderRef)
	require.Equal(t, other, f.job.active.IndexedBy, "the binding is indexed as the run is")
}

// Before the flip, choosing the MODEL IN USE again is the take-over of the
// BINDING's indexer (ruling R13, R9's rule): search and every writer embed as
// that indexer for the whole rebuild, so one who cannot index would leave search
// refused until the flip. The running switch is left as it is -- its state, its
// indexer, its counts -- nothing is probed, and the choice is written under the
// binding lock. An indexer who can index keeps it, and the switch is not refused
// either.
func TestChoosingTheModelInUseDuringASwitchTakesOverTheBindingsIndexer(t *testing.T) {
	ctx := context.Background()
	const other = memorySearchOtherOwner
	for _, state := range []string{memoryIndexStateIndexing, memoryIndexStateCatchingUp, memoryIndexStateNeedsDecision,
		memoryIndexStateSwitching, memoryIndexStateFinishing, memoryIndexStateWaiting} {
		t.Run(state, func(t *testing.T) {
			f := newMemorySearchFake(3)
			f.job.fill(fakeOldRef, 4)
			f.setSwitchState(state)
			f.job.owners[other] = true
			delete(f.job.owners, fakeOwner)
			before := cloneMemoryIndexRun(f.job.runs[memoryIndexKindSwitch])

			require.NoError(t, memorySearchSwitch(ctx, f, other, fakeOldRef))
			require.Equal(t, other, f.job.active.IndexedBy, "search and the writers embed on the new indexer's machines")
			require.Equal(t, other, f.job.active.ChosenBy)
			require.Equal(t, fakeOldRef, f.job.active.ProviderRef, "nothing moves")
			require.Zero(t, f.runAttempts, "the running switch is not written")
			require.Equal(t, before, cloneMemoryIndexRun(f.job.runs[memoryIndexKindSwitch]), "and goes on as it was")
			require.Empty(t, f.probes)
			require.Equal(t, []string{"lock", "writeChoice locked=true", "unlock"}, f.events)
		})
	}

	f := newMemorySearchFake(3)
	f.job.fill(fakeOldRef, 4)
	f.job.owners[other] = true
	f.job.active.ChosenBy = fakeOwner
	f.catalogs[fakeOwner] = offeredFor("old-embedder", true)
	f.catalogs[other] = offeredFor("old-embedder", true)
	require.NoError(t, memorySearchSwitch(ctx, f, other, fakeOldRef), "the model in use is not another model")
	require.Equal(t, fakeOwner, f.job.active.IndexedBy, "an indexer whose machines offer it keeps it")
	require.Empty(t, f.choices)
	require.Zero(t, f.runAttempts)
	require.Equal(t, memoryIndexStateIndexing, f.job.runs[memoryIndexKindSwitch].State)
}

// Choosing another model while the walk after a flip runs -- switching back
// included -- closes the flipped switch as done, leaving its remaining walk to
// the backfill, and starts the new switch (ruling R11 a).
func TestSwitchingBackDuringTheWalkAfterTheFlipStartsANewSwitch(t *testing.T) {
	ctx := context.Background()
	f := newMemorySearchFake(4)
	f.job.fill(fakeOldRef, 4)
	tickUntil(t, f.job, memoryIndexStateFinishing)
	require.Equal(t, fakeNewRef, f.job.active.ProviderRef)

	require.NoError(t, memorySearchSwitch(ctx, f, fakeOwner, fakeOldRef))
	require.Equal(t, []string{"lock", "writeRun done locked=true", "writeRun indexing locked=true", "unlock"}, f.events)
	run := f.job.runs[memoryIndexKindSwitch]
	require.Equal(t, memoryIndexStateIndexing, run.State)
	require.Equal(t, fakeOldRef, run.TargetRef)
	require.Equal(t, fakeOwner, run.StartedBy)
	require.Equal(t, fakeNewRef, f.job.active.ProviderRef, "the builtin moves nothing")

	require.NoError(t, runMemoryIndexTick(ctx, f.job, time.Hour))
	require.Equal(t, memoryIndexStateDone, f.job.runs[memoryIndexKindSwitch].State)
	require.Equal(t, fakeOldRef, f.job.active.ProviderRef, "switched back")
}

// A new switch starts only once the steps that can fail are done: choosing
// another model during a flipped switch's walk leaves that switch as it was when
// the new model's table cannot be made, rather than closing it for a switch that
// never started. And the active index is only asked whether it holds anything
// -- an EXISTS, never a count, under the binding lock.
func TestASwitchStartsOnlyOnceItsTableIsReady(t *testing.T) {
	ctx := context.Background()
	f := newMemorySearchFake(4)
	f.job.fill(fakeOldRef, 4)
	tickUntil(t, f.job, memoryIndexStateFinishing)
	before := cloneMemoryIndexRun(f.job.runs[memoryIndexKindSwitch])
	f.ensureErr = errors.New("the table could not be created")

	require.ErrorContains(t, memorySearchSwitch(ctx, f, fakeOwner, fakeOldRef), "the table could not be created")
	require.Zero(t, f.runAttempts, "nothing is written")
	require.Equal(t, before, cloneMemoryIndexRun(f.job.runs[memoryIndexKindSwitch]), "the flipped switch goes on as it was")

	f.ensureErr = nil
	require.NoError(t, memorySearchSwitch(ctx, f, fakeOwner, fakeOldRef))
	require.Equal(t, []string{"lock", "unlock", "lock", "writeRun done locked=true", "writeRun indexing locked=true", "unlock"}, f.events)
	require.Zero(t, f.counted, "the active index is not counted")
}

// Memory search says "being changed elsewhere, try again" only for a binding
// lock another request holds, and never in the lock's own words, which are
// Ask's ("MemQL is already working in this conversation"). Any other failure to
// take the lock -- no gate pool wired, the pool busy, the database -- is
// reported as the failure it is, the daily cleanup's included.
func TestOnlyALockHeldElsewhereReadsAsBeingChangedElsewhere(t *testing.T) {
	ctx := context.Background()
	const third = "fleet:third-embedder"
	failed := fmt.Errorf("Ask: %w: it was built without one", ErrWriteGatePoolMissing)
	for name, act := range map[string]func(f *memorySearchFake) error{
		"switch":  func(f *memorySearchFake) error { return memorySearchSwitch(ctx, f, fakeOwner, third) },
		"cancel":  func(f *memorySearchFake) error { return memorySearchCancel(ctx, f, fakeOwner) },
		"confirm": func(f *memorySearchFake) error { return memorySearchConfirm(ctx, f, fakeOwner) },
	} {
		fake := func() *memorySearchFake {
			f := newMemorySearchFake(3)
			if name == "switch" {
				f.noSwitch().job.fill(fakeOldRef, 4)
				f.widths[third] = 4
			}
			return f
		}
		t.Run(name+", the lock held elsewhere", func(t *testing.T) {
			f := fake()
			f.locked = true
			err := act(f)
			require.ErrorIs(t, err, errMemorySearchBusy)
			require.NotContains(t, err.Error(), "this conversation", "the lock's own words are Ask's")
			require.Zero(t, f.runAttempts)
		})
		t.Run(name+", the lock failed", func(t *testing.T) {
			f := fake()
			f.lockErr = failed
			err := act(f)
			require.ErrorIs(t, err, ErrWriteGatePoolMissing)
			require.NotContains(t, err.Error(), "elsewhere", "a failure is not somebody else's change")
			require.Zero(t, f.runAttempts)
		})
	}

	// A cluster's first model is bound under bindEmbedder's own lock.
	f := newMemorySearchFake(3).noSwitch()
	f.job.active = EmbedderBinding{}
	f.bindErr = errAskConversationHeld
	require.ErrorIs(t, memorySearchSwitch(ctx, f, fakeOwner, fakeNewRef), errMemorySearchBusy)
	f.bindErr = failed
	err := memorySearchSwitch(ctx, f, fakeOwner, fakeNewRef)
	require.ErrorIs(t, err, ErrWriteGatePoolMissing)
	require.NotContains(t, err.Error(), "elsewhere")

	f = newMemorySearchFake(3)
	f.locked = true
	err = memoryIndexRetire(ctx, f)
	require.ErrorContains(t, err, "being changed")
	require.NotContains(t, err.Error(), "this conversation")
	f = newMemorySearchFake(3)
	f.lockErr = failed
	err = memoryIndexRetire(ctx, f)
	require.ErrorIs(t, err, ErrWriteGatePoolMissing)
	require.NotContains(t, err.Error(), "being changed")
}

// A binding that lands between the switch's read and bindEmbedder's own, on
// the no-binding path, is "changed meanwhile, try again": the owner is already
// on the page bindEmbedder's refusal would send them to. Any other failure to
// bind is reported as it is.
func TestABindingThatLandsMeanwhileIsAChangeNotADirection(t *testing.T) {
	ctx := context.Background()
	f := newMemorySearchFake(3).noSwitch()
	f.job.active = EmbedderBinding{}
	f.bindErr = errEmbedderBindingActive
	err := memorySearchSwitch(ctx, f, fakeOwner, fakeNewRef)
	require.ErrorIs(t, err, errMemorySearchChanged)
	require.NotContains(t, err.Error(), "Cluster > Settings")

	f = newMemorySearchFake(3).noSwitch()
	f.job.active = EmbedderBinding{}
	f.bindErr = errors.New("no machine offering new-embedder is online")
	require.EqualError(t, memorySearchSwitch(ctx, f, fakeOwner, fakeNewRef), "no machine offering new-embedder is online")
}

// An owner check that could not be made is reported, never read as "you are
// not an active cluster owner"; the status withholds the take-over for that
// read, as it does when a catalog cannot be read.
func TestAnOwnerCheckThatFailedIsReportedAsAFailure(t *testing.T) {
	ctx := context.Background()
	const third = "fleet:third-embedder"
	unread := errors.New("the cluster's owners could not be read: database unavailable")
	f := newMemorySearchFake(3).noSwitch()
	f.job.fill(fakeOldRef, 4)
	f.widths[third] = 4
	f.ownerErr = unread
	err := memorySearchSwitch(ctx, f, fakeOwner, third)
	require.ErrorIs(t, err, unread)
	require.NotContains(t, err.Error(), "you are not an active cluster owner")
	require.Empty(t, f.probes)
	require.Zero(t, f.runAttempts)

	// Choosing the model in use asks whether its indexer is still an owner.
	f = newMemorySearchFake(3).noSwitch()
	f.ownerErr = unread
	require.ErrorIs(t, memorySearchSwitch(ctx, f, fakeOwner, fakeOldRef), unread)
	require.Empty(t, f.choices)

	f = newMemorySearchFake(3).noSwitch()
	delete(f.job.owners, fakeOwner)
	f.job.owners[memorySearchOtherOwner] = true
	f.ownerErr = unread
	row, err := memorySearchStatus(ctx, f, memorySearchOtherOwner)
	require.NoError(t, err, "the status still answers")
	require.Equal(t, false, row["takeOver"])
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

	require.NoError(t, memorySearchCancel(ctx, f, fakeOwner))
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
	require.ErrorContains(t, memorySearchCancel(ctx, f, fakeOwner), "no switch")

	// A switch whose binding already names its target has happened.
	f = newMemorySearchFake(3)
	f.setSwitchState(memoryIndexStateFinishing)
	f.job.active = EmbedderBinding{ProviderRef: fakeNewRef, Dimensions: 4, IndexedBy: fakeOwner, ActivatedAt: "2026-11-24T00:00:00Z"}
	err := memorySearchCancel(ctx, f, fakeOwner)
	require.ErrorContains(t, err, "already happened")
	require.Zero(t, f.runAttempts)
	require.Empty(t, f.retiredRows)

	// A run changed meanwhile is answered, never retried.
	f = newMemorySearchFake(3)
	f.writeErr = errMemoryIndexRunChanged
	err = memorySearchCancel(ctx, f, fakeOwner)
	require.ErrorIs(t, err, errMemorySearchChanged)
	require.Equal(t, 1, f.runAttempts)

	// The binding lock held elsewhere is a plain "try again".
	f = newMemorySearchFake(3)
	f.locked = true
	require.ErrorContains(t, memorySearchCancel(ctx, f, fakeOwner), "try again")
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

	require.NoError(t, memorySearchSwitchAnyway(ctx, f, fakeOwner))
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
		require.ErrorContains(t, memorySearchSwitchAnyway(ctx, f, fakeOwner), "waiting for your decision", state)
		require.Zero(t, f.runAttempts, state)
	}
	require.ErrorContains(t, memorySearchSwitchAnyway(ctx, newMemorySearchFake(2).noSwitch(), fakeOwner), "waiting for your decision")

	f = newMemorySearchFake(2)
	f.setSwitchState(memoryIndexStateNeedsDecision)
	f.writeErr = errMemoryIndexRunChanged
	require.ErrorIs(t, memorySearchSwitchAnyway(ctx, f, fakeOwner), errMemorySearchChanged)
	require.Equal(t, 1, f.runAttempts)
}

// The status says what is in use and who indexes it, how many items the index
// holds, the backlog -- the items the backfill's last count found missing, with
// when it was counted (ruling R10) -- and why it waits, the switch in progress
// with an ESTIMATED total, the indexes kept for switching back, and which model
// the binding replaced: the one Switch back names, which a cancelled switch's
// kept target is not.
func TestStatusReportsTheBacklogAndWhyItWaits(t *testing.T) {
	ctx := context.Background()
	f := newMemorySearchFake(10).noSwitch()
	f.job.fill(fakeOldRef, 4)
	f.job.active.ChosenBy = fakeOwner
	f.names[fakeOwner] = "Ana Owner"
	const reason = "none of the indexer's machines offering old-embedder is online"
	counted := f.job.clock.Add(-3 * time.Minute)
	f.job.runs[memoryIndexKindBackfill] = MemoryIndexRun{Kind: memoryIndexKindBackfill, State: memoryIndexStateWaiting,
		TargetRef: fakeOldRef, TargetDimensions: 4, IndexedBy: fakeOwner, WaitingReason: reason,
		Counts: MemoryIndexCounts{Total: 6, Done: 1, Skipped: 3, Failed: 2}, Cursor: map[string]string{"alpha": memoryIndexItemId(6)},
		Missing: 37, MissingAt: counted}

	row, err := memorySearchStatus(ctx, f, fakeOwner)
	require.NoError(t, err)
	require.Equal(t, fakeOldRef, row["providerRef"])
	require.Equal(t, "machine", row["kind"])
	require.Equal(t, "old-embedder", row["label"])
	require.Equal(t, fakeOwner, row["indexedBy"])
	require.Equal(t, "Ana Owner", row["indexedByName"])
	require.Equal(t, fakeOwner, row["chosenBy"])
	require.Equal(t, false, row["needsConfirmation"])
	require.Equal(t, 10, row["indexed"])
	require.Equal(t, 37, row["backlog"], "the items the backfill's last count found missing")
	require.Equal(t, counted.Format(time.RFC3339Nano), row["backlogCountedAt"])
	require.Equal(t, reason, row["waitingReason"])
	require.Nil(t, row["switch"])
	require.Equal(t, []any{}, row["retained"])
	require.Equal(t, "", row["previousRef"], "a cluster's first model replaced none")
	require.Equal(t, false, row["takeOver"], "the caller indexes it already")

	f.job.active.PreviousRef = "fleet:third-embedder"
	row, err = memorySearchStatus(ctx, f, fakeOwner)
	require.NoError(t, err)
	require.Equal(t, "fleet:third-embedder", row["previousRef"])

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

	row, err = memorySearchStatus(ctx, f, fakeOwner)
	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"state": memoryIndexStateIndexing, "targetRef": fakeNewRef, "targetLabel": "new-embedder",
		"counts":         map[string]any{"total": 4, "done": 3, "skipped": 1, "unsourced": 0, "failed": 0},
		"startedAt":      started.Format(time.RFC3339Nano),
		"startedBy":      fakeOwner,
		"waitingReason":  "",
		"estimatedTotal": 47,
		"indexedBy":      fakeOwner,
		"indexedByName":  "Ana Owner",
		"takeOver":       false,
	}, row["switch"])
	require.Equal(t, []any{map[string]any{"providerRef": third, "retireAt": keptUntil.Format(time.RFC3339Nano)}}, row["retained"])

	// A backfill row about another model (the model before a flip) is no
	// backlog of this one.
	bf := f.job.runs[memoryIndexKindBackfill]
	bf.TargetRef = "fleet:previous-embedder"
	f.job.runs[memoryIndexKindBackfill] = bf
	row, err = memorySearchStatus(ctx, f, fakeOwner)
	require.NoError(t, err)
	require.Equal(t, 0, row["backlog"])
	require.Equal(t, "", row["backlogCountedAt"])
	require.Equal(t, "", row["waitingReason"])

	// Nothing bound: one row, empty.
	f = newMemorySearchFake(1).noSwitch()
	f.job.active = EmbedderBinding{}
	row, err = memorySearchStatus(ctx, f, fakeOwner)
	require.NoError(t, err)
	require.Equal(t, "", row["providerRef"])
	require.Equal(t, "", row["kind"])
	require.Equal(t, 0, row["indexed"])
	require.Equal(t, false, row["takeOver"])
}

// The status says when the binding's indexer cannot index the model in use and
// choosing it again would make the caller its indexer (ruling R13): `takeOver`.
// It is the take-over rule itself (memorySearchTakesOver) asked for the caller,
// who must be an active owner to index a machine model, so the screen offers the
// act only where it changes something -- never to the indexer themself, never
// when the indexer's machines offer the model, never when neither person's do.
// A switch in progress does not change the answer: it is about the binding,
// which search and every writer embed as until the flip. Taking over answers
// false afterwards.
func TestStatusSaysWhenChoosingTheModelInUseTakesOver(t *testing.T) {
	ctx := context.Background()
	const other, model = memorySearchOtherOwner, "old-embedder"
	const granted = "v1:identity:user:granted"
	for _, tc := range []struct {
		name    string
		caller  string
		arrange func(f *memorySearchFake)
		want    bool
	}{
		{"the indexer is no longer an owner", other, func(f *memorySearchFake) { delete(f.job.owners, fakeOwner) }, true},
		{"nobody indexes it", other, func(f *memorySearchFake) { f.job.active.IndexedBy = "" }, true},
		{"no machine of the indexer's offers it, and one of the caller's does", other, func(f *memorySearchFake) {
			f.catalogs[other] = offeredFor(model, true)
		}, true},
		{"the indexer's machines offering it are offline, and one of the caller's is online", other, func(f *memorySearchFake) {
			f.catalogs[fakeOwner] = offeredFor(model, true)
			f.catalogs[other] = onlineFor(model)
		}, true},
		{"the indexer's machines offering it are offline, and so are the caller's", other, func(f *memorySearchFake) {
			f.catalogs[fakeOwner] = offeredFor(model, true)
			f.catalogs[other] = offeredFor(model, true)
		}, false},
		// The same, with the catalogs unreadable: the act is withheld for that
		// read rather than offered on a guess, and the status still answers.
		{"a catalog that cannot be read", other, func(f *memorySearchFake) {
			f.catalogs[other] = offeredFor(model, true)
			f.catalogErr = errors.New("the fleet catalog could not be read")
		}, false},
		{"the indexer's machines offer it", other, func(f *memorySearchFake) {
			f.catalogs[fakeOwner] = offeredFor(model, true)
			f.catalogs[other] = offeredFor(model, true)
		}, false},
		{"neither person's machines offer it", other, nil, false},
		{"the caller is the indexer", fakeOwner, func(f *memorySearchFake) { f.catalogs[fakeOwner] = nil }, false},
		{"the caller is not an active owner", granted, func(f *memorySearchFake) { delete(f.job.owners, fakeOwner) }, false},
		{"the cluster asking", "", func(f *memorySearchFake) { delete(f.job.owners, fakeOwner) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMemorySearchFake(2).noSwitch()
			f.job.owners[other] = true
			if tc.arrange != nil {
				tc.arrange(f)
			}
			row, err := memorySearchStatus(ctx, f, tc.caller)
			require.NoError(t, err)
			require.Equal(t, tc.want, row["takeOver"])
			require.Zero(t, f.runAttempts, "a status writes nothing")
			require.Empty(t, f.choices)
		})
	}

	// A switch in progress: still the binding's question, and choosing the model
	// in use again answers it.
	f := newMemorySearchFake(2)
	f.job.owners[other] = true
	delete(f.job.owners, fakeOwner)
	row, err := memorySearchStatus(ctx, f, other)
	require.NoError(t, err)
	require.Equal(t, true, row["takeOver"])
	require.NoError(t, memorySearchSwitch(ctx, f, other, fakeOldRef))
	row, err = memorySearchStatus(ctx, f, other)
	require.NoError(t, err)
	require.Equal(t, false, row["takeOver"], "the caller indexes it now")
	require.Equal(t, other, row["indexedBy"])

	// A vendor model has no indexer to take over from.
	const vendor = "embedding3Small"
	f = newMemorySearchFake(2).noSwitch()
	f.job.active = EmbedderBinding{ProviderRef: vendor, Dimensions: 1536, ActivatedAt: "2026-10-01T00:00:00Z"}
	f.job.owners[other] = true
	row, err = memorySearchStatus(ctx, f, other)
	require.NoError(t, err)
	require.Equal(t, false, row["takeOver"])
}

// A running switch says too when choosing its TARGET again would take over its
// own indexer (ruling R14): the switch summary's takeOver -- the take-over rule
// asked for the caller, the run's indexer and its target -- beside whose
// machines the rebuild embeds on (indexedBy, indexedByName), so the screen can
// offer the act on the Switching row and say the waiting reason from the
// viewer's side. The binding's own takeOver stays the binding's question. A
// switch that has flipped is the binding's question only (its target is the
// model in use), and one that is over offers nothing.
func TestTheSwitchSaysWhenChoosingItsTargetAgainTakesOver(t *testing.T) {
	ctx := context.Background()
	const other, third = memorySearchOtherOwner, "v1:identity:user:owner-3"
	summary := func(t *testing.T, f *memorySearchFake, caller string) map[string]any {
		t.Helper()
		row, err := memorySearchStatus(ctx, f, caller)
		require.NoError(t, err)
		sw, ok := row["switch"].(map[string]any)
		require.True(t, ok, "a switch summary")
		return sw
	}
	// The binding is indexed by a third owner who can index it; the run, by an
	// owner who is no longer one.
	running := func() *memorySearchFake {
		f := newMemorySearchFake(3)
		f.job.fill(fakeOldRef, 4)
		f.job.active.IndexedBy = third
		f.job.owners[third], f.job.owners[other] = true, true
		delete(f.job.owners, fakeOwner)
		f.catalogs[third] = offeredFor("old-embedder", true)
		f.names[fakeOwner] = "Ana Owner"
		return f
	}

	f := running()
	row, err := memorySearchStatus(ctx, f, other)
	require.NoError(t, err)
	require.Equal(t, false, row["takeOver"], "the binding's indexer can index the model in use")
	sw := summary(t, f, other)
	require.Equal(t, true, sw["takeOver"], "the run's indexer is no longer an owner")
	require.Equal(t, fakeOwner, sw["indexedBy"])
	require.Equal(t, "Ana Owner", sw["indexedByName"])
	require.Zero(t, f.runAttempts, "a status writes nothing")

	// Choosing the target again is the act it offers (ruling R11 b).
	require.NoError(t, memorySearchSwitch(ctx, f, other, fakeNewRef))
	sw = summary(t, f, other)
	require.Equal(t, false, sw["takeOver"], "the caller indexes the rebuild now")
	require.Equal(t, other, sw["indexedBy"])

	for name, arrange := range map[string]func(f *memorySearchFake){
		"the run's indexer can index the target": func(f *memorySearchFake) {
			f.job.owners[fakeOwner] = true
			f.catalogs[fakeOwner] = offeredFor("new-embedder", true)
			f.catalogs[other] = offeredFor("new-embedder", true)
		},
		"the switch is over": func(f *memorySearchFake) { f.setSwitchState(memoryIndexStateCancelled) },
		"the target is a vendor model": func(f *memorySearchFake) {
			run := f.job.runs[memoryIndexKindSwitch]
			run.TargetRef, run.IndexedBy = "embedding3Small", ""
			f.job.runs[memoryIndexKindSwitch] = run
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := running()
			arrange(f)
			require.Equal(t, false, summary(t, f, other)["takeOver"])
		})
	}
	require.Equal(t, false, summary(t, running(), fakeOwner)["takeOver"], "the caller is the run's indexer")

	// Flipped: the target is the model in use, whose take-over the row says.
	f = newMemorySearchFake(3)
	f.job.fill(fakeOldRef, 4)
	f.job.owners[other] = true
	tickUntil(t, f.job, memoryIndexStateFinishing)
	delete(f.job.owners, fakeOwner)
	row, err = memorySearchStatus(ctx, f, other)
	require.NoError(t, err)
	require.Equal(t, true, row["takeOver"])
	require.Equal(t, false, row["switch"].(map[string]any)["takeOver"], "one act, offered once")
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

	row, err := memorySearchStatus(ctx, f, fakeOwner)
	require.NoError(t, err)
	require.Equal(t, "vendor", row["kind"])
	require.Equal(t, "text-embedding-3-small", row["label"])
	require.Equal(t, true, row["needsConfirmation"])
	require.Equal(t, "", row["indexedBy"])

	require.NoError(t, memorySearchConfirm(ctx, f, fakeOwner))
	require.Equal(t, fakeOwner, f.job.active.ChosenBy)
	require.Equal(t, []string{"lock", "writeChoice locked=true", "unlock"}, f.events)
	row, err = memorySearchStatus(ctx, f, fakeOwner)
	require.NoError(t, err)
	require.Equal(t, false, row["needsConfirmation"])
	require.Equal(t, fakeOwner, row["chosenBy"])

	// Once: a second confirmation, by another owner, writes nothing.
	f.job.owners[memorySearchOtherOwner] = true
	require.NoError(t, memorySearchConfirm(ctx, f, memorySearchOtherOwner))
	require.Len(t, f.choices, 1)
	require.Equal(t, fakeOwner, f.job.active.ChosenBy)

	// A machine model nobody chose (an adopted indexer) is not a vendor's to confirm.
	f = newMemorySearchFake(2).noSwitch()
	f.job.active.ChosenBy = ""
	row, err = memorySearchStatus(ctx, f, fakeOwner)
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
