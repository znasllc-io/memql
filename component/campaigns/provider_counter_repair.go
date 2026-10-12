package campaigns

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// progressWrite serializes the ordinary worker's last counter writes with a
// later provider repair. A stale batch must not overwrite a terminal repair.
// No receipt is held while taking this gate.
func (w *Worker) progressWrite(systemCtx, ownerCtx context.Context, job SendJob) (gateHold, SendJob, Campaign, error) {
	if w.progressGate == nil {
		return nil, SendJob{}, Campaign{}, fmt.Errorf("campaign progress coordination is unavailable")
	}
	hold, err := w.progressGate(context.WithoutCancel(systemCtx), job.CampaignID)
	if err != nil {
		return nil, SendJob{}, Campaign{}, err
	}
	keep := false
	defer func() {
		if !keep {
			hold.Release()
		}
	}()
	current, found, err := w.store.JobByID(memql.ContextWithFreshRead(systemCtx), job.ID)
	if err != nil || !found || !isDrainable(current.Status) {
		return nil, SendJob{}, Campaign{}, err
	}
	campaign, found, err := w.store.CampaignByID(memql.ContextWithFreshRead(ownerCtx), job.CampaignID)
	if err != nil {
		return nil, SendJob{}, Campaign{}, err
	}
	if !found || current.CampaignID != job.CampaignID || current.CampaignOwnerUserID != job.CampaignOwnerUserID || !sameOrganization(current.CampaignAccountID, job.CampaignAccountID) || campaign.OwnerUserID != current.CampaignOwnerUserID || !sameOrganization(campaign.AccountID, current.CampaignAccountID) {
		return nil, SendJob{}, Campaign{}, fmt.Errorf("campaign progress attribution changed")
	}
	keep = true
	return hold, current, campaign, nil
}

func terminalSendJob(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

// Provider callbacks hold receipt -> provider-ledger, never progress. Repair
// holds progress -> provider-ledger on ONE gate connection, never receipt. The ledger gate stabilizes
// every page and its queue acknowledgements without reversing receipt order.
func (w *Worker) repairProviderCounters(ctx context.Context) {
	system := auth.ContextWithInternalOrigin(w.systemActorContext(memql.ContextWithFreshRead(ctx)))
	seen := map[string]bool{}
	cursor := ""
	for ctx.Err() == nil {
		rows, next, err := w.store.rowsPage(system, cursor, call("query", "providerCountersToRepair"))
		if err != nil {
			w.logger.Warn("campaigns: provider counter queue failed", "error", err)
			return
		}
		selected := map[string]map[string]any{}
		ids := []string{}
		for _, row := range rows {
			campaign := bare(str(row, "campaignId"))
			if campaign == "" || seen[campaign] {
				continue
			}
			seen[campaign] = true
			selected[campaign] = row
			ids = append(ids, campaign)
		}
		_ = campaignPage(ctx, ids, func(ctx context.Context, campaign string) error {
			if err := w.repairProviderCampaign(ctx, selected[campaign]); err != nil {
				w.logger.Warn("campaigns: provider counter repair deferred", "campaign", campaign, "error", err)
			}
			return nil // One unavailable campaign must not starve later queue entries.
		})
		if next == "" {
			return
		}
		if next == cursor {
			w.logger.Warn("campaigns: provider counter queue did not advance")
			return
		}
		cursor = next
	}
}

func (w *Worker) repairProviderCampaign(ctx context.Context, selected map[string]any) error {
	campaignID := bare(str(selected, "campaignId"))
	if campaignID == "" || w.newsletterGate == nil {
		return fmt.Errorf("provider counter coordination is unavailable")
	}
	hold, err := w.newsletterGate(context.WithoutCancel(ctx), campaignProgressLock(campaignID), providerLedgerLock(campaignID))
	if err != nil {
		return err
	}
	defer hold.Release()
	system := auth.ContextWithInternalOrigin(w.systemActorContext(memql.ContextWithFreshRead(ctx)))
	job, found, err := w.store.JobByID(system, campaignID)
	if err != nil {
		return err
	}
	if !found || !terminalSendJob(job.Status) {
		return nil
	}
	campaign, err := w.newsletterRow(system, "providerCounterCampaignById", "campaignId", campaignID)
	if err != nil {
		return err
	}
	owner, account := bare(str(campaign, "ownerUserId")), bare(str(campaign, "accountId"))
	if campaign == nil || owner == "" || account == "" || job.CampaignID != campaignID || job.CampaignOwnerUserID != owner || !sameOrganization(job.CampaignAccountID, account) || bare(str(selected, "ownerUserId")) != owner || !sameOrganization(str(selected, "accountId"), account) {
		return fmt.Errorf("provider counter attribution does not match the send job")
	}
	// Always page the ledger. Count queries can be lower bounds; subtracting a
	// bounded failed count from a bounded total cannot yield an exact repair.
	sent, skipped, failed := 0, 0, 0
	pending := []map[string]any{}
	cursor := ""
	for {
		rows, next, err := w.store.rowsPage(system, cursor, call("query", "providerCounterLedger", arg{"campaignId", campaignID}, arg{"ownerUserId", owner}, arg{"accountId", account}))
		if err != nil {
			return err
		}
		for _, row := range rows {
			var count *int
			switch str(row, "status") {
			case "sent":
				count = &sent
			case "skipped":
				count = &skipped
			case "failed":
				count = &failed
			}
			if count != nil {
				if *count == math.MaxInt {
					return fmt.Errorf("provider counter exceeds supported range")
				}
				*count += 1
			}
			if boolean(row, "providerCountersPending") {
				pending = append(pending, row)
			}
		}
		if next == "" {
			break
		}
		if next == cursor {
			return fmt.Errorf("provider counter ledger did not advance")
		}
		cursor = next
	}
	if len(pending) == 0 {
		return nil
	}
	version := memql.VersionTimeAfter(job.CreatedAt, w.nowUTC())
	if err := commitCampaignProgress(system, hold, func(commit context.Context) error {
		return w.store.UpdateJob(commit, job.ID, SendJobPatch{SentCount: &sent, SkippedCount: &skipped, FailedCount: &failed, VersionTime: &version})
	}); err != nil {
		return err
	}
	if err := commitCampaignProgress(system, hold, func(commit context.Context) error {
		return w.newsletterWrite(commit, "repairProviderCampaignCounters", map[string]any{"campaignId": campaignID, "sentCount": sent, "skippedCount": skipped, "failedCount": failed, "versionTime": memql.VersionTimeAfter(templateTime(campaign["createdAt"]), w.nowUTC()).Format(time.RFC3339Nano)})
	}); err != nil {
		return err
	}
	for _, row := range pending {
		if err := commitCampaignProgress(system, hold, func(commit context.Context) error {
			return w.newsletterWrite(commit, "clearProviderCounterRepair", map[string]any{"deliveryId": bare(str(row, "id")), "versionTime": memql.VersionTimeAfter(templateTime(row["createdAt"]), w.nowUTC()).Format(time.RFC3339Nano)})
		}); err != nil {
			return err
		}
	}
	return nil
}

// A mutation can outlive its caller inside the engine. Keep the lock until the
// bounded commit returns, and verify it using that commit's live context.
func commitCampaignProgress(ctx context.Context, hold gateHold, write func(context.Context) error) error {
	return detachedWrite(ctx, func(commit context.Context) error {
		if err := hold.Verify(commit); err != nil {
			return err
		}
		return write(commit)
	})
}

// A cancelled/failed job can still have a transport call returning on another
// replica. Serialize that final ledger commit with repair and request another
// recount in the same version when the job is already terminal. The sender may
// hold receipt -> ledger here; it never takes the progress lock.
func (w *Worker) recordWorkerDelivery(ownerCtx context.Context, delivery Delivery) error {
	if w.providerLedgerGate == nil {
		return fmt.Errorf("campaign ledger coordination is unavailable")
	}
	hold, err := w.providerLedgerGate(context.WithoutCancel(ownerCtx), delivery.CampaignID)
	if err != nil {
		return err
	}
	defer hold.Release()
	system := auth.ContextWithInternalOrigin(w.systemActorContext(memql.ContextWithFreshRead(ownerCtx)))
	job, found, err := w.store.JobByID(system, delivery.CampaignID)
	if err != nil {
		return err
	}
	owner := bare(callerUserID(ownerCtx))
	if owner == "" {
		return fmt.Errorf("campaign ledger owner is unavailable")
	}
	if found && (job.CampaignOwnerUserID != owner || job.CampaignID != bare(delivery.CampaignID) || !sameOrganization(job.CampaignAccountID, delivery.AccountID)) {
		return fmt.Errorf("campaign ledger attribution changed")
	}
	prior, err := w.newsletterRow(system, "providerDeliveryById", "deliveryId", deliveryRowID(delivery.CampaignID, delivery.RecipientID))
	if err != nil {
		return err
	}
	if prior != nil && (bare(str(prior, "ownerUserId")) != owner || bare(str(prior, "campaignId")) != bare(delivery.CampaignID) || !sameOrganization(str(prior, "accountId"), delivery.AccountID)) {
		return fmt.Errorf("campaign delivery attribution changed")
	}
	version := memql.VersionTimeAfter(templateTime(prior["createdAt"]), w.nowUTC())
	if str(prior, "providerStatus") != "" {
		// A provider-proven outcome outranks an older batch's local verdict. An
		// insert would otherwise erase that provenance and any unacknowledged marker.
		return commitCampaignProgress(system, hold, func(commit context.Context) error {
			return w.newsletterWrite(commit, "markProviderCounterRepair", map[string]any{"deliveryId": deliveryRowID(delivery.CampaignID, delivery.RecipientID), "versionTime": version.Format(time.RFC3339Nano)})
		})
	}
	delivery.ProviderCountersPending = !found || terminalSendJob(job.Status) || boolean(prior, "providerCountersPending")
	delivery.VersionTime = &version
	return commitCampaignProgress(ownerCtx, hold, func(commit context.Context) error { return w.store.RecordDelivery(commit, delivery) })
}
