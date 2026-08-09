package campaigns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/email"
)

// worker.go -- the drain loop.
//
// # The two identities, and why the engine needs no bypass
//
// A send touches rows belonging to somebody else, and there are exactly
// two ways to build such a thing: give the engine an authority that
// out-ranks the owner tier, or have it BORROW the owner's authority. This
// takes the second, and every read and write below is explicit about
// which context it runs under:
//
//	systemCtx  the engine's own operator identity. Reaches ONLY the two
//	           clusterOwner-tier engine concepts: the send-job queue and
//	           the suppression list. Both are engine bookkeeping; neither
//	           carries a recipient's address.
//	ownerCtx   the campaign owner's identity, built from the job row's
//	           campaignOwnerUserId -- which was itself copied off a
//	           campaign row the STARTING CALLER had already read under
//	           their own actor. Reaches the campaign, its template, its
//	           audience and its delivery ledger, with exactly the owner's
//	           authority and no more.
//
// The consequence worth stating: no code path here can read or write a
// row the campaign's owner could not. The owner tier is not weakened by
// the existence of a sender, and `ownerUserId` on a delivery row is
// stamped from `actor.userId` like every other write in the domain --
// which is what took v1:campaigns:delivery off the owner-stamping gate's
// exemption list (memql#3348).
//
// # Suppression outranks the audience, and the ORDER is the mechanism
//
// processRecipient checks the cluster suppression list BEFORE it looks at
// the recipient row's own subscriptionStatus. That ordering is the whole
// of "outranks every audience": a re-imported address whose recipient row
// says `subscribed` is still skipped, because the list is checked at the
// point of send rather than at the point the audience was built. Checking
// only at build time is the bug this ordering exists to prevent -- an
// audience assembled last month cannot know about last week's unsubscribe.
//
// # Idempotency is the ledger, not a flag
//
// The batch is "roster members with no delivery row, plus rows still
// `pending` whose retry is due". A recipient that reached `sent`,
// `skipped` or terminal `failed` is simply not in it. So a resumed send,
// a restarted process, and a claim lost to a dead replica all converge on
// the same behaviour without anything having to remember what it did.

const (
	// WorkerComponent is the Dependency ComponentName.
	WorkerComponent = common.ComponentName("campaigns.worker")

	// sendClaimName is the cross-replica claim namespace. Rows land in
	// the same automation_execution_claims ledger the automation cluster
	// guard uses, under a distinct name.
	sendClaimName = "campaignSend"

	// systemCampaignsActor is the engine's own operator identity for the
	// two clusterOwner-tier engine concepts. It is a synthetic cluster
	// owner, and the scope of that power is bounded by what this file
	// asks it for: two named queries and two named mutations, all over
	// v1:campaigns:sendJob and v1:campaigns:suppression. Every read that
	// touches a recipient runs under the owner instead.
	systemCampaignsActor = "system:campaigns"

	// Backoff for a retryable per-recipient failure: base 60s, factor 4,
	// cap 1h, +-20% jitter. Slower at the base than the outbound worker's
	// 30s because a marketing message has no latency requirement and a
	// tight retry against a struggling provider is what turns a soft
	// failure into a throttle.
	backoffBase   = 60 * time.Second
	backoffFactor = 4
	backoffCap    = time.Hour
)

// ExecutionClaimer is the cross-replica claim gate (the automations
// ClusterExecutionGuard satisfies it). Nil leaves a single-replica
// deployment correct -- the poll is the only driver -- but MUST be wired
// in the mesh: without it two replicas drain the same campaign
// concurrently and the only thing standing between that and a duplicate
// message is the ledger read, which they would both do before either
// wrote.
type ExecutionClaimer interface {
	ClaimWithTTL(ctx context.Context, name, dedupKey string, ttl time.Duration) bool
}

// Worker drains v1:campaigns:sendJob rows.
type Worker struct {
	store    *Store
	claimer  ExecutionClaimer
	resolve  func() email.Sender
	logger   *slog.Logger
	cfg      Config
	limiter  *rateLimiter
	now      func() time.Time
	sendHook func(ctx context.Context, sender email.Sender, msg email.Message) error

	cancel      context.CancelFunc
	running     atomic.Bool
	startOnce   sync.Once
	stopOnce    sync.Once
	readyCh     chan struct{}
	doneCh      chan struct{}
	mu          sync.Mutex
	sentTotal   atomic.Int64
	skipTotal   atomic.Int64
	failedTotal atomic.Int64
}

// NewWorker constructs the drain worker. resolveSender is called at send
// time rather than at construction, mirroring the outbound email
// transport: on a booting node the plug-in registry may not be populated
// yet, and a sender captured at wiring time would be nil forever.
func NewWorker(engine Engine, claimer ExecutionClaimer, resolveSender func() email.Sender, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	cfg := LoadConfig()
	return &Worker{
		store:   NewStore(engine),
		claimer: claimer,
		resolve: resolveSender,
		logger:  logger.With("component", string(WorkerComponent)),
		cfg:     cfg,
		limiter: newRateLimiter(cfg.SendRatePerMinute, time.Now),
		now:     time.Now,
		readyCh: make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
}

// Start launches the drain loop. Idempotent; a no-op when disabled or
// unwired.
func (w *Worker) Start(_ context.Context) {
	w.startOnce.Do(func() {
		defer close(w.readyCh)
		if !w.cfg.Enabled {
			w.logger.Info("campaigns worker: disabled (MEMQL_CAMPAIGNS_ENABLED=false)")
			close(w.doneCh)
			return
		}
		if w.store == nil || w.store.engine == nil {
			w.logger.Warn("campaigns worker: no engine wired; not starting")
			close(w.doneCh)
			return
		}
		// A loud, ONE-TIME warning rather than a refusal to start. The
		// worker still has work to do without an unsubscribe secret --
		// draining a paused or cancelled job to its terminal status -- and
		// the actual refusal lives at campaignStartSend, where it can be
		// reported to the operator who pressed the button instead of
		// buried in a boot log.
		if reason := w.cfg.RequireUnsubscribe(); reason != "" {
			w.logger.Warn("campaigns worker: one-click unsubscribe is NOT configured, so no campaign can be started", "reason", reason)
		}
		runCtx, cancel := context.WithCancel(context.Background())
		w.mu.Lock()
		w.cancel = cancel
		w.mu.Unlock()
		w.running.Store(true)
		go w.loop(runCtx)
	})
}

// Stop cancels the loop.
func (w *Worker) Stop(_ context.Context) {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		cancel := w.cancel
		w.cancel = nil
		w.mu.Unlock()
		if cancel != nil {
			cancel()
			<-w.doneCh
		}
		w.running.Store(false)
	})
}

// Dependency surface.
func (w *Worker) IsRunning() bool                     { return w.running.Load() }
func (w *Worker) Order() int                          { return 12 }
func (w *Worker) ComponentName() common.ComponentName { return WorkerComponent }
func (w *Worker) Ready() <-chan struct{}              { return w.readyCh }

func (w *Worker) loop(ctx context.Context) {
	defer close(w.doneCh)
	w.logger.Info("campaigns worker: started",
		"pollInterval", w.cfg.Poll.String(),
		"batchSize", w.cfg.BatchSize,
		"sendRatePerMinute", w.cfg.SendRatePerMinute,
		"maxAudience", w.cfg.MaxAudience)
	startup := time.NewTimer(w.cfg.StartupDelay)
	defer startup.Stop()
	select {
	case <-ctx.Done():
		return
	case <-startup.C:
	}
	ticker := time.NewTicker(w.cfg.Poll)
	defer ticker.Stop()
	w.DrainOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("campaigns worker: stopped")
			return
		case <-ticker.C:
			w.DrainOnce(ctx)
		}
	}
}

// DrainOnce runs one pass over every drainable job. Exported so a test
// drives the loop synchronously (the outbound worker's precedent) rather
// than racing a ticker.
func (w *Worker) DrainOnce(ctx context.Context) {
	systemCtx := w.systemActorContext(ctx)
	jobs, err := w.store.DrainableJobs(systemCtx)
	if err != nil {
		w.logger.Debug("campaigns worker: scan failed (engine likely not ready)", "error", err)
		return
	}
	for _, job := range jobs {
		select {
		case <-ctx.Done():
			return
		default:
		}
		w.processJob(ctx, systemCtx, job)
	}
}

func (w *Worker) processJob(ctx context.Context, systemCtx context.Context, job SendJob) {
	now := w.now().UTC()

	// THE PROVIDER'S limit, half one. A job the provider told us to back
	// off from is left entirely alone until its deadline -- no claim, no
	// read, no send. Claiming first and then discovering the throttle
	// would hold the lease for nothing and block the replica that will be
	// free sooner.
	if !job.ThrottledUntil.IsZero() && now.Before(job.ThrottledUntil) {
		return
	}
	if job.CampaignOwnerUserID == "" {
		w.failJob(systemCtx, job, "send job carries no campaign owner, so there is no identity to run the send under")
		return
	}

	// Cross-replica claim BEFORE any side effect, keyed per (job,
	// progress). Progress advances whenever a batch produced a terminal
	// outcome, so the next batch claims a fresh key; a batch that
	// produced none re-claims the same key and is correctly blocked until
	// the lease expires, which is what stops a permanently-failing job
	// from spinning on every tick of every replica.
	if w.claimer != nil {
		key := fmt.Sprintf("%s:%d", job.ID, job.Progress())
		if !w.claimer.ClaimWithTTL(ctx, sendClaimName, key, w.cfg.ClaimTTL) {
			return
		}
	}

	// Re-read under the claim: an operator may have paused the campaign
	// between the scan and here.
	fresh, ok, err := w.store.JobByID(systemCtx, job.ID)
	if err != nil {
		w.logger.Warn("campaigns worker: could not re-read send job", "job", job.ID, "error", err)
		return
	}
	if !ok || !isDrainable(fresh.Status) {
		return
	}
	job = fresh

	ownerCtx := w.ownerActorContext(ctx, job.CampaignOwnerUserID)
	campaign, found, err := w.store.CampaignByID(ownerCtx, job.CampaignID)
	if err != nil {
		w.logger.Warn("campaigns worker: could not read campaign", "job", job.ID, "error", err)
		return
	}
	if !found {
		// Either the campaign is gone or campaignOwnerUserId does not own
		// it. Both are terminal for the job and neither is recoverable by
		// retrying, so it fails loudly rather than spinning.
		w.failJob(systemCtx, job, "campaign is not readable as its recorded owner; refusing to send")
		return
	}
	if next, stop := terminalFromCampaignStatus(campaign.Status); stop {
		w.stopJob(systemCtx, job, next, fmt.Sprintf("campaign status is %q", campaign.Status))
		return
	}

	tmpl, found, err := w.store.TemplateByID(ownerCtx, job.TemplateID)
	if err != nil {
		w.logger.Warn("campaigns worker: could not read template", "job", job.ID, "error", err)
		return
	}
	if !found {
		w.failJob(systemCtx, job, "template is not readable; refusing to send")
		return
	}

	roster, err := w.store.Roster(ownerCtx, job.AudienceID)
	if err != nil {
		w.logger.Warn("campaigns worker: could not read audience roster", "job", job.ID, "error", err)
		return
	}
	// The ceiling is re-checked here and not only at start: an audience
	// can grow mid-send, and the roster read is bounded by the query's
	// paginate, so past the bound the worker would be diffing a TRUNCATED
	// roster and would silently declare the campaign complete having
	// never seen the tail.
	if len(roster) >= w.cfg.MaxAudience {
		w.failJob(systemCtx, job, fmt.Sprintf(
			"audience has reached the %d-recipient ceiling (MEMQL_CAMPAIGNS_MAX_AUDIENCE); "+
				"the roster read is bounded, so continuing would silently skip the remainder", w.cfg.MaxAudience))
		return
	}

	ledger, err := w.store.Ledger(ownerCtx, job.CampaignID)
	if err != nil {
		w.logger.Warn("campaigns worker: could not read delivery ledger", "job", job.ID, "error", err)
		return
	}

	batch := selectBatch(roster, ledger, now, w.cfg.BatchSize)
	if len(batch) == 0 {
		if pendingRemains(roster, ledger) {
			// Nothing due right now -- every outstanding recipient is
			// waiting out a backoff. Leave the job running.
			return
		}
		w.completeJob(systemCtx, ownerCtx, job, campaign, len(roster))
		return
	}

	if err := w.sendBatch(ctx, systemCtx, ownerCtx, &job, campaign, tmpl, batch); err != nil {
		w.logger.Warn("campaigns worker: batch ended early", "job", job.ID, "error", err)
	}
	w.stampProgress(systemCtx, ownerCtx, job, len(roster))
}

// sendBatch mails one batch, updating the job's in-memory counters as it
// goes so the caller can stamp them once at the end rather than once per
// recipient.
func (w *Worker) sendBatch(
	ctx context.Context,
	systemCtx, ownerCtx context.Context,
	job *SendJob,
	campaign Campaign,
	tmpl Template,
	batch []batchItem,
) error {
	sender := w.resolveSender()
	if sender == nil {
		w.failJob(systemCtx, *job, "no email sender is registered on this node; refusing to send")
		return errors.New("no email sender registered")
	}
	if reason := w.cfg.RequireUnsubscribe(); reason != "" {
		w.failJob(systemCtx, *job, reason)
		return errors.New(reason)
	}
	if job.StartedAt.IsZero() {
		started := w.now().UTC()
		running := "running"
		_ = w.store.UpdateJob(systemCtx, job.ID, SendJobPatch{Status: &running, StartedAt: &started})
		job.StartedAt = started
	}

	for _, item := range batch {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		// OUR limit. Non-blocking: an exhausted bucket ends the batch and
		// releases the claim rather than sleeping under it.
		if !w.limiter.Allow() {
			return nil
		}
		stop, err := w.processRecipient(ctx, systemCtx, ownerCtx, job, campaign, tmpl, item)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	return nil
}

// processRecipient handles exactly one address. Returns stop=true when
// the whole batch must end (a provider throttle).
func (w *Worker) processRecipient(
	ctx context.Context,
	systemCtx, ownerCtx context.Context,
	job *SendJob,
	campaign Campaign,
	tmpl Template,
	item batchItem,
) (bool, error) {
	r := item.recipient
	now := w.now().UTC()

	digest := EmailDigest(r.Email)
	if digest == "" {
		// Not a suppression and not a transport failure: the address
		// cannot be mailed at all, and retrying it will never help.
		// Terminal `failed` with the reason on the row.
		w.recordFailed(ownerCtx, job, campaign.ID, r, item.attempts+1, "recipient address is not a usable email address")
		return false, nil
	}

	// SUPPRESSION FIRST -- before the recipient row's own state. This
	// ordering IS the "outranks every audience" rule: an address
	// re-imported after a bounce has a recipient row saying `subscribed`,
	// and the cluster list is what still refuses it.
	if sup, found, err := w.store.SuppressionByDigest(systemCtx, digest); err == nil && found {
		w.recordSkipped(ownerCtx, job, campaign.ID, r, sup.Reason)
		// Converge this operator's own row onto the cluster verdict, so
		// the audience view stops showing the address as sendable. Done
		// under the OWNER's actor: the cluster list is authoritative, but
		// only the owner may write the owner's row.
		if status := recipientStatusFor(sup.Reason); status != "" && r.SubscriptionStatus != status {
			at := sup.SuppressedAt
			if at.IsZero() {
				at = now
			}
			if err := w.store.SetRecipientSubscription(ownerCtx, r.ID, status, at); err != nil {
				w.logger.Warn("campaigns worker: could not converge recipient subscription", "error", err)
			}
		}
		return false, nil
	} else if err != nil {
		// A suppression lookup that FAILED must not be read as "not
		// suppressed". Skip the recipient this batch and try again next
		// tick: a delayed message is recoverable, a message sent to
		// somebody who opted out is not.
		w.logger.Warn("campaigns worker: suppression lookup failed; leaving recipient for the next batch",
			"job", job.ID, "error", err)
		return true, nil
	}

	if r.SubscriptionStatus != "" && r.SubscriptionStatus != "subscribed" {
		w.recordSkipped(ownerCtx, job, campaign.ID, r, r.SubscriptionStatus)
		return false, nil
	}

	token, err := MintUnsubscribeToken(w.cfg.UnsubscribeSecret, job.CampaignOwnerUserID, r.ID, campaign.ID)
	if err != nil {
		w.failJob(systemCtx, *job, "cannot mint an unsubscribe token: "+err.Error())
		return true, err
	}
	msg := renderMessage(campaign, tmpl, r, unsubscribeURL(w.cfg.UnsubscribeBaseURL, token))

	sendCtx, cancel := context.WithTimeout(ctx, w.cfg.SendTimeout)
	err = w.deliver(sendCtx, msg)
	cancel()

	attempts := item.attempts + 1
	if err == nil {
		job.SentCount++
		w.sentTotal.Add(1)
		if derr := w.store.RecordDelivery(ownerCtx, Delivery{
			CampaignID: campaign.ID, RecipientID: r.ID, Email: r.Email,
			Status: "sent", SentAt: now, Attempts: attempts,
		}); derr != nil {
			// The message IS sent. A ledger write that fails leaves the
			// recipient eligible again next batch, which risks ONE
			// duplicate -- and that is the right side to fail on, because
			// the alternative (assume it landed) risks silently never
			// mailing somebody. Logged loudly so it is not invisible.
			w.logger.Error("campaigns worker: delivered but could not record the delivery; recipient may be re-mailed once",
				"job", job.ID, "error", derr)
		}
		return false, nil
	}

	// THE PROVIDER'S limit, half two. A throttle is not this recipient's
	// failure: the message was never accepted or rejected on its merits,
	// so the delivery row stays retryable and the whole JOB parks until
	// the provider's own deadline.
	if wait, throttled := email.IsThrottled(err); throttled {
		if wait <= 0 {
			wait = w.cfg.DefaultThrottle
		}
		until := now.Add(wait)
		if uerr := w.store.UpdateJob(systemCtx, job.ID, SendJobPatch{ThrottledUntil: &until}); uerr != nil {
			w.logger.Warn("campaigns worker: could not record the throttle deadline", "error", uerr)
		}
		job.ThrottledUntil = until
		_ = w.store.RecordDelivery(ownerCtx, Delivery{
			CampaignID: campaign.ID, RecipientID: r.ID, Email: r.Email,
			Status: "pending", Attempts: item.attempts, LastError: err.Error(),
			NextAttemptAt: until,
		})
		w.logger.Warn("campaigns worker: provider throttled the send; parking the job",
			"job", job.ID, "until", until.Format(time.RFC3339), "error", err)
		return true, nil
	}

	if email.IsPermanent(err) || attempts >= w.cfg.MaxAttempts {
		w.recordFailed(ownerCtx, job, campaign.ID, r, attempts, err.Error())
		return false, nil
	}

	next := now.Add(backoffFor(attempts))
	if derr := w.store.RecordDelivery(ownerCtx, Delivery{
		CampaignID: campaign.ID, RecipientID: r.ID, Email: r.Email,
		Status: "pending", Attempts: attempts, LastError: err.Error(), NextAttemptAt: next,
	}); derr != nil {
		w.logger.Warn("campaigns worker: could not record a retryable delivery", "error", derr)
	}
	return false, nil
}

func (w *Worker) deliver(ctx context.Context, msg email.Message) error {
	sender := w.resolveSender()
	if sender == nil {
		return errors.New("campaigns: no email sender registered")
	}
	if w.sendHook != nil {
		return w.sendHook(ctx, sender, msg)
	}
	return sender.Send(ctx, msg)
}

func (w *Worker) resolveSender() email.Sender {
	if w.resolve == nil {
		return nil
	}
	return w.resolve()
}

func (w *Worker) recordSkipped(ownerCtx context.Context, job *SendJob, campaignID string, r Recipient, reason string) {
	job.SkippedCount++
	w.skipTotal.Add(1)
	if err := w.store.RecordDelivery(ownerCtx, Delivery{
		CampaignID: campaignID, RecipientID: r.ID, Email: r.Email,
		Status: "skipped", SkipReason: reason,
	}); err != nil {
		w.logger.Warn("campaigns worker: could not record a skipped delivery", "error", err)
	}
}

func (w *Worker) recordFailed(ownerCtx context.Context, job *SendJob, campaignID string, r Recipient, attempts int, detail string) {
	job.FailedCount++
	w.failedTotal.Add(1)
	if err := w.store.RecordDelivery(ownerCtx, Delivery{
		CampaignID: campaignID, RecipientID: r.ID, Email: r.Email,
		Status: "failed", Attempts: attempts, LastError: detail,
	}); err != nil {
		w.logger.Warn("campaigns worker: could not record a failed delivery", "error", err)
	}
}

func (w *Worker) stampProgress(systemCtx, ownerCtx context.Context, job SendJob, rosterSize int) {
	sent, skipped, failed, count := job.SentCount, job.SkippedCount, job.FailedCount, rosterSize
	if err := w.store.UpdateJob(systemCtx, job.ID, SendJobPatch{
		SentCount: &sent, SkippedCount: &skipped, FailedCount: &failed, RecipientCount: &count,
	}); err != nil {
		w.logger.Warn("campaigns worker: could not stamp job progress", "error", err)
	}
	if err := w.store.UpdateCampaignProgress(ownerCtx, job.CampaignID, CampaignProgress{
		RecipientCount: &count, SentCount: &sent, FailedCount: &failed,
	}); err != nil {
		w.logger.Warn("campaigns worker: could not stamp campaign progress", "error", err)
	}
}

func (w *Worker) completeJob(systemCtx, ownerCtx context.Context, job SendJob, campaign Campaign, rosterSize int) {
	now := w.now().UTC()
	done := "completed"
	sent, skipped, failed, count := job.SentCount, job.SkippedCount, job.FailedCount, rosterSize
	if err := w.store.UpdateJob(systemCtx, job.ID, SendJobPatch{
		Status: &done, CompletedAt: &now,
		SentCount: &sent, SkippedCount: &skipped, FailedCount: &failed, RecipientCount: &count,
	}); err != nil {
		w.logger.Warn("campaigns worker: could not complete send job", "error", err)
	}
	status := "sent"
	if err := w.store.UpdateCampaignProgress(ownerCtx, job.CampaignID, CampaignProgress{
		Status: &status, CompletedAt: &now,
		RecipientCount: &count, SentCount: &sent, FailedCount: &failed,
	}); err != nil {
		w.logger.Warn("campaigns worker: could not complete campaign", "error", err)
	}
	w.logger.Info("campaigns worker: campaign complete",
		"campaign", campaign.ID, "recipients", rosterSize,
		"sent", job.SentCount, "skipped", job.SkippedCount, "failed", job.FailedCount)
}

func (w *Worker) failJob(systemCtx context.Context, job SendJob, reason string) {
	now := w.now().UTC()
	status := "failed"
	detail := truncateError(reason)
	if err := w.store.UpdateJob(systemCtx, job.ID, SendJobPatch{
		Status: &status, LastError: &detail, CompletedAt: &now,
	}); err != nil {
		w.logger.Warn("campaigns worker: could not fail send job", "error", err)
	}
	w.logger.Warn("campaigns worker: send job failed", "job", job.ID, "reason", reason)
}

func (w *Worker) stopJob(systemCtx context.Context, job SendJob, status, reason string) {
	detail := truncateError(reason)
	if err := w.store.UpdateJob(systemCtx, job.ID, SendJobPatch{Status: &status, LastError: &detail}); err != nil {
		w.logger.Warn("campaigns worker: could not stop send job", "error", err)
	}
}

// systemActorContext builds the engine's own operator identity.
//
// Role owner, and that is the deliberate part: the two engine concepts
// declare the clusterOwner tier, and the read gate resolves that tier
// through auth.IsClusterOwner() with no other way in. The alternative
// would be an escape hatch in the enforcement layer, which is strictly
// worse -- a bypass is available to every caller that can reach it,
// whereas an identity is only as powerful as the queries it is used for.
// Every use of this context in this package is a named construct over
// v1:campaigns:sendJob or v1:campaigns:suppression; nothing here reads a
// recipient under it.
func (w *Worker) systemActorContext(ctx context.Context) context.Context {
	claims := map[string]any{"sub": systemCampaignsActor, "role": "owner"}
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: systemCampaignsActor,
		Role:   auth.RoleOwner,
	})
}

// ownerActorContext borrows the campaign owner's identity for every
// owned-tier read and write. auth.ContextWithUserActor sets all three
// surfaces (claims, TokenInfo, AccessContext) because `createdBy` and
// `actor.userId` read different ones (memql#2989) -- and a delivery row
// whose ownerUserId came out empty would be a row nobody can read.
func (w *Worker) ownerActorContext(ctx context.Context, ownerUserID string) context.Context {
	return auth.ContextWithUserActor(ctx, ownerUserID)
}

// --- pure helpers -------------------------------------------------------

type batchItem struct {
	recipient Recipient
	attempts  int
}

// selectBatch is the resume rule, isolated so it is testable without an
// engine: roster members with no ledger entry, plus entries still
// `pending` whose retry is due. Roster order (oldest recipient first) is
// preserved, so a resumed send continues where it left off rather than
// re-shuffling.
func selectBatch(roster []Recipient, ledger map[string]LedgerEntry, now time.Time, limit int) []batchItem {
	batch := make([]batchItem, 0, limit)
	for _, r := range roster {
		entry, seen := ledger[r.ID]
		switch {
		case !seen:
			// never attempted
		case entry.Status != "pending":
			// terminal: sent, skipped or failed. NEVER re-mailed.
			continue
		case !entry.NextAttemptAt.IsZero() && now.Before(entry.NextAttemptAt):
			continue
		}
		batch = append(batch, batchItem{recipient: r, attempts: entry.Attempts})
		if len(batch) >= limit {
			break
		}
	}
	return batch
}

// pendingRemains reports whether any roster member is still outstanding
// (unattempted, or pending a future retry). Distinguishes "the batch is
// empty because everything is done" from "the batch is empty because
// everything is waiting", which are opposite conclusions about whether
// the campaign is finished.
func pendingRemains(roster []Recipient, ledger map[string]LedgerEntry) bool {
	for _, r := range roster {
		entry, seen := ledger[r.ID]
		if !seen || entry.Status == "pending" {
			return true
		}
	}
	return false
}

func isDrainable(status string) bool { return status == "queued" || status == "running" }

// terminalFromCampaignStatus maps the CAMPAIGN's status onto what the job
// should do. The campaign row is what an operator manipulates, so it is
// authoritative over the job -- pausing a campaign has to stop its send
// even though the operator never touched the job row.
func terminalFromCampaignStatus(status string) (string, bool) {
	switch status {
	case "sending":
		return "", false
	case "paused":
		return "paused", true
	case "cancelled":
		return "cancelled", true
	case "sent":
		return "completed", true
	default:
		// draft / scheduled / failed: the campaign is not in a state that
		// authorizes a send, so the job pauses rather than failing --
		// resuming is one status change away and nothing has been lost.
		return "paused", true
	}
}

// recipientStatusFor maps a suppression reason onto the recipient row's
// own subscription enum. `manual` deliberately maps to `unsubscribed`:
// the recipient enum has no operator-suppression state, and inventing one
// would mean a schema change for a case where "do not mail this person"
// is exactly what unsubscribed already means.
func recipientStatusFor(reason string) string {
	switch reason {
	case "unsubscribed", "manual":
		return "unsubscribed"
	case "hard_bounce":
		return "bounced"
	case "complaint":
		return "complained"
	default:
		return ""
	}
}

// backoffFor returns the delay before the given (1-based) attempt's
// retry: bounded exponential with +-20% jitter.
func backoffFor(attempt int) time.Duration {
	d := backoffBase
	for i := 1; i < attempt; i++ {
		d *= backoffFactor
		if d >= backoffCap {
			d = backoffCap
			break
		}
	}
	jitter := 1 + (rand.Float64()*0.4 - 0.2)
	j := time.Duration(float64(d) * jitter)
	if j > backoffCap {
		j = backoffCap
	}
	return j
}
