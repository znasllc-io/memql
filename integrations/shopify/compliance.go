package shopify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
		"orderGids":   append([]string{}, job.OrderGIDs...),
		"dueAt":       job.DueAt.UTC().Format(time.RFC3339),
	})
	if _, err := c.engine.Execute(operatorContext(ctx), call); err != nil {
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
	return "shpcj" + string(idEngine.FromString(storeID + "\x00" + topic + "\x00" + subject))[:24]
}

// RunDueComplianceJobs runs every queued privacy job whose hold has
// elapsed.
func (c *Connector) RunDueComplianceJobs(ctx context.Context) (int, error) {
	res, err := c.engine.Execute(operatorContext(ctx), renderCall("complianceJobsDue", map[string]any{
		"asOf": c.now().UTC().Format(time.RFC3339),
	}))
	if err != nil {
		return 0, fmt.Errorf("shopify: read due compliance jobs: %w", err)
	}
	ran := 0
	for _, row := range memql.MaterializeRows(res) {
		job := complianceJobOf(row)
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
	if _, err := c.engine.Execute(operatorContext(ctx), call); err != nil {
		c.logger.Warn("shopify: could not record a compliance job's outcome", "job", jobID, "error", err)
	}
}

// runComplianceJob performs one privacy request and reports what it did.
func (c *Connector) runComplianceJob(ctx context.Context, store Store, job ComplianceJob) (string, error) {
	switch job.Topic {
	case TopicDataRequest:
		export, err := c.ExportCustomerData(ctx, store, job.CustomerGID, job.OrderGIDs)
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
		erased, err := c.RedactCustomer(ctx, store.ID, job.CustomerGID, job.OrderGIDs)
		if err != nil {
			return "", err
		}
		// The scrub ran, and is audited as run whatever the restart answers.
		outcome, err := c.afterErasure(ctx, store, fmt.Sprintf("scrubbed %d row version(s)", erased.Versions))
		if auditErr := c.auditCompliance(ctx, store, "shopify_customer_redacted", job); err == nil {
			err = auditErr
		}
		return outcome, err

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
		outcome, err := c.afterErasure(ctx, store, fmt.Sprintf("purged %d row version(s)", rows))
		if auditErr := c.auditCompliance(ctx, store, "shopify_shop_redacted", job); err == nil {
			err = auditErr
		}
		return outcome, err
	}
	return "", fmt.Errorf("shopify: %q is not a compliance topic", job.Topic)
}

// afterErasure is what a redaction or a purge that has just run still owes:
// the store's backfills in flight started again (restartBackfillsInFlight).
// It answers the job's outcome with what it restarted added. A restart that
// could not be queued fails the job, saying that the erasure itself ran: an
// erasure an export may still undo is not a job that is done.
func (c *Connector) afterErasure(ctx context.Context, store Store, outcome string) (string, error) {
	restarted, err := c.restartBackfillsInFlight(ctx, store)
	if len(restarted) > 0 {
		outcome += fmt.Sprintf("; started %d backfill(s) again from nothing", len(restarted))
		c.logger.Info("shopify: the store's backfills in flight were started again after an erasure",
			"store", store.ID, "domains", strings.Join(restarted, ", "))
	}
	if err != nil {
		return "", fmt.Errorf("shopify: %s, but a backfill in flight could not be started again, so an export begun before the erasure may still be applied: %w", outcome, err)
	}
	return outcome, nil
}

// restartBackfillsInFlight starts again, from nothing, every backfill of the
// store that is running or queued, and answers the domains it restarted.
//
// WHY. customers/redact is one point-in-time update, and shop/redact one
// point-in-time delete. A backfill whose export the origin started BEFORE the
// erasure, streamed after it, would write the erased fields back line by
// line. Restarted, its row is queued with an empty cursor: the invocation in
// flight abandons at its next write (the sync runtime's claim on the row), no
// later step reads another line of the old export, and the export the
// continuing pass starts next is one the origin takes after the erasure.
//
// It is the restart a reconnect uses (queueBackfill with its restart flag),
// asked of the sync runtime under the operator's identity. A domain with no
// backfill running or queued is left alone: a complete one holds nothing
// exported before the erasure that the scrub did not reach, and restarting it
// would read the whole domain again for every privacy request. After a purge
// there is normally nothing to restart, because the purge deletes the store's
// sync state with its mirror.
//
// Each domain's row is read BY ID, fresh, as the boot pass reads it.
func (c *Connector) restartBackfillsInFlight(ctx context.Context, store Store) ([]string, error) {
	var restarted []string
	var errs []error
	for _, concept := range generated.ApplyOrder {
		if !bulkDomain(generated.Types[concept]) {
			continue
		}
		stateID := memqlsync.StateID(generated.ConceptID(concept), ConnectorName, string(memqlsync.DirectionInbound), store.ID)
		res, err := c.engine.Execute(memql.ContextWithFreshRead(operatorContext(ctx)), renderCall("syncStateById", map[string]any{"stateId": stateID}))
		if err != nil {
			errs = append(errs, fmt.Errorf("the sync state of %s could not be read: %w", concept, err))
			continue
		}
		rows := memql.MaterializeRows(res)
		if len(rows) == 0 {
			continue
		}
		if status := mapString(rows[0], "backfillStatus"); status != "running" && status != "queued" {
			continue
		}
		if err := c.queueBackfill(ctx, store.ID, concept, true); err != nil {
			errs = append(errs, fmt.Errorf("the backfill of %s could not be queued again: %w", concept, err))
			continue
		}
		restarted = append(restarted, concept)
	}
	return restarted, errors.Join(errs...)
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
	// A managed app shares one signing secret across stores. A path or header
	// alone cannot bind a privacy request to a tenant; the signed body must.
	if domain := firstString(obj, "shop_domain"); domain == "" || !strings.EqualFold(domain, store.Domain) {
		return ComplianceJob{}, fmt.Errorf("shopify: compliance delivery shop does not match the store")
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
	cust, hasCustomer := obj["customer"].(map[string]any)
	dataRequest, hasDataRequest := obj["data_request"].(map[string]any)
	_, hasOrdersToRedact := obj["orders_to_redact"]
	// The topic is an UNSIGNED header, and the three privacy payloads share
	// one signing secret per app, so a captured delivery replays under any
	// topic the sender chooses. Each operation is therefore bound to the
	// payload shape only its own topic carries (memql#5707 review):
	// shop/redact is {shop_id, shop_domain} with no customer;
	// customers/redact carries the customer and no data_request;
	// customers/data_request carries the customer and data_request.id.
	//
	// customers/redact does NOT require orders_to_redact (memql#5707
	// residual), and that is a decision rather than leniency. Shopify
	// documents the key as always present, but it separates nothing: the
	// customer already tells this body from shop/redact, and the absent
	// data_request tells it from customers/data_request, so every replay
	// the check exists for is refused without it. Requiring it could only
	// ever refuse a GENUINE redaction whose key was omitted -- a legal
	// request turned into a failed row, with Shopify never resending it.
	// The key still marks a body as a customer payload for shop/redact.
	switch job.Topic {
	case TopicShopRedact:
		if hasCustomer || hasDataRequest || hasOrdersToRedact {
			return ComplianceJob{}, fmt.Errorf("shopify: %s topic does not match the signed body (a customer payload is present)", job.Topic)
		}
	case TopicRedact:
		if !hasCustomer || hasDataRequest {
			return ComplianceJob{}, fmt.Errorf("shopify: %s topic does not match the signed body", job.Topic)
		}
	case TopicDataRequest:
		if !hasCustomer || !hasDataRequest || firstString(dataRequest, "id") == "" {
			return ComplianceJob{}, fmt.Errorf("shopify: %s topic does not match the signed body", job.Topic)
		}
	}
	if hasCustomer {
		if id := firstString(cust, "id"); id != "" {
			job.CustomerGID = "gid://shopify/Customer/" + strings.TrimPrefix(id, "gid://shopify/Customer/")
		}
	}
	if hasDataRequest {
		job.RequestID = firstString(dataRequest, "id")
	}
	// The orders the request names: orders_to_redact for an erasure,
	// orders_requested for a data request. They are kept on the job, and are in
	// the request's scope whether or not they name the customer
	// (erasureScopeOf). An id arrives as a number, decoded without a float in
	// between (decodeJSONObject), so a long one keeps every digit.
	named := "orders_requested"
	if job.Topic == TopicRedact {
		named = "orders_to_redact"
	}
	if raw, ok := obj[named].([]any); ok && job.Topic != TopicShopRedact {
		for _, v := range raw {
			if v == nil {
				continue
			}
			if s := strings.TrimSpace(fmt.Sprintf("%v", v)); s != "" {
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

// erasedRow is one row a customer's erasure reaches.
type erasedRow struct {
	// ID is the row's id as the table holds it; GID the origin's.
	ID, GID string
	// Own is the erased customer's own row, which keeps nothing.
	Own bool
}

// erasureScope is what one customer's erasure covers in one store: the rows
// it scrubs, which are the rows a data request exports, and the rows outside
// them that only list the customer among their references.
type erasureScope struct {
	// Rows are the rows in scope, by concept, in the order they were found.
	Rows map[string][]erasedRow
	// Listed are the ids of rows OUTSIDE the scope that hold the customer's
	// gid in a list of references, by concept. The gid is taken out of the
	// list; nothing else in such a row is about the customer.
	Listed map[string][]string
}

// count is how many rows the scope holds.
func (s *erasureScope) count() int {
	n := 0
	for _, rows := range s.Rows {
		n += len(rows)
	}
	return n
}

// erasureBatch is how many gids or row ids one statement of the scope or the
// scrub names. They travel as one JSON array parameter, of half a megabyte or
// so at this size; a smaller batch is another read of the concept's rows.
const erasureBatch = 10000

// complianceDB is the raw handle the privacy jobs work through, or the reason
// there is none.
func (c *Connector) complianceDB(what string) (*sql.DB, error) {
	if c.db == nil {
		return nil, fmt.Errorf("shopify: %s needs a database handle", what)
	}
	db := c.db()
	if db == nil {
		return nil, fmt.Errorf("shopify: %s needs a database handle", what)
	}
	return db, nil
}

// erasureScopeOf finds what the erasure of one customer covers in one store
// (rulings 78 and 83). It is the ONE function the erasure and the data request
// both use, so an export and an erasure cannot disagree about what MemQL
// holds on a person.
//
// THE SCOPE starts at the customer's own row and the orders the request names
// (orders_to_redact, orders_requested), and grows to a fixed point by three
// links and no other:
//
//   - a row that hangs from an in-scope row by the model's parent link (its
//     parentGid is that row's gid): an order's lines, transactions, refunds,
//     fulfilments and returns, a return's reverse fulfilment orders, the
//     customer's own payment methods;
//   - a row that holds an in-scope row's gid as the WHOLE value of one of its
//     gid fields: every row "of" the customer (orders, checkouts, contacts),
//     and every row of an order that does not hang from it (a fulfilment
//     order, a tender transaction, a payments dispute);
//   - a row that holds the CUSTOMER's gid anywhere inside an object or a list
//     of objects: the object is about the person.
//
// A LIST OF REFERENCES IS NOT A LINK. A row that holds the customer's gid only
// in one is not about that person; it is answered in Listed, and the erasure
// takes the gid out of the list. An in-scope order's gid in somebody else's
// list of references is left where it is. A metafield is not an object of the
// model either: a merchant's reference to a customer on a product does not
// make the product hers.
//
// A gid is matched as a whole value, never as the beginning of a longer one:
// the scrub this replaces matched a substring of the row's text, so erasing
// customer 123 scrubbed every row of customer 1234.
//
// A ROW IS IN SCOPE WHEN ANY OF ITS VERSIONS HOLDS THE LINK. MemoryNodes is
// keyed (id, createdAt), and an order the origin has since taken the customer
// off still names her in the versions before; those versions are what a
// point-in-time read answers.
//
// It reads the table and not the engine, and every row of the store, a
// tombstoned one included: the paged read the export used answered the first
// hundred rows of a concept.
//
// staged-data: MUST-NOT-GATE -- a staged row left out of the scope KEEPS ITS
// PII and is missing from a data request's export. The obligation is that the
// person's data is found wherever it sits, and a visibility gate would leave
// out the one copy nobody can see.
func (c *Connector) erasureScopeOf(ctx context.Context, db *sql.DB, storeID, customerGID string, orderGIDs []string) (*erasureScope, error) {
	scope := &erasureScope{Rows: map[string][]erasedRow{}, Listed: map[string][]string{}}
	known := map[string]bool{}
	add := func(concept, id, gid string) bool {
		if id == "" || known[id] {
			return false
		}
		known[id] = true
		scope.Rows[concept] = append(scope.Rows[concept], erasedRow{ID: id, GID: gid, Own: concept == erasedConcept && gid == customerGID})
		return true
	}

	// The frontier: the gids whose holders and children are still to be
	// found, by the concept of the row each is the gid of. It starts with the
	// customer's gid and the orders the request names, whether or not the
	// mirror holds a row of either.
	seen := map[string]bool{}
	frontier := map[string][]string{}
	reach := func(concept, gid string) {
		if gid != "" && !seen[gid] {
			seen[gid] = true
			frontier[concept] = append(frontier[concept], gid)
		}
	}
	reach(erasedConcept, customerGID)
	for _, gid := range orderGIDs {
		reach(requestedConcept, gid)
	}

	// The rows the request names: the customer's own, and the orders.
	for _, seed := range []struct {
		concept string
		gids    []string
	}{{erasedConcept, []string{customerGID}}, {requestedConcept, orderGIDs}} {
		for _, batch := range batches(seed.gids) {
			rows, err := queryRows(ctx, db,
				`SELECT id, max(payload->>'gid') FROM "MemoryNodes"
				  WHERE concept = $1 AND payload->>'storeId' = $2
				    AND payload->>'gid' IN (SELECT jsonb_array_elements_text($3::jsonb))
				  GROUP BY id`,
				generated.ConceptID(seed.concept), storeID, jsonArray(batch))
			if err != nil {
				return nil, fmt.Errorf("shopify: read the %s rows a privacy request names: %w", seed.concept, err)
			}
			for _, row := range rows {
				add(seed.concept, row[0], row[1])
			}
		}
	}

	// The customer's gid inside an object, or in a list of references. Only
	// versions whose text holds the gid as a whole JSON string are looked at.
	listed := map[string][]string{}
	whole := `%"` + likeEscape(customerGID) + `"%`
	inside := jsonObject(map[string]any{"g": customerGID})
	for _, concept := range generated.ApplyOrder {
		spec := generated.Types[concept]
		if spec == nil {
			continue
		}
		var objects, lists []string
		for _, f := range spec.Fields {
			switch f.Kind {
			case generated.KindObject, generated.KindObjectList:
				objects = append(objects, fmt.Sprintf(`jsonb_path_exists(payload->%s, '$.** ? (@ == $g)', $4::jsonb)`, sqlText(f.Name)))
			case generated.KindRefs:
				lists = append(lists, fmt.Sprintf(`(jsonb_typeof(payload->%s) = 'array' AND jsonb_exists(payload->%s, $5))`, sqlText(f.Name), sqlText(f.Name)))
			}
		}
		if len(objects) == 0 && len(lists) == 0 {
			continue
		}
		// The last line of the WHERE names both parameters whether or not the
		// concept has fields of the kind that reads them: a parameter no part
		// of a statement reads has no type, and the statement is refused.
		rows, err := queryRows(ctx, db, fmt.Sprintf(
			`SELECT id, max(payload->>'gid'),
			        COALESCE(bool_or(%s), false)::text, COALESCE(bool_or(%s), false)::text
			   FROM "MemoryNodes"
			  WHERE concept = $1 AND payload->>'storeId' = $2 AND payload::text LIKE $3
			    AND $4::jsonb IS NOT NULL AND $5::text IS NOT NULL
			  GROUP BY id`, anyOf(objects), anyOf(lists)),
			generated.ConceptID(concept), storeID, whole, inside, customerGID)
		if err != nil {
			return nil, fmt.Errorf("shopify: look for the customer in the %s rows: %w", concept, err)
		}
		for _, row := range rows {
			if row[2] == "true" && add(concept, row[0], row[1]) {
				reach(concept, row[1])
			}
			if row[3] == "true" {
				listed[concept] = append(listed[concept], row[0])
			}
		}
	}

	// The fixed point: the children and the holders of every gid reached.
	//
	// A concept's rows are read in a round only for the gids they can hold. A
	// gid field names rows of one mirrored type (generated.FieldSpec.AdminType)
	// and a parent link rows of the parent's concept, so a line item's gid is
	// asked of the one concept that can name a line item and not of all of
	// them. For a customer with thousands of orders that is the difference
	// between a few reads of each table and a few hundred.
	conceptsOf := map[string][]string{}
	for concept, spec := range generated.Types {
		conceptsOf[spec.GraphQLType] = append(conceptsOf[spec.GraphQLType], concept)
	}
	for len(frontier) > 0 {
		round := frontier
		frontier = map[string][]string{}
		for _, concept := range generated.ApplyOrder {
			spec := generated.Types[concept]
			if spec == nil {
				continue
			}
			var links []string
			named := map[string]bool{}
			every := false
			if spec.Parent != "" {
				links = append(links, `payload->>'parentGid' IN (SELECT gid FROM reached)`)
				named[spec.Parent] = true
			}
			for _, f := range spec.Fields {
				if f.Kind != generated.KindGid {
					continue
				}
				links = append(links, fmt.Sprintf(`payload->>%s IN (SELECT gid FROM reached)`, sqlText(f.Name)))
				targets := conceptsOf[f.AdminType]
				if len(targets) == 0 {
					// A reference the model cannot place is asked every gid.
					every = true
				}
				for _, target := range targets {
					named[target] = true
				}
			}
			if len(links) == 0 {
				continue
			}
			var gids []string
			for target, reached := range round {
				if every || named[target] {
					gids = append(gids, reached...)
				}
			}
			for _, batch := range batches(gids) {
				rows, err := queryRows(ctx, db, fmt.Sprintf(
					`WITH reached AS (SELECT jsonb_array_elements_text($3::jsonb) AS gid)
					 SELECT id, max(payload->>'gid') FROM "MemoryNodes"
					  WHERE concept = $1 AND payload->>'storeId' = $2 AND (%s)
					  GROUP BY id`, strings.Join(links, " OR ")),
					generated.ConceptID(concept), storeID, jsonArray(batch))
				if err != nil {
					return nil, fmt.Errorf("shopify: follow an erasure's links into the %s rows: %w", concept, err)
				}
				for _, row := range rows {
					if add(concept, row[0], row[1]) {
						reach(concept, row[1])
					}
				}
			}
		}
	}

	for concept, ids := range listed {
		for _, id := range ids {
			if !known[id] {
				scope.Listed[concept] = append(scope.Listed[concept], id)
			}
		}
	}
	return scope, nil
}

// queryRows runs a read whose every column is text, or NULL read as "".
func queryRows(ctx context.Context, db *sql.DB, query string, args ...any) ([][]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		cells := make([]sql.NullString, len(cols))
		into := make([]any, len(cols))
		for i := range cells {
			into[i] = &cells[i]
		}
		if err := rows.Scan(into...); err != nil {
			return nil, err
		}
		row := make([]string, len(cols))
		for i, cell := range cells {
			row[i] = cell.String
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// batches cuts a list into statements' worth.
func batches(items []string) [][]string {
	var out [][]string
	for len(items) > erasureBatch {
		out = append(out, items[:erasureBatch])
		items = items[erasureBatch:]
	}
	if len(items) > 0 {
		out = append(out, items)
	}
	return out
}

// jsonArray is a list of strings as the one text parameter a statement takes
// it in. A gid holds colons and slashes, which an array literal would need
// quoted; JSON has no such trouble.
func jsonArray(items []string) string {
	if items == nil {
		items = []string{}
	}
	raw, _ := json.Marshal(items)
	return string(raw)
}

func jsonObject(v map[string]any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// sqlText is a field's name as a SQL string literal. The names come from the
// generated model, never from a request, and are quoted all the same.
func sqlText(name string) string { return "'" + strings.ReplaceAll(name, "'", "''") + "'" }

// anyOf joins conditions with OR, false for none.
func anyOf(conditions []string) string {
	if len(conditions) == 0 {
		return "false"
	}
	return strings.Join(conditions, " OR ")
}

// likeEscape makes a value match itself in a LIKE pattern.
func likeEscape(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

// ExportCustomerData collects every mirror row a customer's erasure would
// scrub, for the store's owner to answer a data request with: the rows of
// erasureScopeOf, each at its newest version.
func (c *Connector) ExportCustomerData(ctx context.Context, store Store, customerGID string, orderGIDs []string) (map[string]any, error) {
	if customerGID == "" {
		return nil, fmt.Errorf("shopify: data request carried no customer id")
	}
	db, err := c.complianceDB("customers/data_request")
	if err != nil {
		return nil, err
	}
	scope, err := c.erasureScopeOf(ctx, db, store.ID, customerGID, orderGIDs)
	if err != nil {
		return nil, err
	}
	collected := map[string]any{}
	for concept, rows := range scope.Rows {
		var exported []map[string]any
		for _, batch := range batches(rowIDs(rows)) {
			found, err := queryRows(ctx, db,
				`SELECT DISTINCT ON (id) payload::text FROM "MemoryNodes"
				  WHERE concept = $1 AND id IN (SELECT jsonb_array_elements_text($2::jsonb))
				  ORDER BY id, "createdAt" DESC`,
				generated.ConceptID(concept), jsonArray(batch))
			if err != nil {
				return nil, fmt.Errorf("shopify: export %s: %w", concept, err)
			}
			for _, row := range found {
				payload, err := decodeJSONObject(row[0])
				if err != nil {
					return nil, fmt.Errorf("shopify: export %s: a stored row is not JSON", concept)
				}
				exported = append(exported, payload)
			}
		}
		if len(exported) > 0 {
			collected[concept] = exported
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

func rowIDs(rows []erasedRow) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out
}

// ErasureResult is what one pass of a customer's erasure did.
type ErasureResult struct {
	// Rows is how many rows the erasure's scope held.
	Rows int
	// Versions is how many row versions the pass changed. A second pass over
	// rows nothing has written since changes none.
	Versions int64
	// Unlisted is how many versions of rows OUTSIDE the scope had the
	// customer's gid taken out of a list of references.
	Unlisted int64
}

// RedactCustomer erases one customer from a store's mirror: it finds the scope
// (erasureScopeOf) and rewrites every version of every row in it by the policy
// of erasure_fields.go, keeping the opaque gids and the facts of the sale.
//
// WHAT SURVIVES IS THE POINT. An order's total, its line quantities and its
// dates stay: the merchant's books are not the customer's personal data, and
// destroying them would be a different compliance failure. What goes is
// everything that is not shown to be about the sale.
//
// It runs by the STORE'S ID and needs no store row: the mirror's rows can
// outlive the row that configured them. It is safe to run twice, and every
// erasure is (ErasureSecondPassAfter): a version the pass leaves as it should
// be is not written, so what a second pass changes is what was written
// between the two.
func (c *Connector) RedactCustomer(ctx context.Context, storeID, customerGID string, orderGIDs []string) (ErasureResult, error) {
	db, err := c.complianceDB("customers/redact")
	if err != nil {
		return ErasureResult{}, err
	}
	if customerGID == "" {
		return ErasureResult{}, fmt.Errorf("shopify: redact carried no customer id")
	}
	scope, err := c.erasureScopeOf(ctx, db, storeID, customerGID, orderGIDs)
	if err != nil {
		return ErasureResult{}, err
	}
	res := ErasureResult{Rows: scope.count()}
	for _, concept := range generated.ApplyOrder {
		spec := generated.Types[concept]
		if spec == nil {
			continue
		}
		var own, others []string
		for _, row := range scope.Rows[concept] {
			if row.Own {
				own = append(own, row.ID)
			} else {
				others = append(others, row.ID)
			}
		}
		for _, part := range []struct {
			ids  []string
			plan erasurePlan
		}{{own, erasurePlanFor(spec, true)}, {others, erasurePlanFor(spec, false)}} {
			for _, batch := range batches(part.ids) {
				n, err := scrubRows(ctx, db, generated.ConceptID(concept), part.plan, batch)
				if err != nil {
					return res, fmt.Errorf("shopify: redact %s: %w", concept, err)
				}
				res.Versions += n
			}
		}
		var lists []string
		for _, f := range spec.Fields {
			if f.Kind == generated.KindRefs {
				lists = append(lists, f.Name)
			}
		}
		for _, batch := range batches(scope.Listed[concept]) {
			n, err := unlistGID(ctx, db, generated.ConceptID(concept), lists, customerGID, batch)
			if err != nil {
				return res, fmt.Errorf("shopify: take the customer out of the references of %s: %w", concept, err)
			}
			res.Unlisted += n
		}
	}
	return res, nil
}

// scrubRows rewrites every VERSION of the given rows of one concept by a plan,
// and answers how many versions it changed.
//
// A key the plan keeps is left; a string it marks becomes the marker; a list
// it empties becomes []; and EVERY OTHER KEY IS REMOVED, a key the model does
// not know included. That last is default deny in the one place it has to
// hold: a payload is rebuilt from what may stay, not edited by a list of what
// must go.
//
// The values are ones the concept's schema accepts (erasureScrubFate), which
// is what lets the mirror write the row again. Whatever a field holds now is
// rewritten, a value an earlier erasure left included: that one put the
// string marker in objects, lists and booleans, and a row it touched is
// writable again after this.
//
// Every version, and that is the requirement rather than thoroughness for its
// own sake: MemoryNodes is keyed (id, createdAt), so scrubbing the newest
// version alone leaves the customer's name in the row's history, where a
// point-in-time read still returns it. A version already as the plan leaves it
// is not written.
//
// staged-data: MUST-NOT-GATE -- a staged row skipped here KEEPS ITS PII.
func scrubRows(ctx context.Context, db *sql.DB, concept string, plan erasurePlan, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res, err := db.ExecContext(ctx,
		`WITH scrubbed AS (
		   SELECT m.id, m."createdAt",
		          COALESCE((SELECT jsonb_object_agg(e.key, CASE
		                      WHEN e.key IN (SELECT jsonb_array_elements_text($4::jsonb)) THEN to_jsonb($7::text)
		                      WHEN e.key IN (SELECT jsonb_array_elements_text($6::jsonb)) AND COALESCE(m.payload->>$8, '') = '' THEN to_jsonb($7::text)
		                      WHEN e.key IN (SELECT jsonb_array_elements_text($5::jsonb)) THEN '[]'::jsonb
		                      ELSE e.value END)
		                      FROM jsonb_each(m.payload) e
		                     WHERE e.key IN (SELECT jsonb_array_elements_text($3::jsonb))
		                        OR e.key IN (SELECT jsonb_array_elements_text($4::jsonb))
		                        OR e.key IN (SELECT jsonb_array_elements_text($5::jsonb))
		                        OR e.key IN (SELECT jsonb_array_elements_text($6::jsonb))), '{}'::jsonb) AS payload
		     FROM "MemoryNodes" m
		    WHERE m.concept = $1 AND m.id IN (SELECT jsonb_array_elements_text($2::jsonb))
		 )
		 UPDATE "MemoryNodes" m SET payload = s.payload
		   FROM scrubbed s
		  WHERE m.concept = $1 AND m.id = s.id AND m."createdAt" = s."createdAt"
		    AND m.payload IS DISTINCT FROM s.payload`,
		concept, jsonArray(ids), jsonArray(plan.Keep), jsonArray(plan.Mark), jsonArray(plan.Empty),
		jsonArray(plan.MarkOnCustomItem), RedactionMarker, plan.CustomItemReference)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// unlistGID takes one gid out of the lists of references of the given rows,
// in every version that holds it, and changes nothing else in them.
func unlistGID(ctx context.Context, db *sql.DB, concept string, lists []string, gid string, ids []string) (int64, error) {
	if len(ids) == 0 || len(lists) == 0 {
		return 0, nil
	}
	res, err := db.ExecContext(ctx,
		`WITH unlisted AS (
		   SELECT m.id, m."createdAt",
		          (SELECT jsonb_object_agg(e.key, CASE
		                    WHEN e.key IN (SELECT jsonb_array_elements_text($3::jsonb)) AND jsonb_typeof(e.value) = 'array' THEN e.value - $4::text
		                    ELSE e.value END)
		             FROM jsonb_each(m.payload) e) AS payload
		     FROM "MemoryNodes" m
		    WHERE m.concept = $1 AND m.id IN (SELECT jsonb_array_elements_text($2::jsonb))
		 )
		 UPDATE "MemoryNodes" m SET payload = u.payload
		   FROM unlisted u
		  WHERE m.concept = $1 AND m.id = u.id AND m."createdAt" = u."createdAt"
		    AND u.payload IS NOT NULL AND m.payload IS DISTINCT FROM u.payload`,
		concept, jsonArray(ids), jsonArray(lists), gid)
	if err != nil {
		return 0, err
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
	// a mirror that no longer has the rows the cursor counted. A store's
	// rows are partitions of the mirror and carry it as partitionKey, not
	// storeId; both are matched, so a row of either shape goes.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'connector' = $1 AND (payload->>'storeId' = $2 OR payload->>'partitionKey' = $2)`,
		ConnectorName, store.ID); err != nil {
		return total, fmt.Errorf("shopify: purge sync state: %w", err)
	}
	stamp := renderCall("markStoreRedacted", map[string]any{
		"storeId":    store.ID,
		"redactedAt": c.now().UTC().Format(time.RFC3339),
	})
	if _, err := c.engine.Execute(operatorContext(ctx), stamp); err != nil {
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
	if _, err := c.engine.Execute(operatorContext(ctx), call); err != nil {
		// An audit write that fails must not swallow the compliance
		// action, but it must be loud: the trail is the deliverable.
		c.logger.Error("shopify: could not write compliance audit event", "action", action, "store", store.ID, "error", err)
		return nil
	}
	return nil
}

// complianceJobOf is a job as its row holds it.
func complianceJobOf(row map[string]any) ComplianceJob {
	job := ComplianceJob{
		ID:          mapString(row, "id"),
		Topic:       mapString(row, "topic"),
		StoreID:     mapString(row, "storeId"),
		CustomerGID: mapString(row, "customerGid"),
		RequestID:   mapString(row, "requestId"),
		ShopDomain:  mapString(row, "shopDomain"),
		OrderGIDs:   mapStringSlice(row, "orderGids"),
		DueAt:       rowTime(row, "dueAt"),
	}
	if job.ID == "" {
		// The default projection is payload-only, so the row id is not in
		// it; recompute it from the fields that derived it.
		job.ID = ComplianceJobID(job.StoreID, job.Topic, job.CustomerGID+"\x00"+job.RequestID)
	}
	return job
}
