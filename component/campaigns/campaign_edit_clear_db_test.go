package campaigns

import (
	"context"
	"database/sql"
	"fmt"
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

// TestEditingACampaignStoresAFieldSentEmptyAsCleared pins what the OS Email
// app's Edit page relies on (memql#6033). updateCampaign read-merges: a field
// it is not sent keeps what is stored, so the page sends only what changed. A
// field somebody EMPTIED is a change, and the page sends it as "" -- no
// reply-to (replies go to the sender), no saved time (the draft waits for Send
// now). If the engine kept the old value instead, the page would say the
// campaign was saved while it still carried both.
//
// The write is the call the OS makes, with name, audience and template always
// present, and the campaign is read back through campaignById, the read the
// sending worker uses: what is checked is what a send would see.
func TestEditingACampaignStoresAFieldSentEmptyAsCleared(t *testing.T) {
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	ping, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		dbtest.Unreachable(t, "clearing a campaign's field", dbtest.DSN(), err)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	eng, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	eng.Logger = quietLogger()
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	worker := newTestWorker(t, deliveryDBEngine{eng}, &recordingSender{})
	worker.templateGate = NewWorker(deliveryDBEngine{eng}, nil, nil, quietLogger(), func() *sql.DB { return db.DB }).templateGate

	// Every row this test writes carries the suffix in its id, its payload or
	// its author, and every one of them goes when the test ends -- failed or not.
	suffix := fmt.Sprintf("clear-field-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		like := "%" + suffix + "%"
		if _, err := db.NewDelete().
			Model((*memorynodes.MemoryNode)(nil)).
			Where(`"id" LIKE ? OR "createdBy" LIKE ? OR "payload"::text LIKE ?`, like, like, like).
			Exec(context.Background()); err != nil {
			t.Errorf("removing the test's rows: %v", err)
		}
	})
	user, org, audience, template, campaign := suffix+"-user", suffix+"-org", suffix+"-aud", suffix+"-tpl", suffix+"-campaign"
	owner := auth.ContextWithToken(auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: user, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: user})
	system := auth.ContextWithInternalOrigin(worker.systemActorContext(ctx))
	execute := func(ctx context.Context, q string) {
		t.Helper()
		if _, err := eng.Execute(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Editor","role":"owner","active":true})`, langparser.QuoteString(user), langparser.QuoteString(user+"@example.test")))
	execute(owner, call("mutation", "createClientAccount", arg{"accountId", org}, arg{"name", "Editing client"}))
	execute(owner, call("mutation", "createAudience", arg{"audienceId", audience}, arg{"accountId", org}, arg{"name", "Newsletter"}))
	if _, err := worker.handleSaveTemplate(owner, map[string]any{"templateId": template, "accountId": org, "name": "Digest", "content": map[string]any{"subject": "Digest", "textBody": "This week"}}, 0); err != nil {
		t.Fatal(err)
	}
	execute(owner, call("mutation", "createCampaign",
		arg{"campaignId", campaign}, arg{"accountId", org}, arg{"name", "Weekly digest"}, arg{"audienceId", audience}, arg{"templateId", template},
		arg{"replyTo", "help@client.test"}, arg{"scheduledAt", "2031-10-14T16:00:00Z"}, arg{"trackClicks", false}))

	read := func() map[string]any {
		t.Helper()
		rows, err := worker.store.rows(memql.ContextWithFreshRead(owner), call("query", "campaignById", arg{"campaignId", campaign}))
		if err != nil || len(rows) != 1 {
			t.Fatalf("reading the campaign back: %d rows, %v", len(rows), err)
		}
		return rows[0]
	}
	before := read()
	if str(before, "replyTo") != "help@client.test" || !parseTime(str(before, "scheduledAt")).Equal(time.Date(2031, 10, 14, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("the campaign was not stored with a reply-to and a time to clear: %v", before)
	}

	// The Edit page's save after somebody emptied the reply-to and took the
	// saved time away -- and touched nothing else.
	execute(owner, call("mutation", "updateCampaign",
		arg{"campaignId", campaign}, arg{"name", "Weekly digest"}, arg{"audienceId", audience}, arg{"templateId", template},
		arg{"replyTo", ""}, arg{"scheduledAt", ""}))

	after := read()
	for _, field := range []string{"replyTo", "scheduledAt"} {
		if v, present := after[field]; present && v != nil && v != "" {
			t.Errorf("%s sent as \"\" is still stored as %#v", field, v)
		}
	}
	// Read-merge, the other half of the contract: a field the save left out keeps what is stored.
	if v, present := after["trackClicks"]; !present || v != false {
		t.Errorf("trackClicks was not sent and changed from false to %#v", v)
	}
	// What the sending worker reads: nobody to reply to but the sender, and no time to send at.
	stored, found, err := worker.store.CampaignByID(memql.ContextWithFreshRead(owner), campaign)
	if err != nil || !found {
		t.Fatalf("the worker's read of the campaign: found=%t, %v", found, err)
	}
	if stored.ReplyTo != "" || !stored.ScheduledAt.IsZero() {
		t.Errorf("the worker still reads reply-to %q and a time of %v", stored.ReplyTo, stored.ScheduledAt)
	}
}
