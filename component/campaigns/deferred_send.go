package campaigns

import (
	"context"
	"fmt"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"time"
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
			_, err = w.sendToRecipientHeld(actor, args, receipt, opts)
			if err == nil || isUndecided(err) || memql.IsWriteGateBusy(err) {
				return err
			}
			// Only a conclusive pre-send refusal leaves the retry scan. Keep the real
			// mailbox in the ledger; the references are checked by the write guard.
			recipient, found, readErr := w.store.RecipientByID(actor, str(row, "recipientId"))
			if readErr != nil {
				return readErr
			}
			if !found {
				return err
			}
			if verifyErr := hold.Verify(ctx); verifyErr != nil {
				return verifyErr
			}
			return detachedWrite(actor, func(write context.Context) error {
				return w.recordLoneDelivery(write, args, Campaign{AccountID: str(row, "accountId"), TemplateID: str(row, "templateId"), SenderIdentityID: str(row, "senderIdentityId")}, recipient, str(row, "emailRuleId"), Delivery{Status: "failed", LastError: err.Error()})
			})
		})
		if next == "" || next == cursor {
			return
		}
		cursor = next
	}
}
