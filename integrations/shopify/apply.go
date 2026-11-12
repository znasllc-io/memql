package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	stdsync "sync"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// apply.go -- a webhook is a trigger; the object is fetched (design D2).
//
// Nothing in this file reads a business field out of a payload, and that is
// the design rather than an accident. Webhook payloads are REST-shaped: they
// lose fields (the 2025-01 customer removals), truncate (100 variants),
// arrive unordered and duplicated, and are not guaranteed at all. Reading
// them would make the mirror a copy of the webhook rather than of the store.
//
// So a delivery contributes exactly three things: WHICH object changed (its
// id), WHETHER it was deleted (its topic), and WHEN (for the audit trail).
// The object itself is read back through Admin GraphQL with the generated
// selection set, and applied under the version guard.

// dedupeWindow bounds the in-process record of applied webhook ids.
const (
	dedupeWindow  = 15 * time.Minute
	dedupeMaxKeys = 4096
)

// applied is a bounded, time-windowed set of webhook ids this process has
// already handled.
//
// It is a SHORT-CIRCUIT, not the correctness mechanism, and the difference
// matters because it is process-local and this runs in a mesh. Idempotency
// comes from two places that survive a restart and a second replica: the
// receiver derives the staged row's id from SIGNED material, so a redelivery
// lands on the same row and emits no second creation event; and the version
// guard makes an identical re-fetch a no-op. What this saves is the Admin
// round-trip, which is the expensive part and the one that consumes the cost
// bucket.
type applied struct {
	mu   stdsync.Mutex
	seen map[string]time.Time
}

func newApplied() *applied { return &applied{seen: map[string]time.Time{}} }

func (a *applied) check(id string, now time.Time) bool {
	if id == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if at, ok := a.seen[id]; ok && now.Sub(at) < dedupeWindow {
		return true
	}
	if len(a.seen) >= dedupeMaxKeys {
		for k, at := range a.seen {
			if now.Sub(at) >= dedupeWindow {
				delete(a.seen, k)
			}
		}
		if len(a.seen) >= dedupeMaxKeys {
			// Still full of live entries: drop the whole window rather
			// than grow without bound. Losing the short-circuit costs a
			// duplicate fetch, which the guard absorbs.
			a.seen = map[string]time.Time{}
		}
	}
	a.seen[id] = now
	return false
}

// forget drops one id, so the next delivery carrying it is worked again. A
// delivery is remembered when it is first checked, before its fetch; one whose
// fetch or write then failed must not stay remembered, or the runtime's retry a
// minute later is answered "already seen" and the change is never fetched.
func (a *applied) forget(id string) {
	if id == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.seen, id)
}

// Apply implements sync.Connector.
//
// A delivery the short-circuit recorded is forgotten again when Apply fails,
// so the short-circuit holds only deliveries this connector applied.
func (c *Connector) Apply(ctx context.Context, req memqlsync.InboundRequest) ([]memqlsync.MirrorWrite, error) {
	if c == nil || c.stores == nil {
		return nil, nil
	}
	if req.Source == ConnectorName {
		if _, _, ok := c.managedAppSigning(ctx); !ok {
			// A row under the app-level source name while the managed app has
			// no client id or no sealed secret: nothing this connector trusts
			// verified it, so it is refused with a reason rather than bound
			// to a store by client id (memql#5707 review).
			return nil, fmt.Errorf("shopify: delivery on the app-level source while the managed app's client id or sealed secret is missing; nothing verified it as app-signed")
		}
	}
	store, found, err := c.deliveryStore(ctx, req)
	if err != nil {
		// COULD NOT TELL whose the delivery is: the store list, or the
		// managed app's configuration, could not be read. That is not "nobody
		// configured this store". Answered as that, the delivery was stamped
		// processed with nothing done, and a privacy request is not sent
		// twice. It fails as worth another try instead.
		return nil, fmt.Errorf("shopify: could not tell which store a delivery on source %q belongs to; it is tried again: %w", req.Source, err)
	}
	if !found {
		// A delivery for a store nobody configured, said definitely: the
		// list was read and holds no such store. Not an error: an operator
		// may have removed the store while Shopify still had a
		// subscription, and failing would retry it for no reason.
		c.logger.Warn("shopify: delivery for an unknown store", "source", req.Source, "topic", req.Topic)
		return nil, nil
	}

	topic := req.Topic
	if topic == "" {
		topic = header(req, HeaderTopic)
	}
	if isComplianceTopic(topic) {
		if req.Source != ConnectorName {
			managed, err := c.storeSignsWithManagedSecret(ctx, store)
			if err != nil {
				return nil, fmt.Errorf("shopify: %s for store %s: could not tell whether the managed app installed it: %w", topic, store.ID, err)
			}
			if managed {
				// A store installed through the managed app signs its
				// per-store deliveries with the app secret, so a captured
				// privacy delivery replays onto the per-store URL as well.
				// Privacy topics for such a store are accepted only on the
				// app-level source, where the webhook id collapses a replay
				// (memql#5707 review). privacy_refusal.go makes the refusal
				// impossible to miss.
				return nil, c.refusePrivacyOnStoreURL(ctx, store, topic, req)
			}
		}
		return nil, c.enqueueComplianceJob(ctx, store, topic, req)
	}
	if isUninstallTopic(topic) && req.Source == ConnectorName {
		// The app's own topic, declared app-level beside the privacy topics
		// and bound the same way (managedAppLevelStore), so a PAUSED store is
		// disconnected too (memql#5638). On a per-store source it stays the
		// unmirrored-topic no-op below: a managed store's per-store URL
		// verifies with the app's secret as well, and a captured app-level
		// delivery replayed there would stage as a new row.
		return nil, c.applyUninstall(ctx, store, req)
	}
	if !store.Ingests() {
		if pausedByAPerson(store) {
			// A PAUSE HOLDS, IT DOES NOT LOSE. The receiver staged the
			// delivery, and answering "nothing to write" for it would have the
			// runtime stamp it processed: the sweep after a resume reads what
			// changed, but a domain swept by its last-changed time cannot see
			// a deletion, so a record deleted during a pause would stay in the
			// mirror for good. The runtime holds the delivery instead
			// (memqlsync.ErrRecordingPaused). When the store records again,
			// resumeStore puts one page of what is held back in line and the
			// watch returns the rest on its passes (returnHeld).
			return nil, memqlsync.RecordingPaused(ConnectorName, store.ID)
		}
		// Not connected -- Shopify reported the app uninstalled, the store's
		// data was purged, or its status records nothing: nobody is waiting
		// for a resume, so nothing is asked and nothing is written, and the
		// runtime stamps the delivery processed.
		return nil, nil
	}

	enum := generated.TopicHeaderToEnum(topic)
	if enum == generated.TopicBulkOperationsFinish {
		return nil, c.onBulkOperationFinish(ctx, store, req)
	}
	route, known := generated.Topics[enum]
	if !known {
		// A topic this build does not mirror. Silence is right: the
		// subscription set is generated, so an unmirrored topic means
		// somebody subscribed by hand or Shopify sent one we did not ask
		// for, and neither is a delivery failure.
		return nil, nil
	}
	spec := generated.Types[route.Concept]
	if spec == nil {
		return nil, nil
	}

	gid := deliveredGID(req.Body, spec.GraphQLType)
	if gid == "" {
		return nil, fmt.Errorf("shopify: %s delivery carried no object id", topic)
	}

	if c.deduped(req, gid) {
		return nil, nil
	}

	if route.Action == generated.ActionDelete {
		// The origin's announcement: it retires at once. Its time is when
		// the delivery was RECEIVED (staged), on the receiving node's clock:
		// a moment by which the record was gone, kept by a retry. The guard
		// allows for that clock against the fetching node's
		// (memqlsync.AnnouncedRetireAllowance).
		w := tombstone(spec.Concept, store.ID, gid, announcedAt(req, c.now()))
		w.Announced = true
		// Stored, so a live write over it is held to the same allowance.
		w.Payload["versionSource"] = memqlsync.VersionFromAnnouncement
		return []memqlsync.MirrorWrite{w}, nil
	}

	writes, gone, readAt, err := c.fetchAndMap(ctx, store, spec, gid)
	if err != nil {
		c.forgetDelivery(req, gid)
		return nil, err
	}
	if gone {
		return c.answeredNothing(ctx, store, spec, req, gid, readAt)
	}
	return writes, nil
}

// answeredNothing decides what a clean answer of nothing does: the origin
// said, with no error at all, that it has no such record.
//
// A create or update notice for something Shopify no longer has is the RACE
// this exists for: the delete notice may arrive later, out of order, or never,
// and retiring on an empty fetch is what makes the mirror converge without it.
// But an answer of nothing is not always a deletion. In order:
//
//  1. THE MIRROR HOLDS THE RECORD AS RETIRED. Nothing is written and nothing
//     is asked again: the record is already gone here. A second tombstone, at
//     this fetch's later time and unannounced, would win over the one that is
//     there (memqlsync.Refuses: the later time wins) and take the standing of
//     a deletion the origin announced with it.
//
//  2. ORDER DATA, HELD LIVE, UNDER A GRANT THAT CANNOT READ PAST SIXTY DAYS
//     (emptyAnswerRetires). The origin hides an order older than that from an
//     app without read_all_orders and answers a clean null for it, so a late
//     refund's update notice is answered nothing however often it is asked,
//     and nothing would bring a retired order back: the sweep and the export
//     list the same sixty days. Nothing is written and nothing is asked again.
//     Only the origin's own announcement, the delete notice, retires it.
//
//  3. THE FIRST EMPTY ANSWER, one that does not follow one, is not believed:
//     it fails as worth another try (memqlsync.ErrAnsweredNothing), the retry
//     lane fetches again, and the dispatcher tells that try the last one
//     answered nothing (InboundRequest.AnsweredNothingBefore). For a record
//     the mirror holds, a deletion the origin did not announce is confirmed
//     before it is believed: on a domain swept by updated_at, a record wrongly
//     retired has no scheduled read that brings it back. For a record it does
//     not hold, a create notice can arrive before its record is readable, and
//     processing it at once would leave the record to the next sweep. A try
//     number alone proves nothing: a first try that failed for an ordinary
//     reason is not an answer. ON THE DELIVERY'S LAST TRY
//     (InboundRequest.LastTry) there is no other: a first empty answer for a
//     record the mirror does not hold is believed at once and nothing is
//     written, so the delivery does not end given up with nothing to record.
//
//  4. THE SECOND IN A ROW is believed. A record the mirror does not hold has
//     nothing to retire, so nothing is written; a held one is retired, at this
//     fetch's time, which is the read that found it gone.
func (c *Connector) answeredNothing(ctx context.Context, store Store, spec *generated.TypeSpec, req memqlsync.InboundRequest, gid string, readAt time.Time) ([]memqlsync.MirrorWrite, error) {
	conceptID := generated.ConceptID(spec.Concept)
	stored, _, err := c.storedMirrorRow(ctx, memqlsync.MirrorWrite{Concept: conceptID, RowId: MirrorRowID(store.ID, gid)})
	if err != nil {
		c.forgetDelivery(req, gid)
		return nil, err
	}
	held := stored != nil
	switch {
	case held && memqlsync.StoredStamp(mirrorDomain(conceptID), stored).Retired:
		return nil, nil
	case held && !emptyAnswerRetires(store, spec):
		c.logger.Info("shopify: an answer of nothing for order data was not taken as a deletion; this store's grant does not read orders older than sixty days",
			"store", store.ID, "concept", spec.Concept)
		return nil, nil
	case !held && req.LastTry():
		// The delivery's last try: failed to be asked once more, it would end
		// given up, and count as a change that could not be recorded, though
		// there is nothing to record. Believed at once.
		return nil, nil
	case !req.AnsweredNothingBefore:
		c.forgetDelivery(req, gid)
		return nil, memqlsync.AnsweredNothing(ConnectorName, gid)
	case !held:
		return nil, nil
	}
	return []memqlsync.MirrorWrite{tombstone(spec.Concept, store.ID, gid, readAt)}, nil
}

// The permission order data is read under, and the one that widens it past
// the last sixty days. A managed connection asks for the first and never the
// second; a store connected through its own app may hold both.
const (
	ordersScope    = "read_orders"
	allOrdersScope = "read_all_orders"
)

// readUnderOrders reports whether a kind is order data: read under the orders
// permission, so hidden by the origin, past sixty days, from a grant without
// read_all_orders. It is read off the generated specs, never a list of names:
// a kind whose own required access names read_orders, and every kind that
// hangs from one (Parent) -- an order's fulfilments and returns are read with
// it, whatever scope of their own the allowlist names.
func readUnderOrders(spec *generated.TypeSpec) bool {
	for hops := 0; spec != nil && hops <= len(generated.ApplyOrder); hops++ {
		for _, scope := range spec.Scopes {
			if scope == ordersScope {
				return true
			}
		}
		spec = generated.Types[spec.Parent]
	}
	return false
}

// emptyAnswerRetires reports whether an answer of nothing the origin did not
// announce may retire a record of this kind for this store: every kind, except
// order data under a grant that does not hold read_all_orders. A grant that
// was never recorded is not known to hold it.
func emptyAnswerRetires(store Store, spec *generated.TypeSpec) bool {
	return !readUnderOrders(spec) || grantIncludes(store.ScopesGranted)[allOrdersScope]
}

// pausedByAPerson reports whether the store's recording is paused and nothing
// else: a person chose it, and a resume returns the store to recording. A
// purge leaves the status paused as well (markStoreRedacted), and an
// uninstalled store keeps whatever status it had; neither is such a pause,
// and the verdict reads both as not connected.
func pausedByAPerson(store Store) bool {
	return store.Status == StatusPaused && store.UninstalledAt == "" && store.RedactedAt == ""
}

func (c *Connector) deduped(req memqlsync.InboundRequest, gid string) bool {
	key := dedupeKey(req, gid)
	if c.applied == nil || key == "" {
		return false
	}
	return c.applied.check(key, c.now())
}

// dedupeKey is what the short-circuit records a delivery under: its webhook id
// and the object it names, or "" for a delivery carrying no webhook id.
func dedupeKey(req memqlsync.InboundRequest, gid string) string {
	id := header(req, HeaderWebhookID)
	if id == "" {
		return ""
	}
	return id + "\x00" + gid
}

// forgetDelivery drops one delivery from the short-circuit.
func (c *Connector) forgetDelivery(req memqlsync.InboundRequest, gid string) {
	if c.applied != nil {
		c.applied.forget(dedupeKey(req, gid))
	}
}

// ForgetInbound implements memqlsync.InboundForgetter. The runtime calls it
// when a delivery this connector answered could not be written, and will be
// tried again: Apply returned, so the delivery is recorded as handled, and the
// retry would otherwise be answered from the short-circuit with nothing.
func (c *Connector) ForgetInbound(_ context.Context, req memqlsync.InboundRequest) {
	topic := req.Topic
	if topic == "" {
		topic = header(req, HeaderTopic)
	}
	route, known := generated.Topics[generated.TopicHeaderToEnum(topic)]
	if !known {
		return
	}
	spec := generated.Types[route.Concept]
	if spec == nil {
		return
	}
	if gid := deliveredGID(req.Body, spec.GraphQLType); gid != "" {
		c.forgetDelivery(req, gid)
	}
}

// fetchAndMap reads one object with the generated selection set and maps it,
// every write carrying readAt: when this fetch began, on this engine's clock.
//
// gone is true only for a CLEAN answer that the origin has no such record:
// the object null, and no error at all. Every other answer without the
// object is an error and retires nothing:
//   - the node itself refused (how Shopify refuses protected customer data on
//     node(id:)): not granted, permanent (callWithGrant);
//   - a denial still nulling the object after the prune limit (a partial
//     answer): not granted, permanent;
//   - a reply with no data, or data without the object's key: a failure the
//     retry lane tries again;
//   - any other error: as the client classed it.
func (c *Connector) fetchAndMap(ctx context.Context, store Store, spec *generated.TypeSpec, gid string) (writes []memqlsync.MirrorWrite, gone bool, readAt time.Time, err error) {
	if !spec.Fetchable {
		// A type with no node(id:) resolution and no singleton query is
		// only ever materialised with its parent. Reaching here means a
		// topic routes to it directly, which the allowlist should not
		// allow -- so it is a configuration error, said plainly.
		return nil, false, readAt, fmt.Errorf("shopify: %s cannot be fetched by GID (it is materialised with its parent)", spec.GraphQLType)
	}
	vars := map[string]any{}
	if spec.Singleton == "" {
		vars["id"] = gid
	}
	readAt = c.now()
	resp, err := c.callWithGrant(ctx, store, spec, spec.FetchOp, vars)
	if err != nil {
		if memqlsync.IsNotGranted(err) {
			// A delivery for a domain the grant does not cover: no
			// number of retries fetches it, so the drain must not spend
			// them.
			return nil, false, readAt, memqlsync.Permanent(err)
		}
		return nil, false, readAt, err
	}
	data, err := resp.DataMap()
	if err != nil {
		return nil, false, readAt, err
	}
	key := "node"
	if spec.Singleton != "" {
		key = spec.Singleton
	}
	raw, present := data[key]
	if !present {
		return nil, false, readAt, fmt.Errorf("shopify: %s answered no %s for %s", spec.FetchOp, key, gid)
	}
	obj, _ := raw.(map[string]any)
	if obj == nil {
		if len(resp.Errors) > 0 {
			// A partial answer: the origin refused part of the selection and
			// what is left has no object. That is the grant, not a deletion.
			return nil, false, readAt, memqlsync.Permanent(fmt.Errorf("%w: %s for %s answered no object beside: %v",
				memqlsync.ErrNotGranted, spec.FetchOp, gid, adminErrors(resp.Errors)))
		}
		return nil, true, readAt, nil
	}
	// node(id:) resolves ANY type, and an inline fragment on the wrong one
	// returns an object carrying nothing but __typename. Without this check
	// that becomes a mirror row with every field blank -- a silent
	// corruption rather than an error.
	if tn, _ := obj["__typename"].(string); tn != "" && tn != spec.GraphQLType {
		return nil, false, readAt, fmt.Errorf("shopify: %s resolved to a %s, not a %s", gid, tn, spec.GraphQLType)
	}
	if _, ok := obj["id"].(string); !ok {
		// Both the node and the singleton select the object's own id; an
		// object without one is a malformed answer, not a record to write
		// or a deletion.
		return nil, false, readAt, fmt.Errorf("shopify: %s response carried no id", spec.GraphQLType)
	}
	// Before the object is mapped, so the later pages' children are
	// mapped with the rest: under this parent, on this store's partition.
	if err := c.completeChildren(ctx, store, spec, obj); err != nil {
		return nil, false, readAt, err
	}
	return mapObject(spec, store.ID, obj, "", readAt), false, readAt, nil
}

// fetchedObject digs the object out of whichever shape the operation used.
func fetchedObject(data map[string]any, spec *generated.TypeSpec) map[string]any {
	if spec.Singleton != "" {
		obj, _ := data[spec.Singleton].(map[string]any)
		return obj
	}
	obj, _ := data["node"].(map[string]any)
	return obj
}

// deliveredGID recovers the object's GID from a delivery.
//
// Three spellings, because Shopify has used three. `admin_graphql_api_id` is
// the modern one and carries the GID whole; older topics carry a numeric
// `id`; a few carry `admin_graphql_api_id` nested under the resource. A
// connector that knew only the first would silently ignore every delivery of
// the others -- which looks exactly like a store that has stopped changing.
func deliveredGID(body []byte, graphqlType string) string {
	if len(body) == 0 {
		return ""
	}
	var payload map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return ""
	}
	if gid := firstString(payload, "admin_graphql_api_id"); strings.HasPrefix(gid, "gid://") {
		return gid
	}
	if id := firstString(payload, "id"); id != "" {
		if strings.HasPrefix(id, "gid://") {
			return id
		}
		return "gid://shopify/" + graphqlType + "/" + id
	}
	return ""
}

// announcedAt is a delete topic's tombstone time: the delivery's receipt.
// A receipt an earlier build staged at whole seconds (the receiver now stages
// it to the nanosecond) counts as the END of its second: it may have been
// received any time in it, and a read inside that second before the receipt
// must not outrank the delete.
func announcedAt(req memqlsync.InboundRequest, fallback time.Time) time.Time {
	at := receivedAt(req, fallback)
	if !req.ReceivedAt.IsZero() && at.Nanosecond() == 0 {
		at = at.Add(time.Second - time.Nanosecond)
	}
	return at
}

// receivedAt is when a delivery was received (staged), on this engine's
// clock, or `fallback` for one that carries none. A retry keeps it: it is the
// staged row's, not the try's.
func receivedAt(req memqlsync.InboundRequest, fallback time.Time) time.Time {
	if !req.ReceivedAt.IsZero() {
		return req.ReceivedAt.UTC()
	}
	return fallback.UTC()
}

// deliveredAt reads the origin's own timestamp for the delivery, falling back
// to its receipt and then to now: the event's time, for the uninstall audit.
func deliveredAt(req memqlsync.InboundRequest, fallback time.Time) time.Time {
	if raw := header(req, HeaderTriggeredAt); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t.UTC()
		}
	}
	return receivedAt(req, fallback)
}

func isComplianceTopic(topic string) bool {
	t := strings.ToLower(strings.TrimSpace(topic))
	for _, c := range generated.ComplianceTopics {
		if t == c {
			return true
		}
	}
	return false
}
