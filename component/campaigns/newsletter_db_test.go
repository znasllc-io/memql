package campaigns

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"github.com/znasllc-io/memql/component/server"
)

func TestNewsletterEnrollmentAndWelcomeAcrossReplicas(t *testing.T) {
	if reachable, err := dbtest.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	} else if !reachable {
		dbtest.Unreachable(t, "newsletter enrollment", dbtest.DSN(), nil)
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
		workers[n].newsletterGate, workers[n].templateGate, workers[n].singleSendGate = wired.newsletterGate, wired.templateGate, wired.singleSendGate
		if err := e.RegisterIntegration(workers[n]); err != nil {
			t.Fatal(err)
		}
	}
	suffix := fmt.Sprintf("newsletter-%d", time.Now().UnixNano())
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

	site := suffix + "-site"
	execute(owner, call("mutation", "createSite", arg{"siteId", site}, arg{"accountId", org}, arg{"hostname", suffix + ".example.test"}, arg{"bundleRef", "sites/newsletter/test"}, arg{"kind", "static"}))
	configArgs := map[string]any{"siteId": site, "audienceId": audience, "templateId": template, "senderIdentityId": identity, "consentText": "I agree to receive Client's newsletter by email.", "enabled": true}
	execute(owner, call("mutation", "createClientAccount", arg{"accountId", org + "-other"}, arg{"name", "Other client"}))
	execute(owner, call("mutation", "createAudience", arg{"audienceId", audience + "-other"}, arg{"accountId", org + "-other"}, arg{"name", "Other audience"}))
	configArgs["audienceId"] = audience + "-other"
	if _, err := workers[0].handleConfigureNewsletter(owner, configArgs, 0); err == nil {
		t.Fatal("newsletter crossed organizations")
	}
	configArgs["audienceId"] = audience
	nodes, err = workers[0].handleConfigureNewsletter(owner, configArgs, 0)
	if err != nil {
		t.Fatal(err)
	}
	configRevision := decodeResult(t, nodes)["revision"]
	args := map[string]any{"siteId": site, "storeId": "", "submissionId": "submission-one", "email": " Subscriber@EXAMPLE.test ", "displayName": "Subscriber", "consent": "yes", "consentRevision": configRevision}
	subscribe := func(w *Worker, args map[string]any) error { _, err := w.handleSubscribe(owner, args, 0); return err }
	race := func(fn func(*Worker) error) {
		t.Helper()
		var wg sync.WaitGroup
		failures := make(chan error, 2)
		for _, w := range workers {
			wg.Add(1)
			go func(w *Worker) { defer wg.Done(); failures <- fn(w) }(w)
		}
		wg.Wait()
		close(failures)
		for err := range failures {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	readSignup := func(address string) map[string]any {
		t.Helper()
		row, err := workers[1].newsletterRow(owner, "newsletterSignupById", "signupId", newsletterSignupID(audience, address))
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	race(func(w *Worker) error { return subscribe(w, args) })
	signup := readSignup("subscriber@example.test")
	if signup == nil || str(signup, "status") != "pending" || bare(str(signup, "accountId")) != org || str(signup, "consentText") != configArgs["consentText"] || !templateTime(signup["consentRevision"]).Equal(templateTime(configRevision)) {
		t.Fatalf("signup: %v", signup)
	}
	roster, err := workers[1].store.Roster(memql.ContextWithFreshRead(owner), audience)
	if err != nil || len(roster) != 1 || roster[0].Email != "subscriber@example.test" {
		t.Fatalf("roster after duplicate forms: %v %v", roster, err)
	}
	consent, err := workers[1].newsletterRow(owner, "newsletterConsentById", "eventId", sha256Hex("newsletter-consent\x00"+newsletterSignupID(audience, "subscriber@example.test")))
	if err != nil || consent == nil || str(consent, "source") != "signup" {
		t.Fatalf("consent: %v %v", consent, err)
	}
	// The real HTTP carrier stamps trusted binding fields; forged form copies
	// cannot change the audience or organization. It answers only a redirect.
	handler := server.NewShopperHandler(deliveryDBEngine{engines[1]}, newsletterTestSites{workers[1]}, quietLogger())
	body := url.Values{"email": {"subscriber@example.test"}, "consent": {"yes"}, "consentRevision": {fmt.Sprint(configRevision)}, "audienceId": {audience + "-other"}, "storeId": {"attacker-store"}, "ownerUserId": {"attacker"}}
	req := httptest.NewRequest(http.MethodPost, "/forms/campaigns/subscribe", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(memql.ShopperSiteHeader, site)
	req.Header.Set(memql.ShopperOwnerHeader, user)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/newsletter/thank-you" {
		t.Fatalf("public form: %d %s", response.Code, response.Header().Get("Location"))
	}
	if readSignup("subscriber@example.test") == nil {
		t.Fatal("carrier lost signup")
	}
	// Editing after signup leaves the already reviewed welcome intact.
	published, _, err := workers[0].store.TemplateByID(memql.ContextWithFreshRead(owner), template)
	if err != nil {
		t.Fatal(err)
	}
	templateArgs["action"], templateArgs["expectedRevision"] = "save", published.Revision
	templateArgs["content"] = map[string]any{"subject": "Next welcome", "textBody": "New text", "htmlBody": "<p>New text</p>"}
	edited, err := workers[0].handleSaveTemplate(owner, templateArgs, 0)
	if err != nil {
		t.Fatal(err)
	}
	race(func(w *Worker) error {
		return w.sendNewsletterWelcome(context.Background(), w.systemActorContext(context.Background()), signup)
	})
	if sender.count() != 1 || sender.sent[0].Subject != "Welcome" {
		t.Fatalf("welcome: %v", sender.sent)
	}
	if str(readSignup("subscriber@example.test"), "status") != "sent" {
		t.Fatal("welcome outcome not saved")
	}
	// A lost progress write cannot cause a second provider submission.
	execute(system, call("mutation", "updateNewsletterWelcome", arg{"signupId", bare(str(signup, "id"))}, arg{"status", "pending"}, arg{"lastError", ""}, arg{"versionTime", time.Now().Add(time.Second).Format(time.RFC3339Nano)}))
	if err = workers[1].sendNewsletterWelcome(context.Background(), workers[1].systemActorContext(context.Background()), signup); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 {
		t.Fatal("lost outcome submitted the welcome twice")
	}
	// Re-publish for later signups; the first consent and recipient stay stable.
	templateArgs["action"], templateArgs["expectedRevision"] = "publish", decodeResult(t, edited)["revision"]
	if _, err = workers[0].handleSaveTemplate(owner, templateArgs, 0); err != nil {
		t.Fatal(err)
	}
	if err = subscribe(workers[1], args); err != nil {
		t.Fatal(err)
	}
	repeated, err := workers[1].newsletterRow(owner, "newsletterConsentById", "eventId", bare(str(consent, "id")))
	if err != nil || !templateTime(repeated["createdAt"]).Equal(templateTime(consent["createdAt"])) {
		t.Fatal("duplicate form rewrote consent")
	}
	// Neither missing consent, stale copy nor a preview-store stamp enrolls.
	for key, value := range map[string]any{"consent": "no", "consentRevision": "2020-01-01T00:00:00Z", "storeId": "different-preview-store"} {
		bad := map[string]any{}
		for k, v := range args {
			bad[k] = v
		}
		bad["email"] = "refused@example.test"
		bad[key] = value
		if err = subscribe(workers[0], bad); err == nil {
			t.Fatalf("accepted invalid %s", key)
		}
	}
	if readSignup("refused@example.test") != nil {
		t.Fatal("refused form created a welcome")
	}
	// Prior unsubscribe state is never reset by a public form.
	execute(owner, call("mutation", "addRecipient", arg{"recipientId", recipient}, arg{"audienceId", audience}, arg{"accountId", org}, arg{"email", "optout@example.test"}))
	execute(owner, call("mutation", "setRecipientSubscription", arg{"recipientId", recipient}, arg{"subscriptionStatus", "unsubscribed"}))
	args["email"] = "optout@example.test"
	if err = subscribe(workers[0], args); err != nil {
		t.Fatal(err)
	}
	if readSignup("optout@example.test") != nil {
		t.Fatal("opted-out recipient was enrolled")
	}
	// A failed consent write precedes recipient creation, so it sends nothing.
	actual := workers[0].store.engine
	workers[0].store.engine = newsletterWriteFailure{actual, "recordConsentGrant"}
	args["email"] = "consent-failed@example.test"
	if err = subscribe(workers[0], args); err == nil {
		t.Fatal("injected consent failure ignored")
	}
	workers[0].store.engine = actual
	roster, err = workers[1].store.Roster(memql.ContextWithFreshRead(owner), audience)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range roster {
		if r.Email == "consent-failed@example.test" {
			t.Fatal("recipient became sendable without consent")
		}
	}
	// An interrupted final acceptance can be retried without a duplicate recipient.
	workers[0].store.engine = newsletterWriteFailure{actual, "recordNewsletterSignup"}
	args["email"] = "retry@example.test"
	if err = subscribe(workers[0], args); err == nil {
		t.Fatal("injected signup failure ignored")
	}
	workers[0].store.engine = actual
	if err = subscribe(workers[1], args); err != nil {
		t.Fatal(err)
	}
	roster, err = workers[1].store.Roster(memql.ContextWithFreshRead(owner), audience)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range roster {
		if r.Email == "retry@example.test" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("retry created %d recipients", count)
	}
	// A queued welcome stays bound to the address that actually consented.
	args["email"] = "original@example.test"
	if err = subscribe(workers[0], args); err != nil {
		t.Fatal(err)
	}
	changed := readSignup("original@example.test")
	execute(owner, call("mutation", "addRecipient", arg{"recipientId", bare(str(changed, "recipientId"))}, arg{"audienceId", audience}, arg{"accountId", org}, arg{"email", "replacement@example.test"}))
	if err = workers[1].sendNewsletterWelcome(context.Background(), workers[1].systemActorContext(context.Background()), changed); err != nil {
		t.Fatal(err)
	}
	changed = readSignup("original@example.test")
	if str(changed, "status") != "blocked" || str(changed, "email") != "original@example.test" || !strings.Contains(str(changed, "lastError"), "address changed") || sender.count() != 1 {
		t.Fatalf("queued welcome followed an edited address: %v", changed)
	}
	// Rate limiting is retryable before the transport; it must leave the
	// accepted welcome pending rather than require an operator to repair it.
	args["email"] = "rate@example.test"
	if err = subscribe(workers[0], args); err != nil {
		t.Fatal(err)
	}
	limited := readSignup("rate@example.test")
	workers[0].limiter = &rateLimiter{now: time.Now, last: time.Now(), tokens: 0, perSec: 0, capacity: 1}
	if err = workers[0].sendNewsletterWelcome(context.Background(), workers[0].systemActorContext(context.Background()), limited); err != nil {
		t.Fatal(err)
	}
	if str(readSignup("rate@example.test"), "status") != "pending" {
		t.Fatal("rate ceiling lost a welcome")
	}
	workers[0].limiter = nil
	// A timeout after beginning a provider attempt is uncertain. Rechecking
	// it on another replica must consult the durable receipt, never resubmit.
	args["email"] = "uncertain@example.test"
	if err = subscribe(workers[0], args); err != nil {
		t.Fatal(err)
	}
	uncertain := readSignup("uncertain@example.test")
	attempts := 0
	sender.fail = func(int) error { attempts++; return context.DeadlineExceeded }
	if err = workers[0].sendNewsletterWelcome(context.Background(), workers[0].systemActorContext(context.Background()), uncertain); err != nil {
		t.Fatal(err)
	}
	sender.fail = nil
	uncertain = readSignup("uncertain@example.test")
	if str(uncertain, "status") != "blocked" {
		t.Fatal("failed attempt not reported")
	}
	if _, err = workers[1].handleRetryNewsletterWelcome(owner, map[string]any{"signupId": bare(str(uncertain, "id")), "expectedRevision": templateTime(uncertain["createdAt"]).Format(time.RFC3339Nano)}, 0); err != nil {
		t.Fatal(err)
	}
	if err = workers[1].sendNewsletterWelcome(context.Background(), workers[1].systemActorContext(context.Background()), uncertain); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || sender.count() != 1 || str(readSignup("uncertain@example.test"), "status") != "uncertain" {
		t.Fatal("uncertain attempt was submitted again")
	}
	// Current permissions, not the authority held at signup, decide the send.
	args["email"] = "revoked@example.test"
	if err = subscribe(workers[0], args); err != nil {
		t.Fatal(err)
	}
	revoked := readSignup("revoked@example.test")
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Sender","role":"reader","active":true})`, langparser.QuoteString(user), langparser.QuoteString(user+"@example.test")))
	if err = workers[1].sendNewsletterWelcome(context.Background(), workers[1].systemActorContext(context.Background()), revoked); err != nil {
		t.Fatal(err)
	}
	if str(readSignup("revoked@example.test"), "status") != "blocked" || sender.count() != 1 {
		t.Fatal("revoked authority sent a welcome")
	}
	// A stranger cannot read configuration or receipts, and raw writes cannot forge them.
	outsider := suffix + "-outsider"
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Outside","role":"writer","active":true})`, langparser.QuoteString(outsider), langparser.QuoteString(outsider+"@example.test")))
	hidden, err := workers[1].newsletterRow(actor(outsider, auth.RoleWriter), "newsletterForSite", "siteId", site)
	if err == nil && hidden != nil {
		t.Fatal("another organization read newsletter config")
	}
	hidden, err = workers[1].newsletterRow(actor(outsider, auth.RoleWriter), "newsletterSignupById", "signupId", bare(str(signup, "id")))
	if err == nil && hidden != nil {
		t.Fatal("another organization read the subscriber mailbox")
	}
	hidden, err = workers[1].newsletterRow(actor(outsider, auth.RoleWriter), "newsletterWelcomesForAudience", "audienceId", audience)
	if err == nil && hidden != nil {
		t.Fatal("another organization read recent welcome outcomes")
	}
	for _, concept := range []string{"newsletterBinding", "newsletterSignup"} {
		_, err = engines[1].Execute(owner, fmt.Sprintf(`insert("v1:campaigns:%s", id="forged-newsletter", payload={})`, concept))
		if err == nil || !strings.Contains(err.Error(), "newsletter writes require") {
			t.Fatalf("raw %s: %v", concept, err)
		}
	}
	// Retiring a sender must not trap an enabled form in an uneditable state.
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Sender","role":"owner","active":true})`, langparser.QuoteString(user), langparser.QuoteString(user+"@example.test")))
	execute(owner, call("mutation", "setSenderIdentityStatus", arg{"senderIdentityId", identity}, arg{"status", "disabled"}))
	configArgs["enabled"], configArgs["expectedRevision"] = false, configRevision
	if _, err = workers[1].handleConfigureNewsletter(owner, configArgs, 0); err != nil {
		t.Fatalf("could not turn off a retired sender's form: %v", err)
	}
}

type newsletterWriteFailure struct {
	Engine
	mutation string
}

func (e newsletterWriteFailure) Execute(ctx context.Context, statement string) (any, error) {
	if strings.Contains(statement, e.mutation+"(") {
		return nil, errors.New("injected newsletter write failure")
	}
	return e.Engine.Execute(ctx, statement)
}

type newsletterTestSites struct{ worker *Worker }

func (s newsletterTestSites) ShopperSite(ctx context.Context, siteID, owner string) (*server.ShopperSite, error) {
	row, err := s.worker.newsletterRow(auth.ContextWithUserActor(ctx, owner), "siteById", "siteId", siteID)
	if err != nil || row == nil || bare(str(row, "ownerUserId")) != owner {
		return nil, err
	}
	return &server.ShopperSite{ID: siteID, ShopperForms: booleanOr(row, "shopperForms", false), StoreID: bare(str(objectField(row, "binding"), "storeId"))}, nil
}
