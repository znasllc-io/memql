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

// TestAStorefrontSignupBorrowsTheOwnerTheEdgeStamps signs up the way a shopper's browser does, not the way
// TestNewsletterEnrollmentAndWelcomeAcrossReplicas does. That test subscribes under a ranked owner context with a BARE
// user id; a real signup arrives through the shopper surface, which borrows the site's owner exactly as the edge stamps
// it -- the site row's ownerUserId as a read returns it (component/edge/edge.go siteFromRow), which is the CANONICAL
// v1:identity:user:<id> spelling. subscribeBinding compared that against the bared site owner, so every storefront
// signup was refused with "newsletter signups are unavailable for this deployable" (production, 2026-11-10).
func TestAStorefrontSignupBorrowsTheOwnerTheEdgeStamps(t *testing.T) {
	if reachable, err := dbtest.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	} else if !reachable {
		dbtest.Unreachable(t, "storefront signup owner", dbtest.DSN(), nil)
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
	w.newsletterGate, w.templateGate, w.singleSendGate = wired.newsletterGate, wired.templateGate, wired.singleSendGate
	if err := e.RegisterIntegration(w); err != nil {
		t.Fatal(err)
	}

	suffix := fmt.Sprintf("edge-owner-%d", time.Now().UnixNano())
	user, org, audience, site := suffix+"-user", suffix+"-org", suffix+"-aud", suffix+"-site"
	owner := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: user})
	system := auth.ContextWithInternalOrigin(auth.ContextWithToken(auth.ContextWithAccess(context.Background(), auth.SystemActor("edge-owner-test")), &auth.TokenInfo{Subject: "edge-owner-test"}))
	execute := func(ctx context.Context, q string) []map[string]any {
		t.Helper()
		result, err := e.Execute(auth.ContextWithInternalOrigin(ctx), q)
		if err != nil {
			t.Fatal(err)
		}
		return memql.MaterializeRows(result)
	}
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Owner","role":"owner","active":true})`, langparser.QuoteString(user), langparser.QuoteString(user+"@example.test")))
	execute(owner, call("mutation", "createClientAccount", arg{"accountId", org}, arg{"name", "Storefront client"}))
	execute(owner, call("mutation", "createAudience", arg{"audienceId", audience}, arg{"accountId", org}, arg{"name", "Newsletter"}))
	execute(owner, call("mutation", "createSite", arg{"siteId", site}, arg{"accountId", org}, arg{"hostname", suffix + ".example.test"}, arg{"bundleRef", "blob://sites/" + site + "/v1/"}, arg{"kind", "static"}))
	nodes, err := w.handleConfigureNewsletter(owner, map[string]any{"siteId": site, "audienceId": audience, "consentText": "Yes, email me news and offers. I can unsubscribe anytime.", "enabled": true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	revision := fmt.Sprint(decodeResult(t, nodes)["revision"])

	// The owner the edge would stamp: the site row's ownerUserId as a read returns it.
	sites := execute(system, call("query", "siteById", arg{"siteId", site}))
	if len(sites) != 1 {
		t.Fatalf("the site did not read back: %v", sites)
	}
	stamped := str(sites[0], "ownerUserId")
	if bare(stamped) != user {
		t.Fatalf("the site's owner is %q, not %q", stamped, user)
	}

	// What component/server/shopper_handler.go executes: the declared builtin, under the borrowed owner, no internal origin.
	borrowed := auth.ContextWithUserActor(context.Background(), stamped)
	if _, err := e.Execute(borrowed, call("builtin", "campaignSubscribe", arg{"consent", "yes"}, arg{"consentRevision", revision},
		arg{"email", "shopper@example.test"}, arg{"siteId", site}, arg{"storeId", ""}, arg{"submissionId", suffix})); err != nil {
		t.Fatalf("a storefront signup under the owner the edge stamps (%q) was refused: %v", stamped, err)
	}
}
