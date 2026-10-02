package campaigns

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

// Sending readiness belongs to an organization and its chosen sender. The
// cluster's operator mailbox is not evidence that a client's domain is ready.
// This is a fresh, non-sending check; actual delivery revalidates everything.
func (w *Worker) handleSendingReadiness(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	campaign := Campaign{AccountID: bare(argString(args, "accountId")), SenderIdentityID: bare(argString(args, "senderIdentityId"))}
	if campaignID := bare(argString(args, "campaignId")); campaignID != "" {
		stored, found, err := w.store.CampaignByID(ctx, campaignID)
		if err != nil || !found {
			return nil, fmt.Errorf("campaign is inaccessible")
		}
		campaign = stored
	}
	if campaign.AccountID == "" {
		return nil, fmt.Errorf("choose an organization")
	}
	if err := w.requireSendAuthority(ctx, campaign.AccountID); err != nil {
		return nil, err
	}
	reason := w.cfg.RequireUnsubscribe()
	if reason == "" {
		identity, refusal := w.resolveSendIdentity(ctx, campaign)
		reason = refusal.Reason
		if reason == "" {
			if err := email.CheckSender(ctx, w.resolveSender(), identity.SendAs); err != nil {
				reason = err.Error()
			}
		}
	}
	return resultNode("campaignSendingReadiness", map[string]any{"accountId": campaign.AccountID, "campaignId": campaign.ID, "ready": reason == "", "reason": reason})
}
