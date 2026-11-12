package wholesalepack_test

// open_application_db_test.go -- ONE OPEN APPLICATION PER ADDRESS ON A STORE
// (rulings 59 and 61), against a real database and a real engine.
//
// A storefront post from an address that already has a submitted or an
// approved application on the store writes nothing -- no application, no row of
// the client's extension -- and is answered exactly as an accepted post, so
// nobody posting the form learns who a store's wholesale buyers are. Two posts
// in one instant can still both be written, as can rows written before the
// rule, so an approve is refused while another application of the same address
// on the store is approved.
//
// Postgres-gated like every *_db_test.go.

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	memqldsl "github.com/znasllc-io/memql/dsl"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// northwindTaxDetail is the fixture client's extension concept: its row is
// written beside the pack's application by the shopper handler.
const northwindTaxDetail = "v1:northwind:northwindTaxDetail"

// withNorthwind mounts the fixture client whose extension adds an EIN to the
// application form, so a post writes the client's own row too.
func withNorthwind(t *testing.T) {
	t.Helper()
	memqldsl.RegisterTree("northwind", os.DirFS(filepath.Join("testdata", "clients", "northwind")))
	t.Cleanup(func() { memqldsl.UnregisterTree("northwind") })
}

// storeConceptRows is how many distinct rows of concept name the store, read
// straight from the table: what was written, whoever may read it.
func storeConceptRows(t *testing.T, concept, storeID string) int {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	defer db.Close()
	var n int
	if err := db.NewRaw(`SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept = ? AND payload->>'storeId' = ?`,
		concept, storeID).Scan(context.Background(), &n); err != nil {
		t.Fatalf("count %s on %s: %v", concept, storeID, err)
	}
	return n
}

// postFrom is a shopper's post on s's site with an EIN for the extension.
func postFrom(t *testing.T, eng *memql.MemQLEngine, s accessStore, company, email string) *httptest.ResponseRecorder {
	t.Helper()
	return postApplication(t, eng, s.site, s.store, s.owner, url.Values{
		"companyName": {company}, "applicantName": {"Ada Example"}, "applicantEmail": {email}, "ein": {"98-7654321"},
	})
}

// sameAnswer fails unless the two answers are the same to the poster, field by
// field: the status, every header (the redirect target included) and the body.
func sameAnswer(t *testing.T, accepted, repeat *httptest.ResponseRecorder) {
	t.Helper()
	if accepted.Code != http.StatusSeeOther || !strings.HasPrefix(accepted.Header().Get("Location"), "/wholesale/thank-you") {
		t.Fatalf("the accepted post answered %d %q, want 303 to the thank-you page", accepted.Code, accepted.Header().Get("Location"))
	}
	if repeat.Code != accepted.Code {
		t.Errorf("status: the repeat answered %d, the accepted post %d", repeat.Code, accepted.Code)
	}
	if got, want := repeat.Header().Get("Location"), accepted.Header().Get("Location"); got != want {
		t.Errorf("Location: the repeat answered %q, the accepted post %q", got, want)
	}
	if !reflect.DeepEqual(repeat.Header(), accepted.Header()) {
		t.Errorf("headers: the repeat answered %v, the accepted post %v", repeat.Header(), accepted.Header())
	}
	if !bytes.Equal(repeat.Body.Bytes(), accepted.Body.Bytes()) {
		t.Errorf("body: the repeat answered %q, the accepted post %q", repeat.Body.String(), accepted.Body.String())
	}
}

// writtenCounts is the store's applications, extension rows and decisions.
func writtenCounts(t *testing.T, s accessStore) [3]int {
	t.Helper()
	return [3]int{storeConceptRows(t, applicationConcept, s.store), storeConceptRows(t, northwindTaxDetail, s.store),
		storeConceptRows(t, "v1:wholesale:applicationDecision", s.store)}
}

// A SECOND POST FROM AN ADDRESS WITH A SUBMITTED APPLICATION writes nothing --
// no application, no extension row -- and is answered as an accepted post. The
// address is the staff add's: case and surrounding space make no other address.
func TestAPostFromAnAddressWithASubmittedApplicationWritesNothingAndIsAnsweredAsAnAcceptedOne(t *testing.T) {
	withNorthwind(t)
	eng := liveWholesaleEngine(t)
	s := newAccessStoreWith(t, eng, "open-sub", url.Values{"ein": {"12-3456789"}})
	accepted := postFrom(t, eng, s, "Heron Cycles", "first@heron.example.com")
	before := writtenCounts(t, s)
	if before != [3]int{2, 2, 0} {
		t.Fatalf("before the repeat the store holds %v applications, extension rows and decisions; want [2 2 0]", before)
	}
	for _, email := range []string{"sam@open-sub.example.com", "  SAM@Open-Sub.example.com "} {
		repeat := postFrom(t, eng, s, "Another Name Trading", email)
		sameAnswer(t, accepted, repeat)
		if after := writtenCounts(t, s); after != before {
			t.Fatalf("%q: a post from an address with a submitted application left %v, want %v", email, after, before)
		}
	}
}

// THE SAME AGAINST AN APPROVED APPLICATION.
func TestAPostFromAnAddressWithAnApprovedApplicationWritesNothingAndIsAnsweredAsAnAcceptedOne(t *testing.T) {
	withNorthwind(t)
	eng := liveWholesaleEngine(t)
	s := newAccessStoreWith(t, eng, "open-app", url.Values{"ein": {"12-3456789"}})
	decideAs(t, eng, signedIn(s.staff), s.appID, "approve")
	accepted := postFrom(t, eng, s, "Heron Cycles", "first@heron.example.com")
	before := writtenCounts(t, s)
	repeat := postFrom(t, eng, s, "Trading open-app", "Sam@open-app.example.com")
	sameAnswer(t, accepted, repeat)
	if after := writtenCounts(t, s); after != before {
		t.Fatalf("a post from an address with an approved application left %v, want %v", after, before)
	}
}

// AFTER A REJECT, AND AFTER A REVOKE, the address applies again: a final
// application is not open, and the new post IS written, extension row and all.
func TestAPostFromAnAddressWhoseApplicationIsRejectedOrRevokedIsWritten(t *testing.T) {
	withNorthwind(t)
	eng := liveWholesaleEngine(t)
	for _, final := range [][]string{{"reject"}, {"approve", "revoke"}} {
		label := "open-" + final[len(final)-1]
		s := newAccessStoreWith(t, eng, label, url.Values{"ein": {"12-3456789"}})
		for _, transition := range final {
			decideAs(t, eng, signedIn(s.staff), s.appID, transition)
		}
		before := writtenCounts(t, s)
		rec := postFrom(t, eng, s, "Trading "+label, "sam@"+label+".example.com")
		if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/wholesale/thank-you") {
			t.Fatalf("%v: the post answered %d %q", final, rec.Code, loc)
		}
		if after := writtenCounts(t, s); after[0] != before[0]+1 || after[1] != before[1]+1 {
			t.Fatalf("%v: a post from an address whose application is final left %v, want one more application "+
				"and extension row than %v", final, after, before)
		}
	}
}

// ANOTHER STORE'S OPEN APPLICATION for the same address blocks nothing here.
func TestAnotherStoresOpenApplicationDoesNotBlockThisStores(t *testing.T) {
	eng := liveWholesaleEngine(t)
	a, b := newAccessStore(t, eng, "open-a"), newAccessStore(t, eng, "open-b")
	before := storeConceptRows(t, applicationConcept, b.store)
	rec := postApplication(t, eng, b.site, b.store, b.owner, applicationForm("sam@open-a.example.com"))
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/wholesale/thank-you") {
		t.Fatalf("the post on store b answered %d %q", rec.Code, loc)
	}
	if after := storeConceptRows(t, applicationConcept, b.store); after != before+1 {
		t.Fatalf("store b holds %d applications after a post from store a's applicant, want %d", after, before+1)
	}
	if n := storeConceptRows(t, applicationConcept, a.store); n != 1 {
		t.Fatalf("store a holds %d applications, want its one", n)
	}
}

// submitCall is wholesaleSubmitApplication called directly, as the shopper
// handler renders it.
func submitCall(s accessStore, submissionID, company, email string) string {
	return `builtin wholesaleSubmitApplication(applicantEmail: ` + quote(email) + `, applicantName: "Ada Example", companyName: ` +
		quote(company) + `, siteId: ` + quote(s.site) + `, storeId: ` + quote(s.store) + `, accountId: ` + quote(s.account) +
		`, submissionId: ` + quote(submissionID) + `)`
}

// A DIRECT CALLER IS ANSWERED THE SAME RECEIPT for a post that is written and
// one that is not: the id it named, the store and the company it sent. The
// receipt's own id and time are minted per answer either way.
func TestTheBuiltinAnswersARepeatAsItAnswersAnAcceptedSubmission(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "open-direct")
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	first, repeat := "sub-first-"+n, "sub-repeat-"+n
	got := map[string]map[string]any{
		first:  answerOf(t, eng, asSiteOwner(s.owner), submitCall(s, first, "Heron Cycles", "first@heron.example.com")),
		repeat: answerOf(t, eng, asSiteOwner(s.owner), submitCall(s, repeat, "Heron Cycles", "SAM@open-direct.example.com")),
	}
	want := func(id string) map[string]any {
		return map[string]any{"applicationId": id, "storeId": s.store, "companyName": "Heron Cycles"}
	}
	for id, answer := range got {
		if !reflect.DeepEqual(answer["payload"], want(id)) {
			t.Errorf("%s answered %+v, want %+v", id, answer, want(id))
		}
	}
	if rows := wsRows(t, eng, seedCtx(), `query applicationById(applicationId: `+quote(repeat)+`)`); len(rows) != 0 {
		t.Fatalf("the repeat was written: %+v", rows)
	}
	if n := storeConceptRows(t, applicationConcept, s.store); n != 2 {
		t.Fatalf("the store holds %d applications, want the shopper's and the first direct one", n)
	}
}

// THE STAFF SEE IT: a not-written post leaves one line naming the store and the
// open application, and no line anywhere carries the address.
func TestANotWrittenPostLeavesALineForTheStaffAndNoAddress(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "open-trace")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })

	const address = "Sam@Open-Trace.example.com"
	h := server.NewShopperHandler(engineExecutor{eng}, engineSites{eng}, logger)
	req := httptest.NewRequest(http.MethodPost, memql.ShopperFormPathPrefix+"wholesale/application",
		strings.NewReader(applicationForm(address).Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(memql.ShopperSiteHeader, s.site)
	req.Header.Set(memql.ShopperStoreHeader, s.store)
	req.Header.Set(memql.ShopperOwnerHeader, s.owner)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/wholesale/thank-you") {
		t.Fatalf("the repeat answered %d %q", rec.Code, loc)
	}
	if n := storeConceptRows(t, applicationConcept, s.store); n != 1 {
		t.Fatalf("the store holds %d applications, want its one", n)
	}
	var trace []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(strings.ToLower(line), strings.ToLower(address)) || strings.Contains(line, "@open-trace") {
			t.Errorf("a log line carries the address: %s", line)
		}
		if strings.Contains(line, wholesalepack.OpenApplicationNotWrittenLine) {
			trace = append(trace, line)
		}
	}
	if len(trace) != 1 || !strings.Contains(trace[0], "storeId="+s.store) ||
		!strings.Contains(trace[0], "openApplicationId="+s.appID) {
		t.Fatalf("the staff's trace is %q in %q, want one line naming store %s and application %s",
			trace, buf.String(), s.store, s.appID)
	}
}

// TWO SUBMITTED APPLICATIONS OF ONE ADDRESS, written directly as two posts in
// one instant, or rows written before the rule, leave them (the twins carry the
// address in another case and with surrounding space): approving the first succeeds,
// approving the second is refused naming the first and writes nothing, a reject
// of a third is still allowed, and once the first is revoked the second is
// approved.
func TestAnApproveIsRefusedWhileAnotherApplicationOfTheAddressIsApproved(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "open-race")
	twin := func(label string) string {
		id := "twin-" + label + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		wsExec(t, eng, seedCtx(), `createApplicationRow(applicationId: `+quote(id)+`, ownerUserId: `+quote(s.owner)+
			`, storeId: `+quote(s.store)+`, siteId: `+quote(s.site)+`, accountId: `+quote(s.account)+
			`, companyName: "Trading twin", applicantName: "Sam Rivers", applicantEmail: " SAM@open-race.example.com ")`)
		return id
	}
	second, third := twin("second"), twin("third")
	staff := signedIn(s.staff)
	decideAs(t, eng, staff, s.appID, "approve")

	_, err := eng.Execute(staff, `builtin wholesaleRecordDecision(applicationId: `+quote(second)+`, transition: "approve")`)
	if want := wholesalepack.AnotherApprovedSentence(s.appID); err == nil || err.Error() != want {
		t.Fatalf("approving a second application of an approved address: %v, want %q", err, want)
	}
	if rows := decisionsOf(t, eng, staff, second); len(rows) != 0 {
		t.Fatalf("the refused approve wrote %+v", rows)
	}
	decideAs(t, eng, staff, third, "reject")

	decideAs(t, eng, staff, s.appID, "revoke")
	decideAs(t, eng, staff, second, "approve")
	if _, state := stageOf(t, eng, staff, second); state != wholesalepack.StateApproved {
		t.Fatalf("the second application reads %s after the first was revoked, want approved", state)
	}
}
