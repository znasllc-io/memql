package shopify

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

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
      endpoint {
        __typename
        ... on WebhookHttpEndpoint { callbackUrl }
      }
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
	StoreID  string    `json:"storeId"`
	Desired  int       `json:"desired"`
	Existing int       `json:"existing"`
	Created  []string  `json:"created,omitempty"`
	Updated  []string  `json:"updated,omitempty"`
	Removed  []string  `json:"removed,omitempty"`
	Failed   []string  `json:"failed,omitempty"`
	At       time.Time `json:"at"`
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
		report, err := c.EnsureSubscriptionsForStore(ctx, store)
		if err != nil {
			failures = append(failures, store.ID+": "+err.Error())
			continue
		}
		if err := c.recordSubscriptionHealth(ctx, store, report); err != nil {
			c.logger.Warn("shopify: could not record subscription health", "store", store.ID, "error", err)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("shopify: subscription reconcile failed for %d store(s): %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
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

	desired := map[string]bool{}
	for _, topic := range generated.SubscribedTopics {
		desired[topic] = true
	}
	report.Desired = len(desired)

	byTopic := map[string]Subscription{}
	for _, sub := range existing {
		byTopic[sub.Topic] = sub
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
		case sub.CallbackURL != callback || sub.APIVersion != version || !sameFields(sub.IncludeFields, includeFields):
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
					Endpoint struct {
						Typename    string `json:"__typename"`
						CallbackURL string `json:"callbackUrl"`
					} `json:"endpoint"`
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
				CallbackURL:   n.Endpoint.CallbackURL,
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

func subscriptionInput(callback string) map[string]any {
	return map[string]any{
		"callbackUrl":   callback,
		"format":        "JSON",
		"includeFields": includeFields,
		"apiVersion":    generated.APIVersion,
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

// recordSubscriptionHealth folds a reconcile report into the store's health.
func (c *Connector) recordSubscriptionHealth(ctx context.Context, store Store, report SubscriptionReport) error {
	health := map[string]any{}
	for k, v := range store.Health {
		health[k] = v
	}
	health["subscriptions"] = map[string]any{
		"desired":  report.Desired,
		"existing": report.Existing,
		"created":  toAny(report.Created),
		"updated":  toAny(report.Updated),
		"removed":  toAny(report.Removed),
		"failed":   toAny(report.Failed),
		"at":       report.At.Format(time.RFC3339),
	}
	call := renderCall("recordStoreHealth", map[string]any{
		"storeId":                store.ID,
		"health":                 health,
		"subscriptionsCheckedAt": report.At.Format(time.RFC3339),
	})
	if _, err := c.engine.Execute(connectorContext(ctx), call); err != nil {
		return err
	}
	c.stores.Invalidate()
	return nil
}

func toAny(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}
