package wholesalepack_test

// happened_db_test.go -- WHEN A THING HAPPENED IS DATA, AND THE BACKFILL KEEPS
// IT, against a real database and a real engine.
//
// A row's write time is the time of its newest version, so any rewrite of the
// row moves it. The pack reads when an application was submitted, when each
// decision was made and when trade prices went on or came off from fields of
// their own; a row written before those fields reads by its write time, and the
// account backfill carries the write time across into the field in the same
// write that ties the row. After a backfill every state, every order and every
// date the pack answers is what it answered before.
//
// Postgres-gated like every *_db_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// happenedStore is an organization, a site of it bound to a fresh store, and
// the store's settings already tied to the organization -- what every
// store-wide read needs -- with applications written before the fields.
type happenedStore struct {
	merchant, store, site, account string
}

func newHappenedStore(t *testing.T, eng *memql.MemQLEngine, label string) happenedStore {
	t.Helper()
	merchant, store, site, account := tiedStore(t, eng, label)
	s := happenedStore{merchant: merchant, store: store, site: site, account: account}
	wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:wholesaleSettings", wholesalepack.SettingsRowID(store), map[string]any{
		"ownerUserId": userRef(merchant), "storeId": store, "applicationsOpen": true, "accountId": account, "siteId": site,
		"entitlementAdapter": wholesalepack.AdapterCustomerTag}))
	return s
}

func (s happenedStore) operator() context.Context { return asOperator("operator-" + s.merchant) }

// oldApplication writes an untied application from before the fields, through
// the store's site, and its decisions in order, real time apart: a log's order
// is its rows' creation order.
func (s happenedStore) oldApplication(t *testing.T, eng *memql.MemQLEngine, label string, transitions ...string) string {
	t.Helper()
	appID := "old-" + label + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:application", appID, map[string]any{
		"ownerUserId": userRef(s.merchant), "storeId": s.store, "siteId": s.site, "companyName": "Old " + label,
		"applicantName": "Pat " + label, "applicantEmail": label + "@example.com"}))
	time.Sleep(5 * time.Millisecond)
	for i, tr := range transitions {
		s.oldDecision(t, eng, appID, appID+"-d"+strconv.Itoa(i), tr, nil)
	}
	return appID
}

// oldDecision writes one untied decision from before the fields, with any extra
// payload fields.
func (s happenedStore) oldDecision(t *testing.T, eng *memql.MemQLEngine, appID, decisionID, transition string, extra map[string]any) {
	t.Helper()
	payload := map[string]any{"applicationId": "v1:wholesale:application:" + appID, "storeId": s.store,
		"transition": transition, "decidedBy": userRef(s.merchant), "principalKind": "client", "note": "long ago"}
	for k, v := range extra {
		payload[k] = v
	}
	wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:applicationDecision", decisionID, payload))
	time.Sleep(5 * time.Millisecond)
}

// oldEntitlement writes the application's untied entitlement from before the
// fields, in state.
func (s happenedStore) oldEntitlement(t *testing.T, eng *memql.MemQLEngine, appID, state string) {
	t.Helper()
	wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:entitlement", wholesalepack.EntitlementRowID(appID), map[string]any{
		"ownerUserId": userRef(s.merchant), "applicationId": "v1:wholesale:application:" + appID, "storeId": s.store,
		"buyerEmail": "buyer@example.com", "adapter": wholesalepack.AdapterCustomerTag, "state": state, "reference": "wholesale"}))
	time.Sleep(5 * time.Millisecond)
}

func (s happenedStore) backfill(t *testing.T, eng *memql.MemQLEngine) map[string]any {
	t.Helper()
	return answerOf(t, eng, s.operator(), `builtin wholesaleBackfillAccounts(storeId: `+quote(s.store)+`)`)
}

// happenedNow is now as a MemQL literal, for a fixture written through one of
// the pack's own mutations, which take the time a thing happened.
func happenedNow() string { return quote(time.Now().UTC().Format(time.RFC3339Nano)) }

// textOf is a row's value as text, a time in RFC3339Nano.
func textOf(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprint(v)
}

// views is everything the pack answers about a store's applications that
// carries a state, an order or a date: each application's page (its state,
// acts, date, decisions and access), the list in the queue's order and in the
// newest-first order, the buyers, the decision log and the export's dates.
type views struct {
	Pages     map[string]string
	Queue     []string
	All       []string
	Buyers    []string
	Log       []string
	Exported  []string
	Decisions map[string][]string // each application's decisions as the export holds them, in order
}

func readViews(t *testing.T, eng *memql.MemQLEngine, s happenedStore, appIDs ...string) views {
	t.Helper()
	op := s.operator()
	v := views{Pages: map[string]string{}, Decisions: map[string][]string{}}
	for _, id := range appIDs {
		got := answerOf(t, eng, op, `builtin wholesaleGet(applicationId: `+quote(id)+`)`)
		app, _ := got["application"].(map[string]any)
		var decisions []string
		for _, d := range listRows(got["decisions"]) {
			decisions = append(decisions, textOf(d["transition"])+"@"+textOf(d["createdAt"]))
		}
		access := "none"
		if ent, ok := got["entitlement"].(map[string]any); ok {
			access = textOf(ent["state"]) + "@" + textOf(ent["updatedAt"])
		}
		acts, _ := json.Marshal(got["acts"])
		v.Pages[id] = fmt.Sprintf("state %v, acts %s, submitted %s, decisions %v, access %s",
			got["state"], acts, textOf(app["createdAt"]), decisions, access)
	}
	wide := `(siteId: ` + quote(s.site) + `, storeId: ` + quote(s.store)
	for _, row := range listRows(answerOf(t, eng, op, `builtin wholesaleList`+wide+`, stage: "readyToReview")`)["rows"]) {
		v.Queue = append(v.Queue, bareID(textOf(row["applicationId"]))+"@"+textOf(row["createdAt"]))
	}
	for _, row := range listRows(answerOf(t, eng, op, `builtin wholesaleList`+wide+`, stage: "all")`)["rows"]) {
		v.All = append(v.All, bareID(textOf(row["applicationId"]))+" "+textOf(row["stage"])+"@"+textOf(row["createdAt"]))
	}
	for _, row := range listRows(answerOf(t, eng, op, `builtin wholesaleBuyers`+wide+`)`)["rows"]) {
		v.Buyers = append(v.Buyers, bareID(textOf(row["applicationId"]))+" "+textOf(row["accessState"])+" since "+textOf(row["grantedAt"]))
	}
	for _, row := range listRows(answerOf(t, eng, op, `builtin wholesaleDecisionLog`+wide+`)`)["rows"]) {
		v.Log = append(v.Log, bareID(textOf(row["applicationId"]))+" "+textOf(row["transition"])+"@"+textOf(row["createdAt"]))
	}
	exported := map[string]any{}
	if err := json.Unmarshal([]byte(textOf(answerOf(t, eng, op, `builtin wholesaleExportStore`+wide+`)`)["json"])), &exported); err != nil {
		t.Fatalf("the export is not JSON: %v", err)
	}
	dates := []string{"createdAt", "submittedAt", "decidedAt", "attemptedAt", "grantedAt", "revokedAt"}
	for _, section := range []string{"applications", "decisions", "entitlements"} {
		for _, row := range listRows(exported[section]) {
			line := section + " " + bareID(textOf(row["id"]))
			for _, k := range dates {
				if val, ok := row[k]; ok && textOf(val) != "" {
					line += " " + k + "=" + textOf(val)
				}
			}
			if section == "applications" {
				line += " state=" + textOf(row["state"])
			}
			if section == "decisions" {
				app := bareID(textOf(row["applicationId"]))
				v.Decisions[app] = append(v.Decisions[app], textOf(row["transition"])+"@"+textOf(row["createdAt"]))
			}
			v.Exported = append(v.Exported, line)
		}
	}
	return v
}

// sameViews fails the test, naming every view that moved.
func sameViews(t *testing.T, when string, before, after views) {
	t.Helper()
	if reflect.DeepEqual(before, after) {
		return
	}
	for id, page := range before.Pages {
		if after.Pages[id] != page {
			t.Errorf("%s: application %s's page moved:\n  before %s\n  after  %s", when, id, page, after.Pages[id])
		}
	}
	for name, pair := range map[string][2][]string{"the queue": {before.Queue, after.Queue}, "the list": {before.All, after.All},
		"the buyers": {before.Buyers, after.Buyers}, "the decision log": {before.Log, after.Log}, "the export": {before.Exported, after.Exported}} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Errorf("%s: %s moved:\n  before %v\n  after  %v", when, name, pair[0], pair[1])
		}
	}
	if !reflect.DeepEqual(before.Decisions, after.Decisions) {
		t.Errorf("%s: the export's decisions moved:\n  before %v\n  after  %v", when, before.Decisions, after.Decisions)
	}
	t.FailNow()
}

func happenedState(t *testing.T, eng *memql.MemQLEngine, s happenedStore, appID string) any {
	t.Helper()
	return answerOf(t, eng, s.operator(), `builtin wholesaleApplicationState(applicationId: `+quote(appID)+`)`)["state"]
}

// decisionRows is the application's decisions as the query answers them.
func decisionRows(t *testing.T, eng *memql.MemQLEngine, s happenedStore, appID string) []map[string]any {
	t.Helper()
	return wsRows(t, eng, s.operator(), `query decisionsForApplication(applicationId: `+quote("v1:wholesale:application:"+appID)+`)`)
}

// THE SCRATCH CASE: an old untied application approved and then revoked. The
// buyer is removed before the backfill and after it, with no act that would
// put trade prices back, and each decision keeps its date.
func TestARemovedBuyerIsStillRemovedAfterTheBackfill(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newHappenedStore(t, eng, "removed")
	app := s.oldApplication(t, eng, "removed", "approve", "revoke")
	s.oldEntitlement(t, eng, app, wholesalepack.EntitlementRevoked)
	before := readViews(t, eng, s, app)
	if got := happenedState(t, eng, s, app); got != wholesalepack.StateRevoked {
		t.Fatalf("before the backfill the application is %v, want revoked", got)
	}
	s.backfill(t, eng)
	if got := happenedState(t, eng, s, app); got != wholesalepack.StateRevoked {
		t.Fatalf("after the backfill the application is %v, want revoked", got)
	}
	after := readViews(t, eng, s, app)
	if strings.Contains(after.Pages[app], "retryAccess") {
		t.Fatalf("after the backfill a removed buyer's page offers retryAccess: %s", after.Pages[app])
	}
	sameViews(t, "after the backfill", before, after)
}

// APPROVED, REVOKED AND APPROVED AGAIN (a log only old rows can hold) stays
// approved through the backfill, its three decisions in their order.
func TestAnApplicationApprovedAgainStaysApprovedAfterTheBackfill(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newHappenedStore(t, eng, "again")
	app := s.oldApplication(t, eng, "again", "approve", "revoke", "approve")
	s.oldEntitlement(t, eng, app, wholesalepack.EntitlementGranted)
	before := readViews(t, eng, s, app)
	if got := happenedState(t, eng, s, app); got != wholesalepack.StateApproved {
		t.Fatalf("before the backfill the application is %v, want approved", got)
	}
	s.backfill(t, eng)
	if got := happenedState(t, eng, s, app); got != wholesalepack.StateApproved {
		t.Fatalf("after the backfill the application is %v, want approved", got)
	}
	sameViews(t, "after the backfill", before, readViews(t, eng, s, app))
}

// A RUN THAT STOPS HALFWAY: the oldest decision is tied as the backfill ties it
// -- its tie and its time in one write -- and the run stops before the newer
// one. The state between the two runs, and after the second, is what it was.
func TestABackfillStoppedHalfwayAndRunAgainKeepsTheLog(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newHappenedStore(t, eng, "halfway")
	app := s.oldApplication(t, eng, "halfway", "approve", "revoke")
	s.oldEntitlement(t, eng, app, wholesalepack.EntitlementRevoked)
	before := readViews(t, eng, s, app)

	var oldest map[string]any
	for _, d := range decisionRows(t, eng, s, app) {
		if textOf(d["transition"]) == "approve" {
			oldest = d
		}
	}
	if oldest == nil {
		t.Fatal("the old approve decision is not readable")
	}
	wsExec(t, eng, seedCtx(), `backfillDecisionRow(accountId: `+quote(s.account)+`, applicationId: `+quote(textOf(oldest["applicationId"]))+
		`, decidedAt: `+quote(textOf(oldest["createdAt"]))+`, decidedBy: `+quote(textOf(oldest["decidedBy"]))+
		`, decisionId: `+quote(bareID(textOf(oldest["id"])))+`, note: `+quote(textOf(oldest["note"]))+
		`, ownerUserId: `+quote(userRef(s.merchant))+`, principalKind: "client", storeId: `+quote(s.store)+`, transition: "approve")`)
	if got := happenedState(t, eng, s, app); got != wholesalepack.StateRevoked {
		t.Fatalf("between the two runs the application is %v, want revoked", got)
	}
	sameViews(t, "between the two runs", before, readViews(t, eng, s, app))

	s.backfill(t, eng)
	if got := happenedState(t, eng, s, app); got != wholesalepack.StateRevoked {
		t.Fatalf("after the second run the application is %v, want revoked", got)
	}
	sameViews(t, "after the second run", before, readViews(t, eng, s, app))
}

// ONE DECISION OF THREE WRITTEN AGAIN, by a write at its id carrying every
// field it held: the log keeps its order and every decision its date.
func TestADecisionRewrittenLaterKeepsItsPlaceAndItsDate(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newHappenedStore(t, eng, "rewritten")
	app := s.oldApplication(t, eng, "rewritten", "approve", "revoke", "approve")
	s.oldEntitlement(t, eng, app, wholesalepack.EntitlementGranted)
	before := readViews(t, eng, s, app)
	s.backfill(t, eng)
	sameViews(t, "after the backfill", before, readViews(t, eng, s, app))

	rows := decisionRows(t, eng, s, app)
	if len(rows) != 3 {
		t.Fatalf("the application has %d decisions, want 3", len(rows))
	}
	oldest := rows[len(rows)-1]
	payload := map[string]any{}
	for k, val := range oldest {
		switch k {
		case "id", "createdAt":
			continue
		}
		if val != nil {
			payload[k] = val
		}
	}
	wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:applicationDecision", bareID(textOf(oldest["id"])), payload))
	if got := versionCount(t, "v1:wholesale:applicationDecision", bareID(textOf(oldest["id"]))); got < 3 {
		t.Fatalf("the oldest decision has %d versions; the case needs it written again", got)
	}
	sameViews(t, "after the oldest decision was written again", before, readViews(t, eng, s, app))
}

// THE QUEUE: three applications submitted one after another. Through a backfill
// the queue keeps its order, oldest first, and each its date.
func TestTheQueueKeepsItsOrderAndItsDatesThroughABackfill(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newHappenedStore(t, eng, "queue")
	first := s.oldApplication(t, eng, "first")
	time.Sleep(20 * time.Millisecond)
	second := s.oldApplication(t, eng, "second")
	time.Sleep(20 * time.Millisecond)
	third := s.oldApplication(t, eng, "third")
	before := readViews(t, eng, s, first, second, third)
	if len(before.Queue) != 3 || !strings.HasPrefix(before.Queue[0], first) || !strings.HasPrefix(before.Queue[2], third) {
		t.Fatalf("before the backfill the queue is %v, want %s, %s, %s", before.Queue, first, second, third)
	}
	s.backfill(t, eng)
	sameViews(t, "after the backfill", before, readViews(t, eng, s, first, second, third))
}

// "TRADE PRICES ON SINCE" is when they went on, and not the backfill's day; a
// removal's date likewise.
func TestTradePricesOnSinceIsNotMovedByABackfill(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newHappenedStore(t, eng, "since")
	granted := s.oldApplication(t, eng, "granted", "approve")
	s.oldEntitlement(t, eng, granted, wholesalepack.EntitlementGranted)
	removed := s.oldApplication(t, eng, "removed", "approve", "revoke")
	s.oldEntitlement(t, eng, removed, wholesalepack.EntitlementRevoked)
	before := readViews(t, eng, s, granted, removed)
	if len(before.Buyers) != 1 || !strings.Contains(before.Buyers[0], "granted since 20") {
		t.Fatalf("before the backfill the buyers are %v, want the granted one with its date", before.Buyers)
	}
	s.backfill(t, eng)
	sameViews(t, "after the backfill", before, readViews(t, eng, s, granted, removed))
}

// THE EXPORT answers when things happened: every date in it is the same after
// a backfill as before.
func TestTheExportAnswersWhenThingsHappenedAfterABackfill(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newHappenedStore(t, eng, "export")
	granted := s.oldApplication(t, eng, "granted", "approve")
	s.oldEntitlement(t, eng, granted, wholesalepack.EntitlementGranted)
	removed := s.oldApplication(t, eng, "removed", "approve", "revoke")
	s.oldEntitlement(t, eng, removed, wholesalepack.EntitlementRevoked)
	waiting := s.oldApplication(t, eng, "waiting")
	before := readViews(t, eng, s, granted, removed, waiting)
	s.backfill(t, eng)
	after := readViews(t, eng, s, granted, removed, waiting)
	if !reflect.DeepEqual(before.Exported, after.Exported) || !reflect.DeepEqual(before.Decisions, after.Decisions) {
		t.Fatalf("the export's dates moved:\n  before %v\n  after  %v\n  decisions before %v\n  decisions after  %v",
			before.Exported, after.Exported, before.Decisions, after.Decisions)
	}
	sameViews(t, "after the backfill", before, after)
}

// A ROW FROM BEFORE THE FIELDS, with no backfill run, reads by its write time
// in every view: the application's date is its row's, each decision's its
// row's, newest first, and the access's its row's.
func TestARowFromBeforeTheFieldsReadsByItsWriteTime(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newHappenedStore(t, eng, "before")
	app := s.oldApplication(t, eng, "before", "approve", "revoke", "approve")
	s.oldEntitlement(t, eng, app, wholesalepack.EntitlementGranted)
	waiting := s.oldApplication(t, eng, "waiting")

	appRow := answerOf(t, eng, s.operator(), `query applicationById(applicationId: `+quote(app)+`)`)
	waitingRow := answerOf(t, eng, s.operator(), `query applicationById(applicationId: `+quote(waiting)+`)`)
	ent := answerOf(t, eng, s.operator(), `query entitlementForApplication(applicationId: `+quote("v1:wholesale:application:"+app)+`)`)
	var decisions, logLines []string
	for _, d := range decisionRows(t, eng, s, app) {
		decisions = append(decisions, textOf(d["transition"])+"@"+textOf(d["createdAt"]))
		logLines = append(logLines, app+" "+textOf(d["transition"])+"@"+textOf(d["createdAt"]))
	}
	got := readViews(t, eng, s, app, waiting)
	wantPage := fmt.Sprintf("submitted %s, decisions %v, access granted@%s",
		textOf(appRow["createdAt"]), decisions, textOf(ent["createdAt"]))
	if !strings.HasPrefix(got.Pages[app], "state approved, ") || !strings.HasSuffix(got.Pages[app], wantPage) {
		t.Fatalf("the application's page answers\n  %s\nwant\n  %s", got.Pages[app], wantPage)
	}
	if want := []string{waiting + "@" + textOf(waitingRow["createdAt"])}; !reflect.DeepEqual(got.Queue, want) {
		t.Fatalf("the queue is %v, want %v", got.Queue, want)
	}
	if want := []string{app + " granted since " + textOf(ent["createdAt"])}; !reflect.DeepEqual(got.Buyers, want) {
		t.Fatalf("the buyers are %v, want %v", got.Buyers, want)
	}
	if !reflect.DeepEqual(got.Log, logLines) {
		t.Fatalf("the decision log is %v, want %v", got.Log, logLines)
	}
	if !reflect.DeepEqual(got.Decisions[app], decisions) {
		t.Fatalf("the export's decisions are %v, want %v", got.Decisions[app], decisions)
	}
}

// A DECISION THE BUILTIN WRITES CARRIES ITS OWN TIME, and a later write of the
// row does not move it, nor the date the application's page shows.
func TestANewDecisionCarriesItsOwnTime(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "own-time")
	if err := s.approve(eng, signedIn(s.staff)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	h := happenedStore{merchant: s.merchant, store: s.store, site: s.site, account: s.account}
	rows := decisionRows(t, eng, h, s.appID)
	if len(rows) != 1 {
		t.Fatalf("the application has %d decisions, want 1", len(rows))
	}
	decidedAt := textOf(rows[0]["decidedAt"])
	at, err := time.Parse(time.RFC3339Nano, decidedAt)
	if err != nil || at.Sub(rowTimeOf(rows[0]["createdAt"])).Abs() > time.Minute {
		t.Fatalf("the decision's decidedAt is %q (%v), want the time it was made, near %v", decidedAt, err, rows[0]["createdAt"])
	}
	page := func() string {
		got := answerOf(t, eng, h.operator(), `builtin wholesaleGet(applicationId: `+quote(s.appID)+`)`)
		d := listRows(got["decisions"])
		if len(d) != 1 {
			t.Fatalf("the page shows %d decisions", len(d))
		}
		return textOf(d[0]["createdAt"])
	}
	shown := page()
	if !rowTimeOf(shown).Equal(at) {
		t.Fatalf("the page dates the decision %s, want its decidedAt %s", shown, decidedAt)
	}
	time.Sleep(20 * time.Millisecond)
	payload := map[string]any{}
	for k, val := range rows[0] {
		if k != "id" && k != "createdAt" && val != nil {
			payload[k] = val
		}
	}
	wsExec(t, eng, seedCtx(), rawInsert("v1:wholesale:applicationDecision", bareID(textOf(rows[0]["id"])), payload))
	again := decisionRows(t, eng, h, s.appID)
	if textOf(again[0]["decidedAt"]) != decidedAt || rowTimeOf(again[0]["createdAt"]).Equal(rowTimeOf(rows[0]["createdAt"])) {
		t.Fatalf("after a rewrite the decision reads decidedAt %v at write time %v; want decidedAt %s kept and a new write time",
			again[0]["decidedAt"], again[0]["createdAt"], decidedAt)
	}
	if got := page(); got != shown {
		t.Fatalf("after a rewrite the page dates the decision %s, want %s", got, shown)
	}
}

// rowTimeOf is a value as a time.
func rowTimeOf(v any) time.Time {
	switch x := v.(type) {
	case time.Time:
		return x
	case string:
		at, _ := time.Parse(time.RFC3339Nano, x)
		return at
	}
	return time.Time{}
}

// A SECOND BACKFILL CHANGES NOTHING: it finds every row the first one wrote
// already tied, writes nothing, and no state, order or date moves.
func TestASecondBackfillChangesNothing(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newHappenedStore(t, eng, "second")
	removed := s.oldApplication(t, eng, "removed", "approve", "revoke")
	s.oldEntitlement(t, eng, removed, wholesalepack.EntitlementRevoked)
	granted := s.oldApplication(t, eng, "granted", "approve")
	s.oldEntitlement(t, eng, granted, wholesalepack.EntitlementGranted)
	waiting := s.oldApplication(t, eng, "waiting")
	before := readViews(t, eng, s, removed, granted, waiting)
	first := s.backfill(t, eng)
	afterFirst := readViews(t, eng, s, removed, granted, waiting)
	versions := map[string]int{}
	for _, id := range []string{removed, granted, waiting} {
		versions[id] = versionCount(t, "v1:wholesale:application", id)
	}
	second := s.backfill(t, eng)
	// Three applications, three decisions, two entitlements, and the settings
	// the first run found tied.
	if countOf(first["tied"]) != 8 || countOf(second["tied"]) != 0 || countOf(second["alreadyTied"]) != 9 {
		t.Fatalf("first run %+v, second run %+v: want the first to tie 8 and the second to find all 9 rows tied", first, second)
	}
	for id, n := range versions {
		if got := versionCount(t, "v1:wholesale:application", id); got != n {
			t.Fatalf("the second run wrote application %s again: %d versions, then %d", id, n, got)
		}
	}
	sameViews(t, "after the first run", before, afterFirst)
	sameViews(t, "after the second run", before, readViews(t, eng, s, removed, granted, waiting))
}

// A TIME THAT IS SET IS NEVER OVERWRITTEN by the backfill, even on a row it
// ties, and the views answer it rather than the row's write time.
func TestTheBackfillNeverOverwritesATimeThatIsSet(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newHappenedStore(t, eng, "set")
	app := s.oldApplication(t, eng, "set")
	decided := time.Now().UTC().Add(-36 * time.Hour).Truncate(time.Millisecond)
	s.oldDecision(t, eng, app, app+"-set", "approve", map[string]any{"decidedAt": decided.Format(time.RFC3339Nano)})
	want := decided.Format(time.RFC3339Nano)
	check := func(when string) {
		t.Helper()
		page := readViews(t, eng, s, app).Pages[app]
		if !strings.Contains(page, "approve@"+want) {
			t.Fatalf("%s: the page answers %s, want the decision dated %s", when, page, want)
		}
		rows := decisionRows(t, eng, s, app)
		if len(rows) != 1 || !rowTimeOf(rows[0]["decidedAt"]).Equal(decided) {
			t.Fatalf("%s: the decision holds decidedAt %v, want %s", when, rows, want)
		}
	}
	check("before the backfill")
	s.backfill(t, eng)
	check("after the backfill")
}
