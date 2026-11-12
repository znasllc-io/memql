package wholesalepack_test

// email_automation_db_test.go -- THE TEN SENDS THROUGH A CLIENT'S OWN
// AUTOMATIONS, against a real database with the automation scheduler running.
//
// testdata/clients/northwind wires every send and notice as the pack's guide
// asks a client to: the seven the pack does not guard on the first version of
// the application, acceptance or decision row, the three it does on every
// entitlement version. These tests drive the REAL trigger path -- rows written,
// events published, the scheduler running northwind's automations -- and never
// call a send directly. NO EMAIL LEAVES: the lane's provider is the recorder of
// email_db_test.go.
//
// Postgres-gated like every *_db_test.go.

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// mailedTo is the messages handed over to address.
func (s emailStore) mailedTo(address string) int {
	n := 0
	for _, m := range s.mail.messages() {
		if m.To == address {
			n++
		}
	}
	return n
}

// waitFor polls until done answers true, for at most d.
func waitFor(d time.Duration, done func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if done() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return done()
}

// staffNotices is how many versions of each notice of application are staged.
func staffNotices(t *testing.T, appID string, decisions ...string) map[string]int {
	t.Helper()
	out := map[string]int{
		"readyToReview":    versionCount(t, "v1:platform:outboundRequest", wholesalepack.StaffRequestID(wholesalepack.StaffEventReadyToReview, appID, "")),
		"accessNotGranted": 0,
		"accessNotRemoved": 0,
	}
	for attempt := 0; attempt <= 3; attempt++ {
		out["accessNotGranted"] += versionCount(t, "v1:platform:outboundRequest",
			wholesalepack.AccessStaffRequestID(wholesalepack.StaffEventAccessNotGranted, appID, attempt))
		out["accessNotRemoved"] += versionCount(t, "v1:platform:outboundRequest",
			wholesalepack.AccessStaffRequestID(wholesalepack.StaffEventAccessNotRemoved, appID, attempt))
	}
	for _, d := range decisions {
		for _, event := range []string{wholesalepack.StaffEventApproved, wholesalepack.StaffEventRejected, wholesalepack.StaffEventRevoked} {
			out[event] += versionCount(t, "v1:platform:outboundRequest", wholesalepack.StaffRequestID(event, appID, d))
		}
	}
	return out
}

// emailRows is how many email rows of each kind the application has, across
// its first three attempts.
func emailRows(t *testing.T, appID string) int {
	t.Helper()
	n := 0
	for _, kind := range wholesalepack.EmailKinds {
		for attempt := 1; attempt <= 3; attempt++ {
			n += versionCount(t, "v1:wholesale:emailSend", wholesalepack.EmailSendRowID(kind, appID, attempt))
		}
	}
	return n
}

// THE ACCOUNT BACKFILL MAILS NOBODY THROUGH A CLIENT'S AUTOMATIONS. A store
// that requires its terms, with old applications written before the pack sent
// email and before rows were tied to their organization -- one approved and
// granted, one rejected, one removed, one still waiting for its terms and one
// that accepted them -- is backfilled while northwind's automations run: every
// row the backfill re-writes publishes its event, and nothing reaches the lane,
// nothing is staged and no email row is written. Then a retry of the old
// buyer's access, after the release, IS mailed through the same automations:
// the trigger path was live all along.
//
// EACH OLD ROW IS THERE FOR A FILTER. northwind runs seven of its sends on the
// first version of a row, each under a filter of its own, and the backfill's
// versions are not first versions. With a filter removed, the row it guards is
// mailed or told of here, and the test fails:
//
//   - the waiting application: the terms email (northwindTermsOnApplication);
//   - the approved one's decision: the approved notice (northwindToldOnApproval);
//   - the rejected one's: the rejection email and notice
//     (northwindMailedOnRejection, northwindToldOnRejection);
//   - the removed one's: the removal email and notice (northwindMailedOnRemoval,
//     northwindToldOnRemoval).
//
// The seventh, the ready notice on an acceptance (northwindReadyOnAcceptance),
// has its row seeded too -- an old acceptance of an application ready to review
// -- but the backfill writes no version of an acceptance, so nothing here runs
// that automation with or without its filter.
func TestTheBackfillMailsNobodyThroughAClientsAutomations(t *testing.T) {
	fakeAdapterOnce.Do(func() { wholesalepack.RegisterAdapter(fakeAdapter) })
	withNorthwind(t)
	eng := liveWholesaleEngine(t)
	s := newEmailStoreOn(t, eng, "mail-quiet", true, url.Values{"ein": {"12-3456789"}})
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	approved, rejected, removed := "old-granted-"+n, "old-rejected-"+n, "old-removed-"+n
	waiting, accepted := "old-waiting-"+n, "old-accepted-"+n
	// THE REMOVED BUYER'S LOG IS ITS ONE REMOVAL: what the fold reads of it is
	// the same before and after the backfill re-writes it.
	old := []struct{ id, transition, email string }{
		{approved, "approve", "pat@mail-quiet.example.com"},
		{rejected, "reject", "kim@mail-quiet.example.com"},
		{removed, "revoke", "uma@mail-quiet.example.com"},
		{waiting, "", "ray@mail-quiet.example.com"},
		{accepted, "", "ola@mail-quiet.example.com"},
	}
	for _, o := range old {
		wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:application", o.id, map[string]any{"ownerUserId": userRef(s.owner),
			"storeId": s.store, "siteId": s.site, "companyName": "Old " + o.id, "applicantName": "Old Applicant",
			"applicantEmail": o.email}))
		if o.transition == "" {
			continue
		}
		wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:applicationDecision", "dec-"+o.id, map[string]any{
			"applicationId": "v1:wholesale:application:" + o.id, "storeId": s.store, "transition": o.transition,
			"decidedBy": userRef(s.staff), "principalKind": "client", "note": "decided long ago"}))
	}
	wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:entitlement", wholesalepack.EntitlementRowID(approved), map[string]any{
		"ownerUserId": userRef(s.owner), "applicationId": "v1:wholesale:application:" + approved, "storeId": s.store,
		"buyerEmail": "pat@mail-quiet.example.com", "adapter": automationFakeAdapter, "state": "granted", "reference": "wholesale"}))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:acceptance", wholesalepack.AcceptanceRowID(accepted), map[string]any{
		"ownerUserId": userRef(s.owner), "storeId": s.store, "applicationId": "v1:wholesale:application:" + accepted,
		"applicantEmail": "ola@mail-quiet.example.com", "signedName": "Old Applicant", "acceptedAt": now,
		"termsVersionId": "old-terms", "termsSha256": strings.Repeat("0", 64), "mapVersionId": "old-map",
		"mapSha256": strings.Repeat("0", 64), "statement": "I accept."}))
	// THE SEEDS ARE WHAT EACH FILTER GUARDS: each old application is in the state
	// its email or notice is sent in, so a send that ran would not be declined.
	for id, want := range map[string]string{approved: "approved", rejected: "rejected", removed: "revoked",
		waiting: "waitingForTerms", accepted: "readyToReview"} {
		if got := answerOf(t, eng, asOperator("operator-"+s.site), `builtin wholesaleGet(applicationId: `+quote(id)+`)`)["stage"]; got != want {
			t.Fatalf("the old application %s is seeded in stage %v, want %s", id, got, want)
		}
	}

	// THE CLIENT'S AUTOMATIONS START NOW, over a store whose old rows are written.
	startScheduler(t, eng, "northwind")
	report := answerOf(t, eng, asOperator("operator-"+s.site), `builtin wholesaleBackfillAccounts(storeId: `+quote(s.store)+`)`)
	if countOf(report["tied"]) < 9 {
		t.Fatalf("the backfill tied %v rows, want five applications, three decisions and an entitlement: %v", report["tied"], report)
	}
	// The automations run on the bus's goroutines; give them every chance.
	time.Sleep(3 * time.Second)
	if sent := s.mail.messages(); len(sent) != 0 {
		var to []string
		for _, m := range sent {
			to = append(to, m.To)
		}
		t.Fatalf("the backfill mailed %d messages through the client's automations, to %v", len(sent), to)
	}
	for _, o := range old {
		for notice, count := range staffNotices(t, o.id, "dec-"+o.id) {
			if count != 0 {
				t.Fatalf("the backfill staged the %s notice of %s", notice, o.id)
			}
		}
		if rows := emailRows(t, o.id); rows != 0 {
			t.Fatalf("the backfill wrote %d email rows for %s", rows, o.id)
		}
	}

	// THE TRIGGER PATH IS LIVE: a retry of the old buyer's access is attempt 1,
	// and northwind's automation on the granted version mails it once.
	openApplications(t, eng, asSiteOwner(s.owner), s.store, automationFakeAdapter)
	if _, err := eng.Execute(signedIn(s.staff), `builtin wholesaleRetryAccess(applicationId: `+quote(approved)+`)`); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !waitFor(10*time.Second, func() bool { return s.mailedTo("pat@mail-quiet.example.com") >= 1 }) {
		t.Fatal("the retry's granted version mailed nothing through the client's automation")
	}
	time.Sleep(2 * time.Second)
	if got := s.mailedTo("pat@mail-quiet.example.com"); got != 1 || len(s.mail.messages()) != 1 {
		t.Fatalf("after the retry %d messages to the buyer and %d in all, want one", got, len(s.mail.messages()))
	}
	if lines := s.logs.lines("@mail-quiet"); lines != "" {
		t.Fatalf("a log line carries an applicant's address:\n%s", lines)
	}
}

// A NEW APPLICATION AFTER THE RELEASE IS MAILED WHAT IT SHOULD BE, ONCE, through
// the client's automations: the terms email when it arrives, the readyToReview
// notice when the terms are accepted, the approved notice on the decision, and
// the approval email when the grant lands -- each once, and nothing else.
func TestANewApplicationIsMailedOnceAtEachStepThroughAClientsAutomations(t *testing.T) {
	fakeAdapterOnce.Do(func() { wholesalepack.RegisterAdapter(fakeAdapter) })
	withNorthwind(t)
	eng := liveWholesaleEngine(t)
	startScheduler(t, eng, "northwind")
	s := newEmailStoreOn(t, eng, "mail-flow", true, url.Values{"ein": {"12-3456789"}})
	openApplications(t, eng, asSiteOwner(s.owner), s.store, automationFakeAdapter)
	wsExec(t, eng, signedIn(s.staff), `builtin wholesaleUpdateSettings(siteId: `+quote(s.site)+`, storeId: `+quote(s.store)+
		`, termsRequired: true)`)

	applicant := "lee@mail-flow.example.com"
	rec := postApplication(t, eng, s.site, s.store, s.owner, url.Values{"companyName": {"Flow Trading"},
		"applicantName": {"Lee Flow"}, "applicantEmail": {applicant}, "ein": {"98-7654321"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the application post answered %d", rec.Code)
	}
	appID := ""
	for _, row := range wsRows(t, eng, signedIn(s.merchant), `query applicationsForStore(storeId: `+quote(s.store)+`)`) {
		if asString(row["applicantEmail"]) == applicant {
			appID = bareID(asString(row["id"]))
		}
	}
	if appID == "" {
		t.Fatal("the new application is not readable")
	}

	// THE TERMS EMAIL, when the application arrives.
	if !waitFor(10*time.Second, func() bool { return s.mailedTo(applicant) == 1 }) {
		t.Fatalf("the new application was mailed %d terms emails, want one", s.mailedTo(applicant))
	}
	var terms string
	for _, m := range s.mail.messages() {
		if m.To == applicant {
			terms = linkToken(t, m, "/wholesale/terms")
		}
	}
	access := s.accessStore
	access.appID = appID
	code, read := readTerms(t, eng, access, terms)
	docs := listRows(read["documents"])
	if code != http.StatusOK || len(docs) != 2 {
		t.Fatalf("the mailed terms link answered %d %v", code, read)
	}
	if rec := postAcceptance(t, eng, access, url.Values{"requestToken": {terms}, "acceptTerms": {"on"}, "acceptMap": {"on"},
		"signedName": {"Lee Flow"}, "termsSha256": {asString(docs[0]["sha256"])}, "mapSha256": {asString(docs[1]["sha256"])},
		"statementSha256": {asString(read["statementSha256"])}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("the acceptance answered %d", rec.Code)
	}
	// THE READY NOTICE, when the terms are accepted.
	if !waitFor(10*time.Second, func() bool { return staffNotices(t, appID)["readyToReview"] == 1 }) {
		t.Fatal("the acceptance staged no readyToReview notice")
	}

	// THE APPROVAL: the approved notice on the decision, the grant through the
	// client's own automation, and the approval email on the granted version.
	if _, err := eng.Execute(signedIn(s.staff), `builtin wholesaleRecordDecision(applicationId: `+quote(appID)+
		`, transition: "approve", note: "known to us")`); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !waitFor(15*time.Second, func() bool { return s.mailedTo(applicant) == 2 }) {
		t.Fatalf("after the approval %d emails reached the applicant, want the terms and the approval", s.mailedTo(applicant))
	}
	decisions := wsRows(t, eng, signedIn(s.staff), `query decisionsForApplication(applicationId: `+quote("v1:wholesale:application:"+appID)+`)`)
	if len(decisions) != 1 {
		t.Fatalf("%d decisions", len(decisions))
	}
	decision := bareID(asString(decisions[0]["id"]))
	if !waitFor(10*time.Second, func() bool { return staffNotices(t, appID, decision)[wholesalepack.StaffEventApproved] == 1 }) {
		t.Fatal("the approval staged no approved notice")
	}

	// AND NOTHING MORE, once everything has had its chance to run again.
	time.Sleep(3 * time.Second)
	if got := s.mailedTo(applicant); got != 2 {
		t.Fatalf("%d emails reached the applicant, want two", got)
	}
	var approval string
	for _, m := range s.mail.messages() {
		if m.To == applicant && strings.Contains(m.TextBody, "/wholesale/price-sheet?t=") {
			approval = m.TextBody
		}
	}
	if approval == "" {
		t.Fatal("no approval email with the price sheet reached the applicant")
	}
	for notice, count := range staffNotices(t, appID, decision) {
		want := 0
		if notice == "readyToReview" || notice == wholesalepack.StaffEventApproved {
			want = 1
		}
		if count != want {
			t.Fatalf("the %s notice has %d versions, want %d", notice, count, want)
		}
	}
	if ent := latestPayload(t, "v1:wholesale:entitlement", wholesalepack.EntitlementRowID(appID)); ent["state"] != "granted" {
		t.Fatalf("the entitlement is %v", ent)
	}
	if lines := s.logs.lines(applicant); lines != "" {
		t.Fatalf("a log line carries the applicant's address:\n%s", lines)
	}
}
