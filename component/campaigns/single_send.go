package campaigns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

var singleSendRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,160}$`)

// completeReceiptLimit bounds the write of the receipt that records an accepted
// send. The message is out whatever becomes of it, so it is written on a context
// the caller's end does not reach, for at most this long.
const completeReceiptLimit = 5 * time.Second

// What a receipt says about its send. An attempt that began says "attempting"
// until it is known how it ended: "complete" (the provider accepted it, and its
// result is replayed) or "failed" (the provider definitely did not accept it, and
// the next attempt submits it again). An "attempting" receipt is never submitted
// again while the provider's record of it says nothing (reconcileAttempt).
const (
	receiptAttempting = "attempting"
	receiptComplete   = "complete"
	receiptFailed     = "failed"
)

type singleSendReceipt struct {
	id, fingerprint, accountID string
	// revision is the version this call wrote, zero until it writes one: a send
	// whose revision is set began.
	revision time.Time
	// prior is the stored version this call replaces -- a receipt that failed, or
	// one whose attempt is being settled -- so the next version is written after it.
	prior time.Time

	// hold is the gate holding this receipt's lock (singleSendLock of id), set by
	// whoever holds it before the send is made under the receipt. The receipt that
	// says an attempt began is written only if the hold says its lock is still held
	// (recordAttempt).
	hold gateHold
}

// Event automations supply a stable requestId for one intended message. The
// same request cannot call the transport twice, even if the first replica dies
// after the provider accepted it. This is at-most-once submission, not a claim
// that SMTP or a provider can guarantee exactly-once mailbox delivery.
func (w *Worker) handleSendToRecipient(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return w.sendToRecipientOnce(ctx, args, singleSend{deferWhenLimited: true})
}

// singleSend is what only an internal, durable caller can say about a single send,
// and never wire args: the reviewed template snapshot a welcome froze at signup, and
// the digest of the address its consent covered. The zero value is an ordinary send.
type singleSend struct {
	// orderRecipient, when set, is the recipient an ORDER entitles
	// (send_order.go): used in place of a recipient row, and asked no
	// subscription -- the order is the reason for the message.
	orderRecipient *Recipient
	// installationSender, set only with orderRecipient, is the trusted job
	// saying an operator chose the installation's own sender for this
	// organization's order mail (send_order.go). Only the transport moves:
	// suppression, the unsubscribe and the ledger stay the organization's.
	installationSender bool
	deferWhenLimited   bool
	confirm            bool
	// snapshot, when set, is the template the send must carry: the copy frozen when
	// the person signed up, checked against the stored template's identity.
	snapshot *Template
	// emailDigest, when set, is the digest of the address consent was recorded for;
	// the send is refused if the recipient's address has since changed.
	emailDigest string
	// beforeAttempt, when set, is asked right before the attempt is recorded and
	// the message handed over, under the caller's locks. An error stops the send
	// with nothing attempted: an automation that was paused sends nothing.
	beforeAttempt func(context.Context) error
}

// singleSendReceiptID is the identity of one single send's receipt, and of the lock
// that serialises it: the caller and the stable request identifier. A flow that
// knows both before it starts can name the lock up front and take it together with
// the others it needs (the newsletter welcome does).
func singleSendReceiptID(owner, requestID string) string {
	return sha256Hex("campaign-single-send\x00" + owner + "\x00" + requestID)
}

// singleSendReceiptFor validates a single send's identity and derives its receipt.
// ok is false for a send with no request identifier, which has no receipt and no
// lock and is made as often as it is asked for.
func singleSendReceiptFor(ctx context.Context, args map[string]any, opts singleSend) (receipt *singleSendReceipt, ok bool, err error) {
	requestID := argString(args, "requestId")
	if requestID == "" {
		return nil, false, nil
	}
	owner := callerUserID(ctx)
	if owner == "" || !singleSendRequestID.MatchString(requestID) {
		return nil, false, fmt.Errorf("campaigns.sendToRecipient: a signed-in caller and a stable request identifier are required")
	}
	values := map[string]string{}
	for _, key := range []string{"templateId", "recipientId", "senderIdentityId", "emailRuleId"} {
		values[key] = memql.BareShortId(strings.TrimSpace(argString(args, key)))
	}
	if opts.snapshot != nil {
		values["templateRevision"] = opts.snapshot.Revision
	}
	if opts.emailDigest != "" {
		values["emailDigest"] = opts.emailDigest
	}
	if opts.confirm {
		values["confirm"] = "true"
	}
	encoded, _ := json.Marshal(values)
	return &singleSendReceipt{id: singleSendReceiptID(owner, requestID), fingerprint: sha256Hex(string(encoded))}, true, nil
}

// opts is accepted only from an internal durable caller (a welcome), never wire args.
func (w *Worker) sendToRecipientOnce(ctx context.Context, args map[string]any, opts singleSend) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	receipt, ok, err := singleSendReceiptFor(ctx, args, opts)
	if err != nil {
		return nil, err
	}
	if !ok {
		return w.sendToRecipient(ctx, args, nil, opts)
	}
	if w.singleSendGate == nil {
		return nil, fmt.Errorf("campaigns.sendToRecipient: send coordination is unavailable")
	}
	// The lock lives until Release, below, and nothing else: the caller's leaving
	// mid-write must not end the transaction that holds it (database/sql rolls a
	// transaction back when its context ends) while the receipt it protects is being
	// written (as a signup's and a welcome's locks). A caller already gone takes none.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hold, err := w.singleSendGate(context.WithoutCancel(ctx), receipt.id)
	if err != nil {
		// Nothing was attempted: no receipt exists. A busy lock is "try later" to
		// every caller that sends mail, never a verdict on the message.
		return nil, err
	}
	defer hold.Release()
	receipt.hold = hold
	return w.sendToRecipientHeld(ctx, args, receipt, opts)
}

// sendToRecipientHeld is sendToRecipientOnce for a caller that already holds the
// receipt's lock (singleSendLock of receipt.id), taken together with the others it
// needed, and has set receipt.hold to the gate that holds it. It does not take it
// again: a second transaction asking for a lock the first holds would wait for it.
func (w *Worker) sendToRecipientHeld(ctx context.Context, args map[string]any, receipt *singleSendReceipt, opts singleSend) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	values := map[string]string{}
	for _, key := range []string{"templateId", "recipientId", "senderIdentityId", "emailRuleId"} {
		values[key] = memql.BareShortId(strings.TrimSpace(argString(args, key)))
	}
	rows, err := w.store.rows(auth.ContextWithInternalOrigin(ctx), call("query", "campaignSingleSendById", arg{"receiptId", receipt.id}))
	if err != nil {
		// The receipt could not be read: that decides nothing about the message.
		return nil, undecided(err)
	}
	if len(rows) > 0 {
		row := rows[0]
		if str(row, "fingerprint") != receipt.fingerprint {
			return nil, fmt.Errorf("campaigns.sendToRecipient: this request identifier already names a different message")
		}
		if err := w.requireSendAuthority(ctx, str(row, "accountId")); err != nil {
			return nil, err
		}
		switch str(row, "status") {
		case receiptComplete:
			result := objectField(row, "result")
			if result == nil || result["sent"] != true {
				return nil, fmt.Errorf("campaigns.sendToRecipient: saved send receipt is unreadable; refusing to resubmit")
			}
			result["replayed"] = true
			return resultNode("campaignRecipientSend", result)
		case receiptFailed:
			// The provider definitely did not accept the previous attempt. This one
			// submits it again, as a new version of the same receipt.
			receipt.prior = templateTime(row["createdAt"])
		default:
			return w.reconcileAttempt(ctx, row, receipt, values)
		}
	}
	nodes, err := w.sendToRecipient(ctx, args, receipt, opts)
	if err != nil || len(nodes) != 1 {
		return nodes, err
	}
	// Only an actual send has an attempting receipt. Suppression and preflight
	// refusals remain retryable: they have performed no external side effect.
	if !receipt.revision.IsZero() {
		var result map[string]any
		if err := json.Unmarshal(nodes[0].Payload, &result); err != nil {
			return nil, err
		}
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), completeReceiptLimit)
		defer cancel()
		if err := w.saveSingleSendReceipt(persist, receipt, "complete", result); err != nil {
			// The provider accepted the message. A persistence failure cannot
			// make it unsent; the prior receipt prevents another submission.
			w.logger.Error("campaigns: accepted single send has an uncertain receipt", "receipt", receipt.id, "error", err)
		}
	}
	return nodes, nil
}

// uncertainSendReason is what a send that may have reached the provider says while
// the provider's record of it says nothing.
const uncertainSendReason = "The previous send may have reached the provider. It is not sent again until the provider says whether it accepted it."

// errPriorSendNotAccepted is a previous attempt the provider's record says it did
// not accept. It is a failed attempt: the next one submits the message again.
var errPriorSendNotAccepted = errors.New("campaigns.sendToRecipient: the provider did not accept the previous attempt; the next attempt sends it again")

// reconcileAttempt settles a receipt that says an attempt began and never said how
// it ended: its answer was lost, or so was the node that waited for it. Only the
// provider's own record of the submission can say (email.DeliveryOutcomes), and
// nothing is submitted again while it says nothing.
//
// Accepted: the receipt is completed and the send replays as sent, with no second
// submission. Not accepted: the receipt says so, and the next attempt submits the
// message again (errPriorSendNotAccepted). Otherwise the send is uncertain, as it
// always was, and says when its attempt began (attemptedAt) so a caller can stop
// waiting for an answer that will not come.
func (w *Worker) reconcileAttempt(ctx context.Context, row map[string]any, receipt *singleSendReceipt, values map[string]string) ([]memorynodes.MemoryNode, error) {
	receipt.prior, receipt.accountID = templateTime(row["createdAt"]), str(row, "accountId")
	switch w.priorOutcome(ctx, receipt) {
	case email.OutcomeAccepted:
		result := map[string]any{"sent": true, "skipped": false, "recipientId": values["recipientId"], "emailRuleId": values["emailRuleId"]}
		if err := w.saveReceiptHeld(ctx, receipt, receiptComplete, result); err != nil {
			// The provider has the message whatever becomes of this write, and a
			// receipt that still says an attempt began never submits it again.
			w.logger.Warn("campaigns: an accepted single send could not be recorded as complete", "receipt", receipt.id, "error", err)
		}
		result["replayed"] = true
		return resultNode("campaignRecipientSend", result)
	case email.OutcomeNotAccepted:
		if err := w.saveReceiptHeld(ctx, receipt, receiptFailed, nil); err != nil {
			return nil, undecided(err)
		}
		return nil, errPriorSendNotAccepted
	}
	return resultNode("campaignRecipientSend", map[string]any{
		"sent": false, "skipped": false, "uncertain": true, "replayed": true,
		"reason": uncertainSendReason, "attemptedAt": receipt.prior.UTC().Format(time.RFC3339Nano),
		"recipientId": values["recipientId"], "emailRuleId": values["emailRuleId"],
	})
}

// priorOutcome asks the transport what became of the attempt a receipt records,
// without submitting anything. A transport that keeps no record of its submissions,
// or cannot read it now, knows nothing.
func (w *Worker) priorOutcome(ctx context.Context, receipt *singleSendReceipt) email.Outcome {
	outcomes, ok := w.resolveSender().(email.DeliveryOutcomes)
	if !ok {
		return email.OutcomeUnknown
	}
	outcome, err := outcomes.DeliveryOutcome(ctx, email.Message{IntentID: singleSendIntent(receipt.id)}, email.SendAs{AccountID: bare(receipt.accountID)})
	if err != nil {
		w.logger.Debug("campaigns: the provider's record of a send could not be read", "receipt", receipt.id, "error", err)
		return email.OutcomeUnknown
	}
	return outcome
}

func (w *Worker) saveSingleSendReceipt(ctx context.Context, receipt *singleSendReceipt, status string, result map[string]any) error {
	after := receipt.revision
	if receipt.prior.After(after) {
		after = receipt.prior
	}
	revision := memql.VersionTimeAfter(after, w.nowUTC())
	if result == nil {
		result = map[string]any{}
	}
	statement, err := langparser.RenderCall("recordCampaignSingleSend", map[string]any{
		"receiptId": receipt.id, "fingerprint": receipt.fingerprint,
		"accountId": receipt.accountID, "status": status, "result": result,
		"versionTime": revision.Format(time.RFC3339Nano),
	})
	if err != nil {
		return err
	}
	err = w.store.execServerOnly(ctx, "mutation "+statement)
	if err == nil {
		receipt.revision = revision
	}
	return err
}
