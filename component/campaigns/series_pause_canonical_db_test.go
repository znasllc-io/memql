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

// A signed-in session carries the CANONICAL user id (IdentityResolver.LoadFromClaims
// refuses any other sub), while TestRecurringCampaignsAcrossReplicas signs in
// with a bare one. Repeating a pause whose reply was lost must answer the
// paused series' receipt, not refuse the old revision or write a new one.
func TestARepeatedPauseFromASignedInOwnerIsAReceipt(t *testing.T) {
	if reachable, err := dbtest.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	} else if !reachable {
		dbtest.Unreachable(t, "repeated pause receipt", dbtest.DSN(), nil)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
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
	w := newTestWorker(t, deliveryDBEngine{e}, &recordingSender{})
	wired := NewWorker(deliveryDBEngine{e}, nil, nil, quietLogger(), gatePoolFor(t))
	w.seriesGate, w.templateGate = wired.seriesGate, wired.templateGate

	suffix := fmt.Sprintf("pause-%d", time.Now().UnixNano())
	user, org, audience, template, identity := suffix+"-user", suffix+"-org", suffix+"-aud", suffix+"-tpl", suffix+"-sender"
	signedIn := "v1:identity:user:" + user
	owner := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: signedIn, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: signedIn})
	system := auth.ContextWithInternalOrigin(auth.ContextWithToken(auth.ContextWithAccess(context.Background(), auth.SystemActor("repeated-pause-test")), &auth.TokenInfo{Subject: "repeated-pause-test"}))
	execute := func(ctx context.Context, q string) {
		t.Helper()
		if _, err := e.Execute(auth.ContextWithInternalOrigin(ctx), q); err != nil {
			t.Fatal(err)
		}
	}
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Sender","role":"owner","active":true})`, langparser.QuoteString(user), langparser.QuoteString(user+"@example.test")))
	execute(owner, call("mutation", "createClientAccount", arg{"accountId", org}, arg{"name", "Pause client"}))
	execute(owner, call("mutation", "createAudience", arg{"audienceId", audience}, arg{"accountId", org}, arg{"name", "Newsletter"}))
	execute(owner, call("mutation", "createSenderIdentity", arg{"senderIdentityId", identity}, arg{"accountId", org}, arg{"address", "hello@client.test"}, arg{"fromName", "Client"}))
	templateArgs := map[string]any{"templateId": template, "accountId": org, "name": "Welcome", "content": map[string]any{"subject": "Welcome", "textBody": "x", "htmlBody": "<p>x</p>"}}
	nodes, err := w.handleSaveTemplate(owner, templateArgs, 0)
	if err != nil {
		t.Fatal(err)
	}
	templateArgs["expectedRevision"], templateArgs["action"] = decodeResult(t, nodes)["revision"], "publish"
	if _, err := w.handleSaveTemplate(owner, templateArgs, 0); err != nil {
		t.Fatal(err)
	}
	blueprint := suffix + "-blueprint"
	execute(owner, call("mutation", "createCampaign", arg{"campaignId", blueprint}, arg{"accountId", org}, arg{"name", "Blueprint"}, arg{"audienceId", audience}, arg{"templateId", template}, arg{"senderIdentityId", identity}))
	now := time.Date(2028, 1, 1, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }
	nodes, err = w.handleConfigureSeries(owner, map[string]any{"campaignId": blueprint, "action": "save", "intervalWeeks": 2, "firstSendAt": now.AddDate(0, 0, 1).Format(time.RFC3339Nano), "timeZone": "UTC"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	saved := decodeResult(t, nodes)["revision"]
	pause := map[string]any{"campaignId": blueprint, "action": "pause", "expectedRevision": saved}
	nodes, err = w.handleConfigureSeries(owner, pause, 0)
	if err != nil {
		t.Fatal(err)
	}
	paused := decodeResult(t, nodes)["revision"]

	// The reply was lost; the client sends the identical pause again.
	nodes, err = w.handleConfigureSeries(owner, pause, 0)
	if err != nil {
		t.Fatalf("a repeated pause was refused: %v", err)
	}
	if got := decodeResult(t, nodes)["revision"]; got != paused {
		t.Fatalf("a repeated pause wrote revision %v, want the paused series' %v", got, paused)
	}
}
