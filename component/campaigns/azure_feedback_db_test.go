package campaigns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/email"
)

// A second engine receives provider feedback with no sender's process-local
// state. All identifiers and credentials here are synthetic fixture values.
func TestAzureFeedbackCampaignAttributionAcrossReplicas(t *testing.T) {
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ad", 32))
	t.Setenv("MEMQL_EMAIL_TRANSPORT", "capture")
	t.Setenv("MEMQL_DOMAIN", "example.test")
	f := newDigestFixture(t)
	n := f.newEmptyNewsletter(t, "provider-hop")
	e2, db2 := digestDBEngine(t)
	w2 := newGatedWorker(t, e2, db2, n.sender)
	providers := make([]*email.Integration, 2)
	for index, engine := range []*memql.MemQLEngine{n.engine, e2} {
		for _, registration := range memql.RegisteredPlugins() {
			if registration.Name == "email" {
				provider, err := registration.Factory(memql.PluginContext{Engine: engine, GateDB: gatePoolFor(t), Logger: quietLogger()})
				if err != nil {
					t.Fatal(err)
				}
				providers[index] = provider.(*email.Integration)
				if err = engine.RegisterIntegration(provider); err != nil {
					t.Fatal(err)
				}
			}
		}
		if providers[index] == nil {
			t.Fatal("email plugin is missing")
		}
	}
	now := time.Now().UTC()
	stateID := func(key string) string { return string(id.New().FromString("email-state:" + key)) }
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
		n.execute(t, n.system, call("mutation", "storeEmailConnectionState", arg{"id", stateID(key)}, arg{"encryptedValue", seal(value)}, arg{"versionTime", time.Now().UTC().Format(time.RFC3339Nano)}))
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
	n.execute(t, n.owner, call("mutation", "createCampaign", arg{"campaignId", campaign}, arg{"name", "Provider receipt"}, arg{"audienceId", n.audience}, arg{"templateId", n.suffix + "-tpl"}, arg{"accountId", n.org}))
	handler := email.NewAzureFeedbackHandler(func() *email.Integration { return providers[1] }, func(ctx context.Context, report email.AzureDeliveryReport) error {
		err := w2.ApplyAzureDelivery(ctx, report)
		if err != nil {
			t.Logf("apply: %v", err)
		}
		return err
	})
	receipt := func(label, status string) (string, string, string) {
		t.Helper()
		recipient := n.suffix + "-" + label
		address := label + "-" + n.suffix + "@example.test"
		n.execute(t, n.owner, call("mutation", "addRecipient", arg{"recipientId", recipient}, arg{"audienceId", n.audience}, arg{"accountId", n.org}, arg{"email", address}))
		if err := n.store.RecordDelivery(n.owner, Delivery{CampaignID: campaign, RecipientID: recipient, Email: address, AccountID: n.org, Status: status, Attempts: 1}); err != nil {
			t.Fatal(err)
		}
		delivery := deliveryRowID(campaign, recipient)
		operation := id.NewShortId()
		value := map[string]any{"ID": operation, "AccountID": n.org, "DeliveryID": delivery, "ResourceID": topic, "RecipientDigest": EmailDigest(address), "SenderDigest": EmailDigest("sender@example.test"), "Status": "unknown", "SubmittedAt": now}
		n.execute(t, n.system, call("mutation", "storeEmailSendOperation", arg{"id", n.suffix + "-op-" + label}, arg{"accountId", n.org}, arg{"providerMessageId", operation}, arg{"status", "unknown"}, arg{"nextPollAt", now.Format(time.RFC3339Nano)}, arg{"encryptedValue", seal(value)}, arg{"versionTime", now.Format(time.RFC3339Nano)}))
		return operation, address, delivery
	}
	post := func(operation, address, status string) int {
		t.Helper()
		body, _ := json.Marshal([]map[string]any{{"id": "event-" + operation + "-" + status, "eventType": "Microsoft.Communication.EmailDeliveryReportReceived", "topic": topic, "data": map[string]any{"sender": "sender@example.test", "recipient": address, "messageId": operation, "status": status, "deliveryAttemptTimeStamp": now}}})
		request := httptest.NewRequest(http.MethodPost, email.AzureFeedbackPath, strings.NewReader(string(body)))
		request.Header.Set("Authorization", "Bearer "+credential)
		request.Header.Set("X-Memql-Email-Account", n.org)
		request.Header.Set("aeg-subscription-name", "synthetic-subscription")
		request.Header.Set("aeg-event-type", "Notification")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 204 {
			t.Log(response.Body.String())
		}
		return response.Code
	}
	for _, tc := range []struct {
		label, status string
		hard          bool
	}{{"delivered", "Delivered", false}, {"bounced", "Bounced", true}, {"filtered", "FilteredSpam", false}, {"failed", "Failed", false}, {"expanded", "Expanded", false}} {
		t.Run(tc.label, func(t *testing.T) {
			operation, address, delivery := receipt(tc.label, "failed")
			for repeat := 0; repeat < 2; repeat++ {
				if code := post(operation, address, tc.status); code != 204 {
					t.Fatalf("feedback code %d", code)
				}
			}
			row, err := w2.newsletterRow(n.system, "providerDeliveryById", "deliveryId", delivery)
			if err != nil {
				t.Fatal(err)
			}
			if str(row, "status") != "sent" || str(row, "providerStatus") == "" || bare(str(row, "ownerUserId")) != n.user {
				t.Fatalf("ledger %v", row)
			}
			var count int
			if err := db2.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE concept='v1:campaigns:consentEvent' AND payload->>'emailDigest'=? AND payload->>'kind'='bounce'`, EmailDigest(address)).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.hard {
				want = 1
			}
			if count != want {
				t.Fatalf("bounce versions=%d want%d", count, want)
			}
			_, suppressed, err := w2.store.SuppressionByDigest(n.system, EmailDigest(address))
			if err != nil {
				t.Fatal(err)
			}
			if suppressed != tc.hard {
				t.Fatalf("hard suppression %v", suppressed)
			}
		})
	}
	stats, err := w2.handleStats(n.owner, map[string]any{"campaignId": campaign}, 0)
	if err != nil {
		t.Fatal(err)
	}
	values := decodeResult(t, stats)
	summary := values["delivery"].(map[string]any)
	if values["sent"] != float64(5) || summary["confirmed"] != float64(1) || summary["failed"] != float64(3) || summary["expanded"] != float64(1) || summary["unconfirmed"] != float64(0) {
		t.Fatalf("stats %v", values)
	}
	if err := w2.ApplyAzureDelivery(n.system, email.AzureDeliveryReport{OperationID: "forged", AccountID: n.org, Email: "forged@example.test", Status: "bounced"}); err == nil {
		t.Fatal("constructed report suppressed an address")
	}
	if _, err := n.engine.Execute(n.owner, call("mutation", "recordProviderBounce", arg{"eventId", "forged"}, arg{"emailDigest", EmailDigest("forged@example.test")}, arg{"accountId", n.org}, arg{"ownerUserId", n.user}, arg{"recipientId", "forged"}, arg{"occurredAt", now.Format(time.RFC3339Nano)})); err == nil {
		t.Fatal("public caller forged provider audit")
	}
}
