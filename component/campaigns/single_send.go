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
	ctx = memql.ContextWithFreshRead(ctx)
	requestID := argString(args, "requestId")
	if requestID == "" {
		return w.sendToRecipient(ctx, args, nil)
	}
	owner := callerUserID(ctx)
	if owner == "" || !singleSendRequestID.MatchString(requestID) {
		return nil, fmt.Errorf("campaigns.sendToRecipient: a signed-in caller and a stable request identifier are required")
	}
	if w.singleSendGate == nil {
		return nil, fmt.Errorf("campaigns.sendToRecipient: send coordination is unavailable")
	}
	values := map[string]string{}
	for _, key := range []string{"templateId", "recipientId", "senderIdentityId", "emailRuleId"} {
		values[key] = memql.BareShortId(strings.TrimSpace(argString(args, key)))
	}
	encoded, _ := json.Marshal(values)
	receipt := &singleSendReceipt{id: sha256Hex("campaign-single-send\x00" + owner + "\x00" + requestID), fingerprint: sha256Hex(string(encoded))}
	release, err := w.singleSendGate(ctx, receipt.id)
	if err != nil {
		return nil, err
	}
	defer release()
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
	nodes, err := w.sendToRecipient(ctx, args, receipt)
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
