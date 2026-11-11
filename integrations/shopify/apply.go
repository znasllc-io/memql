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
	store, ok := c.StoreFor(ctx, req)
	if !ok {
		// A delivery for a store nobody configured. Not an error: an
		// operator may have removed the store while Shopify still had a
		// subscription, and failing would retry it eight times over four
		// hours for no reason.
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
		// A paused store still STAGES -- the receiver recorded the
		// delivery -- so a pause loses telemetry rather than events, and
		// resuming does not need a backfill.
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
		// Its time is when the delivery was RECEIVED (staged), on this
		// engine's clock: a moment by which the record was gone, kept by a
		// retry, and ordered against when the origin was last read for the
		// row (memqlsync.Refuses).
		return []memqlsync.MirrorWrite{tombstone(spec.Concept, store.ID, gid, receivedAt(req, c.now()))}, nil
	}

	writes, gone, readAt, err := c.fetchAndMap(ctx, store, spec, gid)
	if err != nil {
		c.forgetDelivery(req, gid)
		return nil, err
	}
	if gone {
		// The origin answered cleanly that it has no such record. A
		// create/update topic for something Shopify no longer has is the
		// RACE this arm exists for: the delete delivery may arrive later,
		// out of order, or never. Tombstoning on an empty fetch is what makes
		// the mirror converge without it.
		//
		// Its time is the FETCH's: that read is what found the record gone.
		// A record the mirror does not hold has nothing to retire -- a
		// create delivery can arrive before its record is readable -- so
		// nothing is written, and the record arrives with the next read that
		// finds it.
		held, err := c.mirrorHoldsRow(ctx, spec, store.ID, gid)
		if err != nil {
			c.forgetDelivery(req, gid)
			return nil, err
		}
		if !held {
			return nil, nil
		}
		return []memqlsync.MirrorWrite{tombstone(spec.Concept, store.ID, gid, readAt)}, nil
	}
	return writes, nil
}

// mirrorHoldsRow reports whether the mirror holds a row for a gid, live or
// retired.
func (c *Connector) mirrorHoldsRow(ctx context.Context, spec *generated.TypeSpec, storeID, gid string) (bool, error) {
	stored, err := c.storedMirrorRow(ctx, memqlsync.MirrorWrite{Concept: generated.ConceptID(spec.Concept), RowId: MirrorRowID(storeID, gid)})
	return stored != nil, err
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
