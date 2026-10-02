package email

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
)

func TestACSSendReceiptReconcilesOnAnotherReplicaWithoutReposting(t *testing.T) {
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ac", 32))
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	defer db.Close()
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "ACS receipts", dbtest.DSN(), err)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	account := fmt.Sprintf("acs-receipt-%d", now.UnixNano())
	owner := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: account + "-owner", Role: auth.RoleOwner}), &auth.TokenInfo{Subject: account + "-owner"})
	instances := make([]*Integration, 2)
	engines := make([]*memql.MemQLEngine, 2)
	var posts, gets atomic.Int32
	transport := acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			return nil, fmt.Errorf("response lost after acceptance")
		}
		count := gets.Add(1)
		status := "Running"
		if count > 1 {
			status = "Succeeded"
		}
		op := strings.TrimPrefix(r.URL.Path, "/emails/operations/")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"id":%q,"status":%q}`, op, status)))}, nil
	})
	for n := range instances {
		e, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		if err = e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		engines[n] = e
		a := &azureSetup{store: &connectionStore{engine: NewConfigWriter(e), database: func() *sql.DB { return db.DB }}, protocol: newAzureProtocol("12345678-1234-1234-1234-123456789012"), now: func() time.Time { return now }, operationClient: &http.Client{Transport: transport}, canManage: func(ctx context.Context, id string) bool {
			return e.OrganizationCapable(ctx, id, auth.VerbUpdate, auth.ResourceData)
		}}
		instances[n] = NewIntegration(NewLogSender(nil), nil)
		instances[n].azure = a
	}
	if _, err := engines[0].Execute(owner, renderConfigCall("createClientAccount", map[string]string{"accountId": account, "name": "Receipt client"})); err != nil {
		t.Fatal(err)
	}
	cfg := acsFixture(t).cfg
	cfg.AccountID = account
	var cluster azureClusterConnection
	if err := instances[0].azure.store.change(owner, azureClusterKey, &cluster, func(bool) error {
		cluster = azureClusterConnection{ID: account, Status: "connected"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var connection azureConnection
	if err := instances[0].azure.store.change(owner, "organization:"+account, &connection, func(bool) error {
		connection = azureConnection{Status: "ready", AccountID: account, Config: cfg, Plan: azurePlan{ClusterID: cluster.ID}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	msg := Message{IntentID: "campaign:saved:recipient:one", To: "private-recipient@example.test", Subject: "Private subject", TextBody: "private body"}
	senders := make([]*ACSSender, 2)
	for n, i := range instances {
		sender, err := i.azure.sender(owner, account)
		if err != nil {
			t.Fatal(err)
		}
		senders[n] = sender.(*ACSSender)
		senders[n].client = &http.Client{Transport: transport}
		senders[n].now = i.azure.clock
	}
	if err := senders[0].Send(owner, msg, SendAs{AccountID: account}); !IsPermanent(err) {
		t.Fatal(err)
	}
	if err := senders[1].Send(owner, msg, SendAs{AccountID: account}); !IsPermanent(err) {
		t.Fatal(err)
	}
	if posts.Load() != 1 {
		t.Fatal("replica retry resubmitted message")
	}
	now = now.Add(16 * time.Second)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, i := range instances {
		wg.Add(1)
		go func(i *Integration) {
			defer wg.Done()
			<-start
			if err := i.PollOperations(owner); err != nil {
				t.Error(err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if gets.Load() != 1 {
		t.Fatalf("concurrent polling calls=%d", gets.Load())
	}
	// Reconciled acceptance may repair the delivery ledger without another POST.
	if err := senders[1].Send(owner, msg, SendAs{AccountID: account}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(16 * time.Second)
	if err := instances[1].PollOperations(owner); err != nil {
		t.Fatal(err)
	}
	status, err := instances[0].handleAzureSetup(owner, map[string]any{"accountId": account, "action": "operations"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if len(status) != 1 || json.Unmarshal(status[0].Payload, &payload) != nil {
		t.Fatal("missing operation status")
	}
	ops := payload["operations"].([]any)
	if len(ops) != 1 || ops[0].(map[string]any)["status"] != "succeeded" || strings.Contains(string(status[0].Payload), msg.To) || strings.Contains(string(status[0].Payload), "delivered") {
		t.Fatalf("unsafe status: %s", status[0].Payload)
	}
	if posts.Load() != 1 || gets.Load() != 2 {
		t.Fatalf("posts=%d gets=%d", posts.Load(), gets.Load())
	}
	if _, err = engines[0].Execute(owner, `insert("v1:email:sendOperation", id="forged", payload={"accountId":"self","status":"succeeded","nextPollAt":"2026-10-01T00:00:00Z","encryptedValue":"forged"})`); err == nil {
		t.Fatal("raw owner forged provider receipt")
	}
	outsider := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: account + "-other", Role: auth.RoleDeveloper})
	if _, err = instances[0].handleAzureSetup(outsider, map[string]any{"accountId": account, "action": "operations"}, 0); err == nil {
		t.Fatal("another actor read client send receipts")
	}
	changed := msg
	changed.Subject = "Changed"
	if err = senders[1].Send(owner, changed, SendAs{AccountID: account}); !IsPermanent(err) {
		t.Fatal("reused intent accepted different content")
	}
}
