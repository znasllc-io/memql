package email

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// AzureFeedbackPath is the owner-approved Event Grid HTTP exception (#6037).
const AzureFeedbackPath = "/email/events/azure"
const azureDeliveryEvent = "Microsoft.Communication.EmailDeliveryReportReceived"
const azureValidationEvent = "Microsoft.EventGrid.SubscriptionValidationEvent"

// AzureDeliveryReport is emitted only after webhook authentication and correlation
// with a pre-send receipt. Campaign attribution comes from that receipt.
// Email is temporary protocol data; persisted feedback uses its digest.
type AzureDeliveryReport struct {
	OperationID, DeliveryID, AccountID, Email, Status string
	OccurredAt, AcceptedAt                            time.Time
	verify                                            func(context.Context) error
}

func (r AzureDeliveryReport) Verify(ctx context.Context) error {
	if r.verify == nil {
		return errors.New("provider feedback has no held receipt")
	}
	return r.verify(ctx)
}

type acsMessageKey struct{}
type acsMessageIdentity struct{ DeliveryID, RecipientDigest, SenderDigest string }

func azureMailboxDigest(address string) string {
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(address))))
	return hex.EncodeToString(sum[:])
}
func withACSMessage(ctx context.Context, message Message, sender string) context.Context {
	recipient, _ := mail.ParseAddress(message.To)
	value := acsMessageIdentity{DeliveryID: message.DeliveryID, SenderDigest: azureMailboxDigest(sender)}
	if recipient != nil {
		value.RecipientDigest = azureMailboxDigest(recipient.Address)
	}
	return context.WithValue(ctx, acsMessageKey{}, value)
}

type azureEvent struct {
	ID        string          `json:"id"`
	Topic     string          `json:"topic"`
	EventType string          `json:"eventType"`
	EventTime time.Time       `json:"eventTime"`
	Data      json.RawMessage `json:"data"`
}
type azureDeliveryData struct {
	Sender      string    `json:"sender"`
	Recipient   string    `json:"recipient"`
	MessageID   string    `json:"messageId"`
	Status      string    `json:"status"`
	AttemptTime time.Time `json:"deliveryAttemptTimeStamp"`
}

func azureDeliveryStatus(status string) string {
	return map[string]string{"Delivered": "delivered", "Suppressed": "suppressed", "Bounced": "bounced", "Quarantined": "quarantined", "FilteredSpam": "filtered-spam", "Expanded": "expanded", "Failed": "failed"}[status]
}
func sameAzureResource(a, b string) bool {
	left, err := url.PathUnescape(a)
	if err != nil {
		return false
	}
	right, err := url.PathUnescape(b)
	if err != nil {
		return false
	}
	return left != "" && strings.EqualFold(left, right)
}

// NewAzureFeedbackHandler authenticates Event Grid's supported secret static
// Authorization header. Authentication is not the subscription handshake: every
// delivery also needs the credential, expected topic, and original send receipt.
// Secrets never appear in URLs, logs, events, or the public connection response.
func NewAzureFeedbackHandler(resolve func() *Integration, apply func(context.Context, AzureDeliveryReport) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", 405)
			return
		}
		if resolve == nil || apply == nil {
			http.Error(w, "email feedback unavailable", 503)
			return
		}
		integration := resolve()
		if integration == nil || integration.azure == nil || integration.azure.store == nil {
			http.Error(w, "email feedback unavailable", 503)
			return
		}
		account := r.Header.Get("X-Memql-Email-Account")
		authorization := r.Header.Get("Authorization")
		if len(account) == 0 || len(account) > 128 || memql.BareShortId(account) != account || strings.ContainsAny(account, "/\\\r\n\t ") || len(r.Header.Values("Authorization")) != 1 || len(r.Header.Values("X-Memql-Email-Account")) != 1 || !strings.HasPrefix(authorization, "Bearer ") || len(authorization) > 512 {
			http.Error(w, "invalid email feedback authorization", 401)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		a := integration.azure
		var configured azureFeedbackConnection
		found, _, err := a.store.read(ctx, "azure-feedback:"+account, &configured)
		if err != nil {
			http.Error(w, "email feedback unavailable", 503)
			return
		}
		if !found || configured.AccountID != account || configured.Secret == "" || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(authorization, "Bearer ")), []byte(configured.Secret)) != 1 || r.Header.Get("aeg-subscription-name") != configured.Subscription {
			http.Error(w, "invalid email feedback authorization", 401)
			return
		}
		// Disconnect and resource replacement invalidate the old receiver on every replica.
		var connection azureConnection
		found, _, err = a.store.read(ctx, "organization:"+account, &connection)
		if err != nil {
			http.Error(w, "email feedback unavailable", 503)
			return
		}
		var cluster azureClusterConnection
		clusterFound, _, clusterErr := a.store.read(ctx, azureClusterKey, &cluster)
		if clusterErr != nil {
			http.Error(w, "email feedback unavailable", 503)
			return
		}
		if !clusterFound || (cluster.Status != "connected" && cluster.Status != "reauthorize") || cluster.ID != connection.Plan.ClusterID {
			http.Error(w, "email feedback connection is inactive", 403)
			return
		}
		if !found || connection.Status != "ready" || azurePlanID(connection) != configured.PlanID || !sameAzureResource(connection.Plan.communicationPath(), configured.Topic) {
			http.Error(w, "email feedback connection is inactive", 403)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "email feedback is too large", 413)
			return
		}
		var events []azureEvent
		if json.Unmarshal(data, &events) != nil || len(events) == 0 || len(events) > 100 {
			http.Error(w, "invalid email feedback", 400)
			return
		}
		for _, event := range events {
			if event.ID == "" || len(event.ID) > 256 || !sameAzureResource(event.Topic, configured.Topic) {
				http.Error(w, "invalid email feedback source", 400)
				return
			}
		}
		if len(events) == 1 && events[0].EventType == azureValidationEvent {
			var validation struct {
				Code string `json:"validationCode"`
			}
			if r.Header.Get("aeg-event-type") != "SubscriptionValidation" || json.Unmarshal(events[0].Data, &validation) != nil || validation.Code == "" || len(validation.Code) > 512 {
				http.Error(w, "invalid subscription validation", 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"validationResponse": validation.Code})
			return
		}
		if r.Header.Get("aeg-event-type") != "Notification" {
			http.Error(w, "invalid email feedback event", 400)
			return
		}
		reports := make([]azureDeliveryData, len(events))
		for index, event := range events {
			if event.EventType != azureDeliveryEvent || json.Unmarshal(event.Data, &reports[index]) != nil || !azureUUID.MatchString(reports[index].MessageID) || azureDeliveryStatus(reports[index].Status) == "" || azureMailboxDigest(reports[index].Recipient) == "" || azureMailboxDigest(reports[index].Sender) == "" || reports[index].AttemptTime.IsZero() || reports[index].AttemptTime.After(a.clock().Add(10*time.Minute)) {
				http.Error(w, "invalid email delivery report", 400)
				return
			}
		}
		for index, event := range events {
			if err := a.applyDeliveryEvent(ctx, configured, event, reports[index], apply); err != nil {
				// Never log provider payloads, bearer values, or recipient addresses.
				if integration.logger != nil {
					integration.logger.Warn("Azure email feedback will retry", "error", err)
				}
				w.Header().Set("Retry-After", "15")
				http.Error(w, "email feedback could not be recorded; retry", 503)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (a *azureSetup) applyDeliveryEvent(ctx context.Context, configured azureFeedbackConnection, event azureEvent, data azureDeliveryData, apply func(context.Context, AzureDeliveryReport) error) error {
	store := &storedACSOperations{connection: a.store}
	rows, err := store.rows(ctx, "emailSendOperationByProviderId", map[string]any{"accountId": configured.AccountID, "providerMessageId": data.MessageID})
	if err != nil {
		return err
	}
	if len(rows) != 1 {
		return errors.New("the original Azure send receipt is not available")
	}
	key := memql.BareShortId(fmt.Sprint(rows[0]["id"]))
	db, err := a.store.gates()
	if err != nil {
		return err
	}
	hold, err := memql.AcquireGateHold(context.WithoutCancel(ctx), db, 0, "email-operation:"+key)
	if err != nil {
		return err
	}
	defer hold.Release()
	op, prior, found, err := store.read(ctx, key)
	if err != nil {
		return err
	}
	if !found || op.ID != data.MessageID || op.AccountID != configured.AccountID || !sameAzureResource(op.ResourceID, event.Topic) || op.RecipientDigest != azureMailboxDigest(data.Recipient) || op.SenderDigest != azureMailboxDigest(data.Sender) {
		return errors.New("the delivery report does not match the original Azure send")
	}
	status := azureDeliveryStatus(data.Status)
	if at := op.DeliveryEvents[status]; !at.IsZero() && !data.AttemptTime.After(at) {
		return nil
	}
	// Bounces remain terminal even if a delayed, contradictory delivery event
	// arrives later. A later failure never erases a confirmed MTA handoff.
	rank := map[string]int{"expanded": 1, "failed": 2, "quarantined": 2, "filtered-spam": 2, "delivered": 3, "bounced": 4, "suppressed": 4}
	if rank[status] > rank[op.DeliveryStatus] || (rank[status] == rank[op.DeliveryStatus] && data.AttemptTime.After(op.DeliveryAt)) {
		report := AzureDeliveryReport{OperationID: op.ID, DeliveryID: op.DeliveryID, AccountID: op.AccountID, Email: data.Recipient, Status: status, OccurredAt: data.AttemptTime.UTC(), AcceptedAt: op.SubmittedAt, verify: hold.Verify}
		if err := hold.Verify(ctx); err != nil {
			return err
		}
		if err := apply(emailStateContext(ctx), report); err != nil {
			return err
		}
		op.DeliveryStatus, op.DeliveryAt = status, data.AttemptTime.UTC()
	}
	if op.DeliveryEvents == nil {
		op.DeliveryEvents = map[string]time.Time{}
	}
	op.DeliveryEvents[status] = data.AttemptTime.UTC()
	// A recipient report is proof that the original operation was accepted and
	// processed, even when the caller lost the POST response.
	op.Status, op.Detail, op.CheckedAt = "succeeded", "", a.clock()
	if err := hold.Verify(ctx); err != nil {
		return err
	}
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, err = store.write(write, key, op, prior)
	return err
}
