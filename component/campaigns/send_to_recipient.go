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
// # Resolving the recipient
//
// One read: `recipientById`, under the caller's own actor. It is worth saying
// what that replaced, because the shape recurs. Until the query existed every
// read on v1:campaigns:recipient was audience-scoped -- every other caller
// already held an audience id and this one does not -- so the audience had to
// be DERIVED: from the emailRule when one named it, and otherwise by walking
// the caller's audiences and refusing past a bound.
//
// A bounded scan standing in for a by-id read is a correct answer that gets
// SLOWER AS AN OPERATOR SUCCEEDS, and refuses at exactly the point somebody
// has enough audiences to care. It also made the two paths behave
// differently for no reason a caller could see: a rule-driven send resolved
// in one read, the same call by hand took a roster walk per audience.
//
// The tier conjunct on the query is what makes an unscoped by-id read safe --
// a recipient the caller does not own is simply not found, which is the same
// answer campaignById gives. The audience was never a check; it was a search
// key, and there is nothing left on this path that needs the audience itself.

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

	recipient, found, err := w.store.RecipientByID(ctx, recipientID)
	if err != nil {
		return nil, fmt.Errorf("campaigns.sendToRecipient: %w", err)
	}
	if !found {
		// "No such row" and "not yours" are one answer here, deliberately.
		// The composite tier decides it inside the query, so this branch
		// cannot distinguish them and must not try: an error that said which
		// would be an existence oracle over every operator's recipients,
		// reachable by anybody who can call the builtin.
		return nil, fmt.Errorf("campaigns.sendToRecipient: recipient %q is not readable", recipientID)
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
		w.recordLoneDelivery(ctx, campaign, recipient, emailRuleID, Delivery{
			Status: "skipped", SkipReason: sup.Reason,
		})
		return resultNode("campaignRecipientSend", map[string]any{
			"sent": false, "skipped": true, "reason": sup.Reason,
			"recipientId": recipient.ID, "emailRuleId": emailRuleID,
		})
	}
	if recipient.SubscriptionStatus != "" && recipient.SubscriptionStatus != "subscribed" {
		w.recordLoneDelivery(ctx, campaign, recipient, emailRuleID, Delivery{
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

	if !w.allowSend() {
		return nil, errors.New("campaigns.sendToRecipient: the send-rate limit is exhausted; try again shortly")
	}
	now := w.nowUTC()
	sendCtx, cancel := context.WithTimeout(ctx, w.cfg.SendTimeout)
	sendErr := w.deliver(sendCtx, msg, identity.SendAs)
	cancel()

	if sendErr != nil {
		w.recordLoneDelivery(ctx, campaign, recipient, emailRuleID, Delivery{
			Status: "failed", Attempts: 1, LastError: sendErr.Error(),
		})
		return nil, fmt.Errorf("campaigns.sendToRecipient: %w", sendErr)
	}

	w.reputation.observeAs(now, identity.Label, recipient.Email, "accepted")
	w.noteActiveIdentity(identity.Label)
	w.recordLoneDelivery(ctx, campaign, recipient, emailRuleID, Delivery{
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
// # The rule id is stamped HERE, from an explicit argument
//
// On this path the synthetic campaign's id happens to BE the rule id, so
// `d.CampaignID` would have carried it by accident. It is passed separately
// anyway, because that coincidence is a property of one line in the caller
// and not a thing the ledger should depend on: change what the synthetic
// campaign id holds and the answer to "which rule mailed this person" would
// change with it, silently and in a row somebody audits.
//
// It is left EMPTY by every other writer in this package, which is what makes
// the field mean something -- an ordinary campaign delivery carrying a rule
// id would be a false attribution, and there is no reader that could tell.
//
// # One row per (rule, recipient), and that is history rather than safety
//
// The delivery id derives from (campaignId, recipientId), so a rule that
// fires twice for the same person writes a new VERSION of one row rather than
// two rows. On the campaign path that derivation IS the idempotency
// mechanism; here nothing reads the ledger to decide whether to send -- an
// event triggered this, not a roster diff -- so what is lost is the earlier
// send's record, not correctness. Worth knowing before treating this ledger
// as a per-send log.
//
// Best-effort, and that asymmetry is the campaign path's: the message IS
// sent, and a failed ledger write must not turn a delivered message into an
// error the caller retries. Logged loudly instead, because an unrecorded send
// is invisible everywhere else.
func (w *Worker) recordLoneDelivery(ctx context.Context, campaign Campaign, recipient Recipient, emailRuleID string, d Delivery) {
	d.CampaignID = campaign.ID
	d.RecipientID = recipient.ID
	d.Email = recipient.Email
	d.EmailRuleID = emailRuleID
	if err := w.store.RecordDelivery(ctx, d); err != nil {
		w.logger.Warn("campaigns.sendToRecipient: could not record the delivery outcome",
			"recipient", recipient.ID, "rule", emailRuleID, "status", d.Status, "error", err)
	}
}
