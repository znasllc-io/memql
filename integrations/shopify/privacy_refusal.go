package shopify

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/frontdoor"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// privacy_refusal.go -- a privacy delivery refused on a managed store's
// per-store URL, made impossible to miss (memql#5707 residual).
//
// Before memql#5707 the runbook pointed the three compliance topics at the
// per-store URL /inbound/shopify-<storeId>. Since then a store installed
// through the managed app has its privacy topics accepted ONLY on the
// app-level source (apply.go says why), so a cluster configured by the old
// runbook refuses every mandatory privacy delivery -- and Shopify, having had
// its 202 from the receiver, never sends one again. The refusal is right; a
// refusal nobody sees is a legal request silently dropped. So each one is
// said four ways:
//
//   - the ERROR is the staged row's lastError (the dispatcher stamps it), and
//     it is the whole instruction: the exact app-level URL to configure;
//   - the same sentence is logged at ERROR, so the log store has it;
//   - it is AUDITED, blocked, on the store -- a refused privacy request is a
//     decision about somebody's personal data, which is what the audit log
//     records;
//   - storeHealth COUNTS those audit events, and the Store panel draws them.
//
// The count is read from the audit trail rather than kept on the store row.
// A counter on the row would be a read-modify-write across replicas racing
// recordSubscriptionHealth's copy of the same health map; the audit trail is
// append-only, shared by every node, and already the record of what happened.

// privacyRefusalAction is the audit action a refused per-store privacy
// delivery is recorded under, and the one storeHealth counts.
const privacyRefusalAction = "shopify_privacy_delivery_refused"

// privacyRefusalReason is the audit event's failureReason.
const privacyRefusalReason = "privacy_topic_on_store_url"

// privacyRefusalMaxPages bounds the audit walk behind the health count. A page
// is auditEventsByTarget's 50; a store's trail is a few events per privacy
// request, so one page is the ordinary case and the cap is reported, never
// silently applied.
const privacyRefusalMaxPages = 10

// appLevelDeliveryURL is where Shopify must send this app's three privacy
// topics: the connector's own source, with no store id. Composed from
// MEMQL_DOMAIN through the same front-door helper as deliveryURL; a cluster
// with no domain says so in the URL rather than printing "https://api./...".
func appLevelDeliveryURL() string {
	domain := strings.TrimSpace(os.Getenv("MEMQL_DOMAIN"))
	if domain == "" {
		domain = "<your-domain>"
	}
	return "https://" + frontdoor.RoleHost(frontdoor.RoleAPI, domain) + "/inbound/" + ConnectorName
}

// refusePrivacyOnStoreURL logs, audits and returns the refusal of a privacy
// delivery that arrived on a managed store's per-store source.
func (c *Connector) refusePrivacyOnStoreURL(ctx context.Context, store Store, topic string, req memqlsync.InboundRequest) error {
	appURL := appLevelDeliveryURL()
	err := fmt.Errorf("shopify: %s refused on the per-store URL /inbound/%s -- this store was installed through "+
		"the managed app, whose privacy topics are accepted only at the app-level URL. In the app's configuration "+
		"at Shopify (Dev Dashboard or Partner Dashboard), set the customers/data_request, customers/redact and "+
		"shop/redact URLs to %s "+
		"(docs/public/operate/shopify-connector.md, Step 5). Shopify does not resend this request; it is on the "+
		"audit trail as %s", topic, req.Source, appURL, privacyRefusalAction)
	c.logger.Error("shopify: privacy delivery refused on a managed store's per-store URL",
		"store", store.ID, "topic", topic, "source", req.Source, "inboundRequestId", req.RequestId,
		"configure", appURL)

	// One event per STAGED DELIVERY: a row the dispatcher works twice is one
	// lost request, not two.
	subject := req.RequestId
	if subject == "" {
		subject = topic + "\x00" + req.ReceivedAt.UTC().Format(time.RFC3339Nano)
	}
	call := renderCall("createAuditEvent", map[string]any{
		"eventId":     "aud" + MirrorRowID(store.ID, privacyRefusalAction+"\x00"+subject),
		"occurredAt":  c.now().UTC().Format(time.RFC3339),
		"category":    "data",
		"action":      privacyRefusalAction,
		"actorUserId": "system:connector:" + ConnectorName,
		"targetType":  "shopifyStore",
		"targetId":    store.ID,
		"detail": map[string]any{
			"topic":            topic,
			"source":           req.Source,
			"inboundRequestId": req.RequestId,
			"appLevelUrl":      appURL,
		},
		"outcome":       "blocked",
		"failureReason": privacyRefusalReason,
	})
	if _, auditErr := c.engine.Execute(operatorContext(ctx), call); auditErr != nil {
		// The refusal stands either way; the row's lastError and the log line
		// above still say it. Loud, because the audit event is what the health
		// count reads.
		c.logger.Error("shopify: could not audit a refused privacy delivery", "store", store.ID, "topic", topic, "error", auditErr)
	}
	return err
}

// privacyDeliveries is storeHealth's figure for one store: how many privacy
// deliveries were refused on its per-store URL, the latest of them, and the
// URL that would have accepted them.
//
// A measured zero means the trail was read and holds none. `capped` says the
// walk stopped at privacyRefusalMaxPages with more trail behind it, so the
// count is a floor rather than the number.
func (c *Connector) privacyDeliveries(ctx context.Context, store Store) (map[string]any, error) {
	refused, last, capped := 0, "", false
	cursor := ""
	for page := 1; ; page++ {
		pageCtx := operatorContext(ctx)
		if cursor != "" {
			pageCtx = memql.ContextWithCursor(pageCtx, cursor)
		}
		res, err := c.engine.Execute(pageCtx, renderCall("auditEventsByTarget", map[string]any{"targetId": store.ID}))
		if err != nil {
			return nil, fmt.Errorf("shopify: read store %s's audit trail: %w", store.ID, err)
		}
		for _, row := range memql.MaterializeRows(res) {
			if mapString(row, "action") != privacyRefusalAction || mapString(row, "targetType") != "shopifyStore" {
				continue
			}
			refused++
			// occurredAt is written as RFC3339 in UTC, so the strings order
			// as the instants do.
			if at := mapString(row, "occurredAt"); at > last {
				last = at
			}
		}
		cursor = ""
		if meta := res.GetMeta(); meta != nil {
			cursor = strings.TrimSpace(meta.Cursor)
		}
		if cursor == "" {
			break
		}
		if page == privacyRefusalMaxPages {
			capped = true
			break
		}
	}
	return map[string]any{
		"refused":       refused,
		"lastRefusedAt": last,
		"capped":        capped,
		"appLevelUrl":   appLevelDeliveryURL(),
	}, nil
}
