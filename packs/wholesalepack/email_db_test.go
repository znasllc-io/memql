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

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

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
	// refuse, when set, is the provider's answer to a message in place of taking it.
	refuse func(email.Message) error
}

func (m *recordingMail) Send(_ context.Context, msg email.Message, _ email.SendAs) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refuse != nil {
		if err := m.refuse(msg); err != nil {
			return err
		}
	}
	m.sent = append(m.sent, msg)
	return nil
}

// refuseWith makes the provider answer every message from now on with refuse.
func (m *recordingMail) refuseWith(refuse func(email.Message) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refuse = refuse
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

	// THE PROVIDER REFUSES THE NEXT ONE, IN WORDS THAT QUOTE THE APPLICANT'S
	// ADDRESS, as a mail server's refusal does. What a person reads is the lane's
	// sentence for a message the provider did not take: on the resend's reply, on
	// the email's row, on the application's page and in the export, and none of
	// the four carries the provider's words or the address. The lane's own record
	// of the delivery keeps them; no log line does.
	s.mail.refuseWith(func(msg email.Message) error {
		return &email.SendError{StatusCode: 550, Permanent: true, NotAccepted: true,
			Detail: "550 5.1.1 <" + msg.To + ">: Recipient address rejected"}
	})
	sentence := campaigns.RowSendSentence(campaigns.RowSendProviderRefused)
	refused := answerOf(t, eng, signedIn(s.staff), `builtin wholesaleResendEmail(applicationId: `+quote(s.appID)+`, kind: "terms")`)
	if refused["attempt"] != float64(3) || refused["state"] != wholesalepack.SendFailed || refused["sent"] != false ||
		refused["reason"] != sentence || len(s.mail.messages()) != 2 {
		t.Fatalf("the resend the provider refused answered %v after %d messages, want attempt 3 failed with %q", refused, len(s.mail.messages()), sentence)
	}
	failedRow := latestPayload(t, "v1:wholesale:emailSend", wholesalepack.EmailSendRowID("terms", s.appID, 3))
	application = answerOf(t, eng, signedIn(s.staff), `builtin wholesaleGet(applicationId: `+quote(s.appID)+`)`)
	shown := emailOf(t, application, wholesalepack.EmailTerms, 3)
	var exported struct {
		EmailSends []map[string]any `json:"emailSends"`
	}
	export := answerOf(t, eng, signedIn(s.staff), `builtin wholesaleExportStore(siteId: `+quote(s.site)+`, storeId: `+quote(s.store)+`)`)
	if err := json.Unmarshal([]byte(asString(export["json"])), &exported); err != nil || len(exported.EmailSends) != 3 {
		t.Fatalf("the export's emails: %v %v", err, exported.EmailSends)
	}
	if failedRow["state"] != wholesalepack.SendFailed || failedRow["reason"] != sentence || shown["state"] != wholesalepack.SendFailed || shown["reason"] != sentence {
		t.Fatalf("the refused email's row is %v and the page shows %v, want both failed with %q", failedRow, shown, sentence)
	}
	for where, value := range map[string]any{"the resend's reply": refused, "the email's row": failedRow,
		"the application's page": application["emails"], "the export": exported.EmailSends} {
		raw, _ := json.Marshal(value)
		for _, never := range []string{"sam@", "Recipient address rejected", "550 5.1.1", "campaigns.", "permanent failure"} {
			if strings.Contains(string(raw), never) {
				t.Fatalf("%s carries %q: %s", where, never, raw)
			}
		}
	}
	if _, ledger := laneDelivery(t, wholesalepack.LaneRequestID("terms", s.appID, 3)); ledger["status"] != "failed" ||
		!strings.Contains(asString(ledger["lastError"]), "Recipient address rejected") {
		t.Fatalf("the lane's own record of the delivery does not keep the provider's answer: %v", ledger)
	}
	if lines := s.logs.lines("sam@"); lines != "" {
		t.Fatalf("a log line carries the applicant's address:\n%s", lines)
	}
	if lines := s.logs.lines("Recipient address rejected"); lines != "" {
		t.Fatalf("a log line carries the provider's words:\n%s", lines)
	}
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

	// THE PROVIDER LATER REPORTS IT BOUNCED (its report lands on the lane's own
	// record of the delivery): the email stays sent -- handed to the email
	// service -- with the report beside it, and since it did not reach the buyer
	// the page offers to send it again.
	second := wholesalepack.LaneRequestID(wholesalepack.EmailRevoked, s.appID, 2)
	ledgerID, ledger := laneDelivery(t, second)
	ledger["providerStatus"] = "bounced"
	wsExec(t, eng, seedCtx(), rawInsert("v1:campaigns:delivery", ledgerID, ledger))
	application = answerOf(t, eng, signedIn(s.staff), `builtin wholesaleGet(applicationId: `+quote(s.appID)+`)`)
	bounced := emailOf(t, application, wholesalepack.EmailRevoked, 2)
	if d, _ := bounced["delivery"].(map[string]any); bounced["state"] != wholesalepack.SendSent || d["providerStatus"] != "bounced" {
		t.Fatalf("a removal email the provider reported bounced shows %v, want it sent with the report beside it", bounced)
	}
	if acts, _ := json.Marshal(application["acts"]); !strings.Contains(string(acts), `"resendRevoked"`) {
		t.Fatalf("a bounced removal email offers acts %s", acts)
	}
	// THE ADDRESS IS NOW ONE THE EMAIL SERVICE DOES NOT SEND TO for this
	// organization: the resend is the next attempt, recorded skipped, and its
	// reason is the lane's sentence -- on the reply, on the row and on the page --
	// with the lane's code beside it in the delivery, never in the reason.
	if err := campaigns.NewStore(campaignsEngine{eng}).RecordOrganizationSuppression(seedCtx(), s.account,
		campaigns.EmailDigest("sam@mail-sheet.example.com"), "manual", "example.com", "", ""); err != nil {
		t.Fatalf("block the address: %v", err)
	}
	sentence := campaigns.SkipSentence("manual")
	third := answerOf(t, eng, signedIn(s.staff), `builtin wholesaleResendEmail(applicationId: `+quote(s.appID)+`, kind: "revoked")`)
	if third["attempt"] != float64(3) || third["state"] != wholesalepack.SendSkipped || third["sent"] != false ||
		third["reason"] != sentence || len(s.mail.messages()) != 2 {
		t.Fatalf("the resend to a blocked address answered %v after %d messages, want attempt 3 skipped with %q", third, len(s.mail.messages()), sentence)
	}
	if row := latestPayload(t, "v1:wholesale:emailSend", wholesalepack.EmailSendRowID("revoked", s.appID, 3)); row["reason"] != sentence {
		t.Fatalf("the skipped email's row is %v", row)
	}
	application = answerOf(t, eng, signedIn(s.staff), `builtin wholesaleGet(applicationId: `+quote(s.appID)+`)`)
	skipped := emailOf(t, application, wholesalepack.EmailRevoked, 3)
	if d, _ := skipped["delivery"].(map[string]any); skipped["state"] != wholesalepack.SendSkipped || skipped["reason"] != sentence ||
		d["skipReason"] != "manual" || d["skipSentence"] != sentence {
		t.Fatalf("the skipped email shows %v, want the lane's sentence as its reason and the code beside it", skipped)
	}
	if lines := s.logs.lines("sam@"); lines != "" {
		t.Fatalf("a log line carries the applicant's address:\n%s", lines)
	}
}

// emailOf is the application's email of kind and attempt, as wholesaleGet shows it.
func emailOf(t *testing.T, application map[string]any, kind string, attempt int) map[string]any {
	t.Helper()
	emails, _ := application["emails"].([]any)
	for _, e := range emails {
		if m, _ := e.(map[string]any); m["kind"] == kind && m["attempt"] == float64(attempt) {
			return m
		}
	}
	t.Fatalf("the application shows no %s email attempt %d: %v", kind, attempt, application["emails"])
	return nil
}

// laneDelivery is the row lane's own record of the message sent under
// requestID, newest version: its row id and its payload, read straight from
// the table.
func laneDelivery(t *testing.T, requestID string) (string, map[string]any) {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	defer db.Close()
	var id string
	var raw []byte
	if err := db.NewRaw(`SELECT id, payload FROM "MemoryNodes" WHERE concept = 'v1:campaigns:delivery' AND payload->>'requestId' = ? `+
		`ORDER BY "createdAt" DESC LIMIT 1`, requestID).Scan(context.Background(), &id, &raw); err != nil {
		t.Fatalf("read the lane's delivery of %s: %v", requestID, err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode the lane's delivery of %s: %v", requestID, err)
	}
	return bareID(id), out
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
	// THE REJECTION IS DUE AND THE STORE DECLINES IT: it is RECORDED -- attempt 1,
	// skipped, with the sentence, the lane not asked -- and of four calls at once
	// one row is written, which every call answers.
	rejectedRow := wholesalepack.EmailSendRowID(wholesalepack.EmailRejected, s.appID, 1)
	var wg sync.WaitGroup
	answers := make([]map[string]any, 4)
	for i := range answers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := eng.Execute(auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "automation-runner")),
				s.sendCall("wholesaleSendBuyerEmail", "rejected"))
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
	for i, got := range answers {
		if got["state"] != wholesalepack.SendSkipped || got["recorded"] != true || got["sent"] != false || got["attempt"] != float64(1) ||
			got["reason"] != wholesalepack.NoSenderChosenReason {
			t.Fatalf("call %d for a rejection with no sender chosen answered %v", i, got)
		}
	}
	if n := versionCount(t, "v1:wholesale:emailSend", rejectedRow); n != 1 || len(s.mail.messages()) != 0 {
		t.Fatalf("%d versions of the declined attempt and %d messages, want one version and no message", n, len(s.mail.messages()))
	}
	// THE PAGE LISTS IT, and offers no resend while no sender is chosen; one asked
	// for is refused with the sentence.
	application := answerOf(t, eng, signedIn(s.staff), `builtin wholesaleGet(applicationId: `+quote(s.appID)+`)`)
	if e := emailOf(t, application, wholesalepack.EmailRejected, 1); e["state"] != wholesalepack.SendSkipped ||
		e["reason"] != wholesalepack.NoSenderChosenReason {
		t.Fatalf("the page shows the declined rejection as %v", e)
	}
	if acts, _ := json.Marshal(application["acts"]); strings.Contains(string(acts), "resendRejected") {
		t.Fatalf("with no sender chosen the page offers %s", acts)
	}
	resend := `builtin wholesaleResendEmail(applicationId: ` + quote(s.appID) + `, kind: "rejected")`
	if _, err := eng.Execute(signedIn(s.staff), resend); err == nil || !strings.Contains(err.Error(), wholesalepack.NoSenderChosenReason) {
		t.Fatalf("a resend with no sender chosen answered %v", err)
	}
	// THE OPERATORS CHOOSE THE SENDER AGAIN: the act is offered, a person's resend
	// is attempt 2 and sends, and the automatic call made again sends nothing.
	wsExec(t, eng, auth.ContextWithInternalOrigin(asOperator("operator-"+s.site)),
		`builtin wholesaleUpdateSettings(`+store+`, senderIdentityId: `+quote("sender-"+s.site)+`)`)
	application = answerOf(t, eng, signedIn(s.staff), `builtin wholesaleGet(applicationId: `+quote(s.appID)+`)`)
	if acts, _ := json.Marshal(application["acts"]); string(acts) != `["resendRejected"]` {
		t.Fatalf("once a sender is chosen the page offers %s, want resendRejected", acts)
	}
	resent := answerOf(t, eng, signedIn(s.staff), resend)
	if resent["state"] != wholesalepack.SendSent || resent["attempt"] != float64(2) || len(s.mail.messages()) != 1 ||
		s.mail.messages()[0].To != "sam@mail-may.example.com" {
		t.Fatalf("the resend answered %v after %d messages", resent, len(s.mail.messages()))
	}
	again := automationCall(t, eng, s.sendCall("wholesaleSendBuyerEmail", "rejected"))
	if again["attempt"] != float64(1) || again["state"] != wholesalepack.SendSkipped || len(s.mail.messages()) != 1 ||
		versionCount(t, "v1:wholesale:emailSend", rejectedRow) != 1 {
		t.Fatalf("the automatic call after the resend answered %v after %d messages", again, len(s.mail.messages()))
	}
}

// A STORE'S EMAILS FOLLOW THE OWNER OF THE SITE THAT SERVES IT, through the real
// row lane and its real check of who may send for an organization. The
// application was posted, and the store's settings saved, through a site one
// person owns, and both rows name that person for good. Then another site of
// the organization, owned by another of its people, takes the store over: it is
// bound to the store and the first site to another. (Through the engine's own
// writes that is how the person changes: an organization's site keeps the owner
// it was created with -- the engine restores the stored owner of an
// organization's row on every write, an ownership transfer included.) The new
// owner reads the organization's data and may not change it:
//
//   - the settings say no email would be sent, in the pack's own sentence, which
//     names what is missing and who can give it;
//   - a rejection that falls due is recorded as not sent with that sentence,
//     and nothing reaches the provider; the application's page lists it and
//     offers no resend, and a resend asked for is refused with the sentence.
//
// Then the new owner is given write permission and the OLD owner loses it: the
// act is offered, a person's resend goes out, and the lane's own record of the
// delivery is the new owner's -- while the email's row stays the application's
// merchant's, as every row of an application is.
func TestAStoresEmailsFollowTheOwnerOfTheSiteThatServesItThroughTheRealLane(t *testing.T) {
	eng, s := newEmailStore(t, "mail-owner", false)
	get := `builtin wholesaleGet(applicationId: ` + quote(s.appID) + `)`
	resend := `builtin wholesaleResendEmail(applicationId: ` + quote(s.appID) + `, kind: "rejected")`
	// senderOf is the sender line and the rejection email's standing, as the
	// store's settings answer them through site.
	senderOf := func(site string) (map[string]any, map[string]any) {
		t.Helper()
		got := answerOf(t, eng, signedIn(s.staff), `builtin wholesaleStoreSettings(siteId: `+quote(site)+`, storeId: `+quote(s.store)+`)`)
		line, _ := got["sender"].(map[string]any)
		would, _ := got["emailsWouldSend"].(map[string]any)
		rejected, _ := would[wholesalepack.EmailRejected].(map[string]any)
		return line, rejected
	}
	setRole := func(user, role string) {
		t.Helper()
		row := latestPayload(t, "v1:identity:user", user)
		row["role"] = role
		delete(row, "id")
		wsExec(t, eng, seedCtx(), rawInsert("v1:identity:user", user, row))
	}
	if line, _ := senderOf(s.site); line["ready"] != true {
		t.Fatalf("before the store changes sites the sender reads %v", line)
	}

	// ANOTHER SITE TAKES THE STORE OVER, owned by a person of the organization
	// who reads its data and may not change it.
	oldOwner := bareID(s.owner)
	heir, heirs, next := "user-heir-"+s.site, "group-heirs-"+s.site, "next-"+s.site
	seedGroup(t, eng, heirs, "custom", s.account)
	seedPersonIn(t, eng, heir, heir, "reader", true, heirs, s.account, "added")
	seedSiteIn(t, eng, next, heir, s.store, s.account)
	elsewhere := s.store + "-elsewhere"
	wsExec(t, eng, seedCtx(), rawInsert("v1:shopify:store", elsewhere, map[string]any{"domain": elsewhere + ".myshopify.com"}))
	wsExec(t, eng, seedCtx(), rawInsert("v1:platform:site", s.site, map[string]any{"binding": map[string]any{"storeId": elsewhere}}))
	if got := siteOwnerOf(t, eng, next); bareID(got) != heir {
		t.Fatalf("the new site's owner reads %q", got)
	}
	for concept, row := range map[string]string{"v1:wholesale:application": s.appID, "v1:wholesale:wholesaleSettings": wholesalepack.SettingsRowID(s.store)} {
		if got := latestPayload(t, concept, row); bareID(asString(got["ownerUserId"])) != oldOwner {
			t.Fatalf("%s %s is owned by %v; the case needs it to keep naming the old owner", concept, row, got["ownerUserId"])
		}
	}

	const want = wholesalepack.NoSendAuthoritySentence
	if line, rejected := senderOf(next); line["ready"] != false || line["reason"] != want || rejected["wouldSend"] != false || rejected["reason"] != want {
		t.Fatalf("with a site owner who may not send, the sender reads %v and the rejection email %v; want both %q", line, rejected, want)
	}
	if _, err := eng.Execute(signedIn(s.staff), `builtin wholesaleRecordDecision(applicationId: `+quote(s.appID)+
		`, transition: "reject", note: "not a trade buyer")`); err != nil {
		t.Fatalf("reject: %v", err)
	}
	declined := automationCall(t, eng, s.sendCall("wholesaleSendBuyerEmail", "rejected"))
	if declined["state"] != wholesalepack.SendSkipped || declined["recorded"] != true || declined["attempt"] != float64(1) ||
		declined["reason"] != want || len(s.mail.messages()) != 0 {
		t.Fatalf("the due rejection answered %v after %d messages, want attempt 1 recorded as skipped with %q", declined, len(s.mail.messages()), want)
	}
	application := answerOf(t, eng, signedIn(s.staff), get)
	if e := emailOf(t, application, wholesalepack.EmailRejected, 1); e["state"] != wholesalepack.SendSkipped || e["reason"] != want {
		t.Fatalf("the page shows the attempt as %v", e)
	}
	if acts, _ := json.Marshal(application["acts"]); strings.Contains(string(acts), "resendRejected") {
		t.Fatalf("while the site's owner may not send the page offers %s", acts)
	}
	if _, err := eng.Execute(signedIn(s.staff), resend); err == nil || !strings.Contains(err.Error(), want) || len(s.mail.messages()) != 0 {
		t.Fatalf("a resend answered %v after %d messages, want it refused with the sentence", err, len(s.mail.messages()))
	}

	// THE NEW OWNER IS GIVEN WRITE PERMISSION, AND THE OLD OWNER LOSES IT.
	setRole(heir, "writer")
	setRole(oldOwner, "reader")
	if line, rejected := senderOf(next); line["ready"] != true || line["address"] != "trade@example.com" || rejected["wouldSend"] != true {
		t.Fatalf("once the site's owner may send, the sender reads %v and the rejection email %v", line, rejected)
	}
	application = answerOf(t, eng, signedIn(s.staff), get)
	if acts, _ := json.Marshal(application["acts"]); string(acts) != `["resendRejected"]` {
		t.Fatalf("once the site's owner may send the page offers %s, want resendRejected", acts)
	}
	resent := answerOf(t, eng, signedIn(s.staff), resend)
	if resent["state"] != wholesalepack.SendSent || resent["attempt"] != float64(2) || len(s.mail.messages()) != 1 ||
		s.mail.messages()[0].To != "sam@mail-owner.example.com" {
		t.Fatalf("the resend answered %v after %d messages", resent, len(s.mail.messages()))
	}
	// SENT AS THE NEW OWNER: the lane's own record of the delivery is theirs. The
	// email's row is the application's merchant's, as it always was.
	if _, ledger := laneDelivery(t, wholesalepack.LaneRequestID(wholesalepack.EmailRejected, s.appID, 2)); bareID(asString(ledger["ownerUserId"])) != heir {
		t.Fatalf("the lane's record of the delivery is owned by %v, want the owner of the site that serves the store", ledger["ownerUserId"])
	}
	row := latestPayload(t, "v1:wholesale:emailSend", wholesalepack.EmailSendRowID(wholesalepack.EmailRejected, s.appID, 2))
	if row["state"] != wholesalepack.SendSent || bareID(asString(row["ownerUserId"])) != oldOwner {
		t.Fatalf("the email's row is %v, want it sent and the application's merchant's", row)
	}
	if again := automationCall(t, eng, s.sendCall("wholesaleSendBuyerEmail", "rejected")); again["attempt"] != float64(1) ||
		again["state"] != wholesalepack.SendSkipped || len(s.mail.messages()) != 1 {
		t.Fatalf("the automatic call after the resend answered %v after %d messages", again, len(s.mail.messages()))
	}
	if lines := s.logs.lines("sam@"); lines != "" {
		t.Fatalf("a log line carries the applicant's address:\n%s", lines)
	}
}
