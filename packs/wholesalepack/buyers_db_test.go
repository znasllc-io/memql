package wholesalepack_test

// buyers_db_test.go -- THE STORE'S PEOPLE ADD A BUYER BY HAND AND REMOVE ONE
// (U10), against a real database and a real engine.
//
// An added buyer is an application like any other, written once: submitted,
// owned by the store's merchant, tied to the site's organization, and -- while
// the store requires its terms -- waiting for the acceptance before it can be
// approved. Removing a buyer records a revoke by the caller. The organizations,
// groups and grants are seeded through seed_helpers_db_test.go.
//
// Postgres-gated like every *_db_test.go.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

const applicationConcept = "v1:wholesale:application"

// addBuyerCall is wholesaleAddBuyer on a site and a store.
func addBuyerCall(site, store, company, name, email string) string {
	return `builtin wholesaleAddBuyer(siteId: ` + quote(site) + `, storeId: ` + quote(store) + `, companyName: ` + quote(company) +
		`, applicantName: ` + quote(name) + `, applicantEmail: ` + quote(email) + `)`
}

// removeBuyerCall is wholesaleRemoveBuyer on one application.
func removeBuyerCall(applicationID, note string) string {
	return `builtin wholesaleRemoveBuyer(applicationId: ` + quote(applicationID) + `, note: ` + quote(note) + `)`
}

// addBuyer adds a buyer as ctx and answers the new application's id.
func addBuyer(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, s accessStore, company, email string) string {
	t.Helper()
	got := answerOf(t, eng, ctx, addBuyerCall(s.site, s.store, company, "Ada Example", email))
	appID := asString(got["applicationId"])
	if appID == "" {
		t.Fatalf("wholesaleAddBuyer answered %+v, want an applicationId", got)
	}
	return appID
}

// decideAs records one decision on an application as ctx.
func decideAs(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, applicationID, transition string) {
	t.Helper()
	wsExec(t, eng, ctx, `builtin wholesaleRecordDecision(applicationId: `+quote(applicationID)+`, transition: `+quote(transition)+`)`)
}

// decisionsOf is an application's decisions, newest first, as ctx reads them.
func decisionsOf(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, applicationID string) []map[string]any {
	t.Helper()
	return wsRows(t, eng, ctx, `query decisionsForApplication(applicationId: `+quote(applicationConcept+":"+bareID(applicationID))+`)`)
}

// stageOf is one application's stage and state, as the Wholesale section opens it.
func stageOf(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, applicationID string) (stage, state string) {
	t.Helper()
	got := answerOf(t, eng, ctx, `builtin wholesaleGet(applicationId: `+quote(applicationID)+`)`)
	return asString(got["stage"]), asString(got["state"])
}

// storeApplicationCount is how many applications the merchant reads on the store.
func storeApplicationCount(t *testing.T, eng *memql.MemQLEngine, s accessStore) int {
	t.Helper()
	return len(wsRows(t, eng, signedIn(s.merchant), `query applicationsForStore(storeId: `+quote(s.store)+`)`))
}

// AN ADDED BUYER IS A SUBMITTED APPLICATION, OWNED BY THE MERCHANT, TIED TO THE
// ORGANIZATION, written once, with source staff and the caller as addedBy, its
// three fields trimmed.
func TestAnAddedBuyerIsASubmittedApplicationOwnedByTheMerchantAndTiedToTheOrganization(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "add-buyer")
	got := answerOf(t, eng, signedIn(s.staff), addBuyerCall(s.site, s.store, "  Kestrel Cycles ", " Ada Example ", "  ada@kestrel.example.com "))
	appID := asString(got["applicationId"])
	if !strings.HasPrefix(bareID(appID), "wholesale-staff-") {
		t.Fatalf("the added buyer's application is at %q, want wholesale-staff-<id>", appID)
	}
	row := latestPayload(t, applicationConcept, bareID(appID))
	want := map[string]any{
		"ownerUserId": s.owner, "storeId": s.store, "siteId": s.site, "companyName": "Kestrel Cycles",
		"applicantName": "Ada Example", "applicantEmail": "ada@kestrel.example.com",
		"source": wholesalepack.SourceStaff, "addedBy": userRef(s.staff),
	}
	for field, value := range want {
		if row[field] != value {
			t.Errorf("%s = %v, want %v", field, row[field], value)
		}
	}
	if bareID(asString(row["accountId"])) != s.account {
		t.Errorf("accountId = %v, want the site's organization %s", row["accountId"], s.account)
	}
	if n := versionCount(t, applicationConcept, bareID(appID)); n != 1 {
		t.Fatalf("the added buyer's application was written %d times, want once", n)
	}
	if stage, state := stageOf(t, eng, signedIn(s.staff), appID); state != wholesalepack.StateSubmitted ||
		stage != wholesalepack.StageReadyToReview {
		t.Fatalf("the added buyer reads %s / %s, want submitted / readyToReview", state, stage)
	}
	if rows := decisionsOf(t, eng, signedIn(s.staff), appID); len(rows) != 0 {
		t.Fatalf("adding a buyer decided it: %+v", rows)
	}
	// THE SHOPPER'S RULES: the three fields are required and bounded as the form bounds them.
	before := storeApplicationCount(t, eng, s)
	for name, call := range map[string]string{
		"an empty company":                addBuyerCall(s.site, s.store, "   ", "Ada Example", "ada2@kestrel.example.com"),
		"a company too long":              addBuyerCall(s.site, s.store, strings.Repeat("k", 201), "Ada Example", "ada3@kestrel.example.com"),
		"a name too long":                 addBuyerCall(s.site, s.store, "Kestrel Cycles", strings.Repeat("a", 121), "ada4@kestrel.example.com"),
		"an email too long":               addBuyerCall(s.site, s.store, "Kestrel Cycles", "Ada Example", strings.Repeat("a", 309)+"@example.com"),
		"an empty email":                  addBuyerCall(s.site, s.store, "Kestrel Cycles", "Ada Example", " "),
		"not an address":                  addBuyerCall(s.site, s.store, "Kestrel Cycles", "Ada Example", "not an address"),
		"a store the site does not serve": addBuyerCall(s.site, s.store+"-elsewhere", "Kestrel Cycles", "Ada Example", "ada5@kestrel.example.com"),
	} {
		if _, err := eng.Execute(signedIn(s.staff), call); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if after := storeApplicationCount(t, eng, s); after != before {
		t.Fatalf("refused adds wrote %d applications", after-before)
	}
}

// ADDING A BUYER WORKS WHILE APPLICATIONS ARE CLOSED: closed is the public form.
func TestAddingABuyerWorksWhileApplicationsAreClosed(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "add-closed")
	wsExec(t, eng, signedIn(s.staff), `builtin wholesaleUpdateSettings(siteId: `+quote(s.site)+`, storeId: `+quote(s.store)+
		`, applicationsOpen: false)`)
	appID := addBuyer(t, eng, signedIn(s.staff), s, "Heron Bikes", "parts@heron.example.com")
	if stage, state := stageOf(t, eng, signedIn(s.staff), appID); state != wholesalepack.StateSubmitted ||
		stage != wholesalepack.StageReadyToReview {
		t.Fatalf("the buyer added to a closed store reads %s / %s", state, stage)
	}
}

// AN ADDRESS WITH A SUBMITTED OR AN APPROVED APPLICATION ON THE STORE IS
// REFUSED, compared trimmed and lower-cased, and nothing is written.
func TestAnAddressWithAnOpenApplicationIsRefused(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "add-dup")
	first := addBuyer(t, eng, signedIn(s.staff), s, "Osprey Wheels", "buyer@example.com")
	before := storeApplicationCount(t, eng, s)
	_, err := eng.Execute(signedIn(s.staff), addBuyerCall(s.site, s.store, "Osprey Wheels", "Ada Example", " Buyer@Example.com "))
	if err == nil || !strings.Contains(err.Error(), "This address already has an application on this store.") {
		t.Fatalf("a second application for a submitted address: %v", err)
	}
	// AN APPROVED ONE BLOCKS TOO, and so does the shopper's own application.
	decideAs(t, eng, signedIn(s.staff), first, "approve")
	for _, email := range []string{"buyer@example.com", "SAM@add-dup.example.com"} {
		_, err := eng.Execute(signedIn(s.staff), addBuyerCall(s.site, s.store, "Osprey Wheels", "Ada Example", email))
		if err == nil || !strings.Contains(err.Error(), "This address already has an application on this store.") {
			t.Fatalf("%s: a second application: %v", email, err)
		}
	}
	if after := storeApplicationCount(t, eng, s); after != before {
		t.Fatalf("refused adds wrote %d applications", after-before)
	}
}

// THE DUPLICATE CHECK SEES AN APPLICATION STORED UNDER THE OTHER SPELLING OF THE
// STORE ID, whichever spelling the caller names.
func TestTheDuplicateCheckOfAnAddedBuyerFindsTheOtherSpellingOfTheStoreId(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "add-spelling")
	canonical := "v1:shopify:store:" + s.store
	seeded := "seeded-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	wsExec(t, eng, seedCtx(), `createApplicationRow(applicationId: `+quote(seeded)+`, ownerUserId: `+quote(s.owner)+
		`, storeId: `+quote(canonical)+`, siteId: `+quote(s.site)+`, accountId: `+quote(s.account)+
		`, companyName: "Plover Cycles", applicantName: "Ida Example", applicantEmail: "ida@plover.example.com", submittedAt: `+happenedNow()+`)`)
	for _, store := range []string{s.store, canonical} {
		_, err := eng.Execute(signedIn(s.staff), addBuyerCall(s.site, store, "Plover Cycles", "Ida Example", "Ida@Plover.example.com"))
		if err == nil || !strings.Contains(err.Error(), "This address already has an application on this store.") {
			t.Fatalf("store named %q: an address held under the canonical store id was added again: %v", store, err)
		}
	}
	// AND THE REVERSE: the shopper's application is under the bare id.
	_, err := eng.Execute(signedIn(s.staff), addBuyerCall(s.site, canonical, "Trading", "Sam Rivers", "sam@add-spelling.example.com"))
	if err == nil || !strings.Contains(err.Error(), "This address already has an application on this store.") {
		t.Fatalf("an address held under the bare store id was added again naming the canonical one: %v", err)
	}
}

// AN ADDRESS WHOSE APPLICATION WAS REJECTED -- or whose buyer was removed --
// MAY BE ADDED AGAIN: a final application blocks nothing.
func TestAnAddressWhoseApplicationWasRejectedMayBeAddedAgain(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "add-again")
	decideAs(t, eng, signedIn(s.staff), s.appID, "reject")
	again := addBuyer(t, eng, signedIn(s.staff), s, "Trading add-again", "sam@add-again.example.com")
	if bareID(again) == s.appID {
		t.Fatalf("adding the address again reused the rejected application")
	}
	decideAs(t, eng, signedIn(s.staff), again, "approve")
	wsExec(t, eng, signedIn(s.staff), removeBuyerCall(again, "closed the account"))
	third := addBuyer(t, eng, signedIn(s.staff), s, "Trading add-again", "sam@add-again.example.com")
	if stage, state := stageOf(t, eng, signedIn(s.staff), third); state != wholesalepack.StateSubmitted ||
		stage != wholesalepack.StageReadyToReview {
		t.Fatalf("the address added after a removal reads %s / %s", state, stage)
	}
}

// WHEN THE STORE REQUIRES ITS TERMS, adding a buyer needs the three documents,
// and the added buyer waits for the acceptance before it can be approved.
func TestAddingABuyerNeedsTheDocumentsWhenTermsAreRequired(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "add-terms")
	wsExec(t, eng, signedIn(s.staff), `builtin wholesaleUpdateSettings(siteId: `+quote(s.site)+`, storeId: `+quote(s.store)+
		`, applicationsOpen: false, termsRequired: true)`)
	before := storeApplicationCount(t, eng, s)
	_, err := eng.Execute(signedIn(s.staff), addBuyerCall(s.site, s.store, "Wren Cycles", "Ada Example", "ada@wren.example.com"))
	if err == nil || !strings.Contains(err.Error(), "Publish the terms, the MAP policy and the price sheet first.") {
		t.Fatalf("an add with no documents while terms are required: %v", err)
	}
	if after := storeApplicationCount(t, eng, s); after != before {
		t.Fatalf("the refused add wrote %d applications", after-before)
	}
	for kind, body := range map[string]string{"terms": "Net 30 for Wren Cycles.", "map": "Advertise at list.", "priceSheet": "Frame 410.00"} {
		answerOf(t, eng, signedIn(s.staff), `builtin wholesalePublishDocument(siteId: `+quote(s.site)+`, storeId: `+quote(s.store)+
			`, kind: `+quote(kind)+`, label: `+quote(kind+" 2026")+`, body: `+quote(body)+`)`)
	}
	appID := addBuyer(t, eng, signedIn(s.staff), s, "Wren Cycles", "ada@wren.example.com")
	if stage, state := stageOf(t, eng, signedIn(s.staff), appID); state != wholesalepack.StateSubmitted ||
		stage != wholesalepack.StageWaitingForTerms {
		t.Fatalf("the added buyer reads %s / %s, want submitted / waitingForTerms", state, stage)
	}
	_, err = eng.Execute(signedIn(s.staff), `builtin wholesaleRecordDecision(applicationId: `+quote(appID)+`, transition: "approve")`)
	if err == nil || !strings.Contains(err.Error(), "This application has not accepted the terms yet.") {
		t.Fatalf("an approve of an added buyer before the acceptance: %v", err)
	}
	// THE SAME STEPS AS AN APPLICANT: the terms link, the acceptance, then the approval.
	token, err := wholesalepack.MintLinkForTest(context.Background(), eng, bareID(appID), wholesalepack.LinkPurposeTerms)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	code, terms := readTerms(t, eng, s, token)
	if code != http.StatusOK {
		t.Fatalf("the added buyer's terms link answered %d %+v", code, terms)
	}
	if rec := postAcceptance(t, eng, s, acceptForm(token, terms)); rec.Header().Get("Location") != "/wholesale/accepted" {
		t.Fatalf("the acceptance answered %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if stage, _ := stageOf(t, eng, signedIn(s.staff), appID); stage != wholesalepack.StageReadyToReview {
		t.Fatalf("after the acceptance the added buyer reads %s", stage)
	}
	decideAs(t, eng, signedIn(s.staff), appID, "approve")
}

// REMOVING A BUYER RECORDS A REVOKE BY THE CALLER, with the note, and answers the
// decision receipt wholesaleRecordDecision answers.
func TestRemoveRecordsARevokeByTheCaller(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "remove")
	decideAs(t, eng, signedIn(s.staff), s.appID, "approve")
	got := answerOf(t, eng, signedIn(s.staff), removeBuyerCall(s.appID, "stopped trading"))
	want := map[string]any{
		"applicationId": s.appID, "storeId": s.store, "transition": "revoke", "decidedBy": userRef(s.staff),
		"state": wholesalepack.StateRevoked, "stage": wholesalepack.StateRevoked,
	}
	for field, value := range want {
		if got[field] != value {
			t.Errorf("the receipt's %s = %v, want %v", field, got[field], value)
		}
	}
	rows := decisionsOf(t, eng, signedIn(s.staff), s.appID)
	if len(rows) != 2 || rows[0]["transition"] != "revoke" || rows[0]["decidedBy"] != userRef(s.staff) ||
		rows[0]["note"] != "stopped trading" || bareID(asString(rows[0]["accountId"])) != s.account ||
		rows[0]["ownerUserId"] != s.owner {
		t.Fatalf("the decision log reads %+v, want a revoke by %s with the note", rows, userRef(s.staff))
	}
	if _, state := stageOf(t, eng, signedIn(s.staff), s.appID); state != wholesalepack.StateRevoked {
		t.Fatalf("the removed buyer reads %s", state)
	}
}

// ONLY AN APPROVED BUYER CAN BE REMOVED: a submitted, a rejected and a removed
// application are refused, and nothing is written.
func TestRemoveIsRefusedForAnApplicationThatIsNotApproved(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "remove-not")
	rejected := addBuyer(t, eng, signedIn(s.staff), s, "Swift Frames", "swift@example.com")
	decideAs(t, eng, signedIn(s.staff), rejected, "reject")
	removed := addBuyer(t, eng, signedIn(s.staff), s, "Lark Bicycles", "lark@example.com")
	decideAs(t, eng, signedIn(s.staff), removed, "approve")
	wsExec(t, eng, signedIn(s.staff), removeBuyerCall(removed, ""))
	for name, appID := range map[string]string{"submitted": s.appID, "rejected": rejected, "removed": removed} {
		before := len(decisionsOf(t, eng, signedIn(s.staff), appID))
		_, err := eng.Execute(signedIn(s.staff), removeBuyerCall(appID, ""))
		if err == nil || !strings.Contains(err.Error(), "wholesale: only an approved buyer can be removed") {
			t.Errorf("removing a %s application: %v", name, err)
		}
		if after := len(decisionsOf(t, eng, signedIn(s.staff), appID)); after != before {
			t.Errorf("the refused removal of a %s application wrote %d decisions", name, after-before)
		}
	}
}

// A CLUSTER OPERATOR ADDS AND REMOVES NO BUYER (X4), with the refusal a decision
// gets, and nothing is written.
func TestAnOperatorCannotAddOrRemoveABuyer(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "operator-buyer")
	operator := asOperator("operator-" + s.staff)
	const refusal = "the cluster's operators host the store's applications and do not decide them"
	before := storeApplicationCount(t, eng, s)
	_, err := eng.Execute(operator, addBuyerCall(s.site, s.store, "Finch Cycles", "Ada Example", "finch@example.com"))
	if err == nil || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("a cluster operator's add: %v", err)
	}
	if after := storeApplicationCount(t, eng, s); after != before {
		t.Fatalf("the operator's refused add wrote %d applications", after-before)
	}
	decideAs(t, eng, signedIn(s.staff), s.appID, "approve")
	_, err = eng.Execute(operator, removeBuyerCall(s.appID, ""))
	if err == nil || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("a cluster operator's removal: %v", err)
	}
	if rows := decisionsOf(t, eng, signedIn(s.staff), s.appID); len(rows) != 1 {
		t.Fatalf("the operator's refused removal left %d decisions, want the approval alone", len(rows))
	}
}

// A PERSON OF THE ORGANIZATION WITHOUT THE PART can neither add nor remove a
// buyer: the merchant here is one.
func TestAPersonWithoutThePartCannotAddOrRemoveABuyer(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "buyer-nopart")
	decideAs(t, eng, signedIn(s.staff), s.appID, "approve")
	before := storeApplicationCount(t, eng, s)
	for name, call := range map[string]string{
		"wholesaleAddBuyer":    addBuyerCall(s.site, s.store, "Finch Cycles", "Ada Example", "finch@example.com"),
		"wholesaleRemoveBuyer": removeBuyerCall(s.appID, ""),
	} {
		if _, err := eng.Execute(signedIn(s.merchant), call); err == nil || !strings.Contains(err.Error(), memql.CodeCapabilityNotHeld) {
			t.Errorf("%s by a person of the organization without the part: %v, want %s", name, err, memql.CodeCapabilityNotHeld)
		}
	}
	if after := storeApplicationCount(t, eng, s); after != before {
		t.Fatalf("a refused add wrote %d applications", after-before)
	}
	if rows := decisionsOf(t, eng, signedIn(s.staff), s.appID); len(rows) != 1 {
		t.Fatalf("a refused removal left %d decisions", len(rows))
	}
}

// A HOLDER OF THE PART IN ANOTHER ORGANIZATION can neither add nor remove a
// buyer: on the store's own site and application, and through a site of their
// organization bound to the store. A removal they cannot be admitted for is
// answered exactly what an unknown application is.
func TestAHolderInAnotherOrganizationCannotAddOrRemoveABuyer(t *testing.T) {
	eng := liveWholesaleEngine(t)
	a, b := newAccessStore(t, eng, "buyer-a"), newAccessStore(t, eng, "buyer-b")
	decideAs(t, eng, signedIn(a.staff), a.appID, "approve")
	before := storeApplicationCount(t, eng, a)
	_, addErr := eng.Execute(signedIn(b.staff), addBuyerCall(a.site, a.store, "Finch Cycles", "Ada Example", "finch@example.com"))
	if addErr == nil || !strings.Contains(addErr.Error(), memql.CodeCapabilityNotHeld) {
		t.Errorf("B's person adding on A's site: %v, want %s", addErr, memql.CodeCapabilityNotHeld)
	}
	_, removeErr := eng.Execute(signedIn(b.staff), removeBuyerCall(a.appID, ""))
	if removeErr == nil || !strings.Contains(removeErr.Error(), memql.CodeCapabilityNotHeld) {
		t.Errorf("B's person removing A's buyer: %v, want %s", removeErr, memql.CodeCapabilityNotHeld)
	}
	_, unknownErr := eng.Execute(signedIn(b.staff), removeBuyerCall("no-such-application-"+b.appID, ""))
	if removeErr == nil || unknownErr == nil || removeErr.Error() != unknownErr.Error() {
		t.Errorf("a foreign application answers %v, an unknown one %v; want the same answer", removeErr, unknownErr)
	}
	bound := "site-buyer-b-on-a-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seedSiteIn(t, eng, bound, b.merchant, a.store, b.account)
	for _, store := range []string{a.store, "v1:shopify:store:" + a.store} {
		_, err := eng.Execute(signedIn(b.staff), addBuyerCall(bound, store, "Finch Cycles", "Ada Example", "finch@example.com"))
		if err == nil || !strings.Contains(err.Error(), "another organization") {
			t.Errorf("store named %q: B's person adding through B's site on A's store: %v", store, err)
		}
	}
	if after := storeApplicationCount(t, eng, a); after != before {
		t.Fatalf("refused adds wrote %d applications on A's store", after-before)
	}
	if rows := decisionsOf(t, eng, signedIn(a.staff), a.appID); len(rows) != 1 {
		t.Fatalf("a refused removal left %d decisions on A's buyer", len(rows))
	}
}

// A STAFF ADD OF SOMETHING THAT IS NOT AN ADDRESS is refused by name, as the
// shopper's form refuses it, and nothing is written.
func TestAddingABuyerRefusesSomethingThatIsNotAnAddress(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "add-notaddress")
	before := storeApplicationCount(t, eng, s)
	for _, email := range []string{"not an address", "kestrel.example.com", "a@b@example.com", "@example.com", "ada@"} {
		_, err := eng.Execute(signedIn(s.staff), addBuyerCall(s.site, s.store, "Kestrel Cycles", "Ada Example", email))
		if err == nil || !strings.Contains(err.Error(), "applicantEmail") {
			t.Errorf("adding %q: %v, want a refusal naming applicantEmail", email, err)
		}
	}
	if after := storeApplicationCount(t, eng, s); after != before {
		t.Fatalf("refused adds wrote %d applications", after-before)
	}
}

// unsetStore is an organization whose person holds the wholesale part, and a
// site of it bound to a store whose wholesale program nobody has set up: no
// settings row and no application.
func unsetStore(t *testing.T, eng *memql.MemQLEngine, label string) accessStore {
	t.Helper()
	merchant, store, _ := wholesaleScope()
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	s := accessStore{merchant: merchant + "-" + label, store: store + "-" + label,
		account: "acct-" + label + "-" + n, site: "site-" + label + "-" + n, staff: "user-staff-" + label + "-" + n}
	group := seedOrganization(t, eng, s.account)
	grantGroup(t, eng, group, "read", "app:deployables")
	grantGroup(t, eng, group, "execute", "app:deployables/wholesale")
	seedPersonIn(t, eng, s.staff, s.staff, "writer", true, group, s.account, "added")
	own := "group-own-" + label + "-" + n
	seedGroup(t, eng, own, "custom", s.account)
	seedPersonIn(t, eng, s.merchant, s.merchant, "writer", true, own, s.account, "added")
	seedSiteIn(t, eng, s.site, s.merchant, s.store, s.account)
	s.owner = siteOwnerOf(t, eng, s.site)
	return s
}

// unsetApplications is how many applications exist on a store, whoever owns
// them, under either spelling of its id.
func unsetApplications(t *testing.T, eng *memql.MemQLEngine, store string) int {
	t.Helper()
	n := 0
	for _, form := range []string{store, "v1:shopify:store:" + store} {
		n += len(wsRows(t, eng, seedCtx(), `query applicationsForStore(storeId: `+quote(form)+`)`))
	}
	return n
}

// A STORE NOBODY HAS SET UP TAKES NO BUYER: the add is refused with the
// sentence wholesaleUpdateSettings answers, and nothing is written.
func TestAddingABuyerToAStoreThatIsNotSetUpIsRefused(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := unsetStore(t, eng, "add-unset")
	_, err := eng.Execute(signedIn(s.staff), addBuyerCall(s.site, s.store, "Kestrel Cycles", "Ada Example", "ada@kestrel.example.com"))
	if err == nil || !strings.Contains(err.Error(), wholesalepack.NotSetUpSentence) {
		t.Fatalf("an add on a store with no settings: %v, want %q", err, wholesalepack.NotSetUpSentence)
	}
	if n := unsetApplications(t, eng, s.store); n != 0 {
		t.Fatalf("the refused add wrote %d applications", n)
	}
}

// ANOTHER ORGANIZATION'S HOLDER, THROUGH THEIR OWN SITE BOUND TO A STORE NOT SET
// UP, adds nobody: with no settings row there is no organization to check the
// site against, and the refusal is the store's not being set up.
func TestAHolderInAnotherOrganizationCannotAddABuyerToAStoreNotSetUp(t *testing.T) {
	eng := liveWholesaleEngine(t)
	a, b := unsetStore(t, eng, "unset-a"), unsetStore(t, eng, "unset-b")
	bound := "site-unset-b-on-a-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seedSiteIn(t, eng, bound, b.merchant, a.store, b.account)
	for _, store := range []string{a.store, "v1:shopify:store:" + a.store} {
		_, err := eng.Execute(signedIn(b.staff), addBuyerCall(bound, store, "Kestrel Cycles", "Ada Example", "ada@kestrel.example.com"))
		if err == nil || !strings.Contains(err.Error(), wholesalepack.NotSetUpSentence) {
			t.Errorf("store named %q: B's person adding through B's site on A's store not set up: %v", store, err)
		}
	}
	if n := unsetApplications(t, eng, a.store); n != 0 {
		t.Fatalf("refused adds wrote %d applications on A's store", n)
	}
}
