package campaigns

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

// ApplyAzureDelivery is the native receiver seam, never a public capability.
// The integration has authenticated and correlated the provider report while
// holding its original send receipt. These are mandatory provenance writes;
// no follow-on send, journey, or subscription workflow is started here.
func (w *Worker) ApplyAzureDelivery(ctx context.Context, report email.AzureDeliveryReport) error {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return fmt.Errorf("provider feedback requires internal origin")
	}
	digest := EmailDigest(report.Email)
	if digest == "" || report.OperationID == "" || report.AccountID == "" {
		return fmt.Errorf("provider feedback identity is missing")
	}
	write := func(effect func(context.Context) error) error {
		if err := report.Verify(ctx); err != nil {
			return err
		}
		return detachedWrite(ctx, func(commit context.Context) error { return effect(w.systemActorContext(commit)) })
	}
	system := w.systemActorContext(memql.ContextWithFreshRead(ctx))
	var delivery map[string]any
	var err error
	if report.DeliveryID != "" {
		delivery, err = w.newsletterRow(system, "providerDeliveryById", "deliveryId", report.DeliveryID)
		if err != nil {
			return err
		}
		if delivery == nil {
			return fmt.Errorf("the original campaign delivery is not recorded yet")
		}
		if !sameOrganization(str(delivery, "accountId"), report.AccountID) || EmailDigest(str(delivery, "email")) != digest {
			return fmt.Errorf("the provider receipt does not match the campaign delivery")
		}
	}
	hard := report.Status == "bounced" || report.Status == "suppressed"
	if hard {
		// The id is the mailbox digest, so the stored list remains private and
		// idempotent. Provider prose is never copied into the shared suppression.
		if err := write(func(commit context.Context) error {
			return w.store.RecordSuppression(commit, digest, "hard_bounce", EmailDomain(report.Email), str(delivery, "campaignId"), "Azure reported a permanent delivery failure.")
		}); err != nil {
			return err
		}
		if delivery != nil {
			eventID := sha256Hex("azure-bounce\x00" + report.OperationID)
			prior, err := w.newsletterRow(system, "providerConsentById", "eventId", eventID)
			if err != nil {
				return err
			}
			if prior == nil {
				values := map[string]any{"eventId": eventID, "emailDigest": digest, "ownerUserId": str(delivery, "ownerUserId"), "accountId": report.AccountID, "recipientId": str(delivery, "recipientId"), "campaignId": str(delivery, "campaignId"), "occurredAt": report.OccurredAt.UTC().Format(time.RFC3339Nano)}
				if err := write(func(commit context.Context) error { return w.newsletterWrite(commit, "recordProviderBounce", values) }); err != nil {
					return err
				}
				w.reputation.observe(w.nowUTC(), report.Email, "hard_bounce")
			}
		}
	}
	if delivery == nil {
		return nil
	} // Operational mail retains the provider receipt.
	if str(delivery, "providerStatus") == report.Status && templateTime(delivery["providerAt"]).Equal(report.OccurredAt) {
		return nil
	}
	values := map[string]any{"deliveryId": report.DeliveryID, "providerStatus": report.Status, "providerAt": report.OccurredAt.UTC().Format(time.RFC3339Nano), "versionTime": memql.VersionTimeAfter(templateTime(delivery["createdAt"]), w.nowUTC()).Format(time.RFC3339Nano)}
	if templateTime(delivery["sentAt"]).IsZero() && !report.AcceptedAt.IsZero() {
		values["sentAt"] = report.AcceptedAt.UTC().Format(time.RFC3339Nano)
	}
	if str(delivery, "status") != "sent" && str(delivery, "campaignId") != "" {
		values["providerCountersPending"] = true
	}
	return write(func(commit context.Context) error { return w.newsletterWrite(commit, "recordProviderDelivery", values) })
}
