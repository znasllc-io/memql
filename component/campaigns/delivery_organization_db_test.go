package campaigns

import (
	"context"
	"database/sql"
	"encoding/json"
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

type deliveryDBEngine struct{ *memql.MemQLEngine }

func (e deliveryDBEngine) Execute(ctx context.Context, query string) (any, error) {
	return e.MemQLEngine.Execute(ctx, query)
}

// Exercise the actual post-send writer: a rule is not a campaign, and a fake
// engine cannot catch the organization guard rejecting that false reference.
func TestRuleDeliveryPersistsOrganizationAndOwnerWithoutFalseCampaign(t *testing.T) {
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	defer db.Close()
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		dbtest.Unreachable(t, "rule delivery attribution", dbtest.DSN(), err)
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
	suffix := fmt.Sprintf("rule-ledger-%d", time.Now().UnixNano())
	user, account, audience, recipient, template, rule := suffix+"-user", suffix+"-org", suffix+"-aud", suffix+"-recipient", suffix+"-template", suffix+"-rule"
	system := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, auth.SystemActor("rule-ledger-test")))
	system = auth.ContextWithToken(system, &auth.TokenInfo{Subject: "rule-ledger-test"})
	execute := func(ctx context.Context, q string) {
		t.Helper()
		if _, err := eng.Execute(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"displayName":"Rule owner","primaryEmail":"%s@example.test","role":"owner","active":true})`, langparser.QuoteString(user), user))
	owner := auth.ContextWithToken(auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: user, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: user})
	execute(owner, fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Rule organization")`, langparser.QuoteString(account)))
	execute(owner, fmt.Sprintf(`mutation createAudience(audienceId: %s, name: "Rule audience", accountId: %s)`, langparser.QuoteString(audience), langparser.QuoteString(account)))
	execute(owner, fmt.Sprintf(`mutation addRecipient(recipientId: %s, audienceId: %s, email: "recipient@example.test", accountId: %s)`, langparser.QuoteString(recipient), langparser.QuoteString(audience), langparser.QuoteString(account)))
	execute(owner, fmt.Sprintf(`mutation createTemplate(templateId: %s, name: "Rule template", subject: "Hello", htmlBody: "Hello", textBody: "Hello", accountId: %s)`, langparser.QuoteString(template), langparser.QuoteString(account)))
	execute(owner, fmt.Sprintf(`mutation createEmailRule(emailRuleId: %s, name: "Rule", triggerConcept: "v1:campaigns:recipient", eventKind: "created", templateId: %s, recipientMode: "audience", audienceId: %s, accountId: %s)`, langparser.QuoteString(rule), langparser.QuoteString(template), langparser.QuoteString(audience), langparser.QuoteString(account)))
	worker := &Worker{store: NewStore(deliveryDBEngine{eng}), logger: quietLogger()}
	for i := 0; i < 2; i++ {
		worker.recordLoneDelivery(owner, Campaign{ID: rule, AccountID: account, OwnerUserID: user}, Recipient{ID: recipient, Email: "recipient@example.test"}, rule, Delivery{Status: "sent", SentAt: time.Now()})
	}
	var rows []memorynodes.MemoryNode
	if err := db.NewSelect().Model(&rows).Where("concept = ?", "v1:campaigns:delivery").Where("payload->>'emailRuleId' IN (?, ?)", rule, "v1:campaigns:emailRule:"+rule).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d ledger versions, want 2 successful writes", len(rows))
	}
	if want := deliveryRowID(rule, recipient); memql.BareShortId(rows[0].ID) != want {
		t.Fatalf("rule ledger id = %q, want existing derivation %q", rows[0].ID, want)
	}
	if rows[0].ID != rows[1].ID {
		t.Fatal("rule retry split ledger identity")
	}
	for _, row := range rows {
		if memql.BareShortId(row.CreatedBy) != user {
			t.Fatalf("creator = %q, want %q", row.CreatedBy, user)
		}
		var p map[string]any
		if err := json.Unmarshal(row.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p["campaignId"] != nil && p["campaignId"] != "" {
			t.Fatalf("false campaign relationship: %v", p)
		}
		if memql.BareShortId(fmt.Sprint(p["ownerUserId"])) != user || memql.BareShortId(fmt.Sprint(p["accountId"])) != account {
			t.Fatalf("wrong owner/organization: %v", p)
		}
	}
}
