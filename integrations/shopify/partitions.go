package shopify

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
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
//
// A domain Shopify will not export for this store is answered the way the
// sweep answers it (sweepStore), so the runtime records a standing answer
// rather than a failure it would retry: outside the store's connection, not
// requested; inside it with none of its scopes granted, not granted, without
// asking Shopify; refused by Shopify all the same, not granted when the
// refusal is one a person can fix and not requested otherwise.
func (c *Connector) BackfillPartition(ctx context.Context, partition, conceptName, cursor string) (memqlsync.BackfillPage, error) {
	store, err := c.ingestingStore(ctx, partition)
	if err != nil {
		return memqlsync.BackfillPage{}, err
	}
	if spec := generated.Types[generated.ConceptFromID(conceptName)]; bulkDomain(spec) {
		if !domainInConnection(store, spec) {
			return memqlsync.BackfillPage{}, memqlsync.NotRequested(ConnectorName)
		}
		if len(scopesMissingFor(store, spec)) > 0 {
			// Refused from the grant read fresh, never the cached one alone
			// (freshGrant).
			fresh, err := c.freshGrant(ctx, store, spec)
			if err != nil {
				return memqlsync.BackfillPage{}, err
			}
			if len(scopesMissingFor(fresh, spec)) > 0 {
				return memqlsync.BackfillPage{}, memqlsync.NotGranted(ConnectorName, uniqueSorted(askedOf(fresh, spec.Scopes))...)
			}
			store = fresh
		}
	}
	page, err := c.BackfillStore(ctx, store, conceptName, cursor)
	return page, backfillRefusal(store, err)
}

// backfillRefusal classifies Shopify's refusal of a bulk export. Shopify
// refuses a selection the grant does not reach either as an ACCESS_DENIED
// error or as a userError of bulkOperationRunQuery; both say "Access denied"
// and name the scope. Any other error is returned as it is.
func backfillRefusal(store Store, err error) error {
	if err == nil || memqlsync.IsNotGranted(err) || memqlsync.IsNotRequested(err) {
		return err
	}
	var ae *AdminError
	var ue *UserError
	denied := errors.As(err, &ae) && ae.DeniedOnly()
	if !denied && errors.As(err, &ue) {
		msg := strings.ToLower(ue.Error())
		denied = strings.Contains(msg, "access denied") || protectedDataRefusal(ue.Error())
	}
	if !denied {
		return err
	}
	if refusalIsFixable(store, err.Error()) {
		return fmt.Errorf("%w: %v", memqlsync.ErrNotGranted, err)
	}
	return memqlsync.NotRequested(ConnectorName)
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

// ingestingStore resolves a partition to its store. The runtime only asks for
// partitions it was given, so a store that does not ingest here has stopped
// since the pass listed it: somebody paused it, or it was uninstalled.
//
// THAT IS A WAIT, NOT A FAILURE (memqlsync.Busy): the store cannot be asked
// now, and nothing is wrong with any of its domains. The runtime leaves a busy
// backfill's row as it is -- queued, or running at its cursor -- and skips a
// busy sweep, so a store resumed later continues from where it stopped. As a
// plain error the runtime recorded the backfill `failed`, which nothing
// retries: a pause that landed while a pass ran ended it for good. A store
// that is not connected at all is still an error.
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
			return Store{}, memqlsync.Busy(ConnectorName, fmt.Sprintf("store %q does not ingest now, so its mirror waits for it", partition))
		}
		return store, nil
	}
	return Store{}, fmt.Errorf("shopify: store %q is not a connected store, so it is not a partition of the mirror", partition)
}

var _ memqlsync.Partitioned = (*Connector)(nil)
