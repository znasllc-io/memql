package campaigns

import (
	"context"
	"errors"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// send_to_recipient.go -- the marketing lane's primitive (memql#4829,
// program P5).
//
// # Why this exists, and why there is no sendEmail beside it
//
// The program fixes exactly two sanctioned shapes for automated mail.
// Operational recipients -- this cluster's own people -- ride the
// transactional outbox, which enforces an allowlist and carries no
// unsubscribe because there is nothing to unsubscribe from. MARKETING
// recipients ride this, and everything that makes marketing mail lawful comes
// with it: the cluster suppression list consulted at the POINT OF SEND, the
// RFC 8058 header pair, a resolved sending identity, and an outcome on the
// delivery ledger.
//
// It is deliberately NOT a free-form sendEmail. It takes a TEMPLATE and an
// AUDIENCE RECIPIENT, not a subject and a body, and that is the whole
// difference: a free-form primitive is one an automation can point at any
// address with any content, which is a spam cannon with a graph in front of
// it. Every message this sends is to somebody already on a list, with copy
// somebody already authored.
//
// # Resolving the recipient, and the gap this works around
//
// There is no `recipientById` query in the domain. Every read on
// v1:campaigns:recipient is scoped by audience -- recipientsForAudience,
// sendableRecipientsForAudience, audienceRosterForSend, audienceRosterSize --
// because every existing caller already holds an audience id. This one does
// not: the builtin's signature is (templateId, recipientId, senderIdentityId,
// emailRuleId), fixed by the DSL.
//
// So the audience is DERIVED, in two steps, and the order is by cost:
//
//	the rule's audience   emailRuleId names a v1:campaigns:emailRule whose
//	                      audienceId is exactly the list the rule mails. This
//	                      is the PRODUCTION path -- an event-email rule in
//	                      recipientMode=audience -- and it is one extra read.
//	a bounded scan        otherwise, walk the caller's own audiences and look
//	                      for the recipient. This is the manual path (an
//	                      operator or a test calling the builtin directly),
//	                      and it is bounded rather than cheap.
//
// The scan is capped and REFUSES rather than silently answering "not found"
// past its bound, because "we did not look far enough" and "that recipient
// does not exist" are different facts and only one of them is the caller's
// fault. A `recipientById` query would remove the scan entirely; adding one
// is a DSL change and is recorded here as the fix rather than worked around
// more cleverly.

const (
	// sendToRecipientAudienceScanCap bounds the fallback scan. Ten audiences
	// covers an operator calling the builtin by hand; past it the honest
	// answer is a refusal naming the cheap path, not a longer walk.
	sendToRecipientAudienceScanCap = 10

	// sendToRecipientSource labels the delivery's skip reasons and log lines
	// so a single-recipient send is distinguishable from a campaign's in the
	// ledger.
	sendToRecipientSource = "sendToRecipient"
)

func (w *Worker) handleSendToRecipient(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	templateID := memql.BareShortId(strings.TrimSpace(argString(args, "templateId")))
	recipientID := memql.BareShortId(strings.TrimSpace(argString(args, "recipientId")))
	senderIdentityID := memql.BareShortId(strings.TrimSpace(argString(args, "senderIdentityId")))
	emailRuleID := memql.BareShortId(strings.TrimSpace(argString(args, "emailRuleId")))
	if templateID == "" || recipientID == "" {
		return nil, errors.New("campaigns.sendToRecipient: templateId and recipientId are both required")
	}
	ownerUserID := strings.TrimSpace(callerUserID(ctx))
	if ownerUserID == "" {
		return nil, errors.New("campaigns.sendToRecipient: no caller identity; a send is always made by somebody")
	}
	if reason := w.cfg.RequireUnsubscribe(); reason != "" {
		return nil, fmt.Errorf("campaigns.sendToRecipient: %s", reason)
	}
	if w.resolveSender() == nil {
		return nil, errors.New("campaigns.sendToRecipient: no email sender is registered on this node")
	}

	// AUTHORIZATION IS THE READS. The template and the recipient are both
	// composite-tier and both are read under the CALLER's own actor, so this
	// can only ever mail somebody on a list the caller can see, with copy the
	// caller can see.
	tmpl, found, err := w.store.TemplateByID(ctx, templateID)
	if err != nil {
		return nil, fmt.Errorf("campaigns.sendToRecipient: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("campaigns.sendToRecipient: template %q is not readable", templateID)
	}

	recipient, audienceID, err := w.resolveLoneRecipient(ctx, recipientID, emailRuleID)
	if err != nil {
		return nil, fmt.Errorf("campaigns.sendToRecipient: %w", err)
	}

	// A SYNTHETIC campaign, and it is what makes every downstream piece work
	// unchanged. The renderer, the identity resolver and the unsubscribe
	// minter all take a Campaign; building one from the rule (or from
	// nothing) means a single-recipient send goes through exactly the code a
	// campaign send does, rather than through a parallel path that would
	// drift on the details that matter -- the header pair, the footer, the
	// From resolution.
	//
	// The id is the RULE's when there is one, so `X-Campaign-Id` and the
	// ledger both answer "which rule mailed this person". With no rule it is
	// empty, and the ledger row is filed under the recipient alone.
	campaign := Campaign{
		ID:               emailRuleID,
		OwnerUserID:      ownerUserID,
		Name:             strings.TrimSpace(tmpl.Subject),
		AudienceID:       audienceID,
		TemplateID:       templateID,
		SenderIdentityID: senderIdentityID,
		Status:           "sending",
		// Tracking OFF. A tracked link needs a delivery row id in the body,
		// and this path's delivery id is derived from a campaign id that may
		// be empty -- so a pixel would attribute hits to a row that is not
		// per-send distinguishable. Event-email tracking is a follow-up, not
		// a thing to approximate.
	}

	identity, refusal := w.resolveSendIdentity(ctx, campaign)
	if refusal.refused() {
		return nil, fmt.Errorf("campaigns.sendToRecipient: %s", refusal.Reason)
	}

	// SUPPRESSION AT THE POINT OF SEND, before anything is rendered. The
	// cluster list outranks the recipient row exactly as it does on the
	// campaign path, and the ordering is the mechanism: an address
	// re-imported after a bounce has a recipient row saying `subscribed`.
	digest := EmailDigest(recipient.Email)
	if digest == "" {
		return nil, fmt.Errorf("campaigns.sendToRecipient: recipient %q has no usable address", recipientID)
	}
	if sup, suppressed, err := w.store.SuppressionByDigest(w.systemActorContext(ctx), digest); err != nil {
		// A failed lookup must NOT read as "not suppressed". Refusing is the
		// only safe answer: a delayed message is recoverable, one sent to
		// somebody who opted out is not.
		return nil, fmt.Errorf("campaigns.sendToRecipient: the suppression list could not be consulted, "+
			"so this send is refused rather than made: %w", err)
	} else if suppressed {
		w.recordLoneDelivery(ctx, campaign, recipient, Delivery{
			Status: "skipped", SkipReason: sup.Reason,
		})
		return resultNode("campaignRecipientSend", map[string]any{
			"sent": false, "skipped": true, "reason": sup.Reason,
			"recipientId": recipient.ID, "emailRuleId": emailRuleID,
		})
	}
	if recipient.SubscriptionStatus != "" && recipient.SubscriptionStatus != "subscribed" {
		w.recordLoneDelivery(ctx, campaign, recipient, Delivery{
			Status: "skipped", SkipReason: recipient.SubscriptionStatus,
		})
		return resultNode("campaignRecipientSend", map[string]any{
			"sent": false, "skipped": true, "reason": recipient.SubscriptionStatus,
			"recipientId": recipient.ID, "emailRuleId": emailRuleID,
		})
	}

	token, err := MintUnsubscribeToken(w.cfg.UnsubscribeSecret, ownerUserID, recipient.ID, campaign.ID)
	if err != nil {
		return nil, fmt.Errorf("campaigns.sendToRecipient: cannot mint an unsubscribe token: %w", err)
	}
	accountName, err := w.store.AccountName(ctx, campaign.AccountID)
	if err != nil {
		w.logger.Debug("campaigns.sendToRecipient: could not resolve the account name", "error", err)
	}
	msg, err := renderMessage(campaign, tmpl, recipient, unsubscribeURL(w.cfg.UnsubscribeBaseURL, token), RenderOptions{
		ReplyTo:     replyToFor(campaign, identity),
		AccountName: accountName,
	})
	if err != nil {
		return nil, fmt.Errorf("campaigns.sendToRecipient: %w", err)
	}

	if !w.limiter.Allow() {
		return nil, errors.New("campaigns.sendToRecipient: the send-rate limit is exhausted; try again shortly")
	}
	now := w.nowUTC()
	sendCtx, cancel := context.WithTimeout(ctx, w.cfg.SendTimeout)
	sendErr := w.deliver(sendCtx, msg, identity.SendAs)
	cancel()

	if sendErr != nil {
		w.recordLoneDelivery(ctx, campaign, recipient, Delivery{
			Status: "failed", Attempts: 1, LastError: sendErr.Error(),
		})
		return nil, fmt.Errorf("campaigns.sendToRecipient: %w", sendErr)
	}

	w.reputation.observeAs(now, identity.Label, recipient.Email, "accepted")
	w.noteActiveIdentity(identity.Label)
	w.recordLoneDelivery(ctx, campaign, recipient, Delivery{
		Status: "sent", SentAt: now, Attempts: 1,
	})
	return resultNode("campaignRecipientSend", map[string]any{
		"sent": true, "skipped": false,
		"recipientId": recipient.ID, "emailRuleId": emailRuleID,
		"sentAs": identity.SendAs.Address,
	})
}

// recordLoneDelivery ledgers one outcome.
//
// Best-effort, and that asymmetry is the campaign path's: the message IS
// sent, and a failed ledger write must not turn a delivered message into an
// error the caller retries. Logged loudly instead, because an unrecorded send
// is invisible everywhere else.
func (w *Worker) recordLoneDelivery(ctx context.Context, campaign Campaign, recipient Recipient, d Delivery) {
	d.CampaignID = campaign.ID
	d.RecipientID = recipient.ID
	d.Email = recipient.Email
	if err := w.store.RecordDelivery(ctx, d); err != nil {
		w.logger.Warn("campaigns.sendToRecipient: could not record the delivery outcome",
			"recipient", recipient.ID, "status", d.Status, "error", err)
	}
}

// resolveLoneRecipient finds one recipient and the audience it belongs to.
//
// See the file doc for why this is a search at all. The rule's own audience
// is tried first because it is the production path and costs one read; the
// scan is the manual fallback and is bounded.
func (w *Worker) resolveLoneRecipient(ctx context.Context, recipientID, emailRuleID string) (Recipient, string, error) {
	if emailRuleID != "" {
		audienceID, err := w.store.EmailRuleAudience(ctx, emailRuleID)
		if err != nil {
			return Recipient{}, "", err
		}
		if audienceID != "" {
			r, found, err := w.store.RecipientByID(ctx, audienceID, recipientID)
			if err != nil {
				return Recipient{}, "", err
			}
			if found {
				return r, audienceID, nil
			}
			return Recipient{}, "", fmt.Errorf(
				"recipient %q is not in audience %q, which is the audience rule %q mails",
				recipientID, audienceID, emailRuleID)
		}
	}

	audiences, err := w.store.AudienceIDs(ctx)
	if err != nil {
		return Recipient{}, "", err
	}
	scanned := 0
	for _, audienceID := range audiences {
		if scanned >= sendToRecipientAudienceScanCap {
			// A REFUSAL, not a "not found". "We did not look far enough" and
			// "that recipient does not exist" are different facts, and
			// answering the second when the first is true would make a rule
			// silently stop mailing people as the operator's audience list
			// grew.
			return Recipient{}, "", fmt.Errorf(
				"recipient %q was not found in the first %d audiences and the search stops there. "+
					"Pass emailRuleId so the audience is named rather than searched for",
				recipientID, sendToRecipientAudienceScanCap)
		}
		scanned++
		r, found, err := w.store.RecipientByID(ctx, audienceID, recipientID)
		if err != nil {
			return Recipient{}, "", err
		}
		if found {
			return r, audienceID, nil
		}
	}
	return Recipient{}, "", fmt.Errorf("recipient %q is not readable in any of this caller's audiences", recipientID)
}
