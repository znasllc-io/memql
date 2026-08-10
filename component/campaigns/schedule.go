package campaigns

import (
	"context"
	"fmt"
	"time"
)

// schedule.go -- turning a scheduledAt into a send (memql#3459).
//
// # Why the schedule lives on the engine's job row and not on a clock
//
// The obvious design is a cron automation scanning v1:campaigns:campaign for
// rows whose scheduledAt has passed. It is not expressible. `campaign` is
// owned-tier, the owned tier injects `ownerUserId==actor.userId` into every
// read, and there is no cluster-owner bypass on it -- so "which campaigns are
// due, across every operator" is a question no actor can ask. The same wall
// the drain worker met when it needed a queue.
//
// So scheduling reuses the answer that wall already forced: the
// `campaignScheduleSend` builtin reads the campaign UNDER THE CALLER'S OWN
// ACTOR (which is the authorization) and writes a clusterOwner-tier
// v1:campaigns:sendJob carrying the owner it derived, in the `scheduled`
// status. The worker can scan that. Nothing new was needed -- not a concept,
// not a cron automation, not a bypass.
//
// # The job knows the time; the CAMPAIGN decides it
//
// The job row carries a copy of scheduledAt, for the scan and for an operator
// looking at it. The copy is never the authority. An operator who moves the
// date with `updateCampaign` never touches the job, so a worker trusting the
// copy would fire a send that had been postponed -- so the promotion re-reads
// the campaign and compares THAT. The cost is reading one campaign row per
// pending scheduled campaign per tick, which is a set of a size the operator
// chose.
//
// # A due campaign fires exactly once with two replicas, structurally
//
// Three independent reasons, and the claim is the least of them:
//
//  1. The send job's id IS the campaign's bare short id. Two replicas
//     promoting the same campaign write the same row to the same value.
//  2. The drain claim (see processJob) admits one replica per (job,
//     progress), so only one ever batches.
//  3. The delivery ledger is per (campaign, recipient), so even a promotion
//     that somehow ran twice cannot mail anyone twice.
//
// The claim below therefore exists for tidiness -- one log line, one campaign
// status write -- rather than for correctness. That ordering matters: a
// correctness property resting on a lease is a correctness property with an
// expiry date.
//
// # Catch-up: a campaign whose time passed while the cluster was down fires
//
// There is no expiry window, deliberately. The comparison is "scheduledAt has
// passed", not "scheduledAt is near now", so a campaign scheduled for Tuesday
// on a cluster that was down until Thursday sends on Thursday. The
// alternative -- a window past which the send is silently dropped --
// reintroduces the exact failure this issue exists to remove: an operator who
// scheduled a send and got nothing, with nothing saying why. An operator who
// no longer wants a late send cancels it, and the campaign row is sitting
// there in `scheduled` saying it is going to happen.

// scheduleClaimName is the cross-replica claim namespace for promotion.
// Distinct from sendClaimName so a promotion and a drain of the same
// campaign never contend for one lease.
const scheduleClaimName = "campaignSchedule"

// promoteDueSchedules runs one pass over every scheduled job. Called at the
// top of each drain pass, so a campaign that comes due is promoted and
// drained within the same tick rather than waiting for the next one.
func (w *Worker) promoteDueSchedules(ctx context.Context, systemCtx context.Context) {
	jobs, err := w.store.ScheduledJobs(systemCtx)
	if err != nil {
		w.logger.Debug("campaigns worker: scheduled-job scan failed (engine likely not ready)", "error", err)
		return
	}
	for _, job := range jobs {
		select {
		case <-ctx.Done():
			return
		default:
		}
		w.promoteSchedule(ctx, systemCtx, job)
	}
}

// promoteSchedule decides what one scheduled job should do right now.
func (w *Worker) promoteSchedule(ctx context.Context, systemCtx context.Context, job SendJob) {
	if job.CampaignOwnerUserID == "" {
		w.failJob(systemCtx, job, "scheduled send job carries no campaign owner, so there is no identity to run the send under")
		return
	}

	ownerCtx := w.ownerActorContext(ctx, job.CampaignOwnerUserID)
	campaign, found, err := w.store.CampaignByID(ownerCtx, job.CampaignID)
	if err != nil {
		w.logger.Warn("campaigns worker: could not read the campaign behind a scheduled job", "job", job.ID, "error", err)
		return
	}
	if !found {
		// Same reasoning as the drain path: either the campaign is gone or
		// campaignOwnerUserId does not own it, and neither is fixed by
		// waiting.
		w.failJob(systemCtx, job, "campaign behind a scheduled send is not readable as its recorded owner; refusing to schedule")
		return
	}

	switch campaign.Status {
	case "scheduled":
		// The only status a promotion may fire from. Falls through below.
	case "cancelled":
		w.stopJob(systemCtx, job, "cancelled", "campaign was cancelled before its scheduled time")
		return
	case "sent":
		w.stopJob(systemCtx, job, "completed", "campaign was already sent before its scheduled time")
		return
	case "sending":
		// A promotion interrupted between its two writes: the campaign moved
		// and the job did not. Finishing it here is what keeps that crash
		// from leaving a campaign that reads `sending` behind a job no drain
		// pass will ever look at.
		w.queueScheduledJob(systemCtx, job, campaign)
		return
	default:
		// draft / paused / failed. The schedule is not in force, the job is
		// inert, and the campaign row says so where an operator reads it.
		return
	}

	now := w.nowUTC()
	if campaign.ScheduledAt.IsZero() {
		// The time was cleared without the job hearing about it. Not an
		// error and not a send: there is no moment for this to fire at.
		return
	}
	if now.Before(campaign.ScheduledAt) {
		return
	}

	// Keyed on the DUE TIME as well as the job, so a campaign postponed and
	// re-due later is not blocked by the lease its earlier attempt took.
	if w.claimer != nil {
		key := fmt.Sprintf("%s:%s", job.ID, campaign.ScheduledAt.UTC().Format(time.RFC3339))
		if !w.claimer.ClaimWithTTL(ctx, scheduleClaimName, key, w.cfg.ClaimTTL) {
			return
		}
	}

	if reason, terminal := w.fireTimePreflight(ownerCtx, campaign); reason != "" {
		w.refuseScheduledSend(systemCtx, ownerCtx, job, campaign, reason, terminal)
		return
	}

	// Campaign first, job second. A crash between the two lands in the
	// `sending` arm above on the next tick and is finished there; the
	// opposite order would leave a drainable job under a campaign still
	// reading `scheduled`, which the drain path treats as "not authorized to
	// send" and PAUSES -- a stuck campaign rather than a resumable one.
	if err := w.store.SetCampaignStatus(ownerCtx, "startCampaign", campaign.ID); err != nil {
		w.logger.Warn("campaigns worker: could not move a scheduled campaign into sending", "campaign", campaign.ID, "error", err)
		return
	}
	w.queueScheduledJob(systemCtx, job, campaign)
}

// queueScheduledJob flips the job to drainable and says so once.
func (w *Worker) queueScheduledJob(systemCtx context.Context, job SendJob, campaign Campaign) {
	queued := "queued"
	cleared := ""
	if err := w.store.UpdateJob(systemCtx, job.ID, SendJobPatch{Status: &queued, LastError: &cleared}); err != nil {
		w.logger.Warn("campaigns worker: could not promote a scheduled send job", "job", job.ID, "error", err)
		return
	}
	w.logger.Info("campaigns worker: scheduled campaign is due; sending",
		"campaign", campaign.ID,
		"scheduledAt", campaign.ScheduledAt.UTC().Format(time.RFC3339),
		"lateBy", w.nowUTC().Sub(campaign.ScheduledAt).Round(time.Second).String())
}

// fireTimePreflight re-runs the checks campaignScheduleSend ran when the
// operator committed to the time, because hours have passed since and any of
// them may have stopped being true.
//
// The second return value splits the failures into the two kinds that deserve
// different treatment, and the split is by WHO CAN FIX IT rather than by
// severity:
//
//	terminal      an AUTHORING problem -- the template was un-readied, the
//	              audience was emptied. Nothing the operator does to the
//	              cluster changes it, so the campaign fails with the reason
//	              on the row and the operator re-schedules after fixing it.
//	not terminal  an ENVIRONMENT problem -- no sender registered on this
//	              node, one-click unsubscribe unconfigured. The campaign is
//	              fine; the cluster is not. Failing it would make an
//	              operator re-author a schedule to recover from a bad
//	              deploy, so the send waits, the reason is stamped where the
//	              portal shows it, and the next tick tries again.
//
// The environment arm is where the boot race lands too: a node whose
// integration registry has not populated yet has no sender, and a campaign
// due in that window must not be destroyed by it.
func (w *Worker) fireTimePreflight(ownerCtx context.Context, campaign Campaign) (string, bool) {
	if reason := w.cfg.RequireUnsubscribe(); reason != "" {
		return reason, false
	}
	if w.resolveSender() == nil {
		return "no email sender is registered on this node, so the scheduled send cannot leave; it will retry", false
	}

	tmpl, found, err := w.store.TemplateByID(ownerCtx, campaign.TemplateID)
	if err != nil {
		return "could not read the template: " + err.Error(), false
	}
	if !found {
		return fmt.Sprintf("template %q is no longer readable", campaign.TemplateID), true
	}
	if tmpl.Status != "ready" {
		return fmt.Sprintf("template %q is %q rather than \"ready\"; it was un-readied after the campaign was scheduled", tmpl.ID, tmpl.Status), true
	}

	size, err := w.store.RosterSize(ownerCtx, campaign.AudienceID)
	if err != nil {
		return "could not count the audience: " + err.Error(), false
	}
	if size == 0 {
		return fmt.Sprintf("audience %q has no recipients", campaign.AudienceID), true
	}
	if size > w.cfg.MaxAudience {
		return fmt.Sprintf(
			"audience %q has grown to %d recipients, over the %d ceiling (MEMQL_CAMPAIGNS_MAX_AUDIENCE)",
			campaign.AudienceID, size, w.cfg.MaxAudience), true
	}
	return "", false
}

// refuseScheduledSend records why a due campaign did not go out. Both arms
// write the reason onto the CAMPAIGN row, because that is the row the
// operator who scheduled it is looking at -- a reason that lives only in a
// node log recreates the silence this feature exists to remove.
func (w *Worker) refuseScheduledSend(
	systemCtx, ownerCtx context.Context,
	job SendJob,
	campaign Campaign,
	reason string,
	terminal bool,
) {
	detail := truncateError(reason)
	progress := CampaignProgress{LastError: &detail}
	if terminal {
		failed := "failed"
		progress.Status = &failed
	}
	if err := w.store.UpdateCampaignProgress(ownerCtx, campaign.ID, progress); err != nil {
		w.logger.Warn("campaigns worker: could not record why a scheduled send was refused", "campaign", campaign.ID, "error", err)
	}
	if terminal {
		w.failJob(systemCtx, job, "scheduled send refused: "+reason)
		return
	}
	w.logger.Warn("campaigns worker: scheduled campaign is due but cannot send yet; will retry",
		"campaign", campaign.ID, "reason", reason)
}
