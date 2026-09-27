package shopify

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	// relistRowCap bounds a full re-list, in ROWS: the page a walk uses
	// shrinks when the cost ceiling says so, and a cap counted in pages
	// would shrink the tombstoning budget with it. Past the cap the pass
	// reports what it found but does NOT tombstone -- see the note in
	// relist.
	relistRowCap = 200 * reconcilePageSize
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
	var swept int
	var missingScopes []string
	var refused error
	for _, store := range stores {
		if !store.Ingests() {
			continue
		}
		if missing := scopesMissingFor(store, spec); len(missing) > 0 {
			// The store was connected without any scope this domain reads
			// under. Asking would get ACCESS_DENIED, as it did once per
			// tick for 22 domains on one store; the answer is known here.
			missingScopes = append(missingScopes, missing...)
			continue
		}
		one, err := c.reconcileStore(ctx, store, spec, since)
		if memqlsync.IsNotGranted(err) {
			// The origin said what the recorded grant could not: this
			// store does not have the domain. The other stores still get
			// their sweep.
			refused = err
			continue
		}
		if err != nil {
			return report, err
		}
		swept++
		report.Checked += one.Checked
		report.Drifted += one.Drifted
		report.Healed += one.Healed
	}
	if swept == 0 {
		if len(missingScopes) > 0 {
			return report, memqlsync.NotGranted(ConnectorName, uniqueSorted(missingScopes)...)
		}
		if refused != nil {
			return report, refused
		}
	}
	return report, nil
}

// scopesMissingFor is the domain's scopes the store holds NONE of.
//
// The rule is "none", not "any missing", because the allowlist's scope list
// mixes the scope a domain needs with the scopes that widen it: orders reads
// under read_orders and read_all_orders, and a store with only the first
// still lists its last sixty days. A domain the store holds no scope for is
// certainly refused; one it holds some scope for is the origin's to answer,
// field by field -- which listPage handles when it does.
//
// A store whose grant was never recorded (an older row) reports nothing
// missing: the origin decides, and a whole-connection denial still lands as
// not-granted through listPage.
func scopesMissingFor(store Store, spec *generated.TypeSpec) []string {
	if spec == nil || len(spec.Scopes) == 0 || len(store.ScopesGranted) == 0 {
		return nil
	}
	granted := grantIncludes(store.ScopesGranted)
	for _, s := range spec.Scopes {
		if granted[s] {
			return nil
		}
	}
	return append([]string(nil), spec.Scopes...)
}

// grantIncludes is the set of scopes a recorded grant reaches. A write
// scope implies its read, as Shopify reports and its own client reads
// them: a store granted write_products lists products.
func grantIncludes(granted []string) map[string]bool {
	have := map[string]bool{}
	for _, s := range granted {
		have[s] = true
		if i := strings.Index(s, "write_"); i >= 0 {
			have[s[:i]+"read_"+s[i+len("write_"):]] = true
		}
	}
	return have
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
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
	if !since.IsZero() && spec.ListFilterable {
		// Only a connection that takes `query:` gets the watermark. The
		// generated list operation declares the variable only for those,
		// and a variable an operation does not declare is a validation
		// error before the origin reads anything else.
		filter = "updated_at:>'" + since.UTC().Format(time.RFC3339) + "'"
	}
	after := ""
	for rows := 0; rows < relistRowCap; {
		vars := map[string]any{"first": c.pageSizeFor(store, spec)}
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
		rows += len(nodes)
		if len(nodes) == 0 && hasNext {
			// A page with nothing on it and more promised: the origin is
			// not advancing, and neither should this loop.
			break
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
	c.logger.Warn("shopify: updated_at sweep hit the row budget",
		"store", store.ID, "concept", spec.Concept, "rows", relistRowCap)
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
	for rows := 0; rows < relistRowCap; {
		vars := map[string]any{"first": c.pageSizeFor(store, spec)}
		if after != "" {
			vars["after"] = after
		}
		nodes, next, hasNext, err := c.listPage(ctx, store, spec, vars)
		if err != nil {
			return report, err
		}
		rows += len(nodes)
		if len(nodes) == 0 && hasNext {
			break
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
			"store", store.ID, "concept", spec.Concept, "rows", relistRowCap)
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

// listPage fetches one page of a domain, and decides what a refusal means.
//
// Three refusals reached a live cluster on every ten-minute tick, and none
// of them was something the next tick could change:
//
//   - MAX_COST_EXCEEDED: the page times the nested selection costs more than
//     the 1,000-point ceiling. The message says by how much, so the page is
//     shrunk to fit and remembered per (store, domain); the next sweep
//     starts small instead of paying for the refusal again. A page of one
//     that still exceeds the ceiling is the selection itself, and permanent.
//   - ACCESS_DENIED on PART of the selection (a variant's publication counts
//     under a grant without read_publications): callWithGrant removes the
//     denied fields from the document and asks again, so the rows are
//     mirrored without them. A denial of the root connection is the domain
//     not granted.
//   - anything else the client says is not retryable (a query the origin
//     rejects outright): permanent, so the runtime reports it and holds the
//     domain to its cadence rather than its tick.
func (c *Connector) listPage(ctx context.Context, store Store, spec *generated.TypeSpec, vars map[string]any) ([]map[string]any, string, bool, error) {
	for shrinks := 0; ; shrinks++ {
		resp, err := c.callWithGrant(ctx, store, spec, spec.ListOp, vars)
		if err == nil {
			return pageOf(resp, spec)
		}
		if memqlsync.IsNotGranted(err) || memqlsync.IsPermanent(err) {
			return nil, "", false, err
		}
		var ae *AdminError
		if !errors.As(err, &ae) || ae.Retryable() || !ae.Standing() {
			// A network failure, a throttle, or a 200 carrying a code that
			// names the moment rather than the document: the next tick
			// may well succeed, so it is reported and retried.
			return nil, "", false, err
		}
		if cost, limit, exceeded := ae.CostExceeded(); exceeded {
			first, _ := vars["first"].(int)
			next := shrinkPage(first, cost, limit)
			if next < first && shrinks < maxPageShrinks {
				vars["first"] = next
				c.pageSizes.Store(pageSizeKey(store, spec), next)
				c.logger.Info("shopify: page shrunk to fit the query cost ceiling",
					"store", store.ID, "concept", spec.Concept, "from", first, "to", next, "cost", cost, "limit", limit)
				continue
			}
			return nil, "", false, memqlsync.Permanent(err)
		}
		return nil, "", false, memqlsync.Permanent(err)
	}
}

// pageOf reads the connection a list reply carries.
func pageOf(resp *AdminResponse, spec *generated.TypeSpec) ([]map[string]any, string, bool, error) {
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

const (
	// minReconcilePage is the smallest page worth asking for. One: an order
	// with its line items and fulfilments can cost a quarter of the ceiling
	// on its own, and a floor of five refused every order sweep on a store
	// with ordinary orders. Below one the selection itself is over the
	// ceiling and no page size helps.
	minReconcilePage = 1
	// maxPageShrinks bounds the retries one page fetch spends finding a
	// size that fits; the shrink is geometric, so eight reaches one from
	// any page the generator emits.
	maxPageShrinks = 8
)

// shrinkPage picks the page that fits the ceiling the origin reported,
// with a margin, never smaller than minReconcilePage and always smaller
// than what was refused (so the loop moves even when the numbers are odd).
func shrinkPage(first, cost, limit int) int {
	if first <= 0 {
		first = reconcilePageSize
	}
	next := first / 2
	if cost > 0 && limit > 0 {
		next = int(float64(first) * float64(limit) / float64(cost) * 0.8)
	}
	if next >= first {
		next = first / 2
	}
	if next < minReconcilePage {
		next = minReconcilePage
	}
	return next
}

func pageSizeKey(store Store, spec *generated.TypeSpec) string {
	return store.ID + "|" + spec.Concept
}

// pageSizeFor is the page a domain's walk starts at: the default, or the
// size a previous cost refusal taught.
func (c *Connector) pageSizeFor(store Store, spec *generated.TypeSpec) int {
	if v, ok := c.pageSizes.Load(pageSizeKey(store, spec)); ok {
		if n, ok := v.(int); ok && n > 0 {
			return n
		}
	}
	return reconcilePageSize
}
