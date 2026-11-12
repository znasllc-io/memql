package campaigns

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/email"
)

// ledgerHarness is one engine receiving Event Grid delivery reports for
// synthetic send operations. All identifiers and credentials are fixture values.
type ledgerHarness struct {
	n        *digestNewsletter
	campaign string
	receipt  func(label, status string, withRow bool) (operation, address, delivery string)
	// laneReceipt is a sent row lane message's receipt: its delivery row names a
	// row (rowConcept, rowId) and a recipient id that names no recipient row.
	laneReceipt func(label string) (operation, address, delivery string)
	post        func(operation, address, status string) int
	// noAcceptedTime stores the next receipts with no accepted time, as a
	// send whose response was lost has none.
	noAcceptedTime bool
}

func newLedgerHarness(t *testing.T) *ledgerHarness {
	t.Helper()
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ad", 32))
	t.Setenv("MEMQL_EMAIL_TRANSPORT", "capture")
	t.Setenv("MEMQL_DOMAIN", "example.test")
	f := newDigestFixture(t)
	n := f.newEmptyNewsletter(t, "ledger-hop")
	tiny, err := database.NewGatePool(dbtest.DSN(), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tiny.Close() })
	sharedGate := func() *sql.DB { return tiny }
	n.worker.newsletterGate = NewWorker(deliveryDBEngine{n.engine}, nil, nil, quietLogger(), sharedGate).newsletterGate
	var provider *email.Integration
	for _, registration := range memql.RegisteredPlugins() {
		if registration.Name == "email" {
			p, err := registration.Factory(memql.PluginContext{Engine: feedbackCampaignEngine{MemQLEngine: n.engine, prefix: n.suffix}, GateDB: sharedGate, Logger: quietLogger()})
			if err != nil {
				t.Fatal(err)
			}
			provider = p.(*email.Integration)
			if err = n.engine.RegisterIntegration(p); err != nil {
				t.Fatal(err)
			}
		}
	}
	if provider == nil {
		t.Fatal("email plugin is missing")
	}
	now := time.Now().UTC()
	seal := func(value any) string {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		encrypted, _, err := secret.Encrypt(string(data))
		if err != nil {
			t.Fatal(err)
		}
		return encrypted
	}
	state := func(key string, value any) {
		t.Helper()
		if key == "azure-cluster" {
			key = n.suffix + key
		}
		n.execute(t, n.system, call("mutation", "storeEmailConnectionState", arg{"id", string(id.New().FromString("email-state:" + key))}, arg{"encryptedValue", seal(value)}, arg{"versionTime", time.Now().UTC().Format(time.RFC3339Nano)}))
	}
	plan := map[string]any{"clusterId": n.suffix, "subscriptionId": "12345678-1234-1234-1234-123456789012", "resourceGroup": "feedback-tests", "emailService": "synthetic", "communicationService": "synthetic", "domain": "example.test", "dataLocation": "United States"}
	topic := "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/feedback-tests/providers/Microsoft.Communication/communicationServices/synthetic"
	state("azure-cluster", map[string]any{"id": n.suffix, "status": "connected"})
	state("organization:"+n.org, map[string]any{"accountId": n.org, "status": "ready", "plan": plan})
	result, err := n.engine.Execute(n.owner, call("builtin", "emailAzureSetup", arg{"accountId", n.org}, arg{"action", "status"}))
	if err != nil {
		t.Fatal(err)
	}
	rows := memql.MaterializeRows(result)
	if len(rows) != 1 || str(rows[0], "planId") == "" {
		t.Fatalf("status %v", rows)
	}
	credential := "synthetic-private-feedback-credential"
	state("azure-feedback:"+n.org, map[string]any{"accountId": n.org, "planId": str(rows[0], "planId"), "topic": topic, "subscription": "synthetic-subscription", "secret": credential, "status": "ready"})
	campaign := n.suffix + "-campaign"
	n.execute(t, n.owner, call("mutation", "createCampaign", arg{"campaignId", campaign}, arg{"name", "Ledger"}, arg{"audienceId", n.audience}, arg{"templateId", n.suffix + "-tpl"}, arg{"accountId", n.org}))
	if err := n.store.SetCampaignStatus(n.owner, "startCampaign", campaign); err != nil {
		t.Fatal(err)
	}
	handler := email.NewAzureFeedbackHandler(func() *email.Integration { return provider }, n.worker.ApplyAzureDelivery, n.worker.AzureDeliveryGateKey)
	h := &ledgerHarness{n: n, campaign: campaign}
	h.receipt = func(label, status string, withRow bool) (string, string, string) {
		t.Helper()
		recipient := n.suffix + "-" + label
		address := label + "-" + n.suffix + "@example.test"
		n.execute(t, n.owner, call("mutation", "addRecipient", arg{"recipientId", recipient}, arg{"audienceId", n.audience}, arg{"accountId", n.org}, arg{"email", address}))
		delivery := deliveryRowID(campaign, recipient)
		if withRow {
			if err := n.store.RecordDelivery(n.owner, Delivery{CampaignID: campaign, RecipientID: recipient, Email: address, AccountID: n.org, Status: status, Attempts: 1}); err != nil {
				t.Fatal(err)
			}
		}
		operation := id.NewShortId()
		value := map[string]any{"ID": operation, "AccountID": n.org, "DeliveryID": delivery, "ResourceID": topic, "RecipientDigest": EmailDigest(address), "SenderDigest": EmailDigest("sender@example.test"), "Status": "unknown", "SubmittedAt": now}
		if h.noAcceptedTime {
			value["SubmittedAt"] = time.Time{}
		}
		n.execute(t, n.system, call("mutation", "storeEmailSendOperation", arg{"id", n.suffix + "-op-" + label}, arg{"accountId", n.org}, arg{"providerMessageId", operation}, arg{"status", "unknown"}, arg{"nextPollAt", now.Format(time.RFC3339Nano)}, arg{"encryptedValue", seal(value)}, arg{"versionTime", now.Format(time.RFC3339Nano)}))
		return operation, address, delivery
	}
	h.laneReceipt = func(label string) (string, string, string) {
		t.Helper()
		request := n.suffix + "-lane-" + label
		recipient := rowRecipientID("v1:test:laneFeedback", request)
		address := label + "-" + n.suffix + "@example.test"
		if err := n.store.RecordDelivery(n.owner, Delivery{RecipientID: recipient, Email: address, AccountID: n.org, Status: "sent", Attempts: 1, RequestID: request,
			TemplateID: n.suffix + "-tpl", RowConcept: "v1:test:laneFeedback", RowID: request, RowKind: "approved"}); err != nil {
			t.Fatal(err)
		}
		delivery := singleDeliveryRowID(request, recipient)
		operation := id.NewShortId()
		value := map[string]any{"ID": operation, "AccountID": n.org, "DeliveryID": delivery, "ResourceID": topic, "RecipientDigest": EmailDigest(address), "SenderDigest": EmailDigest("sender@example.test"), "Status": "unknown", "SubmittedAt": now}
		n.execute(t, n.system, call("mutation", "storeEmailSendOperation", arg{"id", n.suffix + "-op-" + label}, arg{"accountId", n.org}, arg{"providerMessageId", operation}, arg{"status", "unknown"}, arg{"nextPollAt", now.Format(time.RFC3339Nano)}, arg{"encryptedValue", seal(value)}, arg{"versionTime", now.Format(time.RFC3339Nano)}))
		return operation, address, delivery
	}
	h.post = func(operation, address, status string) int {
		t.Helper()
		body, _ := json.Marshal([]map[string]any{{"id": "event-" + operation + "-" + status, "eventType": "Microsoft.Communication.EmailDeliveryReportReceived", "topic": topic, "data": map[string]any{"sender": "sender@example.test", "recipient": address, "messageId": operation, "status": status, "deliveryAttemptTimeStamp": now.Add(time.Minute)}}})
		request := httptest.NewRequest(http.MethodPost, email.AzureFeedbackPath, strings.NewReader(string(body)))
		request.Header.Set("Authorization", "Bearer "+credential)
		request.Header.Set("X-Memql-Email-Account", n.org)
		request.Header.Set("aeg-subscription-name", "synthetic-subscription")
		request.Header.Set("aeg-event-type", "Notification")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}
	return h
}

// A send whose POST response was lost is recorded failed. When Azure later
// says it was delivered, the ledger must say sent, with the time it went.
func TestADeliveredReportCorrectsAFailedRow(t *testing.T) {
	h := newLedgerHarness(t)
	operation, address, delivery := h.receipt("lost", "failed", true)
	if code := h.post(operation, address, "Delivered"); code != http.StatusNoContent {
		t.Fatalf("feedback code %d", code)
	}
	row, err := h.n.worker.newsletterRow(h.n.system, "providerDeliveryById", "deliveryId", delivery)
	if err != nil {
		t.Fatal(err)
	}
	if str(row, "status") != "sent" || str(row, "providerStatus") != "delivered" {
		t.Fatalf("ledger %v", row)
	}
	if templateTime(row["sentAt"]).IsZero() {
		t.Fatalf("a delivered report left no sentAt: %v", row)
	}
}

// After Event Grid gives up on a bounce whose delivery row is missing, the
// address must already be suppressed: the suppression is written first, and
// the report stays retryable.
func TestABounceSuppressesEvenWithoutItsDeliveryRow(t *testing.T) {
	h := newLedgerHarness(t)
	operation, address, _ := h.receipt("norow", "", false)
	if code := h.post(operation, address, "Bounced"); code != http.StatusServiceUnavailable {
		t.Fatalf("a bounce with no delivery row answered %d, want a retryable 503", code)
	}
	_, suppressed, err := h.n.worker.store.SuppressionByDigest(h.n.system, EmailDigest(address))
	if err != nil {
		t.Fatal(err)
	}
	if !suppressed {
		t.Fatal("the bounced address was not suppressed")
	}
}

// With no accepted time on the receipt, the delivered report's own time is
// when the message went.
func TestADeliveredReportWithNoAcceptedTimeSetsSentAtFromTheReport(t *testing.T) {
	h := newLedgerHarness(t)
	h.noAcceptedTime = true
	operation, address, delivery := h.receipt("noaccept", "failed", true)
	if code := h.post(operation, address, "Delivered"); code != http.StatusNoContent {
		t.Fatalf("feedback code %d", code)
	}
	row, err := h.n.worker.newsletterRow(h.n.system, "providerDeliveryById", "deliveryId", delivery)
	if err != nil {
		t.Fatal(err)
	}
	sent, provider := templateTime(row["sentAt"]), templateTime(row["providerAt"])
	if str(row, "status") != "sent" || sent.IsZero() || !sent.Equal(provider) {
		t.Fatalf("sentAt %v should be the report's time %v: %v", sent, provider, row)
	}
}

// A bounce of a row lane message suppresses its address and settles its row. Its
// consent audit names no recipient: the row's recipient id was minted for the
// one send and names no audience membership, so the audit is written without it
// rather than refused for it.
func TestABounceOfARowLaneMessageIsRecorded(t *testing.T) {
	h := newLedgerHarness(t)
	operation, address, delivery := h.laneReceipt("bounced")
	if code := h.post(operation, address, "Bounced"); code != http.StatusNoContent {
		t.Fatalf("feedback code %d", code)
	}
	if _, suppressed, err := h.n.worker.store.SuppressionByDigest(h.n.system, EmailDigest(address)); err != nil || !suppressed {
		t.Fatalf("the bounced address was not suppressed: %v %v", suppressed, err)
	}
	row, err := h.n.worker.newsletterRow(h.n.system, "providerDeliveryById", "deliveryId", delivery)
	if err != nil {
		t.Fatal(err)
	}
	if str(row, "providerStatus") != "bounced" || str(row, "rowConcept") != "v1:test:laneFeedback" {
		t.Fatalf("ledger %v", row)
	}
	var audits int
	if err := h.n.db.QueryRowContext(t.Context(), `SELECT count(*) FROM "MemoryNodes" WHERE concept = 'v1:campaigns:consentEvent' AND payload->>'emailDigest' = ? AND coalesce(payload->>'recipientId', '') = ''`, EmailDigest(address)).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("%d consent audits with no recipient for the bounce", audits)
	}
}
