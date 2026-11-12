package shopify

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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
// The runtime never calls it: the connector is partitioned, and a sweep runs
// one store at a time through ReconcilePartition. It stays for the contract,
// as a loop over Partitions that answers for the stores together.
//
// The contract's report carries COUNTS rather than writes, so the sweep
// performs its own writes -- see mirror.go for why that is a local write
// rather than the runtime's. `since` is the previous sweep's time, which
// an updated_at domain narrows on and a re-list ignores.
func (c *Connector) Reconcile(ctx context.Context, conceptID string, since time.Time) (memqlsync.ReconcileReport, error) {
	spec, err := reconcileSpec(conceptID)
	if err != nil || spec == nil {
		return memqlsync.ReconcileReport{}, err
	}
	parts, err := c.Partitions(ctx)
	if err != nil {
		return memqlsync.ReconcileReport{}, err
	}
	var report memqlsync.ReconcileReport
	var swept int
	var missingScopes []string
	var refused, notRequested error
	for _, part := range parts {
		store, err := c.ingestingStore(ctx, part)
		if err != nil {
			// Stopped ingesting since it was listed: not swept.
			continue
		}
		one, missing, err := c.sweepStore(ctx, store, spec, since)
		switch {
		case len(missing) > 0:
			missingScopes = append(missingScopes, missing...)
			continue
		case memqlsync.IsNotRequested(err):
			notRequested = err
			continue
		case memqlsync.IsNotGranted(err):
			// This store does not have the domain. The other stores still
			// get their sweep.
			refused = err
			continue
		case err != nil:
			return report, err
		}
		swept++
		report.Checked += one.Checked
		report.Drifted += one.Drifted
		report.Healed += one.Healed
	}
	if swept == 0 {
		// Something a person can fix is said before what nobody asked for.
		if len(missingScopes) > 0 {
			return report, memqlsync.NotGranted(ConnectorName, uniqueSorted(missingScopes)...)
		}
		if refused != nil {
			return report, refused
		}
		if notRequested != nil {
			return report, notRequested
		}
	}
	return report, nil
}

// reconcileSpec is the mirrored type a sweep of conceptID walks. A concept
// this build does not mirror is not implemented; one with nothing to sweep --
// a child materialised with its parent, or a singleton -- is nil with no
// error, which the runtime records as a clean pass.
func reconcileSpec(conceptID string) (*generated.TypeSpec, error) {
	spec := generated.Types[generated.ConceptFromID(conceptID)]
	if spec == nil {
		return nil, memqlsync.NotImplemented(ConnectorName, "Reconcile of "+conceptID)
	}
	if spec.Reconcile == generated.ReconcileNone {
		return nil, nil
	}
	return spec, nil
}

// sweepStore is one store's sweep of one domain, in the order its answer is
// decided:
//
//  1. The store's connection never asks for any scope the domain reads
//     under: nothing to grant and nothing to ask, so not requested.
//  2. The store was connected without any scope the domain reads under:
//     asking would get ACCESS_DENIED, as it did once per tick for 22 domains
//     on one store, so the origin is not asked. The scopes the connection
//     asks for -- the ones a person can approve -- come back as missing.
//  3. The sweep itself. The origin's root refusal stays not granted when a
//     person can fix it, and is not requested otherwise.
func (c *Connector) sweepStore(ctx context.Context, store Store, spec *generated.TypeSpec, since time.Time) (memqlsync.ReconcileReport, []string, error) {
	if !domainInConnection(store, spec) {
		return memqlsync.ReconcileReport{}, nil, memqlsync.NotRequested(ConnectorName)
	}
	if missing := scopesMissingFor(store, spec); len(missing) > 0 {
		return memqlsync.ReconcileReport{}, askedOf(store, spec.Scopes), nil
	}
	report, err := c.reconcileStore(ctx, store, spec, since)
	if memqlsync.IsNotGranted(err) && !refusalIsFixable(store, err.Error()) {
		return report, nil, memqlsync.NotRequested(ConnectorName)
	}
	return report, nil, err
}

// domainInConnection reports whether the store's own connection asks for any
// scope the domain reads under (a write scope asked reaches its read). A
// domain that names no scope is the origin's to answer, and inside.
func domainInConnection(store Store, spec *generated.TypeSpec) bool {
	if spec == nil || len(spec.Scopes) == 0 {
		return true
	}
	return len(askedOf(store, spec.Scopes)) > 0
}

// askedOf is the scopes among `scopes` that the store's connection asks for.
func askedOf(store Store, scopes []string) []string {
	asked := grantIncludes(connectionScopes(store))
	var out []string
	for _, s := range scopes {
		if asked[s] {
			out = append(out, s)
		}
	}
	return out
}

// refusalIsFixable sorts Shopify's refusal of a domain's root. Two are
// something a person can fix and stay not granted: the app has not declared
// the protected customer data the domain carries, or the refusal names a
// scope the connection asks for and the grant lacks. Every other root refusal
// -- a scope the connection never asks for, a feature the store's plan does
// not have -- is outside the connection.
func refusalIsFixable(store Store, message string) bool {
	if protectedDataRefusal(message) {
		return true
	}
	scope := requiredScopeOf(message)
	return scope != "" && grantIncludes(connectionScopes(store))[scope]
}

// requiredScopeRE reads the scope in Shopify's "Required access: `<scope>`
// access scope" sentence.
var requiredScopeRE = regexp.MustCompile("Required access: `([a-z_]+)` access scope")

// requiredScopeOf is the scope a Shopify refusal names, or "".
func requiredScopeOf(message string) string {
	if m := requiredScopeRE.FindStringSubmatch(message); m != nil {
		return m[1]
	}
	return ""
}

// protectedDataRefusal reports whether a refusal is Shopify's protected
// customer data gate: the app has not declared, or been approved for, the
// customer data an object carries.
func protectedDataRefusal(message string) bool {
	return strings.Contains(message, "is not approved to access") || strings.Contains(message, "protected-customer-data")
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

// topicScopesMissingFor is scopesMissingFor asked of the concept a webhook
// topic routes to, so subscriptions and reconciliation refuse a domain by one
// rule (memql#5638). A topic Shopify scopes differently from its concept is
// judged by the concept's scopes, which are what a delivery's fetch reads
// under. A topic that routes to no concept -- BULK_OPERATIONS_FINISH, the
// connector's own bulk completions -- reads nothing and needs no scope.
func topicScopesMissingFor(store Store, topic string) []string {
	route, ok := generated.Topics[topic]
	if !ok {
		return nil
	}
	return scopesMissingFor(store, generated.Types[route.Concept])
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
//
// A SWEEP IN WHICH A WRITE FAILED IS NOT A CLEAN SWEEP (unwritten). Every row
// is still healed that can be -- one row's failure does not stop the rest --
// and the sweep then answers an error, so the runtime holds the watermark
// where it was and the next sweep reads the same rows again.
//
// THE ROW BUDGET CUTS A SWEEP SHORT AND IT IS STILL RECORDED AS RUN. The
// watermark could be moved only to the last row read if the rows came in
// order of their last change, and they do not: the generated list operation
// names no sortKey, so the origin answers in its own default order (the id
// for most connections, processed_at for orders), and the last row read says
// nothing about which changes are still unread. Ordering the listing is the
// generator's to do.
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
	failed := 0
	for rows := 0; rows < relistRowCap; {
		vars := map[string]any{"first": c.pageSizeFor(store, spec)}
		if after != "" {
			vars["after"] = after
		}
		if filter != "" {
			vars["query"] = filter
		}
		// When this page was read: the read time every row on it carries.
		readAt := c.now()
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
			if err := c.completeChildren(ctx, store, spec, obj); err != nil {
				return report, err
			}
			writes := mapObject(spec, store.ID, obj, "", readAt)
			healed, lost := c.heal(ctx, writes)
			failed += lost
			if healed > 0 {
				report.Drifted++
				report.Healed++
			}
		}
		if !hasNext || next == "" {
			return report, unwritten(failed)
		}
		after = next
	}
	c.logger.Warn("shopify: updated_at sweep hit the row budget",
		"store", store.ID, "concept", spec.Concept, "rows", relistRowCap)
	return report, unwritten(failed)
}

// unwritten is the failure of a sweep or a re-list that read rows the mirror
// did not take: heal could not read or write them. It is an ordinary failure,
// worth another try: the runtime records it as the domain's error and leaves
// the watermark where it was, so the next sweep reads those rows again. A
// sweep that dropped the count was recorded clean, and a change that could
// not be recorded read as recorded.
func unwritten(failed int) error {
	switch failed {
	case 0:
		return nil
	case 1:
		return errors.New("shopify: 1 row read from the store could not be written to the mirror; the next check reads it again")
	}
	return fmt.Errorf("shopify: %d rows read from the store could not be written to the mirror; the next check reads them again", failed)
}

// relistListsPartially names the full re-list domains whose origin listing
// does not return every object the mirror may hold, and why. Tombstoning
// is an argument from absence, valid only against a listing that would
// have named the row, so these domains are healed by the walk and never
// tombstoned by it; a deletion reaches them through a delete topic or not
// at all. Every other full re-list domain is listed with no query, no date
// floor and no status filter (relist sends first and after only).
var relistListsPartially = map[string]string{
	"fulfillmentOrder": "fulfillmentOrders lists open fulfillment orders only (includeClosed defaults to false), " +
		"and only those the grant's fulfillment scopes reach",
	"abandonedCheckout": "order data read under read_orders; without read_all_orders, which is not requested, " +
		"Shopify answers recent order data only, so an older checkout is absent without being gone",
	"tenderTransaction": "order data read under read_orders; without read_all_orders, which is not requested, " +
		"Shopify answers recent order data only, so an older transaction is absent without being gone",
	"sellingPlanGroup": "sellingPlanGroups may answer only the groups the calling app created, " +
		"so a group another app made is absent without being gone",
	"marketingActivity": "marketingActivities may answer only the activities the calling app created, " +
		"so an activity another app made is absent without being gone",
	"marketingEvent": "marketingEvents may answer only the events the calling app created, " +
		"so an event another app made is absent without being gone",
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
	// When the listing began: the read time of every row it writes and every
	// tombstone, on this engine's clock. A row a delivery read after this
	// moment is not in the listing and not gone, and was read after it, so
	// heal refuses the tombstone (memqlsync.Refuses).
	begun := c.now()
	seen := map[string]bool{}
	after := ""
	complete := false
	// Rows the mirror did not take: the pass goes on, and answers them at its
	// end (unwritten). Tombstoning does not wait on them -- a row that was
	// listed is in `seen` whether or not its write landed.
	failed := 0
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
			if err := c.completeChildren(ctx, store, spec, obj); err != nil {
				return report, err
			}
			healed, lost := c.heal(ctx, mapObject(spec, store.ID, obj, "", begun))
			failed += lost
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
		return report, unwritten(failed)
	}
	if _, partial := relistListsPartially[spec.Concept]; partial {
		return report, unwritten(failed)
	}

	absent, err := c.liveRowsAbsentFrom(ctx, store, spec, seen)
	if err != nil {
		return report, err
	}
	for _, gid := range absent {
		// heal logs a row it could not write; a tombstone it refused is a
		// row written after the listing began.
		healed, lost := c.heal(ctx, []memqlsync.MirrorWrite{tombstone(spec.Concept, store.ID, gid, begun)})
		failed += lost
		if healed > 0 {
			report.Drifted++
			report.Healed++
		}
	}
	return report, unwritten(failed)
}

// liveRowPageCap bounds the read of a domain's live mirror rows, in pages
// of the generated read (100 rows, newest first). It is the re-list's own
// row cap and one page over, so a domain the walk could list whole is read
// whole; a mirror past it leaves its oldest rows untombstoned this pass,
// which errs toward keeping a row.
const liveRowPageCap = relistRowCap/100 + 1

// liveRowsAbsentFrom lists the mirror's live GIDs for a domain and returns
// the ones the origin walk did not produce. The generated read pages, newest
// first, and the oldest rows are the likeliest to be gone, so it is read to
// its end, the cursor riding the context as walk's does.
func (c *Connector) liveRowsAbsentFrom(ctx context.Context, store Store, spec *generated.TypeSpec, seen map[string]bool) ([]string, error) {
	var absent []string
	// operatorContext, not connectorContext: the generated mirror reads
	// filter on actor.isClusterOwner, which a connector actor never is, so
	// under its own identity the connector read none of the rows it wrote
	// (F10). TestTheConnectorReadsBackTheMirrorRowsItWrote holds it.
	base := operatorContext(ctx)
	call := renderCall(spec.ForStoreFn, map[string]any{"storeId": store.ID})
	cursor := ""
	for page := 0; page < liveRowPageCap; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pageCtx := base
		if cursor != "" {
			pageCtx = memql.ContextWithCursor(base, cursor)
		}
		res, err := c.engine.Execute(pageCtx, call)
		if err != nil {
			return nil, fmt.Errorf("shopify: list mirrored %s: %w", spec.Concept, err)
		}
		rows := memql.MaterializeRows(res)
		for _, row := range rows {
			gid := mapString(row, "gid")
			if gid == "" || seen[gid] {
				continue
			}
			absent = append(absent, gid)
		}
		next := ""
		if res != nil {
			if meta := res.GetMeta(); meta != nil {
				next = strings.TrimSpace(meta.Cursor)
			}
		}
		if next == "" || next == cursor || len(rows) == 0 {
			return absent, nil
		}
		cursor = next
	}
	c.logger.Warn("shopify: the mirror holds more live rows than one tombstone pass reads; the oldest are kept",
		"store", store.ID, "concept", spec.Concept, "pages", liveRowPageCap)
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
