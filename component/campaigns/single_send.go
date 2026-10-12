package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

var singleSendRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,160}$`)

// completeReceiptLimit bounds the write of the receipt that records an accepted
// send. The message is out whatever becomes of it, so it is written on a context
// the caller's end does not reach, for at most this long.
const completeReceiptLimit = 5 * time.Second

type singleSendReceipt struct {
	id, fingerprint, accountID string
	revision                   time.Time

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
	return w.sendToRecipientOnce(ctx, args, singleSend{})
}

// singleSend is what only an internal, durable caller can say about a single send,
// and never wire args: the reviewed template snapshot a welcome froze at signup, and
// the digest of the address its consent covered. The zero value is an ordinary send.
type singleSend struct {
	// snapshot, when set, is the template the send must carry: the copy frozen when
	// the person signed up, checked against the stored template's identity.
	snapshot *Template
	// emailDigest, when set, is the digest of the address consent was recorded for;
	// the send is refused if the recipient's address has since changed.
	emailDigest string
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
		if str(row, "status") == "complete" {
			result := objectField(row, "result")
			if result == nil || result["sent"] != true {
				return nil, fmt.Errorf("campaigns.sendToRecipient: saved send receipt is unreadable; refusing to resubmit")
			}
			result["replayed"] = true
			return resultNode("campaignRecipientSend", result)
		}
		return resultNode("campaignRecipientSend", map[string]any{
			"sent": false, "skipped": false, "uncertain": true, "replayed": true,
			"reason":      "The previous send may have reached the provider. It will not be submitted again.",
			"recipientId": values["recipientId"], "emailRuleId": values["emailRuleId"],
		})
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

func (w *Worker) saveSingleSendReceipt(ctx context.Context, receipt *singleSendReceipt, status string, result map[string]any) error {
	revision := memql.VersionTimeAfter(receipt.revision, w.nowUTC())
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
