package sync

import (
	"context"
	"time"
)

// partitioned.go -- a connector that serves many tenants.
//
// The runtime keeps one health row per (concept, connector, direction): the
// backfill cursor, the reconcile watermark and the last error. A connector
// that serves several tenants through one name -- Shopify's serves every
// connected store -- would share all three across them: one store's cursor
// resumes another's backfill, one store's sweep moves every store's
// watermark, and one store's refusal reads as every store's.
//
// Partitioned is the optional half of the contract that ends that. A
// connector implementing it names its tenants, one opaque partition string
// each, and is backfilled and reconciled one partition at a time, each with a
// health row of its own. The runtime then NEVER calls such a connector's
// shared Backfill or Reconcile: a call naming no partition fans out over
// Partitions instead. A connector with one tenant does not implement it, and
// keeps the single row it always had.
type Partitioned interface {
	// Partitions lists the tenants this connector currently serves. A
	// tenant it leaves out is not swept, and its row keeps its state.
	Partitions(ctx context.Context) ([]string, error)

	// BackfillPartition is Connector.Backfill for one tenant, resuming
	// from that tenant's own cursor.
	BackfillPartition(ctx context.Context, partition, conceptName, cursor string) (BackfillPage, error)

	// ReconcilePartition is Connector.Reconcile for one tenant, from that
	// tenant's own watermark.
	ReconcilePartition(ctx context.Context, partition, conceptName string, since time.Time) (ReconcileReport, error)
}
