package campaigns

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// renewExpiredSignup accepts a new, affirmative form submission after this
// membership's confirmation run expired. Duplicates and opted-out mailboxes stay
// settled. The current membership is checked again under the same recipient and
// mailbox gates as confirmation and preferences, so neither can be overwritten.
func (w *Worker) renewExpiredSignup(ctx context.Context, binding map[string]any, args map[string]any, signupID, address string) error {
	signup, err := w.newsletterRow(ctx, "newsletterSignupById", "signupId", signupID)
	if err != nil || signup == nil {
		return err
	}
	if str(signup, "submissionId") == argString(args, "submissionId") || argString(args, "submissionId") == "" {
		return nil
	}
	recipientID := bare(str(signup, "recipientId"))
	recipient, found, err := w.store.RecipientByID(ctx, recipientID)
	if err != nil || !found || recipient.SubscriptionStatus != "pending" {
		return err
	}
	eligible, err := w.expiredMembership(ctx, recipient)
	if err != nil || !eligible {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	accountID, audienceID, siteID := str(binding, "accountId"), bare(str(binding, "audienceId")), bare(str(binding, "siteId"))
	hold, err := w.newsletterGate(context.WithoutCancel(ctx), newsletterLock("site", siteID), newsletterLock("audience", audienceID), "campaign-journey:recipient:"+recipientID, "campaign-email-mailbox:"+organizationSuppressionID(accountID, EmailDigest(address)))
	if err != nil {
		return err
	}
	defer hold.Release()
	phase, cancel := context.WithTimeout(ctx, signupLockedPhase)
	defer cancel()
	current, err := w.subscribeBinding(phase, args, siteID)
	if err != nil {
		return err
	}
	if bare(str(current, "audienceId")) != audienceID || !sameOrganization(str(current, "accountId"), accountID) {
		return fmt.Errorf("the signup form changed; refresh it before subscribing")
	}
	recipient, found, err = w.store.RecipientByID(phase, recipientID)
	if err != nil || !found {
		return err
	}
	if recipient.SubscriptionStatus != "pending" || recipient.AudienceID != audienceID || !sameOrganization(recipient.AccountID, accountID) || NormalizeEmail(recipient.Email) != address {
		return nil
	}
	members, err := w.store.MailboxMembers(phase, audienceID, address)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.ID != recipientID {
			return nil
		}
	}
	if _, blocked, err := w.store.SuppressionForSend(w.systemActorContext(phase), accountID, EmailDigest(address)); err != nil || blocked {
		return err
	}
	eligible, err = w.expiredMembership(phase, recipient)
	if err != nil || !eligible {
		return err
	}
	if err = hold.Verify(phase); err != nil {
		return err
	}
	return detachedWrite(phase, func(write context.Context) error {
		return w.store.RejoinRecipient(write, recipientID, "pending", memql.VersionTimeAfter(recipient.JoinedAt, w.nowUTC()))
	})
}

// Every open run belongs to the current membership. A past membership never
// authorizes another reset, and an incomplete scan never authorizes one either.
func (w *Worker) expiredMembership(ctx context.Context, recipient Recipient) (bool, error) {
	if recipient.JoinedAt.IsZero() {
		return false, nil
	}
	system := auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(w.systemActorContext(ctx)))
	cursor := ""
	seen := map[string]bool{}
	expired := false
	for page := 0; page < journeyDuePages; page++ {
		rows, next, err := w.store.rowsPage(system, cursor, call("query", "recipientJourneyEnrollments", arg{"recipientId", recipient.ID}))
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			if bare(str(row, "id")) != journeyEnrollmentID(str(row, "journeyId"), recipient.ID, recipient.JoinedAt) {
				continue
			}
			if str(row, "status") == "active" || str(row, "status") == "waiting" {
				return false, nil
			}
			if str(row, "status") == "exited" && str(row, "exitReason") == "not confirmed" {
				expired = true
			}
		}
		if next == "" {
			return expired, nil
		}
		if seen[next] {
			return false, fmt.Errorf("membership scan repeated a cursor")
		}
		seen[next] = true
		cursor = next
	}
	return false, signupsBusy
}
