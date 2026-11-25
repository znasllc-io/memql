package memql

// memory_index_flip.go -- the flip: a switch's target becomes the active
// memory-search model, in ordered steps under the binding lock. Why it is
// not one transaction, and what each prefix of it leaves, is in
// memory_index_job.go's header ("The flip").

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// flipMemoryIndex makes a switch's target the active memory-search model: the
// active model's row is kept as retired for memoryIndexRetention, the binding
// names the target, the semantic answer cache is emptied (its vectors are the
// old model's), and the run goes to `finishing` for the walk that covers what
// was written meanwhile. See memory_index_job.go's header for why this is
// ordered steps under one lock rather than one transaction, and why a crash
// anywhere in it is finished by the next tick.
//
// run is the switch the caller read; the flip happens only for that switch
// (same target, same start) in `switching`. An owner's Switch anyway writes
// `switching` too, and the next tick flips it (memorySearchSwitchAnyway): a
// run held in needsDecision is the owner's to decide, and is refused here.
// The binding lock (lockAskConversationKind "active", "embedder-binding") is
// held around the fresh reads and every write, and the binding write is
// fenced to the version read under it, so a concurrent binding writer on
// another replica is never reverted and the new version is stamped after the
// one read (memql#5431). Every id it writes is derived here
// (RetiredEmbedderBindingID), never taken from input.
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
	if current.State != memoryIndexStateSwitching {
		return fmt.Errorf("memory index: a switch flips from switching, not from %s", current.State)
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
