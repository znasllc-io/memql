package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// The receipt gate survives node changes; revalidation and replay happen through
// the ordinary single-send boundary. No provider outcome is retried here.
func (w *Worker) retryDeferredSends(ctx, systemCtx context.Context) {
	cursor := ""
	for page := 0; page < 5; page++ {
		rows, next, err := w.store.rowsPage(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(systemCtx)), cursor, call("query", "deferredSingleSends", arg{"dueAt", w.nowUTC().Format(time.RFC3339Nano)}))
		if err != nil {
			w.logger.Warn("campaigns: deferred send scan failed", "error", err)
			return
		}
		ids := []string{}
		selected := map[string]map[string]any{}
		for _, row := range rows {
			id := str(row, "id")
			ids = append(ids, id)
			selected[id] = row
		}
		_ = campaignPage(ctx, ids, func(ctx context.Context, id string) error {
			row := selected[id]
			owner := bare(str(row, "ownerUserId"))
			if owner == "" {
				return nil
			}
			actor := w.ownerActorContext(ctx, owner)
			args := map[string]any{}
			for _, key := range []string{"templateId", "recipientId", "senderIdentityId", "emailRuleId", "requestId"} {
				args[key] = str(row, key)
			}
			opts := singleSend{deferWhenLimited: true}
			receipt, ok, err := singleSendReceiptFor(actor, args, opts)
			if err != nil || !ok {
				return fmt.Errorf("deferred send identity is invalid")
			}
			if w.singleSendGate == nil {
				return fmt.Errorf("send coordination is unavailable")
			}
			hold, err := w.singleSendGate(context.WithoutCancel(ctx), receipt.id)
			if err != nil {
				return err
			}
			defer hold.Release()
			receipt.hold = hold
			// A second replica may have completed or refused this delivery since
			// the scan. The receipt lock protects this fresh status read too.
			current, err := w.newsletterRow(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(systemCtx)), "deferredSingleSendById", "deliveryId", id)
			if err != nil {
				return err
			}
			if current == nil || str(current, "status") != "pending" || templateTime(current["nextAttemptAt"]).After(w.nowUTC()) {
				return nil
			}
			if bare(str(current, "ownerUserId")) != owner || str(current, "requestId") != str(row, "requestId") {
				return fmt.Errorf("deferred delivery identity changed")
			}
			for key := range args {
				args[key] = str(current, key)
			}
			receipt, ok, err = singleSendReceiptFor(actor, args, opts)
			if err != nil || !ok {
				return fmt.Errorf("deferred send identity is invalid")
			}
			receipt.hold = hold
			finish := func(status, reason string, sentAt time.Time) error {
				return w.settleDeferredDelivery(ctx, systemCtx, hold, id, status, reason, sentAt)
			}
			// Reconcile a durable provider receipt before asking the former
			// owner for permission to send again. This records an already-known
			// outcome even after revocation, and never grants send authority.
			prior, err := w.store.rows(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(systemCtx)), call("query", "campaignSingleSendById", arg{"receiptId", receipt.id}))
			if err != nil {
				return err
			}
			if len(prior) > 0 {
				saved := prior[0]
				if str(saved, "fingerprint") != receipt.fingerprint || !sameOrganization(str(saved, "accountId"), str(current, "accountId")) {
					return finish("failed", "The saved send receipt does not match this request.", time.Time{})
				}
				if str(saved, "status") == "complete" && objectField(saved, "result")["sent"] == true {
					return finish("sent", "", templateTime(saved["createdAt"]))
				}
				return finish("failed", "The previous send may have reached the provider. It will not be submitted again.", time.Time{})
			}
			nodes, sendErr := w.sendToRecipientHeld(actor, args, receipt, opts)
			if isUndecided(sendErr) || memql.IsWriteGateBusy(sendErr) || ctx.Err() != nil {
				return sendErr
			}
			if sendErr != nil {
				return finish("failed", sendErr.Error(), time.Time{})
			}
			if len(nodes) != 1 {
				return fmt.Errorf("deferred send result is unavailable")
			}
			var result map[string]any
			if err := json.Unmarshal(nodes[0].Payload, &result); err != nil {
				return err
			}
			switch {
			case result["sent"] == true:
				return finish("sent", "", receipt.revision)
			case result["skipped"] == true:
				return finish("skipped", str(result, "reason"), time.Time{})
			case result["uncertain"] == true:
				return finish("failed", str(result, "reason"), time.Time{})
			default:
				return nil // A rate-limit deferral remains pending.
			}
		})
		if next == "" || next == cursor {
			return
		}
		cursor = next
	}
}

// The verdict updates only an existing pending row, with attribution preserved.
// A normal send may already have written its outcome; fresh read avoids replacing it.
func (w *Worker) settleDeferredDelivery(ctx, system context.Context, hold gateHold, id, status, reason string, sentAt time.Time) error {
	current, err := w.newsletterRow(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(system)), "deferredSingleSendById", "deliveryId", id)
	if err != nil || current == nil {
		return err
	}
	if str(current, "status") != "pending" {
		return nil
	}
	values := map[string]any{"deliveryId": id, "status": status, "lastError": "", "skipReason": "", "attempts": integer(current, "attempts"), "versionTime": memql.VersionTimeAfter(templateTime(current["createdAt"]), w.nowUTC()).Format(time.RFC3339Nano)}
	if status == "failed" {
		values["lastError"] = truncateError(reason)
	}
	if status == "skipped" {
		values["skipReason"] = truncateError(reason)
	}
	if status == "sent" {
		values["attempts"] = max(1, integer(current, "attempts"))
		if !sentAt.IsZero() {
			values["sentAt"] = sentAt.UTC().Format(time.RFC3339Nano)
		}
	}
	if err := hold.Verify(ctx); err != nil {
		return err
	}
	return detachedWrite(system, func(write context.Context) error {
		return w.newsletterWrite(w.systemActorContext(write), "settleDeferredSingleSend", values)
	})
}
