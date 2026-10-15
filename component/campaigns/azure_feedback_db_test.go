package campaigns

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	langparser "github.com/znasllc-io/memql/component/language/parser"
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
	// One shared connection must accommodate receipt + ledger together.
	tiny, err := database.NewGatePool(dbtest.DSN(), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tiny.Close() })
	sharedGate := func() *sql.DB { return tiny }
	w2.newsletterGate = NewWorker(deliveryDBEngine{e2}, nil, nil, quietLogger(), sharedGate).newsletterGate
	providers := make([]*email.Integration, 2)
	for index, engine := range []*memql.MemQLEngine{n.engine, e2} {
		for _, registration := range memql.RegisteredPlugins() {
			if registration.Name == "email" {
				provider, err := registration.Factory(memql.PluginContext{Engine: feedbackCampaignEngine{MemQLEngine: engine, prefix: n.suffix}, GateDB: sharedGate, Logger: quietLogger()})
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
		if key == "azure-cluster" {
			key = n.suffix + key
		}
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
	if err := n.store.SetCampaignStatus(n.owner, "startCampaign", campaign); err != nil {
		t.Fatal(err)
	}
	if err := n.store.EnqueueSend(n.system, SendJob{CampaignID: campaign, CampaignOwnerUserID: n.user, CampaignAccountID: n.org, AudienceID: n.audience, TemplateID: n.suffix + "-tpl", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	five, zero := 5, 0
	if err := n.store.UpdateJob(n.system, campaign, SendJobPatch{FailedCount: &five}); err != nil {
		t.Fatal(err)
	}
	if err := n.store.UpdateCampaignProgress(n.owner, campaign, CampaignProgress{FailedCount: &five}); err != nil {
		t.Fatal(err)
	}
	handler := email.NewAzureFeedbackHandler(func() *email.Integration { return providers[1] }, func(ctx context.Context, report email.AzureDeliveryReport) error {
		err := w2.ApplyAzureDelivery(ctx, report)
		if err != nil {
			t.Logf("apply: %v", err)
		}
		return err
	}, w2.AzureDeliveryGateKey)
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
			if err := db2.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE concept='v1:campaigns:consentEvent' AND payload->>'emailDigest'=? AND payload->>'kind'='bounce' AND payload->>'ownerUserId' IN (?,?)`, EmailDigest(address), n.user, "v1:identity:user:"+n.user).Scan(&count); err != nil {
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

	// These five reports repaired genuinely unknown provider receipts, while
	// the original worker may still be completing other recipients. Preserve a
	// durable request until a terminal job can be recounted safely.
	pendingRows := func() []map[string]any {
		t.Helper()
		rows, err := w2.store.rows(n.system, call("query", "providerCounterLedger", arg{"campaignId", campaign}, arg{"accountId", n.org}, arg{"ownerUserId", n.user}))
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	pending := pendingRows()
	if len(pending) != 5 {
		t.Fatalf("repair ledger has %d rows", len(pending))
	}
	for _, row := range pending {
		if !boolean(row, "providerCountersPending") {
			t.Fatal("unknown receipt recovery did not request counter repair")
		}
	}
	w2.repairProviderCounters(context.Background())
	job, found, err := w2.store.JobByID(memql.ContextWithFreshRead(n.system), campaign)
	if err != nil || !found || job.FailedCount != five || job.SentCount != zero {
		t.Fatalf("running job was repaired: %+v %v", job, err)
	}
	stale := job

	// Replica 2 waits behind replica 1's progress gate, then re-reads the
	// terminal state from the database rather than its earlier running state.
	hold, err := n.worker.progressGate(context.Background(), campaign)
	if err != nil {
		t.Fatal(err)
	}
	// Both repair locks must fit on one connection; nested gate acquisition
	// would wait on itself even after the peer releases its progress lock.
	w2.newsletterGate = NewWorker(deliveryDBEngine{e2}, nil, nil, quietLogger(), func() *sql.DB { return tiny }).newsletterGate
	entered, done := make(chan struct{}), make(chan error, 1)
	originalGate := w2.newsletterGate
	w2.newsletterGate = func(ctx context.Context, keys ...string) (gateHold, error) {
		close(entered)
		return originalGate(ctx, keys...)
	}
	go func() { done <- w2.repairProviderCampaign(context.Background(), pending[0]) }()
	<-entered
	select {
	case err := <-done:
		hold.Release()
		t.Fatalf("repair passed peer progress gate: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	completed := "completed"
	if err := n.store.UpdateJob(n.system, campaign, SendJobPatch{Status: &completed}); err != nil {
		hold.Release()
		t.Fatal(err)
	}
	sentStatus := "sent"
	if err := n.store.UpdateCampaignProgress(n.owner, campaign, CampaignProgress{Status: &sentStatus}); err != nil {
		hold.Release()
		t.Fatal(err)
	}
	hold.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("repair did not resume after peer released gate")
	}
	w2.newsletterGate = originalGate
	job, found, err = w2.store.JobByID(memql.ContextWithFreshRead(n.system), campaign)
	if err != nil || !found || job.FailedCount != zero || job.SentCount != five {
		t.Fatalf("terminal job not repaired: %+v %v", job, err)
	}
	rows, err = n.store.rows(memql.ContextWithFreshRead(n.owner), call("query", "campaignById", arg{"campaignId", campaign}))
	if err != nil || len(rows) != 1 || integer(rows[0], "sentCount") != five || integer(rows[0], "failedCount") != zero || str(rows[0], "status") != "sent" || bare(str(rows[0], "ownerUserId")) != n.user {
		t.Fatalf("campaign repair: %v %v", rows, err)
	}
	for _, row := range pendingRows() {
		if boolean(row, "providerCountersPending") {
			t.Fatal("completed repair left queue marker")
		}
	}
	n.worker.stampProgress(n.system, n.owner, stale)
	n.worker.completeJob(n.system, n.owner, stale, Campaign{ID: campaign})
	job, _, _ = w2.store.JobByID(memql.ContextWithFreshRead(n.system), campaign)
	if job.SentCount != five || job.FailedCount != zero {
		t.Fatal("older worker overwrote provider repair")
	}
	if err := w2.ApplyAzureDelivery(n.system, email.AzureDeliveryReport{OperationID: "forged", AccountID: n.org, Email: "forged@example.test", Status: "bounced"}); err == nil {
		t.Fatal("constructed report suppressed an address")
	}
	if _, err := n.engine.Execute(n.owner, call("mutation", "recordProviderBounce", arg{"eventId", "forged"}, arg{"emailDigest", EmailDigest("forged@example.test")}, arg{"accountId", n.org}, arg{"recipientId", "forged"}, arg{"occurredAt", now.Format(time.RFC3339Nano)})); err == nil {
		t.Fatal("public caller forged provider audit")
	}

	// A corrupt historical row must not silently give the audit to the
	// engine's system actor or suppress a mailbox before attribution is known.
	operation, address, missingOwner := receipt("missing-owner", "failed")
	if _, err := db2.ExecContext(context.Background(), `UPDATE "MemoryNodes" SET payload=payload-'ownerUserId' WHERE concept='v1:campaigns:delivery' AND id=?`, "v1:campaigns:delivery:"+missingOwner); err != nil {
		t.Fatal(err)
	}
	if code := post(operation, address, "Bounced"); code != http.StatusServiceUnavailable {
		t.Fatalf("missing owner report code %d", code)
	}
	if _, suppressed, err := w2.store.SuppressionByDigest(n.system, EmailDigest(address)); err != nil || suppressed {
		t.Fatalf("missing owner changed suppression: %v %v", suppressed, err)
	}

	// Revoking future send permission cannot erase the attribution of a
	// provider outcome for a message already submitted.
	operation, address, _ = receipt("revoked-owner", "failed")
	n.execute(t, n.system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Sender","role":"reader","active":true})`, langparser.QuoteString(n.user), langparser.QuoteString(n.user+"@example.test")))
	if code := post(operation, address, "Bounced"); code != http.StatusNoContent {
		t.Fatalf("revoked owner report code %d", code)
	}
	var auditOwner string
	if err := db2.QueryRowContext(context.Background(), `SELECT payload->>'ownerUserId' FROM "MemoryNodes" WHERE concept='v1:campaigns:consentEvent' AND payload->>'emailDigest'=? AND payload->>'kind'='bounce'`, EmailDigest(address)).Scan(&auditOwner); err != nil || bare(auditOwner) != n.user {
		t.Fatalf("revoked owner audit %q: %v", auditOwner, err)
	}
}

// Isolate the installation-wide fixture without weakening production lookup.
type feedbackCampaignEngine struct {
	*memql.MemQLEngine
	prefix string
}

func (e feedbackCampaignEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	if strings.Contains(query, "emailConnectionStateById(") || strings.Contains(query, "storeEmailConnectionState(") {
		original := string(id.New().FromString("email-state:azure-cluster"))
		isolated := string(id.New().FromString("email-state:" + e.prefix + "azure-cluster"))
		query = strings.ReplaceAll(query, original, isolated)
	}
	return e.MemQLEngine.Execute(ctx, query)
}
