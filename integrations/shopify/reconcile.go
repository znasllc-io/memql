package shopify

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// reconcile.go -- catching what live delivery lost.
//
// Shopify states plainly that webhook delivery is not guaranteed. That single
// sentence is why this file exists and why it is not a backstop: a mirror
// whose only input is webhooks is a mirror that is wrong by an unknown amount
// for an unknown length of time, and nothing in it says so.
//
// Two modes, and which one a domain gets is a property of the API rather than
// a preference:
//
//   - updated_at -- the root connection accepts a `query:` filter, so ask for
//     what changed since the watermark. Cheap, and can run often.
//   - full re-list -- the domain publishes no change signal at all (gift
//     cards, price lists, menus, pages, policies, payouts...). Walk the whole
//     thing on a cadence and tombstone what is no longer there. Expensive, so
//     the cadence is per domain and lives in the allowlist.

const (
	// reconcilePageSize is the page for a `query:`-filtered walk. 100 keeps
	// a single query well under the 1,000-point cost ceiling with the
	// nested selections a mirrored type carries.
	reconcilePageSize = 100
	// relistPageCap bounds a full re-list. Past it the pass reports what it
	// found but does NOT tombstone -- see the note in relist.
	relistPageCap = 200
)

// Reconcile implements sync.Connector: sweep one domain across every
// ingesting store, healing as it goes.
//
// The contract's report carries COUNTS rather than writes, so the sweep
// performs its own writes -- see mirror.go for why that is a local write
// rather than the runtime's. `since` is the previous sweep's time, which
// an updated_at domain narrows on and a re-list ignores.
func (c *Connector) Reconcile(ctx context.Context, conceptID string, since time.Time) (memqlsync.ReconcileReport, error) {
	spec := generated.Types[generated.ConceptFromID(conceptID)]
	if spec == nil {
		return memqlsync.ReconcileReport{}, memqlsync.NotImplemented(ConnectorName, "Reconcile of "+conceptID)
	}
	if spec.Reconcile == generated.ReconcileNone {
		// A child materialised with its parent, or a singleton. Not a
		// failure and not a not-implemented: there is genuinely nothing
		// to sweep, and the runtime records a clean pass.
		return memqlsync.ReconcileReport{}, nil
	}
	stores, err := c.stores.Stores(ctx)
	if err != nil {
		return memqlsync.ReconcileReport{}, err
	}
	var report memqlsync.ReconcileReport
	for _, store := range stores {
		if !store.Ingests() {
			continue
		}
		one, err := c.reconcileStore(ctx, store, spec, since)
		if err != nil {
			return report, err
		}
		report.Checked += one.Checked
		report.Drifted += one.Drifted
		report.Healed += one.Healed
	}
	return report, nil
}

// reconcileStore sweeps one store's domain.
func (c *Connector) reconcileStore(ctx context.Context, store Store, spec *generated.TypeSpec, since time.Time) (memqlsync.ReconcileReport, error) {
	switch spec.Reconcile {
	case generated.ReconcileUpdatedAt:
		return c.reconcileByUpdatedAt(ctx, store, spec, since)
	case generated.ReconcileFullRelist:
		return c.relist(ctx, store, spec)
	default:
		return memqlsync.ReconcileReport{}, nil
	}
}

// reconcileByUpdatedAt pages the domain for rows the origin changed since
// the last sweep and re-applies them.
//
// Tombstoning is NOT part of this mode, and that is not an oversight: an
// `updated_at:>` filter cannot express "and tell me what you deleted". A
// deletion is carried by the delete topic and, when that is lost, by the
// domain's periodic full re-list -- which is why every updated_at domain
// that can be deleted also carries delete topics.
func (c *Connector) reconcileByUpdatedAt(ctx context.Context, store Store, spec *generated.TypeSpec, since time.Time) (memqlsync.ReconcileReport, error) {
	var report memqlsync.ReconcileReport
	if spec.ListQuery == "" || spec.ListOp == "" {
		return report, nil
	}
	filter := ""
	if !since.IsZero() {
		filter = "updated_at:>'" + since.UTC().Format(time.RFC3339) + "'"
	}
	after := ""
	for page := 0; page < relistPageCap; page++ {
		vars := map[string]any{"first": reconcilePageSize}
		if after != "" {
			vars["after"] = after
		}
		if filter != "" {
			vars["query"] = filter
		}
		nodes, next, hasNext, err := c.listPage(ctx, store, spec, vars)
		if err != nil {
			return report, err
		}
		for _, obj := range nodes {
			report.Checked++
			writes := mapObject(spec, store.ID, obj, "", c.now())
			healed, _ := c.heal(ctx, writes)
			if healed > 0 {
				report.Drifted++
				report.Healed++
			}
		}
		if !hasNext || next == "" {
			return report, nil
		}
		after = next
	}
	c.logger.Warn("shopify: updated_at sweep hit the page cap",
		"store", store.ID, "concept", spec.Concept, "pages", relistPageCap)
	return report, nil
}

// relist walks a whole domain and tombstones what the origin no longer
// has.
//
// THE CAP IS LOAD-BEARING. Tombstoning is an argument from ABSENCE: a
// mirror row is retired because the origin did not mention it. That
// argument is only valid if the walk was complete, so a pass that hits
// the page cap heals what it saw and tombstones NOTHING. Tombstoning on a
// partial walk would retire the tail of a large domain on every pass and
// re-create it on the next -- churn that looks like activity.
func (c *Connector) relist(ctx context.Context, store Store, spec *generated.TypeSpec) (memqlsync.ReconcileReport, error) {
	var report memqlsync.ReconcileReport
	if spec.ListQuery == "" || spec.ListOp == "" {
		return report, nil
	}
	seen := map[string]bool{}
	after := ""
	complete := false
	for page := 0; page < relistPageCap; page++ {
		vars := map[string]any{"first": reconcilePageSize}
		if after != "" {
			vars["after"] = after
		}
		nodes, next, hasNext, err := c.listPage(ctx, store, spec, vars)
		if err != nil {
			return report, err
		}
		for _, obj := range nodes {
			report.Checked++
			if gid, ok := obj["id"].(string); ok {
				seen[gid] = true
			}
			healed, _ := c.heal(ctx, mapObject(spec, store.ID, obj, "", c.now()))
			if healed > 0 {
				report.Drifted++
				report.Healed++
			}
		}
		if !hasNext || next == "" {
			complete = true
			break
		}
		after = next
	}
	if !complete {
		c.logger.Warn("shopify: full re-list hit the page cap; not tombstoning",
			"store", store.ID, "concept", spec.Concept, "pages", relistPageCap)
		return report, nil
	}

	absent, err := c.liveRowsAbsentFrom(ctx, store, spec, seen)
	if err != nil {
		return report, err
	}
	for _, gid := range absent {
		if err := c.writeMirror(ctx, tombstone(spec.Concept, store.ID, gid, c.now())); err != nil {
			c.logger.Warn("shopify: could not tombstone an absent row",
				"store", store.ID, "concept", spec.Concept, "gid", gid, "error", err)
			continue
		}
		report.Drifted++
		report.Healed++
	}
	return report, nil
}

// liveRowsAbsentFrom lists the mirror's live GIDs for a domain and returns
// the ones the origin walk did not produce.
func (c *Connector) liveRowsAbsentFrom(ctx context.Context, store Store, spec *generated.TypeSpec, seen map[string]bool) ([]string, error) {
	var absent []string
	res, err := c.engine.Execute(connectorContext(ctx), renderCall(spec.ForStoreFn, map[string]any{"storeId": store.ID}))
	if err != nil {
		return nil, fmt.Errorf("shopify: list mirrored %s: %w", spec.Concept, err)
	}
	for _, row := range memql.MaterializeRows(res) {
		gid := mapString(row, "gid")
		if gid == "" || seen[gid] {
			continue
		}
		absent = append(absent, gid)
	}
	return absent, nil
}

// listPage runs one page of the generated list operation.
func (c *Connector) listPage(ctx context.Context, store Store, spec *generated.TypeSpec, vars map[string]any) ([]map[string]any, string, bool, error) {
	resp, err := c.adminCall(ctx, store, spec.FetchDocument, spec.ListOp, vars)
	if err != nil {
		return nil, "", false, err
	}
	data, err := resp.DataMap()
	if err != nil {
		return nil, "", false, err
	}
	conn, ok := data[spec.ListQuery].(map[string]any)
	if !ok {
		return nil, "", false, fmt.Errorf("shopify: %s response carried no %s connection", spec.ListOp, spec.ListQuery)
	}
	nodes := objectsOf(conn["nodes"])
	info, _ := conn["pageInfo"].(map[string]any)
	hasNext, _ := info["hasNextPage"].(bool)
	end, _ := info["endCursor"].(string)
	return nodes, end, hasNext, nil
}

var _ = strings.TrimSpace
