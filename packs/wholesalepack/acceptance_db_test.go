package wholesalepack_test

// acceptance_db_test.go -- THE TERMS LINK AND THE ACCEPTANCE against a real
// database and a real engine: an applicant reads the terms and accepts them
// through the real shopper handler, the record carries the address and the
// browser, and of two acceptances posted at once one is written.
//
// Postgres-gated like every *_db_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// termsStore is an access store whose terms are required and published, with
// the application's terms link minted, and answers the link's token.
func termsStore(t *testing.T, eng *memql.MemQLEngine, label string) (accessStore, string) {
	t.Helper()
	s := newAccessStore(t, eng, label)
	for kind, body := range map[string]string{"terms": "Net 30 for Kestrel Cycles.", "map": "Advertise at list.", "priceSheet": "Frame 410.00"} {
		answerOf(t, eng, signedIn(s.staff), `builtin wholesalePublishDocument(siteId: `+quote(s.site)+`, storeId: `+quote(s.store)+
			`, kind: `+quote(kind)+`, label: `+quote(kind+" 2026")+`, body: `+quote(body)+`)`)
	}
	wsExec(t, eng, signedIn(s.staff), `builtin wholesaleUpdateSettings(siteId: `+quote(s.site)+`, storeId: `+quote(s.store)+
		`, termsRequired: true)`)
	token, err := wholesalepack.MintLinkForTest(context.Background(), eng, s.appID, wholesalepack.LinkPurposeTerms)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return s, token
}

// shopperRequest is a request through the real shopper handler, stamped as the
// edge stamps it.
func shopperRequest(t *testing.T, eng *memql.MemQLEngine, s accessStore, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req.Header.Set(memql.ShopperSiteHeader, s.site)
	req.Header.Set(memql.ShopperStoreHeader, s.store)
	req.Header.Set(memql.ShopperOwnerHeader, s.owner)
	rec := httptest.NewRecorder()
	server.NewShopperHandler(engineExecutor{eng}, engineSites{eng}, nil).ServeHTTP(rec, req)
	return rec
}

// readTerms is the terms read through the handler: its status and its one row.
func readTerms(t *testing.T, eng *memql.MemQLEngine, s accessStore, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, memql.ShopperReadPathPrefix+"wholesale/terms?requestToken="+url.QueryEscape(token), nil)
	rec := shopperRequest(t, eng, s, req)
	var body struct {
		Data  []map[string]any `json:"data"`
		Error string           `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("the terms read is cacheable: %q", rec.Header().Get("Cache-Control"))
	}
	if len(body.Data) == 1 {
		return rec.Code, body.Data[0]
	}
	return rec.Code, map[string]any{"error": body.Error}
}

// postAcceptance is the acceptance form through the handler, from an address
// the edge saw and a browser.
func postAcceptance(t *testing.T, eng *memql.MemQLEngine, s accessStore, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, memql.ShopperFormPathPrefix+"wholesale/acceptance", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(memql.ShopperClientHeader, "198.51.100.7")
	req.Header.Set("User-Agent", "Example Browser/1.0")
	return shopperRequest(t, eng, s, req)
}

func TestATermsLinkIsAcceptedOnceThroughTheShopperPath(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, token := termsStore(t, eng, "accept-once")

	got := answerOf(t, eng, signedIn(s.staff), `builtin wholesaleGet(applicationId: `+quote(s.appID)+`)`)
	if got["stage"] != wholesalepack.StageWaitingForTerms {
		t.Fatalf("before the acceptance the application is %v", got["stage"])
	}
	if err := s.approve(eng, signedIn(s.staff)); err == nil || !strings.Contains(err.Error(), "This application has not accepted the terms yet.") {
		t.Fatalf("an approve before the acceptance: %v", err)
	}
	link := latestPayload(t, "v1:wholesale:privateLink", wholesalepack.PrivateLinkRowID(wholesalepack.LinkPurposeTerms, s.appID))
	stored, _ := json.Marshal(link)
	if asString(link["usedAt"]) != "" || asString(link["expiresAt"]) == "" || strings.Contains(string(stored), token) ||
		!strings.Contains(string(stored), sha256Text(token)) {
		t.Fatalf("the minted link is %+v", link)
	}

	code, terms := readTerms(t, eng, s, token)
	docs := listRows(terms["documents"])
	if code != http.StatusOK || len(docs) != 2 || terms["statement"] != wholesalepack.DefaultAcceptanceText {
		t.Fatalf("the terms read answered %d %+v", code, terms)
	}
	if raw, _ := json.Marshal(terms); strings.Contains(string(raw), "sam@") || strings.Contains(string(raw), s.appID) {
		t.Fatalf("the terms read carries the email or the id: %s", raw)
	}
	form := url.Values{"requestToken": {token}, "acceptTerms": {"on"}, "acceptMap": {"on"}, "signedName": {"Ada Example"},
		"termsSha256": {asString(docs[0]["sha256"])}, "mapSha256": {asString(docs[1]["sha256"])},
		"statementSha256": {asString(terms["statementSha256"])},
		"clientAddress":   {"203.0.113.9"}, "userAgent": {"Forged/1.0"}}
	rec := postAcceptance(t, eng, s, form)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || loc != "/wholesale/accepted" {
		t.Fatalf("the acceptance answered %d %q", rec.Code, loc)
	}

	rows := wsRows(t, eng, signedIn(s.merchant), `query acceptanceForApplication(applicationId: `+quote("v1:wholesale:application:"+s.appID)+`)`)
	if len(rows) != 1 {
		t.Fatalf("the merchant reads %d acceptances", len(rows))
	}
	a := rows[0]
	if a["signedName"] != "Ada Example" || a["clientAddress"] != "198.51.100.7" || a["userAgent"] != "Example Browser/1.0" ||
		a["termsSha256"] != docs[0]["sha256"] || a["mapSha256"] != docs[1]["sha256"] || a["statement"] != wholesalepack.DefaultAcceptanceText ||
		bareID(asString(a["accountId"])) != s.account || asString(a["applicantEmail"]) == "" {
		t.Fatalf("the acceptance is %+v", a)
	}
	if used := latestPayload(t, "v1:wholesale:privateLink", wholesalepack.PrivateLinkRowID(wholesalepack.LinkPurposeTerms, s.appID)); asString(used["usedAt"]) == "" {
		t.Fatalf("the link was not marked used: %+v", used)
	}

	// ONCE: the same post again is link_used, and the read is too.
	if loc := postAcceptance(t, eng, s, form).Header().Get("Location"); loc != "/wholesale/link-problem?reason=link_used" {
		t.Fatalf("a second acceptance redirected to %q", loc)
	}
	if code, again := readTerms(t, eng, s, token); code != http.StatusUnprocessableEntity || again["error"] != wholesalepack.RefusalLinkUsed {
		t.Fatalf("the terms read after the acceptance answered %d %+v", code, again)
	}
	if n := versionCount(t, "v1:wholesale:acceptance", wholesalepack.AcceptanceRowID(s.appID)); n != 1 {
		t.Fatalf("%d versions of the acceptance", n)
	}

	got = answerOf(t, eng, signedIn(s.staff), `builtin wholesaleGet(applicationId: `+quote(s.appID)+`)`)
	acc, _ := got["acceptance"].(map[string]any)
	if got["stage"] != wholesalepack.StageReadyToReview || acc["signedName"] != "Ada Example" || acc["termsLabel"] != "terms 2026" {
		t.Fatalf("after the acceptance the application is %v with %+v", got["stage"], acc)
	}
	if err := s.approve(eng, signedIn(s.staff)); err != nil {
		t.Fatalf("an approve after the acceptance: %v", err)
	}
}

// A LINK SENT AGAIN REPLACES ITS TOKEN, in the database too: the version before
// it no longer matches, and an unknown token answers the same.
func TestAReplacedTokenFindsNothingInTheDatabase(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, first := termsStore(t, eng, "accept-replaced")
	second, err := wholesalepack.MintLinkForTest(context.Background(), eng, s.appID, wholesalepack.LinkPurposeTerms)
	if err != nil {
		t.Fatalf("mint again: %v", err)
	}
	for token, want := range map[string]string{first: wholesalepack.RefusalLinkInvalid, "an-unknown-token": wholesalepack.RefusalLinkInvalid} {
		if code, got := readTerms(t, eng, s, token); code != http.StatusUnprocessableEntity || got["error"] != want {
			t.Fatalf("token %.8s answered %d %+v, want %s", token, code, got, want)
		}
	}
	if code, _ := readTerms(t, eng, s, second); code != http.StatusOK {
		t.Fatalf("the new token answered %d", code)
	}
}

// ruling 49: A NEW TERMS VERSION BETWEEN THE READ AND THE POST refuses the post
// as terms_changed through the handler, and leaves the link usable.
func TestANewTermsVersionBetweenTheReadAndThePostIsRefusedThroughTheShopperPath(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, token := termsStore(t, eng, "accept-changed")
	_, terms := readTerms(t, eng, s, token)
	docs := listRows(terms["documents"])
	answerOf(t, eng, signedIn(s.staff), `builtin wholesalePublishDocument(siteId: `+quote(s.site)+`, storeId: `+quote(s.store)+
		`, kind: "terms", label: "terms 2027", body: "Net 45.")`)
	form := url.Values{"requestToken": {token}, "acceptTerms": {"on"}, "acceptMap": {"on"}, "signedName": {"Ada Example"},
		"termsSha256": {asString(docs[0]["sha256"])}, "mapSha256": {asString(docs[1]["sha256"])},
		"statementSha256": {asString(terms["statementSha256"])}}
	if loc := postAcceptance(t, eng, s, form).Header().Get("Location"); loc != "/wholesale/link-problem?reason=terms_changed" {
		t.Fatalf("a post of the old terms redirected to %q", loc)
	}
	if n := versionCount(t, "v1:wholesale:acceptance", wholesalepack.AcceptanceRowID(s.appID)); n != 0 {
		t.Fatalf("%d acceptances after terms_changed", n)
	}
	code, again := readTerms(t, eng, s, token)
	if code != http.StatusOK || listRows(again["documents"])[0]["label"] != "terms 2027" {
		t.Fatalf("the read again answered %d %+v", code, again)
	}
	form["termsSha256"] = []string{asString(listRows(again["documents"])[0]["sha256"])}
	form["mapSha256"] = []string{"zz" + strings.Repeat("0", 62)}
	if loc := postAcceptance(t, eng, s, form).Header().Get("Location"); loc != "/wholesale/link-problem?reason=invalid" {
		t.Fatalf("a fingerprint that is not hex redirected to %q", loc)
	}
}

// OF TWO ACCEPTANCES POSTED AT ONCE, both past the acceptance read, one is
// written and the other is told the link is used.
func TestTwoAcceptancesPostedAtOnceWriteOneAndTheOtherIsLinkUsed(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, token := termsStore(t, eng, "accept-race")
	_, terms := readTerms(t, eng, s, token)
	docs := listRows(terms["documents"])
	args := map[string]any{"requestToken": token, "acceptTerms": "on", "acceptMap": "on", "signedName": "Ada Example",
		"termsSha256": asString(docs[0]["sha256"]), "mapSha256": asString(docs[1]["sha256"]),
		"statementSha256": asString(terms["statementSha256"]), "storeId": s.store, "siteId": s.site}
	var arrived sync.WaitGroup
	arrived.Add(2)
	barrier := func() {
		arrived.Done()
		done := make(chan struct{})
		go func() { arrived.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
		}
	}
	errs := make([]error, 2)
	var all sync.WaitGroup
	for i := range errs {
		all.Add(1)
		go func(i int) {
			defer all.Done()
			errs[i] = wholesalepack.AcceptTermsForTest(asSiteOwner(s.owner), eng, args, barrier)
		}(i)
	}
	all.Wait()
	var ok, used int
	for _, err := range errs {
		var r *memql.ShopperRefusal
		switch {
		case err == nil:
			ok++
		case errors.As(err, &r) && r.Code == wholesalepack.RefusalLinkUsed:
			used++
		default:
			t.Fatalf("a post answered %v", err)
		}
	}
	if ok != 1 || used != 1 {
		t.Fatalf("%d posts written and %d told link_used, want one of each (%v)", ok, used, errs)
	}
	if n := versionCount(t, "v1:wholesale:acceptance", wholesalepack.AcceptanceRowID(s.appID)); n != 1 {
		t.Fatalf("%d versions of the acceptance", n)
	}
}

// shopperGet is one private read through the handler: its status and body.
func shopperGet(t *testing.T, eng *memql.MemQLEngine, s accessStore, read, token string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, memql.ShopperReadPathPrefix+"wholesale/"+read+"?requestToken="+url.QueryEscape(token), nil)
	rec := shopperRequest(t, eng, s, req)
	return rec.Code, rec.Body.String()
}

// acceptForm is a good acceptance post for what the terms read answered.
func acceptForm(token string, terms map[string]any) url.Values {
	docs := listRows(terms["documents"])
	return url.Values{"requestToken": {token}, "acceptTerms": {"on"}, "acceptMap": {"on"}, "signedName": {"Ada Example"},
		"termsSha256": {asString(docs[0]["sha256"])}, "mapSha256": {asString(docs[1]["sha256"])},
		"statementSha256": {asString(terms["statementSha256"])}}
}

// ruling 52, through the handler: A STAFF EDIT OF THE SENTENCE between the read
// and the post is terms_changed, nothing is written, and the read again answers
// the new sentence and its fingerprint.
func TestAnEditOfTheSentenceBetweenTheReadAndThePostIsRefusedThroughTheShopperPath(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, token := termsStore(t, eng, "accept-sentence")
	_, terms := readTerms(t, eng, s, token)
	if terms["statementSha256"] != sha256Text(wholesalepack.DefaultAcceptanceText) {
		t.Fatalf("the read answers the fingerprint %v", terms["statementSha256"])
	}
	revised := "I accept the revised Kestrel Cycles trade terms."
	wsExec(t, eng, signedIn(s.staff), `builtin wholesaleUpdateSettings(siteId: `+quote(s.site)+`, storeId: `+quote(s.store)+
		`, acceptanceText: `+quote(revised)+`)`)
	if loc := postAcceptance(t, eng, s, acceptForm(token, terms)).Header().Get("Location"); loc != "/wholesale/link-problem?reason=terms_changed" {
		t.Fatalf("a post of the old sentence redirected to %q", loc)
	}
	if n := versionCount(t, "v1:wholesale:acceptance", wholesalepack.AcceptanceRowID(s.appID)); n != 0 {
		t.Fatalf("%d acceptances after terms_changed", n)
	}
	link := latestPayload(t, "v1:wholesale:privateLink", wholesalepack.PrivateLinkRowID(wholesalepack.LinkPurposeTerms, s.appID))
	if asString(link["usedAt"]) != "" {
		t.Fatalf("the link was marked used: %+v", link)
	}
	code, again := readTerms(t, eng, s, token)
	if code != http.StatusOK || again["statement"] != revised || again["statementSha256"] != sha256Text(revised) {
		t.Fatalf("the read again answered %d %+v", code, again)
	}
}

// ONE MERCHANT, TWO STORES: a real token of store A presented on store B answers
// exactly what an unknown token answers, for each of the three link builtins
// through the handler.
func TestATokenOfTheMerchantsOtherStoreAnswersAsAnUnknownOneThroughTheShopperPath(t *testing.T) {
	eng := liveWholesaleEngine(t)
	a, terms := termsStore(t, eng, "two-stores")
	sheet, err := wholesalepack.MintLinkForTest(context.Background(), eng, a.appID, wholesalepack.LinkPurposePriceSheet)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	b := a
	b.store, b.site = a.store+"-b", a.site+"-b"
	seedSiteIn(t, eng, b.site, a.merchant, b.store, a.account)
	b.owner = siteOwnerOf(t, eng, b.site)
	if b.owner != a.owner {
		t.Fatalf("the two sites have two owners: %q and %q", a.owner, b.owner)
	}
	const unknown = "an-unknown-token"
	for _, read := range []string{"terms", "priceSheet"} {
		for _, token := range []string{terms, sheet} {
			wantCode, wantBody := shopperGet(t, eng, b, read, unknown)
			code, body := shopperGet(t, eng, b, read, token)
			if code != wantCode || body != wantBody || !strings.Contains(body, wholesalepack.RefusalLinkInvalid) {
				t.Fatalf("%s with store A's token on store B answered %d %s; an unknown token answers %d %s", read, code, body, wantCode, wantBody)
			}
		}
	}
	_, shown := readTerms(t, eng, a, terms)
	for _, token := range []string{terms, sheet} {
		form := acceptForm(token, shown)
		got := postAcceptance(t, eng, b, form).Header().Get("Location")
		form.Set("requestToken", unknown)
		want := postAcceptance(t, eng, b, form).Header().Get("Location")
		if got != want || want != "/wholesale/link-problem?reason=link_invalid" {
			t.Fatalf("an acceptance with store A's token on store B redirected to %q; an unknown token to %q", got, want)
		}
	}
	if n := versionCount(t, "v1:wholesale:acceptance", wholesalepack.AcceptanceRowID(a.appID)); n != 0 {
		t.Fatalf("%d acceptances", n)
	}
}

// A LINK SENT AGAIN CLEARS ITS usedAt in the database, with a new token hash.
func TestARemintedLinkClearsItsUsedAt(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, token := termsStore(t, eng, "remint")
	_, terms := readTerms(t, eng, s, token)
	if loc := postAcceptance(t, eng, s, acceptForm(token, terms)).Header().Get("Location"); loc != "/wholesale/accepted" {
		t.Fatalf("the acceptance redirected to %q", loc)
	}
	id := wholesalepack.PrivateLinkRowID(wholesalepack.LinkPurposeTerms, s.appID)
	if used := latestPayload(t, "v1:wholesale:privateLink", id); asString(used["usedAt"]) == "" {
		t.Fatalf("the link was not marked used: %+v", used)
	}
	again, err := wholesalepack.MintLinkForTest(context.Background(), eng, s.appID, wholesalepack.LinkPurposeTerms)
	if err != nil {
		t.Fatalf("mint again: %v", err)
	}
	fresh := latestPayload(t, "v1:wholesale:privateLink", id)
	if asString(fresh["usedAt"]) != "" || fresh["tokenHash"] != sha256Text(again) {
		t.Fatalf("the link sent again is %+v", fresh)
	}
	// And the acceptance still makes it used.
	if code, got := readTerms(t, eng, s, again); code != http.StatusUnprocessableEntity || got["error"] != wholesalepack.RefusalLinkUsed {
		t.Fatalf("the new token after the acceptance answered %d %+v", code, got)
	}
}

// THE PRICE-SHEET ROUTE through the real handler: the link stores no expiry,
// opens while the buyer is approved and granted, and is link_used after a revoke.
func TestAPriceSheetLinkOpensThroughTheShopperPathWhileApprovedAndGranted(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, token := termsStore(t, eng, "pricesheet")
	_, terms := readTerms(t, eng, s, token)
	if loc := postAcceptance(t, eng, s, acceptForm(token, terms)).Header().Get("Location"); loc != "/wholesale/accepted" {
		t.Fatalf("the acceptance redirected to %q", loc)
	}
	if err := s.approve(eng, signedIn(s.staff)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	sheet, err := wholesalepack.MintLinkForTest(context.Background(), eng, s.appID, wholesalepack.LinkPurposePriceSheet)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	link := latestPayload(t, "v1:wholesale:privateLink", wholesalepack.PrivateLinkRowID(wholesalepack.LinkPurposePriceSheet, s.appID))
	if asString(link["expiresAt"]) != "" || asString(link["usedAt"]) != "" {
		t.Fatalf("the price-sheet link is %+v, want no expiry", link)
	}
	if code, body := shopperGet(t, eng, s, "priceSheet", sheet); code != http.StatusUnprocessableEntity || !strings.Contains(body, wholesalepack.RefusalLinkUsed) {
		t.Fatalf("before access is granted the price sheet answered %d %s", code, body)
	}
	// A GRANTED ENTITLEMENT, seeded at internal origin in test code only: the
	// test engine has no Shopify connector to grant one.
	app := "v1:wholesale:application:" + s.appID
	wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:entitlement", wholesalepack.EntitlementRowID(s.appID), map[string]any{
		"ownerUserId": s.owner, "applicationId": app, "storeId": s.store, "buyerEmail": "sam@pricesheet.example.com",
		"adapter": wholesalepack.AdapterCustomerTag, "state": wholesalepack.EntitlementGranted, "accountId": s.account}))
	code, body := shopperGet(t, eng, s, "priceSheet", sheet)
	if code != http.StatusOK || !strings.Contains(body, "Frame 410.00") || !strings.Contains(body, "priceSheet 2026") ||
		strings.Contains(body, "sam@") || strings.Contains(body, s.appID) {
		t.Fatalf("the price sheet answered %d %s", code, body)
	}
	if _, err := eng.Execute(signedIn(s.staff), `builtin wholesaleRecordDecision(applicationId: `+quote(s.appID)+
		`, transition: "revoke", note: "account closed")`); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if code, body := shopperGet(t, eng, s, "priceSheet", sheet); code != http.StatusUnprocessableEntity || !strings.Contains(body, wholesalepack.RefusalLinkUsed) {
		t.Fatalf("after the revoke the price sheet answered %d %s", code, body)
	}
}

// THE LINK READ OF AN APPLICATION carries no token hash to the store's people.
func TestTheLinkReadOfAnApplicationCarriesNoTokenHash(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, token := termsStore(t, eng, "nohash")
	rows := wsRows(t, eng, signedIn(s.merchant), `query privateLinkForApplication(applicationId: `+
		quote("v1:wholesale:application:"+s.appID)+`, purpose: "terms")`)
	raw, _ := json.Marshal(rows)
	if len(rows) != 1 || strings.Contains(string(raw), "tokenHash") || strings.Contains(string(raw), sha256Text(token)) {
		t.Fatalf("the merchant reads %s", raw)
	}
}
