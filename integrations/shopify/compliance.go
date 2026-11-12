package shopify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	RequestedAt time.Time      `json:"requestedAt,omitempty"`
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
	failRepair      = "the customer's rows were scrubbed, but rows an earlier erasure left unwritable could not be repaired"
	failRequeue     = "rows an earlier erasure wrongly scrubbed could not be queued to be read again"
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
		}))
		return err
	}
	if job.Topic == TopicDataRequest {
		return nil
	}
	orders := unionOf(existing.OrderGIDs, job.OrderGIDs)
	if existing.Status == jobDone {
		_, err := c.engine.Execute(fenced, renderCall("reopenComplianceJob", map[string]any{
			"jobId":       job.ID,
			"dueAt":       job.DueAt.UTC().Format(time.RFC3339),
			"orderGids":   orders,
			"requestedAt": now,
		}))
		return err
	}
	_, err = c.engine.Execute(fenced, renderCall("extendComplianceJob", map[string]any{
		"jobId":       job.ID,
		"orderGids":   orders,
		"requestedAt": now,
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
}

// open answers the run's session, opening it at first use.
func (r *privacyRun) open(ctx context.Context) (*privacySession, error) {
	if r.session != nil {
		return r.session, nil
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
	store, found, err := r.c.storeByID(memql.ContextWithFreshRead(operatorContext(ctx)), job.StoreID)
	if err != nil {
		return jobStep{}, failedJob(failStoreRead, err)
	}
	return r.job(ctx, store, found, job)
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
	ends := step.Status == jobDone && (job.Topic == TopicRedact || job.Topic == TopicShopRedact)
	again := ends && !job.RequestedAt.IsZero() && !job.RequestedAt.Before(started)
	for try := 0; ; try++ {
		var err error
		if again {
			err = c.writeJob(ctx, "reopenComplianceJob", job.ID, version, map[string]any{
				"jobId":   job.ID,
				"outcome": step.Outcome + "; a request for the same erasure arrived while this pass ran, so the erasure is made again",
			})
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
		// row as it is. One that would have ended it does not end it.
		version = at
		again = ends
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

func (r *privacyRun) dataRequest(ctx context.Context, store Store, found bool, job ComplianceJob) (jobStep, error) {
	c := r.c
	now := c.now()
	if !found {
		const nothing = "the store is no longer configured; nothing to export"
		_ = c.auditCompliance(ctx, job.StoreID, "shopify_data_request_not_exported", job, now, nothing)
		return jobStep{Status: jobDone, Outcome: nothing}, nil
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
	_ = c.auditCompliance(ctx, job.StoreID, "shopify_data_request_exported", job, now, what)
	return jobStep{Status: jobDone, Outcome: what}, nil
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
	if erased.Unlisted > 0 {
		what += fmt.Sprintf(", and took the customer out of %d version(s) of rows that referred to her", erased.Unlisted)
	}
	blanked, err := blankProcessedDeliveries(ctx, s, job.StoreID)
	if err != nil {
		return jobStep{}, failedJob(failDeliveries, err)
	}
	if blanked > 0 {
		what += fmt.Sprintf("; blanked the body of %d version(s) of the store's processed deliveries", blanked)
	}
	if !second && !r.repaired[job.StoreID] {
		repaired, err := r.repairOldErasure(ctx, s, store, found, job.StoreID, scope)
		if err != nil {
			return jobStep{}, err
		}
		r.repaired[job.StoreID] = true
		what += repaired
	}
	after, err := c.afterErasure(ctx, store, found, !second)
	if err != nil {
		return jobStep{}, err
	}
	_ = c.auditCompliance(ctx, job.StoreID, passAction("shopify_customer_redacted", second), job, c.now(), what)
	return c.erasurePass(second, what+after), nil
}

// shopErasure is one pass of a shop's erasure: the purge of the store's
// mirror, its sync state and its staged deliveries, unless the store is back.
//
// THE STORE IS ASKED BEFORE EACH PURGE, the second exactly as the first, and
// on the row as it is now (privacyRun.run reads it when the job is taken). An
// uninstall an operator reverses within the hold must not cost the mirror;
// and a store that came back between the two purges has had everything read
// again by its reconnect, which a second purge would delete.
//
// ONLY A CLEAR REFUSAL BY THE ORIGIN SAYS THE STORE IS GONE. A store whose
// row says it ingests is asked, and it is purged only when the origin refuses
// the app outright (storeAnswers). A timeout, a 5xx or a network error says
// nothing about the store: the job fails, nothing is deleted, and the next
// run asks again.
func (r *privacyRun) shopErasure(ctx context.Context, store Store, found bool, job ComplianceJob) (jobStep, error) {
	c := r.c
	second := !job.ScrubbedAt.IsZero()
	if found && store.Ingests() && store.RedactedAt == "" {
		back, err := c.storeAnswers(ctx, store)
		if err != nil {
			return jobStep{}, failedJob(failReach, err)
		}
		if back {
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
	}
	s, err := r.open(ctx)
	if err != nil {
		return jobStep{}, err
	}
	var purged purgeResult
	if found && store.RedactedAt == "" {
		purged, err = c.purgeStore(ctx, s, store)
	} else {
		// No row to stamp, or one the first purge has stamped already.
		purged, err = purgeMirror(ctx, s, job.StoreID)
	}
	if err != nil {
		return jobStep{}, failedJob(failPurge, err)
	}
	what := fmt.Sprintf("purged %d row version(s)", purged.Rows)
	if purged.Deliveries > 0 {
		what += fmt.Sprintf(" and %d version(s) of the store's staged deliveries", purged.Deliveries)
	}
	after, err := c.afterErasure(ctx, store, found, !second)
	if err != nil {
		return jobStep{}, err
	}
	_ = c.auditCompliance(ctx, job.StoreID, passAction("shopify_shop_redacted", second), job, c.now(), what)
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

// storeAnswers asks the store's origin a trivial question, to learn whether
// the app is still installed there, and answers one of three things: the
// store answered (true); the origin refused the app OUTRIGHT (false); or
// nothing can be said, as an error.
//
// A refusal is the origin's own 401 -- the token is not one it accepts: the
// app was uninstalled, or the token revoked -- or its 404, there is no such
// shop. EVERYTHING ELSE IS NOT AN ANSWER: a timeout, a 5xx, a throttle the
// tries ran out on, a 403, a network error, a grant this node could not
// resolve. A purge decided on one of those deleted the mirror of a store that
// was connected and slow.
func (c *Connector) storeAnswers(ctx context.Context, store Store) (bool, error) {
	_, err := c.adminCall(ctx, store, `query ShopifyReachable { shop { id } }`, "ShopifyReachable", nil)
	if err == nil {
		return true, nil
	}
	var refused *AdminError
	if errors.As(err, &refused) && (refused.StatusCode == http.StatusUnauthorized || refused.StatusCode == http.StatusNotFound) {
		return false, nil
	}
	return false, err
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
// deliveries.
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
	}
	if job.ID == "" {
		// The default projection is payload-only, so the row id is not in
		// it; recompute it from the fields that derived it.
		job.ID = ComplianceJobID(job.StoreID, job.Topic, job.CustomerGID+"\x00"+job.RequestID)
	}
	return job
}
