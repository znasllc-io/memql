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

type singleSendReceipt struct {
	id, fingerprint, accountID string
	revision                   time.Time
}

// Event automations supply a stable requestId for one intended message. The
// same request cannot call the transport twice, even if the first replica dies
// after the provider accepted it. This is at-most-once submission, not a claim
// that SMTP or a provider can guarantee exactly-once mailbox delivery.
func (w *Worker) handleSendToRecipient(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return w.sendToRecipientOnce(ctx, args, nil, "")
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
func singleSendReceiptFor(ctx context.Context, args map[string]any, snapshot *Template, expectedEmailDigest string) (receipt *singleSendReceipt, ok bool, err error) {
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
	if snapshot != nil {
		values["templateRevision"] = snapshot.Revision
	}
	if expectedEmailDigest != "" {
		values["emailDigest"] = expectedEmailDigest
	}
	encoded, _ := json.Marshal(values)
	return &singleSendReceipt{id: singleSendReceiptID(owner, requestID), fingerprint: sha256Hex(string(encoded))}, true, nil
}

// snapshot is accepted only from an internal durable welcome, never wire args.
func (w *Worker) sendToRecipientOnce(ctx context.Context, args map[string]any, snapshot *Template, expectedEmailDigest string) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	receipt, ok, err := singleSendReceiptFor(ctx, args, snapshot, expectedEmailDigest)
	if err != nil {
		return nil, err
	}
	if !ok {
		return w.sendToRecipient(ctx, args, nil, snapshot, expectedEmailDigest)
	}
	if w.singleSendGate == nil {
		return nil, fmt.Errorf("campaigns.sendToRecipient: send coordination is unavailable")
	}
	release, err := w.singleSendGate(ctx, receipt.id)
	if err != nil {
		// Nothing was attempted: no receipt exists. A busy lock is "try later" to
		// every caller that sends mail, never a verdict on the message.
		return nil, err
	}
	defer release()
	return w.sendToRecipientHeld(ctx, args, snapshot, expectedEmailDigest, receipt)
}

// sendToRecipientHeld is sendToRecipientOnce for a caller that already holds the
// receipt's lock (singleSendLock of receipt.id), taken together with the others it
// needed. It does not take it again: a second transaction asking for a lock the
// first holds would wait for it.
func (w *Worker) sendToRecipientHeld(ctx context.Context, args map[string]any, snapshot *Template, expectedEmailDigest string, receipt *singleSendReceipt) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	values := map[string]string{}
	for _, key := range []string{"templateId", "recipientId", "senderIdentityId", "emailRuleId"} {
		values[key] = memql.BareShortId(strings.TrimSpace(argString(args, key)))
	}
	rows, err := w.store.rows(auth.ContextWithInternalOrigin(ctx), call("query", "campaignSingleSendById", arg{"receiptId", receipt.id}))
	if err != nil {
		return nil, err
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
	nodes, err := w.sendToRecipient(ctx, args, receipt, snapshot, expectedEmailDigest)
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
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
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
