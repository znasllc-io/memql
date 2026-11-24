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
// A builtin moves the binding in one case only: a cluster with NO model binds
// its first through bindEmbedder, because nothing is indexed under another
// model. Every other change of model is the index job's (memory_index_job.go):
// a builtin writes the `switch` run row and the job's tick rebuilds and flips.
// No builtin calls flipMemoryIndex, Switch anyway included. TRIGGER for that
// rule: the flip is ordered steps finished by the next tick from `switching`
// (ruling R6); a builtin that died after moving the binding would leave the run
// in a state the tick never resumes. So memorySearchSwitchAnyway writes
// `switching` and the tick flips within a minute, and a switch over an index
// that holds no vector is written `switching` directly for the same reason.
//
// # Every run-row write is fenced, and a refusal is answered
//
// The run rows are written through writeMemoryIndexRun, fenced to the version
// the builtin read: the job writes after every batch, and an unfenced write
// would revert its cursor or be reverted by it. A refused fence reaches the
// owner as "changed meanwhile, try again" (errMemorySearchChanged) and is never
// retried here: the state it was decided on is gone, and deciding again is the
// owner's call. The acts that read the binding and decide on it -- switch,
// cancel, confirm, and the cleanup -- hold the binding lock
// (lockAskConversationKind "active", "embedder-binding") around those reads and
// their writes, which is what the flip holds, so neither can interleave with a
// flip on another replica (memql#5431). The lock is a try-lock: held elsewhere,
// the act says "try again" rather than waiting.
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
// Regression tests: memory_search_builtins_test.go (the decisions, over the
// job's map world) and memory_search_builtins_db_test.go (Postgres, two engines).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/num"
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

// memoryIndexModelTable is a model's own vector table, the only kind the cleanup
// may drop: node_vectors_<width>_<16 hex> (EmbeddingVectorTable). The shared
// tables of older releases (node_vectors, node_vectors_<width>) never match.
var memoryIndexModelTable = regexp.MustCompile(`^node_vectors_[0-9]+_[0-9a-f]{16}$`)

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
	// writeRun writes the whole run fenced to version, and answers
	// errMemoryIndexRunChanged when somebody wrote it since.
	writeRun(ctx context.Context, run MemoryIndexRun, version time.Time) error
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
	retired(ctx context.Context) ([]retiredMemoryIndex, error)
	recordRetired(ctx context.Context, r retiredMemoryIndex) error
	dropTable(ctx context.Context, table string) error
	isActiveOwner(ctx context.Context, userId string) bool
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

// memorySearchLock takes the binding lock, or says plainly that another request
// holds it.
func memorySearchLock(ctx context.Context, w memorySearchWorld) (func(), error) {
	unlock, err := w.lockBinding(ctx)
	if err != nil {
		return nil, fmt.Errorf("memory search's model is being changed elsewhere right now; try again in a moment (%w)", err)
	}
	return unlock, nil
}

// memorySearchWriteRun writes the run fenced to the version read, and answers a
// refused fence as errMemorySearchChanged.
func memorySearchWriteRun(ctx context.Context, w memorySearchWorld, run MemoryIndexRun, version time.Time) error {
	if err := w.writeRun(ctx, run, version); err != nil {
		if errors.Is(err, errMemoryIndexRunChanged) {
			return errMemorySearchChanged
		}
		return err
	}
	return nil
}

// memorySearchSwitchRunning is the refusal of a second switch.
func memorySearchSwitchRunning(w memorySearchWorld, sw MemoryIndexRun) error {
	return fmt.Errorf("a switch to %s is still in progress (%s); cancel it or let it finish before choosing another model",
		memorySearchLabel(w, sw.TargetRef), sw.State)
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
// In order:
//
//  1. A switch in progress refuses another (memorySearchSwitchOver).
//  2. No model bound: bound at once through bindEmbedder, which embeds one
//     sentence to learn the width; nothing is indexed under another model.
//  3. The model in use: nothing moves (memorySearchKeep).
//  4. Otherwise the new model is probed, as its indexer, OUTSIDE the lock (a
//     machine call takes seconds); then, under the binding lock, the run and
//     the binding are read again, and a binding moved since is "changed
//     meanwhile". The target's table is ensured -- a table kept from earlier is
//     reused, so the walk embeds only what it is missing -- and the run is
//     written `indexing`, or `switching` when the active index holds no vector
//     (nothing to rebuild first; the tick flips it).
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
		if !w.isActiveOwner(ctx, caller) {
			return fmt.Errorf("a machine model indexes memory search on a cluster owner's machines, and you are not an active cluster owner")
		}
		indexer = caller
	}
	sw, _, has, err := w.readRun(ctx, memoryIndexKindSwitch)
	if err != nil {
		return err
	}
	if has && !memorySearchSwitchOver(sw.State) {
		return memorySearchSwitchRunning(w, sw)
	}
	active, err := w.binding(ctx)
	switch {
	case errors.Is(err, ErrNoEmbedderBound):
		return w.bind(ctx, provider, embedderChoice{IndexedBy: indexer, ChosenBy: caller})
	case err != nil:
		return err
	case active.ProviderRef == provider:
		return memorySearchKeep(ctx, w, caller, provider)
	}
	dims, err := w.probe(ctx, provider, indexer)
	if err != nil {
		return fmt.Errorf("%s could not be checked: %w", memorySearchLabel(w, provider), err)
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
	if has && !memorySearchSwitchOver(sw.State) {
		return memorySearchSwitchRunning(w, sw)
	}
	current, err := w.binding(ctx)
	if err != nil && !errors.Is(err, ErrNoEmbedderBound) {
		return err
	}
	if err != nil || current.ProviderRef != active.ProviderRef || current.Dimensions != active.Dimensions {
		return errMemorySearchChanged
	}
	activeTable, err := EmbeddingVectorTable(active.ProviderRef, active.Dimensions)
	if err != nil {
		return err
	}
	indexed, err := w.vectors(ctx, activeTable)
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
	if indexed == 0 {
		run.State = memoryIndexStateSwitching
	}
	return memorySearchWriteRun(ctx, w, run, version)
}

// memorySearchKeep is choosing the model already in use: nothing is rebuilt and
// the binding's model does not move. Under the binding lock, it records the
// chooser where nobody is recorded, and makes the chooser the indexer of a
// machine model that has none or whose indexer is no longer an active owner --
// the job's wait for a demoted indexer says "an owner can choose again", and
// this is that choice. An indexer who is still an owner is kept.
func memorySearchKeep(ctx context.Context, w memorySearchWorld, caller, provider string) error {
	unlock, err := memorySearchLock(ctx, w)
	if err != nil {
		return err
	}
	defer unlock()
	b, err := w.binding(ctx)
	if err != nil && !errors.Is(err, ErrNoEmbedderBound) {
		return err
	}
	if err != nil || b.ProviderRef != provider {
		return errMemorySearchChanged
	}
	var choice embedderChoice
	indexer, fleet := IndexerFor(b)
	takeOver := fleet && (indexer == "" || !w.isActiveOwner(ctx, indexer))
	if takeOver {
		choice.IndexedBy = caller
	}
	if takeOver || strings.TrimSpace(b.ChosenBy) == "" {
		choice.ChosenBy = caller
	}
	if choice == (embedderChoice{}) {
		return nil
	}
	return w.writeChoice(ctx, choice)
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
	return memorySearchWriteRun(ctx, w, next, version)
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
	return memorySearchWriteRun(ctx, w, next, version)
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

// memorySearchStatus is the one row Memory search reads.
//
//   - The model in use: providerRef, kind, label; whose machines index it
//     (indexedBy, indexedByName; empty for a vendor model, which the cluster's
//     account pays for) and who chose it (chosenBy); needsConfirmation for a
//     vendor model nobody chose.
//   - indexed: the entries in its vector table.
//   - backlog and waitingReason, from the backfill run while it is about this
//     model: backlog is the items its latest walk found missing and could not
//     index (counts.failed). A LOWER BOUND while it waits: a waiting walk stops
//     at the first item it cannot embed and counts nothing past it, which
//     waitingReason says.
//   - switch: the switch run's state, target, counts, start and starter, and
//     its own waitingReason; null when there has never been one. counts.total
//     is the items the CURRENT walk has visited and starts again with each
//     walk, so estimatedTotal gives the screen a denominator: AN ESTIMATE,
//     indexed + backlog, the size of the active index plus what is known to be
//     missing from it. It is not a walk of the corpus, which a poll must not
//     cost, and it counts entries no source rebuilds.
//   - retained: the retired indexes still kept, {providerRef, retireAt}, by
//     date. A row naming the active model or a running switch's target is not
//     one: that table is in use.
func memorySearchStatus(ctx context.Context, w memorySearchWorld) (map[string]any, error) {
	row := map[string]any{"providerRef": "", "kind": "", "label": "", "indexedBy": "", "indexedByName": "", "chosenBy": "",
		"needsConfirmation": false, "indexed": 0, "backlog": 0, "waitingReason": "", "switch": nil, "retained": []any{}}
	active, err := w.binding(ctx)
	if err != nil && !errors.Is(err, ErrNoEmbedderBound) {
		return nil, err
	}
	bound := err == nil && active.Valid()
	activeTable, indexed, backlog := "", 0, 0
	if bound {
		row["providerRef"] = active.ProviderRef
		row["kind"] = memorySearchKind(active.ProviderRef)
		row["label"] = memorySearchLabel(w, active.ProviderRef)
		indexer, fleet := IndexerFor(active)
		row["indexedBy"] = indexer
		if indexer != "" {
			row["indexedByName"] = w.displayName(ctx, indexer)
		}
		row["chosenBy"] = strings.TrimSpace(active.ChosenBy)
		row["needsConfirmation"] = !fleet && strings.TrimSpace(active.ChosenBy) == ""
		if activeTable, err = EmbeddingVectorTable(active.ProviderRef, active.Dimensions); err != nil {
			return nil, err
		}
		if indexed, err = w.vectors(ctx, activeTable); err != nil {
			return nil, err
		}
		row["indexed"] = indexed
	}
	bf, _, hasBackfill, err := w.readRun(ctx, memoryIndexKindBackfill)
	if err != nil {
		return nil, err
	}
	if bound && hasBackfill && bf.TargetRef == active.ProviderRef && bf.TargetDimensions == active.Dimensions {
		backlog = bf.Counts.Failed
		row["backlog"] = backlog
		if bf.State == memoryIndexStateWaiting {
			row["waitingReason"] = bf.WaitingReason
		}
	}
	sw, _, hasSwitch, err := w.readRun(ctx, memoryIndexKindSwitch)
	if err != nil {
		return nil, err
	}
	targetTable := ""
	if hasSwitch {
		if !memorySearchSwitchOver(sw.State) {
			targetTable, _ = EmbeddingVectorTable(sw.TargetRef, sw.TargetDimensions)
		}
		startedAt := ""
		if !sw.StartedAt.IsZero() {
			startedAt = sw.StartedAt.UTC().Format(time.RFC3339Nano)
		}
		row["switch"] = map[string]any{
			"state": sw.State, "targetRef": sw.TargetRef, "targetLabel": memorySearchLabel(w, sw.TargetRef),
			"counts": map[string]any{"total": sw.Counts.Total, "done": sw.Counts.Done, "skipped": sw.Counts.Skipped,
				"unsourced": sw.Counts.Unsourced, "failed": sw.Counts.Failed},
			"startedAt": startedAt, "startedBy": sw.StartedBy, "waitingReason": sw.WaitingReason,
			"estimatedTotal": indexed + backlog,
		}
	}
	retired, err := w.retired(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(retired, func(i, j int) bool { return retired[i].RetireAt.Before(retired[j].RetireAt) })
	retained := []any{}
	for _, r := range retired {
		table, err := EmbeddingVectorTable(r.ProviderRef, r.Dimensions)
		if err != nil || !r.DroppedAt.IsZero() || r.RetireAt.IsZero() || table == activeTable || table == targetTable {
			continue
		}
		retained = append(retained, map[string]any{"providerRef": r.ProviderRef, "retireAt": r.RetireAt.UTC().Format(time.RFC3339Nano)})
	}
	row["retained"] = retained
	return row, nil
}

// memorySearchChoices are the models caller may choose (design record, section
// 3), one row each: {providerRef, kind, label, machines, paidByCluster,
// available, hint}.
//
//   - MACHINE models: the embedding models of caller's own catalog -- their
//     machines and the ones lent to them, what the router reads for a fleet door
//     -- counted by the machines that embed with the model; available while one
//     of those is online.
//   - VENDOR models: only the vendor embedding records the cluster's own account
//     is set up for, each marked paidByCluster. "Set up" is the vendor-account
//     predicate (federationReadiness, VendorDoorFor's ClusterSetUp: an available
//     record whose credential came through federation), applied to embedding
//     records by federationEmbedderRecords. Who the account pays for is not
//     asked: an embedding is the cluster's work, paid by its account for
//     everybody (vendor_account_cluster.go, "EMBEDDINGS ARE NOT ASKED").
func memorySearchChoices(ctx context.Context, w memorySearchWorld, caller string) ([]map[string]any, error) {
	models, err := w.catalog(ctx, caller)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, m := range models {
		if !m.Embeddings || strings.TrimSpace(m.ModelId) == "" {
			continue
		}
		machines, online := 0, 0
		for _, mm := range m.Machines {
			if mm.Capabilities.Embeddings {
				machines++
				if mm.Online {
					online++
				}
			}
		}
		if machines == 0 {
			// A reader that reports the capability for the model only: every
			// machine behind it counts.
			for _, mm := range m.Machines {
				machines++
				if mm.Online {
					online++
				}
			}
		}
		hint := "None of the machines offering it is online"
		switch {
		case online == 1:
			hint = "1 machine offering it is online"
		case online > 1:
			hint = fmt.Sprintf("%d machines offering it are online", online)
		}
		out = append(out, map[string]any{"providerRef": FleetReferencePrefix + m.ModelId, "kind": "machine", "label": m.ModelId,
			"machines": machines, "paidByCluster": false, "available": online > 0, "hint": hint})
	}
	for _, v := range w.vendorEmbedders() {
		label := strings.TrimSpace(v.Model)
		if label == "" {
			label = v.Name
		}
		out = append(out, map[string]any{"providerRef": v.Name, "kind": "vendor", "label": label, "machines": 0,
			"paidByCluster": true, "available": true, "hint": "Paid by the cluster's vendor account"})
	}
	return out, nil
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
	if err != nil {
		return fmt.Errorf("memory index cleanup: the memory-search model is being changed; tomorrow's run tries again: %w", err)
	}
	defer unlock()
	keep := map[string]bool{}
	active, err := w.binding(ctx)
	if err != nil && !errors.Is(err, ErrNoEmbedderBound) {
		return err
	}
	if err == nil && active.Valid() {
		if table, err := EmbeddingVectorTable(active.ProviderRef, active.Dimensions); err == nil {
			keep[table] = true
		}
	}
	sw, _, has, err := w.readRun(ctx, memoryIndexKindSwitch)
	if err != nil {
		return err
	}
	if has && !memorySearchSwitchOver(sw.State) {
		if table, err := EmbeddingVectorTable(sw.TargetRef, sw.TargetDimensions); err == nil {
			keep[table] = true
		}
	}
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
func memorySearchStatusRow(ctx context.Context, w memorySearchWorld) ([]memorynodes.MemoryNode, error) {
	row, err := memorySearchStatus(ctx, w)
	if err != nil {
		return nil, err
	}
	return singleVirtualRow(MemorySearchStatusConcept, "current", row)
}

func (e *MemQLEngine) memorySearchStatusBuiltin(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return memorySearchStatusRow(ctx, e.memorySearch())
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
	return memorySearchStatusRow(ctx, w)
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

// --- the engine's world -----------------------------------------------------

// engineMemorySearchWorld is the builtins' world over Postgres.
type engineMemorySearchWorld struct{ e *MemQLEngine }

func (w engineMemorySearchWorld) binding(ctx context.Context) (EmbedderBinding, error) {
	return w.e.ReadEmbedderBinding(ctx)
}

func (w engineMemorySearchWorld) lockBinding(ctx context.Context) (func(), error) {
	return w.e.lockAskConversationKind(ctx, ActiveEmbedderBindingID, "embedder-binding")
}

func (w engineMemorySearchWorld) readRun(ctx context.Context, kind string) (MemoryIndexRun, time.Time, bool, error) {
	return w.e.readMemoryIndexRun(ctx, kind)
}

func (w engineMemorySearchWorld) writeRun(ctx context.Context, run MemoryIndexRun, version time.Time) error {
	_, err := w.e.writeMemoryIndexRun(ctx, run, version)
	return err
}

func (w engineMemorySearchWorld) writeChoice(ctx context.Context, choice embedderChoice) error {
	return w.e.writeEmbedderChoice(ctx, choice)
}

func (w engineMemorySearchWorld) bind(ctx context.Context, provider string, choice embedderChoice) error {
	_, err := w.e.bindEmbedder(ctx, provider, choice)
	return err
}

func (w engineMemorySearchWorld) probe(ctx context.Context, provider, indexer string) (int, error) {
	return w.e.probeEmbedder(ctx, provider, indexer)
}

func (w engineMemorySearchWorld) ensureTable(ctx context.Context, ref string, dims int) (string, error) {
	db := w.e.database()
	if db == nil {
		return "", fmt.Errorf("memory search: no database")
	}
	return EnsureEmbeddingVectorTable(ctx, db.DB, ref, dims)
}

// vectors counts a vector table's entries. The name comes from
// EmbeddingVectorTable and is checked again before it enters the statement.
func (w engineMemorySearchWorld) vectors(ctx context.Context, table string) (int, error) {
	db := w.e.database()
	if db == nil || !embeddingTableName.MatchString(table) {
		return 0, fmt.Errorf("memory search: cannot read vector table %q", table)
	}
	var exists bool
	if err := db.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = current_schema() AND tablename = $1)`, table).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var n int64
	if err := db.DB.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		return 0, err
	}
	return num.ClampInt64(n), nil
}

// retiredEmbedderPayload is a retired model's row as stored.
type retiredEmbedderPayload struct {
	ProviderRef string `json:"providerRef"`
	Dimensions  int    `json:"dimensions"`
	IndexedBy   string `json:"indexedBy"`
	RetireAt    string `json:"retireAt"`
	DroppedAt   string `json:"droppedAt"`
}

// retired reads the newest version of every retired model's row
// (`retired-<16 hex>`, RetiredEmbedderBindingID), straight off the store, so the
// cleanup and the status decide from what is stored now and never from a
// result cache.
//
// staged-data: MUST-NOT-GATE -- the retained list of the cluster's own vector
// tables (a core platform concept, never promoted). Read as staged, a retired
// index would read as absent: the cleanup would never drop it, and Memory search
// would not offer it for switching back.
func (w engineMemorySearchWorld) retired(ctx context.Context) ([]retiredMemoryIndex, error) {
	db := w.e.database()
	if db == nil {
		return nil, fmt.Errorf("memory search: no database")
	}
	var nodes []memorynodes.MemoryNode
	err := db.NewSelect().Model(&nodes).DistinctOn("id").
		Where("concept = ?", EmbedderBindingConceptID).
		Where("id LIKE ?", EmbedderBindingConceptID+":"+retiredEmbedderBindingPrefix+"%").
		OrderExpr(`id ASC, "createdAt" DESC`).Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("memory search: reading the retired indexes: %w", err)
	}
	out := make([]retiredMemoryIndex, 0, len(nodes))
	for _, n := range nodes {
		var p retiredEmbedderPayload
		if err := json.Unmarshal(n.Payload, &p); err != nil {
			w.e.logOrDefault().Warn("memory search: a retired index's row does not decode; it is left as it is", "id", n.ID, "error", err)
			continue
		}
		out = append(out, retiredMemoryIndex{ProviderRef: p.ProviderRef, Dimensions: p.Dimensions, IndexedBy: p.IndexedBy,
			RetireAt: memoryIndexTime(p.RetireAt), DroppedAt: memoryIndexTime(p.DroppedAt)})
	}
	return out, nil
}

// recordRetired writes a retired model's row WHOLE (recordRetiredEmbedder), at
// the id derived from its model and width, never one taken from input.
func (w engineMemorySearchWorld) recordRetired(ctx context.Context, r retiredMemoryIndex) error {
	id := RetiredEmbedderBindingID(r.ProviderRef, r.Dimensions)
	if id == "" {
		return fmt.Errorf("memory search: %q has no index to record", r.ProviderRef)
	}
	args := map[string]any{"bindingId": id, "providerRef": r.ProviderRef, "dimensions": r.Dimensions}
	if v := strings.TrimSpace(r.IndexedBy); v != "" {
		args["indexedBy"] = v
	}
	memoryIndexTimeArg(args, "retireAt", r.RetireAt)
	memoryIndexTimeArg(args, "droppedAt", r.DroppedAt)
	return w.e.memoryIndexMutation(auth.ContextWithInternalOrigin(ctx), "platform.recordRetiredEmbedder", args)
}

// dropTable drops one model's vector table. Only a name of a model's own table
// (memoryIndexModelTable) is accepted, so no other table can reach the
// statement.
func (w engineMemorySearchWorld) dropTable(ctx context.Context, table string) error {
	db := w.e.database()
	if db == nil || !memoryIndexModelTable.MatchString(table) {
		return fmt.Errorf("memory search: refusing to drop table %q", table)
	}
	_, err := db.DB.ExecContext(ctx, `DROP TABLE IF EXISTS `+table)
	return err
}

func (w engineMemorySearchWorld) isActiveOwner(ctx context.Context, userId string) bool {
	return w.e.IndexerIsActiveOwner(ctx, userId)
}

func (w engineMemorySearchWorld) catalog(ctx context.Context, userId string) ([]FleetModel, error) {
	if w.e.providers == nil {
		return nil, nil
	}
	return w.e.providers.FleetCatalog(ctx, userId)
}

func (w engineMemorySearchWorld) vendorEmbedders() []vendorEmbedderRecord {
	if w.e.providers == nil {
		return nil
	}
	return w.e.providers.federationEmbedderRecords()
}

// displayName is a person's display name, through the narrow cross-user read
// (userDisplayById: id and displayName, nothing else); empty when unknown.
func (w engineMemorySearchWorld) displayName(ctx context.Context, userId string) string {
	call, err := parser.RenderCall("identity.userDisplayById", map[string]any{"userId": BareShortId(userId)})
	if err != nil {
		return ""
	}
	res, err := w.e.Execute(ctx, "query "+call)
	if err != nil {
		return ""
	}
	rows := MaterializeRows(res.OutputPayload())
	if len(rows) != 1 {
		return ""
	}
	return strings.TrimSpace(stringFromAny(rows[0]["displayName"]))
}

func (w engineMemorySearchWorld) now() time.Time { return time.Now().UTC() }

func (w engineMemorySearchWorld) logger() *slog.Logger { return w.e.logOrDefault() }
