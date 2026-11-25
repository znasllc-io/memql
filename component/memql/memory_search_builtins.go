package memql

// memory_search_builtins.go -- the cluster owner's memory-search builtins and the
// daily cleanup of the indexes a switch leaves behind (design record
// docs/superpowers/specs/2026-11-23-memory-search-model-switch-design.md,
// sections 3, 6, 7 and 8). MemQL OS's Cluster > Settings > Memory search reads
// and acts through them over the SDK.
//
// # The builtins, and their gates
//
//   - memorySearchStatus and memorySearchChoices: `read` on
//     app:cluster/memorysearch.
//   - memorySearchSwitch, memorySearchCancel, memorySearchSwitchAnyway and
//     memorySearchConfirm: `execute` on it.
//   - memoryIndexRetire: the cleanup's own step. No part; the handler refuses a
//     call without internal origin, as memoryIndexStep does.
//
// Both parts are seeded on the owner role and no other (dsl/rbac/seeds.memql),
// and the builtin path checks them before a handler runs. An act is recorded
// as a PERSON (memorySearchCaller): who started a switch, who chose a model,
// whose machines index it. The cluster acting is nobody to record.
//
// # Who moves what
//
// A builtin moves the binding's MODEL in one case only: a cluster with NO model
// binds its first through bindEmbedder, because nothing is indexed under
// another model. Every other change of model is the index job's
// (memory_index_job.go): a builtin writes the `switch` run row and the job's
// tick rebuilds and flips. No builtin calls flipMemoryIndex, Switch anyway
// included. TRIGGER for that rule: the flip is ordered steps finished by the
// next tick from `switching` (ruling R6); a builtin that died after moving the
// binding would leave the run in a state the tick never resumes. So
// memorySearchSwitchAnyway writes `switching` and the tick flips within a
// minute, and a switch over an index that holds no vector is written
// `switching` directly for the same reason.
//
// # The take-over, and the way out of a flipped switch
//
// Choosing the model in use again is how an owner TAKES OVER indexing it
// (design record section 1, ruling R9; memorySearchTakesOver): when its indexer
// is missing, is no longer an active owner, or has no machine -- own or lent,
// embeddings only -- offering the model while one of the caller's does, the
// caller becomes its indexer and chooser. Otherwise only an empty chooser is
// filled. Choosing a running switch's own target is the same take-over of the
// RUN's indexer. Choosing the model in use while a switch runs, before its flip,
// is still the take-over of the BINDING's (ruling R13): search queries and every
// writer embed as the binding's indexer for the whole rebuild, and refusing it
// as "another model" left search refused until the flip whenever that indexer
// could not index. The running switch is left as it is: its own indexer is the
// run's, taken over by choosing its target, and the flip names that one. The
// status says when choosing the model in use would take over (`takeOver`), so
// the screen offers the act only where it changes something. And a switch that
// has FLIPPED (memorySearchFlipped) is no
// longer one switch at a time's to guard (ruling R11): its walk after the flip
// embeds as the run's indexer, and with that indexer demoted or without the
// model every other act would be refused while search refuses queries and the
// previous model's kept index nears its drop. So choosing the model in use
// takes over the binding and the run, and choosing another model closes the
// flipped switch as done -- its remaining walk is the backfill's -- and starts
// the new one. Cancel stays refused after the flip: the switch happened.
//
// # Every run-row write is fenced, and a refusal is answered
//
// The run rows are written through writeMemoryIndexRun, fenced to the version
// the builtin read: the job writes after every batch, and an unfenced write
// would revert its cursor or be reverted by it. A refused fence reaches the
// owner as "changed meanwhile, try again" (errMemorySearchChanged) and is never
// retried here: the state it was decided on is gone, and deciding again is the
// owner's call. Every write of the run, binding and retired rows is stamped
// after the version it read (VersionTimeAfter, ruling R12), so a replica whose
// clock lags the last writer's still writes the newest version. The acts that
// read the binding and decide on it -- switch, cancel, confirm, and the cleanup
// -- hold the binding lock (lockAskConversationKind "active",
// "embedder-binding") around those reads and their writes, which is what the
// flip holds, so neither can interleave with a flip on another replica
// (memql#5431). The lock is a try-lock: held by another request, the act says
// the model is being changed elsewhere and to try again, rather than waiting;
// a lock that could not be taken at all is reported as the failure it is
// (memorySearchLock). So is an owner check that could not be made, which is
// never read as "not an owner" (indexerOwnerAnswer).
//
// # The 30-day retention
//
// A flip records the model it replaced as retired with retireAt = now + 30 days
// (memoryIndexRetention); Cancel records the abandoned target the same way, so
// choosing it again reuses what was built. The daily memoryIndexRetentionSweep
// (dsl/platform/automations.memql) drops each retired table past its date that
// is neither the active model's nor a running switch's target, and records
// droppedAt. The two protected tables are protected by NAME, whatever the
// retired rows say: a flip that stopped part way dates the active model before
// the binding moves, and a cancelled target can be chosen again.
//
// # What survives a retry or another replica
//
// All of it is rows and tables in Postgres: the binding, the run rows, the
// retired rows, the vector tables. A builtin keeps nothing between calls, so a
// retry on another replica decides from what is stored.
//
// # The files
//
// This file holds the acts, the cleanup and the builtins' handlers.
// memory_search_status.go holds the two reads, and memory_search_world.go
// the builtins' world over Postgres.
//
// Regression tests: memory_search_builtins_test.go (the decisions, over the
// job's map world) and memory_search_builtins_db_test.go (Postgres, two engines).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// The virtual concepts the read builtins answer in. Never persisted.
const (
	MemorySearchStatusConcept = "v1:platform:memorySearchStatus"
	MemorySearchChoiceConcept = "v1:platform:memorySearchChoice"
)

// memorySearchStateFailed is the design record's name for a switch that ended in
// error. The job does not write it today (the concept's state enum has no such
// value); it is named so that a switch in that state reads as over, not running.
const memorySearchStateFailed = "failed"

// errMemorySearchChanged is a write the fence refused: somebody else -- the job,
// another owner, another replica -- wrote the row since it was read.
var errMemorySearchChanged = errors.New("memory search changed meanwhile, try again")

// errMemorySearchBusy is the binding lock held by another request: the one
// failure to take it that is somebody else's change, and the one an owner is
// told to try again after (memorySearchLock).
var errMemorySearchBusy = errors.New("memory search's model is being changed elsewhere right now; try again in a moment")

// retiredMemoryIndex is one retired model's row (RetiredEmbedderBindingID): its
// index is kept until RetireAt, and DroppedAt says when it was dropped.
type retiredMemoryIndex struct {
	ProviderRef         string
	Dimensions          int
	IndexedBy           string
	RetireAt, DroppedAt time.Time
}

// memorySearchWorld is everything the builtins touch: the binding and its lock,
// the run rows, the vector tables, the retired rows, the owner check, the
// catalogs. engineMemorySearchWorld implements it over Postgres; the unit tests
// implement it over the index job's map world.
type memorySearchWorld interface {
	// binding reads the active binding fresh; ErrNoEmbedderBound when none.
	binding(ctx context.Context) (EmbedderBinding, error)
	// lockBinding takes the binding lock without waiting; release it.
	lockBinding(ctx context.Context) (func(), error)
	readRun(ctx context.Context, kind string) (MemoryIndexRun, time.Time, bool, error)
	// writeRun writes the whole run fenced to version, and answers the version
	// it stored, or errMemoryIndexRunChanged when somebody wrote it since.
	writeRun(ctx context.Context, run MemoryIndexRun, version time.Time) (time.Time, error)
	// writeChoice records the active binding's indexer and/or chooser; the
	// caller holds the binding lock.
	writeChoice(ctx context.Context, choice embedderChoice) error
	// bind makes provider the cluster's FIRST model (bindEmbedder).
	bind(ctx context.Context, provider string, choice embedderChoice) error
	// probe embeds one sentence with provider, as indexer, and answers its width.
	probe(ctx context.Context, provider, indexer string) (int, error)
	ensureTable(ctx context.Context, ref string, dims int) (string, error)
	// vectors counts a vector table's entries; 0 for a table that does not exist.
	vectors(ctx context.Context, table string) (int, error)
	// hasVectors reports whether a vector table holds any entry; false for a
	// table that does not exist.
	hasVectors(ctx context.Context, table string) (bool, error)
	retired(ctx context.Context) ([]retiredMemoryIndex, error)
	recordRetired(ctx context.Context, r retiredMemoryIndex) error
	dropTable(ctx context.Context, table string) error
	isActiveOwner(ctx context.Context, userId string) (bool, error)
	// catalog is a person's machine models: their own machines and the ones lent to them.
	catalog(ctx context.Context, userId string) ([]FleetModel, error)
	// vendorEmbedders are the vendor embedding records the cluster's own account pays for.
	vendorEmbedders() []vendorEmbedderRecord
	displayName(ctx context.Context, userId string) string
	now() time.Time
	logger() *slog.Logger
}

// memorySearchSwitchOver reports whether a switch in this state is over, so a
// new one may start: done, cancelled or failed. Every other state is a switch in
// progress -- finishing, waiting and needsDecision included: one switch at a
// time (design record, section 6).
func memorySearchSwitchOver(state string) bool {
	switch state {
	case memoryIndexStateDone, memoryIndexStateCancelled, memorySearchStateFailed:
		return true
	}
	return false
}

// memorySearchTablesInUse is the set of vector tables in use, which the daily
// cleanup never drops and the status never lists as kept for switching back:
// the active model's (active is the binding read, zero when none is bound) and
// a running switch's target. They are protected by NAME, whatever a retired row
// says: a flip that stopped part way dates the active model before the binding
// moves, and a cancelled target can be chosen again.
func memorySearchTablesInUse(active EmbedderBinding, sw MemoryIndexRun, hasSwitch bool) map[string]bool {
	inUse := map[string]bool{}
	if active.Valid() {
		if table, err := EmbeddingVectorTable(active.ProviderRef, active.Dimensions); err == nil {
			inUse[table] = true
		}
	}
	if hasSwitch && !memorySearchSwitchOver(sw.State) {
		if table, err := EmbeddingVectorTable(sw.TargetRef, sw.TargetDimensions); err == nil {
			inUse[table] = true
		}
	}
	return inUse
}

// memorySearchKind is "machine" for a fleet model and "vendor" for a vendor
// record.
func memorySearchKind(ref string) string {
	if _, fleet := IsFleetReference(ref); fleet {
		return "machine"
	}
	return "vendor"
}

// memorySearchLabel is a model's name as a person reads it: the model id of a
// fleet reference, the vendor's model of a vendor record, else the reference.
func memorySearchLabel(w memorySearchWorld, ref string) string {
	if model, fleet := IsFleetReference(ref); fleet {
		return model
	}
	for _, v := range w.vendorEmbedders() {
		if v.Name == ref && strings.TrimSpace(v.Model) != "" {
			return v.Model
		}
	}
	return ref
}

// memorySearchLock takes the binding lock. Held by another request, the act
// says so plainly, and not in the lock's own words ("MemQL is already working
// in this conversation", which are Ask's): errMemorySearchBusy. Any other
// failure to take it -- no gate pool wired, every gate connection busy, the
// database -- is reported as the failure it is, never as a change somebody
// else is making.
func memorySearchLock(ctx context.Context, w memorySearchWorld) (func(), error) {
	unlock, err := w.lockBinding(ctx)
	switch {
	case errors.Is(err, errAskConversationHeld):
		return nil, errMemorySearchBusy
	case err != nil:
		return nil, fmt.Errorf("memory search could not take its lock: %w", err)
	}
	return unlock, nil
}

// memorySearchWriteRun writes the run fenced to the version read, and answers a
// refused fence as errMemorySearchChanged. It answers the version it stored:
// the witness for a next write of the same row in the same act.
func memorySearchWriteRun(ctx context.Context, w memorySearchWorld, run MemoryIndexRun, version time.Time) (time.Time, error) {
	stored, err := w.writeRun(ctx, run, version)
	if errors.Is(err, errMemoryIndexRunChanged) {
		return time.Time{}, errMemorySearchChanged
	}
	return stored, err
}

// memorySearchSwitchRunning is the refusal of a second switch before the flip.
// A switch held in needsDecision moves only by the owner's answer, so it names
// the two answers instead of a finish it will never reach by itself.
func memorySearchSwitchRunning(w memorySearchWorld, sw MemoryIndexRun) error {
	if sw.State == memoryIndexStateNeedsDecision {
		return fmt.Errorf("the switch to %s is waiting for your decision: Switch anyway, or Cancel it, before choosing another model",
			memorySearchLabel(w, sw.TargetRef))
	}
	return fmt.Errorf("a switch to %s is still in progress (%s); cancel it or let it finish before choosing another model",
		memorySearchLabel(w, sw.TargetRef), sw.State)
}

// memorySearchFlipped reports whether sw is a switch whose binding already names
// its target and whose walk after the flip is under way or waiting: the switch
// happened, and only that walk remains (ruling R11).
func memorySearchFlipped(sw MemoryIndexRun, active EmbedderBinding) bool {
	return (sw.State == memoryIndexStateFinishing || sw.State == memoryIndexStateWaiting) &&
		active.ProviderRef == sw.TargetRef && active.Dimensions == sw.TargetDimensions
}

// memorySearchStartsASwitch reports whether choosing provider, with this switch
// run and binding, starts a new switch -- the one act that probes the model.
func memorySearchStartsASwitch(sw MemoryIndexRun, has bool, active EmbedderBinding, bound bool, provider string) bool {
	running := has && !memorySearchSwitchOver(sw.State)
	switch {
	case running && bound && memorySearchFlipped(sw, active):
		return provider != active.ProviderRef
	case running:
		return false
	}
	return bound && provider != active.ProviderRef
}

// memorySearchOffers reports whether a catalog holds model for embeddings, on
// any of its machines, online or not: an offline machine still holds the model
// (adoptEmbedderIndexer's reading).
func memorySearchOffers(models []FleetModel, model string) bool {
	for _, m := range models {
		if m.ModelId == model && m.Embeddings {
			return true
		}
	}
	return false
}

// memorySearchTakesOver is the TAKE-OVER rule (design record section 1, ruling
// R9): whether caller, choosing ref again, becomes its indexer in place of
// indexer. A machine model's indexer is replaced when it is empty, is no longer
// an active owner, or has no machine -- own or lent, embeddings only -- offering
// the model while one of the caller's does; replacing an indexer whose machines
// offer it, or with a caller whose machines do not either, would make nothing
// index. A vendor model has no indexer, and the caller never takes over from
// themself.
func memorySearchTakesOver(ctx context.Context, w memorySearchWorld, caller, indexer, ref string) (bool, error) {
	model, fleet := IsFleetReference(ref)
	if !fleet || BareShortId(indexer) == BareShortId(caller) {
		return false, nil
	}
	if strings.TrimSpace(indexer) == "" {
		return true, nil
	}
	owner, err := w.isActiveOwner(ctx, indexer)
	if err != nil {
		return false, fmt.Errorf("checking whether %s's indexer is still an owner: %w", model, err)
	}
	if !owner {
		return true, nil
	}
	theirs, err := w.catalog(ctx, indexer)
	if err != nil {
		return false, fmt.Errorf("reading whose machines offer %s: %w", model, err)
	}
	if memorySearchOffers(theirs, model) {
		return false, nil
	}
	mine, err := w.catalog(ctx, caller)
	if err != nil {
		return false, fmt.Errorf("reading whose machines offer %s: %w", model, err)
	}
	return memorySearchOffers(mine, model), nil
}

// memorySearchCaller is the person an owner's act is recorded as. The cluster
// acting (a sweep, an automation's system actor) is refused: a switch it
// started, or a model it chose, would name nobody.
func memorySearchCaller(ctx context.Context) (string, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || strings.TrimSpace(ac.UserId) == "" || auth.ActsForNoPerson(ctx, ac.UserId) {
		return "", fmt.Errorf("memory search's model is chosen by a signed-in cluster owner")
	}
	return strings.TrimSpace(ac.UserId), nil
}

// memorySearchSwitch is an owner choosing provider as memory search's model.
//
// With no model bound, it is bound at once through bindEmbedder, which embeds
// one sentence to learn the width: nothing is indexed under another model.
// Otherwise everything is decided under the binding lock, from the run and the
// binding read again there (an earlier look, without the lock, only says
// whether a new switch starts, so its model can be probed -- a machine call of
// seconds -- before the lock is taken; a state that moved meanwhile is "changed
// meanwhile, try again"):
//
//   - A FLIPPED switch (memorySearchFlipped). The model in use: the take-over
//     of the binding (memorySearchKeep) and of the run's indexer, so the walk
//     after the flip resumes on machines that can serve it (ruling R11 a).
//     Another model, switching back included: that switch is closed as done --
//     its remaining walk is the backfill's, over the model it made active --
//     and the new switch starts.
//   - A switch before the flip. Its own target: the take-over of the run's
//     indexer (ruling R11 b). The model in use: the take-over of the BINDING's
//     indexer (memorySearchKeep; ruling R13), the switch left as it is. Another
//     model: refused (one switch at a time).
//   - No switch in progress. The model in use: memorySearchKeep. Another: a new
//     switch -- the target's table ensured (a kept table is reused, so the walk
//     embeds only what it is missing) and the run written `indexing`, or
//     `switching` when the active index holds no vector (nothing to rebuild
//     first; the tick flips it).
//
// A machine model is indexed on the chooser's machines and the ones lent to
// them, so the chooser must be an active owner (the indexer rule, R5). A vendor
// model is paid by the cluster's account and indexed on nobody's machines.
func memorySearchSwitch(ctx context.Context, w memorySearchWorld, caller, provider string) error {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return fmt.Errorf("choose an embedding model")
	}
	_, fleet := IsFleetReference(provider)
	indexer := ""
	if fleet {
		owner, err := w.isActiveOwner(ctx, caller)
		if err != nil {
			return fmt.Errorf("memory search could not check that you are an active cluster owner: %w", err)
		}
		if !owner {
			return fmt.Errorf("a machine model indexes memory search on a cluster owner's machines, and you are not an active cluster owner")
		}
		indexer = caller
	}
	sw, _, has, err := w.readRun(ctx, memoryIndexKindSwitch)
	if err != nil {
		return err
	}
	active, bound, err := memorySearchBinding(ctx, w)
	if err != nil {
		return err
	}
	if !bound && (!has || memorySearchSwitchOver(sw.State)) {
		switch err := w.bind(ctx, provider, embedderChoice{IndexedBy: indexer, ChosenBy: caller}); {
		case errors.Is(err, errEmbedderBindingActive):
			// A binding landed between this read and bindEmbedder's own, under
			// its lock. Its refusal sends the owner to Memory search, where they
			// are: what happened is a change meanwhile, and deciding again is
			// theirs.
			return errMemorySearchChanged
		case errors.Is(err, errAskConversationHeld):
			// bindEmbedder's lock held by another request, said as
			// memorySearchLock says it.
			return errMemorySearchBusy
		default:
			return err
		}
	}
	dims := 0
	if memorySearchStartsASwitch(sw, has, active, bound, provider) {
		if dims, err = w.probe(ctx, provider, indexer); err != nil {
			return fmt.Errorf("%s could not be checked: %w", memorySearchLabel(w, provider), err)
		}
	}

	unlock, err := memorySearchLock(ctx, w)
	if err != nil {
		return err
	}
	defer unlock()
	sw, version, has, err := w.readRun(ctx, memoryIndexKindSwitch)
	if err != nil {
		return err
	}
	if active, bound, err = memorySearchBinding(ctx, w); err != nil {
		return err
	}
	running := has && !memorySearchSwitchOver(sw.State)
	flipped := running && bound && memorySearchFlipped(sw, active)
	switch {
	case flipped && provider == active.ProviderRef:
		return memorySearchTakeOverFlipped(ctx, w, caller, active, sw, version)
	case running && !flipped && provider == sw.TargetRef:
		return memorySearchTakeOverRun(ctx, w, caller, sw, version)
	case running && !flipped && bound && provider == active.ProviderRef:
		// Ruling R13: the take-over of the BINDING's indexer. The switch is left
		// as it is; search and the writers embed as the binding's indexer until
		// the flip, which names the run's.
		_, err := memorySearchKeep(ctx, w, caller, active)
		return err
	case running && !flipped:
		return memorySearchSwitchRunning(w, sw)
	case !bound:
		return errMemorySearchChanged
	case provider == active.ProviderRef:
		_, err := memorySearchKeep(ctx, w, caller, active)
		return err
	case dims == 0:
		// The look before the lock saw nothing to start.
		return errMemorySearchChanged
	}
	// THE STEPS THAT CAN FAIL COME FIRST: the active table's name, whether it
	// holds anything, the target's table. A failure among them leaves a flipped
	// switch running as it was, rather than closed for a switch that never
	// started. Whether the active index holds anything is an EXISTS, not a
	// count: only empty or not decides, and a count of a large index would hold
	// the binding lock for a scan of it.
	activeTable, err := EmbeddingVectorTable(active.ProviderRef, active.Dimensions)
	if err != nil {
		return err
	}
	indexed, err := w.hasVectors(ctx, activeTable)
	if err != nil {
		return err
	}
	table, err := w.ensureTable(ctx, provider, dims)
	if err != nil {
		return err
	}
	run := MemoryIndexRun{Kind: memoryIndexKindSwitch, State: memoryIndexStateIndexing, TargetRef: provider,
		TargetDimensions: dims, TargetTable: table, IndexedBy: indexer, StartedBy: caller, StartedAt: w.now(),
		Cursor: map[string]string{}}
	if !indexed {
		run.State = memoryIndexStateSwitching
	}
	if flipped {
		// Two writes of the one run row, the close and then the new switch,
		// fenced to the version the close stored. Only the second can still
		// fail after the first: another writer between them, or the database.
		// The flipped switch is then closed, its remaining walk the backfill's,
		// and choosing the model again starts the new switch.
		closed := cloneMemoryIndexRun(sw)
		closed.State, closed.WaitingReason, closed.FinishedAt = memoryIndexStateDone, "", w.now()
		if version, err = memorySearchWriteRun(ctx, w, closed, version); err != nil {
			return err
		}
	}
	_, err = memorySearchWriteRun(ctx, w, run, version)
	return err
}

// memorySearchBinding reads the active binding, and whether there is one.
func memorySearchBinding(ctx context.Context, w memorySearchWorld) (EmbedderBinding, bool, error) {
	b, err := w.binding(ctx)
	switch {
	case errors.Is(err, ErrNoEmbedderBound):
		return EmbedderBinding{}, false, nil
	case err != nil:
		return EmbedderBinding{}, false, err
	}
	return b, true, nil
}

// memorySearchKeep is choosing the model already in use, under the binding lock
// the caller holds: nothing is rebuilt and the model does not move. It is the
// take-over (memorySearchTakesOver): the caller becomes the indexer and the
// chooser of a machine model whose indexer cannot index it. Otherwise only an
// empty chooser is filled, which for a vendor model nobody chose is also its
// confirmation. It answers the indexer the binding names afterwards.
func memorySearchKeep(ctx context.Context, w memorySearchWorld, caller string, b EmbedderBinding) (string, error) {
	indexer, _ := IndexerFor(b)
	takeOver, err := memorySearchTakesOver(ctx, w, caller, indexer, b.ProviderRef)
	if err != nil {
		return "", err
	}
	var choice embedderChoice
	switch {
	case takeOver:
		choice, indexer = embedderChoice{IndexedBy: caller, ChosenBy: caller}, caller
	case strings.TrimSpace(b.ChosenBy) == "":
		choice.ChosenBy = caller
	}
	if choice != (embedderChoice{}) {
		if err := w.writeChoice(ctx, choice); err != nil {
			return "", err
		}
	}
	return indexer, nil
}

// memorySearchTakeOverFlipped is the model in use chosen again while a flipped
// switch's walk remains: the binding is kept (memorySearchKeep), and the run is
// then given the indexer the binding names, so the walk after the flip embeds on
// machines that can serve it. A run already on that indexer is left alone; one
// on another -- the take-over just made, or one an earlier attempt made before
// its run write was refused -- is written, fenced.
func memorySearchTakeOverFlipped(ctx context.Context, w memorySearchWorld, caller string, active EmbedderBinding, sw MemoryIndexRun, version time.Time) error {
	indexer, err := memorySearchKeep(ctx, w, caller, active)
	if err != nil {
		return err
	}
	if _, fleet := IsFleetReference(sw.TargetRef); !fleet || indexer == "" || indexer == sw.IndexedBy {
		return nil
	}
	next := cloneMemoryIndexRun(sw)
	next.IndexedBy = indexer
	_, err = memorySearchWriteRun(ctx, w, next, version)
	return err
}

// memorySearchTakeOverRun is a switch's own target chosen again before the flip:
// the take-over of the run's indexer, under the same rule. The switch goes on,
// and flips with the new indexer.
func memorySearchTakeOverRun(ctx context.Context, w memorySearchWorld, caller string, sw MemoryIndexRun, version time.Time) error {
	takeOver, err := memorySearchTakesOver(ctx, w, caller, sw.IndexedBy, sw.TargetRef)
	if err != nil || !takeOver {
		return err
	}
	next := cloneMemoryIndexRun(sw)
	next.IndexedBy = caller
	_, err = memorySearchWriteRun(ctx, w, next, version)
	return err
}

// memorySearchCancel stops the switch in progress. Under the binding lock: a
// switch whose target the binding already names has happened, and is refused
// (switching back is a new switch). Otherwise the target is recorded retired
// with retireAt = now + 30 days FIRST -- once the run says cancelled nothing
// else would ever date the partial index, and the cleanup drops only dated
// ones -- and then the run is written `cancelled`, fenced. Choosing the same
// model again reuses what was built.
func memorySearchCancel(ctx context.Context, w memorySearchWorld) error {
	unlock, err := memorySearchLock(ctx, w)
	if err != nil {
		return err
	}
	defer unlock()
	sw, version, has, err := w.readRun(ctx, memoryIndexKindSwitch)
	if err != nil {
		return err
	}
	if !has || memorySearchSwitchOver(sw.State) {
		return fmt.Errorf("no switch of memory search's model is in progress")
	}
	active, err := w.binding(ctx)
	if err != nil && !errors.Is(err, ErrNoEmbedderBound) {
		return err
	}
	if err == nil && active.ProviderRef == sw.TargetRef && active.Dimensions == sw.TargetDimensions {
		return fmt.Errorf("the switch to %s has already happened and search uses it now; switching back is a new switch, and the previous model's index is kept for 30 days",
			memorySearchLabel(w, sw.TargetRef))
	}
	if RetiredEmbedderBindingID(sw.TargetRef, sw.TargetDimensions) == "" {
		return fmt.Errorf("the switch names no model to keep the index of")
	}
	if err := w.recordRetired(ctx, retiredMemoryIndex{ProviderRef: sw.TargetRef, Dimensions: sw.TargetDimensions,
		IndexedBy: sw.IndexedBy, RetireAt: w.now().Add(memoryIndexRetention)}); err != nil {
		return err
	}
	next := cloneMemoryIndexRun(sw)
	next.State, next.WaitingReason = memoryIndexStateCancelled, ""
	_, err = memorySearchWriteRun(ctx, w, next, version)
	return err
}

// memorySearchSwitchAnyway is the owner's answer to a switch held in
// needsDecision for items that failed or that no source rebuilds: the run is
// written `switching`, fenced, and the job's next tick flips. Those items drop
// out of search until they are written again or a later walk picks them up.
func memorySearchSwitchAnyway(ctx context.Context, w memorySearchWorld) error {
	sw, version, has, err := w.readRun(ctx, memoryIndexKindSwitch)
	if err != nil {
		return err
	}
	if !has || sw.State != memoryIndexStateNeedsDecision {
		return fmt.Errorf("Switch anyway is for a switch waiting for your decision, and none is")
	}
	next := cloneMemoryIndexRun(sw)
	next.State, next.WaitingReason = memoryIndexStateSwitching, ""
	_, err = memorySearchWriteRun(ctx, w, next, version)
	return err
}

// memorySearchConfirm is an owner confirming a VENDOR model nobody chose (bound
// by the vendor fallback the design removed): under the binding lock it records
// the caller as its chooser. Once: a vendor model with a chooser is left as it
// is. A machine model is never put to an owner this way.
func memorySearchConfirm(ctx context.Context, w memorySearchWorld, caller string) error {
	unlock, err := memorySearchLock(ctx, w)
	if err != nil {
		return err
	}
	defer unlock()
	b, err := w.binding(ctx)
	if errors.Is(err, ErrNoEmbedderBound) {
		return fmt.Errorf("no memory-search model is bound yet")
	}
	if err != nil {
		return err
	}
	if _, fleet := IsFleetReference(b.ProviderRef); fleet {
		return fmt.Errorf("memory search uses a machine model, %s; only a vendor model nobody chose is confirmed", memorySearchLabel(w, b.ProviderRef))
	}
	if strings.TrimSpace(b.ChosenBy) != "" {
		return nil
	}
	return w.writeChoice(ctx, embedderChoice{ChosenBy: caller})
}

// memoryIndexRetire is the daily cleanup: under the binding lock, each retired
// index past its retireAt and not yet dropped is dropped and its row records
// droppedAt -- except the active model's table and a running switch's target,
// which are never dropped whatever a retired row says. A table that cannot be
// dropped is reported, and the sweep goes on with the next.
//
// The binding lock is what the flip and the owner's acts hold, so the cleanup
// never reads the protected set and then drops a table that became active or a
// target meanwhile. Held elsewhere, the sweep fails this run and tomorrow's
// tries again: a day late costs disk, never an index in use.
func memoryIndexRetire(ctx context.Context, w memorySearchWorld) error {
	unlock, err := w.lockBinding(ctx)
	switch {
	case errors.Is(err, errAskConversationHeld):
		return fmt.Errorf("memory index cleanup: the memory-search model is being changed; tomorrow's run tries again")
	case err != nil:
		return fmt.Errorf("memory index cleanup: the binding lock could not be taken; tomorrow's run tries again: %w", err)
	}
	defer unlock()
	active, _, err := memorySearchBinding(ctx, w)
	if err != nil {
		return err
	}
	sw, _, has, err := w.readRun(ctx, memoryIndexKindSwitch)
	if err != nil {
		return err
	}
	keep := memorySearchTablesInUse(active, sw, has)
	retired, err := w.retired(ctx)
	if err != nil {
		return err
	}
	now := w.now()
	var failures []error
	for _, r := range retired {
		if !r.DroppedAt.IsZero() || r.RetireAt.IsZero() || now.Before(r.RetireAt) {
			continue
		}
		table, err := EmbeddingVectorTable(r.ProviderRef, r.Dimensions)
		if err != nil || keep[table] {
			continue
		}
		if err := w.dropTable(ctx, table); err != nil {
			failures = append(failures, fmt.Errorf("dropping %s's index: %w", r.ProviderRef, err))
			continue
		}
		r.DroppedAt = now
		if err := w.recordRetired(ctx, r); err != nil {
			failures = append(failures, fmt.Errorf("recording %s's index dropped: %w", r.ProviderRef, err))
			continue
		}
		w.logger().Info("memory index: dropped a retired model's index past its 30 days",
			"provider", r.ProviderRef, "dimensions", r.Dimensions, "retireAt", r.RetireAt)
	}
	return errors.Join(failures...)
}

// --- the builtins' handlers -------------------------------------------------

func (e *MemQLEngine) memorySearch() memorySearchWorld { return engineMemorySearchWorld{e: e} }

// memorySearchStatusRow answers the status as its builtin's one row, id `current`.
func memorySearchStatusRow(ctx context.Context, w memorySearchWorld, caller string) ([]memorynodes.MemoryNode, error) {
	row, err := memorySearchStatus(ctx, w, caller)
	if err != nil {
		return nil, err
	}
	return singleVirtualRow(MemorySearchStatusConcept, "current", row)
}

// memorySearchStatusBuiltin answers the status as the signed-in caller sees it.
// A read records nobody, so the cluster asking is answered too, with no
// take-over to offer (an empty caller).
func (e *MemQLEngine) memorySearchStatusBuiltin(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	caller, _ := memorySearchCaller(ctx)
	return memorySearchStatusRow(ctx, e.memorySearch(), caller)
}

func (e *MemQLEngine) memorySearchChoicesBuiltin(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	caller, err := memorySearchCaller(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := memorySearchChoices(ctx, e.memorySearch(), caller)
	if err != nil {
		return nil, err
	}
	out := make([]memorynodes.MemoryNode, 0, len(rows))
	for _, row := range rows {
		node, err := singleVirtualRow(MemorySearchChoiceConcept, row["providerRef"].(string), row)
		if err != nil {
			return nil, err
		}
		out = append(out, node...)
	}
	return out, nil
}

// memorySearchAct runs one owner act as the signed-in caller, and answers the
// status after it.
func (e *MemQLEngine) memorySearchAct(ctx context.Context, act func(w memorySearchWorld, caller string) error) ([]memorynodes.MemoryNode, error) {
	caller, err := memorySearchCaller(ctx)
	if err != nil {
		return nil, err
	}
	w := e.memorySearch()
	if err := act(w, caller); err != nil {
		return nil, err
	}
	return memorySearchStatusRow(ctx, w, caller)
}

func (e *MemQLEngine) memorySearchSwitchBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return e.memorySearchAct(ctx, func(w memorySearchWorld, caller string) error {
		return memorySearchSwitch(ctx, w, caller, stringArg(args, "provider"))
	})
}

func (e *MemQLEngine) memorySearchCancelBuiltin(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return e.memorySearchAct(ctx, func(w memorySearchWorld, _ string) error { return memorySearchCancel(ctx, w) })
}

func (e *MemQLEngine) memorySearchSwitchAnywayBuiltin(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return e.memorySearchAct(ctx, func(w memorySearchWorld, _ string) error { return memorySearchSwitchAnyway(ctx, w) })
}

func (e *MemQLEngine) memorySearchConfirmBuiltin(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return e.memorySearchAct(ctx, func(w memorySearchWorld, caller string) error { return memorySearchConfirm(ctx, w, caller) })
}

// memoryIndexRetireBuiltin is builtin memoryIndexRetire: the daily cleanup, for
// its schedule (memoryIndexRetentionSweep) and nothing else. THE GATE IS HERE,
// for memoryIndexStep's reason: a builtin takes no @serverOnly and any signed-in
// client's query can name one, so a call without internal origin is refused
// before anything is read. It runs as the index job's own system actor.
func (e *MemQLEngine) memoryIndexRetireBuiltin(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return nil, fmt.Errorf("memoryIndexRetire is memory search's daily cleanup: only its schedule runs it")
	}
	return nil, memoryIndexRetire(auth.ContextWithSystemActor(ctx, memoryIndexActorName), e.memorySearch())
}
