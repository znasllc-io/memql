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
	ID          string    `json:"id"`
	Topic       string    `json:"topic"`
	StoreID     string    `json:"storeId"`
	ShopDomain  string    `json:"shopDomain"`
	CustomerGID string    `json:"customerGid,omitempty"`
	RequestID   string    `json:"requestId,omitempty"`
	OrderGIDs   []string  `json:"orderGids,omitempty"`
	ReceivedAt  time.Time `json:"receivedAt"`
	DueAt       time.Time `json:"dueAt"`
	// Status is the job's as its row holds it; ScrubbedAt is when the first
	// pass of its erasure ran, zero until it has.
	Status     string         `json:"status,omitempty"`
	ScrubbedAt time.Time      `json:"scrubbedAt,omitempty"`
	Raw        map[string]any `json:"-"`
}

// ErasureSecondPassAfter is how long after the first pass of an erasure its
// second pass is due. EVERY ERASURE IS MADE TWICE.
//
// A write that is in flight when the scrub runs lands after it: the rest of a
// page of an export being applied, a delivery fetched a moment before. Starting
// the store's backfills again closes the long case, an old export read later
// (afterErasure). This closes the short one: the erasure is made again once
// whatever was in flight has landed, and only then is the job done. An hour is
// long beside a page or a delivery and short beside the thirty days an erasure
// has. The runner is hourly, so the second pass runs at its first run after
// the hour is up.
const ErasureSecondPassAfter = time.Hour

// A job's statuses, as the concept's enum spells them.
const (
	jobPending = "pending"
	jobDone    = "done"
	jobFailed  = "failed"
)

// jobStep is what one run of a job leaves on its row when it did not fail.
type jobStep struct {
	// Status is jobPending when the second pass of an erasure is still to
	// come, else jobDone.
	Status  string
	Outcome string
	// ScrubbedAt and DueAt are set by the first pass of an erasure: when it
	// ran, and when the second is due.
	ScrubbedAt, DueAt time.Time
}

// jobFailure is a run that failed: one sentence for the job's row, and the
// cause for the server's log.
//
// THE ROW TAKES THE SENTENCE AND NEVER THE CAUSE. The cause is the engine's
// or the database's own text, and such text can quote the statement it
// refused -- a customer's gid, an address, a name. A name cannot be told from
// any other word, so nothing of a cause is copied: lastError says what
// failed, from the closed set below, and not for whom.
type jobFailure struct {
	sentence string
	cause    error
}

func (f *jobFailure) Error() string {
	if f.cause == nil {
		return f.sentence
	}
	return f.sentence + ": " + f.cause.Error()
}

func (f *jobFailure) Unwrap() error { return f.cause }

func failedJob(sentence string, cause error) error {
	return &jobFailure{sentence: sentence, cause: cause}
}

// The sentences a failed job's row can hold.
const (
	failNoCustomer  = "the request names no customer"
	failNotATopic   = "the job's topic is not a privacy topic"
	failScrub       = "the customer's rows could not be scrubbed"
	failPurge       = "the store's mirror could not be deleted"
	failRestart     = "the erasure ran, but a backfill in flight could not be started again"
	failDropExport  = "the erasure ran, but a failed backfill's old export could not be dropped"
	failExportRead  = "the customer's rows could not be read for the export"
	failExportOwner = "the store names no owner to file the export under"
	failExportWrite = "the export could not be filed in the store owner's library"
	failUnknown     = "the job could not be run"
)

// lastErrorOf is the sentence a failed run leaves on its job's row.
func lastErrorOf(err error) string {
	var f *jobFailure
	if errors.As(err, &f) {
		return f.sentence
	}
	return failUnknown
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
	return c.auditCompliance(ctx, store.ID, complianceAuditAction(topic)+"_received", job, job.ReceivedAt, "")
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

// complianceJobPages bounds one run's walk of the due read, in pages of a
// hundred jobs.
const complianceJobPages = 1000

// RunDueComplianceJobs runs every privacy job that is due: one waiting whose
// time has come, one that failed, and an erasure an earlier version recorded
// done (the due read, complianceJobsDue, says which and why). It answers how
// many it ran without failing.
//
// A FAILED JOB IS RUN AGAIN AT EVERY RUN UNTIL IT IS DONE. Nothing a job does
// is unsafe to do twice: an erasure rewrites rows to what they should be, a
// purge deletes what is there, and an export is written under the id of its
// request.
//
// EVERY DUE JOB, not the first page of them: failed jobs stay due, and a
// hundred standing failures would otherwise fill the one page a run read,
// oldest first, for good. All of them are read before any is run, because a
// run rewrites its job's row and a walk over rows that change under it can
// skip one.
func (c *Connector) RunDueComplianceJobs(ctx context.Context) (int, error) {
	// The stores, read once and fresh. A store row that is gone changes what
	// a job does (runComplianceJob), so a list that could not be read must
	// not be taken for one with no stores in it: the run stops and the jobs
	// stay due.
	if c.stores == nil {
		return 0, fmt.Errorf("shopify: the privacy jobs have no store registry to read the stores from")
	}
	stores, err := c.stores.Stores(memql.ContextWithFreshRead(ctx))
	if err != nil {
		return 0, fmt.Errorf("shopify: the privacy jobs could not read the stores: %w", err)
	}
	byID := make(map[string]Store, len(stores))
	for _, s := range stores {
		byID[s.ID] = s
	}

	var jobs []ComplianceJob
	due := renderCall("complianceJobsDue", map[string]any{"asOf": c.now().UTC().Format(time.RFC3339)})
	walkErr := memql.WalkQueryPages(operatorContext(ctx), c.engine.Execute, due, complianceJobPages, func(res *memql.ExecuteResult) error {
		for _, row := range memql.MaterializeRows(res) {
			jobs = append(jobs, complianceJobOf(row))
		}
		return nil
	})
	if walkErr != nil && len(jobs) == 0 {
		return 0, fmt.Errorf("shopify: read due compliance jobs: %w", walkErr)
	}

	ran := 0
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return ran, err
		}
		store, found := byID[job.StoreID]
		step, err := c.runComplianceJob(ctx, store, found, job)
		if err != nil {
			// The cause is logged here and nowhere else (jobFailure).
			c.logger.Error("shopify: compliance job failed", "topic", job.Topic, "store", job.StoreID, "job", job.ID, "error", err)
			c.recordJobFailed(ctx, job, lastErrorOf(err))
			continue
		}
		ran++
		c.recordJobStep(ctx, job, step)
	}
	if walkErr != nil {
		// What was read was run. The rest is still due and is read next time.
		return ran, fmt.Errorf("shopify: read due compliance jobs: %w", walkErr)
	}
	return ran, nil
}

// recordJobStep writes what a run that did not fail leaves on its job's row.
// A second pass still to come leaves the job pending, with when the first ran
// and when the second is due; a job that is done is stamped as completed. An
// earlier failure's sentence is cleared either way.
func (c *Connector) recordJobStep(ctx context.Context, job ComplianceJob, step jobStep) {
	args := map[string]any{"jobId": job.ID, "status": step.Status, "outcome": step.Outcome, "lastError": ""}
	if !step.ScrubbedAt.IsZero() {
		args["scrubbedAt"] = step.ScrubbedAt.UTC().Format(time.RFC3339)
	}
	if !step.DueAt.IsZero() {
		args["dueAt"] = step.DueAt.UTC().Format(time.RFC3339)
	}
	if step.Status == jobDone {
		args["completedAt"] = c.now().UTC().Format(time.RFC3339)
	}
	c.recordJob(ctx, job.ID, args)
}

// recordJobFailed writes a failed run: the status and the sentence. What an
// earlier pass left -- its outcome and when it ran -- is not named, so it
// stays, and the next run takes the job up where it stands: a failed first
// pass is a first pass again, a failed second pass a second pass.
func (c *Connector) recordJobFailed(ctx context.Context, job ComplianceJob, sentence string) {
	c.recordJob(ctx, job.ID, map[string]any{"jobId": job.ID, "status": jobFailed, "lastError": sentence})
}

func (c *Connector) recordJob(ctx context.Context, jobID string, args map[string]any) {
	if _, err := c.engine.Execute(operatorContext(ctx), renderCall("recordComplianceJob", args)); err != nil {
		c.logger.Warn("shopify: could not record a compliance job's outcome", "job", jobID, "error", err)
	}
}

// runComplianceJob performs one run of one privacy request and reports what
// it leaves on the job's row. found says whether the job's store still has a
// row; store is that row, or nothing.
//
// A JOB WHOSE STORE ROW IS GONE IS NOT A FAILURE. The mirror's rows can
// outlive the row that configured them, so an erasure runs by the job's own
// store id; only the store's backfills are left alone, as nothing reads a
// store that is not configured. A data request for such a store has nothing
// configured to export and nobody to file an export under, and is done.
func (c *Connector) runComplianceJob(ctx context.Context, store Store, found bool, job ComplianceJob) (jobStep, error) {
	if job.StoreID == "" {
		job.StoreID = store.ID
	}
	switch job.Topic {
	case TopicDataRequest:
		return c.runDataRequest(ctx, store, found, job)
	case TopicRedact:
		return c.runCustomerErasure(ctx, store, found, job)
	case TopicShopRedact:
		return c.runShopErasure(ctx, store, found, job)
	}
	return jobStep{}, failedJob(failNotATopic, fmt.Errorf("shopify: %q is not a compliance topic", job.Topic))
}

func (c *Connector) runDataRequest(ctx context.Context, store Store, found bool, job ComplianceJob) (jobStep, error) {
	now := c.now()
	if !found {
		const nothing = "the store is no longer configured; nothing to export"
		_ = c.auditCompliance(ctx, job.StoreID, "shopify_data_request_not_exported", job, now, nothing)
		return jobStep{Status: jobDone, Outcome: nothing}, nil
	}
	if job.CustomerGID == "" {
		return jobStep{}, failedJob(failNoCustomer, nil)
	}
	export, err := c.ExportCustomerData(ctx, store, job.CustomerGID, job.OrderGIDs)
	if err != nil {
		return jobStep{}, failedJob(failExportRead, err)
	}
	artifact, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return jobStep{}, failedJob(failExportRead, err)
	}
	if store.OwnerUserID == "" {
		return jobStep{}, failedJob(failExportOwner, fmt.Errorf("shopify: store %q has no ownerUserId", store.ID))
	}
	if err := c.writeExportArtifact(ctx, store, job, artifact); err != nil {
		return jobStep{}, failedJob(failExportWrite, err)
	}
	what := fmt.Sprintf("exported %d bytes", len(artifact))
	_ = c.auditCompliance(ctx, job.StoreID, "shopify_data_request_exported", job, now, what)
	return jobStep{Status: jobDone, Outcome: what}, nil
}

func (c *Connector) runCustomerErasure(ctx context.Context, store Store, found bool, job ComplianceJob) (jobStep, error) {
	if job.CustomerGID == "" {
		return jobStep{}, failedJob(failNoCustomer, nil)
	}
	second := !job.ScrubbedAt.IsZero()
	erased, err := c.RedactCustomer(ctx, job.StoreID, job.CustomerGID, job.OrderGIDs)
	if err != nil {
		return jobStep{}, failedJob(failScrub, err)
	}
	what := fmt.Sprintf("scrubbed %d row version(s) of %d row(s)", erased.Versions, erased.Rows)
	if erased.Unlisted > 0 {
		what += fmt.Sprintf(", and took the customer out of the references of %d more", erased.Unlisted)
	}
	// The scrub ran, and is audited as run whatever its backfills answer.
	_ = c.auditCompliance(ctx, job.StoreID, passAction("shopify_customer_redacted", second), job, c.now(), what)
	after, err := c.afterErasure(ctx, store, found, !second)
	if err != nil {
		return jobStep{}, err
	}
	return c.erasurePass(second, what+after), nil
}

func (c *Connector) runShopErasure(ctx context.Context, store Store, found bool, job ComplianceJob) (jobStep, error) {
	second := !job.ScrubbedAt.IsZero()
	// Check the store before purging, and before the SECOND purge exactly as
	// before the first. An uninstall an operator reverses within the hold
	// must not cost the mirror; and a store that came back between the two
	// purges has had everything read again by its reconnect, which a second
	// purge would delete.
	if found && store.Ingests() && store.RedactedAt == "" && c.storeReachable(ctx, store) {
		c.logger.Info("shopify: skipping shop/redact purge, the store is reachable again", "store", store.ID, "secondPass", second)
		if second {
			const kept = "second pass: the store is reachable again and was not purged a second time; its reconnect reads everything again"
			_ = c.auditCompliance(ctx, job.StoreID, "shopify_shop_redact_second_pass_skipped", job, c.now(), kept)
			return jobStep{Status: jobDone, Outcome: kept}, nil
		}
		const skipped = "skipped: the store is reachable again"
		_ = c.auditCompliance(ctx, job.StoreID, "shopify_shop_redact_skipped", job, c.now(), skipped)
		return jobStep{Status: jobDone, Outcome: skipped}, nil
	}
	var rows int64
	var err error
	if found && store.RedactedAt == "" {
		rows, err = c.PurgeStore(ctx, store)
	} else {
		// No row to stamp, or one the first purge has stamped already.
		rows, err = c.purgeMirror(ctx, job.StoreID)
	}
	if err != nil {
		return jobStep{}, failedJob(failPurge, err)
	}
	what := fmt.Sprintf("purged %d row version(s)", rows)
	_ = c.auditCompliance(ctx, job.StoreID, passAction("shopify_shop_redacted", second), job, c.now(), what)
	after, err := c.afterErasure(ctx, store, found, !second)
	if err != nil {
		return jobStep{}, err
	}
	return c.erasurePass(second, what+after), nil
}

// passAction is the audit action of one pass of an erasure: the second names
// itself.
func passAction(action string, second bool) string {
	if second {
		return action + "_second_pass"
	}
	return action
}

// erasurePass is what one pass of an erasure leaves on its job: the first
// leaves it pending, with when it ran and when the second is due; the second
// ends it.
func (c *Connector) erasurePass(second bool, what string) jobStep {
	if second {
		return jobStep{Status: jobDone, Outcome: "second pass: " + what}
	}
	now := c.now().UTC()
	due := now.Add(ErasureSecondPassAfter)
	return jobStep{
		Status:     jobPending,
		Outcome:    "first pass: " + what + "; the second pass is due at " + due.Format(time.RFC3339),
		ScrubbedAt: now,
		DueAt:      due,
	}
}

// afterErasure is what an erasure that has just run still owes the store's
// backfills (settleBackfills), and answers what it did as the rest of the
// job's outcome. first is set for the first pass.
//
// A backfill that could not be started again, or an old export that could not
// be dropped, FAILS the job, saying that the erasure itself ran: an erasure an
// export may still undo is not a job that is done. The job is then run again.
//
// A store whose row is gone has no backfill anybody will run, and its row is
// what a restart is asked through; it is left alone and the outcome says so.
func (c *Connector) afterErasure(ctx context.Context, store Store, found, first bool) (string, error) {
	if !found {
		return "; the store is no longer configured, so no backfill was started again", nil
	}
	restarted, dropped, restartErr, dropErr := c.settleBackfills(ctx, store, first)
	note := ""
	if len(restarted) > 0 {
		note += fmt.Sprintf("; started %d backfill(s) again from nothing", len(restarted))
		c.logger.Info("shopify: the store's backfills in flight were started again after an erasure",
			"store", store.ID, "domains", strings.Join(restarted, ", "))
	}
	if len(dropped) > 0 {
		note += fmt.Sprintf("; dropped the old export of %d failed backfill(s)", len(dropped))
		c.logger.Info("shopify: failed backfills had the export they named dropped after an erasure",
			"store", store.ID, "domains", strings.Join(dropped, ", "))
	}
	if restartErr != nil {
		return "", failedJob(failRestart, restartErr)
	}
	if dropErr != nil {
		return "", failedJob(failDropExport, dropErr)
	}
	return note, nil
}

// settleBackfills makes sure no export the origin took BEFORE an erasure is
// read after it, and answers the domains it restarted and those whose old
// export it dropped.
//
// WHY. customers/redact is one point-in-time update, and shop/redact one
// point-in-time delete. A backfill whose export the origin started before the
// erasure, streamed after it, would write the erased fields back line by line.
//
// A backfill that is RUNNING or QUEUED is started again from nothing, on the
// first pass only (restart): its row is queued with an empty cursor, the
// invocation in flight abandons at its next write (the sync runtime's claim on
// the row), no later step reads another line of the old export, and the export
// the continuing pass starts next is one the origin takes after the erasure.
// It is the restart a reconnect uses (queueBackfill with its restart flag),
// asked of the sync runtime under the operator's identity.
//
// A backfill that is FAILED and whose cursor names an export has that export
// dropped from its cursor, on every pass: an operator's "Backfill now" on a
// failed domain continues from its cursor, and would continue the old export.
// Its status and its error are left as they are; nothing is started.
//
// A domain with nothing of the kind is left alone: a complete one holds
// nothing exported before the erasure that the scrub did not reach, and
// starting it again would read the whole domain for every privacy request.
// After a purge there is normally nothing here, because the purge deletes the
// store's sync state with its mirror.
//
// Each domain's row is read BY ID, fresh, as the boot pass reads it.
func (c *Connector) settleBackfills(ctx context.Context, store Store, restart bool) (restarted, dropped []string, restartErr, dropErr error) {
	var restartErrs, dropErrs []error
	for _, concept := range generated.ApplyOrder {
		if !bulkDomain(generated.Types[concept]) {
			continue
		}
		stateID := memqlsync.StateID(generated.ConceptID(concept), ConnectorName, string(memqlsync.DirectionInbound), store.ID)
		res, err := c.engine.Execute(memql.ContextWithFreshRead(operatorContext(ctx)), renderCall("syncStateById", map[string]any{"stateId": stateID}))
		if err != nil {
			restartErrs = append(restartErrs, fmt.Errorf("the sync state of %s could not be read: %w", concept, err))
			continue
		}
		rows := memql.MaterializeRows(res)
		if len(rows) == 0 {
			continue
		}
		switch status := mapString(rows[0], "backfillStatus"); {
		case status == "running" || status == "queued":
			if !restart {
				continue
			}
			if err := c.queueBackfill(ctx, store.ID, concept, true); err != nil {
				restartErrs = append(restartErrs, fmt.Errorf("the backfill of %s could not be queued again: %w", concept, err))
				continue
			}
			restarted = append(restarted, concept)
		case status == "failed":
			did, err := c.dropFailedExport(ctx, stateID, rows[0])
			if err != nil {
				dropErrs = append(dropErrs, fmt.Errorf("the failed backfill of %s could not have its export dropped: %w", concept, err))
				continue
			}
			if did {
				dropped = append(dropped, concept)
			}
		}
	}
	return restarted, dropped, errors.Join(restartErrs...), errors.Join(dropErrs...)
}

// conceptSyncState is the sync runtime's health row, which a write of this
// connector's is fenced on.
const conceptSyncState = "v1:platform:syncState"

// dropFailedExport takes the export out of a failed backfill's cursor, and
// reports whether there was one to take. What is the backfill's own stays in
// the cursor: the rule it began under and its count of restarts. The export
// goes whole -- the operation, the part of a multi-part domain it was, the
// line it had reached and when it was started -- so the next start begins a
// new export at the first part.
//
// The write names the cursor and nothing else, and is fenced on the version of
// the row that was read (memql.ContextWithRowVersionFence): an operator's
// "Backfill now" that started the row in between has written its own cursor,
// and this write is then refused and not made. That row is running now, with
// the old export, and the first pass of the next erasure restarts it; here it
// is reported, so the job is run again.
func (c *Connector) dropFailedExport(ctx context.Context, stateID string, row map[string]any) (bool, error) {
	cur := decodeBackfillCursor(mapString(row, "backfillCursor"))
	if cur.OperationID == "" {
		return false, nil
	}
	kept := backfillCursor{Restarts: cur.Restarts, Rule: cur.Rule}
	cursor := ""
	if kept != (backfillCursor{}) {
		cursor = kept.encode()
	}
	fenced := memql.ContextWithRowVersionFence(operatorContext(ctx), conceptSyncState, stateID, rowTime(row, "createdAt"))
	if _, err := c.engine.Execute(fenced, renderCall("patchSyncState", map[string]any{"stateId": stateID, "backfillCursor": cursor})); err != nil {
		return false, err
	}
	return true, nil
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
	total, err := c.purgeMirror(ctx, store.ID)
	if err != nil {
		return total, err
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

// purgeMirror deletes every mirror row of a store and its sync state, by the
// store's id, and stamps nothing: a store whose row is gone has none to stamp,
// and the second purge of a shop's erasure finds the first one's stamp there.
func (c *Connector) purgeMirror(ctx context.Context, storeID string) (int64, error) {
	db, err := c.complianceDB("shop/redact")
	if err != nil {
		return 0, err
	}
	var total int64
	for _, concept := range generated.ApplyOrder {
		res, err := db.ExecContext(ctx,
			`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'storeId' = $2`,
			generated.ConceptID(concept), storeID)
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
		ConnectorName, storeID); err != nil {
		return total, fmt.Errorf("shopify: purge sync state: %w", err)
	}
	return total, nil
}

// auditCompliance records a privacy action on the audit log.
//
// Every compliance action is audited, including the ones that only queue
// work: the obligation is to be able to SHOW what was done and when, and a
// job that ran leaving no trace is indistinguishable from one that did not.
//
// at is when the action happened: the delivery's receipt for a request that
// was queued, the pass's own time for a run. It is part of the event's id, so
// each pass of an erasure, and each time a job is run again, is an event of
// its own. what is what the run did, for the trail.
func (c *Connector) auditCompliance(ctx context.Context, storeID, action string, job ComplianceJob, at time.Time, what string) error {
	details := map[string]any{
		"topic":      job.Topic,
		"storeId":    storeID,
		"shopDomain": job.ShopDomain,
		"dueAt":      job.DueAt.Format(time.RFC3339),
	}
	if job.CustomerGID != "" {
		details["customerGid"] = job.CustomerGID
	}
	if job.RequestID != "" {
		details["requestId"] = job.RequestID
	}
	if what != "" {
		details["outcome"] = what
	}
	call := renderCall("createAuditEvent", map[string]any{
		"eventId":     "aud" + MirrorRowID(storeID, action+"\x00"+job.CustomerGID+"\x00"+job.RequestID+"\x00"+at.Format(time.RFC3339)),
		"occurredAt":  c.now().UTC().Format(time.RFC3339),
		"category":    "data",
		"action":      action,
		"actorUserId": "system:connector:" + ConnectorName,
		"targetType":  "shopifyStore",
		"targetId":    storeID,
		"detail":      details,
		"outcome":     "success",
	})
	if _, err := c.engine.Execute(operatorContext(ctx), call); err != nil {
		// An audit write that fails must not swallow the compliance
		// action, but it must be loud: the trail is the deliverable.
		c.logger.Error("shopify: could not write compliance audit event", "action", action, "store", storeID, "error", err)
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
		Status:      mapString(row, "status"),
		ScrubbedAt:  rowTime(row, "scrubbedAt"),
	}
	if job.ID == "" {
		// The default projection is payload-only, so the row id is not in
		// it; recompute it from the fields that derived it.
		job.ID = ComplianceJobID(job.StoreID, job.Topic, job.CustomerGID+"\x00"+job.RequestID)
	}
	return job
}
