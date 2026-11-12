package shopify

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// verdict_capabilities.go -- the verdict's builtins: the read that gathers a
// store's verdict input, the reply, the two acts its reasons offer, and the
// Store page's pause and resume. The rules themselves are verdict.go's, which
// reads nothing.

// verdictCapabilities is the Store page's verdict, its two acts, and pause and
// resume. Each is declared with @requiresCapability("execute",
// "app:deployables/store") in dsl/shopify/overlay/builtins.memql, which the
// engine asks before the handler runs; who may see or act on THIS store is
// asked in the handler.
func (i *Integration) verdictCapabilities() []memql.IntegrationCapability {
	store := map[string]string{"storeId": "string - the store"}
	return []memql.IntegrationCapability{
		{
			Name:        "storeVerdict",
			Description: "One store's verdict -- recording, starting, paused, needs attention or not connected -- with each reason a store needs attention, its one act, what is recorded, what is outside the connection and what Shopify withholds. For the store's owner, a cluster owner, or a caller who may read stores.",
			Handler:     i.handleStoreVerdict,
			ArgsSchema:  store,
		},
		{
			Name:        "storeRegisterEvents",
			Description: "Run one store's subscription pass now and record it on the store's health: Register again. For the store's owner or a cluster owner.",
			Handler:     i.handleStoreRegisterEvents,
			ArgsSchema:  store,
		},
		{
			Name:        "storeRetryDeliveries",
			Description: "Put one store's deliveries that used their four tries back in line: Try again. For the store's owner or a cluster owner.",
			Handler:     i.handleStoreRetryDeliveries,
			ArgsSchema:  store,
		},
		{
			Name:        "storeSetRecording",
			Description: "Pause or resume recording one store: a store being recorded (configured, backfilling or live) moves to paused, a paused one to live, written as the deployment and audited under the caller. A store that is not connected (error, uninstalled, purged) is refused, and asking for the state it is in writes nothing. For the store's owner or a cluster owner.",
			Handler:     i.handleStoreSetRecording,
			ArgsSchema:  map[string]string{"storeId": "string - the store", "recording": "boolean - true resumes recording, false pauses it"},
		},
	}
}

// registerAgainWait is how long after a recorded subscription pass Register
// again refuses: each pass is one paced Admin call per topic, and a second
// one straight after the first finds what the first left.
const registerAgainWait = time.Minute

// mayActOnStore reports whether the caller may act on a store: its owner, or
// a cluster owner -- who may connect it.
func (c *Connector) mayActOnStore(ctx context.Context, store Store) bool {
	return isClusterOwner(ctx) || ownsStore(ctx, store)
}

// ownsStore reports whether the caller is the store's owner.
func ownsStore(ctx context.Context, store Store) bool {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return false
	}
	caller := strings.TrimSpace(ac.UserId)
	owner := strings.TrimSpace(store.OwnerUserID)
	return caller != "" && owner != "" && memql.BareShortId(caller) == memql.BareShortId(owner)
}

// storeForCaller resolves the store a verdict call names and asks `may` of
// the caller before anything else about the store is read. A refused caller
// is told `refused` whether or not the store exists.
func (c *Connector) storeForCaller(ctx context.Context, storeID, builtin, refused string, may func(Store) bool) (Store, error) {
	if storeID == "" {
		return Store{}, fmt.Errorf("shopify: %s needs a storeId", builtin)
	}
	store, ok := c.stores.ByID(ctx, storeID)
	if !ok || !may(store) {
		if !ok && may(Store{}) {
			return Store{}, fmt.Errorf("shopify: store %q is not configured", storeID)
		}
		return Store{}, fmt.Errorf("shopify: %s %s", builtin, refused)
	}
	return store, nil
}

const actRefusal = "is for the store's owner or a cluster owner"

// readRefusal is the refusal of a read for the store's owner, a cluster owner,
// or a caller who may read stores.
const readRefusal = "is for the store's owner, a cluster owner, or a caller who may read stores"

// mayReadStore reports whether the caller may read a store: its owner, a
// cluster owner, or a caller the store tier lets read every store.
func (c *Connector) mayReadStore(ctx context.Context, store Store) bool {
	return c.mayActOnStore(ctx, store) || c.mayReadStores(ctx)
}

func (i *Integration) handleStoreVerdict(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	store, err := c.storeForCaller(ctx, argString(args, "storeId"), "shopifyStoreVerdict", readRefusal,
		func(s Store) bool { return c.mayReadStore(ctx, s) })
	if err != nil {
		return nil, err
	}
	v, err := c.storeVerdict(ctx, store)
	if err != nil {
		return nil, err
	}
	reply := verdictPayload(v)
	reply["alert"] = recordedAlert(store, v.State)
	reply["may"] = map[string]any{"act": c.mayActOnStore(ctx, store)}
	return resultNode("shopify", reply)
}

func (i *Integration) handleStoreRegisterEvents(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	// Read fresh: whether a pass ran a moment ago is read off the row, and
	// another node may have recorded it.
	store, err := c.storeForCaller(memql.ContextWithFreshRead(ctx), argString(args, "storeId"), "shopifyStoreRegisterEvents", actRefusal,
		func(s Store) bool { return c.mayActOnStore(ctx, s) })
	if err != nil {
		return nil, err
	}
	if !store.Ingests() {
		return nil, fmt.Errorf("shopify: store %q is not recording, so its events are not registered", store.ID)
	}
	if sub, ok := store.Health["subscriptions"].(map[string]any); ok {
		if at := rowTime(sub, "at"); !at.IsZero() && c.now().UTC().Sub(at) < registerAgainWait {
			return nil, fmt.Errorf("shopify: this store's events were registered just now; try again in a minute")
		}
	}
	report, readErr := c.subscriptionPass(ctx, store)
	status, detail := "registered", ""
	switch {
	case readErr != nil:
		status, detail = "failed", readErr.Error()
	case len(report.Failed) > 0:
		status, detail = "failed", strings.Join(report.Failed, "; ")
	}
	return resultNode("shopify", map[string]any{
		"status": status, "detail": detail, "storeId": store.ID, "desired": report.Desired, "existing": report.Existing,
		"created": toAny(report.Created), "failed": toAny(report.Failed),
	})
}

func (i *Integration) handleStoreRetryDeliveries(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	store, err := c.storeForCaller(ctx, argString(args, "storeId"), "shopifyStoreRetryDeliveries", actRefusal,
		func(s Store) bool { return c.mayActOnStore(ctx, s) })
	if err != nil {
		return nil, err
	}
	source := storeInboundSource(store.ID)
	if _, err := c.engine.Execute(operatorContext(ctx), "builtin "+renderCall("datasyncRequeueInbound", map[string]any{"source": source})); err != nil {
		return nil, fmt.Errorf("shopify: the store's deliveries could not be put back in line: %w", err)
	}
	return resultNode("shopify", map[string]any{"status": "requeued", "storeId": store.ID, "source": source})
}

// notConnectedRefusal refuses a pause or a resume of a store that is not
// connected: uninstalled, purged, or in a status it neither ingests under nor
// is paused in (error).
const notConnectedRefusal = "shopify: a store that is not connected cannot be paused or resumed"

// handleStoreSetRecording is the Store page's Pause recording and Resume.
//
// A STORE THAT IS BEING RECORDED CAN BE PAUSED WHATEVER ITS STATUS WORD: every
// status it ingests under (Store.Ingests: configured, backfilling, live) moves
// to paused -- createStore stamps configured and nothing moves a connected
// store on to live, so most stores that are recorded say configured. A paused
// store resumes to live. Asking for the state the store is in writes nothing.
//
// THE WRITE IS THE DEPLOYMENT'S. setStoreStatus's row is a cluster owner's
// (@rowAuthz(clusterOwner)), so a store's owner who is not one would be offered
// the act and refused by the row. The caller is asked here instead -- the
// store's owner or a cluster owner, before anything but the store row is read
// -- and the write runs as the operator, so the row's own gate is not widened.
// The person is kept on an audit event of their own, one per pause or resume.
// The next verdict says the store resumed, and the next watch pass records it.
//
// A RESUME THEN DOES WHAT IT OWES THE STORE (resumeStore): one page of the
// deliveries held while it was paused goes back in line, and the backfills it
// never started are queued. It does not wait for more, and it does not fail
// for what it could not do: the store watch returns whatever is still held on
// its next pass. A pause does nothing more than write the status: its
// deliveries are held as they arrive.
func (i *Integration) handleStoreSetRecording(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	// Read fresh: the status decides what is written, and another node may
	// have moved it.
	store, err := c.storeForCaller(memql.ContextWithFreshRead(ctx), argString(args, "storeId"), "shopifyStoreSetRecording", actRefusal,
		func(s Store) bool { return c.mayActOnStore(ctx, s) })
	if err != nil {
		return nil, err
	}
	recording, ok := args["recording"].(bool)
	if !ok {
		return nil, fmt.Errorf("shopify: shopifyStoreSetRecording needs recording, true or false")
	}
	paused := store.Status == StatusPaused
	if store.RedactedAt != "" || store.UninstalledAt != "" || (!store.Ingests() && !paused) {
		return nil, fmt.Errorf("%s", notConnectedRefusal)
	}
	switch {
	case recording && paused:
		if err = c.writeRecording(ctx, store, StatusLive, "shopify_store_resumed"); err == nil {
			// What the act does for the store before it answers: one page of
			// what was held goes back in line, and the backfills it never
			// started are queued. The resume stands whatever comes of it, and
			// the watch does the rest.
			resumed := store
			resumed.Status = StatusLive
			c.resumeStore(ctx, resumed)
		}
	case !recording && !paused:
		err = c.writeRecording(ctx, store, StatusPaused, "shopify_store_paused")
	}
	if err != nil {
		return nil, err
	}
	reply := StatusLive
	if !recording {
		reply = StatusPaused
	}
	return resultNode("shopify", map[string]any{"status": reply, "storeId": store.ID})
}

// writeRecording moves the store to `to` as the operator and records who did,
// as markReconnected does: an audit event naming the caller, the store and the
// two statuses, and nothing else. A failed audit write is loud and does not
// undo the act.
func (c *Connector) writeRecording(ctx context.Context, store Store, to, action string) error {
	if _, err := c.engine.Execute(operatorContext(ctx), renderCall("setStoreStatus", map[string]any{"storeId": store.ID, "status": to})); err != nil {
		return fmt.Errorf("shopify: the store's status could not be written: %w", err)
	}
	c.stores.Invalidate()
	person := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		person = strings.TrimSpace(ac.UserId)
	}
	at := c.now().UTC()
	call := renderCall("createAuditEvent", map[string]any{
		"eventId":     "aud" + MirrorRowID(store.ID, action+"\x00"+person+"\x00"+at.Format(time.RFC3339Nano)),
		"occurredAt":  at.Format(time.RFC3339),
		"category":    "configuration",
		"action":      action,
		"actorUserId": person,
		"targetType":  "shopifyStore",
		"targetId":    store.ID,
		"detail":      map[string]any{"from": store.Status, "to": to},
		"outcome":     "success",
	})
	if _, err := c.engine.Execute(operatorContext(ctx), call); err != nil {
		c.logger.Error("shopify: could not write a recording audit event", "store", store.ID, "action", action, "error", err)
	}
	return nil
}

// storeVerdict gathers one store's verdict input and computes it: the rows
// of the domains the verdict reads, by id; the given-up deliveries that can
// still count; the fields Shopify withholds; and the verdict last recorded on
// its health.
//
// A paused or disconnected store carries no reasons, so its given-up
// deliveries are not read at all.
func (c *Connector) storeVerdict(ctx context.Context, store Store) (Verdict, error) {
	states, err := c.syncStatesOf(ctx, store.ID, verdictDomains(store))
	if err != nil {
		return Verdict{}, err
	}
	var givenUp []map[string]any
	floor := false
	if !storeInactive(store) {
		givenUp, floor, err = c.givenUpDeliveries(ctx, store.ID, givenUpBound(store, states))
		if err != nil {
			return Verdict{}, err
		}
	}
	// The store's privacy requests that keep failing, read whatever the
	// store's state (privacy_verdict.go).
	privacy, err := c.privacyRequestsFailing(ctx, store.ID)
	if err != nil {
		return Verdict{}, err
	}
	previous, _ := store.Health["verdict"].(map[string]any)
	return computeVerdict(verdictInput{
		Store:        store,
		States:       states,
		Intervals:    sweepIntervals(c.Domains()),
		GivenUp:      givenUp,
		GivenUpFloor: floor,
		Denied:       c.FieldsDenied(store.ID),
		Previous:     previous,
		ConnectedAt:  connectedAtOf(store),
		Privacy:      privacy,
		Now:          c.now().UTC(),
	}), nil
}

// connectedAtOf is when a connect last started the store's recording afresh,
// from the store row's own connectedAt. Zero for a store connected before it
// was recorded: nothing else on the row is a connect time (row.createdAt
// moves with every write).
func connectedAtOf(store Store) time.Time {
	at, err := time.Parse(time.RFC3339Nano, store.ConnectedAt)
	if err != nil {
		return time.Time{}
	}
	return at.UTC()
}

func timeOrEmpty(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

// storeInboundSource is the inbound source a store's deliveries are staged
// under.
func storeInboundSource(storeID string) string { return ConnectorName + "-" + storeID }

// givenUpPageBudget bounds the given-up walk, in pages of 50. A walk that
// reaches it is counted as a floor, never a failed verdict.
var givenUpPageBudget = 1000

// givenUpDeliveries reads the store's deliveries that used their four tries,
// most recently written first, and reports whether the count is a floor: the
// walk ran out of budget with more to read.
//
// THE WALK STOPS at the first page whose rows were all last written before
// `bound`, the oldest clean sweep among the domains the verdict asks about
// and the store's grant holds (givenUpBound). A row's last write is never
// earlier than its receipt -- the receiver writes it when the delivery
// arrives, and every write after that is later -- so a row last written
// before the bound was received before it, and before the clean sweep of
// whichever domain it routes to: it cannot count. The rows come newest-written
// first, so every page after that one is older still. A zero bound reads on to
// the end.
func (c *Connector) givenUpDeliveries(ctx context.Context, storeID string, bound time.Time) ([]map[string]any, bool, error) {
	var out []map[string]any
	query := renderCall("inboundRequestsGivenUp", map[string]any{"source": storeInboundSource(storeID)})
	opCtx := operatorContext(ctx)
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < givenUpPageBudget; page++ {
		res, err := c.engine.Execute(memql.ContextWithCursor(memql.ContextWithFreshRead(opCtx), cursor), query)
		if err != nil {
			return nil, false, fmt.Errorf("shopify: read given-up deliveries: %w", err)
		}
		if res == nil {
			return nil, false, fmt.Errorf("shopify: read given-up deliveries: no result")
		}
		rows := memql.MaterializeRows(res)
		older := len(rows) > 0 && !bound.IsZero()
		for _, row := range rows {
			if written := rowTime(row, "createdAt"); written.IsZero() || !written.Before(bound) {
				older = false
			}
		}
		if older {
			return out, false, nil
		}
		out = append(out, rows...)
		next := ""
		if meta := res.GetMeta(); meta != nil {
			next = meta.Cursor
			if meta.HasMore && next == "" {
				return nil, false, fmt.Errorf("shopify: read given-up deliveries: more rows were declared without a cursor")
			}
		}
		if next == "" {
			return out, false, nil
		}
		if seen[next] || next == cursor {
			return nil, false, fmt.Errorf("shopify: read given-up deliveries: the cursor did not advance")
		}
		seen[next] = true
		cursor = next
	}
	return out, true, nil
}

// verdictPayload is the verdict as the reply carries it.
func verdictPayload(v Verdict) map[string]any {
	reasons := make([]any, 0, len(v.Reasons))
	for _, r := range v.Reasons {
		reasons = append(reasons, map[string]any{
			"code": r.Code, "domain": r.Domain, "sentence": r.Sentence, "detail": r.Detail, "count": r.Count,
			"atLeast": r.AtLeast, "act": r.Act, "actLabel": r.ActLabel, "actUrl": r.ActURL,
		})
	}
	recorded := make([]any, 0, len(v.Recorded))
	for _, k := range v.Recorded {
		recorded = append(recorded, map[string]any{"kind": k.Kind, "lastChangeAt": k.LastChangeAt, "lastCheckedAt": k.LastCheckedAt})
	}
	return map[string]any{
		"storeId": v.StoreID, "domain": v.Domain, "state": v.State, "since": v.Since.UTC().Format(time.RFC3339),
		"resumedAt": timeOrEmpty(v.ResumedAt), "connectedAt": timeOrEmpty(v.ConnectedAt),
		"reasons": reasons, "recorded": recorded, "outside": toAny(v.Outside), "withheld": toAny(v.Withheld),
		"ordersFrom": v.OrdersFrom,
	}
}
