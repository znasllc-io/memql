package campaigns

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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

// names_db_test.go -- what a person is told when a name is too long to save
// (memql#6033).
//
// The campaign's name and the sender's name travel in every message's view link,
// so they are capped where they are saved. This drives the real engine, through
// the same mutations the app calls, and reads the refusal as the app does: the
// server's own text, shown beside the control. The test is about that text. A
// refusal that reads `argument "name": value too long (201 runes, max 200)` is
// the engine talking to a developer.

const (
	campaignNameSentence = "Use a campaign name of 200 characters or fewer."
	senderNameSentence   = "Use a sender name of 100 characters or fewer."
)

func TestAnOverLongNameIsRefusedInAPlainSentence(t *testing.T) {
	if reachable, err := dbtest.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	} else if !reachable {
		dbtest.Unreachable(t, "name caps", dbtest.DSN(), nil)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	engine, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	engine.Logger = quietLogger()
	if err := engine.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	w := newTestWorker(t, deliveryDBEngine{engine}, &recordingSender{})
	wired := NewWorker(deliveryDBEngine{engine}, nil, nil, quietLogger(), func() *sql.DB { return db.DB })
	w.templateGate = wired.templateGate

	suffix := fmt.Sprintf("names-%d", time.Now().UnixNano())
	user, org, audience, template, identity := suffix+"-user", suffix+"-org", suffix+"-aud", suffix+"-tpl", suffix+"-sender"
	owner := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: user})
	system := auth.ContextWithInternalOrigin(auth.ContextWithToken(auth.ContextWithAccess(context.Background(), auth.SystemActor("names-test")), &auth.TokenInfo{Subject: "names-test"}))
	run := func(q string) error {
		_, err := engine.Execute(owner, q)
		return err
	}
	must := func(q string) {
		t.Helper()
		if err := run(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := engine.Execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Sender","role":"owner","active":true})`, langparser.QuoteString(user), langparser.QuoteString(user+"@example.test"))); err != nil {
		t.Fatal(err)
	}
	must(call("mutation", "createClientAccount", arg{"accountId", org}, arg{"name", "Names client"}))
	must(call("mutation", "createAudience", arg{"audienceId", audience}, arg{"accountId", org}, arg{"name", "Newsletter"}))
	templateArgs := map[string]any{"templateId": template, "accountId": org, "name": "Welcome", "content": map[string]any{"subject": "Welcome", "textBody": "Thanks", "htmlBody": "<p>Welcome</p>"}}
	nodes, err := w.handleSaveTemplate(owner, templateArgs, 0)
	if err != nil {
		t.Fatal(err)
	}
	templateArgs["expectedRevision"], templateArgs["action"] = decodeResult(t, nodes)["revision"], "publish"
	if _, err := w.handleSaveTemplate(owner, templateArgs, 0); err != nil {
		t.Fatal(err)
	}

	// At the caps, in characters rather than bytes, every write is accepted.
	capName, capSender := strings.Repeat("日", maxCampaignNameRunes), strings.Repeat("é", maxSenderNameRunes)
	must(call("mutation", "createSenderIdentity", arg{"senderIdentityId", identity}, arg{"accountId", org}, arg{"address", "hello@client.test"}, arg{"fromName", capSender}))
	campaign := suffix + "-campaign"
	createCampaign := func(id, name, fromName string) error {
		args := []arg{{"campaignId", id}, {"accountId", org}, {"name", name}, {"audienceId", audience}, {"templateId", template}, {"senderIdentityId", identity}}
		if fromName != "" {
			args = append(args, arg{"fromName", fromName})
		}
		return run(call("mutation", "createCampaign", args...))
	}
	updateCampaign := func(name, fromName string) error {
		args := []arg{{"campaignId", campaign}, {"accountId", org}, {"name", name}, {"audienceId", audience}, {"templateId", template}, {"senderIdentityId", identity}}
		if fromName != "" {
			args = append(args, arg{"fromName", fromName})
		}
		return run(call("mutation", "updateCampaign", args...))
	}
	if err := createCampaign(campaign, capName, capSender); err != nil {
		t.Fatalf("a campaign named at the cap, with a sender name at the cap, was refused: %v", err)
	}
	if err := updateCampaign(capName, capSender); err != nil {
		t.Fatalf("an update that keeps the names at the cap was refused: %v", err)
	}

	// One character over, on every path that saves one.
	overName, overSender := strings.Repeat("日", maxCampaignNameRunes+1), strings.Repeat("é", maxSenderNameRunes+1)
	for name, tc := range map[string]struct {
		save func() error
		want string
	}{
		"createCampaign, name":        {func() error { return createCampaign(suffix+"-c2", overName, "") }, campaignNameSentence},
		"createCampaign, sender name": {func() error { return createCampaign(suffix+"-c3", "Short", overSender) }, senderNameSentence},
		"updateCampaign, name":        {func() error { return updateCampaign(overName, "") }, campaignNameSentence},
		"updateCampaign, sender name": {func() error { return updateCampaign("Short", overSender) }, senderNameSentence},
		"createSenderIdentity": {func() error {
			return run(call("mutation", "createSenderIdentity", arg{"senderIdentityId", suffix + "-s2"}, arg{"accountId", org}, arg{"address", "other@client.test"}, arg{"fromName", overSender}))
		}, senderNameSentence},
		"updateSenderIdentity": {func() error {
			return run(call("mutation", "updateSenderIdentity", arg{"senderIdentityId", identity}, arg{"accountId", org}, arg{"address", "hello@client.test"}, arg{"fromName", overSender}))
		}, senderNameSentence},
	} {
		err := tc.save()
		if err == nil {
			t.Errorf("%s: a name one character over its cap was saved", name)
			continue
		}
		t.Logf("%s: %q", name, err.Error())
		if err.Error() != tc.want {
			t.Errorf("%s: the refusal reads %q, want exactly %q", name, err.Error(), tc.want)
		}
		for _, developerSpeak := range []string{"value too long", "runes", "jsonschema", "argument validation"} {
			if strings.Contains(err.Error(), developerSpeak) {
				t.Errorf("%s: the refusal is the engine talking to a developer (%q): %v", name, developerSpeak, err)
			}
		}
	}
}
