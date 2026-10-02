package campaigns

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestOrganizationUnsubscribeIsVisibleOnAnotherReplica(t *testing.T) {
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	defer db.Close()
	ctx := context.Background()
	ping, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		dbtest.Unreachable(t, "organization opt-out", dbtest.DSN(), err)
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	engines := make([]*memql.MemQLEngine, 2)
	stores := make([]*Store, 2)
	for n := range engines {
		e, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		e.Logger = quietLogger()
		if err := e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		engines[n], stores[n] = e, NewStore(deliveryDBEngine{e})
	}
	suffix := fmt.Sprintf("opt-out-%d", time.Now().UnixNano())
	user, accountA, accountB, audience, recipient := suffix+"-user", suffix+"-a", suffix+"-b", suffix+"-aud", suffix+"-rec"
	address := suffix + "@example.test"
	digest := EmailDigest(address)
	system := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, auth.SystemActor("suppression-tests")))
	system = auth.ContextWithToken(system, &auth.TokenInfo{Subject: "suppression-tests"})
	owner := auth.ContextWithToken(auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: user, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: user})
	run := func(ctx context.Context, q string) {
		t.Helper()
		if _, err := engines[0].Execute(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	quote := langparser.QuoteString
	run(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"displayName":"Opt-out owner","primaryEmail":%s,"role":"owner","active":true})`, quote(user), quote(address)))
	for _, account := range []string{accountA, accountB} {
		run(owner, call("mutation", "createClientAccount", arg{"accountId", account}, arg{"name", account}))
	}
	run(owner, call("mutation", "createAudience", arg{"audienceId", audience}, arg{"accountId", accountA}, arg{"name", "Newsletter"}))
	run(owner, call("mutation", "addRecipient", arg{"recipientId", recipient}, arg{"audienceId", audience}, arg{"accountId", accountA}, arg{"email", address}))

	// Warm the sending replica before a different replica receives the click.
	for _, account := range []string{accountA, accountB} {
		if _, found, err := stores[1].SuppressionForSend(system, account, digest); err != nil || found {
			t.Fatalf("unexpected initial suppression: %v %v", found, err)
		}
	}
	token, err := MintUnsubscribeToken(testSecret, UnsubscribePayload{OwnerUserID: user, RecipientID: recipient, AccountID: accountA, EmailDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerFor(deliveryDBEngine{engines[0]})
	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/unsubscribe?token="+url.QueryEscape(token), nil))
		if rr.Code != 200 {
			t.Fatalf("unsubscribe status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
	if _, found, err := stores[1].SuppressionForSend(system, accountA, digest); err != nil || !found {
		t.Fatalf("other replica missed opt-out: %v %v", found, err)
	}
	if _, found, err := stores[1].SuppressionForSend(system, accountB, digest); err != nil || found {
		t.Fatalf("opt-out crossed client boundary: %v %v", found, err)
	}
	rec, found, err := stores[1].RecipientByID(memql.ContextWithFreshRead(owner), recipient)
	if err != nil || !found || rec.SubscriptionStatus != "unsubscribed" {
		t.Fatalf("membership failed to converge: %+v %v", rec, err)
	}

	if _, err := engines[1].Execute(owner, call("mutation", "recordOrganizationSuppression", arg{"suppressionId", "forged"}, arg{"accountId", accountB}, arg{"emailDigest", digest}, arg{"reason", "manual"})); err == nil {
		t.Fatal("external caller bypassed suppression capability")
	}
	if err := stores[0].RecordSuppression(system, digest, "hard_bounce", "example.test", "", ""); err != nil {
		t.Fatal(err)
	}
	if sup, found, err := stores[1].SuppressionForSend(system, accountB, digest); err != nil || !found || sup.Reason != "hard_bounce" {
		t.Fatalf("global safety block lost: %+v %v %v", sup, found, err)
	}
}
