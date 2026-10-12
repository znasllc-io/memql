package email

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
)

type azureFeedbackWorld struct {
	db         *bun.DB
	instances  [2]*Integration
	account    string
	connection azureConnection
	configured azureFeedbackConnection
	now        time.Time
}

func newAzureFeedbackWorld(t *testing.T) *azureFeedbackWorld {
	t.Helper()
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ad", 32))
	t.Setenv("MEMQL_DOMAIN", "example.test")
	if available, err := dbtest.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	} else if !available {
		dbtest.Unreachable(t, "Azure feedback", dbtest.DSN(), nil)
		return nil
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	w := &azureFeedbackWorld{db: db, account: fmt.Sprintf("acs-feedback-%d", time.Now().UnixNano()), now: time.Now().UTC()}
	for index := range w.instances {
		engine, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		if err = engine.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		w.instances[index] = NewIntegration(NewLogSender(nil), nil)
		w.instances[index].azure = &azureSetup{store: &connectionStore{engine: NewConfigWriter(engine), gateDB: replicaGatePool(t)}, protocol: newAzureProtocol("12345678-1234-1234-1234-123456789012"), now: func() time.Time { return w.now }}
	}
	config := acsFixture(t).cfg
	config.AccountID = w.account
	plan := azurePlan{ClusterID: w.account, SubscriptionID: "12345678-1234-1234-1234-123456789012", ResourceGroup: "feedback-tests", CommunicationService: w.account, EmailService: "test", Domain: "example.test", DataLocation: "United States"}
	config.ResourceID = plan.communicationPath()
	w.connection = azureConnection{AccountID: w.account, Status: "ready", Config: config, Plan: plan}
	w.configured = azureFeedbackConnection{AccountID: w.account, PlanID: azurePlanID(w.connection), Topic: config.ResourceID, Subscription: "expected-subscription", Secret: "private-32-byte-receiver-credential", Status: "ready"}
	store := w.instances[0].azure.store
	cluster := azureClusterConnection{ID: w.account, Status: "connected", Session: azureSession{AccessToken: "private-management-token", TokenExpiresAt: w.now.Add(time.Hour)}}
	for _, record := range []struct {
		key   string
		value any
	}{{azureClusterKey, &cluster}, {"organization:" + w.account, &w.connection}, {"azure-feedback:" + w.account, &w.configured}} {
		saved := reflect.New(reflect.TypeOf(record.value).Elem()).Interface()
		if err := store.change(context.Background(), record.key, saved, func(bool) error { reflect.ValueOf(saved).Elem().Set(reflect.ValueOf(record.value).Elem()); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE payload->>'accountId' = ?`, w.account)
	})
	return w
}
func (w *azureFeedbackWorld) event(t *testing.T, op acsOperation, status string) string {
	t.Helper()
	data := map[string]any{"sender": w.connection.Config.Default, "recipient": "receiver@example.test", "messageId": op.ID, "status": status, "deliveryAttemptTimeStamp": w.now.Format(time.RFC3339Nano)}
	encoded, err := json.Marshal([]map[string]any{{"id": "event-" + op.ID + "-" + status, "topic": w.configured.Topic, "eventType": azureDeliveryEvent, "eventTime": w.now, "data": data}})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
func (w *azureFeedbackWorld) request(handler http.Handler, body string, change func(*http.Request)) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, AzureFeedbackPath, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+w.configured.Secret)
	request.Header.Set("X-Memql-Email-Account", w.account)
	request.Header.Set("aeg-subscription-name", w.configured.Subscription)
	request.Header.Set("aeg-event-type", "Notification")
	if change != nil {
		change(request)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
func (w *azureFeedbackWorld) send(t *testing.T, intent string) (*ACSSender, acsOperation) {
	t.Helper()
	sender, err := NewACSSender(w.connection.Config)
	if err != nil {
		t.Fatal(err)
	}
	sender.operations = &storedACSOperations{connection: w.instances[0].azure.store}
	sender.client = &http.Client{Transport: acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	if err = sender.Send(context.Background(), Message{IntentID: intent, DeliveryID: "delivery-" + intent, To: "receiver@example.test", Subject: "Synthetic receipt test", TextBody: "Fixture"}, SendAs{AccountID: w.account}); err != nil {
		t.Fatal(err)
	}
	op, _, found, err := sender.operations.read(context.Background(), sender.operationKey(intent))
	if err != nil || !found {
		t.Fatalf("receipt %v %v", found, err)
	}
	return sender, op
}

func TestAzureFeedbackAuthenticatesAndReplaysAcrossReplicas(t *testing.T) {
	w := newAzureFeedbackWorld(t)
	sender, op := w.send(t, "hop")
	var calls atomic.Int32
	apply := func(ctx context.Context, report AzureDeliveryReport) error {
		if !auth.OriginFromContext(ctx).IsInternal() || report.Verify(ctx) != nil || report.OperationID != op.ID || report.DeliveryID != "delivery-hop" || report.AccountID != w.account {
			t.Error("lost verified receipt at callback")
		}
		if !lockHeldBy(t, w.db.DB, "email-operation:"+sender.operationKey("hop")) {
			t.Error("feedback lost original send lock")
		}
		calls.Add(1)
		return nil
	}
	handlers := []http.Handler{NewAzureFeedbackHandler(func() *Integration { return w.instances[0] }, apply), NewAzureFeedbackHandler(func() *Integration { return w.instances[1] }, apply)}
	body := w.event(t, op, "Delivered")
	cases := []struct {
		name   string
		body   string
		change func(*http.Request)
		want   int
	}{
		{"missing credential", body, func(r *http.Request) { r.Header.Del("Authorization") }, 401},
		{"wrong credential", body, func(r *http.Request) { r.Header.Set("Authorization", "Bearer incorrect") }, 401},
		{"wrong organization", body, func(r *http.Request) { r.Header.Set("X-Memql-Email-Account", "another") }, 401},
		{"wrong subscription", body, func(r *http.Request) { r.Header.Set("aeg-subscription-name", "other") }, 401},
		{"wrong resource", strings.ReplaceAll(body, w.configured.Topic, w.configured.Topic+"-foreign"), nil, 400},
		{"changed mailbox", strings.ReplaceAll(body, "receiver@example.test", "foreign@example.test"), nil, 503},
		{"unknown operation", strings.ReplaceAll(body, op.ID, "00000000-0000-0000-0000-000000000000"), nil, 503},
		{"processing is not delivery", strings.ReplaceAll(body, "Delivered", "Succeeded"), nil, 400},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response := w.request(handlers[1], test.body, test.change)
			if response.Code != test.want {
				t.Fatalf("code=%d %s", response.Code, response.Body.String())
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("unverified report reached callback")
	}
	var wg sync.WaitGroup
	for _, handler := range handlers {
		wg.Add(1)
		go func(handler http.Handler) {
			defer wg.Done()
			response := w.request(handler, body, nil)
			if response.Code != 204 {
				t.Errorf("code=%d %s", response.Code, response.Body.String())
			}
		}(handler)
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("delivery counted %d times", calls.Load())
	}
	if response := w.request(handlers[1], w.event(t, op, "Bounced"), nil); response.Code != 204 {
		t.Fatal(response.Body.String())
	}
	if response := w.request(handlers[0], body, nil); response.Code != 204 {
		t.Fatal(response.Body.String())
	}
	current, _, _, err := sender.operations.read(context.Background(), sender.operationKey("hop"))
	if err != nil || current.Status != "succeeded" || current.DeliveryStatus != "bounced" || calls.Load() != 2 {
		t.Fatalf("receipt=%+v calls=%d error=%v", current, calls.Load(), err)
	}
	// A request cancellation before verification cannot write a delivery outcome.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.instances[1].azure.applyDeliveryEvent(ctx, w.configured, azureEvent{Topic: w.configured.Topic}, azureDeliveryData{MessageID: op.ID, Sender: w.connection.Config.Default, Recipient: "receiver@example.test", Status: "Suppressed", AttemptTime: w.now}, apply); err == nil {
		t.Fatal("canceled report wrote")
	}
}

func TestAzureFeedbackSubscriptionValidatesAfterCredentialIsDurable(t *testing.T) {
	w := newAzureFeedbackWorld(t)
	// A distinct pending subscription starts without any trusted validation state.
	w.configured.Status = "pending"
	w.configured.Subscription = "old"
	w.configured.PlanID = "old"
	pending := w.configured
	if err := w.instances[0].azure.store.change(context.Background(), "azure-feedback:"+w.account, &w.configured, func(bool) error { w.configured = pending; return nil }); err != nil {
		t.Fatal(err)
	}
	handler := NewAzureFeedbackHandler(func() *Integration { return w.instances[1] }, func(context.Context, AzureDeliveryReport) error {
		t.Error("validation sent a delivery report")
		return nil
	})
	var puts, gets atomic.Int32
	var bearer string
	protocol := w.instances[0].azure.protocol
	protocol.client.Transport = acsRoundTrip(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodGet {
			endpoint, _ := azureFeedbackURL()
			state := "Succeeded"
			if gets.Add(1) == 2 {
				state = "Creating"
			}
			body := fmt.Sprintf(`{"properties":{"provisioningState":%q,"destination":{"endpointType":"WebHook","properties":{"endpointBaseUrl":%q}}}}`, state, endpoint)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		if request.Method != http.MethodPut || !strings.Contains(request.URL.Path, "/providers/Microsoft.EventGrid/eventSubscriptions/") {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		puts.Add(1)
		name := request.URL.Path[strings.LastIndex(request.URL.Path, "/")+1:]
		if len(name) > 64 {
			t.Fatalf("subscription name exceeds Azure's 64-character limit: %d", len(name))
		}
		var body map[string]any
		if json.NewDecoder(request.Body).Decode(&body) != nil {
			t.Fatal("bad ARM body")
		}
		props := body["properties"].(map[string]any)
		dest := props["destination"].(map[string]any)["properties"].(map[string]any)
		mappings := dest["deliveryAttributeMappings"].([]any)
		authHeader := mappings[0].(map[string]any)["properties"].(map[string]any)
		if authHeader["isSecret"] != true {
			t.Error("Azure bearer is not secret")
		}
		bearer = authHeader["value"].(string)
		var saved azureFeedbackConnection
		if found, _, err := w.instances[1].azure.store.read(context.Background(), "azure-feedback:"+w.account, &saved); err != nil || !found || "Bearer "+saved.Secret != bearer {
			t.Fatalf("credential unavailable during handshake: %v", err)
		}
		w.configured = saved
		validation := fmt.Sprintf(`[{"id":"validate","topic":%q,"eventType":%q,"data":{"validationCode":"challenge","validationUrl":"https://attacker.invalid/do-not-follow"}}]`, saved.Topic, azureValidationEvent)
		response := w.request(handler, validation, func(r *http.Request) { r.Header.Set("aeg-event-type", "SubscriptionValidation") })
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"validationResponse":"challenge"`) {
			t.Fatalf("validation %d %s", response.Code, response.Body.String())
		}
		return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	result, err := w.instances[0].azure.ensureDeliveryFeedback(context.Background(), w.account, azurePlanID(w.connection))
	if err != nil || result["deliveryFeedback"] != "pending" {
		t.Fatalf("empty asynchronous PUT became ready: setup=%v %v", result, err)
	}
	if result, err = w.instances[0].azure.ensureDeliveryFeedback(context.Background(), w.account, azurePlanID(w.connection)); err != nil || puts.Load() != 1 || result["deliveryFeedback"] != "pending" {
		t.Fatalf("pending provisioning restarted: calls=%d result=%v error=%v", puts.Load(), result, err)
	}
	if result, err = w.instances[0].azure.ensureDeliveryFeedback(context.Background(), w.account, azurePlanID(w.connection)); err != nil || puts.Load() != 1 || result["deliveryFeedback"] != "ready" {
		t.Fatalf("idempotency calls=%d %v", puts.Load(), err)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), bearer) || strings.Contains(string(encoded), w.configured.Secret) {
		t.Fatal("public setup leaked credential")
	}
}

func TestAzureFeedbackKeepsNewestAttemptWithinAProviderVerdict(t *testing.T) {
	w := newAzureFeedbackWorld(t)
	sender, op := w.send(t, "attempt-order")
	var calls int
	handler := NewAzureFeedbackHandler(func() *Integration { return w.instances[1] }, func(ctx context.Context, report AzureDeliveryReport) error { calls++; return report.Verify(ctx) })
	first := w.now
	post := func(status string, at time.Time) {
		t.Helper()
		body := strings.ReplaceAll(w.event(t, op, status), w.now.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
		if response := w.request(handler, body, nil); response.Code != 204 {
			t.Fatalf("%d %s", response.Code, response.Body.String())
		}
	}
	post("Failed", first)
	post("Quarantined", first.Add(-time.Minute))
	post("Delivered", first.Add(time.Minute))
	post("Delivered", first.Add(2*time.Minute))
	post("Delivered", first.Add(time.Minute))
	current, _, _, err := sender.operations.read(context.Background(), sender.operationKey("attempt-order"))
	if err != nil || current.DeliveryStatus != "delivered" || !current.DeliveryAt.Equal(first.Add(2*time.Minute)) || calls != 3 {
		t.Fatalf("outcome %+v calls%d error%v", current, calls, err)
	}
	post("Bounced", first.Add(-time.Minute))
	post("Delivered", first.Add(3*time.Minute))
	current, _, _, err = sender.operations.read(context.Background(), sender.operationKey("attempt-order"))
	if err != nil || current.DeliveryStatus != "bounced" || calls != 4 {
		t.Fatalf("late delivery erased permanent bounce: %+v calls%d %v", current, calls, err)
	}
}

func TestAzureFeedbackDiscoversPreviouslySavedOrganization(t *testing.T) {
	w := newAzureFeedbackWorld(t)
	a := w.instances[0].azure
	// Simulate a connection written before scheduling metadata existed. Its
	// encrypted organization fields are the only discovery source.
	data, _ := json.Marshal(w.connection)
	sealed, _, err := secret.Encrypt(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id=?`, "v1:email:connectionState:"+emailStateID("organization:"+w.account)); err != nil {
		t.Fatal(err)
	}
	_, err = a.store.engine.Execute(emailStateContext(context.Background()), renderConfigCall("storeEmailConnectionState", map[string]string{"id": emailStateID("organization:" + w.account), "encryptedValue": sealed, "versionTime": w.now.Add(time.Second).Format(time.RFC3339Nano)}))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	a.protocol.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		endpoint, _ := azureFeedbackURL()
		body := fmt.Sprintf(`{"properties":{"provisioningState":"Succeeded","destination":{"endpointType":"WebHook","properties":{"endpointBaseUrl":%q}}}}`, endpoint)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	if err := w.instances[0].PollFeedbackConnections(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() == 0 {
		var metadata string
		readErr := w.db.QueryRowContext(context.Background(), `SELECT (payload-'encryptedValue')::text FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, "v1:email:connectionState:"+emailStateID("organization:"+w.account)).Scan(&metadata)
		t.Fatalf("old ready organization was never discovered: state=%s err=%v", metadata, readErr)
	}
	before := calls.Load()
	if err := w.instances[0].PollFeedbackConnections(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != before {
		t.Fatal("scheduled retry time was ignored")
	}
	// The scheduling write cannot resurrect a disconnected organization.
	if err := a.store.change(context.Background(), "organization:"+w.account, &w.connection, func(bool) error { w.connection.Status = "disconnected"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := w.instances[0].PollFeedbackConnections(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != before {
		t.Fatal("disconnected organization was provisioned")
	}
}

func TestAzureMaintenanceKeepsReceiptsAndFeedbackMovingWhileInstallationIsPending(t *testing.T) {
	w := newAzureFeedbackWorld(t)
	sender, op := w.send(t, "pending-installation")
	stored, prior, _, err := sender.operations.read(context.Background(), sender.operationKey("pending-installation"))
	if err != nil {
		t.Fatal(err)
	}
	stored.NextPollAt = w.now.Add(-time.Minute)
	if _, err = sender.operations.write(context.Background(), sender.operationKey("pending-installation"), stored, prior); err != nil {
		t.Fatal(err)
	}
	if _, err := w.instances[0].azure.store.engine.Execute(emailStateContext(context.Background()), renderConfigCall("createClientAccount", map[string]string{"accountId": w.account, "name": "Synthetic feedback organization"})); err != nil {
		t.Fatal(err)
	}
	w.now = time.Now().UTC().Add(time.Second)
	a := w.instances[1].azure
	a.installation = installationFixture()
	var installationCalls, feedbackCalls, receiptCalls atomic.Int32
	a.store.engine = pendingInstallationTestStore{ConfigWriter: a.store.engine, calls: &installationCalls}
	a.protocol.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		feedbackCalls.Add(1)
		endpoint, _ := azureFeedbackURL()
		body := fmt.Sprintf(`{"properties":{"provisioningState":"Succeeded","destination":{"endpointType":"WebHook","properties":{"endpointBaseUrl":%q}}}}`, endpoint)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	a.operationClient = &http.Client{Transport: acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		receiptCalls.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"id":%q,"status":"Succeeded"}`, op.ID)))}, nil
	})}
	original := w.instances[1].sender
	poller := NewOperationPoller(func() *Integration { return w.instances[1] }, nil)
	poller.interval = 10 * time.Millisecond
	poller.Start(context.Background())
	t.Cleanup(func() { poller.Stop(context.Background()) })
	deadline := time.Now().Add(10 * time.Second)
	for {
		current, _, _, err := sender.operations.read(context.Background(), sender.operationKey("pending-installation"))
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			pollErr := w.instances[1].PollOperations(context.Background())
			t.Fatalf("pending installation starved maintenance: feedback=%d receipts=%d status=%s detail=%s error=%v", feedbackCalls.Load(), receiptCalls.Load(), current.Status, current.Detail, pollErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	poller.Stop(context.Background())
	if installationCalls.Load() == 0 || feedbackCalls.Load() == 0 || receiptCalls.Load() == 0 || w.instances[1].sender != original {
		t.Fatalf("maintenance skipped a phase or replaced the existing transport: feedback=%d receipts=%d", feedbackCalls.Load(), receiptCalls.Load())
	}
}

// Refuse the first installed workflow effect while leaving transport maintenance
// on the same engine available. This makes starvation observable in Start.
type pendingInstallationTestStore struct {
	ConfigWriter
	calls *atomic.Int32
}

func (s pendingInstallationTestStore) Execute(ctx context.Context, query string) (any, error) {
	if strings.Contains(query, "query emailConnectionStateById(") && strings.Contains(query, emailStateID(azureClusterKey)) && s.calls.Add(1) == 1 {
		return nil, fmt.Errorf("installation connection is temporarily unavailable")
	}
	return s.ConfigWriter.Execute(ctx, query)
}
