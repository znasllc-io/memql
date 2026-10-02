package campaigns

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

func TestAudienceRunsUseNormalWorkerAcrossReplicas(t *testing.T) {
	if reachable, err := dbtest.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	} else if !reachable {
		dbtest.Unreachable(t, "campaign test audience", dbtest.DSN(), nil)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	engines := make([]*memql.MemQLEngine, 2)
	workers := make([]*Worker, 2)
	for n := range engines {
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
		t.Cleanup(func() { _ = db.Close() })
		e, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		e.Logger = quietLogger()
		if err := e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		engines[n] = e
		workers[n] = newTestWorker(t, deliveryDBEngine{e}, sender)
		wired := NewWorker(deliveryDBEngine{e}, nil, nil, quietLogger(), func() *sql.DB { return db.DB })
		workers[n].testGate, workers[n].templateGate, workers[n].seriesGate = wired.testGate, wired.templateGate, wired.seriesGate
		if err := e.RegisterIntegration(workers[n]); err != nil {
			t.Fatal(err)
		}
	}
	suffix := fmt.Sprintf("test-run-%d", time.Now().UnixNano())
	user, org, live, internal, template, campaign, identity := suffix+"-user", suffix+"-org", suffix+"-live", suffix+"-internal", suffix+"-template", suffix+"-campaign", suffix+"-sender"
	owner := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: user})
	system := auth.ContextWithInternalOrigin(workers[0].systemActorContext(context.Background()))
	execute := func(ctx context.Context, q string) {
		t.Helper()
		if _, err := engines[0].Execute(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Operator","role":"owner","active":true})`, langparser.QuoteString(user), langparser.QuoteString(user+"@example.test")))
	execute(owner, call("mutation", "createClientAccount", arg{"accountId", org}, arg{"name", "Testing client"}))
	for _, audience := range []string{live, internal} {
		execute(owner, call("mutation", "createAudience", arg{"audienceId", audience}, arg{"accountId", org}, arg{"name", audience}))
	}
	execute(owner, call("mutation", "createSenderIdentity", arg{"senderIdentityId", identity}, arg{"accountId", org}, arg{"address", "news@client.test"}, arg{"fromName", "Client"}))
	templateArgs := map[string]any{"templateId": template, "accountId": org, "name": "Test mail", "content": map[string]any{"subject": "Hello {{name}}", "textBody": "For {{email}}", "htmlBody": "<p>Hello {{name}}</p>"}}
	nodes, err := workers[0].handleSaveTemplate(owner, templateArgs, 0)
	if err != nil {
		t.Fatal(err)
	}
	templateArgs["expectedRevision"], templateArgs["action"] = decodeResult(t, nodes)["revision"], "publish"
	if _, err = workers[0].handleSaveTemplate(owner, templateArgs, 0); err != nil {
		t.Fatal(err)
	}
	execute(owner, call("mutation", "createCampaign", arg{"campaignId", campaign}, arg{"accountId", org}, arg{"name", "Live campaign"}, arg{"audienceId", live}, arg{"templateId", template}, arg{"senderIdentityId", identity}))
	for i, address := range []string{"one@example.test", "two@example.test", "optedout@example.test"} {
		execute(owner, call("mutation", "addRecipient", arg{"recipientId", fmt.Sprintf("%s-r%d", suffix, i)}, arg{"audienceId", internal}, arg{"accountId", org}, arg{"email", address}, arg{"displayName", fmt.Sprintf("Tester %d", i)}, arg{"source", "manual"}))
	}
	execute(owner, call("mutation", "addRecipient", arg{"recipientId", suffix + "-customer"}, arg{"audienceId", live}, arg{"accountId", org}, arg{"email", "customer@example.test"}, arg{"displayName", "Customer"}, arg{"source", "manual"}))
	if err := workers[0].store.RecordSuppression(system, EmailDigest("optedout@example.test"), "manual", "example.test", "", "test"); err != nil {
		t.Fatal(err)
	}
	configure := map[string]any{"accountId": org, "audienceId": internal}
	if _, err := workers[0].handleConfigureTestAudience(owner, configure, 0); err != nil {
		t.Fatal(err)
	}

	// Read-only and cross-organization configuration may never authorize a send.
	reader := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user + "-reader", Role: auth.RoleReader}), &auth.TokenInfo{Subject: user + "-reader"})
	if _, err := workers[1].handleConfigureTestAudience(reader, configure, 0); err == nil {
		t.Fatal("reader configured a testing audience")
	}
	execute(owner, call("mutation", "createClientAccount", arg{"accountId", org + "-other"}, arg{"name", "Other organization"}))
	execute(owner, call("mutation", "createAudience", arg{"audienceId", internal + "-other"}, arg{"accountId", org + "-other"}, arg{"name", "Other reviewers"}))
	if _, err := workers[1].handleConfigureTestAudience(owner, map[string]any{"accountId": org, "audienceId": internal + "-other"}, 0); err == nil {
		t.Fatal("testing audience crossed organizations")
	}
	if _, err := workers[1].handleConfigureTestAudience(owner, map[string]any{"accountId": org, "audienceId": live, "expectedRevision": "2020-01-01T00:00:00Z"}, 0); err == nil {
		t.Fatal("stale configuration overwrote settings")
	}
	args := map[string]any{"campaignId": campaign, "requestId": "request-one", "audienceId": internal}
	// Requests arrive at separate engines; PostgreSQL serialization and fresh
	// reads must converge on one run with no message sent inside the handler.
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	ids := make(chan string, 2)
	for _, w := range workers {
		wg.Add(1)
		go func(w *Worker) {
			defer wg.Done()
			n, err := w.handleTestAudienceSend(owner, args, 0)
			if err != nil {
				errors <- err
				return
			}
			ids <- str(decodeResult(t, n), "testRunId")
		}(w)
	}
	wg.Wait()
	close(errors)
	close(ids)
	for err := range errors {
		t.Fatal(err)
	}
	runID := ""
	for got := range ids {
		if runID != "" && got != runID {
			t.Fatal("duplicate run")
		}
		runID = got
	}
	if sender.count() != 0 {
		t.Fatal("handler bypassed the worker")
	}
	job, found, err := workers[1].store.JobByID(system, runID)
	if err != nil || !found || job.TemplateSnapshot == nil {
		t.Fatalf("queued snapshot: %+v %v", job, err)
	}
	// Test instructions and settings cannot be rewritten through ordinary or raw writes.
	for _, q := range []string{
		call("mutation", "updateCampaign", arg{"campaignId", runID}, arg{"name", "Retarget"}, arg{"audienceId", live}, arg{"templateId", template}),
		fmt.Sprintf(`insert("v1:campaigns:campaign", id=%s, payload={"audienceId":%s})`, langparser.QuoteString(runID), langparser.QuoteString(live)),
		call("mutation", "saveCampaignTestSettings", arg{"settingsId", testSettingsID(org)}, arg{"accountId", org}, arg{"audienceId", live}, arg{"versionTime", time.Now().Format(time.RFC3339Nano)}),
	} {
		if _, err := engines[0].Execute(owner, q); err == nil || (!strings.Contains(err.Error(), "governed actions") && !strings.Contains(err.Error(), "server")) {
			t.Fatalf("unexpected write result: %s: %v", q, err)
		}
	}

	failedOnce := false
	sender.fail = func(int) error {
		if !failedOnce {
			failedOnce = true
			return fmt.Errorf("temporary provider problem")
		}
		return nil
	}
	// Drain on the other replica, which holds none of the request's local state.
	for i := 0; i < 4; i++ {
		workers[1].now = func() time.Time { return time.Now().Add(time.Duration(i) * time.Hour) }
		j, _, _ := workers[1].store.JobByID(system, runID)
		workers[1].processJob(context.Background(), system, j)
	}
	if sender.count() != 2 {
		t.Fatalf("normal worker sent %d messages, want two", sender.count())
	}
	for _, to := range sender.recipients() {
		if to == "customer@example.test" || to == "optedout@example.test" {
			t.Fatalf("unexpected recipient %s", to)
		}
	}
	for _, msg := range sender.sent {
		if strings.Contains(msg.Headers[headerListUnsubscribe], inertUnsubscribeToken) || !strings.Contains(msg.Headers[headerListUnsubscribe], "https://example.test") {
			t.Fatalf("did not use real unsubscribe path: %+v", msg.Headers)
		}
		if strings.Contains(msg.TextBody, "customer@example.test") {
			t.Fatal("borrowed live audience data")
		}
	}
	run, _, _ := workers[1].store.CampaignByID(memql.ContextWithFreshRead(owner), runID)
	if run.Status != "sent" {
		t.Fatalf("test status %q", run.Status)
	}
	source, _, _ := workers[1].store.CampaignByID(memql.ContextWithFreshRead(owner), campaign)
	if source.Status != "draft" || source.AudienceID != live {
		t.Fatalf("live campaign changed: %+v", source)
	}
	if _, err := workers[0].handleTestAudienceSend(owner, args, 0); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 2 {
		t.Fatal("replay resent a completed run")
	}
	workers[1].now = time.Now
	// Source state never prevents testing and a saved series never advances.
	when := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	if _, err := workers[0].handleConfigureSeries(owner, map[string]any{"campaignId": campaign, "action": "save", "intervalWeeks": 2, "timeZone": "UTC", "firstSendAt": when.Format(time.RFC3339)}, 0); err != nil {
		t.Fatal(err)
	}
	before, _ := workers[0].seriesForCampaign(memql.ContextWithFreshRead(owner), campaign)
	for _, state := range []string{"sending", "paused", "sent", "scheduled", "cancelled"} {
		execute(auth.ContextWithInternalOrigin(owner), fmt.Sprintf(`insert("v1:campaigns:campaign", id=%s, payload={"status":%s})`, langparser.QuoteString(campaign), langparser.QuoteString(state)))
		args["requestId"] = "request-" + state
		if _, err := workers[1].handleTestAudienceSend(owner, args, 0); err != nil {
			t.Fatalf("%s: %v", state, err)
		}
	}
	after, _ := workers[1].seriesForCampaign(memql.ContextWithFreshRead(owner), campaign)
	if str(before, "nextAt") != str(after, "nextAt") || str(before, "createdAt") != str(after, "createdAt") {
		t.Fatal("test advanced series")
	}
	args["audienceId"] = live
	args["requestId"] = "wrong-audience"
	if _, err := workers[0].handleTestAudienceSend(owner, args, 0); err == nil {
		t.Fatal("allowed unconfigured live audience")
	}
}
