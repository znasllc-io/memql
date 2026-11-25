package memql

// memory_search_status.go -- the two reads behind Cluster > Settings > Memory
// search: the status row, and the models an owner may choose. The acts, their
// gates and the rules they share are memory_search_builtins.go's (its header).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// memorySearchMayAct is the status's `mayAct`: whether caller may act on memory
// search at all, asked from the caller's side -- `execute` on the part, as the
// builtin path judges an act's call, AND an active cluster owner, as every act
// requires (memorySearchOwnerGate). The screen offers no act where it is false:
// a person granted the part who is not an owner sees the page and its state,
// and is offered nothing the engine would refuse. The cluster asking (no
// caller) may not act. An owner check that cannot be made answers false,
// logged, rather than failing the status: the next read asks again.
func memorySearchMayAct(ctx context.Context, w memorySearchWorld, caller string) bool {
	if strings.TrimSpace(caller) == "" || !w.mayExecute(ctx) {
		return false
	}
	owner, err := w.isActiveOwner(ctx, caller)
	if err != nil {
		w.logger().Warn("memory search: could not tell whether the caller is an active cluster owner", "error", err)
		return false
	}
	return owner
}

// memorySearchOffersTakeOver is a status `takeOver`: whether caller, choosing
// ref again, would become its indexer in place of indexer -- memorySearchSwitch's
// own take-over (memorySearchTakesOver), asked without writing. It is asked of
// the model in use and the BINDING's indexer (the row's takeOver, ruling R13),
// and of a running switch's target and the RUN's indexer (the switch summary's,
// ruling R14), and only of a caller who may act (mayAct): to anybody else
// choosing again is refused, so it takes over nothing.
//
// A catalog that cannot be read answers false, logged, rather than failing the
// status: the screen then withholds the act for that read and still shows what
// is in use, and the next read asks again. An act offered on a guess would be
// one the engine might answer by doing nothing.
func memorySearchOffersTakeOver(ctx context.Context, w memorySearchWorld, caller, indexer, ref string) bool {
	if _, fleet := IsFleetReference(ref); !fleet {
		return false
	}
	takeOver, err := memorySearchTakesOver(ctx, w, caller, indexer, ref)
	if err != nil {
		w.logger().Warn("memory search: could not tell whether choosing a model again would take over its indexing", "provider", ref, "error", err)
		return false
	}
	return takeOver
}

// memorySearchStatus is the one row Memory search reads, as caller sees it.
//
//   - The model in use: providerRef, kind, label; whose machines index it
//     (indexedBy, indexedByName; empty for a vendor model, which the cluster's
//     account pays for) and who chose it (chosenBy); needsConfirmation for a
//     vendor model nobody chose; previousRef, the model the binding replaced
//     (empty for a cluster's first model), which is the index Switch back names
//     -- `retained` also holds a cancelled switch's partial index, which was
//     never in use.
//   - mayAct: whether caller may act on memory search at all
//     (memorySearchMayAct): an active cluster owner holding the part's
//     `execute`. Nobody else is offered an act, a take-over included.
//   - takeOver: whether caller, choosing the model in use again, would become
//     its indexer (memorySearchOffersTakeOver; ruling R13). It is how the status
//     says the binding's indexer cannot index the model, asked from the caller's
//     side so that the screen offers the act only where it changes something.
//   - indexed: the entries in its vector table, as this replica last counted
//     them: kept for memorySearchIndexedTTL (vectorCountCache), so it may lag
//     the table by up to that long.
//   - backlog, backlogCountedAt and waitingReason, from the backfill run while
//     it is about this model: backlog is the items its last count found absent
//     from the index or older there than their row (MemoryIndexRun.Missing:
//     what its last completed walk left missing, or what the counting walk a
//     waiting backfill takes found -- ruling R10), and backlogCountedAt when
//     that count was taken; empty when none has been. waitingReason says why
//     the backfill waits, if it does.
//   - switch: the switch run's state, target, counts, start and starter, and
//     its own waitingReason; null when there has never been one. counts.total
//     is the items the CURRENT walk has visited and starts again with each
//     walk, so estimatedTotal gives the screen a denominator: AN ESTIMATE,
//     indexed + backlog, the size of the active index plus what is known to be
//     missing from it. It is not a walk of the corpus, which a poll must not
//     cost, and it counts entries no source rebuilds. indexedBy and
//     indexedByName name the RUN's indexer, whose machines the rebuild embeds
//     on, so the screen can say its waitingReason from the viewer's side; and
//     takeOver says whether caller, choosing the switch's target again, would
//     become that indexer (ruling R14; R11 b is the act). It is false once the
//     switch has flipped -- its target is then the model in use, whose
//     take-over the row's takeOver says, and one act is offered once -- and
//     once the switch is over.
//   - retained: the retired indexes still kept, {providerRef, retireAt}, by
//     date. A row naming the active model or a running switch's target is not
//     one: that table is in use. Nor is one past its date, which the daily
//     cleanup drops at its next run: listed, it read as kept until a day gone.
func memorySearchStatus(ctx context.Context, w memorySearchWorld, caller string) (map[string]any, error) {
	mayAct := memorySearchMayAct(ctx, w, caller)
	row := map[string]any{"providerRef": "", "kind": "", "label": "", "indexedBy": "", "indexedByName": "", "chosenBy": "",
		"needsConfirmation": false, "previousRef": "", "mayAct": mayAct, "takeOver": false, "indexed": 0, "backlog": 0,
		"backlogCountedAt": "", "waitingReason": "", "switch": nil, "retained": []any{}}
	active, err := w.binding(ctx)
	if err != nil && !errors.Is(err, ErrNoEmbedderBound) {
		return nil, err
	}
	bound := err == nil && active.Valid()
	indexed, backlog := 0, 0
	indexer, indexerName := "", ""
	if bound {
		row["providerRef"] = active.ProviderRef
		row["kind"] = memorySearchKind(active.ProviderRef)
		row["label"] = memorySearchLabel(w, active.ProviderRef)
		var fleet bool
		indexer, fleet = IndexerFor(active)
		if indexer != "" {
			indexerName = w.displayName(ctx, indexer)
		}
		row["indexedBy"], row["indexedByName"] = indexer, indexerName
		row["chosenBy"] = strings.TrimSpace(active.ChosenBy)
		row["needsConfirmation"] = !fleet && strings.TrimSpace(active.ChosenBy) == ""
		row["previousRef"] = strings.TrimSpace(active.PreviousRef)
		row["takeOver"] = mayAct && memorySearchOffersTakeOver(ctx, w, caller, indexer, active.ProviderRef)
		activeTable, err := EmbeddingVectorTable(active.ProviderRef, active.Dimensions)
		if err != nil {
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
		if !bf.MissingAt.IsZero() {
			backlog = bf.Missing
			row["backlog"], row["backlogCountedAt"] = backlog, bf.MissingAt.UTC().Format(time.RFC3339Nano)
		}
		if bf.State == memoryIndexStateWaiting {
			row["waitingReason"] = bf.WaitingReason
		}
	}
	sw, _, hasSwitch, err := w.readRun(ctx, memoryIndexKindSwitch)
	if err != nil {
		return nil, err
	}
	if hasSwitch {
		startedAt := ""
		if !sw.StartedAt.IsZero() {
			startedAt = sw.StartedAt.UTC().Format(time.RFC3339Nano)
		}
		runIndexer, runIndexerName := strings.TrimSpace(sw.IndexedBy), ""
		if runIndexer == indexer {
			runIndexerName = indexerName // the binding's, already read
		} else if runIndexer != "" {
			runIndexerName = w.displayName(ctx, runIndexer)
		}
		takeOver := mayAct && !memorySearchSwitchOver(sw.State) && !(bound && memorySearchFlipped(sw, active)) &&
			memorySearchOffersTakeOver(ctx, w, caller, runIndexer, sw.TargetRef)
		row["switch"] = map[string]any{
			"state": sw.State, "targetRef": sw.TargetRef, "targetLabel": memorySearchLabel(w, sw.TargetRef),
			"counts": map[string]any{"total": sw.Counts.Total, "done": sw.Counts.Done, "skipped": sw.Counts.Skipped,
				"unsourced": sw.Counts.Unsourced, "failed": sw.Counts.Failed},
			"startedAt": startedAt, "startedBy": sw.StartedBy, "waitingReason": sw.WaitingReason,
			"estimatedTotal": indexed + backlog,
			"indexedBy":      runIndexer, "indexedByName": runIndexerName, "takeOver": takeOver,
		}
	}
	retired, err := w.retired(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(retired, func(i, j int) bool { return retired[i].RetireAt.Before(retired[j].RetireAt) })
	inUse := memorySearchTablesInUse(active, sw, hasSwitch)
	retained := []any{}
	now := w.now()
	for _, r := range retired {
		table, err := EmbeddingVectorTable(r.ProviderRef, r.Dimensions)
		// Past its date (the cleanup's own test, memoryIndexRetire) it is
		// about to be dropped: not offered as kept, though the daily cleanup
		// may not have run yet.
		if err != nil || !r.DroppedAt.IsZero() || r.RetireAt.IsZero() || !now.Before(r.RetireAt) || inUse[table] {
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
//
// A model of either kind whose declared width the index cannot hold
// (memorySearchTooWide) stays listed, as an unavailable model is, and is not
// available, its hint saying why: choosing it would be refused before anything
// is embedded, and offering it would invite a confirmation over a switch that
// can never start.
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
		// Each machine carries its own capabilities, and the model's flag is
		// their union (fleetcatalog.Project), so an embedding model has at
		// least one machine that embeds with it.
		machines, online := 0, 0
		for _, mm := range m.Machines {
			if mm.Capabilities.Embeddings {
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
		ref := FleetReferencePrefix + m.ModelId
		available := online > 0
		if tooWide := memorySearchWidthHint(w, ref); tooWide != "" {
			hint, available = tooWide, false
		}
		out = append(out, map[string]any{"providerRef": ref, "kind": "machine", "label": m.ModelId,
			"machines": machines, "paidByCluster": false, "available": available, "hint": hint})
	}
	for _, v := range w.vendorEmbedders() {
		label := strings.TrimSpace(v.Model)
		if label == "" {
			label = v.Name
		}
		hint, available := "Paid by the cluster's vendor account", true
		if tooWide := memorySearchWidthHint(w, v.Name); tooWide != "" {
			hint, available = tooWide, false
		}
		out = append(out, map[string]any{"providerRef": v.Name, "kind": "vendor", "label": label, "machines": 0,
			"paidByCluster": true, "available": available, "hint": hint})
	}
	return out, nil
}

// memorySearchWidthHint is a choice's hint when its declared width is more
// than the index holds (memorySearchTooWide), "" otherwise.
func memorySearchWidthHint(w memorySearchWorld, ref string) string {
	if dims := memorySearchDeclaredWidth(w, ref); dims > maxEmbeddingIndexDimensions {
		return fmt.Sprintf("Its vectors are %d wide, and the index holds at most %d", dims, maxEmbeddingIndexDimensions)
	}
	return ""
}
