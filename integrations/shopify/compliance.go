package shopify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// compliance.go -- the three mandatory privacy topics.
//
// Implemented regardless of app type (decision D6). No Shopify page exempts a
// custom-distribution app explicitly, and the client's own obligations to its
// customers do not depend on Shopify's app-review rules either way. An
// unimplemented compliance topic is a legal exposure that presents as nothing
// at all until somebody asks.
//
// REDACTION IS THE ONE WRITE TO A MIRROR THAT IS NOT AN APPLY. Everything
// else in this connector converges the mirror onto what the origin says;
// redaction deliberately makes the mirror DIFFER from the origin's history,
// by rewriting rows the origin has already forgotten. It is the only path
// allowed to rewrite history, it runs under the connector actor, and it goes
// through raw SQL rather than the engine because the engine's model is
// append-only by design and "every version" is exactly what it will not let
// you touch.

// Compliance topic header spellings, as they arrive.
const (
	TopicDataRequest = generated.TopicCustomersDataRequest
	TopicRedact      = generated.TopicCustomersRedact
	TopicShopRedact  = generated.TopicShopRedact
)

// ShopRedactHold is how long a shop/redact purge waits.
//
// Shopify sends shop/redact 48 hours after an uninstall, and the uninstall
// itself may be an accident an operator reverses. Purging on receipt would
// make a mis-click unrecoverable; waiting the hold and re-checking whether the
// store came back is the difference between compliance and data loss.
const ShopRedactHold = 48 * time.Hour

// PII field names scrubbed by customers/redact.
//
// A DENYLIST, and it is worth being honest about why rather than pretending
// it is a schema-derived set. The mirror is generated from Shopify's schema,
// which carries no PII marking, so there is nothing to derive from. What
// makes the list adequate is its scope: it is applied to rows REFERENCING ONE
// CUSTOMER, so a name field that is missed leaves one person's name behind
// rather than everyone's -- and the list is checked against the generated
// concepts by TestRedactionCoversEveryCustomerNameField, which fails when a
// regeneration introduces a field that looks like PII and is not here.
var piiFieldNames = []string{
	"email", "phone", "firstName", "lastName", "displayName", "name",
	"note", "billingAddress", "shippingAddress", "defaultAddress",
	"addresses", "addressesV2", "customerLocale", "clientIp",
	"emailMarketingConsent", "smsMarketingConsent", "whatsAppMarketingConsent",
	"contactEmail", "verifiedEmail", "multipassIdentifier", "taxExemptions",
	"unsubscribeUrl", "customerJourneySummary",
}

// RedactionMarker replaces a scrubbed value. A marker rather than an empty
// string, so a reader can tell "redacted" from "never had one" -- which is
// the difference between a compliant record and a corrupted one.
const RedactionMarker = "[redacted]"

// ComplianceJob is one queued privacy request.
type ComplianceJob struct {
	ID          string         `json:"id"`
	Topic       string         `json:"topic"`
	StoreID     string         `json:"storeId"`
	ShopDomain  string         `json:"shopDomain"`
	CustomerGID string         `json:"customerGid,omitempty"`
	RequestID   string         `json:"requestId,omitempty"`
	OrderGIDs   []string       `json:"orderGids,omitempty"`
	ReceivedAt  time.Time      `json:"receivedAt"`
	DueAt       time.Time      `json:"dueAt"`
	Raw         map[string]any `json:"-"`
}

// enqueueComplianceJob parses a compliance delivery and records it.
//
// It does NOT do the work inline. customers/redact has a 30-day window
// and shop/redact a 48-hour hold, and the receiver has five seconds -- so
// the delivery becomes a job and the job runs on a schedule. That is also
// what lets a merchant's own grace period apply if one is configured.
func (c *Connector) enqueueComplianceJob(ctx context.Context, store Store, topic string, req memqlsync.InboundRequest) error {
	job, err := parseComplianceJob(topic, store, req, c.now())
	if err != nil {
		return err
	}
	job.ID = ComplianceJobID(store.ID, job.Topic, job.CustomerGID+"\x00"+job.RequestID)
	call := renderCall("queueComplianceJob", map[string]any{
		"jobId":       job.ID,
		"storeId":     store.ID,
		"topic":       job.Topic,
		"customerGid": job.CustomerGID,
		"requestId":   job.RequestID,
		"shopDomain":  job.ShopDomain,
		"dueAt":       job.DueAt.UTC().Format(time.RFC3339),
	})
	if _, err := c.engine.Execute(connectorContext(ctx), call); err != nil {
		return fmt.Errorf("shopify: queue %s: %w", topic, err)
	}
	c.logger.Info("shopify: compliance request queued", "topic", topic, "store", store.ID, "dueAt", job.DueAt.Format(time.RFC3339))
	return c.auditCompliance(ctx, store, complianceAuditAction(topic)+"_received", job)
}

// ComplianceJobID derives the row id from (store, topic, subject).
//
// Deterministic, so Shopify re-sending the same request lands on the same
// row rather than queueing the work twice -- and @createOnly on the
// mutation's lifecycle fields keeps a redelivery from resetting a status
// the runner has already moved.
func ComplianceJobID(storeID, topic, subject string) string {
	return "shpcj" + string(idEngine.FromString(storeID+"\x00"+topic+"\x00"+subject))[:24]
}

// RunDueComplianceJobs runs every queued privacy job whose hold has
// elapsed.
func (c *Connector) RunDueComplianceJobs(ctx context.Context) (int, error) {
	res, err := c.engine.Execute(connectorContext(ctx), renderCall("complianceJobsDue", map[string]any{
		"asOf": c.now().UTC().Format(time.RFC3339),
	}))
	if err != nil {
		return 0, fmt.Errorf("shopify: read due compliance jobs: %w", err)
	}
	ran := 0
	for _, row := range memql.MaterializeRows(res) {
		job := ComplianceJob{
			ID:          mapString(row, "id"),
			Topic:       mapString(row, "topic"),
			StoreID:     mapString(row, "storeId"),
			CustomerGID: mapString(row, "customerGid"),
			RequestID:   mapString(row, "requestId"),
			ShopDomain:  mapString(row, "shopDomain"),
		}
		if job.ID == "" {
			// The default projection is payload-only, so the row id is
			// not in it; recompute it from the fields that derived it.
			job.ID = ComplianceJobID(job.StoreID, job.Topic, job.CustomerGID+"\x00"+job.RequestID)
		}
		store, ok := c.stores.ByID(ctx, job.StoreID)
		if !ok {
			c.recordJob(ctx, job.ID, "failed", "", "store "+job.StoreID+" is no longer configured")
			continue
		}
		outcome, err := c.runComplianceJob(ctx, store, job)
		if err != nil {
			c.logger.Error("shopify: compliance job failed", "topic", job.Topic, "store", store.ID, "error", err)
			c.recordJob(ctx, job.ID, "failed", "", err.Error())
			continue
		}
		ran++
		c.recordJob(ctx, job.ID, "done", outcome, "")
	}
	return ran, nil
}

func (c *Connector) recordJob(ctx context.Context, jobID, status, outcome, lastError string) {
	call := renderCall("recordComplianceJob", map[string]any{
		"jobId": jobID, "status": status, "outcome": outcome, "lastError": lastError,
	})
	if _, err := c.engine.Execute(connectorContext(ctx), call); err != nil {
		c.logger.Warn("shopify: could not record a compliance job's outcome", "job", jobID, "error", err)
	}
}

// runComplianceJob performs one privacy request and reports what it did.
func (c *Connector) runComplianceJob(ctx context.Context, store Store, job ComplianceJob) (string, error) {
	switch job.Topic {
	case TopicDataRequest:
		export, err := c.ExportCustomerData(ctx, store, job.CustomerGID)
		if err != nil {
			return "", err
		}
		artifact, err := json.MarshalIndent(export, "", "  ")
		if err != nil {
			return "", err
		}
		if err := c.writeExportArtifact(ctx, store, job, artifact); err != nil {
			return "", err
		}
		return fmt.Sprintf("exported %d bytes", len(artifact)), c.auditCompliance(ctx, store, "shopify_data_request_exported", job)

	case TopicRedact:
		rows, err := c.RedactCustomer(ctx, store, job.CustomerGID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("scrubbed %d row version(s)", rows), c.auditCompliance(ctx, store, "shopify_customer_redacted", job)

	case TopicShopRedact:
		// Re-check the store before purging: an uninstall an operator
		// reverses within the hold must not cost the mirror.
		fresh, ok := c.stores.ByID(ctx, store.ID)
		if ok && fresh.Ingests() && fresh.RedactedAt == "" && c.storeReachable(ctx, fresh) {
			c.logger.Info("shopify: skipping shop/redact purge, the store is reachable again", "store", store.ID)
			return "skipped: the store is reachable again", c.auditCompliance(ctx, store, "shopify_shop_redact_skipped", job)
		}
		rows, err := c.PurgeStore(ctx, store)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("purged %d row version(s)", rows), c.auditCompliance(ctx, store, "shopify_shop_redacted", job)
	}
	return "", fmt.Errorf("shopify: %q is not a compliance topic", job.Topic)
}

// storeReachable asks the store a trivial question. Used only to decide
// whether an uninstall was reverted before the shop/redact hold elapsed.
func (c *Connector) storeReachable(ctx context.Context, store Store) bool {
	_, err := c.adminCall(ctx, store, `query ShopifyReachable { shop { id } }`, "ShopifyReachable", nil)
	return err == nil
}

// writeExportArtifact puts the data-request export in the Library, under
// the store's owner, and records the Shopify request id with it.
func (c *Connector) writeExportArtifact(ctx context.Context, store Store, job ComplianceJob, body []byte) error {
	name := fmt.Sprintf("shopify-data-request-%s-%s.json", store.ID, job.RequestID)
	if job.RequestID == "" {
		name = fmt.Sprintf("shopify-data-request-%s-%s.json", store.ID, job.ReceivedAt.Format("20060102T150405Z"))
	}
	if store.OwnerUserID == "" {
		// Refused rather than filed somewhere. The export is a document
		// about a real person and the Library is owner-tier; the
		// connector actor is not a person, and picking a cluster owner
		// would file one merchant's customer data in a stranger's
		// library.
		return fmt.Errorf("shopify: store %q has no ownerUserId, so a data-request export has nobody to belong to -- set it on the store row", store.ID)
	}
	// The owner's identity is BORROWED for this write, exactly as the
	// campaigns worker borrows a campaign owner's: createGeneratedOutput
	// stamps ownerUserId from actor.userId, so the row would otherwise
	// belong to the connector.
	ownerCtx := auth.ContextWithUserActor(ctx, store.OwnerUserID)
	call := renderCall("createGeneratedOutput", map[string]any{
		"outputId": "shpdr" + MirrorRowID(store.ID, name),
		"title":    name,
		"summary":  fmt.Sprintf("Shopify customers/data_request export for %s (request %s).", job.CustomerGID, job.RequestID),
		"body":     string(body),
		"format":   "text",
		"mimeType": "application/json",
		"source":   "derived",
	})
	if _, err := c.engine.Execute(ownerCtx, call); err != nil {
		return fmt.Errorf("shopify: write export artifact: %w", err)
	}
	return nil
}

func parseComplianceJob(topic string, store Store, req memqlsync.InboundRequest, now time.Time) (ComplianceJob, error) {
	obj, err := decodeJSONObject(string(req.Body))
	if err != nil {
		return ComplianceJob{}, fmt.Errorf("shopify: %s delivery is not JSON", topic)
	}
	job := ComplianceJob{
		Topic:      strings.ToLower(strings.TrimSpace(topic)),
		StoreID:    store.ID,
		ShopDomain: firstString(obj, "shop_domain"),
		ReceivedAt: req.ReceivedAt.UTC(),
		Raw:        obj,
	}
	if job.ReceivedAt.IsZero() {
		job.ReceivedAt = now.UTC()
	}
	if cust, ok := obj["customer"].(map[string]any); ok {
		if id := firstString(cust, "id"); id != "" {
			job.CustomerGID = "gid://shopify/Customer/" + strings.TrimPrefix(id, "gid://shopify/Customer/")
		}
	}
	job.RequestID = firstString(obj, "data_request", "id")
	if dr, ok := obj["data_request"].(map[string]any); ok {
		job.RequestID = firstString(dr, "id")
	}
	if raw, ok := obj["orders_requested"].([]any); ok {
		for _, v := range raw {
			if s := fmt.Sprintf("%v", v); s != "" {
				job.OrderGIDs = append(job.OrderGIDs, "gid://shopify/Order/"+strings.TrimPrefix(s, "gid://shopify/Order/"))
			}
		}
	}
	switch job.Topic {
	case TopicShopRedact:
		job.DueAt = job.ReceivedAt.Add(ShopRedactHold)
	case TopicRedact:
		// Scheduled rather than immediate: the 30-day window is the
		// obligation, and running on receipt would override a merchant's
		// own grace period if one is configured.
		job.DueAt = job.ReceivedAt.Add(24 * time.Hour)
	default:
		job.DueAt = job.ReceivedAt
	}
	return job, nil
}

func complianceAuditAction(topic string) string {
	switch strings.ToLower(strings.TrimSpace(topic)) {
	case TopicDataRequest:
		return "shopify_data_request"
	case TopicRedact:
		return "shopify_customer_redact"
	case TopicShopRedact:
		return "shopify_shop_redact"
	}
	return "shopify_compliance"
}

// ExportCustomerData collects every mirror row referencing a customer.
//
// Its completeness comes from the generated model rather than from a
// hand-written list of places customers appear: every mirrored concept is
// walked, and a row counts when any of its fields carries the customer's GID.
// A hand-written list would be correct on the day it was written and wrong
// after the next allowlist change, which for a legal obligation is the wrong
// kind of wrong.
func (c *Connector) ExportCustomerData(ctx context.Context, store Store, customerGID string) (map[string]any, error) {
	if customerGID == "" {
		return nil, fmt.Errorf("shopify: data request carried no customer id")
	}
	actorCtx := connectorContext(ctx)
	collected := map[string]any{}
	concepts := append([]string(nil), generated.ApplyOrder...)
	sort.Strings(concepts)
	for _, concept := range concepts {
		spec := generated.Types[concept]
		if spec == nil {
			continue
		}
		res, err := c.engine.Execute(actorCtx, renderCall(spec.ForStoreFn, map[string]any{"storeId": store.ID}))
		if err != nil {
			return nil, fmt.Errorf("shopify: export %s: %w", concept, err)
		}
		var matched []map[string]any
		for _, row := range memql.MaterializeRows(res) {
			if rowMentions(row, customerGID) {
				matched = append(matched, row)
			}
		}
		if len(matched) > 0 {
			collected[concept] = matched
		}
	}
	return map[string]any{
		"store":       store.Domain,
		"customerGid": customerGID,
		"exportedAt":  c.now().UTC().Format(time.RFC3339),
		"apiVersion":  generated.APIVersion,
		"rows":        collected,
	}, nil
}

// rowMentions reports whether any value anywhere in a row equals the GID.
// Deep rather than top-level: a customer appears on an order as customerGid,
// inside a nested billingAddress, and inside a metafield value, and a
// shallow check would miss two of the three.
func rowMentions(v any, gid string) bool {
	switch val := v.(type) {
	case string:
		return val == gid
	case map[string]any:
		for _, item := range val {
			if rowMentions(item, gid) {
				return true
			}
		}
	case []any:
		for _, item := range val {
			if rowMentions(item, gid) {
				return true
			}
		}
	}
	return false
}

// RedactCustomer scrubs a customer's PII from every version of every mirror
// row that references them, keeping the opaque GID and the commercial facts.
//
// WHAT SURVIVES IS THE POINT. An order's total, its line quantities and its
// dates stay: the merchant's books are not the customer's personal data, and
// destroying them would be a different compliance failure. What goes is the
// name, the addresses, the contact details and the consent records.
func (c *Connector) RedactCustomer(ctx context.Context, store Store, customerGID string) (int64, error) {
	if c.db == nil {
		return 0, fmt.Errorf("shopify: customers/redact needs a database handle")
	}
	if customerGID == "" {
		return 0, fmt.Errorf("shopify: redact carried no customer id")
	}
	db := c.db()
	if db == nil {
		return 0, fmt.Errorf("shopify: customers/redact needs a database handle")
	}
	var total int64
	for _, concept := range generated.ApplyOrder {
		n, err := redactConcept(ctx, db, generated.ConceptID(concept), store.ID, customerGID)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// redactConcept rewrites the PII fields of every VERSION of every row of one
// concept that mentions the customer.
//
// staged-data: MUST-NOT-GATE -- a staged row skipped here KEEPS ITS PII. This
// is customers/redact: the obligation is that the person's data is scrubbed
// wherever it sits, and a visibility gate would leave the one copy nobody can
// see -- which is the copy that turns up in a subject-access request later.
//
// Every version, and that is the requirement rather than thoroughness for its
// own sake: MemoryNodes is keyed (id, createdAt), so scrubbing the newest
// version alone leaves the customer's name in the row's history, where a
// point-in-time read still returns it.
func redactConcept(ctx context.Context, db *sql.DB, concept, storeID, customerGID string) (int64, error) {
	sets := make([]string, 0, len(piiFieldNames))
	args := []any{concept, storeID, customerGID}
	for i, field := range piiFieldNames {
		sets = append(sets, fmt.Sprintf("jsonb_set(payload, $%d::text[], to_jsonb($%d::text), false)", 2*i+4, 2*i+5))
		args = append(args, "{"+field+"}", RedactionMarker)
	}
	// Applied left to right: each jsonb_set wraps the previous, and
	// create_missing=false means a field the concept does not carry is left
	// alone rather than invented.
	expr := "payload"
	for _, s := range sets {
		expr = strings.Replace(s, "payload", expr, 1)
	}
	stmt := fmt.Sprintf(`UPDATE "MemoryNodes" SET payload = %s
	  WHERE concept = $1
	    AND payload->>'storeId' = $2
	    AND payload::text LIKE '%%' || $3 || '%%'`, expr)
	res, err := db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, fmt.Errorf("shopify: redact %s: %w", concept, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PurgeStore deletes a store's whole mirror and its sync state.
//
// Runs after the hold, and only when the store has not come back: an
// uninstall an operator reverses within 48 hours must not cost the mirror.
//
// staged-data: MUST-NOT-GATE -- a staged row skipped here SURVIVES A PURGE.
// This is shop/redact: the obligation is that the store's data is gone, and a
// row the delete could not see is a row that stays in the table forever with
// nothing left to find it (the store row is paused, the sync state is
// deleted, and no later pass looks at this concept for this store again). The
// same argument component/identity/authactivity's prune makes, with a legal
// deadline attached rather than a retention window.
func (c *Connector) PurgeStore(ctx context.Context, store Store) (int64, error) {
	if c.db == nil || c.db() == nil {
		return 0, fmt.Errorf("shopify: shop/redact needs a database handle")
	}
	db := c.db()
	var total int64
	for _, concept := range generated.ApplyOrder {
		res, err := db.ExecContext(ctx,
			`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'storeId' = $2`,
			generated.ConceptID(concept), store.ID)
		if err != nil {
			return total, fmt.Errorf("shopify: purge %s: %w", concept, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	// The runtime's health rows for this connector's domains go too.
	// Leaving them would have a re-install resume a backfill cursor into
	// a mirror that no longer has the rows the cursor counted.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'connector' = $1 AND payload->>'storeId' = $2`,
		ConnectorName, store.ID); err != nil {
		return total, fmt.Errorf("shopify: purge sync state: %w", err)
	}
	stamp := renderCall("markStoreRedacted", map[string]any{
		"storeId":    store.ID,
		"redactedAt": c.now().UTC().Format(time.RFC3339),
	})
	if _, err := c.engine.Execute(connectorContext(ctx), stamp); err != nil {
		return total, fmt.Errorf("shopify: stamp store redaction: %w", err)
	}
	c.stores.Invalidate()
	return total, nil
}

// auditCompliance records a privacy action on the audit log.
//
// Every compliance action is audited, including the ones that only queue
// work: the obligation is to be able to SHOW what was done and when, and a
// job that ran leaving no trace is indistinguishable from one that did not.
func (c *Connector) auditCompliance(ctx context.Context, store Store, action string, job ComplianceJob) error {
	details := map[string]any{
		"topic":      job.Topic,
		"storeId":    store.ID,
		"shopDomain": job.ShopDomain,
		"dueAt":      job.DueAt.Format(time.RFC3339),
	}
	if job.CustomerGID != "" {
		details["customerGid"] = job.CustomerGID
	}
	if job.RequestID != "" {
		details["requestId"] = job.RequestID
	}
	call := renderCall("createAuditEvent", map[string]any{
		"eventId":     "aud" + MirrorRowID(store.ID, action+"\x00"+job.CustomerGID+"\x00"+job.RequestID+"\x00"+job.ReceivedAt.Format(time.RFC3339)),
		"occurredAt":  c.now().UTC().Format(time.RFC3339),
		"category":    "data",
		"action":      action,
		"actorUserId": "system:connector:" + ConnectorName,
		"targetType":  "shopifyStore",
		"targetId":    store.ID,
		"detail":      details,
		"outcome":     "success",
	})
	if _, err := c.engine.Execute(connectorContext(ctx), call); err != nil {
		// An audit write that fails must not swallow the compliance
		// action, but it must be loud: the trail is the deliverable.
		c.logger.Error("shopify: could not write compliance audit event", "action", action, "store", store.ID, "error", err)
		return nil
	}
	return nil
}
