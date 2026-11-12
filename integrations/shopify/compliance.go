package shopify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
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
	Status     string    `json:"status,omitempty"`
	ScrubbedAt time.Time `json:"scrubbedAt,omitempty"`
	// RequestedAt is when a request for the job was last recorded on its row.
	RequestedAt time.Time `json:"requestedAt,omitempty"`
	// DeliveryIDs are the row ids of the staged deliveries that asked for
	// the job; ExportID is what a data request filed in the owner's Library.
	DeliveryIDs []string       `json:"deliveryIds,omitempty"`
	ExportID    string         `json:"exportId,omitempty"`
	Raw         map[string]any `json:"-"`
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
	// ExportID is what a data request filed in the owner's Library.
	ExportID string
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
	failSession     = "the job's statements had no database session to run on"
	failStoreRead   = "the store's row could not be read"
	failReach       = "the store did not say whether the app is still installed, so nothing was deleted"
	failScrub       = "the customer's rows could not be scrubbed"
	failDeliveries  = "the customer's rows were scrubbed, but the store's processed deliveries could not be blanked"
	failAsked       = "the job's work was done, but the deliveries that asked for it could not be blanked or deleted"
	failExports     = "the customer's rows were scrubbed, but an export a data request filed for her could not be removed"
	failRepair      = "the customer's rows were scrubbed, but rows an earlier erasure left unwritable could not be repaired"
	failRequeue     = "rows an earlier erasure wrongly scrubbed could not be queued to be read again"
	failPurge       = "the store's mirror could not be deleted"
	failStamp       = "the store's mirror was deleted, but the store did not say whether the app is still installed, so it is not marked redacted yet"
	failReadAgain   = "the store is connected again, but its backfills that had not started could not be queued"
	failOlder       = "the erasure ran, but the shop's uninstall or older privacy deliveries could not be deleted"
	failRestart     = "the erasure ran, but a backfill in flight could not be started again"
	failDropExport  = "the erasure ran, but a failed backfill's old export could not be dropped"
	failExportRead  = "the customer's rows could not be read for the export"
	failExportOwner = "the store names no owner to file the export under"
	failExportWrite = "the export could not be filed in the store owner's library"
	failErasedRead  = "whether the customer has been erased could not be read, so nothing was filed"
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

// errorClass is what kind of error a failure was, for the server's log: a
// word from a closed set, a status, a SQLSTATE or a Go type. NEVER THE
// ERROR'S TEXT. The text is the engine's, the database's or the origin's own,
// and it can quote what it refused: a failed export's cause can hold the
// whole export, a refused statement a customer's gid.
func errorClass(err error) string {
	var failure *jobFailure
	if errors.As(err, &failure) {
		err = failure.cause
	}
	if err == nil {
		return "none"
	}
	var admin *AdminError
	var sqlState interface{ Field(byte) string }
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, memql.ErrRowVersionChanged):
		return "row version changed"
	case errors.As(err, &admin):
		return fmt.Sprintf("origin HTTP %d", admin.StatusCode)
	case errors.As(err, &sqlState):
		return "database SQLSTATE " + sqlState.Field('C')
	}
	// The innermost error's Go type: of several joined, the first's.
	for {
		if joined, ok := err.(interface{ Unwrap() []error }); ok && len(joined.Unwrap()) > 0 {
			err = joined.Unwrap()[0]
			continue
		}
		inner := errors.Unwrap(err)
		if inner == nil {
			break
		}
		err = inner
	}
	return fmt.Sprintf("%T", err)
}

// enqueueComplianceJob parses a compliance delivery and records it.
//
// It does NOT do the work inline. customers/redact has a 30-day window
// and shop/redact a 48-hour hold, and the receiver has five seconds -- so
// the delivery becomes a job and the job runs on a schedule. That is also
// what lets a merchant's own grace period apply if one is configured.
//
// A REQUEST LANDS ON ITS JOB'S ROW AS THE ROW STANDS (ruling 92). The job's
// id is derived from the store, the topic and the customer, so a second
// erasure of the same customer, or of the same shop after a reinstall, finds
// the first one's row:
//
//   - no row: the job is queued, with its hold;
//   - a job that is DONE: it is opened again for both passes, due after the
//     hold of the new request, with the orders of both;
//   - a job that is WAITING or FAILED: the request's orders join the job's,
//     and its dueAt is not moved -- a redelivery must not push an erasure
//     back, least of all one waiting for its second pass.
//
// A data request carries its own request id in its job's id, so a second
// delivery of one is the same request, and its row is left as it is.
//
// The row is read fresh and every write is fenced on the version that was
// read (memql.ContextWithRowVersionFence): the runner writes the same row,
// fenced the same way, so of a request and a pass that finish together one
// is refused, reads again and decides on what the other left.
func (c *Connector) enqueueComplianceJob(ctx context.Context, store Store, topic string, req memqlsync.InboundRequest) error {
	job, err := parseComplianceJob(topic, store, req, c.now())
	if err != nil {
		return err
	}
	job.ID = ComplianceJobID(store.ID, job.Topic, job.CustomerGID+"\x00"+job.RequestID)
	// The delivery that asks is remembered on the job (ruling 95): its body
	// holds the customer's email and phone. It is blanked at each pass the
	// job makes and deleted when the job is done (finishAsked).
	if req.RequestId != "" {
		job.DeliveryIDs = []string{req.RequestId}
	}
	for try := 0; ; try++ {
		err := c.recordRequest(ctx, job)
		if err == nil {
			break
		}
		if !errors.Is(err, memql.ErrRowVersionChanged) || try >= jobWriteTries {
			return fmt.Errorf("shopify: queue %s: %w", topic, err)
		}
	}
	c.logger.Info("shopify: compliance request queued", "topic", topic, "store", store.ID, "dueAt", job.DueAt.Format(time.RFC3339))
	return c.auditCompliance(ctx, store.ID, complianceAuditAction(topic)+"_received", job, job.ReceivedAt, "")
}

// jobWriteTries is how many times a write of a job's row that lost to another
// writer is decided again before it is given up as an error.
const jobWriteTries = 5

// recordRequest records one request on its job's row, once: it reads the row
// and makes the one write the row's state asks for.
func (c *Connector) recordRequest(ctx context.Context, job ComplianceJob) error {
	now := c.now().UTC().Format(time.RFC3339)
	existing, version, found, err := c.complianceJobByID(ctx, job.ID)
	if err != nil {
		return err
	}
	fenced := memql.ContextWithRowVersionFence(operatorContext(ctx), conceptComplianceJob, job.ID, version)
	if !found {
		// version is zero: the write is refused if a row has appeared.
		_, err := c.engine.Execute(fenced, renderCall("queueComplianceJob", map[string]any{
			"jobId":       job.ID,
			"storeId":     job.StoreID,
			"topic":       job.Topic,
			"customerGid": job.CustomerGID,
			"requestId":   job.RequestID,
			"shopDomain":  job.ShopDomain,
			"orderGids":   append([]string{}, job.OrderGIDs...),
			"dueAt":       job.DueAt.UTC().Format(time.RFC3339),
			"requestedAt": now,
			"deliveryIds": append([]string{}, job.DeliveryIDs...),
		}))
		return err
	}
	deliveries := unionOf(existing.DeliveryIDs, job.DeliveryIDs)
	if job.Topic == TopicDataRequest {
		// THE SAME REQUEST AGAIN: a data request names its own request, so
		// the same words are the same request, and it is not answered twice.
		// Told with no staged delivery, its row is left as it is.
		//
		// BUT A DELIVERY THAT REACHES HERE IS A ROW, with the customer's email
		// and phone in its body, and the job is what deletes it. A redelivery
		// lands on the first one's row and is skipped while that row is
		// there; it gets here only once the job has deleted it. So the job
		// must run once more, or the row stays for good:
		//
		//   - a job that is DONE is opened again. Run again it files nothing
		//     (dataRequest: it has filed its export) and deletes the delivery.
		//   - a job that is not done has the request recorded on its row. Its
		//     next run deletes the delivery with the rest; and if a pass is
		//     at this moment between its delete and its last write, that
		//     write is refused by the fence and the job is opened again
		//     (recordJobStep).
		if len(job.DeliveryIDs) == 0 {
			return nil
		}
		if existing.Status == jobDone {
			_, err := c.engine.Execute(fenced, renderCall("reopenComplianceJob", map[string]any{
				"jobId":       job.ID,
				"dueAt":       job.DueAt.UTC().Format(time.RFC3339),
				"orderGids":   append([]string{}, existing.OrderGIDs...),
				"requestedAt": now,
				"deliveryIds": deliveries,
				"outcome":     "",
			}))
			return err
		}
		_, err := c.engine.Execute(fenced, renderCall("extendComplianceJob", map[string]any{
			"jobId":       job.ID,
			"orderGids":   append([]string{}, existing.OrderGIDs...),
			"requestedAt": now,
			"deliveryIds": deliveries,
		}))
		return err
	}
	orders := unionOf(existing.OrderGIDs, job.OrderGIDs)
	if existing.Status == jobDone {
		_, err := c.engine.Execute(fenced, renderCall("reopenComplianceJob", map[string]any{
			"jobId":       job.ID,
			"dueAt":       job.DueAt.UTC().Format(time.RFC3339),
			"orderGids":   orders,
			"requestedAt": now,
			"deliveryIds": deliveries,
			// The earlier completion is over: its outcome goes with its time.
			"outcome": "",
		}))
		return err
	}
	_, err = c.engine.Execute(fenced, renderCall("extendComplianceJob", map[string]any{
		"jobId":       job.ID,
		"orderGids":   orders,
		"requestedAt": now,
		"deliveryIds": deliveries,
	}))
	return err
}

// unionOf is the first list with what the second adds, in order.
func unionOf(have, more []string) []string {
	out := append([]string{}, have...)
	for _, item := range more {
		if !slices.Contains(out, item) {
			out = append(out, item)
		}
	}
	return out
}

// conceptComplianceJob is the privacy job's concept, which a write of a job's
// row is fenced on.
const conceptComplianceJob = "v1:shopify:complianceJob"

// complianceJobByID reads one job's row fresh, with the version a write of it
// is fenced on. A job that has no row answers found false and a zero version,
// which is the fence for "still absent".
func (c *Connector) complianceJobByID(ctx context.Context, jobID string) (ComplianceJob, time.Time, bool, error) {
	res, err := c.engine.Execute(memql.ContextWithFreshRead(operatorContext(ctx)), renderCall("complianceJobById", map[string]any{"jobId": jobID}))
	if err != nil {
		return ComplianceJob{}, time.Time{}, false, err
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return ComplianceJob{}, time.Time{}, false, nil
	}
	job := complianceJobOf(rows[0])
	job.ID = jobID
	return job, rowTime(rows[0], "createdAt"), true, nil
}

// ComplianceJobID derives the row id from (store, topic, subject).
//
// Deterministic, so Shopify re-sending the same request lands on the same
// row rather than queueing the work twice; what a second request does to the
// row it lands on is enqueueComplianceJob's to say.
func ComplianceJobID(storeID, topic, subject string) string {
	return "shpcj" + string(idEngine.FromString(storeID + "\x00" + topic + "\x00" + subject))[:24]
}

// complianceJobPages bounds one run's walk of the due read, in pages of a
// hundred jobs.
const complianceJobPages = 1000

// privacyRunBudget is how long a run of the privacy jobs goes on TAKING jobs.
// A job that is running when the budget is spent is finished; what is left
// waits for the next run, an hour on. The runs are single (the automation's
// mode), so a run that overran the hour would cost the next one, and twenty
// minutes leaves a statement that takes its whole limit room to end before
// the next run begins.
const privacyRunBudget = 20 * time.Minute

// RunDueComplianceJobs runs the privacy jobs that are due: one waiting whose
// time has come, one that failed, and an erasure an earlier version recorded
// done (the due read, complianceJobsDue, says which and why). It answers how
// many it ran without failing.
//
// A FAILED JOB IS RUN AGAIN AT EVERY RUN UNTIL IT IS DONE. Nothing a job does
// is unsafe to do twice: an erasure rewrites rows to what they should be, a
// purge deletes what is there, and an export is written under the id of its
// request.
//
// EVERY DUE JOB IS READ, not the first page of them: failed jobs stay due,
// and a hundred standing failures would otherwise fill the one page a run
// read, oldest first, for good. All of them are read before any is run,
// because a run rewrites its job's row and a walk over rows that change under
// it can skip one.
//
// NEW REQUESTS ARE WORKED BEFORE REPEATS OF OLD ONES. An erasure an earlier
// version recorded done is made again (ruling 84), and an instance can hold
// thousands of them, all due long ago; a request that arrived yesterday must
// not wait behind them. And the run stops taking jobs when its budget is
// spent (privacyRunBudget).
//
// WHAT A JOB IS RUN ON IS READ WHEN THE JOB IS TAKEN, never when the run
// began: the job's own row, which a request may have written since the due
// read, and its store's row, which a reconnect may have. A purge decided on a
// store row an hour old deleted a store that had come back.
func (c *Connector) RunDueComplianceJobs(ctx context.Context) (int, error) {
	started := c.now()
	var jobs []ComplianceJob
	due := renderCall("complianceJobsDue", map[string]any{"asOf": started.UTC().Format(time.RFC3339)})
	walkErr := memql.WalkQueryPages(operatorContext(ctx), c.engine.Execute, due, complianceJobPages, func(res *memql.ExecuteResult) error {
		for _, row := range memql.MaterializeRows(res) {
			jobs = append(jobs, complianceJobOf(row))
		}
		return nil
	})
	if walkErr != nil && len(jobs) == 0 {
		return 0, fmt.Errorf("shopify: read due compliance jobs: %w", walkErr)
	}
	// New requests first, each kind in the order the read gave: oldest due.
	slices.SortStableFunc(jobs, func(a, b ComplianceJob) int {
		switch {
		case a.repeatOfAnOldOne() == b.repeatOfAnOldOne():
			return 0
		case b.repeatOfAnOldOne():
			return -1
		}
		return 1
	})

	run := &privacyRun{c: c, repaired: map[string]bool{}}
	defer run.close()
	ran := 0
	for i, job := range jobs {
		if err := ctx.Err(); err != nil {
			return ran, err
		}
		if c.now().Sub(started) >= privacyRunBudget {
			c.logger.Info("shopify: the privacy run's budget is spent; the jobs left wait for the next run", "ran", ran, "left", len(jobs)-i)
			break
		}
		if run.take(ctx, job) {
			ran++
		}
	}
	if walkErr != nil {
		// What was read was run. The rest is still due and is read next time.
		return ran, fmt.Errorf("shopify: read due compliance jobs: %w", walkErr)
	}
	return ran, nil
}

// repeatOfAnOldOne reports whether a due job is an erasure an earlier version
// recorded done, which is made again: the one job that is due while done.
func (j ComplianceJob) repeatOfAnOldOne() bool { return j.Status == jobDone }

// privacyRun is one run of the privacy jobs: the session its statements run
// on, opened when the first job needs one and closed with the run, and what
// the run has done once and does not do again.
type privacyRun struct {
	c       *Connector
	session *privacySession
	done    func()
	// repaired are the stores whose rows this run has looked through for
	// what an earlier erasure left (repairOldErasure): once a store a run.
	repaired map[string]bool
	// storeVersion is the version of the store's row the job in hand was
	// read on: what a write that depends on that read is fenced on.
	storeVersion time.Time
}

// open answers the run's session, opening it at first use.
func (r *privacyRun) open(ctx context.Context) (*privacySession, error) {
	if r.session != nil {
		// THE SESSION CAN BE GONE. It is held for a whole run, and the server
		// ends one that has been idle past its limit (five minutes): between
		// two jobs, while the origin is asked or the Library removes an
		// export. Held blindly, every later job of the run failed on it. So
		// it is asked a nothing before it is used, and opened again if it
		// does not answer.
		if r.session.alive(ctx) {
			return r.session, nil
		}
		r.c.logger.Info("shopify: the privacy run's session was gone and is opened again")
		r.close()
	}
	s, done, err := r.c.openPrivacySession(ctx, "a privacy job")
	if err != nil {
		return nil, failedJob(failSession, err)
	}
	r.session, r.done = s, done
	return s, nil
}

func (r *privacyRun) close() {
	if r.done != nil {
		r.done()
	}
	r.session, r.done = nil, nil
}

// take runs one due job, on its row and its store's as they stand now, and
// records what the run leaves. It reports whether the job ran without failing.
func (r *privacyRun) take(ctx context.Context, due ComplianceJob) bool {
	c := r.c
	// The pass starts before its job is read: a request the read does not
	// see was recorded no earlier than this.
	started := c.now()
	job, version, found, err := c.complianceJobByID(ctx, due.ID)
	if err != nil {
		c.logger.Error("shopify: a privacy job's row could not be read; it stays due", "topic", due.Topic, "store", due.StoreID, "job", due.ID, "class", errorClass(err))
		return false
	}
	if !found || !job.dueAt(c.now()) {
		// Gone, or run since the due read by a request or another run.
		return false
	}
	if job.repeatOfAnOldOne() {
		// An old erasure being made again is due from NOW: it was recorded
		// done long ago, and a first failure of its repeat is a day old only
		// a day from here (the verdict's day of grace).
		taken := map[string]any{"jobId": job.ID, "status": jobPending, "dueAt": c.now().UTC().Format(time.RFC3339)}
		if err := c.writeJob(ctx, "recordComplianceJob", job.ID, version, taken); err != nil {
			c.logger.Error("shopify: an old erasure could not be taken up again; it stays due", "store", job.StoreID, "job", job.ID, "class", errorClass(err))
			return false
		}
		if job, version, found, err = c.complianceJobByID(ctx, due.ID); err != nil || !found {
			c.logger.Error("shopify: a privacy job's row could not be read; it stays due", "topic", due.Topic, "store", due.StoreID, "job", due.ID, "class", errorClass(err))
			return false
		}
	}

	step, err := r.run(ctx, job)
	if err != nil {
		// A closed sentence and the error's class, and never its text.
		c.logger.Error("shopify: a privacy job failed and is run again at the next run",
			"topic", job.Topic, "store", job.StoreID, "job", job.ID, "what", lastErrorOf(err), "class", errorClass(err))
		c.recordJobFailed(ctx, job, version, lastErrorOf(err))
		return false
	}
	c.recordJobStep(ctx, job, version, started, step)
	return true
}

// dueAt reports whether a job's row, read fresh, is still one to run now.
func (j ComplianceJob) dueAt(now time.Time) bool {
	if j.DueAt.After(now) {
		return false
	}
	switch j.Status {
	case jobPending, jobFailed, "":
		return true
	case jobDone:
		return j.Topic == TopicRedact && j.ScrubbedAt.IsZero()
	}
	return false
}

// run reads the job's store as it is now and runs the job on it.
func (r *privacyRun) run(ctx context.Context, job ComplianceJob) (jobStep, error) {
	store, version, found, err := r.c.storeAsItIs(ctx, job.StoreID)
	if err != nil {
		return jobStep{}, failedJob(failStoreRead, err)
	}
	r.storeVersion = version
	return r.job(ctx, store, found, job)
}

// storeAsItIs reads a store's row fresh, by id, with the version of the row
// that was read: the witness a write decided on it is fenced on.
func (c *Connector) storeAsItIs(ctx context.Context, storeID string) (Store, time.Time, bool, error) {
	res, err := c.engine.Execute(memql.ContextWithFreshRead(operatorContext(ctx)), renderCall("storeById", map[string]any{"storeId": storeID}))
	if err != nil {
		return Store{}, time.Time{}, false, err
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return Store{}, time.Time{}, false, nil
	}
	store, ok := storeFromRow(rows[0])
	return store, rowTime(rows[0], "createdAt"), ok, nil
}

// writeJob makes one write of a job's row, fenced on the version of the row
// that was read: it is refused, with memql.ErrRowVersionChanged, if anything
// has written the row since.
func (c *Connector) writeJob(ctx context.Context, mutation, jobID string, version time.Time, args map[string]any) error {
	fenced := memql.ContextWithRowVersionFence(operatorContext(ctx), conceptComplianceJob, jobID, version)
	_, err := c.engine.Execute(fenced, renderCall(mutation, args))
	return err
}

// recordJobStep writes what a run that did not fail leaves on its job's row.
// A second pass still to come leaves the job pending, with when the first ran
// and when the second is due; a job that is done is stamped as completed. An
// earlier failure's sentence is cleared either way.
//
// AN ERASURE IS RECORDED DONE ONLY IF ITS LAST REQUEST IS OLDER THAN THE PASS
// THAT FINISHED (ruling 92). Otherwise the request may name orders the pass
// never read, and the erasure is opened again for both passes, with what the
// pass did as its outcome. Two things say a request is not older:
//
//   - the time it was recorded at is not before the pass started;
//   - the row was written since the pass read it, and the job stands where
//     the pass found it. Every write here is fenced on the version of the row
//     the pass ran on, so such a write is seen as a refusal; and the only
//     writer that leaves a job's status and its first pass as they were is a
//     request. This is the one that holds on two nodes whose clocks disagree:
//     it asks the row, and not the time a request says it was recorded at.
//
// A write refused because ANOTHER RUN moved the job (its status or its first
// pass is not what this pass found) is not made at all: the row says where
// the job stands, and this pass does not write over it.
func (c *Connector) recordJobStep(ctx context.Context, job ComplianceJob, version, started time.Time, step jobStep) {
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
	if step.ExportID != "" {
		args["exportId"] = step.ExportID
	}
	ends := step.Status == jobDone && (job.Topic == TopicRedact || job.Topic == TopicShopRedact)
	again := ends && !job.RequestedAt.IsZero() && !job.RequestedAt.Before(started)
	meanwhile := "; a request for the same erasure arrived while this pass ran, so the erasure is made again"
	if job.Topic == TopicDataRequest {
		// A data request is due at once, so the time it was recorded at says
		// nothing here; only a write of its row while it ran does (below).
		meanwhile = "; the same request was delivered again while this ran, so the job is run again to delete that delivery"
	}
	for try := 0; ; try++ {
		var err error
		if again {
			reopen := map[string]any{"jobId": job.ID, "outcome": step.Outcome + meanwhile}
			// The export the pass filed stays on the job: the run that
			// follows the reopening must find it, or it files a second one.
			if step.ExportID != "" {
				reopen["exportId"] = step.ExportID
			}
			err = c.writeJob(ctx, "reopenComplianceJob", job.ID, version, reopen)
		} else {
			err = c.writeJob(ctx, "recordComplianceJob", job.ID, version, args)
		}
		if err == nil {
			return
		}
		if !errors.Is(err, memql.ErrRowVersionChanged) || try >= jobWriteTries {
			c.logger.Warn("shopify: could not record a privacy job's outcome", "job", job.ID, "class", errorClass(err))
			return
		}
		at, moved, ok := c.jobSince(ctx, job)
		if !ok || moved {
			return
		}
		// A request reached the job while the pass ran. One that leaves the
		// job waiting takes nothing from it: the write is made again on the
		// row as it is. One that would have ended it does not end it: an
		// erasure is made again for what the request may name, and any job
		// is run again for the delivery the request arrived by, which this
		// pass did not delete.
		version = at
		again = step.Status == jobDone
	}
}

// recordJobFailed writes a failed run: the status and the sentence. What an
// earlier pass left -- its outcome and when it ran -- is not named, so it
// stays, and the next run takes the job up where it stands: a failed first
// pass is a first pass again, a failed second pass a second pass. A request
// that reached the job meanwhile takes nothing from the failure, and the write
// is made again on the row as it is.
func (c *Connector) recordJobFailed(ctx context.Context, job ComplianceJob, version time.Time, sentence string) {
	args := map[string]any{"jobId": job.ID, "status": jobFailed, "lastError": sentence}
	for try := 0; ; try++ {
		err := c.writeJob(ctx, "recordComplianceJob", job.ID, version, args)
		if err == nil {
			return
		}
		if !errors.Is(err, memql.ErrRowVersionChanged) || try >= jobWriteTries {
			c.logger.Warn("shopify: could not record a privacy job's failure", "job", job.ID, "class", errorClass(err))
			return
		}
		at, moved, ok := c.jobSince(ctx, job)
		if !ok || moved {
			return
		}
		version = at
	}
}

// jobSince reads a job's row again after a write of it was refused, and
// answers the row's version now and whether the job has MOVED since the pass
// read it: its status or its first pass is not what the pass found, which is
// another run's doing. A row that was written and has not moved was written
// by a request. ok is false when the row could not be read, or is gone.
func (c *Connector) jobSince(ctx context.Context, job ComplianceJob) (version time.Time, moved, ok bool) {
	now, at, found, err := c.complianceJobByID(ctx, job.ID)
	if err != nil || !found {
		c.logger.Warn("shopify: a privacy job's row could not be read again to record its run", "job", job.ID, "class", errorClass(err))
		return time.Time{}, false, false
	}
	if now.Status != job.Status || !now.ScrubbedAt.Equal(job.ScrubbedAt) {
		c.logger.Info("shopify: a privacy job was moved by another run while this one worked it; its row is left as it stands", "job", job.ID, "status", now.Status)
		return at, true, true
	}
	return at, false, true
}

// runComplianceJob performs one run of one privacy request, on a session of
// its own, and reports what it leaves on the job's row. found says whether
// the job's store still has a row; store is that row, or nothing.
func (c *Connector) runComplianceJob(ctx context.Context, store Store, found bool, job ComplianceJob) (jobStep, error) {
	run := &privacyRun{c: c, repaired: map[string]bool{}}
	defer run.close()
	return run.job(ctx, store, found, job)
}

// job performs one run of one privacy request and reports what it leaves on
// the job's row.
//
// A JOB WHOSE STORE ROW IS GONE IS NOT A FAILURE. The mirror's rows can
// outlive the row that configured them, so an erasure runs by the job's own
// store id; only the store's backfills are left alone, as nothing reads a
// store that is not configured. A data request for such a store has nothing
// configured to export and nobody to file an export under, and is done.
func (r *privacyRun) job(ctx context.Context, store Store, found bool, job ComplianceJob) (jobStep, error) {
	if job.StoreID == "" {
		job.StoreID = store.ID
	}
	switch job.Topic {
	case TopicDataRequest:
		return r.dataRequest(ctx, store, found, job)
	case TopicRedact:
		return r.customerErasure(ctx, store, found, job)
	case TopicShopRedact:
		return r.shopErasure(ctx, store, found, job)
	}
	return jobStep{}, failedJob(failNotATopic, fmt.Errorf("shopify: %q is not a compliance topic", job.Topic))
}

// dataRequest answers one data request: it files what the mirror holds on the
// customer in the store owner's Library, and deletes the deliveries that
// asked.
//
// A REQUEST IS ANSWERED ONCE. A job that has filed its export (exportId) and
// is run again -- the same request was delivered again after it was answered
// -- files nothing: the Library holds the export, and a redelivery does not
// make another. It deletes the delivery that brought it back, and is done
// again.
//
// A CUSTOMER WHO HAS BEEN ERASED IS NOT EXPORTED. Her erasure removes the
// exports her data requests filed, at each of its two passes, and that covers
// every export filed before its second pass. What it cannot cover is a data
// request that runs AFTER her erasure: one delivered again, one that failed
// and is run again, one whose export a slow pass filed late. So a data
// request first asks whether her erasure has been made (customerErased); if
// it has, it files nothing, removes whatever export of hers is in the Library
// (the erasure's own removal, removeExports), deletes its deliveries and is
// done. The two orders of events end the same way, with no export of her:
//
//   - the data request first, her erasure after: the erasure's passes remove
//     what was filed;
//   - her erasure first, the data request after: the data request files
//     nothing and removes what it finds.
//
// An erasure that is waiting for its hold has not been made: a data request
// that runs then files, and the erasure removes it when it runs.
func (r *privacyRun) dataRequest(ctx context.Context, store Store, found bool, job ComplianceJob) (jobStep, error) {
	c := r.c
	now := c.now()
	if job.CustomerGID != "" {
		erased, err := c.customerErased(ctx, job)
		if err != nil {
			return jobStep{}, failedJob(failErasedRead, err)
		}
		if erased {
			const since = "the customer has been erased, so nothing is filed"
			removed, err := c.removeExports(ctx, job)
			if err != nil {
				return jobStep{}, err
			}
			asked, err := r.finishAsked(ctx, job, true)
			if err != nil {
				return jobStep{}, err
			}
			_ = c.auditCompliance(ctx, job.StoreID, "shopify_data_request_customer_erased", job, now, since+removed)
			return jobStep{Status: jobDone, Outcome: since + removed + asked}, nil
		}
	}
	if job.ExportID != "" {
		const before = "the request was answered before, and its export is not filed again"
		asked, err := r.finishAsked(ctx, job, true)
		if err != nil {
			return jobStep{}, err
		}
		_ = c.auditCompliance(ctx, job.StoreID, "shopify_data_request_delivered_again", job, now, before)
		return jobStep{Status: jobDone, Outcome: before + asked, ExportID: job.ExportID}, nil
	}
	if !found {
		const nothing = "the store is no longer configured; nothing to export"
		asked, err := r.finishAsked(ctx, job, true)
		if err != nil {
			return jobStep{}, err
		}
		_ = c.auditCompliance(ctx, job.StoreID, "shopify_data_request_not_exported", job, now, nothing)
		return jobStep{Status: jobDone, Outcome: nothing + asked}, nil
	}
	if job.CustomerGID == "" {
		return jobStep{}, failedJob(failNoCustomer, nil)
	}
	// Who the export will belong to, BEFORE a row of the person is read: an
	// export nobody can be given is not collected.
	if store.OwnerUserID == "" {
		return jobStep{}, failedJob(failExportOwner, fmt.Errorf("shopify: store %q has no ownerUserId", store.ID))
	}
	s, err := r.open(ctx)
	if err != nil {
		return jobStep{}, err
	}
	export, err := c.exportCustomerData(ctx, s, store, job.CustomerGID, job.OrderGIDs)
	if err != nil {
		return jobStep{}, failedJob(failExportRead, err)
	}
	artifact, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return jobStep{}, failedJob(failExportRead, err)
	}
	if err := c.writeExportArtifact(ctx, store, job, artifact); err != nil {
		return jobStep{}, failedJob(failExportWrite, err)
	}
	what := fmt.Sprintf("exported %d bytes", len(artifact))
	// The request has what it asked for; the delivery that asked holds the
	// customer's email and phone, and has no further use.
	asked, err := r.finishAsked(ctx, job, true)
	if err != nil {
		return jobStep{}, err
	}
	_ = c.auditCompliance(ctx, job.StoreID, "shopify_data_request_exported", job, now, what)
	return jobStep{Status: jobDone, Outcome: what + asked, ExportID: exportIDOf(store.ID, job)}, nil
}

// customerErased reports whether the erasure of a data request's customer has
// been made, in that store: a customers/redact job of hers whose first pass
// has run, or that is done. One that is waiting for its hold has not.
func (c *Connector) customerErased(ctx context.Context, job ComplianceJob) (bool, error) {
	erased := false
	erasures := renderCall("complianceJobsOfCustomer", map[string]any{"storeId": job.StoreID, "topic": TopicRedact, "customerGid": job.CustomerGID})
	err := memql.WalkQueryPages(memql.ContextWithFreshRead(operatorContext(ctx)), c.engine.Execute, erasures, complianceJobPages, func(res *memql.ExecuteResult) error {
		for _, row := range memql.MaterializeRows(res) {
			if erasure := complianceJobOf(row); erasure.Status == jobDone || !erasure.ScrubbedAt.IsZero() {
				erased = true
			}
		}
		return nil
	})
	return erased, err
}

// customerErasure is one pass of a customer's erasure: the scrub of the
// mirror's rows, the bodies of the store's processed deliveries, on the first
// pass what an earlier erasure left, and the store's backfills.
//
// THE PASS IS AUDITED AS RUN ONLY WHEN ALL OF IT RAN. A pass that failed
// part-way is run again whole, and an audit trail that said "redacted" for it
// would say so of an erasure an old export could still undo.
func (r *privacyRun) customerErasure(ctx context.Context, store Store, found bool, job ComplianceJob) (jobStep, error) {
	c := r.c
	if job.CustomerGID == "" {
		return jobStep{}, failedJob(failNoCustomer, nil)
	}
	s, err := r.open(ctx)
	if err != nil {
		return jobStep{}, err
	}
	second := !job.ScrubbedAt.IsZero()
	erased, scope, err := redactCustomer(ctx, s, job.StoreID, job.CustomerGID, job.OrderGIDs)
	if err != nil {
		return jobStep{}, failedJob(failScrub, err)
	}
	what := fmt.Sprintf("scrubbed %d row version(s) of %d row(s)", erased.Versions, erased.Rows)
	if erased.Earlier > 0 {
		what += fmt.Sprintf(", among them %d earlier version(s) of rows that are no longer hers", erased.Earlier)
	}
	if erased.Alone > 0 {
		what += fmt.Sprintf(", and %d deleted customer record(s) whose last order is hers, each by itself", erased.Alone)
	}
	if erased.Unlisted > 0 {
		what += fmt.Sprintf(", and took the customer out of %d version(s) of rows that referred to her", erased.Unlisted)
	}
	// THE BACKFILLS IN FLIGHT ARE RESTARTED RIGHT AFTER THE SCRUB, before
	// anything else the pass owes. The scrub is what an export the origin took
	// before it would undo, and a pass that keeps failing further down (an
	// export that cannot be removed, a delivery that cannot be blanked) is
	// made again and again: restarted last, the backfills never were.
	after, err := c.afterErasure(ctx, store, found, !second)
	if err != nil {
		return jobStep{}, err
	}
	blanked, err := blankProcessedDeliveries(ctx, s, job.StoreID)
	if err != nil {
		return jobStep{}, failedJob(failDeliveries, err)
	}
	if blanked > 0 {
		what += fmt.Sprintf("; blanked the body of %d version(s) of the store's processed deliveries", blanked)
	}
	exports, err := c.removeExports(ctx, job)
	if err != nil {
		return jobStep{}, err
	}
	what += exports
	// ONCE A STORE, EVER. What the old erasure left cannot be left anew, and
	// the look reads every row of the store in every kind; so the store's row
	// records that it ran, and no later erasure looks again. A store whose
	// row is gone has nowhere to record it, and is looked through once a run.
	// The deliveries of privacy requests and uninstalls from before a job
	// remembered them go in the same place, under the same record.
	if !second && !r.repaired[job.StoreID] && !(found && mapString(store.Health, healthOldErasureRepaired) != "") {
		// A session a slow removal outlasted is opened again here.
		if s, err = r.open(ctx); err != nil {
			return jobStep{}, err
		}
		repaired, err := r.repairOldErasure(ctx, s, store, found, job.StoreID, scope)
		if err != nil {
			return jobStep{}, err
		}
		what += repaired
		if found {
			older, err := deleteOlderPrivacyDeliveries(ctx, s, store, job.DeliveryIDs)
			if err != nil {
				return jobStep{}, failedJob(failOlder, err)
			}
			if older > 0 {
				what += fmt.Sprintf("; deleted %d version(s) of the shop's older privacy and uninstall deliveries", older)
			}
		}
		r.repaired[job.StoreID] = true
		r.recordOnce(ctx, store, found)
	}
	// The deliveries that asked: blanked while the job has a pass to come,
	// deleted when this pass ends it.
	asked, err := r.finishAsked(ctx, job, second)
	if err != nil {
		return jobStep{}, err
	}
	what += asked
	_ = c.auditCompliance(ctx, job.StoreID, passAction("shopify_customer_redacted", second), job, c.now(), what)
	return c.erasurePass(second, what+after), nil
}

// recordOnce records on the store's row that the once-a-store work ran: one
// key of its health, by the fenced write every such key is written by. Not
// recorded, the work runs again at the next erasure, which costs a look and
// changes nothing.
func (r *privacyRun) recordOnce(ctx context.Context, store Store, found bool) {
	if !found {
		return
	}
	if err := r.c.recordStoreHealthKey(ctx, store.ID, healthOldErasureRepaired, r.c.now().UTC().Format(time.RFC3339), nil); err != nil {
		r.c.logger.Warn("shopify: could not record that a store's once-only erasure work ran; the next erasure does it again", "store", store.ID, "class", errorClass(err))
	}
}

// shopErasure is one pass of a shop's erasure: the purge of the store's
// mirror, its sync state and its staged deliveries, unless the store is back.
//
// THE ORIGIN IS ASKED BEFORE EVERY PURGE, UNLESS THE STORE'S ROW ALREADY
// RECORDS THE UNINSTALL (ruling 97), the second purge exactly as the first,
// and on the row as it is now (privacyRun.run reads it when the job is
// taken). A row that records the uninstall is one the uninstall's own notice
// was applied to: there is nothing to ask. Any other row, whatever its status
// -- live, configured, or paused by a person -- is of a store that may be
// installed, and is purged only if the origin refuses the app outright
// (storeAnswers). One that answers is not purged. The question used to be put
// only to a store whose row said it ingests, so a paused store that was still
// installed was purged without being asked; and now that a delivery a job has
// finished with is deleted, a captured shop/redact replayed is a new
// delivery, which this is what stops from purging an installed store.
//
// A timeout, a 5xx or a network error says nothing about the store: the job
// fails, nothing is deleted, and the next run asks again.
//
// THE STAMP IS FENCED ON THE ROW THAT WAS READ. The mark that a store was
// redacted is written only if the store's row is still the version the purge
// was decided on. A reconnect that lands between the read and the stamp has
// written the row; the stamp is then not written, the store's unstarted
// backfills are queued so that what the purge removed is read again, and the
// job ends done, saying so. What else can have written the row, and how the
// two are told apart, is stampRedacted's.
func (r *privacyRun) shopErasure(ctx context.Context, store Store, found bool, job ComplianceJob) (jobStep, error) {
	c := r.c
	second := !job.ScrubbedAt.IsZero()
	if found && store.UninstalledAt == "" {
		back, err := c.storeAnswers(ctx, store)
		if err != nil {
			return jobStep{}, failedJob(failReach, err)
		}
		if back {
			c.logger.Info("shopify: skipping shop/redact purge, the store is reachable again", "store", store.ID, "secondPass", second)
			// THE STORE'S UNSTARTED BACKFILLS ARE QUEUED EVERY TIME A PURGE IS
			// SKIPPED. A run before this one may have purged the store, found
			// it connected again at the stamp, and failed to queue what reads
			// it again; and nothing on the job's row can be trusted to say
			// so, because a later failure writes its own sentence over that
			// one. This run does not purge, and is the one that pays. Queueing
			// is safe to do twice and leaves a kind whose backfill has
			// started, so a store that is whole has nothing queued, and the
			// outcome then says nothing of it.
			queued, err := c.queueBackfillsAPurgeOwes(ctx, store.ID)
			if err != nil {
				return jobStep{}, failedJob(failReadAgain, err)
			}
			owed := ""
			if queued > 0 {
				owed = fmt.Sprintf("; queued %d backfill(s) of the store that had not started", queued)
			}
			// The job ends here, and the deliveries that asked go as any done
			// job's do. Its outcome is the fixed sentence, and what was queued.
			if _, err := r.finishAsked(ctx, job, true); err != nil {
				return jobStep{}, err
			}
			if second {
				const kept = "second pass: the store is reachable again and was not purged a second time; its reconnect reads everything again"
				_ = c.auditCompliance(ctx, job.StoreID, "shopify_shop_redact_second_pass_skipped", job, c.now(), kept+owed)
				return jobStep{Status: jobDone, Outcome: kept + owed}, nil
			}
			const skipped = "skipped: the store is reachable again"
			_ = c.auditCompliance(ctx, job.StoreID, "shopify_shop_redact_skipped", job, c.now(), skipped+owed)
			return jobStep{Status: jobDone, Outcome: skipped + owed}, nil
		}
	}
	s, err := r.open(ctx)
	if err != nil {
		return jobStep{}, err
	}
	purged, err := purgeMirror(ctx, s, job.StoreID)
	if err != nil {
		return jobStep{}, failedJob(failPurge, err)
	}
	what := fmt.Sprintf("purged %d row version(s)", purged.Rows)
	if purged.Deliveries > 0 {
		what += fmt.Sprintf(" and %d version(s) of the store's staged deliveries", purged.Deliveries)
	}
	// The backfills in flight are started again right after the purge, before
	// the deliveries and the stamp, as a customer's erasure starts them right
	// after its scrub: a pass that keeps failing further down would otherwise
	// never reach them.
	after, err := c.afterErasure(ctx, store, found, !second)
	if err != nil {
		return jobStep{}, err
	}
	if found {
		// The delivery that carried the store's uninstall goes with the
		// purge (its own uninstall deliveries among the ids the row
		// remembers, and nothing else those ids name), and so, once, do the
		// shop's privacy and uninstall deliveries from before a job
		// remembered them.
		var gone int64
		var err error
		if statement, args, ok := rememberedUninstalls(store); ok {
			gone, err = s.exec(ctx, statement, args...)
		}
		if err == nil && mapString(store.Health, healthOldErasureRepaired) == "" {
			var older int64
			older, err = deleteOlderPrivacyDeliveries(ctx, s, store, job.DeliveryIDs)
			gone += older
		}
		if err != nil {
			return jobStep{}, failedJob(failOlder, err)
		}
		if gone > 0 {
			what += fmt.Sprintf("; deleted %d version(s) of the shop's uninstall and older privacy deliveries", gone)
		}
	}
	if found {
		connected, err := c.stampRedacted(ctx, store, r.storeVersion)
		if errors.Is(err, errStoreDidNotSay) {
			return jobStep{}, failedJob(failStamp, err)
		}
		if err != nil {
			return jobStep{}, failedJob(failPurge, err)
		}
		if connected {
			// The store was connected again while it was being purged. It is
			// not redacted; what the purge removed is read again.
			queued, err := c.queueBackfillsAPurgeOwes(ctx, store.ID)
			if err != nil {
				// The job is run again, and that run finds the store answering
				// and does not purge it: it queues them, as every skip does.
				return jobStep{}, failedJob(failReadAgain, err)
			}
			if _, err := r.finishAsked(ctx, job, true); err != nil {
				return jobStep{}, err
			}
			what += fmt.Sprintf("; the store was connected again while it was being purged, so it is not marked redacted, and %d backfill(s) were queued to read again what the purge removed", queued)
			_ = c.auditCompliance(ctx, job.StoreID, "shopify_shop_redact_overtaken", job, c.now(), what)
			return jobStep{Status: jobDone, Outcome: what + after}, nil
		}
		if mapString(store.Health, healthOldErasureRepaired) == "" {
			// Nothing of the store is left for the old erasure's repair to find.
			r.recordOnce(ctx, store, found)
		}
	}
	asked, err := r.finishAsked(ctx, job, second)
	if err != nil {
		return jobStep{}, err
	}
	what += asked
	_ = c.auditCompliance(ctx, job.StoreID, passAction("shopify_shop_redacted", second), job, c.now(), what)
	return c.erasurePass(second, what+after), nil
}

// stampRedacted marks a store redacted, if its row is still the version the
// purge was decided on, and reports whether the store was CONNECTED AGAIN
// instead, in which case nothing is written.
//
// A ROW THAT MOVED IS NOT ALWAYS A STORE THAT CAME BACK. Three things write a
// store's row while a purge runs: a connect; the uninstall's own notice,
// arriving late; and the watch, which writes other keys of the row's health.
// Only the first means the purge was overtaken, and a purge that took the
// other two for it would leave an uninstalled store without its mark and
// without its second purge. So when the fenced write is refused the row is
// read again and asked what happened to it:
//
//   - it SHOWS A CONNECT: the uninstall or the redaction it recorded is
//     cleared, a grant it did not name is on it, or its recording was started
//     afresh. Nothing else writes those. The store was connected again.
//   - it shows none and RECORDS AN UNINSTALL: the store is uninstalled, as the
//     row says. The stamp is made on the row as it is.
//   - it shows none and RECORDS NO UNINSTALL: the purge was decided on the
//     origin's refusal, and a connect can leave such a row's fields exactly
//     as they were (a grant renewed under the name it had). The row cannot
//     say, so the origin is asked again, as it was before the purge
//     (storeAnswers): it answers, the store was connected again; it refuses,
//     the stamp is made on the row as it is; it does not say, the job fails
//     (errStoreDidNotSay) and the next run purges and stamps again.
func (c *Connector) stampRedacted(ctx context.Context, read Store, version time.Time) (connected bool, err error) {
	at := read.RedactedAt
	if at == "" {
		at = c.now().UTC().Format(time.RFC3339)
	}
	for try := 0; try <= jobWriteTries; try++ {
		fenced := memql.ContextWithRowVersionFence(operatorContext(ctx), conceptShopifyStore, read.ID, version)
		_, err = c.engine.Execute(fenced, renderCall("markStoreRedacted", map[string]any{"storeId": read.ID, "redactedAt": at}))
		if err == nil {
			c.stores.Invalidate()
			return false, nil
		}
		if !errors.Is(err, memql.ErrRowVersionChanged) {
			return false, fmt.Errorf("shopify: stamp store redaction: %w", err)
		}
		now, moved, found, rerr := c.storeAsItIs(ctx, read.ID)
		if rerr != nil {
			return false, fmt.Errorf("shopify: read the store again to stamp its redaction: %w", rerr)
		}
		if !found {
			return false, nil
		}
		if showsAConnect(read, now) {
			return true, nil
		}
		if now.UninstalledAt == "" {
			back, aerr := c.storeAnswers(ctx, now)
			if aerr != nil {
				return false, fmt.Errorf("%w: %w", errStoreDidNotSay, aerr)
			}
			if back {
				return true, nil
			}
		}
		version = moved
	}
	return false, fmt.Errorf("shopify: stamp store redaction: %w", err)
}

// errStoreDidNotSay is a stamp that could not be decided: the store's row
// moved while it was purged, and the origin did not say whether the app is
// still installed.
var errStoreDidNotSay = errors.New("shopify: the store did not say whether the app is still installed")

// showsAConnect reports whether a store's row, read again, shows that the
// store was connected since it was first read: what a connect writes, and
// nothing else does. An uninstall that was recorded is cleared, or a
// redaction; a grant the row did not name is on it; or the time its recording
// started afresh has changed. An uninstall that has APPEARED is the opposite,
// and so is a grant that has gone.
func showsAConnect(read, now Store) bool {
	return (read.UninstalledAt != "" && now.UninstalledAt == "") ||
		(read.RedactedAt != "" && now.RedactedAt == "") ||
		(now.AdminTokenRef != "" && now.AdminTokenRef != read.AdminTokenRef) ||
		(now.ConnectedAt != "" && now.ConnectedAt != read.ConnectedAt)
}

// queueBackfillsAPurgeOwes queues the backfill of each of a store's domains
// that has none started, and answers how many: what a purge that a reconnect
// overtook owes the store. A domain whose backfill the reconnect itself has
// queued or started since is left to it. It is the purge's own count of what
// it queued, read from the store as it is now; queueUnstartedBackfillsOf
// (plugin.go) is the boot's and the resume's, for a store already in hand.
//
// It is asked for at two moments: when a purge finds at its stamp that the
// store was connected again, and every time a purge is skipped because the
// store answers (shopErasure says why every time).
func (c *Connector) queueBackfillsAPurgeOwes(ctx context.Context, storeID string) (int, error) {
	store, _, found, err := c.storeAsItIs(ctx, storeID)
	if err != nil || !found {
		return 0, err
	}
	queued := 0
	for _, concept := range generated.ApplyOrder {
		spec := generated.Types[concept]
		if !bulkDomain(spec) || !domainInConnection(store, spec) {
			continue
		}
		stateID := memqlsync.StateID(generated.ConceptID(concept), ConnectorName, string(memqlsync.DirectionInbound), storeID)
		res, err := c.engine.Execute(memql.ContextWithFreshRead(operatorContext(ctx)), renderCall("syncStateById", map[string]any{"stateId": stateID}))
		if err != nil {
			return queued, fmt.Errorf("the sync state of %s could not be read: %w", concept, err)
		}
		if rows := memql.MaterializeRows(res); len(rows) > 0 && mapString(rows[0], "conceptId") != "" && !neverBackfilled(rows[0]) {
			continue
		}
		if err := c.queueBackfill(ctx, storeID, concept, false); err != nil {
			return queued, fmt.Errorf("the backfill of %s could not be queued: %w", concept, err)
		}
		queued++
	}
	return queued, nil
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
// goes -- the operation, the part of a multi-part domain it was and the line
// it had reached -- so the next start begins a new export at the first part,
// with a start time of its own. When the dropped export was started stays:
// that export wrote rows, and the read of every order is owed after it
// (refundDetailOwed); a cursor with no operation names no export to resume.
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
	kept := backfillCursor{Restarts: cur.Restarts, Rule: cur.Rule, At: cur.At}
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

// storeAnswers asks the store's origin a trivial question, to learn whether
// the app is still installed there, and answers one of three things: the
// store answered (true); the origin refused the app OUTRIGHT (false); or
// nothing can be said, as an error.
//
// A refusal is the origin's own 401 -- the token is not one it accepts: the
// app was uninstalled, or the token revoked -- or its 404, there is no such
// shop; or, for a store on the shared app, the token endpoint's refusal to
// renew its grant (below). EVERYTHING ELSE IS NOT AN ANSWER: a timeout, a 5xx,
// a throttle the tries ran out on, a 403, a network error, a grant this node
// could not resolve. A purge decided on one of those deleted the mirror of a
// store that was connected and slow.
func (c *Connector) storeAnswers(ctx context.Context, store Store) (bool, error) {
	_, err := c.adminCall(ctx, store, `query ShopifyReachable { shop { id } }`, "ShopifyReachable", nil)
	if err == nil {
		return true, nil
	}
	var refused *AdminError
	if errors.As(err, &refused) && (refused.StatusCode == http.StatusUnauthorized || refused.StatusCode == http.StatusNotFound) {
		return false, nil
	}
	// A store on the shared app holds a grant that is renewed, and for an
	// uninstalled app it is the renewal the origin refuses, before the Admin
	// API is ever asked: the token endpoint answers 401 invalid_request to a
	// refresh token it no longer honours (offline_token.go, grantRefused).
	var gone grantRefused
	if errors.As(err, &gone) {
		return false, nil
	}
	return false, err
}

// writeExportArtifact puts the data-request export in the Library, under
// the store's owner, and records the Shopify request id with it.
func (c *Connector) writeExportArtifact(ctx context.Context, store Store, job ComplianceJob, body []byte) error {
	name := exportNameOf(store.ID, job)
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
		"outputId": exportIDOf(store.ID, job),
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

// exportNameOf is the title a data request's export is filed under, and
// exportIDOf the id of the Library row that holds it: both made of the store
// and the request, so the same request files the same row, and an erasure of
// the customer can name what her data requests filed.
func exportNameOf(storeID string, job ComplianceJob) string {
	if job.RequestID == "" {
		return fmt.Sprintf("shopify-data-request-%s-%s.json", storeID, job.ReceivedAt.Format("20060102T150405Z"))
	}
	return fmt.Sprintf("shopify-data-request-%s-%s.json", storeID, job.RequestID)
}

func exportIDOf(storeID string, job ComplianceJob) string {
	return "shpdr" + MirrorRowID(storeID, exportNameOf(storeID, job))
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
	// THE IDS ARE REFUSED HERE IF THEY ARE NOT IDS. Shopify sends a customer
	// and an order by number; a gid is accepted too. Anything else -- a word,
	// a number with a pattern character in it, nothing at all -- is not a
	// customer this request can be run for, and is refused at the door, where
	// the staged delivery's own row says so, rather than queued as a job no
	// run can complete. The scope names rows by these ids and matches them as
	// text (erasureScopeOf), which is the other reason they have one form.
	if hasCustomer {
		job.CustomerGID = "gid://shopify/Customer/" + strings.TrimPrefix(firstString(cust, "id"), "gid://shopify/Customer/")
		if !customerGIDForm.MatchString(job.CustomerGID) {
			return ComplianceJob{}, fmt.Errorf("shopify: %s names a customer by an id that is not a customer's", job.Topic)
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
			order := "gid://shopify/Order/" + strings.TrimPrefix(strings.TrimSpace(fmt.Sprintf("%v", v)), "gid://shopify/Order/")
			if !orderGIDForm.MatchString(order) {
				return ComplianceJob{}, fmt.Errorf("shopify: %s names an order by an id that is not an order's", job.Topic)
			}
			job.OrderGIDs = append(job.OrderGIDs, order)
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

// PurgeStore deletes a store's whole mirror, its sync state and its staged
// deliveries, and stamps the store redacted.
//
// IT IS THE PURGE AND NOTHING BEFORE IT: it asks the origin nothing and its
// stamp is not fenced, so it is for a caller that has already decided. A
// shop's erasure does not call it. That one reads the store as it is, asks
// the origin unless the row records the uninstall, and fences its stamp on
// the row it read (shopErasure, stampRedacted).
//
// staged-data: MUST-NOT-GATE -- a staged row skipped here SURVIVES A PURGE.
// This is shop/redact: the obligation is that the store's data is gone, and a
// row the delete could not see is a row that stays in the table forever with
// nothing left to find it (the store row is paused, the sync state is
// deleted, and no later pass looks at this concept for this store again). The
// same argument component/identity/authactivity's prune makes, with a legal
// deadline attached rather than a retention window.
func (c *Connector) PurgeStore(ctx context.Context, store Store) (int64, error) {
	s, done, err := c.openPrivacySession(ctx, "shop/redact")
	if err != nil {
		return 0, err
	}
	defer done()
	purged, err := c.purgeStore(ctx, s, store)
	return purged.Rows, err
}

// purgeResult is what a purge deleted: versions of the mirror's rows, and
// versions of the store's staged deliveries.
type purgeResult struct {
	Rows, Deliveries int64
}

// purgeStore is PurgeStore on a session that is open: the purge, and the
// stamp on the store's row that says it was made.
func (c *Connector) purgeStore(ctx context.Context, s *privacySession, store Store) (purgeResult, error) {
	purged, err := purgeMirror(ctx, s, store.ID)
	if err != nil {
		return purged, err
	}
	stamp := renderCall("markStoreRedacted", map[string]any{
		"storeId":    store.ID,
		"redactedAt": c.now().UTC().Format(time.RFC3339),
	})
	if _, err := c.engine.Execute(operatorContext(ctx), stamp); err != nil {
		return purged, fmt.Errorf("shopify: stamp store redaction: %w", err)
	}
	c.stores.Invalidate()
	return purged, nil
}

// conceptInboundRequest is the staged delivery's concept: every webhook's raw
// body, kept on the row the receiver staged it under.
const conceptInboundRequest = "v1:platform:inboundRequest"

// purgeMirror deletes every mirror row of a store, its sync state and its
// staged deliveries, by the store's id, and stamps nothing: a store whose row
// is gone has none to stamp, and the second purge of a shop's erasure finds
// the first one's stamp there.
//
// IT REFUSES AN EMPTY STORE ID. Every statement here names the store, and
// with no id each would name the rows that carry none.
//
// THE STAGED DELIVERIES GO WITH THE MIRROR (ruling 90): every delivery of the
// store's own source, in every version and whatever its status. A webhook's
// raw body is kept on its staged row, and an order's or a customer's carries
// the person's name, email and address; a purge that left them would leave
// the store's customers in the table under another concept.
//
// staged-data: MUST-NOT-GATE -- a staged row skipped here SURVIVES A PURGE
// (PurgeStore says why that is for ever).
func purgeMirror(ctx context.Context, s *privacySession, storeID string) (purgeResult, error) {
	var purged purgeResult
	if storeID == "" {
		return purged, fmt.Errorf("shopify: a purge needs the store's id, and was given none; nothing was deleted")
	}
	for _, concept := range generated.ApplyOrder {
		n, err := s.exec(ctx,
			`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'storeId' = $2`,
			generated.ConceptID(concept), storeID)
		if err != nil {
			return purged, fmt.Errorf("shopify: purge %s: %w", concept, err)
		}
		purged.Rows += n
	}
	// The runtime's health rows for this connector's domains go too.
	// Leaving them would have a re-install resume a backfill cursor into
	// a mirror that no longer has the rows the cursor counted. A store's
	// rows are partitions of the mirror and carry it as partitionKey, not
	// storeId; both are matched, so a row of either shape goes.
	if _, err := s.exec(ctx,
		`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'connector' = $1 AND (payload->>'storeId' = $2 OR payload->>'partitionKey' = $2)`,
		ConnectorName, storeID); err != nil {
		return purged, fmt.Errorf("shopify: purge sync state: %w", err)
	}
	n, err := s.exec(ctx,
		`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'source' = $2`,
		conceptInboundRequest, storeInboundSource(storeID))
	if err != nil {
		return purged, fmt.Errorf("shopify: purge the store's staged deliveries: %w", err)
	}
	purged.Deliveries = n
	return purged, nil
}

// deliveryBlankBatch is how many staged deliveries one statement blanks, and
// deliveryFindPage how many one read of the store's deliveries answers.
// Variables, so a test can make both loops go round with a handful of rows.
var (
	deliveryBlankBatch = 500
	deliveryFindPage   = 100000
)

// blankProcessedDeliveries blanks the body of every staged delivery of a
// store's own source that is PROCESSED, in every version of its row, and
// answers how many versions it changed (ruling 90).
//
// WHAT A BODY IS. The receiver stages a webhook's raw body, exactly as it was
// signed, in the string field `body` of a v1:platform:inboundRequest row, and
// every later write of the row -- a status, a try -- is a new version that
// carries the body again. The field is required: a version without the key is
// refused by the concept's schema at the next write of the row. So a body is
// blanked to the EMPTY STRING, with its key in place, in each version that
// holds one; the row stays one the engine reads and writes.
//
// A processed delivery's content has no further use: the mirror holds what it
// said. One that is failed, held for another try or given up keeps its body,
// because a later try, or an operator's requeue, applies it; so does one that
// is still to be worked.
//
// IN BATCHES, and in one read of the store's deliveries for each hundred
// thousand of them. A store's deliveries are every webhook it ever sent, so
// the deliveries to blank are read once, by id and nothing wider, and then
// blanked five hundred at a time, each by its row id. Reading a batch at a
// time instead read the whole of what was left for every batch: a hundred
// thousand deliveries took twenty-two seconds that way, and ten times as many
// would have taken a hundred times as long.
//
// staged-data: MUST-NOT-GATE -- a delivery's row skipped here keeps the
// person's name, email and address in its body after her erasure.
func blankProcessedDeliveries(ctx context.Context, s *privacySession, storeID string) (int64, error) {
	if storeID == "" {
		return 0, fmt.Errorf("shopify: blanking a store's deliveries needs the store's id")
	}
	var total int64
	after := ""
	for {
		// The inner read answers a version's id, its time, its status and
		// whether it holds a body, and nothing of the body itself: what is
		// sorted to find each delivery's newest version is then a few bytes a
		// version, not the webhook it carries. OFFSET 0 keeps it so.
		found, err := s.rows(ctx,
			`SELECT v.id FROM (
			   SELECT m.id, m."createdAt", m.payload->>'status' AS status, COALESCE(m.payload->>'body', '') <> '' AS held
			     FROM "MemoryNodes" m
			    WHERE m.concept = $1 AND m.payload->>'source' = $2 AND m.id > $3
			   OFFSET 0) v
			  GROUP BY v.id
			 HAVING (array_agg(v.status ORDER BY v."createdAt" DESC))[1] = 'processed' AND bool_or(v.held)
			  ORDER BY v.id LIMIT $4`,
			conceptInboundRequest, storeInboundSource(storeID), after, deliveryFindPage)
		if err != nil {
			return total, err
		}
		ids := make([]string, 0, len(found))
		for _, row := range found {
			ids = append(ids, row[0])
		}
		for len(ids) > 0 {
			n := min(len(ids), deliveryBlankBatch)
			changed, err := s.exec(ctx,
				`UPDATE "MemoryNodes" m SET payload = jsonb_set(m.payload, '{body}', '""'::jsonb, true)
				  WHERE m.concept = $1 AND m.id IN (SELECT jsonb_array_elements_text($2::jsonb))
				    AND COALESCE(m.payload->>'body', '') <> ''`,
				conceptInboundRequest, jsonArray(ids[:n]))
			if err != nil {
				return total, err
			}
			total += changed
			ids = ids[n:]
		}
		if len(found) < deliveryFindPage {
			return total, nil
		}
		after = found[len(found)-1][0]
	}
}

// removeExports removes from the Library every export a data request of the
// erased customer filed for this store, and answers what it did as part of
// the job's outcome (ruling 93).
//
// A data request files everything MemQL holds on the person in the store
// owner's Library, and the index row's summary names her. Scrubbing the
// mirror leaves that document where it is, so her erasure removes it: on both
// passes, because a data request can end between them.
//
// WHICH EXPORTS. Each data-request job of this store and customer says what
// it filed (exportId); one that ran before the field existed filed under the
// id made of the store and the request (exportIDOf), which is asked for as
// well. A job that filed nothing has nothing there, and the removal says so.
//
// BY THE LIBRARY'S OWN REMOVAL, libraryRemoveArtifact: the Library's rows are
// the Library's to find, and the connector deletes none of them. It removes
// every version of the export, its index row, its history and the comments on
// it, and answers what a second look still found. Anything but "nothing
// remains" fails the pass, which is then made again.
func (c *Connector) removeExports(ctx context.Context, job ComplianceJob) (string, error) {
	var filed []string
	requests := renderCall("complianceJobsOfCustomer", map[string]any{"storeId": job.StoreID, "topic": TopicDataRequest, "customerGid": job.CustomerGID})
	err := memql.WalkQueryPages(memql.ContextWithFreshRead(operatorContext(ctx)), c.engine.Execute, requests, complianceJobPages, func(res *memql.ExecuteResult) error {
		for _, row := range memql.MaterializeRows(res) {
			request := complianceJobOf(row)
			for _, id := range []string{request.ExportID, exportIDOf(job.StoreID, request)} {
				if id != "" && !slices.Contains(filed, id) {
					filed = append(filed, id)
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", failedJob(failExports, fmt.Errorf("read the customer's data requests: %w", err))
	}
	var removed, exports int64
	for _, id := range filed {
		res, err := c.engine.Execute(operatorContext(ctx), "builtin "+renderCall("libraryRemoveArtifact", map[string]any{"sourceConceptRef": "v1:library:generatedOutput:" + id}))
		if err != nil {
			return "", failedJob(failExports, err)
		}
		rows := memql.MaterializeRows(res)
		if len(rows) != 1 {
			return "", failedJob(failExports, fmt.Errorf("the Library's removal answered %d row(s)", len(rows)))
		}
		gone, remaining := wholeNumber(rows[0]["removed"]), wholeNumber(rows[0]["remaining"])
		if gone < 0 || remaining != 0 {
			return "", failedJob(failExports, fmt.Errorf("the Library's removal answered removed %v, remaining %v", rows[0]["removed"], rows[0]["remaining"]))
		}
		if gone > 0 {
			exports++
			removed += gone
		}
	}
	if exports == 0 {
		return "", nil
	}
	return fmt.Sprintf("; removed %d export(s) her data requests had filed in the owner's Library (%d row version(s))", exports, removed), nil
}

// wholeNumber is a count as a builtin answers one, or -1 for anything that
// is not a whole number: a missing answer must not read as zero.
//
// narrowing: GUARDED -- a count is a whole number from 0 to 2^53, the range a
// float carries exactly; anything outside it, a fraction or a negative number
// reads as "no answer" (-1). core/num's helpers would saturate or answer 0,
// and 0 is the one answer that must not be invented here: it says nothing of
// the export remains.
func wholeNumber(v any) int64 {
	const most = 1 << 53
	switch n := v.(type) {
	case int:
		if n >= 0 {
			return int64(n)
		}
	case int64:
		if n >= 0 && n <= most {
			return n
		}
	case float64:
		if n >= 0 && n <= most && n == math.Trunc(n) {
			return int64(n)
		}
	case json.Number:
		if i, err := n.Int64(); err == nil && i >= 0 && i <= most {
			return i
		}
	}
	return -1
}

// blankAsked blanks the bodies of the deliveries that asked for a job, by
// their row ids, in every version and whatever their status, and answers
// what it did as part of the job's outcome (ruling 95).
//
// A privacy delivery's body holds the customer's id, her email address and
// her phone number. The job holds what the delivery asked, so once the job's
// work is done the body has no use. And blanking "the store's own source"
// never reaches these for a managed store: its privacy deliveries arrive on
// the app-level source. So a job remembers the deliveries that asked for it,
// and they are blanked at each pass that leaves the job waiting. The pass
// that ends the job deletes them instead (finishAsked).
//
// staged-data: MUST-NOT-GATE -- a delivery's row skipped here keeps the
// customer's email and phone after the request that named her is done.
func (r *privacyRun) blankAsked(ctx context.Context, job ComplianceJob) (string, error) {
	if len(job.DeliveryIDs) == 0 {
		return "", nil
	}
	s, err := r.open(ctx)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(job.DeliveryIDs))
	for _, id := range job.DeliveryIDs {
		if !strings.HasPrefix(id, conceptInboundRequest+":") {
			id = conceptInboundRequest + ":" + id
		}
		ids = append(ids, id)
	}
	n, err := s.exec(ctx,
		`UPDATE "MemoryNodes" m SET payload = jsonb_set(m.payload, '{body}', '""'::jsonb, true)
		  WHERE m.concept = $1 AND m.id IN (SELECT jsonb_array_elements_text($2::jsonb))
		    AND COALESCE(m.payload->>'body', '') <> ''`,
		conceptInboundRequest, jsonArray(ids))
	if err != nil {
		return "", failedJob(failAsked, err)
	}
	if n == 0 {
		return "", nil
	}
	return fmt.Sprintf("; blanked the body of %d version(s) of the deliveries that asked for it", n), nil
}

// finishAsked deals with the deliveries that asked for a job when a pass has
// done its work: BLANKED while the job has a pass to come, DELETED when the
// pass ends it (ruling 96).
//
// WHY DELETED. The receiver names a staged delivery by the digest of its
// signed body, staging keeps a stored row's status, and the dispatcher skips
// a row that is not waiting to be worked. A shop's erasure request says the
// shop's id and its domain and nothing else, so a second one, after a
// reinstall and a second uninstall, has the first one's body: it landed on
// the first one's row, already processed, and never reached the connector.
// The same for a second erasure of the same customer. A request the connector
// has finished with is therefore not kept as a delivery, and the same words
// arriving again are a new one. While the job is not done its deliveries
// stay, blanked: a redelivery then lands on the same row and is not new.
func (r *privacyRun) finishAsked(ctx context.Context, job ComplianceJob, ends bool) (string, error) {
	if !ends {
		return r.blankAsked(ctx, job)
	}
	if len(job.DeliveryIDs) == 0 {
		return "", nil
	}
	s, err := r.open(ctx)
	if err != nil {
		return "", err
	}
	n, err := deleteDeliveries(ctx, s, job.DeliveryIDs)
	if err != nil {
		return "", failedJob(failAsked, err)
	}
	if n == 0 {
		return "", nil
	}
	return fmt.Sprintf("; deleted the deliveries that asked for it (%d version(s))", n), nil
}

// deliveryRowIDs is staged deliveries' ids as the table holds them.
func deliveryRowIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if !strings.HasPrefix(id, conceptInboundRequest+":") {
			id = conceptInboundRequest + ":" + id
		}
		out = append(out, id)
	}
	return out
}

// deleteDeliveries deletes staged deliveries by their row ids, every version,
// and answers how many versions went.
//
// staged-data: MUST-NOT-GATE -- a delivery's row skipped here keeps its place,
// and the same request arriving again lands on it and is never worked.
func deleteDeliveries(ctx context.Context, s *privacySession, ids []string) (int64, error) {
	rows := deliveryRowIDs(ids)
	if len(rows) == 0 {
		return 0, nil
	}
	return s.exec(ctx, `DELETE FROM "MemoryNodes" WHERE concept = $1 AND id IN (SELECT jsonb_array_elements_text($2::jsonb))`,
		conceptInboundRequest, jsonArray(rows))
}

// olderDeliveryTopics are the topics whose deliveries a job remembers now and
// did not before: the three privacy requests, and the uninstall.
var olderDeliveryTopics = []string{TopicDataRequest, TopicRedact, TopicShopRedact, TopicAppUninstalled}

// deleteOlderPrivacyDeliveries deletes, once a store, the processed
// deliveries of a shop's privacy requests and uninstalls that nothing
// remembers: those staged before a job kept the ids of the deliveries that
// asked for it. Left where they are, each is the row the same request lands
// on when it arrives again (finishAsked). It answers how many versions went.
//
// HOW THE SHOP IS FOUND. By what the delivery's own row records. A row of the
// store's own source (shopify-<store id>) is the store's by its source. A row
// of the app-level source is the shop's when either of two things on it names
// the shop's domain: the headers the receiver staged with it
// (x-shopify-shop-domain), or the body it was signed with, in the field the
// connector bound it to its store by (shop_domain for a privacy request,
// myshopify_domain for an uninstall; managedAppLevelStore). The connector
// refuses to apply an app-level delivery whose header disagrees with the
// domain signed in its body, so on a processed row the two agree where both
// are there. The header is what is left when an erasure has blanked the body,
// and the body is what there is on a row staged without the header. In the
// body the field is matched as a key with its value, quotes and all, which a
// copy of those words inside another value cannot be: there every quote is
// escaped. The topic is read from the headers (x-shopify-topic), where alone
// a row keeps it.
//
// WHAT SOMETHING STILL REMEMBERS IS NOT OLDER, and stays: the deliveries of a
// job of the store that is not done, which go when that job ends (a
// redelivery of a request that is still waiting has to land on its row, and
// not be a new delivery); the delivery of the store's uninstall, which goes
// when the store is connected again or purged; and the deliveries except, the
// job in hand's own.
//
// One read of the two sources' rows, narrow, then the deletes by row id.
//
// staged-data: MUST-NOT-GATE -- a delivery's row skipped here keeps its place,
// and the same request arriving again lands on it and is never worked; and a
// job's row skipped would have the delivery it is still waiting on deleted.
func deleteOlderPrivacyDeliveries(ctx context.Context, s *privacySession, store Store, except []string) (int64, error) {
	if store.ID == "" || store.Domain == "" {
		return 0, nil
	}
	remembered, err := s.rows(ctx,
		`SELECT DISTINCT d.id
		   FROM (SELECT DISTINCT ON (j.id) j.payload FROM "MemoryNodes" j
		          WHERE j.concept = $1 AND j.payload->>'storeId' = $2
		          ORDER BY j.id, j."createdAt" DESC) job
		  CROSS JOIN LATERAL jsonb_array_elements_text(
		        CASE WHEN jsonb_typeof(job.payload->'deliveryIds') = 'array' THEN job.payload->'deliveryIds' ELSE '[]'::jsonb END) AS d(id)
		  WHERE COALESCE(job.payload->>'status', '') <> $3`,
		conceptComplianceJob, store.ID, jobDone)
	if err != nil {
		return 0, err
	}
	except = append(append([]string{}, except...), uninstallDeliveriesOf(store)...)
	for _, row := range remembered {
		except = append(except, row[0])
	}
	var topics []string
	for _, topic := range olderDeliveryTopics {
		topics = append(topics, `v.headers LIKE `+sqlText(`%"x-shopify-topic":"`+likeEscape(strings.ToLower(topic))+`"%`))
	}
	// The body is looked at for the app-level source's rows alone: the
	// store's own are its by their source, and are the many and the large.
	found, err := s.rows(ctx,
		`SELECT v.id FROM (
		   SELECT m.id, m."createdAt", m.payload->>'status' AS status, m.payload->>'source' AS source,
		          lower(COALESCE(m.payload->>'headersJson', '')) AS headers,
		          CASE WHEN m.payload->>'source' = $2 THEN COALESCE(m.payload->>'body', '') ELSE '' END AS body
		     FROM "MemoryNodes" m
		    WHERE m.concept = $1 AND m.payload->>'source' IN ($2, $3)
		   OFFSET 0) v
		  WHERE (v.source = $3 OR v.headers LIKE $4 OR v.body ~* $5) AND (`+strings.Join(topics, " OR ")+`)
		  GROUP BY v.id
		 HAVING (array_agg(v.status ORDER BY v."createdAt" DESC))[1] = 'processed'`,
		conceptInboundRequest, ConnectorName, storeInboundSource(store.ID),
		shopInHeaders(store.Domain), shopInBody(store.Domain))
	if err != nil {
		return 0, err
	}
	kept := deliveryRowIDs(except)
	var ids []string
	for _, row := range found {
		if !slices.Contains(kept, row[0]) {
			ids = append(ids, row[0])
		}
	}
	var total int64
	for len(ids) > 0 {
		n := min(len(ids), deliveryBlankBatch)
		gone, err := deleteDeliveries(ctx, s, ids[:n])
		if err != nil {
			return total, err
		}
		total += gone
		ids = ids[n:]
	}
	return total, nil
}

// shopInHeaders is the LIKE pattern that finds a shop's domain among the
// headers the receiver staged with a delivery (lowercased), and shopInBody the
// case-insensitive expression that finds it in the delivery's signed body, in
// the field the connector binds an app-level delivery to its store by
// (deleteOlderPrivacyDeliveries says why a copy inside another value does not
// match).
func shopInHeaders(domain string) string {
	return `%"x-shopify-shop-domain":"` + likeEscape(strings.ToLower(domain)) + `"%`
}

func shopInBody(domain string) string {
	return `"(shop_domain|myshopify_domain)"\s*:\s*"` + regexp.QuoteMeta(domain) + `"`
}

// rememberedUninstalls is the delete of the deliveries a store's row remembers
// as having carried its uninstall (uninstallDeliveriesOf), every version: the
// statement and its arguments, or false when there is nothing to delete.
//
// WHAT AN ID THERE NAMES IS NOT TAKEN ON TRUST. The ids are a key of the
// store's health, and nothing on the row can show that the uninstall's handler
// is what put an id in it. A delete by id alone would delete whatever a
// write of that key named: another connector's delivery, an order's. So the
// delete is bounded to what this path is for. A row goes only if it is
//
//   - named by an id the store remembers; and
//   - an uninstall, by the topic the receiver staged with it; and
//   - this connector's, for this store: on the store's own source, or on the
//     app-level source and the shop's, by the domain in its staged headers or
//     in its signed body.
//
// Anything else an id names is left where it is.
//
// staged-data: MUST-NOT-GATE -- the row left in place is the one a second
// identical uninstall lands on, processed, and is never worked.
func rememberedUninstalls(store Store) (string, []any, bool) {
	rows := deliveryRowIDs(uninstallDeliveriesOf(store))
	if len(rows) == 0 || store.ID == "" {
		return "", nil, false
	}
	return `DELETE FROM "MemoryNodes" d
	         WHERE d.concept = $1
	           AND d.id IN (
	             SELECT m.id FROM "MemoryNodes" m
	              WHERE m.concept = $1
	                AND m.id IN (SELECT jsonb_array_elements_text($2::jsonb))
	                AND lower(COALESCE(m.payload->>'headersJson', '')) LIKE $3
	                AND (m.payload->>'source' = $4
	                     OR (m.payload->>'source' = $5 AND $6 <> ''
	                         AND (lower(COALESCE(m.payload->>'headersJson', '')) LIKE $7 OR COALESCE(m.payload->>'body', '') ~* $8))))`,
		[]any{conceptInboundRequest, jsonArray(rows),
			`%"x-shopify-topic":"` + likeEscape(strings.ToLower(TopicAppUninstalled)) + `"%`,
			storeInboundSource(store.ID), ConnectorName, store.Domain, shopInHeaders(store.Domain), shopInBody(store.Domain)}, true
}

// rememberUninstallDelivery records on the store the delivery that carried
// its app/uninstalled (one health key, by the fenced write), so that it can
// be deleted when the store is connected again or purged. A shop's details do
// not change between two uninstalls, so the second notice has the first
// one's body and would land on the first one's row.
//
// It is ADDED to what the store remembers, and does not replace it: the same
// uninstall told again in other words is another delivery, and so is a notice
// a reinstall superseded, and one forgotten here would stay for good. store
// is the row as the uninstall's handler read it, fresh and under the store's
// lock.
func (c *Connector) rememberUninstallDelivery(ctx context.Context, store Store, req memqlsync.InboundRequest) {
	if req.RequestId == "" {
		return
	}
	remembered := unionOf(uninstallDeliveriesOf(store), []string{req.RequestId})
	if err := c.recordStoreHealthKey(ctx, store.ID, healthUninstallDeliveries, remembered, nil); err != nil {
		c.logger.Warn("shopify: could not remember the delivery that carried a store's uninstall; the same notice a second time may not be worked", "store", store.ID, "class", errorClass(err))
	}
}

// forgetUninstallDelivery deletes the deliveries that carried a store's
// uninstall, every version, and clears the key that remembered them: the
// store is connected again, and the same notice arriving after another
// uninstall must be a new delivery. A row or two by their ids, on the handle
// Connect holds; and only the store's own uninstall deliveries among what
// the ids name (rememberedUninstalls).
func (c *Connector) forgetUninstallDelivery(ctx context.Context, prior Store) {
	statement, args, ok := rememberedUninstalls(prior)
	if !ok {
		return
	}
	if c.db == nil || c.db() == nil {
		c.logger.Warn("shopify: a reconnected store's uninstall delivery could not be deleted: no database handle", "store", prior.ID)
		return
	}
	if _, err := c.db().ExecContext(ctx, statement, args...); err != nil {
		c.logger.Warn("shopify: a reconnected store's uninstall delivery could not be deleted", "store", prior.ID, "class", errorClass(err))
		return
	}
	if err := c.recordStoreHealthKey(ctx, prior.ID, healthUninstallDeliveries, []string{}, nil); err != nil {
		c.logger.Warn("shopify: a reconnected store still remembers its deleted uninstall delivery", "store", prior.ID, "class", errorClass(err))
	}
}

// repairOldErasure looks through a store's rows for what the erasure before
// this one left, repairs it, and answers what it did as part of the job's
// outcome (ruling 84, refined).
//
// WHAT THE OLD ERASURE LEFT. It wrote the string marker into each field of a
// hand-written list, whatever the field's type, in every row whose text held
// the customer's gid as a SUBSTRING. So it left the marker in objects, lists
// and booleans, which makes every later write of the row fail; and it left it
// in the rows of OTHER customers, whose gid begins with the erased one's.
//
// THE REPAIR follows ruling 81 for every field that is not free text, in every
// row of the store: a list that holds the marker becomes empty, anything else
// that holds it loses its key. A free-text string that reads "[redacted]" is
// left: it is a valid one, and here it cannot be told from one this erasure
// wrote.
//
// A ROW REPAIRED OUTSIDE THE JOB'S SCOPE IS ANOTHER PERSON'S, who never asked.
// The repair makes it writable; it cannot bring back the strings the old
// erasure wrongly scrubbed there (an order's number). Those come back only
// when the row is read again, so the store's backfill of each kind such a row
// is of is queued again from nothing. The kinds are queued BEFORE the rows
// are repaired: a job that fails between the two finds the marked rows again
// at its next run, where a repair made first would leave nothing to say that
// a kind is still owed its read.
//
// It runs at the first pass of every erasure of a customer, once a store a
// run (privacyRun.repaired): a job an earlier version left failed left
// markers as well as one it recorded done, and nothing on a job says which
// version first ran it. A store with nothing to repair costs the look and
// nothing more.
//
// staged-data: MUST-NOT-GATE -- a staged row the old erasure marked and this
// did not see stays unwritable: the mirror fails every later write of it.
func (r *privacyRun) repairOldErasure(ctx context.Context, s *privacySession, store Store, found bool, storeID string, scope *erasureScope) (string, error) {
	marked := map[string][]string{}
	total := 0
	for _, concept := range generated.ApplyOrder {
		lists, others := oldMarkerFields(generated.Types[concept])
		if len(lists)+len(others) == 0 {
			continue
		}
		rows, err := s.rows(ctx,
			`SELECT m.id FROM "MemoryNodes" m
			  WHERE m.concept = $1 AND m.payload->>'storeId' = $2 AND m.payload::text LIKE $3
			    AND EXISTS (SELECT 1 FROM jsonb_each(m.payload) e
			                 WHERE e.value = to_jsonb($4::text)
			                   AND e.key IN (SELECT jsonb_array_elements_text($5::jsonb)))
			  GROUP BY m.id`,
			generated.ConceptID(concept), storeID, `%"`+likeEscape(RedactionMarker)+`"%`, RedactionMarker, jsonArray(append(append([]string{}, lists...), others...)))
		if err != nil {
			return "", failedJob(failRepair, fmt.Errorf("look for what an earlier erasure left in %s: %w", concept, err))
		}
		for _, row := range rows {
			marked[concept] = append(marked[concept], row[0])
		}
		total += len(rows)
	}
	if total == 0 {
		return "", nil
	}

	// The kinds whose rows outside the scope were marked.
	inScope := map[string]bool{}
	for _, rows := range scope.Rows {
		for _, row := range rows {
			inScope[row.ID] = true
		}
	}
	var kinds []string
	for concept, ids := range marked {
		kind := backfillDomainOf(concept)
		if kind == "" || slices.Contains(kinds, kind) {
			continue
		}
		if slices.ContainsFunc(ids, func(id string) bool { return !inScope[id] }) {
			kinds = append(kinds, kind)
		}
	}
	slices.Sort(kinds)
	note := ""
	switch {
	case len(kinds) == 0:
	case !found:
		note = "; rows of other people among them are not read again, as the store is no longer configured"
	default:
		for _, kind := range kinds {
			if err := r.c.queueBackfill(ctx, store.ID, kind, true); err != nil {
				return "", failedJob(failRequeue, fmt.Errorf("queue the backfill of %s again: %w", kind, err))
			}
		}
		note = fmt.Sprintf("; rows of other people among them are read again: queued %d backfill(s) from nothing", len(kinds))
		r.c.logger.Info("shopify: rows an earlier erasure wrongly scrubbed are read again", "store", store.ID, "domains", strings.Join(kinds, ", "))
	}

	for _, concept := range generated.ApplyOrder {
		lists, others := oldMarkerFields(generated.Types[concept])
		for _, batch := range batches(marked[concept]) {
			if _, err := s.exec(ctx,
				`WITH repaired AS (
				   SELECT m.id, m."createdAt",
				          COALESCE((SELECT jsonb_object_agg(e.key, CASE
				                      WHEN e.key IN (SELECT jsonb_array_elements_text($3::jsonb)) AND e.value = to_jsonb($5::text) THEN '[]'::jsonb
				                      ELSE e.value END)
				                      FROM jsonb_each(m.payload) e
				                     WHERE NOT (e.key IN (SELECT jsonb_array_elements_text($4::jsonb)) AND e.value = to_jsonb($5::text))), '{}'::jsonb) AS payload
				     FROM "MemoryNodes" m
				    WHERE m.concept = $1 AND m.id IN (SELECT jsonb_array_elements_text($2::jsonb))
				 )
				 UPDATE "MemoryNodes" m SET payload = r.payload
				   FROM repaired r
				  WHERE m.concept = $1 AND m.id = r.id AND m."createdAt" = r."createdAt"
				    AND m.payload IS DISTINCT FROM r.payload`,
				generated.ConceptID(concept), jsonArray(batch), jsonArray(lists), jsonArray(others), RedactionMarker); err != nil {
				return "", failedJob(failRepair, fmt.Errorf("repair what an earlier erasure left in %s: %w", concept, err))
			}
		}
	}
	return fmt.Sprintf("; repaired %d row(s) an earlier erasure left unwritable", total) + note, nil
}

// healthOldErasureRepaired is the key of a store's health that says the
// store's rows were looked through for what the erasure before this one left,
// and when (repairOldErasure). A store that has it is not looked through
// again.
const healthOldErasureRepaired = "oldErasureRepairedAt"

// healthUninstallDeliveries is the key of a store's health that remembers
// the staged deliveries which carried its app/uninstalled: their rows' ids.
// One uninstall is one notice, and so one id, nearly always; a notice told
// again in other words, or one a reinstall superseded, is another.
const healthUninstallDeliveries = "uninstallDeliveryIds"

// uninstallDeliveriesOf is the deliveries a store's row remembers as having
// carried its uninstall.
func uninstallDeliveriesOf(store Store) []string {
	return mapStringSlice(store.Health, healthUninstallDeliveries)
}

// oldMarkerFields is the fields of a concept in which the string marker is
// not a value the field can hold as free text: the lists, which are emptied,
// and every other one, whose key is removed.
func oldMarkerFields(spec *generated.TypeSpec) (lists, others []string) {
	if spec == nil {
		return nil, nil
	}
	for _, f := range spec.Fields {
		switch erasureScrubFate(f) {
		case fateEmptied:
			lists = append(lists, f.Name)
		case fateRemoved:
			others = append(others, f.Name)
		}
	}
	return lists, others
}

// backfillDomainOf is the domain whose backfill reads a concept's rows: the
// concept itself, or the nearest ancestor a bulk operation backfills, by the
// generated tables' Parent link. Empty for a concept no backfill reads.
func backfillDomainOf(concept string) string {
	for range generated.ApplyOrder {
		spec := generated.Types[concept]
		if spec == nil {
			return ""
		}
		if bulkDomain(spec) {
			return concept
		}
		if spec.Parent == "" {
			return ""
		}
		concept = spec.Parent
	}
	return ""
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
		c.logger.Error("shopify: could not write compliance audit event", "action", action, "store", storeID, "class", errorClass(err))
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
		RequestedAt: rowTime(row, "requestedAt"),
		DeliveryIDs: mapStringSlice(row, "deliveryIds"),
		ExportID:    mapString(row, "exportId"),
	}
	if job.ID == "" {
		// The default projection is payload-only, so the row id is not in
		// it; recompute it from the fields that derived it.
		job.ID = ComplianceJobID(job.StoreID, job.Topic, job.CustomerGID+"\x00"+job.RequestID)
	}
	return job
}
