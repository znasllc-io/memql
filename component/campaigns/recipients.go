package campaigns

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// The operator's two writes on a membership are builtins, because the raw
// mutations (addRecipient, setRecipientSubscription) are @serverOnly: a client
// cannot reach the write without passing the checks below.
//
//   - An address is in an audience once. The audience's lock is taken, the
//     mailbox is looked up by its digest (and, for rows that carry none, by
//     comparing addresses), and a membership in ANY state refuses the add.
//     Existing duplicate rows stay readable; nothing here deletes them.
//   - A person is subscribed in a confirm-first audience only by confirming.
//     The confirmation path records confirmedAt and a consent event, which is
//     what automations triggered on confirmation start from, and wakes an
//     automation waiting on it.

// alreadyInAudience is the sentence the operator reads, here and in the Add
// people form (which asks recipientInAudienceByDigest before submitting).
const alreadyInAudience = "This address is already in this audience."

// confirmFirstSentence is what an operator reads when they try to subscribe
// someone to an audience that asks people to confirm without saying they did.
const confirmFirstSentence = "This audience asks people to confirm. Say they already confirmed, or leave them pending so they get the confirmation email."

// blockedSentence is the refusal for an address that has opted out or cannot be
// emailed: only the person can subscribe again, from the link in their emails.
const blockedSentence = "This address has unsubscribed or cannot be emailed, so only the person can subscribe again, from the link in their emails."

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(strings.TrimSpace(x), "true")
	}
	return false
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func (w *Worker) handleAddRecipient(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	recipientID, audienceID := bare(argString(args, "recipientId")), bare(argString(args, "audienceId"))
	address := strings.TrimSpace(argString(args, "email"))
	normalized := NormalizeEmail(address)
	if callerUserID(ctx) == "" || !templateIDPattern.MatchString(recipientID) || !templateIDPattern.MatchString(audienceID) {
		return nil, fmt.Errorf("choose an audience and a person to add while signed in")
	}
	if !plausibleAddress(normalized) || len(address) > 320 {
		return nil, fmt.Errorf("enter an email address")
	}
	requested, confirmed := argString(args, "subscriptionStatus"), truthy(args["alreadyConfirmed"])
	if requested != "" && requested != "subscribed" && requested != "pending" {
		return nil, fmt.Errorf("a person is added as subscribed or pending")
	}
	audience, found, err := w.store.AudienceByID(ctx, audienceID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("select an audience you can add people to")
	}
	if audience.AccountID != "" {
		if err := w.requireSendAuthority(ctx, audience.AccountID); err != nil {
			return nil, fmt.Errorf("adding people requires write permission in this organization")
		}
	}
	confirmFirst := audience.Confirmation == "required"
	status := requested
	switch {
	case confirmFirst && requested == "subscribed" && !confirmed:
		return nil, fmt.Errorf("%s", confirmFirstSentence)
	case confirmFirst && requested != "subscribed":
		status = "pending"
	case status == "":
		status = "subscribed"
	}
	var fields map[string]string
	if raw, ok := args["fields"].(map[string]any); ok && len(raw) > 0 {
		fields = make(map[string]string, len(raw))
		for k, v := range raw {
			fields[k] = fmt.Sprint(v)
		}
	}
	if w.newsletterGate == nil {
		return nil, fmt.Errorf("adding people is unavailable right now")
	}
	hold, err := w.newsletterGate(context.WithoutCancel(ctx), newsletterLock("audience", audienceID))
	if errors.Is(err, memql.ErrWriteGateBusy) {
		return nil, signupsBusy
	}
	if err != nil {
		return nil, err
	}
	defer hold.Release()
	phase, cancel := context.WithTimeout(ctx, signupLockedPhase)
	defer cancel()

	members, err := w.store.MailboxMembers(phase, audienceID, normalized)
	if errors.Is(err, ErrMailboxUndecidable) {
		return nil, fmt.Errorf("this audience cannot be checked for duplicates right now; try again later")
	}
	if err != nil {
		return nil, err
	}
	for _, member := range members {
		if member.ID != recipientID {
			return nil, fmt.Errorf("%s", alreadyInAudience)
		}
	}
	if confirmFirst && status == "subscribed" {
		if err := w.refuseBlockedConfirmation(phase, audience.AccountID, normalized); err != nil {
			return nil, err
		}
	}
	if len(members) == 0 {
		if err := hold.Verify(phase); err != nil {
			return nil, signupsBusy
		}
		// A person who already confirmed is written pending first and confirmed
		// after: a write cut short leaves someone who still has to confirm,
		// never someone who skipped it, and the retry (same recipientId) finishes.
		written := status
		if confirmFirst && status == "subscribed" {
			written = "pending"
		}
		if err := detachedWrite(phase, func(write context.Context) error {
			return w.store.AddRecipient(write, recipientID, audienceID, address, strings.TrimSpace(argString(args, "displayName")), orDefault(argString(args, "source"), "manual"), audience.AccountID, written, fields)
		}); err != nil {
			return nil, err
		}
	}
	if confirmFirst && status == "subscribed" {
		recipient, found, err := w.store.RecipientByID(phase, recipientID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("the person was added but could not be read back to record the confirmation; add them again to finish")
		}
		if recipient.SubscriptionStatus == "pending" {
			if err := detachedWrite(phase, func(write context.Context) error {
				return w.recordConfirmation(write, recipient, w.nowUTC())
			}); err != nil {
				return nil, err
			}
		}
	}
	return resultNode("campaignRecipientAdded", map[string]any{"recipientId": recipientID, "audienceId": audienceID, "subscriptionStatus": status})
}

// recordConfirmation is the confirmation path for a person confirmed by hand:
// the consent event first (so a half-finished write is never a sendable person
// without a record), then confirmedAt and the subscribed state.
func (w *Worker) recordConfirmation(ctx context.Context, r Recipient, at time.Time) error {
	eventID := sha256Hex("operator-confirm\x00" + r.ID + "\x00" + r.JoinedAt.UTC().Format(time.RFC3339Nano))
	exists, err := w.store.consentExists(ctx, eventID)
	if err != nil {
		return err
	}
	if !exists {
		if err := w.store.RecordConsent(ctx, ConsentGrant, ConsentRecord{EventID: eventID, EmailDigest: EmailDigest(r.Email), Source: ConsentSourceOperator, RecipientID: r.ID, AccountID: r.AccountID, OccurredAt: at}); err != nil {
			return err
		}
	}
	return w.store.ConfirmRecipient(ctx, r.ID, at)
}

func (w *Worker) handleSetRecipientSubscription(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	recipientID, status := bare(argString(args, "recipientId")), argString(args, "subscriptionStatus")
	if callerUserID(ctx) == "" || !templateIDPattern.MatchString(recipientID) || status == "" {
		return nil, fmt.Errorf("choose a person and a subscription state while signed in")
	}
	recipient, found, err := w.store.RecipientByID(ctx, recipientID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("select a person you can change")
	}
	if recipient.AccountID != "" {
		if err := w.requireSendAuthority(ctx, recipient.AccountID); err != nil {
			return nil, fmt.Errorf("changing a subscription requires write permission in this organization")
		}
	}
	if status == "subscribed" && recipient.SubscriptionStatus != "subscribed" {
		audience, found, err := w.store.AudienceByID(ctx, recipient.AudienceID)
		if err != nil {
			return nil, err
		}
		if found && audience.Confirmation == "required" {
			// Every way to subscribed in a confirm-first audience is a confirmation.
			if !truthy(args["alreadyConfirmed"]) {
				return nil, fmt.Errorf("%s", confirmFirstSentence)
			}
			if err := w.confirmByHand(ctx, recipient); err != nil {
				return nil, err
			}
			return resultNode("campaignRecipientSubscription", map[string]any{"recipientId": recipientID, "subscriptionStatus": status})
		}
	}
	var at time.Time
	if raw := argString(args, "unsubscribedAt"); raw != "" {
		at = templateTime(raw)
	}
	if err := w.store.SetRecipientSubscription(ctx, recipientID, status, at); err != nil {
		return nil, err
	}
	return resultNode("campaignRecipientSubscription", map[string]any{"recipientId": recipientID, "subscriptionStatus": status})
}

// confirmByHand takes the person's own gates (the ones the confirmation page
// takes), re-reads them, records the confirmation, lets go, and only then wakes
// a waiting automation, which takes its own gates in the drain's order.
func (w *Worker) confirmByHand(ctx context.Context, recipient Recipient) error {
	if w.newsletterGate == nil {
		return fmt.Errorf("confirming is unavailable right now")
	}
	keys := []string{"campaign-journey:recipient:" + recipient.ID}
	if digest := EmailDigest(recipient.Email); digest != "" {
		keys = append(keys, "campaign-email-mailbox:"+organizationSuppressionID(recipient.AccountID, digest))
	}
	hold, err := w.newsletterGate(context.WithoutCancel(ctx), keys...)
	if errors.Is(err, memql.ErrWriteGateBusy) {
		return signupsBusy
	}
	if err != nil {
		return err
	}
	released := false
	release := func() {
		if !released {
			released = true
			hold.Release()
		}
	}
	defer release()
	phase, cancel := context.WithTimeout(ctx, signupLockedPhase)
	defer cancel()
	current, found, err := w.store.RecipientByID(phase, recipient.ID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("this person can no longer be found; refresh and try again")
	}
	alreadyDone := current.SubscriptionStatus == "subscribed" && !current.ConfirmedAt.IsZero()
	if !alreadyDone {
		if current.SubscriptionStatus != recipient.SubscriptionStatus {
			return fmt.Errorf("this person's subscription changed while you were editing it; refresh and try again")
		}
		switch current.SubscriptionStatus {
		case "unsubscribed", "bounced", "complained":
			return fmt.Errorf("%s", blockedSentence)
		}
		if err := w.refuseBlockedConfirmation(phase, current.AccountID, current.Email); err != nil {
			return err
		}
		if err := hold.Verify(phase); err != nil {
			return signupsBusy
		}
		if err := detachedWrite(phase, func(write context.Context) error { return w.recordConfirmation(write, current, w.nowUTC()) }); err != nil {
			return err
		}
	}
	release()
	enrollments := func(ctx context.Context, journeyID, recipientID, enrollmentID string) (gateHold, error) {
		return w.newsletterGate(ctx, journeyLock(journeyID), "campaign-journey:recipient:"+bare(recipientID), "campaign-journey:enrollment:"+bare(enrollmentID))
	}
	return w.store.NudgeWaitingEnrollments(w.systemActorContext(ctx), recipient.ID, w.nowUTC(), enrollments)
}

// refuseBlockedConfirmation is the public confirmation page's rule for the
// operator's hand: an address on the suppression list (the organization's or the
// cluster's) is never marked confirmed.
func (w *Worker) refuseBlockedConfirmation(ctx context.Context, accountID, address string) error {
	_, blocked, err := w.store.SuppressionForSend(memql.ContextWithFreshRead(w.systemActorContext(ctx)), accountID, EmailDigest(address))
	if err != nil {
		return err
	}
	if blocked {
		return fmt.Errorf("%s", blockedSentence)
	}
	return nil
}
