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

// Apply implements sync.Connector.
func (c *Connector) Apply(ctx context.Context, req memqlsync.InboundRequest) ([]memqlsync.MirrorWrite, error) {
	if c == nil || c.stores == nil {
		return nil, nil
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
	if !store.Ingests() {
		// A paused store still STAGES -- the receiver recorded the
		// delivery -- so a pause loses telemetry rather than events, and
		// resuming does not need a backfill.
		return nil, nil
	}

	topic := req.Topic
	if topic == "" {
		topic = header(req, HeaderTopic)
	}
	if isComplianceTopic(topic) {
		return nil, c.enqueueComplianceJob(ctx, store, topic, req)
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
		return []memqlsync.MirrorWrite{tombstone(spec.Concept, store.ID, gid, deliveredAt(req, c.now()))}, nil
	}

	writes, err := c.fetchAndMap(ctx, store, spec, gid)
	if err != nil {
		return nil, err
	}
	if writes == nil {
		// The object is gone. A create/update topic for something Shopify
		// no longer has is the RACE this arm exists for: the delete
		// delivery may arrive later, out of order, or never. Tombstoning
		// on an empty fetch is what makes the mirror converge without it.
		return []memqlsync.MirrorWrite{tombstone(spec.Concept, store.ID, gid, deliveredAt(req, c.now()))}, nil
	}
	return writes, nil
}

func (c *Connector) deduped(req memqlsync.InboundRequest, gid string) bool {
	if c.applied == nil {
		return false
	}
	key := header(req, HeaderWebhookID)
	if key == "" {
		key = header(req, HeaderWebhookID)
	}
	if key == "" {
		return false
	}
	return c.applied.check(key+"\x00"+gid, c.now())
}

// FetchByGID reads one object with the generated selection set and maps it.
// Returns nil writes when the object no longer exists.
func (c *Connector) fetchAndMap(ctx context.Context, store Store, spec *generated.TypeSpec, gid string) ([]memqlsync.MirrorWrite, error) {
	if !spec.Fetchable {
		// A type with no node(id:) resolution and no singleton query is
		// only ever materialised with its parent. Reaching here means a
		// topic routes to it directly, which the allowlist should not
		// allow -- so it is a configuration error, said plainly.
		return nil, fmt.Errorf("shopify: %s cannot be fetched by GID (it is materialised with its parent)", spec.GraphQLType)
	}
	vars := map[string]any{}
	if spec.Singleton == "" {
		vars["id"] = gid
	}
	resp, err := c.adminCall(ctx, store, spec.FetchDocument, spec.FetchOp, vars)
	if err != nil {
		return nil, err
	}
	data, err := resp.DataMap()
	if err != nil {
		return nil, err
	}
	obj := fetchedObject(data, spec)
	if obj == nil {
		return nil, nil
	}
	// node(id:) resolves ANY type, and an inline fragment on the wrong one
	// returns an object carrying nothing but __typename. Without this check
	// that becomes a mirror row with every field blank -- a silent
	// corruption rather than an error.
	if tn, _ := obj["__typename"].(string); tn != "" && tn != spec.GraphQLType {
		return nil, fmt.Errorf("shopify: %s resolved to a %s, not a %s", gid, tn, spec.GraphQLType)
	}
	if _, ok := obj["id"].(string); !ok && spec.Singleton != "" {
		// The singleton query returns the object directly and Shop's own
		// id is in the selection; a missing one is a malformed response.
		return nil, fmt.Errorf("shopify: %s response carried no id", spec.GraphQLType)
	}
	return mapObject(spec, store.ID, obj, "", c.now()), nil
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

// deliveredAt reads the origin's own timestamp for the delivery, falling back
// to now. Used as the version on a tombstone, where there is no object left
// to read a version from.
func deliveredAt(req memqlsync.InboundRequest, fallback time.Time) time.Time {
	if raw := header(req, HeaderTriggeredAt); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t.UTC()
		}
	}
	if !req.ReceivedAt.IsZero() {
		return req.ReceivedAt.UTC()
	}
	return fallback.UTC()
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
