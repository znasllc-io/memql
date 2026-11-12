package shopify

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// subscriptions.go -- making Shopify tell us about changes, and keeping it
// telling us.
//
// The reconcile half is the part that earns its keep. Shopify retries a
// failed delivery eight times over four hours and then DELETES the
// subscription after eight consecutive failures. A deployment that was
// unreachable for an afternoon comes back with its webhooks silently gone,
// and nothing in the store or the app says so -- the mirror simply stops
// changing. So the subscription list is compared against the generated set on
// boot and daily, and the result lands on the store's health where an
// operator can see it.

const subscriptionPageSize = 250

// includeFields is what Shopify sends in the payload body.
//
// Trimmed to identity plus a change timestamp on purpose: the payload is a
// SIGNAL, and the object is fetched (design D2). A full payload would be
// bigger, slower, lossier than the API, and would tempt a future reader into
// writing it. `id` and `admin_graphql_api_id` are what the apply path reads;
// `updated_at` is for the audit trail.
var includeFields = []string{"id", "admin_graphql_api_id", "updated_at"}

const webhookListQuery = `query ShopifyWebhookSubscriptions($first: Int!, $after: String) {
  webhookSubscriptions(first: $first, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id
      topic
      includeFields
      apiVersion { handle }
      uri
    }
  }
}`

const webhookCreateMutation = `mutation ShopifyWebhookCreate($topic: WebhookSubscriptionTopic!, $sub: WebhookSubscriptionInput!) {
  webhookSubscriptionCreate(topic: $topic, webhookSubscription: $sub) {
    webhookSubscription { id topic }
    userErrors { field message }
  }
}`

const webhookUpdateMutation = `mutation ShopifyWebhookUpdate($id: ID!, $sub: WebhookSubscriptionInput!) {
  webhookSubscriptionUpdate(id: $id, webhookSubscription: $sub) {
    webhookSubscription { id topic }
    userErrors { field message }
  }
}`

const webhookDeleteMutation = `mutation ShopifyWebhookDelete($id: ID!) {
  webhookSubscriptionDelete(id: $id) {
    deletedWebhookSubscriptionId
    userErrors { field message }
  }
}`

// Subscription is one registered webhook, as the reconcile pass reads it.
type Subscription struct {
	ID            string
	Topic         string
	CallbackURL   string
	APIVersion    string
	IncludeFields []string
}

// SubscriptionReport is what one reconcile pass did. It goes onto the store's
// health verbatim, because "nothing changed" and "we could not tell" are
// different states and an operator needs to see which one this was.
type SubscriptionReport struct {
	StoreID  string   `json:"storeId"`
	Desired  int      `json:"desired"`
	Existing int      `json:"existing"`
	Created  []string `json:"created,omitempty"`
	Updated  []string `json:"updated,omitempty"`
	Removed  []string `json:"removed,omitempty"`
	Failed   []string `json:"failed,omitempty"`
	// NotGranted is the generated topics this store's grant cannot hold, so
	// they were not asked for. A standing fact about the connection, like a
	// domain's `not granted` phase, rather than a failure.
	NotGranted []string `json:"notGranted,omitempty"`
	// DeliveredAtVersion is the API version Shopify serializes this store's
	// events at, when it is not the pinned one. Empty when it is the pin, or
	// when the store holds no subscription of ours to read it from.
	DeliveredAtVersion string    `json:"deliveredAtVersion,omitempty"`
	At                 time.Time `json:"at"`
}

// EnsureSubscriptions implements sync.Connector: brings every configured
// store's subscription set in line with the generated one.
func (c *Connector) EnsureSubscriptions(ctx context.Context) error {
	stores, err := c.stores.Stores(ctx)
	if err != nil {
		return err
	}
	var failures []string
	for _, store := range stores {
		if !store.Ingests() {
			continue
		}
		if _, err := c.subscriptionPass(ctx, store); err != nil {
			failures = append(failures, store.ID+": "+err.Error())
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("shopify: subscription reconcile failed for %d store(s): %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}

// subscriptionPass reconciles one store's subscriptions and records the pass
// on its health, and returns the error of a pass that could not read the
// store's subscriptions at all. That pass is recorded too, as a failure: a
// read that failed and a pass nobody recorded look the same on the store's
// health otherwise, and the verdict reads it (events_not_registered). The
// daily pass and Register again run through here; the connect's pass
// (registerConnectWebhooks) records the same failure in the same words.
func (c *Connector) subscriptionPass(ctx context.Context, store Store) (SubscriptionReport, error) {
	report, err := c.EnsureSubscriptionsForStore(ctx, store)
	if err != nil {
		c.logger.Warn("shopify: could not read the store's webhook subscriptions", "store", store.ID, "error", err.Error())
		report.Failed = append(report.Failed, subscriptionsUnreadable)
	}
	if recErr := c.recordSubscriptionHealth(ctx, store, report); recErr != nil {
		c.logger.Warn("shopify: could not record subscription health", "store", store.ID, "error", recErr.Error())
	}
	return report, err
}

// EnsureSubscriptionsForStore reconciles one store.
func (c *Connector) EnsureSubscriptionsForStore(ctx context.Context, store Store) (SubscriptionReport, error) {
	report := SubscriptionReport{StoreID: store.ID, At: c.now().UTC()}
	callback := c.deliveryURL(store)
	version := generated.APIVersion

	existing, err := c.listSubscriptions(ctx, store)
	if err != nil {
		return report, err
	}
	report.Existing = len(existing)

	// THE GRANT DECIDES WHAT IS ASKED FOR (memql#5638). A store connected
	// through Connect Shopify holds the managed scopes, and most generated
	// topics route to concepts none of them reads: Shopify refuses each such
	// creation, the refusal lands on the store's health, and the daily pass
	// asks again. A topic left out here is also one this store does not want,
	// so a subscription of ours for it is removed below like any other.
	desired := map[string]bool{}
	for _, topic := range generated.SubscribedTopics {
		if len(topicScopesMissingFor(store, topic)) > 0 {
			report.NotGranted = append(report.NotGranted, topic)
			continue
		}
		desired[topic] = true
	}
	report.Desired = len(desired)

	byTopic := map[string]Subscription{}
	for _, sub := range existing {
		byTopic[sub.Topic] = sub
		// Every subscription of ours carries the app's version, so the first
		// that is not the pin says what Shopify delivers at. Reported rather
		// than rewritten: an update cannot change it, and the place that can
		// is the app's configuration at Shopify.
		if report.DeliveredAtVersion == "" && sub.APIVersion != "" && sub.APIVersion != version {
			report.DeliveredAtVersion = sub.APIVersion
		}
	}

	topics := make([]string, 0, len(desired))
	for t := range desired {
		topics = append(topics, t)
	}
	sort.Strings(topics)

	for _, topic := range topics {
		sub, present := byTopic[topic]
		switch {
		case !present:
			if err := c.createSubscription(ctx, store, topic, callback); err != nil {
				report.Failed = append(report.Failed, topic+": "+err.Error())
				continue
			}
			report.Created = append(report.Created, topic)
		case sub.CallbackURL != callback || !sameFields(sub.IncludeFields, includeFields):
			if err := c.updateSubscription(ctx, store, sub.ID, callback); err != nil {
				report.Failed = append(report.Failed, topic+": "+err.Error())
				continue
			}
			report.Updated = append(report.Updated, topic)
		}
	}

	// Remove OURS that we no longer want -- a topic dropped from the
	// allowlist. Another app's subscriptions are not visible through this
	// app's token at all, but the callback check is kept anyway: it is the
	// difference between "we tidy up after ourselves" and "we delete
	// whatever we can see", and only the first is safe to run daily.
	for _, sub := range existing {
		if desired[sub.Topic] || sub.CallbackURL != callback {
			continue
		}
		if err := c.deleteSubscription(ctx, store, sub.ID); err != nil {
			report.Failed = append(report.Failed, sub.Topic+": "+err.Error())
			continue
		}
		report.Removed = append(report.Removed, sub.Topic)
	}
	return report, nil
}

func (c *Connector) listSubscriptions(ctx context.Context, store Store) ([]Subscription, error) {
	var out []Subscription
	after := ""
	for {
		vars := map[string]any{"first": subscriptionPageSize}
		if after != "" {
			vars["after"] = after
		}
		resp, err := c.adminCall(ctx, store, webhookListQuery, "ShopifyWebhookSubscriptions", vars)
		if err != nil {
			return nil, err
		}
		var decoded struct {
			WebhookSubscriptions struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []struct {
					ID            string   `json:"id"`
					Topic         string   `json:"topic"`
					IncludeFields []string `json:"includeFields"`
					APIVersion    struct {
						Handle string `json:"handle"`
					} `json:"apiVersion"`
					URI string `json:"uri"`
				} `json:"nodes"`
			} `json:"webhookSubscriptions"`
		}
		if err := resp.DecodeInto(&decoded); err != nil {
			return nil, err
		}
		for _, n := range decoded.WebhookSubscriptions.Nodes {
			out = append(out, Subscription{
				ID:            n.ID,
				Topic:         n.Topic,
				CallbackURL:   n.URI,
				APIVersion:    n.APIVersion.Handle,
				IncludeFields: n.IncludeFields,
			})
		}
		if !decoded.WebhookSubscriptions.PageInfo.HasNextPage {
			return out, nil
		}
		after = decoded.WebhookSubscriptions.PageInfo.EndCursor
		if after == "" {
			return out, nil
		}
	}
}

func (c *Connector) createSubscription(ctx context.Context, store Store, topic, callback string) error {
	resp, err := c.adminCall(ctx, store, webhookCreateMutation, "ShopifyWebhookCreate", map[string]any{
		"topic": topic,
		"sub":   subscriptionInput(callback),
	})
	if err != nil {
		return err
	}
	return userErrorsFrom(resp, "webhookSubscriptionCreate")
}

func (c *Connector) updateSubscription(ctx context.Context, store Store, id, callback string) error {
	resp, err := c.adminCall(ctx, store, webhookUpdateMutation, "ShopifyWebhookUpdate", map[string]any{
		"id":  id,
		"sub": subscriptionInput(callback),
	})
	if err != nil {
		return err
	}
	return userErrorsFrom(resp, "webhookSubscriptionUpdate")
}

func (c *Connector) deleteSubscription(ctx context.Context, store Store, id string) error {
	resp, err := c.adminCall(ctx, store, webhookDeleteMutation, "ShopifyWebhookDelete", map[string]any{"id": id})
	if err != nil {
		return err
	}
	return userErrorsFrom(resp, "webhookSubscriptionDelete")
}

// subscriptionInput is the WebhookSubscriptionInput of a create or an update.
//
// ONLY FIELDS THE TYPE DEFINES. Shopify refuses the whole call for one it does
// not ("Field `apiVersion` is not defined by type `WebhookSubscriptionInput`"),
// and a refused creation is a store that is never told about a change. The
// version events are serialized at is not asked for here because it cannot be:
// a subscription inherits it from the app, whose configuration at Shopify names
// it. `uri` is the destination; `callbackUrl` is its deprecated spelling.
func subscriptionInput(callback string) map[string]any {
	return map[string]any{
		"uri":           callback,
		"format":        "JSON",
		"includeFields": includeFields,
	}
}

// userErrorsFrom surfaces Shopify's userErrors as a Go error.
//
// A GraphQL mutation that "succeeds" with userErrors is the trap of this API:
// the HTTP status is 200, the errors array is empty, and the failure is
// inside data. Code that only checks err would report a subscription created
// that was not.
func userErrorsFrom(resp *AdminResponse, field string) error {
	var decoded map[string]struct {
		UserErrors []struct {
			Field   []string `json:"field"`
			Message string   `json:"message"`
		} `json:"userErrors"`
	}
	if err := resp.DecodeInto(&decoded); err != nil {
		return nil // no userErrors shape: nothing to report
	}
	payload, ok := decoded[field]
	if !ok || len(payload.UserErrors) == 0 {
		return nil
	}
	parts := make([]string, 0, len(payload.UserErrors))
	for _, ue := range payload.UserErrors {
		if len(ue.Field) > 0 {
			parts = append(parts, strings.Join(ue.Field, ".")+": "+ue.Message)
			continue
		}
		parts = append(parts, ue.Message)
	}
	return &UserError{Field: field, Messages: parts}
}

// UserError is a typed Shopify validation failure. It is separate from
// AdminError because the two need OPPOSITE handling: an AdminError may be a
// throttle worth retrying, while a userError is a statement that the request
// as sent will never be accepted -- so the push path dead-letters it rather
// than consuming the queue with retries.
type UserError struct {
	Field    string
	Messages []string
}

func (e *UserError) Error() string {
	return "shopify: " + e.Field + " userErrors: " + strings.Join(e.Messages, "; ")
}

func sameFields(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// recordSubscriptionHealth folds a reconcile report into the store's health,
// as its `subscriptions` key.
func (c *Connector) recordSubscriptionHealth(ctx context.Context, store Store, report SubscriptionReport) error {
	subscriptions := map[string]any{
		"desired":    report.Desired,
		"existing":   report.Existing,
		"created":    toAny(report.Created),
		"updated":    toAny(report.Updated),
		"removed":    toAny(report.Removed),
		"failed":     toAny(report.Failed),
		"notGranted": toAny(report.NotGranted),
		"at":         report.At.Format(time.RFC3339),
	}
	if report.DeliveredAtVersion != "" {
		subscriptions["deliveredAtVersion"] = report.DeliveredAtVersion
	}
	return c.recordStoreHealthKey(ctx, store.ID, "subscriptions", subscriptions,
		map[string]any{"subscriptionsCheckedAt": report.At.Format(time.RFC3339)})
}

// recordStoreHealthKey writes ONE key of a store's health: it reads the store
// row fresh and writes the health back with that key replaced, and every other
// key as the row holds it. extra carries the mutation's other arguments.
//
// recordStoreHealth replaces the whole object, and each writer used to send
// the health of the copy of the store it held. The connect's webhook pass
// holds the store as it was before the connect, so whatever another writer
// added meanwhile -- the ordersFrom stamp, a verdict -- was erased by the
// next pass. Every writer of a health key goes through here instead.
//
// THE WRITE IS FENCED ON THE VERSION IT READ. The read and the write are two
// statements, and the writers run at once: the watch, the subscription pass
// and a connect each write their own key of one row. A write of another key
// that landed between this writer's read and its write was put back as this
// writer had read it. For the watch's key that is an older verdict, whose
// mailedFor says the episode was never mailed, and its owner was mailed
// again. So the write carries a row-version fence on the version the read
// answered (memql.ContextWithRowVersionFence); one another writer overtook is
// refused, and this writer reads again and re-applies ITS OWN KEY ONLY, at
// most maxHealthKeyAttempts times -- the pattern of the sync runtime's
// PatchSyncState. A store with no row yet is fenced on its absence.
func (c *Connector) recordStoreHealthKey(ctx context.Context, storeID, key string, value any, extra map[string]any) error {
	return c.recordStoreHealthKeys(ctx, storeID, map[string]any{key: value}, nil, extra)
}

// recordStoreHealthKeys is recordStoreHealthKey for several keys in one fenced
// write: each key of set replaced, each key of drop removed, every other key as
// the row holds it.
func (c *Connector) recordStoreHealthKeys(ctx context.Context, storeID string, set map[string]any, drop []string, extra map[string]any) error {
	opCtx := operatorContext(ctx)
	for attempt := 0; attempt < maxHealthKeyAttempts; attempt++ {
		if attempt > 0 {
			healthKeyRetryWait(attempt)
		}
		held, version, err := c.storeHealthForWrite(opCtx, storeID)
		if err != nil {
			return err
		}
		health := map[string]any{}
		for k, v := range held {
			health[k] = v
		}
		for k, v := range set {
			health[k] = v
		}
		for _, k := range drop {
			delete(health, k)
		}
		args := map[string]any{"storeId": storeID, "health": health}
		for k, v := range extra {
			args[k] = v
		}
		fenced := memql.ContextWithRowVersionFence(opCtx, conceptShopifyStore, storeID, version)
		_, err = c.engine.Execute(fenced, renderCall("recordStoreHealth", args))
		if errors.Is(err, memql.ErrRowVersionChanged) {
			continue
		}
		if err != nil {
			return err
		}
		c.stores.Invalidate()
		return nil
	}
	keys := make([]string, 0, len(set)+len(drop))
	for k := range set {
		keys = append(keys, k)
	}
	keys = append(keys, drop...)
	sort.Strings(keys)
	return fmt.Errorf("shopify: the row of store %q kept changing under %d writes of its health key %q; nothing was written", storeID, maxHealthKeyAttempts, strings.Join(keys, ", "))
}

// storeHealthForWrite reads a store's health fresh, with the version of the
// row it was read from: the witness a write decided on it is fenced on. No
// row answers no health and the zero version.
func (c *Connector) storeHealthForWrite(ctx context.Context, storeID string) (map[string]any, time.Time, error) {
	res, err := c.engine.Execute(memql.ContextWithFreshRead(ctx), renderCall("storeById", map[string]any{"storeId": storeID}))
	if err != nil {
		return nil, time.Time{}, err
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, time.Time{}, nil
	}
	health, _ := rowValue(rows[0], "health").(map[string]any)
	return health, rowTime(rows[0], "createdAt"), nil
}

// healthKeyRetryWait is the pause before a health-key write that lost its
// fence is decided again: a few milliseconds, longer each time and jittered,
// so two writers that lost to each other do not decide again in step. The
// four waits of one write add up to less than a tenth of a second.
func healthKeyRetryWait(attempt int) {
	base := 2 * time.Millisecond << (attempt - 1)
	time.Sleep(base + time.Duration(rand.Int64N(int64(base))))
}

// maxHealthKeyAttempts bounds how often a write of one health key that another
// writer of the store row overtook is decided again.
const maxHealthKeyAttempts = 5

func toAny(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}
