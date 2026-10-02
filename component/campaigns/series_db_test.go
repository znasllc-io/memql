package campaigns

import (
	"context"
	"database/sql"
	"errors"
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

func TestRecurringCampaignsAcrossReplicas(t *testing.T) {
	if reachable, err := dbtest.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	} else if !reachable {
		dbtest.Unreachable(t, "single send receipt", dbtest.DSN(), nil)
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
		workers[n].seriesGate, workers[n].templateGate = wired.seriesGate, wired.templateGate
	}
	suffix := fmt.Sprintf("series-%d", time.Now().UnixNano())
	user, org, audience, recipient, template, identity := suffix+"-user", suffix+"-org", suffix+"-aud", suffix+"-rec", suffix+"-tpl", suffix+"-sender"
	actor := func(user string, role auth.Role) context.Context {
		return auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: role}), &auth.TokenInfo{Subject: user})
	}
	owner := actor(user, auth.RoleOwner)
	system := auth.ContextWithInternalOrigin(auth.ContextWithToken(auth.ContextWithAccess(context.Background(), auth.SystemActor("single-send-test")), &auth.TokenInfo{Subject: "single-send-test"}))
	execute := func(ctx context.Context, q string) {
		t.Helper()
		if _, err := engines[0].Execute(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Sender","role":"owner","active":true})`, langparser.QuoteString(user), langparser.QuoteString(user+"@example.test")))
	execute(owner, call("mutation", "createClientAccount", arg{"accountId", org}, arg{"name", "Receipt client"}))
	execute(owner, call("mutation", "createAudience", arg{"audienceId", audience}, arg{"accountId", org}, arg{"name", "Newsletter"}))
	execute(owner, call("mutation", "createSenderIdentity", arg{"senderIdentityId", identity}, arg{"accountId", org}, arg{"address", "hello@client.test"}, arg{"fromName", "Client"}))
	templateArgs := map[string]any{"templateId": template, "accountId": org, "name": "Welcome", "content": map[string]any{"subject": "Welcome", "textBody": "Thanks for subscribing", "htmlBody": "<p>Welcome</p>"}}
	nodes, err := workers[0].handleSaveTemplate(owner, templateArgs, 0)
	if err != nil {
		t.Fatal(err)
	}
	templateArgs["expectedRevision"], templateArgs["action"] = decodeResult(t, nodes)["revision"], "publish"
	if _, err := workers[0].handleSaveTemplate(owner, templateArgs, 0); err != nil {
		t.Fatal(err)
	}

	blueprint := suffix + "-blueprint"
	execute(owner, call("mutation", "createCampaign", arg{"campaignId", blueprint}, arg{"accountId", org}, arg{"name", "Client newsletter"}, arg{"audienceId", audience}, arg{"templateId", template}, arg{"senderIdentityId", identity}))
	now := time.Date(2028, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, w := range workers {
		w.now = func() time.Time { return now }
	}
	first := now.AddDate(0, 0, 1)
	args := map[string]any{"campaignId": blueprint, "action": "save", "intervalWeeks": 2, "firstSendAt": first.Format(time.RFC3339Nano), "timeZone": "America/Phoenix"}
	nodes, err = workers[0].handleConfigureSeries(owner, args, 0)
	if err != nil {
		t.Fatal(err)
	}
	revision := decodeResult(t, nodes)["revision"]
	again, err := workers[1].handleConfigureSeries(owner, args, 0)
	if err != nil || decodeResult(t, again)["revision"] != revision {
		t.Fatalf("save retry changed series: %v", err)
	}
	read := func(w *Worker) map[string]any {
		t.Helper()
		row, err := w.seriesForCampaign(memql.ContextWithFreshRead(owner), blueprint)
		if err != nil || row == nil {
			t.Fatalf("series read: %v", err)
		}
		return row
	}
	// Saving an empty audience is useful before the storefront has subscribers.
	execute(owner, call("mutation", "addRecipient", arg{"recipientId", recipient}, arg{"audienceId", audience}, arg{"accountId", org}, arg{"email", "subscriber@example.test"}))
	outsider := suffix + "-outsider"
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Outside","role":"writer","active":true})`, langparser.QuoteString(outsider), langparser.QuoteString(outsider+"@example.test")))
	hidden, denied := workers[1].seriesForCampaign(actor(outsider, auth.RoleWriter), blueprint)
	if denied == nil && hidden != nil {
		t.Fatal("another organization can read a private schedule")
	}
	stale := read(workers[1])
	now = first.Add(time.Minute)
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for _, w := range workers {
		wg.Add(1)
		go func(w *Worker) {
			defer wg.Done()
			failures <- w.promoteSeries(context.Background(), w.systemActorContext(context.Background()), stale)
		}(w)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	row := read(workers[1])
	firstID := occurrenceID(seriesID(blueprint), first)
	if bare(str(row, "lastCampaignId")) != firstID || !templateTime(row["nextAt"]).Equal(first.AddDate(0, 0, 14)) {
		t.Fatalf("wrong occurrence: %v", row)
	}
	deliver := func(w *Worker, id string) {
		t.Helper()
		ctx := memql.ContextWithFreshRead(context.Background())
		sys := w.systemActorContext(ctx)
		job, ok, err := w.store.JobByID(sys, id)
		if err != nil || !ok {
			t.Fatalf("job absent: %v", err)
		}
		w.promoteSchedule(ctx, sys, job)
		job, ok, err = w.store.JobByID(sys, id)
		if err != nil || !ok {
			t.Fatal(err)
		}
		w.processJob(ctx, sys, job)
	}
	deliver(workers[0], firstID)
	deliver(workers[1], firstID)
	if sender.count() != 1 {
		t.Fatalf("first occurrence sent %d messages", sender.count())
	}
	// Lose the series advance after its occurrence is committed. A sibling must
	// keep the already-created job and its ledger, then advance the series once.
	nextAt := templateTime(row["nextAt"])
	now = nextAt.Add(time.Minute)
	actual := workers[0].store.engine
	workers[0].store.engine = seriesAdvanceFailure{actual}
	if err = workers[0].promoteSeries(context.Background(), workers[0].systemActorContext(context.Background()), row); err == nil {
		t.Fatal("injected advance failure ignored")
	}
	workers[0].store.engine = actual
	secondID := occurrenceID(seriesID(blueprint), nextAt)
	deliver(workers[0], secondID)
	if err = workers[1].promoteSeries(context.Background(), workers[1].systemActorContext(context.Background()), row); err != nil {
		t.Fatal(err)
	}
	deliver(workers[1], secondID)
	if sender.count() != 2 {
		t.Fatalf("separate occurrences or recovery lost ledger: %d", sender.count())
	}
	row = read(workers[1])
	// Downtime produces one overdue occurrence, then advances strictly beyond now.
	missed := templateTime(row["nextAt"])
	now = missed.AddDate(0, 0, 70)
	if err = workers[0].promoteSeries(context.Background(), workers[0].systemActorContext(context.Background()), row); err != nil {
		t.Fatal(err)
	}
	row = read(workers[1])
	if !templateTime(row["nextAt"]).After(now) || bare(str(row, "lastCampaignId")) != occurrenceID(seriesID(blueprint), missed) {
		t.Fatalf("downtime replayed missed intervals: %v", row)
	}
	pause := map[string]any{"campaignId": blueprint, "action": "pause", "expectedRevision": templateTime(row["createdAt"]).Format(time.RFC3339Nano)}
	if _, err = workers[1].handleConfigureSeries(owner, pause, 0); err != nil {
		t.Fatal(err)
	}
	paused := read(workers[0])
	now = now.AddDate(0, 0, 100)
	if err = workers[0].promoteSeries(context.Background(), workers[0].systemActorContext(context.Background()), paused); err != nil {
		t.Fatal(err)
	}
	if str(read(workers[1]), "lastCampaignId") != str(paused, "lastCampaignId") {
		t.Fatal("paused series created an occurrence")
	}
	if _, err = workers[0].handleConfigureSeries(owner, map[string]any{"campaignId": blueprint, "action": "resume", "expectedRevision": revision}, 0); err == nil {
		t.Fatal("stale update admitted")
	}
	if _, err = workers[0].handleConfigureSeries(owner, map[string]any{"campaignId": blueprint, "action": "resume", "expectedRevision": templateTime(paused["createdAt"]).Format(time.RFC3339Nano)}, 0); err != nil {
		t.Fatal(err)
	}

	peerID := suffix + "-peer"
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Peer","role":"owner","active":true})`, langparser.QuoteString(peerID), langparser.QuoteString(peerID+"@example.test")))
	peer := actor(peerID, auth.RoleOwner)
	current := read(workers[0])
	if _, err = workers[1].handleConfigureSeries(peer, map[string]any{"campaignId": blueprint, "action": "pause", "expectedRevision": templateTime(current["createdAt"]).Format(time.RFC3339Nano)}, 0); err != nil {
		t.Fatal(err)
	}
	current = read(workers[0])
	if bare(str(current, "ownerUserId")) != user || bare(str(current, "authorizedByUserId")) != peerID {
		t.Fatalf("organization edit lost creator or current authority: %v", current)
	}
	if _, err = workers[0].handleConfigureSeries(owner, map[string]any{"campaignId": blueprint, "action": "resume", "expectedRevision": templateTime(current["createdAt"]).Format(time.RFC3339Nano)}, 0); err != nil {
		t.Fatal(err)
	}
	resumed := read(workers[1])
	if !templateTime(resumed["nextAt"]).After(now) {
		t.Fatal("resume replays overdue mail")
	}
	// Raw and named writes cannot forge an approved schedule or its progress.
	for _, q := range []string{
		fmt.Sprintf(`insert("v1:campaigns:campaignSeries", id=%s, payload={"status":"active"})`, langparser.QuoteString(seriesID(blueprint))),
		call("mutation", "advanceCampaignSeries", arg{"seriesId", seriesID(blueprint)}, arg{"nextAt", first.Format(time.RFC3339)}, arg{"status", "active"}, arg{"lastError", ""}, arg{"versionTime", now.Format(time.RFC3339Nano)}),
	} {
		if _, err = engines[0].Execute(owner, q); err == nil {
			t.Fatal("forged series write admitted")
		}
	}
	// A permission loss is checked on the second replica at occurrence time.
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Sender","role":"reader","active":true})`, langparser.QuoteString(user), langparser.QuoteString(user+"@example.test")))
	now = templateTime(resumed["nextAt"]).Add(time.Minute)
	if err = workers[1].promoteSeries(context.Background(), workers[1].systemActorContext(context.Background()), resumed); err != nil {
		t.Fatal(err)
	}
	blocked, err := workers[0].seriesForCampaign(memql.ContextWithFreshRead(workers[0].systemActorContext(context.Background())), blueprint)
	if err != nil || str(blocked, "status") != "blocked" || str(blocked, "lastError") == "" {
		t.Fatalf("revoked authority not blocked: %v %v", blocked, err)
	}
	if sender.count() != 2 {
		t.Fatal("permission revocation mailed recipients")
	}
}

type seriesAdvanceFailure struct{ Engine }

func (e seriesAdvanceFailure) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "mutation advanceCampaignSeries(") {
		return nil, errors.New("simulated lost advance")
	}
	return e.Engine.Execute(ctx, q)
}
