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
	"sync/atomic"
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
	verifyGate                                        func(context.Context, string) error
}

func (r AzureDeliveryReport) Verify(ctx context.Context) error {
	if r.verify == nil {
		return errors.New("provider feedback has no held receipt")
	}
	return r.verify(ctx)
}

// VerifyGate checks that the native callback's ledger lock is part of the
// original receipt transaction, rather than taking another pool connection.
func (r AzureDeliveryReport) VerifyGate(ctx context.Context, key string) error {
	if r.verifyGate == nil {
		return errors.New("provider feedback has no held delivery gate")
	}
	return r.verifyGate(ctx, key)
}

type AzureDeliveryGateResolver func(context.Context, string) (string, error)

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
func NewAzureFeedbackHandler(resolve func() *Integration, apply func(context.Context, AzureDeliveryReport) error, gateResolver ...AzureDeliveryGateResolver) http.Handler {
	const (
		invalidHeaders = iota
		invalidAccount
		invalidScheme
		unconfiguredReceiver
		wrongCredential
		wrongSubscription
	)
	reasons := [...]string{"invalid_headers", "invalid_account", "invalid_bearer_scheme", "receiver_unconfigured", "credential_mismatch", "subscription_mismatch"}
	// A public endpoint must not let rejected requests flood logs. Each fixed
	// reason gets at most one warning per minute on this replica; no request or
	// stored credential values, lengths, or derived identifiers are logged.
	var nextWarning [len(reasons)]atomic.Int64
	reject := func(w http.ResponseWriter, integration *Integration, reason int) {
		if integration.logger != nil {
			now := time.Now().Unix()
			next := nextWarning[reason].Load()
			if now >= next && nextWarning[reason].CompareAndSwap(next, now+60) {
				integration.logger.Warn("Azure email feedback authorization refused", "reason", reasons[reason])
			}
		}
		http.Error(w, "invalid email feedback authorization", http.StatusUnauthorized)
	}
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
		for _, name := range []string{"Authorization", "X-Memql-Email-Account", "aeg-subscription-name", "aeg-event-type"} {
			values := r.Header.Values(name)
			if len(values) != 1 || values[0] == "" || strings.Contains(values[0], ",") {
				reject(w, integration, invalidHeaders)
				return
			}
		}
		account := r.Header.Get("X-Memql-Email-Account")
		authorization := r.Header.Get("Authorization")
		if len(account) > 128 || memql.BareShortId(account) != account || strings.ContainsAny(account, "/\\\r\n\t ") {
			reject(w, integration, invalidAccount)
			return
		}
		// HTTP authentication schemes are case-insensitive (RFC 9110, 11.1).
		// RFC 6750 permits one or more separator spaces. Consume only those;
		// token bytes remain case-sensitive, with no trailing whitespace trimming.
		scheme, credential, separated := strings.Cut(authorization, " ")
		credential = strings.TrimLeft(credential, " ")
		if !separated || !strings.EqualFold(scheme, "Bearer") || credential == "" || len(authorization) > 512 {
			reject(w, integration, invalidScheme)
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
		if !found || configured.AccountID != account || configured.Secret == "" || configured.Subscription == "" {
			reject(w, integration, unconfiguredReceiver)
			return
		}
		if subtle.ConstantTimeCompare([]byte(credential), []byte(configured.Secret)) != 1 {
			reject(w, integration, wrongCredential)
			return
		}
		// Event Grid supplies an Azure resource name. Azure can return names
		// with different casing than creation and requires case-insensitive comparison.
		if !strings.EqualFold(r.Header.Get("aeg-subscription-name"), configured.Subscription) {
			reject(w, integration, wrongSubscription)
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
			if err := a.applyDeliveryEvent(ctx, configured, event, reports[index], apply, gateResolver...); err != nil {
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

func (a *azureSetup) applyDeliveryEvent(ctx context.Context, configured azureFeedbackConnection, event azureEvent, data azureDeliveryData, apply func(context.Context, AzureDeliveryReport) error, gateResolver ...AzureDeliveryGateResolver) error {
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
	keys := []string{"email-operation:" + key}
	initial, _, found, err := store.read(ctx, key)
	if err != nil || !found {
		return errors.New("the original Azure send receipt is unavailable")
	}
	if len(gateResolver) > 0 && gateResolver[0] != nil && initial.DeliveryID != "" {
		gate, err := gateResolver[0](emailStateContext(ctx), initial.DeliveryID)
		if err != nil {
			return err
		}
		if gate != "" {
			keys = append(keys, gate)
		}
	}
	hold, err := memql.AcquireGateHold(context.WithoutCancel(ctx), db, 0, keys...)
	if err != nil {
		return err
	}
	defer hold.Release()
	op, prior, found, err := store.read(ctx, key)
	if err != nil {
		return err
	}
	if !found || op.DeliveryID != initial.DeliveryID || op.ID != data.MessageID || op.AccountID != configured.AccountID || !sameAzureResource(op.ResourceID, event.Topic) || op.RecipientDigest != azureMailboxDigest(data.Recipient) || op.SenderDigest != azureMailboxDigest(data.Sender) {
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
		report := AzureDeliveryReport{OperationID: op.ID, DeliveryID: op.DeliveryID, AccountID: op.AccountID, Email: data.Recipient, Status: status, OccurredAt: data.AttemptTime.UTC(), AcceptedAt: op.SubmittedAt, verify: hold.Verify, verifyGate: func(ctx context.Context, key string) error {
			for _, held := range keys[1:] {
				if held == key {
					return hold.Verify(ctx)
				}
			}
			return errors.New("provider feedback did not reserve this delivery gate")
		}}
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
