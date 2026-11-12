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
// with old decided applications -- one approved and granted, one rejected,
// written before the pack sent email and before rows were tied to their
// organization -- is backfilled while northwind's automations run: every row
// the backfill re-writes publishes its event, and nothing reaches the lane,
// nothing is staged and no email row is written. Then a retry of the old
// buyer's access, after the release, IS mailed through the same automations:
// the trigger path was live all along.
func TestTheBackfillMailsNobodyThroughAClientsAutomations(t *testing.T) {
	fakeAdapterOnce.Do(func() { wholesalepack.RegisterAdapter(fakeAdapter) })
	withNorthwind(t)
	eng := liveWholesaleEngine(t)
	s := newEmailStoreOn(t, eng, "mail-quiet", false, url.Values{"ein": {"12-3456789"}})
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	approved, rejected := "old-granted-"+n, "old-rejected-"+n
	for _, old := range []struct{ id, transition, email string }{
		{approved, "approve", "pat@mail-quiet.example.com"},
		{rejected, "reject", "kim@mail-quiet.example.com"},
	} {
		wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:application", old.id, map[string]any{"ownerUserId": userRef(s.owner),
			"storeId": s.store, "siteId": s.site, "companyName": "Old " + old.transition, "applicantName": "Old Applicant",
			"applicantEmail": old.email}))
		wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:applicationDecision", "dec-"+old.id, map[string]any{
			"applicationId": "v1:wholesale:application:" + old.id, "storeId": s.store, "transition": old.transition,
			"decidedBy": userRef(s.staff), "principalKind": "client", "note": "decided long ago"}))
	}
	wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:entitlement", wholesalepack.EntitlementRowID(approved), map[string]any{
		"ownerUserId": userRef(s.owner), "applicationId": "v1:wholesale:application:" + approved, "storeId": s.store,
		"buyerEmail": "pat@mail-quiet.example.com", "adapter": automationFakeAdapter, "state": "granted", "reference": "wholesale"}))

	// THE CLIENT'S AUTOMATIONS START NOW, over a store whose old rows are written.
	startScheduler(t, eng, "northwind")
	report := answerOf(t, eng, asOperator("operator-"+s.site), `builtin wholesaleBackfillAccounts(storeId: `+quote(s.store)+`)`)
	if countOf(report["tied"]) < 5 {
		t.Fatalf("the backfill tied %v rows, want two applications, two decisions and an entitlement: %v", report["tied"], report)
	}
	// The automations run on the bus's goroutines; give them every chance.
	time.Sleep(3 * time.Second)
	if sent := s.mail.messages(); len(sent) != 0 {
		t.Fatalf("the backfill mailed %d messages through the client's automations", len(sent))
	}
	for _, app := range []string{approved, rejected} {
		for notice, count := range staffNotices(t, app, "dec-"+app) {
			if count != 0 {
				t.Fatalf("the backfill staged the %s notice of %s", notice, app)
			}
		}
		if rows := emailRows(t, app); rows != 0 {
			t.Fatalf("the backfill wrote %d email rows for %s", rows, app)
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
