package email

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
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

// The domain page's acts, end to end over the real store and sender records:
// each Add sender registers the address with Azure and records its sender in
// one act without moving the default, the page reads where each sender's
// replies go, and Make default moves only the default without asking Azure.
func TestTheDomainsSenderActsKeepTheDefaultAndRecordEachSender(t *testing.T) {
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ac", 32))
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "Azure senders", dbtest.DSN(), err)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	e, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("senders-%d", time.Now().UnixNano())
	account := suffix + "-client"
	owner := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: suffix + "-owner", Role: auth.RoleOwner}), &auth.TokenInfo{Subject: suffix + "-owner"})
	if _, err = e.Execute(owner, renderConfigCall("createClientAccount", map[string]string{"accountId": account, "name": account})); err != nil {
		t.Fatal(err)
	}
	plan := azurePlanFixture()
	plan.ClusterID = "cluster-" + suffix
	registered := map[string]string{}
	protocol := azureDomainFake(t, account, plan.ClusterID, registered)
	azureCalls := 0
	fake := protocol.client.Transport
	protocol.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) { azureCalls++; return fake.RoundTrip(r) })
	setup := &azureSetup{store: &connectionStore{engine: installationTestStore{ConfigWriter: NewConfigWriter(e), prefix: suffix}, gateDB: replicaGatePool(t)}, protocol: protocol}
	var connection azureConnection
	if err = setup.store.change(owner, "organization:"+account, &connection, func(bool) error {
		connection = azureConnection{Status: "verified", AccountID: account, Plan: plan}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	session := azureSession{AccountID: account, ClusterID: plan.ClusterID, AccessToken: "token"}
	add := func(username, displayName, replyTo string) map[string]any {
		t.Helper()
		out, err := setup.resourceAction(owner, session, "sender", map[string]any{"username": username, "displayName": displayName, "replyTo": replyTo, "planId": azurePlanID(connection)})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	if out := add("news", "Client", "help@client.example"); out["sender"] != "news@client.example" || out["status"] != "ready" {
		t.Fatalf("the first sender is not the default: %v", out)
	}
	add("hello", "Client Hello", "replies@client.example")
	out := add("billing", "Client Billing", "")
	if out["sender"] != "news@client.example" || out["replyTo"] != "help@client.example" {
		t.Fatalf("adding a sender moved the default: %v, replies to %v", out["sender"], out["replyTo"])
	}
	if registered["hello"] != "Client Hello" || registered["billing"] != "Client Billing" || len(out["senders"].(map[string]string)) != 3 {
		t.Fatalf("Azure has %v; the domain lists %v", registered, out["senders"])
	}
	// Each sender's replies go to its own Reply-To, else to the connection's.
	want := map[string]string{"news@client.example": "help@client.example", "hello@client.example": "replies@client.example", "billing@client.example": "help@client.example"}
	if fmt.Sprint(out["replies"]) != fmt.Sprint(want) {
		t.Fatalf("replies: %v", out["replies"])
	}
	// Every sender the domain added is an ordinary sender record, the default or not.
	result, err := e.Execute(memql.ContextWithFreshRead(owner), strings.Replace(renderConfigCall("senderIdentitiesForAccount", map[string]string{"accountId": account}), "mutation ", "query ", 1))
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]string{}
	for _, row := range memql.MaterializeRows(result) {
		records[argString(row, "address")] = argString(row, "fromName") + " / " + argString(row, "replyTo")
	}
	if records["news@client.example"] != "Client / help@client.example" || records["hello@client.example"] != "Client Hello / replies@client.example" || records["billing@client.example"] != "Client Billing / " {
		t.Fatalf("sender records: %v", records)
	}

	// Make default asks Azure nothing and changes only the default.
	before := azureCalls
	moved, err := setup.connectionAction(owner, account, "makeDefault", map[string]any{"sender": "hello@client.example"})
	if err != nil {
		t.Fatal(err)
	}
	if azureCalls != before {
		t.Fatal("make default called Azure")
	}
	if moved["sender"] != "hello@client.example" || moved["replyTo"] != "help@client.example" || len(moved["senders"].(map[string]string)) != 3 {
		t.Fatalf("make default: %v", moved)
	}
	status, err := setup.connectionAction(owner, account, "status", nil)
	if err != nil || status["sender"] != "hello@client.example" || status["replies"] == nil {
		t.Fatalf("status after make default: %v %v", status, err)
	}
	if _, err := setup.connectionAction(owner, account, "makeDefault", map[string]any{"sender": "someone@client.example"}); err == nil {
		t.Fatal("made a sender Azure does not have the default")
	}
	// A later sender still keeps the default where Make default put it.
	if out := add("orders", "Client Orders", ""); out["sender"] != "hello@client.example" {
		t.Fatalf("adding a sender after make default moved it to %v", out["sender"])
	}
}
