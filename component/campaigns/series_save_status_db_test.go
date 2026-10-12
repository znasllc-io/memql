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

// TestSavingASeriesNeverChangesWhetherItRuns pins memql#6033's ruling:
// saving a series' configuration says WHAT it repeats and how often, never
// WHETHER it runs. The OS saves the series from a campaign's Edit page, and
// Edit is offered for a paused and a stopped series -- so a save that wrote
// "active" restarted, without a question, a series somebody had paused, or one
// the scheduler had stopped for a reason nobody had fixed. A new series starts
// (that is Start repeating); an existing one keeps its status, a stopped one
// its reason too; an unchanged save writes nothing; Resume is the only way back.
func TestSavingASeriesNeverChangesWhetherItRuns(t *testing.T) {
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	ping, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		dbtest.Unreachable(t, "saving a recurring campaign", dbtest.DSN(), err)
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
	w := newTestWorker(t, deliveryDBEngine{eng}, &recordingSender{})
	wired := NewWorker(deliveryDBEngine{eng}, nil, nil, quietLogger(), func() *sql.DB { return db.DB })
	w.seriesGate, w.templateGate = wired.seriesGate, wired.templateGate
	now := time.Date(2028, 1, 1, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }

	// Every row this test writes carries the suffix in its id, its payload or
	// its author, and every one of them goes when the test ends -- failed or not.
	suffix := fmt.Sprintf("series-status-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		like := "%" + suffix + "%"
		if _, err := db.NewDelete().
			Model((*memorynodes.MemoryNode)(nil)).
			Where(`"id" LIKE ? OR "createdBy" LIKE ? OR "payload"::text LIKE ?`, like, like, like).
			Exec(context.Background()); err != nil {
			t.Errorf("removing the test's rows: %v", err)
		}
	})
	user, org, audience, template, identity, campaign := suffix+"-user", suffix+"-org", suffix+"-aud", suffix+"-tpl", suffix+"-sender", suffix+"-campaign"
	owner := auth.ContextWithToken(auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: user, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: user})
	system := auth.ContextWithInternalOrigin(w.systemActorContext(ctx))
	execute := func(ctx context.Context, q string) {
		t.Helper()
		if _, err := eng.Execute(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Editor","role":"owner","active":true})`, langparser.QuoteString(user), langparser.QuoteString(user+"@example.test")))
	execute(owner, call("mutation", "createClientAccount", arg{"accountId", org}, arg{"name", "Repeating client"}))
	execute(owner, call("mutation", "createAudience", arg{"audienceId", audience}, arg{"accountId", org}, arg{"name", "Newsletter"}))
	execute(owner, call("mutation", "createSenderIdentity", arg{"senderIdentityId", identity}, arg{"accountId", org}, arg{"address", "hello@client.test"}, arg{"fromName", "Client"}))
	templateArgs := map[string]any{"templateId": template, "accountId": org, "name": "Digest", "content": map[string]any{"subject": "Digest", "textBody": "This week", "htmlBody": "<p>This week</p>"}}
	nodes, err := w.handleSaveTemplate(owner, templateArgs, 0)
	if err != nil {
		t.Fatal(err)
	}
	templateArgs["expectedRevision"], templateArgs["action"] = decodeResult(t, nodes)["revision"], "publish"
	if _, err = w.handleSaveTemplate(owner, templateArgs, 0); err != nil {
		t.Fatal(err)
	}
	execute(owner, call("mutation", "createCampaign", arg{"campaignId", campaign}, arg{"accountId", org}, arg{"name", "Weekly digest"}, arg{"audienceId", audience}, arg{"templateId", template}, arg{"senderIdentityId", identity}))

	configure := func(args map[string]any) {
		t.Helper()
		if _, err := w.handleConfigureSeries(owner, args, 0); err != nil {
			t.Fatalf("%v: %v", args["action"], err)
		}
	}
	read := func() map[string]any {
		t.Helper()
		row, err := w.seriesForCampaign(memql.ContextWithFreshRead(owner), campaign)
		if err != nil || row == nil {
			t.Fatalf("reading the series: %v", err)
		}
		return row
	}
	revisionOf := func(row map[string]any) string { return templateTime(row["createdAt"]).UTC().Format(time.RFC3339Nano) }
	first := now.AddDate(0, 0, 1)
	save := func(weeks int, revision string) {
		t.Helper()
		args := map[string]any{"campaignId": campaign, "action": "save", "intervalWeeks": weeks, "firstSendAt": first.Format(time.RFC3339Nano), "timeZone": "America/Phoenix"}
		if revision != "" {
			args["expectedRevision"] = revision
		}
		configure(args)
	}
	resume := func(row map[string]any) {
		t.Helper()
		configure(map[string]any{"campaignId": campaign, "action": "resume", "expectedRevision": revisionOf(row)})
	}

	// A new series runs: saving one for the first time is Start repeating.
	save(2, "")
	if status := str(read(), "status"); status != "active" {
		t.Fatalf("a new series is %q, not active", status)
	}

	// Paused, then saved with a new cadence: the cadence changes, and it stays paused.
	configure(map[string]any{"campaignId": campaign, "action": "pause", "expectedRevision": revisionOf(read())})
	save(3, revisionOf(read()))
	row := read()
	if str(row, "status") != "paused" || integer(row, "intervalWeeks") != 3 {
		t.Fatalf("saving a paused series restarted it or lost its cadence: status %q, every %d weeks", str(row, "status"), integer(row, "intervalWeeks"))
	}

	// Saved again with nothing changed: nothing is written.
	unchanged := revisionOf(row)
	save(3, unchanged)
	if revisionOf(read()) != unchanged {
		t.Fatal("an unchanged save wrote a new version of the series")
	}

	// Stopped by the scheduler, then saved: it stays stopped, and keeps saying why.
	resume(read())
	running := read()
	const reason = "Publish the template before resuming this schedule."
	if err := w.advanceSeries(system, running, "blocked", templateTime(running["nextAt"]), "", reason); err != nil {
		t.Fatal(err)
	}
	save(4, revisionOf(read()))
	row = read()
	if str(row, "status") != "blocked" || str(row, "lastError") != reason || integer(row, "intervalWeeks") != 4 {
		t.Fatalf("saving a stopped series restarted it or dropped its reason: status %q, lastError %q, every %d weeks", str(row, "status"), str(row, "lastError"), integer(row, "intervalWeeks"))
	}

	// Resume is the way back, and it clears the reason.
	resume(row)
	if row = read(); str(row, "status") != "active" || str(row, "lastError") != "" {
		t.Fatalf("resuming a stopped series: status %q, lastError %q", str(row, "status"), str(row, "lastError"))
	}
}
