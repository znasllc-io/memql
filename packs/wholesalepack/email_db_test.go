package wholesalepack_test

// email_db_test.go -- THE APPLICANT'S EMAILS THROUGH THE REAL ROW LANE, against
// a real database and a real engine: the campaigns worker registered beside
// the pack, an organization with a published transactional template and a
// sender, and the pack's own resolver registered by its provider. NO EMAIL
// LEAVES: the worker's sender is a recorder in this file, and every address is
// at example.com or example.test.
//
// Postgres-gated like every *_db_test.go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/campaigns"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// recordingMail is the lane's provider: it keeps every message and sends none.
type recordingMail struct {
	mu   sync.Mutex
	sent []email.Message
}

func (m *recordingMail) Send(_ context.Context, msg email.Message, _ email.SendAs) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}

func (m *recordingMail) messages() []email.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]email.Message(nil), m.sent...)
}

// campaignsEngine is the campaigns worker's view of the engine.
type campaignsEngine struct{ eng *memql.MemQLEngine }

func (e campaignsEngine) Execute(ctx context.Context, q string) (any, error) {
	return e.eng.Execute(ctx, q)
}
func (e campaignsEngine) OrganizationCapable(ctx context.Context, account, verb, resource string) bool {
	return e.eng.OrganizationCapable(ctx, account, verb, resource)
}

// emailStore is an access store whose organization has a sender and published
// transactional templates for the four emails, chosen in the store's settings
// by a cluster operator, with the row lane registered on the engine.
type emailStore struct {
	accessStore
	mail *recordingMail
	logs *lockedBuffer
}

// templateFor is the template id this store's email of kind is sent with.
func (s emailStore) templateFor(kind string) string { return "tpl-" + kind + "-" + s.site }

func newEmailStore(t *testing.T, label string, termsRequired bool) (*memql.MemQLEngine, emailStore) {
	t.Helper()
	eng := liveWholesaleEngine(t)
	return eng, newEmailStoreOn(t, eng, label, termsRequired, nil)
}

// newEmailStoreOn is newEmailStore on an engine the caller booted, with extra
// fields posted on the store's first application (a client's extension).
func newEmailStoreOn(t *testing.T, eng *memql.MemQLEngine, label string, termsRequired bool, extra url.Values) emailStore {
	t.Helper()
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng.Logger = logger
	prior := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prior) })
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gates.Close() })
	mail := &recordingMail{}
	worker := campaigns.NewWorker(campaignsEngine{eng}, nil, func() email.Sender { return mail }, logger, func() *sql.DB { return gates })
	if err := eng.RegisterIntegration(worker); err != nil {
		t.Fatalf("register the campaigns worker: %v", err)
	}
	s := emailStore{accessStore: newAccessStoreWith(t, eng, label, extra), mail: mail, logs: logs}
	for kind, body := range map[string]string{"terms": "Net 30 for Kestrel Cycles.", "map": "Advertise at list.", "priceSheet": "Frame 410.00"} {
		answerOf(t, eng, signedIn(s.staff), `builtin wholesalePublishDocument(siteId: `+quote(s.site)+`, storeId: `+quote(s.store)+
			`, kind: `+quote(kind)+`, label: `+quote(kind+" 2026")+`, body: `+quote(body)+`)`)
	}
	sender := "sender-" + s.site
	wsExec(t, eng, seedCtx(), rawInsert("v1:campaigns:senderIdentity", sender, map[string]any{"ownerUserId": s.owner,
		"accountId": s.account, "address": "trade@example.com", "fromName": "Kestrel Trade", "status": "active"}))
	bodies := map[string][2]string{
		wholesalepack.EmailTerms:    {"Your trade terms at {{fields.storeName}}", "Hello {{fields.applicantName}}, read and accept: {{fields.termsUrl}} (until {{fields.termsExpiresOn}})"},
		wholesalepack.EmailApproved: {"Trade prices on at {{fields.storeName}}", "Welcome, {{fields.companyName}}. Your price sheet: {{fields.priceSheetUrl}}"},
		wholesalepack.EmailRejected: {"Your application to {{fields.storeName}}", "We cannot offer {{fields.companyName}} trade terms."},
		wholesalepack.EmailRevoked:  {"Your trade account at {{fields.storeName}}", "{{fields.companyName}} no longer has trade prices."},
	}
	for kind, b := range bodies {
		s.putTemplate(t, eng, kind, b[0], b[1], "ready")
	}
	settings := `builtin wholesaleUpdateSettings(siteId: ` + quote(s.site) + `, storeId: ` + quote(s.store) +
		`, senderIdentityId: ` + quote(sender) + `, termsTemplateId: ` + quote(s.templateFor(wholesalepack.EmailTerms)) +
		`, approvedTemplateId: ` + quote(s.templateFor(wholesalepack.EmailApproved)) +
		`, rejectedTemplateId: ` + quote(s.templateFor(wholesalepack.EmailRejected)) +
		`, revokedTemplateId: ` + quote(s.templateFor(wholesalepack.EmailRevoked))
	if termsRequired {
		settings += `, termsRequired: true`
	}
	// THE OPERATORS CHOOSE THE SENDER; an operator holds no part, so the call
	// comes as trusted server code acting as one.
	wsExec(t, eng, auth.ContextWithInternalOrigin(asOperator("operator-"+s.site)), settings+`, staffEmail: "desk@example.com")`)
	return s
}

// putTemplate writes one version of the store's template of kind.
func (s emailStore) putTemplate(t *testing.T, eng *memql.MemQLEngine, kind, subject, text, status string) {
	t.Helper()
	wsExec(t, eng, seedCtx(), rawInsert("v1:campaigns:template", s.templateFor(kind), map[string]any{
		"ownerUserId": s.owner, "accountId": s.account, "name": "Wholesale " + kind, "subject": subject,
		"textBody": text, "htmlBody": "<p>" + text + "</p>", "status": status, "purpose": "transactional"}))
}

// automationCall is the store's automation calling one of the pack's sends:
// internal origin, as the scheduler's own actor.
func automationCall(t *testing.T, eng *memql.MemQLEngine, call string) map[string]any {
	t.Helper()
	return answerOf(t, eng, auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "automation-runner")), call)
}

// sendCall is a send builtin's call text on the store's application.
func (s emailStore) sendCall(builtin, kind string) string {
	call := `builtin ` + builtin + `(applicationId: ` + quote(s.appID)
	if kind != "" {
		call += `, kind: ` + quote(kind)
	}
	return call + `)`
}

// linkToken is the token the one message's link carries.
func linkToken(t *testing.T, msg email.Message, path string) string {
	t.Helper()
	i := strings.Index(msg.TextBody, path+"?t=")
	if i < 0 {
		t.Fatalf("the message carries no %s link: %q", path, msg.TextBody)
	}
	token := msg.TextBody[i+len(path)+3:]
	if j := strings.IndexAny(token, " )\n"); j >= 0 {
		token = token[:j]
	}
	return token
}

// requireNoTokenStored fails when the email's row, or any log line of the
// send, carries the token.
func (s emailStore) requireNoTokenStored(t *testing.T, kind string, attempt int, token string) {
	t.Helper()
	row := latestPayload(t, "v1:wholesale:emailSend", wholesalepack.EmailSendRowID(kind, s.appID, attempt))
	raw, _ := json.Marshal(row)
	if strings.Contains(string(raw), token) {
		t.Fatalf("the emailSend row carries the token: %s", raw)
	}
	if lines := s.logs.lines(token); lines != "" {
		t.Fatalf("a log line carries the token:\n%s", lines)
	}
	if lines := s.logs.lines("sam@"); lines != "" {
		t.Fatalf("a log line carries the applicant's address:\n%s", lines)
	}
}

// THE TERMS EMAIL REACHES THE LANE: one message to the applicant, carrying the
// link the resolver minted when the lane handed it over and the day it ends;
// the email's row says sent and holds no token; a second call sends nothing;
// and the application shows the email with the lane's own record of it.
func TestTheTermsEmailReachesTheLaneAndItsDeliveryShowsOnTheApplication(t *testing.T) {
	eng, s := newEmailStore(t, "mail-terms", true)
	got := automationCall(t, eng, s.sendCall("wholesaleSendTermsEmail", ""))
	if got["state"] != wholesalepack.SendSent || got["attempt"] != float64(1) || got["requestId"] != wholesalepack.LaneRequestID("terms", s.appID, 1) {
		t.Fatalf("the send answered %v", got)
	}
	sent := s.mail.messages()
	if len(sent) != 1 || sent[0].To != "sam@mail-terms.example.com" {
		t.Fatalf("%d messages sent, the first to %q", len(sent), func() string {
			if len(sent) > 0 {
				return sent[0].To
			}
			return ""
		}())
	}
	if !strings.Contains(sent[0].TextBody, "https://"+s.site+".example.test/wholesale/terms?t=") || !strings.Contains(sent[0].Subject, s.site) {
		t.Fatalf("the message is %q / %q", sent[0].Subject, sent[0].TextBody)
	}
	token := linkToken(t, sent[0], "/wholesale/terms")
	link := latestPayload(t, "v1:wholesale:privateLink", wholesalepack.PrivateLinkRowID(wholesalepack.LinkPurposeTerms, s.appID))
	if link["tokenHash"] != sha256Text(token) {
		t.Fatalf("the link row does not hold the hash of the token mailed")
	}
	if code, _ := readTerms(t, eng, s.accessStore, token); code != http.StatusOK {
		t.Fatalf("the mailed link answered %d", code)
	}
	row := latestPayload(t, "v1:wholesale:emailSend", wholesalepack.EmailSendRowID("terms", s.appID, 1))
	if row["state"] != wholesalepack.SendSent || row["templateId"] != s.templateFor("terms") || bareID(asString(row["accountId"])) != s.account {
		t.Fatalf("the email's row is %v", row)
	}
	s.requireNoTokenStored(t, "terms", 1, token)

	// A RE-DELIVERED EVENT SENDS NOTHING and mints nothing.
	if again := automationCall(t, eng, s.sendCall("wholesaleSendTermsEmail", "")); again["state"] != wholesalepack.SendSent || len(s.mail.messages()) != 1 {
		t.Fatalf("the second call answered %v after %d messages", again, len(s.mail.messages()))
	}
	if versionCount(t, "v1:wholesale:privateLink", wholesalepack.PrivateLinkRowID(wholesalepack.LinkPurposeTerms, s.appID)) != 1 {
		t.Fatal("the second call minted a link")
	}

	application := answerOf(t, eng, signedIn(s.staff), `builtin wholesaleGet(applicationId: `+quote(s.appID)+`)`)
	emails, _ := application["emails"].([]any)
	if len(emails) != 1 {
		t.Fatalf("the application shows %v", application["emails"])
	}
	e, _ := emails[0].(map[string]any)
	delivery, _ := e["delivery"].(map[string]any)
	if e["kind"] != "terms" || e["state"] != wholesalepack.SendSent || delivery["status"] != "sent" {
		t.Fatalf("the application shows the email %v", e)
	}
	if acts, _ := json.Marshal(application["acts"]); string(acts) != `["reject","resendTerms"]` {
		t.Fatalf("acts = %s", acts)
	}

	// A RESEND is attempt two, with a new link that ends the old one.
	resent := answerOf(t, eng, signedIn(s.staff), `builtin wholesaleResendEmail(applicationId: `+quote(s.appID)+`, kind: "terms")`)
	if resent["attempt"] != float64(2) || resent["state"] != wholesalepack.SendSent || len(s.mail.messages()) != 2 {
		t.Fatalf("the resend answered %v", resent)
	}
	fresh := linkToken(t, s.mail.messages()[1], "/wholesale/terms")
	if code, body := readTerms(t, eng, s.accessStore, token); code != http.StatusUnprocessableEntity || body["error"] != wholesalepack.RefusalLinkInvalid {
		t.Fatalf("the old link after the resend answered %d %v", code, body)
	}
	if code, _ := readTerms(t, eng, s.accessStore, fresh); code != http.StatusOK {
		t.Fatalf("the new link answered %d", code)
	}
	s.requireNoTokenStored(t, "terms", 2, fresh)
}

// A PRICE-SHEET LINK STOPS OPENING ONCE THE BUYER IS REMOVED. The approval
// email waits for granted access; two nodes sending it at once send one email
// with one link; the link opens the price sheet while the buyer is approved
// and granted, and once a revoke decision removes them (Task 5's
// wholesaleRecordDecision) it opens nothing, and the removal email is sent. A
// template unpublished meanwhile is a hold, recorded with the lane's sentence.
func TestAPriceSheetLinkStopsOpeningOnceTheBuyerIsRemoved(t *testing.T) {
	eng, s := newEmailStore(t, "mail-sheet", false)
	if err := s.approve(eng, signedIn(s.staff)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got := automationCall(t, eng, s.sendCall("wholesaleSendBuyerEmail", "approved")); got["state"] != wholesalepack.SendSkipped ||
		got["reason"] != wholesalepack.ReasonAccessNotGranted || len(s.mail.messages()) != 0 {
		t.Fatalf("before the grant the approval email answered %v", got)
	}
	// A GRANTED ENTITLEMENT, seeded at internal origin in test code only: the
	// test engine has no Shopify connector to grant one.
	wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:entitlement", wholesalepack.EntitlementRowID(s.appID), map[string]any{
		"ownerUserId": s.owner, "applicationId": "v1:wholesale:application:" + s.appID, "storeId": s.store,
		"buyerEmail": "sam@mail-sheet.example.com", "adapter": wholesalepack.AdapterCustomerTag,
		"state": wholesalepack.EntitlementGranted, "accountId": s.account, "direction": "grant", "attempt": 1}))

	// TWO NODES AT ONCE: both pass the attempt check; the lane's receipt lets one
	// message through, and one link is minted.
	var wg sync.WaitGroup
	answers := make([]map[string]any, 2)
	for i := range answers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := eng.Execute(auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "automation-runner")),
				s.sendCall("wholesaleSendBuyerEmail", "approved"))
			if err != nil {
				t.Errorf("send %d: %v", i, err)
				return
			}
			if rows := memql.MaterializeRows(result); len(rows) == 1 {
				answers[i] = rows[0]
			}
		}(i)
	}
	wg.Wait()
	sent := s.mail.messages()
	if len(sent) != 1 || sent[0].To != "sam@mail-sheet.example.com" {
		t.Fatalf("%d approval emails sent, want one; answers %v", len(sent), answers)
	}
	sheetLink := wholesalepack.PrivateLinkRowID(wholesalepack.LinkPurposePriceSheet, s.appID)
	if n := versionCount(t, "v1:wholesale:privateLink", sheetLink); n != 1 {
		t.Fatalf("%d price-sheet links minted, want one", n)
	}
	// THE RECORD ADMITS ONE HAND-OVER: one call answers sent; the other answers
	// attempt 1 as the row stood when it read it -- sent, or still on its way.
	sentAnswers := 0
	for i, a := range answers {
		switch {
		case a["state"] == wholesalepack.SendSent:
			sentAnswers++
		case a["state"] == wholesalepack.SendDeferred && a["attempt"] == float64(1):
		default:
			t.Fatalf("send %d answered %v", i, a)
		}
	}
	if sentAnswers < 1 {
		t.Fatalf("no call answered the email sent: %v", answers)
	}
	token := linkToken(t, sent[0], "/wholesale/price-sheet")
	s.requireNoTokenStored(t, "approved", 1, token)
	if code, body := shopperGet(t, eng, s.accessStore, "priceSheet", token); code != http.StatusOK || !strings.Contains(body, "Frame 410.00") {
		t.Fatalf("the mailed price-sheet link answered %d %s", code, body)
	}

	// REMOVED: a revoke decision, as wholesaleRecordDecision records it.
	if _, err := eng.Execute(signedIn(s.staff), `builtin wholesaleRecordDecision(applicationId: `+quote(s.appID)+
		`, transition: "revoke", note: "account closed")`); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if code, body := shopperGet(t, eng, s.accessStore, "priceSheet", token); code != http.StatusUnprocessableEntity || !strings.Contains(body, wholesalepack.RefusalLinkClosed) {
		t.Fatalf("after the removal the price-sheet link answered %d %s", code, body)
	}
	// THE REMOVAL EMAIL'S TEMPLATE IS UNPUBLISHED MEANWHILE: the lane holds it,
	// the row says what to fix, the automation is answered that it was not sent,
	// and nothing is sent.
	s.putTemplate(t, eng, wholesalepack.EmailRevoked, "Your trade account", "No longer.", "draft")
	held := automationCall(t, eng, s.sendCall("wholesaleSendBuyerEmail", "revoked"))
	if held["state"] != wholesalepack.SendHeld || held["sent"] != false || held["reason"] != wholesalepack.ReasonHeldForAutomation ||
		strings.Contains(asString(held["reason"]), "Your trade account") || len(s.mail.messages()) != 1 {
		t.Fatalf("a held removal email answered %v after %d messages", held, len(s.mail.messages()))
	}
	row := latestPayload(t, "v1:wholesale:emailSend", wholesalepack.EmailSendRowID("revoked", s.appID, 1))
	if row["state"] != wholesalepack.SendHeld || !strings.Contains(asString(row["reason"]), "is not a published transactional template") {
		t.Fatalf("the held email's row is %v", row)
	}
	application := answerOf(t, eng, signedIn(s.staff), `builtin wholesaleGet(applicationId: `+quote(s.appID)+`)`)
	if acts, _ := json.Marshal(application["acts"]); !strings.Contains(string(acts), `"resendRevoked"`) {
		t.Fatalf("a held removal email offers acts %s", acts)
	}
	// PUBLISHED AGAIN, a person of the store sends it once, as attempt two.
	s.putTemplate(t, eng, wholesalepack.EmailRevoked, "Your trade account", "No longer.", "ready")
	resent := answerOf(t, eng, signedIn(s.staff), `builtin wholesaleResendEmail(applicationId: `+quote(s.appID)+`, kind: "revoked")`)
	if resent["state"] != wholesalepack.SendSent || resent["attempt"] != float64(2) || len(s.mail.messages()) != 2 ||
		s.mail.messages()[1].To != "sam@mail-sheet.example.com" {
		t.Fatalf("the resent removal email answered %v after %d messages", resent, len(s.mail.messages()))
	}
	if _, err := eng.Execute(signedIn(s.staff), `builtin wholesaleResendEmail(applicationId: `+quote(s.appID)+`, kind: "revoked")`); err == nil ||
		len(s.mail.messages()) != 2 {
		t.Fatalf("a second resend of a sent removal email answered %v after %d messages", err, len(s.mail.messages()))
	}
	if lines := s.logs.lines("sam@"); lines != "" {
		t.Fatalf("a log line carries the applicant's address:\n%s", lines)
	}
}

// A STAFF NOTICE IS STAGED ONCE IN THE REAL OUTBOX: the server staging, at the
// store's address, with the event's own subject filled; a re-delivered event
// stages nothing new, and an event that does not match the application is
// refused and stages nothing.
func TestAStaffNoticeIsStagedOnceInTheOutbox(t *testing.T) {
	eng, s := newEmailStore(t, "mail-notice", false)
	call := `builtin wholesaleNotifyStaff(applicationId: ` + quote(s.appID) + `, event: "readyToReview")`
	got := automationCall(t, eng, call)
	want := wholesalepack.StaffRequestID(wholesalepack.StaffEventReadyToReview, s.appID, "")
	if got["status"] != "staged" || got["requestId"] != want {
		t.Fatalf("the notice answered %v", got)
	}
	row := latestPayload(t, "v1:platform:outboundRequest", want)
	if row["target"] != "desk@example.com" || row["medium"] != "email" || row["status"] != "pending" || row["serverStaged"] != true ||
		!strings.Contains(asString(row["subject"]), "Trading mail-notice") || row["requestedBy"] != "wholesale:staffNotice" {
		t.Fatalf("the staged row is %v", row)
	}
	if again := automationCall(t, eng, call); again["alreadyStaged"] != true || versionCount(t, "v1:platform:outboundRequest", want) != 1 {
		t.Fatalf("the re-delivered event answered %v", again)
	}
	if _, err := eng.Execute(auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "automation-runner")),
		`builtin wholesaleNotifyStaff(applicationId: `+quote(s.appID)+`, event: "accessNotGranted")`); err == nil {
		t.Fatal("an access notice for an application with no access record was staged")
	}
	if _, err := eng.Execute(signedIn(s.staff), call); err == nil {
		t.Fatal("a person's call staged a notice")
	}
}

// TWO NODES TELL ONE EVENT AT ONCE, through the real outbox: one notice is
// staged, both calls answer it, and once the outbox has sent it a further call
// leaves it sent and stages nothing.
func TestTwoNodesAtOnceStageOneStaffNoticeAndASentOneStaysSent(t *testing.T) {
	eng, s := newEmailStore(t, "mail-race", false)
	call := `builtin wholesaleNotifyStaff(applicationId: ` + quote(s.appID) + `, event: "readyToReview")`
	want := wholesalepack.StaffRequestID(wholesalepack.StaffEventReadyToReview, s.appID, "")
	var wg sync.WaitGroup
	answers := make([]map[string]any, 4)
	start := make(chan struct{})
	for i := range answers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			result, err := eng.Execute(auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "automation-runner")), call)
			if err != nil {
				t.Errorf("notice %d: %v", i, err)
				return
			}
			if rows := memql.MaterializeRows(result); len(rows) == 1 {
				answers[i] = rows[0]
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if n := versionCount(t, "v1:platform:outboundRequest", want); n != 1 {
		t.Fatalf("%d versions of the notice staged by four calls at once, want one; answers %v", n, answers)
	}
	fresh := 0
	for i, a := range answers {
		if a["status"] != "staged" || a["requestId"] != want {
			t.Fatalf("notice %d answered %v", i, a)
		}
		if a["alreadyStaged"] == false {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d calls answered that they staged the notice, want one: %v", fresh, answers)
	}

	// THE OUTBOX SENDS IT (as its worker records a delivery: a new version, sent).
	row := latestPayload(t, "v1:platform:outboundRequest", want)
	row["status"] = "sent"
	row["attempts"] = 1
	delete(row, "id")
	wsExec(t, eng, seedCtx(), rawInsert("v1:platform:outboundRequest", want, row))
	if again := automationCall(t, eng, call); again["alreadyStaged"] != true {
		t.Fatalf("a call after the notice was sent answered %v", again)
	}
	if got := latestPayload(t, "v1:platform:outboundRequest", want); got["status"] != "sent" || versionCount(t, "v1:platform:outboundRequest", want) != 2 {
		t.Fatalf("after a further call the notice is %v with %d versions, want it still sent", got,
			versionCount(t, "v1:platform:outboundRequest", want))
	}
	if lines := s.logs.lines("sam@"); lines != "" {
		t.Fatalf("a log line carries the applicant's address:\n%s", lines)
	}
}

// NOTHING IS MAILED ON UPGRADE FOR WHAT HAPPENED BEFORE IT. An application
// approved and granted before the pack sent emails -- its rows written without
// an attempt, a direction or an email row, and not yet tied to its
// organization -- is tied by the real account backfill. The store's automation
// on the versions the backfill re-wrote then asks for the approval email and
// the access notice, as an automation wired on every entitlement version would:
// no email reaches the lane, no notice is staged, and no email row is written.
// A retry of the buyer's access after the upgrade is attempt 1, and its
// approval email is sent once.
func TestNothingIsMailedOnUpgradeForAnAccessGrantedBeforeEmails(t *testing.T) {
	fakeAdapterOnce.Do(func() { wholesalepack.RegisterAdapter(fakeAdapter) })
	eng, s := newEmailStore(t, "mail-old", false)
	oldApp := "old-mail-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:application", oldApp, map[string]any{"ownerUserId": userRef(s.owner),
		"storeId": s.store, "siteId": s.site, "companyName": "Old Trading", "applicantName": "Pat Old",
		"applicantEmail": "pat@mail-old.example.com"}))
	wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:applicationDecision", "old-dec-"+oldApp, map[string]any{
		"applicationId": "v1:wholesale:application:" + oldApp, "storeId": s.store, "transition": "approve",
		"decidedBy": userRef(s.staff), "principalKind": "client", "note": "known to us"}))
	wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:entitlement", wholesalepack.EntitlementRowID(oldApp), map[string]any{
		"ownerUserId": userRef(s.owner), "applicationId": "v1:wholesale:application:" + oldApp, "storeId": s.store,
		"buyerEmail": "pat@mail-old.example.com", "adapter": wholesalepack.AdapterCustomerTag, "state": "granted", "reference": "wholesale"}))

	report := answerOf(t, eng, asOperator("operator-"+s.site), `builtin wholesaleBackfillAccounts(storeId: `+quote(s.store)+`)`)
	if countOf(report["tied"]) < 3 {
		t.Fatalf("the backfill tied %v rows, want the application, its decision and its entitlement: %v", report["tied"], report)
	}
	ent := latestPayload(t, "v1:wholesale:entitlement", wholesalepack.EntitlementRowID(oldApp))
	if bareID(asString(ent["accountId"])) != s.account || ent["attempt"] != nil || ent["state"] != "granted" {
		t.Fatalf("the re-written entitlement is %v, want it tied, granted and still without an attempt", ent)
	}

	sendCall := `builtin wholesaleSendBuyerEmail(applicationId: ` + quote(oldApp) + `, kind: "approved")`
	got := automationCall(t, eng, sendCall)
	if got["state"] != wholesalepack.SendSkipped || got["recorded"] != false || got["reason"] != wholesalepack.ReasonAccessPredatesEmail {
		t.Fatalf("the approval email for an access granted before emails answered %v", got)
	}
	notice := automationCall(t, eng, `builtin wholesaleNotifyStaff(applicationId: `+quote(oldApp)+`, event: "accessNotGranted")`)
	if notice["status"] != "skipped" || notice["reason"] != wholesalepack.ReasonAccessPredatesEmail {
		t.Fatalf("the access notice for an access from before emails answered %v", notice)
	}
	if n := len(s.mail.messages()); n != 0 {
		t.Fatalf("%d emails reached the lane for an access granted before emails", n)
	}
	for _, attempt := range []int{0, 1} {
		if n := versionCount(t, "v1:platform:outboundRequest",
			wholesalepack.AccessStaffRequestID(wholesalepack.StaffEventAccessNotGranted, oldApp, attempt)); n != 0 {
			t.Fatalf("an access notice was staged for attempt %d", attempt)
		}
	}
	if n := versionCount(t, "v1:wholesale:emailSend", wholesalepack.EmailSendRowID(wholesalepack.EmailApproved, oldApp, 1)); n != 0 {
		t.Fatalf("%d email rows were written for an access granted before emails", n)
	}

	// A RETRY AFTER THE UPGRADE, through an adapter that reaches no store.
	openApplications(t, eng, asSiteOwner(s.owner), s.store, automationFakeAdapter)
	if _, err := eng.Execute(signedIn(s.staff), `builtin wholesaleRetryAccess(applicationId: `+quote(oldApp)+`)`); err != nil {
		t.Fatalf("retry: %v", err)
	}
	ent = latestPayload(t, "v1:wholesale:entitlement", wholesalepack.EntitlementRowID(oldApp))
	if ent["state"] != "granted" || ent["attempt"] != float64(1) || ent["direction"] != "grant" {
		t.Fatalf("the retry wrote %v, want attempt 1, a granted grant", ent)
	}
	if got := automationCall(t, eng, sendCall); got["state"] != wholesalepack.SendSent || got["attempt"] != float64(1) {
		t.Fatalf("the approval email after the retry answered %v", got)
	}
	automationCall(t, eng, sendCall) // the automation on the next version
	if sent := s.mail.messages(); len(sent) != 1 || sent[0].To != "pat@mail-old.example.com" {
		t.Fatalf("%d approval emails after the retry, want one to the stored address", len(sent))
	}
	if lines := s.logs.lines("pat@"); lines != "" {
		t.Fatalf("a log line carries the applicant's address:\n%s", lines)
	}
}

// THE SETTINGS ANSWER THE SENDER AS THE EMAILS GO OUT AS, through the real row
// lane, and MAY PUBLISH AND MAY EXPORT AGREE WITH THE BUILTINS for a person of
// the store holding the part, one of its people who does not, and a cluster
// operator.
func TestTheSettingsAnswerTheSenderAndWhatTheCallerMayPublishAndExport(t *testing.T) {
	eng, s := newEmailStore(t, "mail-may", false)
	store := `siteId: ` + quote(s.site) + `, storeId: ` + quote(s.store)
	settings := `builtin wholesaleStoreSettings(` + store + `)`
	got := answerOf(t, eng, signedIn(s.staff), settings)
	sender, _ := got["sender"].(map[string]any)
	if sender["ready"] != true || sender["fromName"] != "Kestrel Trade" || sender["address"] != "trade@example.com" || sender["reason"] != "" {
		t.Fatalf("the chosen sender reads %v", sender)
	}

	without := "user-nopart-" + s.site
	group := "group-nopart-" + s.site
	seedGroup(t, eng, group, "custom", s.account)
	seedPersonIn(t, eng, without, without, "writer", true, group, s.account, "added")
	publish := `builtin wholesalePublishDocument(` + store + `, kind: "terms", label: "Terms 2027", body: "Net 45.")`
	export := `builtin wholesaleExportStore(` + store + `)`
	for who, ctx := range map[string]context.Context{
		"a person holding the part": signedIn(s.staff), "a person without it": signedIn(without),
		"a cluster operator": asOperator("operator-" + s.site),
	} {
		_, publishErr := eng.Execute(ctx, publish)
		_, exportErr := eng.Execute(ctx, export)
		result, err := eng.Execute(ctx, settings)
		if err != nil {
			// The settings are refused: so must both acts be.
			if publishErr == nil || exportErr == nil {
				t.Fatalf("%s reads no settings (%v) yet publish answered %v and export %v", who, err, publishErr, exportErr)
			}
			continue
		}
		rows := memql.MaterializeRows(result)
		may, _ := rows[0]["may"].(map[string]any)
		if may["publish"] != (publishErr == nil) || may["export"] != (exportErr == nil) {
			t.Fatalf("%s: may %v, but publish answered %v and export %v", who, may, publishErr, exportErr)
		}
	}

	// NO SENDER CHOSEN: what a send with none chosen goes out as, in the lane's words.
	wsExec(t, eng, auth.ContextWithInternalOrigin(asOperator("operator-"+s.site)),
		`builtin wholesaleUpdateSettings(`+store+`, senderIdentityId: "")`)
	sender, _ = answerOf(t, eng, signedIn(s.staff), settings)["sender"].(map[string]any)
	if sender["ready"] != false || sender["address"] != "" || strings.TrimSpace(asString(sender["reason"])) == "" {
		t.Fatalf("no sender chosen reads %v", sender)
	}
	// AND EVERY EMAIL SAYS IT WOULD NOT BE SENT, with the sentence a send declines with.
	would, _ := answerOf(t, eng, signedIn(s.staff), settings)["emailsWouldSend"].(map[string]any)
	approved, _ := would[wholesalepack.EmailApproved].(map[string]any)
	if approved["wouldSend"] != false || approved["reason"] != wholesalepack.NoSenderChosenReason {
		t.Fatalf("with no sender chosen the approval email reads %v", approved)
	}
	if _, err := eng.Execute(signedIn(s.staff), `builtin wholesaleRecordDecision(applicationId: `+quote(s.appID)+
		`, transition: "reject", note: "not a trade buyer")`); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got := automationCall(t, eng, s.sendCall("wholesaleSendBuyerEmail", "rejected")); got["state"] != wholesalepack.SendSkipped ||
		got["recorded"] != false || got["reason"] != wholesalepack.NoSenderChosenReason || len(s.mail.messages()) != 0 {
		t.Fatalf("a rejection with no sender chosen answered %v after %d messages", got, len(s.mail.messages()))
	}
	if n := versionCount(t, "v1:wholesale:emailSend", wholesalepack.EmailSendRowID(wholesalepack.EmailRejected, s.appID, 1)); n != 0 {
		t.Fatalf("%d email rows written for a store with no sender", n)
	}
}
