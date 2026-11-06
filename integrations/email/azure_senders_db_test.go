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
	// The test database is shared: a ready organization left behind joins every
	// later delivery-feedback scan (which reads 50 at a time), so it goes when done.
	t.Cleanup(func() {
		if _, err := setup.connectionAction(owner, account, "disconnect", nil); err != nil {
			t.Errorf("could not disconnect the test organization: %v", err)
		}
	})
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

	// Make default asks Azure nothing, and moves the default with that sender's
	// own Reply-To.
	before := azureCalls
	moved, err := setup.connectionAction(owner, account, "makeDefault", map[string]any{"sender": "hello@client.example"})
	if err != nil {
		t.Fatal(err)
	}
	if azureCalls != before {
		t.Fatal("make default called Azure")
	}
	if moved["sender"] != "hello@client.example" || moved["replyTo"] != "replies@client.example" || len(moved["senders"].(map[string]string)) != 3 {
		t.Fatalf("make default: %v", moved)
	}
	status, err := setup.connectionAction(owner, account, "status", nil)
	if err != nil || status["sender"] != "hello@client.example" || status["replyTo"] != "replies@client.example" || status["replies"] == nil {
		t.Fatalf("status after make default: %v %v", status, err)
	}
	if _, err := setup.connectionAction(owner, account, "makeDefault", map[string]any{"sender": "someone@client.example"}); err == nil {
		t.Fatal("made a sender Azure does not have the default")
	}

	// A sender recorded by hand before Azure had it is TAKEN OVER when it is
	// added on the domain: one record, under its own id, with the name Azure
	// holds, and what the domain does not set (its notes) kept.
	record := func(id, address, fromName, replyTo string) {
		t.Helper()
		if _, err := e.Execute(owner, renderConfigCall("createSenderIdentity", map[string]string{"senderIdentityId": id, "accountId": account, "address": address, "fromName": fromName, "replyTo": replyTo, "notes": "recorded by hand"})); err != nil {
			t.Fatal(err)
		}
	}
	recordsFor := func(address string) []map[string]any {
		t.Helper()
		rows, err := setup.senderRecords(owner, account)
		if err != nil {
			t.Fatal(err)
		}
		var found []map[string]any
		for _, row := range rows {
			if strings.EqualFold(argString(row, "address"), address) {
				found = append(found, row)
			}
		}
		return found
	}
	handID := suffix + "-orders"
	record(handID, "Orders@Client.example", "Orders Team", "desk@client.example")
	// Someone also adds support@ in Azure directly; the next add lists it.
	registered["support"] = "Client Support"
	if out := add("orders", "Client Orders", "desk@client.example"); out["sender"] != "hello@client.example" {
		t.Fatalf("adding a sender after make default moved it to %v", out["sender"])
	}
	orders := recordsFor("orders@client.example")
	if len(orders) != 1 || memql.BareShortId(argString(orders[0], "id")) != handID || argString(orders[0], "fromName") != "Client Orders" || argString(orders[0], "address") != "orders@client.example" || argString(orders[0], "replyTo") != "desk@client.example" || argString(orders[0], "notes") != "recorded by hand" {
		t.Fatalf("the hand record was not taken over: %v", orders)
	}

	// A record whose name differs from the one Azure holds stays refused --
	// the check every send makes -- until it carries Azure's name.
	record(suffix+"-support", "support@client.example", "Support", "")
	if _, err := setup.resourceAction(owner, session, "sender", map[string]any{"username": "support", "displayName": "Support", "planId": azurePlanID(connection)}); err == nil {
		t.Fatal("registered a display name Azure does not hold")
	}
	var current azureConnection
	if _, _, err := setup.store.read(owner, "organization:"+account, &current); err != nil {
		t.Fatal(err)
	}
	sender, err := NewACSSender(current.Config)
	if err != nil {
		t.Fatal(err)
	}
	refusal := func() string {
		support := recordsFor("support@client.example")
		if len(support) != 1 {
			t.Fatalf("support records: %v", support)
		}
		return SenderRefusal(sender.CheckSender(owner, SendAs{AccountID: account, Address: "support@client.example", FromName: argString(support[0], "fromName")}))
	}
	if got := refusal(); got != "renamed" {
		t.Fatalf("a name Azure does not hold: %q", got)
	}
	add("support", "Client Support", "")
	if _, _, err := setup.store.read(owner, "organization:"+account, &current); err != nil {
		t.Fatal(err)
	}
	if sender, err = NewACSSender(current.Config); err != nil {
		t.Fatal(err)
	}
	if got := refusal(); got != "" || len(recordsFor("support@client.example")) != 1 {
		t.Fatalf("still refused after taking Azure's name: %q", got)
	}

	// The summary says what is recorded at each address, so the page that adds a
	// sender starts from it; a takeover then writes what that page was given,
	// a Reply-To left empty included.
	record(suffix+"-returns", "returns@client.example", "Returns Team", "back@client.example")
	status, err = setup.connectionAction(owner, account, "status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := status["recorded"].(map[string]map[string]string)["returns@client.example"]; got["fromName"] != "Returns Team" || got["replyTo"] != "back@client.example" {
		t.Fatalf("recorded: %v", status["recorded"])
	}
	add("returns", "Client Returns", "")
	if returns := recordsFor("returns@client.example"); len(returns) != 1 || argString(returns[0], "fromName") != "Client Returns" || argString(returns[0], "replyTo") != "" {
		t.Fatalf("the takeover kept a Reply-To it was not given: %v", returns)
	}

	// Make default keeps the bare mailbox of a free-text Reply-To...
	update := func(id, address, fromName, replyTo string) {
		t.Helper()
		if _, err := e.Execute(owner, renderConfigCall("updateSenderIdentity", map[string]string{"senderIdentityId": id, "accountId": account, "address": address, "fromName": fromName, "replyTo": replyTo})); err != nil {
			t.Fatal(err)
		}
	}
	update(azureSenderIdentityID(account, "billing@client.example"), "billing@client.example", "Client Billing", "Billing Desk <billing-desk@client.example>")
	moved, err = setup.connectionAction(owner, account, "makeDefault", map[string]any{"sender": "billing@client.example"})
	if err != nil || moved["sender"] != "billing@client.example" || moved["replyTo"] != "billing-desk@client.example" {
		t.Fatalf("make default with a display-name Reply-To: %v %v", moved, err)
	}
	if _, _, err := setup.store.read(owner, "organization:"+account, &current); err != nil {
		t.Fatal(err)
	}
	if _, err := NewACSSender(current.Config); err != nil || current.Config.ReplyTo != "billing-desk@client.example" {
		t.Fatalf("make default left a connection that cannot send: %v %q", err, current.Config.ReplyTo)
	}
	// ...and refuses one that names no mailbox, writing nothing.
	update(azureSenderIdentityID(account, "hello@client.example"), "hello@client.example", "Client Hello", "the help desk")
	if _, err := setup.connectionAction(owner, account, "makeDefault", map[string]any{"sender": "hello@client.example"}); err == nil || !strings.Contains(err.Error(), "the help desk") {
		t.Fatalf("made a sender with an unusable Reply-To the default: %v", err)
	}
	var after azureConnection
	if _, _, err := setup.store.read(owner, "organization:"+account, &after); err != nil {
		t.Fatal(err)
	}
	if after.Config.Default != "billing@client.example" || after.Config.ReplyTo != "billing-desk@client.example" || after.ReplyTo != "billing-desk@client.example" {
		t.Fatalf("a refused make default changed the connection: %q, %q", after.Config.Default, after.Config.ReplyTo)
	}
}
