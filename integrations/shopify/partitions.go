package shopify

import (
	"context"
	"fmt"
	"sort"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// partitions.go -- each store is a partition of the mirror.
//
// One connector serves every connected store, and the sync runtime keeps one
// health row per (concept, connector, direction): one backfill cursor, one
// reconcile watermark and one last error, shared by every store. One store's
// finished backfill read as every store's, one store's sweep moved every
// store's watermark, and one store's refusal was every store's error.
//
// Implementing memqlsync.Partitioned ends that. The partition is the store id,
// and the runtime then backfills and sweeps one store at a time, each from a
// row of its own, and never calls the shared Backfill or Reconcile. Every
// mirror write carries its store as its partition (mapObject, tombstone), so a
// delivery moves the row of the store it came from.

// Partitions lists the stores that ingest, by id, sorted. A store that does
// not ingest -- paused, uninstalled -- is left out: it is not swept, and its
// rows keep the state they had, so a resumed store continues from where it
// stopped.
func (c *Connector) Partitions(ctx context.Context) ([]string, error) {
	stores, err := c.stores.Stores(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(stores))
	for _, store := range stores {
		if store.Ingests() {
			out = append(out, store.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// BackfillPartition advances one store's backfill of one domain by one step,
// from that store's own cursor.
func (c *Connector) BackfillPartition(ctx context.Context, partition, conceptName, cursor string) (memqlsync.BackfillPage, error) {
	store, err := c.ingestingStore(ctx, partition)
	if err != nil {
		return memqlsync.BackfillPage{}, err
	}
	return c.BackfillStore(ctx, store, conceptName, cursor)
}

// ReconcilePartition sweeps one store's domain from that store's own
// watermark, and answers for that store alone.
func (c *Connector) ReconcilePartition(ctx context.Context, partition, conceptName string, since time.Time) (memqlsync.ReconcileReport, error) {
	store, err := c.ingestingStore(ctx, partition)
	if err != nil {
		return memqlsync.ReconcileReport{}, err
	}
	spec, err := reconcileSpec(conceptName)
	if err != nil || spec == nil {
		return memqlsync.ReconcileReport{}, err
	}
	report, missing, err := c.sweepStore(ctx, store, spec, since)
	if len(missing) > 0 {
		return report, memqlsync.NotGranted(ConnectorName, uniqueSorted(missing)...)
	}
	return report, err
}

// ingestingStore resolves a partition to its store, refusing one that is not
// a store this connector ingests: the runtime only asks for partitions it was
// given, so a store that has stopped ingesting since is named, not swept.
func (c *Connector) ingestingStore(ctx context.Context, partition string) (Store, error) {
	stores, err := c.stores.Stores(ctx)
	if err != nil {
		return Store{}, err
	}
	for _, store := range stores {
		if store.ID != partition {
			continue
		}
		if !store.Ingests() {
			return Store{}, fmt.Errorf("shopify: store %q does not ingest, so it is not a partition of the mirror", partition)
		}
		return store, nil
	}
	return Store{}, fmt.Errorf("shopify: store %q is not a connected store, so it is not a partition of the mirror", partition)
}

var _ memqlsync.Partitioned = (*Connector)(nil)
