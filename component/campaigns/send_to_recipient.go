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

var errCampaignRateLimited = errors.New("campaigns.sendToRecipient: the send-rate limit is exhausted; try again shortly")

// undecidedSend is an error that says nothing about the message. Something the
// send needed -- a read, the record that an attempt began -- failed, or was cut
// short, before the attempt began: the database was slow, a statement timed out, a
// lock was lost. Nothing was attempted, and the same send, made again, may well go
// out.
//
// It exists for the callers that turn an error into a verdict. A welcome whose send
// is refused on the merits (the template is gone, the address changed, the sender is
// disabled) is blocked, and a blocked welcome is never retried; a welcome whose send
// merely could not be decided is still owed its message and stays pending. The
// text of the error is the wrapped error's, unchanged.
type undecidedSend struct{ error }

func (e undecidedSend) Unwrap() error { return e.error }

// undecided marks err as a failure that decided nothing about the send.
func undecided(err error) error {
	if err == nil {
		return nil
	}
	return undecidedSend{err}
}

// isUndecided reports whether err says nothing about the message (undecidedSend).
func isUndecided(err error) bool {
	var u undecidedSend
	return errors.As(err, &u)
}

func (w *Worker) sendToRecipient(ctx context.Context, args map[string]any, receipt *singleSendReceipt, opts singleSend) ([]memorynodes.MemoryNode, error) {
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

	// Resolve both resources under the caller's identity. After checking that
	// they share an organization, require update permission there before mail
	// can leave; visibility alone grants no sending authority.
	tmpl, found, err := w.store.TemplateByID(ctx, templateID)
	if err != nil {
		return nil, undecided(fmt.Errorf("campaigns.sendToRecipient: %w", err))
	}
	if !found {
		return nil, fmt.Errorf("campaigns.sendToRecipient: template %q is not readable", templateID)
	}
	if opts.snapshot != nil {
		if bare(opts.snapshot.ID) != bare(tmpl.ID) || !sameOrganization(opts.snapshot.AccountID, tmpl.AccountID) || opts.snapshot.Status != "ready" {
			return nil, errors.New("campaigns.sendToRecipient: reviewed welcome snapshot does not match its template")
		}
		tmpl = *opts.snapshot
	}
	if tmpl.Status != "ready" {
		return nil, errors.New("campaigns.sendToRecipient: publish the reviewed template before sending")
	}

	recipient, found, err := w.store.RecipientByID(ctx, recipientID)
	if err != nil {
		return nil, undecided(fmt.Errorf("campaigns.sendToRecipient: %w", err))
	}
	if !found {
		// "No such row" and "not yours" are one answer here, deliberately.
		// The composite tier decides it inside the query, so this branch
		// cannot distinguish them and must not try: an error that said which
		// would be an existence oracle over every operator's recipients,
		// reachable by anybody who can call the builtin.
		return nil, fmt.Errorf("campaigns.sendToRecipient: recipient %q is not readable", recipientID)
	}

	if opts.emailDigest != "" && EmailDigest(recipient.Email) != opts.emailDigest {
		return nil, errors.New("campaigns.sendToRecipient: the recipient address changed after signup; the original consent does not cover the new address")
	}

	if err := w.requireSendAuthority(ctx, recipient.AccountID); err != nil {
		return nil, err
	}
	if !sameOrganization(tmpl.AccountID, recipient.AccountID) {
		return nil, errors.New("campaigns.sendToRecipient: template and recipient must belong to the same organization")
	}
	if emailRuleID != "" {
		rows, err := w.store.rows(ctx, call("query", "emailRuleById", arg{"emailRuleId", emailRuleID}))
		if err != nil {
			// The rule could not be read: that is the database's failure, not an
			// answer about the rule.
			return nil, undecided(fmt.Errorf("campaigns.sendToRecipient: email rule must be readable and belong to the recipient's organization: %w", err))
		}
		if len(rows) != 1 || !sameOrganization(recipient.AccountID, str(rows[0], "accountId")) {
			return nil, errors.New("campaigns.sendToRecipient: email rule must be readable and belong to the recipient's organization")
		}
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
	//
	// Its name is the template's subject, as it has always been, and that is
	// what {{campaignName}} says for a send that has no campaign. It is cut to
	// the length of a campaign's name because it is carried in the view link
	// like one, and a subject is the one such name nothing else bounds. It is
	// NOT what the footer names: that is the sending identity (F6).
	campaign := Campaign{
		AccountID:        recipient.AccountID,
		ID:               emailRuleID,
		OwnerUserID:      ownerUserID,
		Name:             clipRunes(strings.TrimSpace(tmpl.Subject), maxCampaignNameRunes),
		TemplateID:       templateID,
		SenderIdentityID: senderIdentityID,
		Status:           "sending",
		// Tracking OFF. A tracked link needs a delivery row id in the body,
		// and this path's delivery id is derived from a rule id that may be
		// empty -- so a pixel would attribute hits to a row that is not
		// per-send distinguishable. A send under a request now has a row of its
		// own (singleDeliveryRowID), but one without a request does not, and an
		// engagement event names a campaign. Event-email tracking is a
		// follow-up, not a thing to approximate.
	}

	identity, refusal := w.resolveSendIdentity(ctx, campaign)
	if refusal.refused() {
		err := fmt.Errorf("campaigns.sendToRecipient: %s", refusal.Reason)
		if !refusal.Terminal {
			// An identity that could not be READ ("the send will retry"), as opposed
			// to one that is missing, retired or another organization's.
			err = undecided(err)
		}
		return nil, err
	}

	// SUPPRESSION AT THE POINT OF SEND, before anything is rendered. The
	// cluster list outranks the recipient row exactly as it does on the
	// campaign path, and the ordering is the mechanism: an address
	// re-imported after a bounce has a recipient row saying `subscribed`.
	digest := EmailDigest(recipient.Email)
	if digest == "" {
		return nil, fmt.Errorf("campaigns.sendToRecipient: recipient %q has no usable address", recipientID)
	}
	if sup, suppressed, err := w.store.SuppressionForSend(w.systemActorContext(ctx), campaign.AccountID, digest); err != nil {
		// A failed lookup must NOT read as "not suppressed". Refusing is the
		// only safe answer: a delayed message is recoverable, one sent to
		// somebody who opted out is not. Nor is it a verdict on the message.
		return nil, undecided(fmt.Errorf("campaigns.sendToRecipient: the suppression list could not be consulted, "+
			"so this send is refused rather than made: %w", err))
	} else if suppressed {
		w.recordLoneDelivery(ctx, args, campaign, recipient, emailRuleID, Delivery{
			Status: "skipped", SkipReason: sup.Reason,
		})
		return resultNode("campaignRecipientSend", map[string]any{
			"sent": false, "skipped": true, "reason": sup.Reason,
			"recipientId": recipient.ID, "emailRuleId": emailRuleID,
		})
	}
	if recipient.SubscriptionStatus != "" && recipient.SubscriptionStatus != "subscribed" && !(opts.confirm && recipient.SubscriptionStatus == "pending") {
		w.recordLoneDelivery(ctx, args, campaign, recipient, emailRuleID, Delivery{
			Status: "skipped", SkipReason: recipient.SubscriptionStatus,
		})
		return resultNode("campaignRecipientSend", map[string]any{
			"sent": false, "skipped": true, "reason": recipient.SubscriptionStatus,
			"recipientId": recipient.ID, "emailRuleId": emailRuleID,
		})
	}

	accountName, err := w.store.AccountName(ctx, campaign.AccountID)
	if err != nil {
		w.logger.Debug("campaigns.sendToRecipient: could not resolve the account name", "error", err)
	}
	renderOpts := RenderOptions{
		ReplyTo:     replyToFor(campaign, identity),
		AccountName: accountName,
		// The footer names the identity's display name, then the organization
		// (F6). It used to name the synthetic campaign, which is the subject.
		SenderName: identity.SendAs.FromName,
	}
	// A rule is not a persisted campaign. Keep its ID out of the links'
	// campaign relationship; the signed organization and digest identify the opt-out.
	// The view link names the sender the footer shows, which the options decide.
	links, err := w.messageLinks(linkRequest{
		Confirm: opts.confirm, OwnerUserID: ownerUserID, RecipientID: recipient.ID, AccountID: campaign.AccountID, EmailDigest: digest,
		TemplateID: tmpl.ID, TemplateRevision: tmpl.Revision,
		CampaignName: campaign.Name, SenderName: footerSender(renderOpts),
	})
	if err != nil {
		return nil, fmt.Errorf("campaigns.sendToRecipient: cannot mint the links for the message: %w", err)
	}
	renderOpts.Links = links
	msg, err := renderMessage(campaign, tmpl, recipient, renderOpts)
	if err != nil {
		return nil, fmt.Errorf("campaigns.sendToRecipient: %w", err)
	}

	if !w.allowSend() {
		if opts.deferWhenLimited && receipt != nil {
			if err := w.recordLoneDelivery(ctx, args, campaign, recipient, emailRuleID, Delivery{Status: "pending", NextAttemptAt: w.nowUTC().Add(time.Minute)}); err != nil {
				return nil, undecided(err)
			}
			return resultNode("campaignRecipientSend", map[string]any{"sent": false, "skipped": false, "deferred": true, "reason": "The send-rate limit is reached; this email will be retried shortly.", "recipientId": recipient.ID, "emailRuleId": emailRuleID})
		}
		return nil, errCampaignRateLimited
	}
	// Record the attempt before contacting the transport. After this point a
	// timeout or a node loss is uncertain, and replay must never send it again.
	//
	// So everything that can be refused for being BUSY comes first. A transport that
	// must take a lock of its own before it can begin (Azure's send receipt) is asked
	// for it here, before the record: a refusal then means nothing was attempted, no
	// receipt exists, and the caller may try again later. The same refusal after the
	// record would leave a receipt saying an attempt began, which replay never sends
	// again, for a message that never went out.
	if receipt != nil {
		msg.IntentID = "campaign-single:" + receipt.id
		receipt.accountID = campaign.AccountID
		reserved, giveBack, err := w.reserveDelivery(ctx, msg, identity.SendAs)
		if err != nil {
			return nil, undecided(fmt.Errorf("campaigns.sendToRecipient: %w", err))
		}
		defer giveBack()
		ctx = reserved
		if err := w.recordAttempt(ctx, receipt); err != nil {
			return nil, undecided(err)
		}
	}
	now := w.nowUTC()
	sendCtx, cancel := context.WithTimeout(ctx, w.cfg.SendTimeout)
	sendErr := w.deliver(sendCtx, msg, identity.SendAs)
	cancel()

	if sendErr != nil {
		w.recordLoneDelivery(ctx, args, campaign, recipient, emailRuleID, Delivery{
			Status: "failed", Attempts: 1, LastError: sendErr.Error(),
		})
		return nil, fmt.Errorf("campaigns.sendToRecipient: %w", sendErr)
	}

	w.reputation.observeAs(now, identity.Label, recipient.Email, "accepted")
	w.noteActiveIdentity(identity.Label)
	w.recordLoneDelivery(ctx, args, campaign, recipient, emailRuleID, Delivery{
		Status: "sent", SentAt: now, Attempts: 1,
	})
	return resultNode("campaignRecipientSend", map[string]any{
		"sent": true, "skipped": false,
		"recipientId": recipient.ID, "emailRuleId": emailRuleID,
		"sentAs": identity.SendAs.Address,
	})
}

// recordAttempt writes the receipt that says an attempt began. It is a write the
// receipt's lock exists to protect -- a replica that took the lock after this one
// gave it up, and read no receipt, would send the message again -- so it is made as
// a signup's writes are, and as a welcome's verdict is: only if the gate says the lock
// is still held (GateHold.Verify, on the caller's context, which answers its end), and
// on detachedWrite, so it commits while the lock is held or does not start. On the
// caller's context it would be cut where that context ends: the client stops waiting
// and the server goes on and commits it after the lock was given back.
//
// An error means no attempt was recorded and the transport was not contacted. (The
// write may still have reached the server; replay then finds a receipt that says an
// attempt began and does not send, which is the safe side.)
func (w *Worker) recordAttempt(ctx context.Context, receipt *singleSendReceipt) error {
	if receipt.hold == nil {
		// Whoever sends under a receipt holds its lock and says so; a send that does
		// not is a wiring mistake, and the attempt is not recorded without the lock.
		return errors.New("campaigns.sendToRecipient: the send does not hold its receipt's lock, so its attempt is not recorded")
	}
	if err := receipt.hold.Verify(ctx); err != nil {
		return fmt.Errorf("campaigns.sendToRecipient: the send's lock is not held, so its attempt is not recorded: %w", err)
	}
	return detachedWrite(ctx, func(ctx context.Context) error {
		return w.saveSingleSendReceipt(ctx, receipt, "attempting", nil)
	})
}

// recordLoneDelivery ledgers one outcome.
//
// The synthetic campaign carries a rendering identity, not a relationship to
// a persisted campaign. Only emailRuleId is stored for a rule send; pretending
// it is campaignId would point at the wrong concept and fail organization
// validation after the message has already left.
//
// # One row per request (memql#6022, F7)
//
// A send made under a stable requestId -- a welcome, a journey step, a rule's
// event -- is keyed by that request: the delivery id derives from (requestId,
// recipientId) (singleDeliveryRowID), so every message to a person has a row of
// its own, and "which message did this person get" has an answer. The row names
// the request, the template and the sender. Before this the id derived from
// (emailRuleId, recipientId), or the recipient alone when no rule was behind the
// send, and a welcome, a journey step and a rule's send to one person were
// versions of ONE row, each replacing the last.
//
// A send made WITHOUT a request has nothing to key by and keeps the key it always
// had. Two such sends from one rule to one person are still two versions of one
// row, and that is history rather than safety: nothing reads this ledger to decide
// whether to send -- an event triggered the send, not a roster diff -- so what is
// lost is the earlier send's record, not correctness. A retry of the SAME request
// writes a new version of ITS row, as a campaign's resumed send does.
//
// Best-effort, and that asymmetry is the campaign path's: the message IS
// sent, and a failed ledger write must not turn a delivered message into an
// error the caller retries. Logged loudly instead, because an unrecorded send
// is invisible everywhere else.
func (w *Worker) recordLoneDelivery(ctx context.Context, args map[string]any, campaign Campaign, recipient Recipient, emailRuleID string, d Delivery) error {
	d.CampaignID = "" // This synthetic send has no campaign relationship.
	d.RecipientID = recipient.ID
	d.Email = recipient.Email
	d.EmailRuleID = emailRuleID
	// What a single send is, beside who it went to. The request id was checked
	// against singleSendRequestID before any send was made (singleSendReceiptFor);
	// blank when the send was made without one, and then not stamped.
	d.RequestID = argString(args, "requestId")
	d.TemplateID = campaign.TemplateID
	d.SenderIdentityID = campaign.SenderIdentityID
	// The client tie, copied from the campaign (epic memql#5165, section J).
	// Set HERE rather than at the four call sites above, because this is the
	// one place every lone delivery passes through -- and a stamp spread
	// across four literals is one a fifth call site forgets.
	d.AccountID = campaign.AccountID
	if err := w.store.RecordDelivery(ctx, d); err != nil {
		w.logger.Warn("campaigns.sendToRecipient: could not record the delivery outcome",
			"recipient", recipient.ID, "rule", emailRuleID, "status", d.Status, "error", err)
		return err
	}
	return nil
}
