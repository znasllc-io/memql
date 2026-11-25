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

// memorySearchOffersTakeOver is the status's `takeOver` (ruling R13): whether
// caller, choosing the model in use again, would become its indexer --
// memorySearchSwitch's own path to memorySearchKeep, asked without writing. A
// machine model is indexed only by an active owner, so a caller who is not one
// takes over nothing; the cluster asking (no caller) takes over nothing either.
//
// A catalog or an owner check that cannot be read answers false, logged, rather
// than failing the status: the screen then withholds the act for that read and
// still shows what is in use, and the next read asks again. An act offered on a
// guess would be one the engine might answer by doing nothing.
func memorySearchOffersTakeOver(ctx context.Context, w memorySearchWorld, caller string, active EmbedderBinding) bool {
	indexer, fleet := IndexerFor(active)
	if !fleet || strings.TrimSpace(caller) == "" {
		return false
	}
	owner, err := w.isActiveOwner(ctx, caller)
	takeOver := false
	if err == nil && owner {
		takeOver, err = memorySearchTakesOver(ctx, w, caller, indexer, active.ProviderRef)
	}
	if err != nil {
		w.logger().Warn("memory search: could not tell whether choosing the model in use again would take over its indexing", "provider", active.ProviderRef, "error", err)
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
//   - takeOver: whether caller, choosing the model in use again, would become
//     its indexer (memorySearchOffersTakeOver; ruling R13). It is how the status
//     says the binding's indexer cannot index the model, asked from the caller's
//     side so that the screen offers the act only where it changes something.
//   - indexed: the entries in its vector table.
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
//     cost, and it counts entries no source rebuilds.
//   - retained: the retired indexes still kept, {providerRef, retireAt}, by
//     date. A row naming the active model or a running switch's target is not
//     one: that table is in use.
func memorySearchStatus(ctx context.Context, w memorySearchWorld, caller string) (map[string]any, error) {
	row := map[string]any{"providerRef": "", "kind": "", "label": "", "indexedBy": "", "indexedByName": "", "chosenBy": "",
		"needsConfirmation": false, "previousRef": "", "takeOver": false, "indexed": 0, "backlog": 0, "backlogCountedAt": "",
		"waitingReason": "", "switch": nil, "retained": []any{}}
	active, err := w.binding(ctx)
	if err != nil && !errors.Is(err, ErrNoEmbedderBound) {
		return nil, err
	}
	bound := err == nil && active.Valid()
	indexed, backlog := 0, 0
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
		row["previousRef"] = strings.TrimSpace(active.PreviousRef)
		row["takeOver"] = memorySearchOffersTakeOver(ctx, w, caller, active)
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
	inUse := memorySearchTablesInUse(active, sw, hasSwitch)
	retained := []any{}
	for _, r := range retired {
		table, err := EmbeddingVectorTable(r.ProviderRef, r.Dimensions)
		if err != nil || !r.DroppedAt.IsZero() || r.RetireAt.IsZero() || inUse[table] {
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
