package sitepreview

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// order.go -- the FOURTH observation, and the only passive one (issue
// memql#5547).
//
// The other three are the engine ASKING the development store a question. This
// one is the engine NOTICING that an order arrived in the mirror under that
// store's id, which is the last step of the walk the design record describes
// and the one that proves a test payment reached MemQL. Nothing here makes it
// happen: the connector mirrors orders because that is its job, and this
// watches.
//
// # Why the automation fires on every order and this is still cheap
//
// `@trigger(event="node.created", concept="v1:shopify:order")` fires for every
// mirrored order on every store, which during a backfill is thousands. A read
// per order would put the graph under the connector's own throughput.
//
// So the FIRST thing this does is a map lookup against a set of store ids with
// a preview open, refreshed at most once per previewStoreTTL. A cluster with no
// preview open -- which is every cluster almost all of the time -- pays one
// query every thirty seconds and nothing per order. The set is per replica and
// a preview opened on a sibling becomes visible within that window, which is
// fine for what it measures: an order arrives minutes after a preview opens,
// not milliseconds.
//
// # Why an order is attributed to EVERY open preview of its store
//
// Two operators can have previews open against one development store, and
// nothing in an order says which of them placed it -- Shopify has no idea MemQL
// has previews. Attributing it to one would be a guess presented as a fact, and
// attributing it to none would lose the observation that matters most. So each
// open preview of that store records that an order arrived, which is exactly
// what each of them can honestly say.

// previewStoreTTL bounds how stale the open-preview store set may be.
const previewStoreTTL = 30 * time.Second

// openPreviewStores is the per-replica cache behind the cheap path.
type openPreviewStores struct {
	mu   sync.RWMutex
	ids  map[string][]Grant
	at   time.Time
	seen map[string]struct{}
}

func newOpenPreviewStores() *openPreviewStores {
	return &openPreviewStores{ids: map[string][]Grant{}, seen: map[string]struct{}{}}
}

// handleNoteOrder records the fourth observation for every preview open against
// the store this order was mirrored under.
//
// IT ANSWERS QUIETLY WHEN THERE IS NOTHING TO SAY. An order on a store nobody
// is previewing is the overwhelmingly common case and it is not a failure, so
// the reply is a count rather than an error -- an automation that logged a
// refusal per mirrored order would make a backfill unreadable.
func (i *Integration) handleNoteOrder(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := memql.BareShortId(strings.TrimSpace(asString(args["storeId"])))
	orderID := memql.BareShortId(strings.TrimSpace(asString(args["orderId"])))
	if storeID == "" {
		return i.node("order:none", map[string]any{"recorded": 0, "reason": "the order names no store"})
	}

	grants, err := i.previewsAgainst(ctx, storeID)
	if err != nil {
		return nil, err
	}
	if len(grants) == 0 {
		return i.node("order:"+storeID, map[string]any{"recorded": 0, "reason": "no preview is open against this store"})
	}

	// ONCE PER ORDER PER PREVIEW. The automation can fire again for the same
	// row -- a webhook redelivery, a reconcile, a re-apply -- and a second row
	// would read as a second order on a screen whose whole job is to say
	// whether one arrived.
	observedAt := time.Now().UTC()
	recorded := 0
	for _, grant := range grants {
		key := grant.ID + "/" + orderID
		if !i.orders.claim(key) {
			continue
		}
		detail := "an order reached the mirror"
		if orderID != "" {
			detail = "order " + orderID + " reached the mirror"
		}
		// UNDER THE GRANT'S OWNER, not under whoever's actor the automation
		// carries: the observation belongs beside the other three of that
		// exercise, and the automation's system principal owns nothing a
		// person can read. auth.ContextWithUserActor is the seam, and the
		// @serverOnly write stamps ownerUserId from the actor it is given.
		as := auth.ContextWithUserActor(ctx, grant.OwnerUserID)
		obs := Observation{
			ID:          id.NewShortId(),
			GrantID:     grant.ID,
			SiteID:      grant.SiteID,
			OwnerUserID: grant.OwnerUserID,
			StoreID:     storeID,
			Kind:        ObservationOrderMirrored,
			ObservedAt:  observedAt,
			OK:          true,
			Detail:      detail,
			// ZERO, AND HONESTLY SO. Nothing timed the order's journey; a
			// duration invented for it would be a measurement of the mirror's
			// lag dressed as the storefront's.
			DurationMs: 0,
		}
		if err := i.store.RecordObservation(as, obs); err != nil {
			return nil, err
		}
		recorded++
	}
	return i.node("order:"+storeID, map[string]any{"recorded": recorded, "storeId": storeID, "orderId": orderID})
}

// previewsAgainst returns the previews open against one store, from the cache.
func (i *Integration) previewsAgainst(ctx context.Context, storeID string) ([]Grant, error) {
	i.orders.mu.RLock()
	fresh := time.Since(i.orders.at) < previewStoreTTL
	grants := i.orders.ids[storeID]
	i.orders.mu.RUnlock()
	if fresh {
		return grants, nil
	}
	refreshed, err := i.refreshOpenPreviews(ctx)
	if err != nil {
		return nil, err
	}
	return refreshed[storeID], nil
}

// refreshOpenPreviews re-reads every unexpired, unrevoked grant and indexes it
// by its preview store.
//
// UNDER THE DEPLOYMENT'S OWN OPERATOR IDENTITY, which is the one read in this
// package that is not the caller's -- and it has to be: the caller here is an
// automation's system principal watching a connector's write, and the question
// "is anybody previewing against this store" is the cluster's rather than any
// person's. The rows it finds are used only to decide where an observation
// belongs; nothing about them is returned to anybody.
func (i *Integration) refreshOpenPreviews(ctx context.Context) (map[string][]Grant, error) {
	rows, err := i.store.rows(systemPreviewContext(ctx), "query sitePreviewGrantsOpen()")
	if err != nil {
		return nil, fmt.Errorf("sitepreview: read the open previews: %w", err)
	}
	now := time.Now().UTC()
	next := map[string][]Grant{}
	for _, r := range rows {
		g := grantFromRow(r)
		if strings.TrimSpace(g.RevokedAt) != "" {
			continue
		}
		if g.ExpiresAt.IsZero() || !now.Before(g.ExpiresAt) {
			continue
		}
		store := strings.TrimSpace(g.PreviewStoreID)
		if store == "" {
			continue
		}
		next[store] = append(next[store], g)
	}
	i.orders.mu.Lock()
	i.orders.ids = next
	i.orders.at = time.Now()
	i.orders.mu.Unlock()
	return next, nil
}

// claim reports whether this (preview, order) pair has not been recorded on
// this replica yet, and marks it.
//
// PER REPLICA, which is the honest bound rather than a guarantee: two replicas
// that both see one order's event would each record once. The engine's own
// event routing is what makes that rare, and the alternative -- a read before
// every write -- would put a query back on the connector's throughput, which is
// the cost this whole file is arranged to avoid.
func (o *openPreviewStores) claim(key string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, taken := o.seen[key]; taken {
		return false
	}
	// A BOUND, because the key set grows with every order a previewed store
	// takes. Previews are short and few; ten thousand pairs is far past any
	// real run, and dropping the map costs at most a duplicate observation on
	// a preview somebody has left open for a very long time.
	if len(o.seen) > 10000 {
		o.seen = map[string]struct{}{}
	}
	o.seen[key] = struct{}{}
	return true
}

// systemPreviewContext stamps the deployment's own operator identity for the
// one clusterOwner-reaching read this package makes on nobody's behalf.
//
// It sets the same three surfaces auth.ContextWithUserActor sets for a real
// user -- claims, TokenInfo, AccessContext -- because createdBy and actor.userId
// read different ones, and RoleOwner rather than RoleWriter because
// AccessContext.IsClusterOwner() reads Role == RoleOwner, which is the bit the
// grant read's own conjunct checks.
func systemPreviewContext(ctx context.Context) context.Context {
	claims := map[string]any{"sub": systemPreviewActor, "role": "owner"}
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: systemPreviewActor,
		Role:   auth.RoleOwner,
	})
}

// systemPreviewActor is the engine's own identity for that one read. Synthetic,
// and scoped by what it is used for: one named query over one concept.
const systemPreviewActor = "system:sitePreview"
