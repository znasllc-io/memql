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
//   - An add writes only a person nobody has yet. An id that already names
//     someone (in another audience, at another address, in another
//     organization) is refused; the same add made again is answered with the
//     person as they are.
//   - A person is subscribed in a confirm-first audience only by confirming.
//     The confirmation path records confirmedAt and a consent event, which is
//     what automations triggered on confirmation start from, and wakes an
//     automation waiting on it.
//   - Nobody who left is brought back by hand. A person who is unsubscribed,
//     bounced or complained stays so whatever an operator sets, and an address
//     on the suppression list is never added or set subscribed: only the person
//     can subscribe again, from the link in their emails.

// alreadyInAudience is the sentence the operator reads, here and in the Add
// people form (which asks recipientInAudienceByDigest before submitting).
const alreadyInAudience = "This address is already in this audience."

// confirmFirstSentence is what an operator reads when they try to subscribe
// someone to an audience that asks people to confirm without saying they did.
const confirmFirstSentence = "This audience asks people to confirm. Say they already confirmed, or leave them pending so they get the confirmation email."

// blockedSentence is the refusal for an address that has opted out or cannot be
// emailed: only the person can subscribe again, from the link in their emails.
const blockedSentence = "This address has unsubscribed or cannot be emailed, so only the person can subscribe again, from the link in their emails."

// operatorStatesSentence is the refusal of a state an operator does not set: a
// bounce or a complaint is the provider's verdict, never a choice made by hand.
const operatorStatesSentence = "A person can be set to subscribed, pending or unsubscribed."

// audienceBusy is what an operator reads when the audience's lock could not be
// taken in time: an import is writing a batch, or somebody else is adding a
// person. Nothing was written, and asking again shortly is the right thing to do.
const audienceBusy = visitorMessage("This audience is busy with an import or another change. Try again shortly.")

// addedPendingSentence is what an operator reads when the person they said already
// confirmed was written pending but their confirmation was not recorded: the person
// is in the audience, pending, and marking them subscribed (saying they already
// confirmed) finishes the add.
const addedPendingSentence = "The person was added, but their confirmation was not recorded, so they are pending. Mark them subscribed to finish."

// idTakenSentence is the refusal of an add whose person id already names someone
// else: another audience, another address or another organization. The add
// would write over them, so nothing is written.
const idTakenSentence = "This id already belongs to someone else, so the person was not added. Add them again."

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
	// The audience's lock, which every writer of a membership takes (the signup
	// form, an import batch, an automation's enrolment), then the person's and
	// the mailbox's, which a send and the pages behind email links take: the
	// person is written whole before a drain can act on them, and the mailbox
	// cannot opt out between the suppression check below and the write.
	hold, err := w.newsletterGate(context.WithoutCancel(ctx), newsletterLock("audience", audienceID), "campaign-journey:recipient:"+recipientID, "campaign-email-mailbox:"+organizationSuppressionID(audience.AccountID, EmailDigest(normalized)))
	if errors.Is(err, memql.ErrWriteGateBusy) {
		return nil, audienceBusy
	}
	if err != nil {
		return nil, err
	}
	defer hold.Release()
	phase, cancel := context.WithTimeout(ctx, signupLockedPhase)
	defer cancel()

	// The write is an insert that merges into a row that already has this id, so
	// an id that names somebody is read first, whoever's they are (the cluster's
	// read: a row the caller cannot see is still a row the insert would write
	// over). The same person in the same audience at the same address is a retry
	// of this add, and gets them back as they are; anyone else is refused.
	existing, taken, err := w.store.RecipientByID(memql.ContextWithFreshRead(w.systemActorContext(phase)), recipientID)
	if err != nil {
		return nil, err
	}
	if taken {
		if bare(existing.AudienceID) != audienceID || NormalizeEmail(existing.Email) != normalized || !sameOrganization(existing.AccountID, audience.AccountID) {
			return nil, fmt.Errorf("%s", idTakenSentence)
		}
		return resultNode("campaignRecipientAdded", map[string]any{"recipientId": recipientID, "audienceId": audienceID, "subscriptionStatus": existing.SubscriptionStatus})
	}

	members, err := w.store.MailboxMembers(phase, audienceID, normalized)
	if errors.Is(err, ErrMailboxUndecidable) {
		return nil, fmt.Errorf("this audience cannot be checked for duplicates right now; try again later")
	}
	if err != nil {
		return nil, err
	}
	if len(members) > 0 {
		// The id is nobody's yet, so every row this mailbox holds is somebody else.
		return nil, fmt.Errorf("%s", alreadyInAudience)
	}
	if status == "subscribed" {
		// Whatever the audience, nobody whose address opted out or cannot be
		// emailed is added as a subscriber by hand.
		if err := w.refuseBlockedConfirmation(phase, audience.AccountID, normalized); err != nil {
			return nil, err
		}
	}
	if err := hold.Verify(phase); err != nil {
		return nil, audienceBusy
	}
	// A person who already confirmed is written pending first and confirmed
	// after: the consent event names the person, so it can only be recorded once
	// they exist, and it is recorded before they are subscribed (recordConfirmation).
	// Both writes are made under the person's lock, which a drain takes before it
	// sends to them, so no drain acts between them: a confirmation is never sent
	// to someone the operator says already confirmed. A write cut short leaves
	// someone who still has to confirm, never someone who skipped it, and marking
	// them subscribed (saying they already confirmed) finishes it.
	confirming := confirmFirst && status == "subscribed"
	written := status
	if confirming {
		written = "pending"
	}
	if err := detachedWrite(phase, func(write context.Context) error {
		return w.store.AddRecipient(write, recipientID, audienceID, address, strings.TrimSpace(argString(args, "displayName")), orDefault(argString(args, "source"), "manual"), audience.AccountID, written, fields)
	}); err != nil {
		return nil, err
	}
	if !confirming {
		return resultNode("campaignRecipientAdded", map[string]any{"recipientId": recipientID, "audienceId": audienceID, "subscriptionStatus": status})
	}
	// From here the person is written, pending: whatever stops the add now -- the
	// read back, a lost lock, the confirmation's write -- leaves them pending, and
	// the answer says so and how to finish, never that nothing was done.
	pendingNow := func(cause error) ([]memorynodes.MemoryNode, error) {
		w.logger.Warn("campaigns: a person added as already confirmed is pending: their confirmation was not recorded", "recipient", recipientID, "error", cause)
		return nil, fmt.Errorf("%s", addedPendingSentence)
	}
	recipient, found, err := w.store.RecipientByID(phase, recipientID)
	if err != nil {
		return pendingNow(err)
	}
	if !found {
		return pendingNow(errors.New("the person could not be read back"))
	}
	if err := hold.Verify(phase); err != nil {
		return pendingNow(err)
	}
	if err := detachedWrite(phase, func(write context.Context) error {
		return w.recordConfirmation(write, recipient, w.nowUTC())
	}); err != nil {
		return pendingNow(err)
	}
	// Then, as a confirmation by hand does, the locks are let go and a run waiting
	// on this person's confirmation is woken: none can have begun while the locks
	// were held, but one could if a lock was lost on the way.
	hold.Release()
	if err := w.nudgeConfirmed(ctx, recipient.ID); err != nil {
		w.logger.Warn("campaigns: a person added as confirmed is recorded, but a run waiting on their confirmation could not be woken; it goes on at its timeout", "recipient", recipient.ID, "error", err)
	}
	return resultNode("campaignRecipientAdded", map[string]any{"recipientId": recipientID, "audienceId": audienceID, "subscriptionStatus": status})
}

// nudgeConfirmed wakes the runs waiting on a person's confirmation, taking the
// locks the drain takes for each, in its order.
func (w *Worker) nudgeConfirmed(ctx context.Context, recipientID string) error {
	enrollments := func(ctx context.Context, journeyID, recipientID, enrollmentID string) (gateHold, error) {
		return w.newsletterGate(ctx, journeyLock(journeyID), "campaign-journey:recipient:"+bare(recipientID), "campaign-journey:enrollment:"+bare(enrollmentID))
	}
	return w.store.NudgeWaitingEnrollments(w.systemActorContext(ctx), recipientID, w.nowUTC(), enrollments)
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
	switch status {
	case "subscribed", "pending", "unsubscribed":
	default:
		// Bounced and complained are the provider's verdicts, never a choice.
		return nil, fmt.Errorf("%s", operatorStatesSentence)
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
	switch recipient.SubscriptionStatus {
	case "unsubscribed", "bounced", "complained":
		// Whatever the audience, an operator never moves someone who left, or whose
		// address the provider gave up on, to any other state: pending is one step
		// from subscribed, and an operator's unsubscribe writes no suppression to
		// catch the next. Only the person can come back, from the preferences page
		// of their own emails. Saying again what they already are changes nothing.
		if status != recipient.SubscriptionStatus {
			return nil, fmt.Errorf("%s", blockedSentence)
		}
		return resultNode("campaignRecipientSubscription", map[string]any{"recipientId": recipientID, "subscriptionStatus": status})
	}
	if status == "subscribed" && recipient.SubscriptionStatus != "subscribed" {
		// Whatever the audience, an operator never subscribes someone whose
		// address is suppressed: only the person can, from the preferences page of
		// their own emails.
		if err := w.refuseBlockedConfirmation(ctx, recipient.AccountID, recipient.Email); err != nil {
			return nil, err
		}
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
	return w.nudgeConfirmed(ctx, recipient.ID)
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
