package wholesalepack_test

// refusals_db_test.go -- WHAT A STOREFRONT PAGE IS TOLD (ruling 62), against a
// real database, through the real shopper handler. A page knows a refusal only
// by its code, so each code must mean one thing:
//
//   - link_used: the terms were accepted through this application's link;
//   - link_closed: the link is real and its application is no longer, or not
//     at the moment, in the state the link serves;
//   - link_invalid: the token is no link of this store, whatever the reason;
//   - documents_unavailable: the store has no current document to show;
//   - applications_closed: the store takes no application through this site.
//
// Postgres-gated like every *_db_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// refusalOf is the code a private read answered, or "" for an answer that is
// not a refusal the read declared.
func refusalOf(t *testing.T, eng *memql.MemQLEngine, s accessStore, read, token string) string {
	t.Helper()
	code, body := shopperGet(t, eng, s, read, token)
	if code == http.StatusOK {
		return ""
	}
	var answer struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &answer)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("the %s read answered %d %s, want 422 with a declared code", read, code, body)
	}
	return answer.Error
}

// acceptanceReason is where an acceptance post of token is sent, as the page
// that receives it reads it: the reason, or "accepted".
func acceptanceReason(t *testing.T, eng *memql.MemQLEngine, s accessStore, token string) string {
	t.Helper()
	zeros := strings.Repeat("0", 64)
	form := url.Values{"requestToken": {token}, "acceptTerms": {"on"}, "acceptMap": {"on"}, "signedName": {"Ada Example"},
		"termsSha256": {zeros}, "mapSha256": {zeros}, "statementSha256": {sha256Text(wholesalepack.DefaultAcceptanceText)}}
	loc := postAcceptance(t, eng, s, form).Header().Get("Location")
	if loc == "/wholesale/accepted" {
		return "accepted"
	}
	if !strings.HasPrefix(loc, "/wholesale/link-problem?reason=") {
		t.Fatalf("the acceptance redirected to %q", loc)
	}
	return strings.TrimPrefix(loc, "/wholesale/link-problem?reason=")
}

// decideOn records one decision on s's application by its staff.
func decideOn(t *testing.T, eng *memql.MemQLEngine, s accessStore, transition string) {
	t.Helper()
	wsExec(t, eng, signedIn(s.staff), `builtin wholesaleRecordDecision(applicationId: `+quote(s.appID)+
		`, transition: `+quote(transition)+`, note: "")`)
}

// termsNotRequired lets s's application be approved without an acceptance.
func termsNotRequired(t *testing.T, eng *memql.MemQLEngine, s accessStore) {
	t.Helper()
	wsExec(t, eng, signedIn(s.staff), `builtin wholesaleUpdateSettings(siteId: `+quote(s.site)+`, storeId: `+quote(s.store)+
		`, termsRequired: false)`)
}

// grantAccess seeds a granted entitlement for s's application, at internal origin in
// test code only: the test engine has no Shopify connector to grant one.
func grantAccess(t *testing.T, eng *memql.MemQLEngine, s accessStore) {
	t.Helper()
	wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:entitlement", wholesalepack.EntitlementRowID(s.appID), map[string]any{
		"ownerUserId": s.owner, "applicationId": "v1:wholesale:application:" + s.appID, "storeId": s.store,
		"buyerEmail": "sam@" + s.store + ".example.com", "adapter": wholesalepack.AdapterCustomerTag,
		"state": wholesalepack.EntitlementGranted, "accountId": s.account}))
}

// publishKind publishes one document of kind on s's store.
func publishKind(t *testing.T, eng *memql.MemQLEngine, s accessStore, kind string) {
	t.Helper()
	answerOf(t, eng, signedIn(s.staff), `builtin wholesalePublishDocument(siteId: `+quote(s.site)+`, storeId: `+quote(s.store)+
		`, kind: `+quote(kind)+`, label: `+quote(kind+" 2026")+`, body: `+quote("The "+kind+" of the store.")+`)`)
}

// mintFor is a new link of purpose for s's application.
func mintFor(t *testing.T, eng *memql.MemQLEngine, s accessStore, purpose string) string {
	t.Helper()
	token, err := wholesalepack.MintLinkForTest(context.Background(), eng, s.appID, purpose)
	if err != nil {
		t.Fatalf("mint %s: %v", purpose, err)
	}
	return token
}

// rewriteTermsLink writes a new version of s's terms link carrying token, with
// expiresAt and usedAt as given and every other field as it stands.
func rewriteTermsLink(t *testing.T, eng *memql.MemQLEngine, s accessStore, token, expiresAt, usedAt string) {
	t.Helper()
	id := wholesalepack.PrivateLinkRowID(wholesalepack.LinkPurposeTerms, s.appID)
	link := latestPayload(t, "v1:wholesale:privateLink", id)
	wsExec(t, eng, seedCtx(), `putPrivateLinkRow(linkId: `+quote(id)+`, ownerUserId: `+quote(asString(link["ownerUserId"]))+
		`, storeId: `+quote(asString(link["storeId"]))+`, accountId: `+quote(asString(link["accountId"]))+
		`, siteId: `+quote(asString(link["siteId"]))+`, applicationId: `+quote(asString(link["applicationId"]))+
		`, purpose: "terms", tokenHash: `+quote(sha256Text(token))+`, issuedAt: `+quote(asString(link["issuedAt"]))+
		`, expiresAt: `+quote(expiresAt)+`, usedAt: `+quote(usedAt)+`)`)
}

// AN APPLICANT WHO ACCEPTED is told link_used on their terms link, and still
// is once the application has been approved: that is the truth they should
// hear, not that the link is closed.
func TestATermsLinkIsLinkUsedAfterItsAcceptanceAndAfterTheApproval(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, token := termsStore(t, eng, "used-approved")
	_, terms := readTerms(t, eng, s, token)
	if loc := postAcceptance(t, eng, s, acceptForm(token, terms)).Header().Get("Location"); loc != "/wholesale/accepted" {
		t.Fatalf("the acceptance redirected to %q", loc)
	}
	if got := refusalOf(t, eng, s, "terms", token); got != wholesalepack.RefusalLinkUsed {
		t.Fatalf("the terms read after the acceptance answered %q, want %s", got, wholesalepack.RefusalLinkUsed)
	}
	decideOn(t, eng, s, "approve")
	if got := refusalOf(t, eng, s, "terms", token); got != wholesalepack.RefusalLinkUsed {
		t.Fatalf("the terms read after the approval answered %q, want %s", got, wholesalepack.RefusalLinkUsed)
	}
	if got := acceptanceReason(t, eng, s, token); got != wholesalepack.RefusalLinkUsed {
		t.Fatalf("an acceptance after the approval answered %q, want %s", got, wholesalepack.RefusalLinkUsed)
	}
}

// A TERMS LINK OF A REJECTED APPLICATION is link_closed, on the read and on the
// acceptance: the link is real and its application is no longer waiting.
func TestATermsLinkOfARejectedApplicationIsLinkClosed(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, token := termsStore(t, eng, "closed-rejected")
	decideOn(t, eng, s, "reject")
	if got := refusalOf(t, eng, s, "terms", token); got != wholesalepack.RefusalLinkClosed {
		t.Fatalf("the terms read of a rejected application answered %q, want %s", got, wholesalepack.RefusalLinkClosed)
	}
	if got := acceptanceReason(t, eng, s, token); got != wholesalepack.RefusalLinkClosed {
		t.Fatalf("an acceptance of a rejected application answered %q, want %s", got, wholesalepack.RefusalLinkClosed)
	}
}

// A TERMS LINK OF AN APPLICATION APPROVED WITHOUT AN ACCEPTANCE is link_closed,
// and so it stays once the buyer is removed.
func TestATermsLinkOfAnApplicationApprovedWithoutAnAcceptanceOrRemovedIsLinkClosed(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, token := termsStore(t, eng, "closed-approved")
	termsNotRequired(t, eng, s)
	decideOn(t, eng, s, "approve")
	if got := refusalOf(t, eng, s, "terms", token); got != wholesalepack.RefusalLinkClosed {
		t.Fatalf("the terms read of an application approved without an acceptance answered %q, want %s", got, wholesalepack.RefusalLinkClosed)
	}
	answerOf(t, eng, signedIn(s.staff), `builtin wholesaleRemoveBuyer(applicationId: `+quote(s.appID)+`, note: "")`)
	if got := refusalOf(t, eng, s, "terms", token); got != wholesalepack.RefusalLinkClosed {
		t.Fatalf("the terms read of a removed buyer answered %q, want %s", got, wholesalepack.RefusalLinkClosed)
	}
	if got := acceptanceReason(t, eng, s, token); got != wholesalepack.RefusalLinkClosed {
		t.Fatalf("an acceptance of a removed buyer answered %q, want %s", got, wholesalepack.RefusalLinkClosed)
	}
}

// A PRICE-SHEET LINK is link_closed while its application is submitted, once it
// is revoked, and when it is rejected; while approved and granted it reads the
// sheet.
func TestAPriceSheetLinkIsLinkClosedUnlessItsApplicationIsApproved(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, _ := termsStore(t, eng, "sheet-closed")
	sheet := mintFor(t, eng, s, wholesalepack.LinkPurposePriceSheet)
	if got := refusalOf(t, eng, s, "priceSheet", sheet); got != wholesalepack.RefusalLinkClosed {
		t.Fatalf("the price sheet of a submitted application answered %q, want %s", got, wholesalepack.RefusalLinkClosed)
	}
	termsNotRequired(t, eng, s)
	decideOn(t, eng, s, "approve")
	if got := refusalOf(t, eng, s, "priceSheet", sheet); got != wholesalepack.RefusalLinkClosed {
		t.Fatalf("the price sheet of an approved application whose access is not granted answered %q, want %s", got, wholesalepack.RefusalLinkClosed)
	}
	grantAccess(t, eng, s)
	if code, body := shopperGet(t, eng, s, "priceSheet", sheet); code != http.StatusOK || !strings.Contains(body, "Frame 410.00") {
		t.Fatalf("the price sheet of an approved, granted application answered %d %s", code, body)
	}
	decideOn(t, eng, s, "revoke")
	if got := refusalOf(t, eng, s, "priceSheet", sheet); got != wholesalepack.RefusalLinkClosed {
		t.Fatalf("the price sheet of a revoked application answered %q, want %s", got, wholesalepack.RefusalLinkClosed)
	}

	r, _ := termsStore(t, eng, "sheet-rejected")
	rejected := mintFor(t, eng, r, wholesalepack.LinkPurposePriceSheet)
	decideOn(t, eng, r, "reject")
	if got := refusalOf(t, eng, r, "priceSheet", rejected); got != wholesalepack.RefusalLinkClosed {
		t.Fatalf("the price sheet of a rejected application answered %q, want %s", got, wholesalepack.RefusalLinkClosed)
	}
}

// AN UNKNOWN TOKEN, A MALFORMED ONE AND ANOTHER STORE'S are link_invalid, byte
// for byte alike, on both reads and the acceptance.
func TestAnUnknownAMalformedAndAnotherStoresTokenAnswerAlike(t *testing.T) {
	eng := liveWholesaleEngine(t)
	a, terms := termsStore(t, eng, "alike-a")
	b, _ := termsStore(t, eng, "alike-b")
	sheet := mintFor(t, eng, a, wholesalepack.LinkPurposePriceSheet)
	tokens := map[string]string{
		"unknown":                           "an-unknown-token-of-forty-three-characters0",
		"malformed":                         "%%not a token!! <>",
		"another store's terms token":       terms,
		"another store's price-sheet token": sheet,
	}
	for _, read := range []string{"terms", "priceSheet"} {
		wantCode, wantBody := shopperGet(t, eng, b, read, tokens["unknown"])
		if wantCode != http.StatusUnprocessableEntity || !strings.Contains(wantBody, wholesalepack.RefusalLinkInvalid) {
			t.Fatalf("an unknown token on the %s read answered %d %s", read, wantCode, wantBody)
		}
		for name, token := range tokens {
			if code, body := shopperGet(t, eng, b, read, token); code != wantCode || body != wantBody {
				t.Fatalf("%s on the %s read answered %d %q; an unknown token answers %d %q", name, read, code, body, wantCode, wantBody)
			}
		}
	}
	for name, token := range tokens {
		if got := acceptanceReason(t, eng, b, token); got != wholesalepack.RefusalLinkInvalid {
			t.Fatalf("an acceptance with %s answered %q, want %s", name, got, wholesalepack.RefusalLinkInvalid)
		}
	}
}

// A STORE WITH NO CURRENT MAP VERSION is documents_unavailable on the terms read
// and on the acceptance, and nothing is written; a store with no current price
// sheet is documents_unavailable on the price-sheet read.
func TestAStoreWithNoCurrentDocumentIsDocumentsUnavailable(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "no-map")
	publishKind(t, eng, s, "terms")
	publishKind(t, eng, s, "priceSheet")
	token := mintFor(t, eng, s, wholesalepack.LinkPurposeTerms)
	if got := refusalOf(t, eng, s, "terms", token); got != wholesalepack.RefusalDocumentsUnavailable {
		t.Fatalf("the terms read with no MAP version answered %q, want %s", got, wholesalepack.RefusalDocumentsUnavailable)
	}
	if got := acceptanceReason(t, eng, s, token); got != wholesalepack.RefusalDocumentsUnavailable {
		t.Fatalf("an acceptance with no MAP version answered %q, want %s", got, wholesalepack.RefusalDocumentsUnavailable)
	}
	if n := versionCount(t, "v1:wholesale:acceptance", wholesalepack.AcceptanceRowID(s.appID)); n != 0 {
		t.Fatalf("%d acceptances with no MAP version", n)
	}

	p := newAccessStore(t, eng, "no-sheet")
	publishKind(t, eng, p, "terms")
	publishKind(t, eng, p, "map")
	decideOn(t, eng, p, "approve")
	grantAccess(t, eng, p)
	sheet := mintFor(t, eng, p, wholesalepack.LinkPurposePriceSheet)
	if got := refusalOf(t, eng, p, "priceSheet", sheet); got != wholesalepack.RefusalDocumentsUnavailable {
		t.Fatalf("the price-sheet read with no price sheet answered %q, want %s", got, wholesalepack.RefusalDocumentsUnavailable)
	}
}

// A TERMS LINK IS JUDGED used, then closed, then expired: an expired link of a
// waiting application is link_expired; once the application is rejected it is
// link_closed, expired or not; once the link is used it is link_used, rejected
// and expired or not.
func TestATermsLinkIsJudgedUsedThenClosedThenExpired(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, token := termsStore(t, eng, "order")
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	rewriteTermsLink(t, eng, s, token, past, "")
	if got := refusalOf(t, eng, s, "terms", token); got != wholesalepack.RefusalLinkExpired {
		t.Fatalf("an expired link answered %q, want %s", got, wholesalepack.RefusalLinkExpired)
	}
	if got := acceptanceReason(t, eng, s, token); got != wholesalepack.RefusalLinkExpired {
		t.Fatalf("an acceptance on an expired link answered %q, want %s", got, wholesalepack.RefusalLinkExpired)
	}
	decideOn(t, eng, s, "reject")
	if got := refusalOf(t, eng, s, "terms", token); got != wholesalepack.RefusalLinkClosed {
		t.Fatalf("an expired link of a rejected application answered %q, want %s", got, wholesalepack.RefusalLinkClosed)
	}
	rewriteTermsLink(t, eng, s, token, past, past)
	if got := refusalOf(t, eng, s, "terms", token); got != wholesalepack.RefusalLinkUsed {
		t.Fatalf("a used, expired link of a rejected application answered %q, want %s", got, wholesalepack.RefusalLinkUsed)
	}
}

// THE APPLICATION FORM NAMES A CLOSED PROGRAM: a store whose switch is off, a
// site the store does not admit, and a store that requires its terms with a
// document missing each send the applicant to the form's error page with
// reason=applications_closed, and the three answers are equal.
func TestTheApplicationFormAnswersApplicationsClosedAlikeForEveryClosedProgram(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newStorefrontScene(t, eng, "sf-closed-reason")
	const want = "/wholesale/problem?reason=applications_closed"
	answers := map[string]string{"the switch off": closedRedirect(t, eng, s)}

	_, store, _ := wholesaleScope()
	siteA, siteB := s.site(t, eng, store, s.accountA), s.site(t, eng, store, s.accountB)
	setUp(t, eng, store, siteA, true)
	answers["a site the store does not admit"] = postApplication(t, eng, siteB, store, s.owner,
		applicationForm("b@kestrel.example.com")).Header().Get("Location")

	_, terms, _ := wholesaleScope()
	terms += "-terms-missing"
	site := s.site(t, eng, terms, s.accountA)
	setUp(t, eng, terms, site, true)
	row := latestPayload(t, "v1:wholesale:wholesaleSettings", wholesalepack.SettingsRowID(terms))
	wsExec(t, eng, seedCtx(), `setWholesaleSettingsRow(settingsId: `+quote(wholesalepack.SettingsRowID(terms))+
		`, ownerUserId: `+quote(asString(row["ownerUserId"]))+`, storeId: `+quote(asString(row["storeId"]))+
		`, applicationsOpen: true, entitlementAdapter: "customerTag", introText: "", accountId: `+quote(asString(row["accountId"]))+
		`, siteId: `+quote(asString(row["siteId"]))+`, staffEmail: "", termsRequired: true, linkLifetimeDays: 0, acceptanceText: "")`)
	answers["terms required and a document missing"] = postApplication(t, eng, site, terms, s.owner,
		applicationForm("c@kestrel.example.com")).Header().Get("Location")

	for name, got := range answers {
		if got != want {
			t.Errorf("%s redirected to %q, want %q", name, got, want)
		}
	}
	if n := unsetApplications(t, eng, store) + unsetApplications(t, eng, terms); n != 0 {
		t.Fatalf("the refused posts wrote %d applications", n)
	}
}
