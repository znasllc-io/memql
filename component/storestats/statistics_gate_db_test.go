package storestats

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
	"github.com/znasllc-io/memql/integrations/shopify"
)

// statistics_gate_db_test.go -- the store statistics read against a real
// engine: WHO may ask for WHICH store, and the frame of the answer.
//
// THE GATE IS TWO LOCKS ON ONE DOOR. The engine judges the construct's
// @requiresCapability("execute", "app:deployables/sales") in the organization
// of the site the call names; the handler judges it again in Go, against the
// site it read under the caller's own context, because a construct of
// another source can reach the same executor without the declaration
// (TestASecondConstructOnTheSameExecutorIsRefusedByTheHandler). Then the
// handler holds the store to the site's own bound or preview store. Every
// refusal of the handler's is one sentence, and none reads a store_sales_
// table: storeSalesQueries proves it on the shared handle.
//
// Postgres-gated: CI's db-tests lane runs this package with MEMQL_REQUIRE_DB=1.

// notServed is the read's one refusal (the plan's Global Constraints).
const notServed = "This deployable does not serve that store for you."

// recordingFrame is a store the walk reads clearly: a store of a city bike
// shop in Madrid, recording since an hour before the test's clock.
func recordingFrame(over map[string]any) map[string]any {
	at := time.Date(2025, 11, 8, 9, 0, 0, 0, time.UTC)
	frame := map[string]any{
		"store_name": "A city bike shop", "store_domain": "acme-widgets.myshopify.com", "currency": "EUR",
		"timezone": "Europe/Madrid", "taxes_included": false, "development": false, "all_orders_readable": true,
		"history_since": time.Date(2025, 9, 11, 22, 0, 0, 0, time.UTC), "verdict_state": "recording",
		"verdict_reasons": []any{}, "verdict_since": at.Add(-24 * time.Hour), "last_clear_at": at,
		"last_change_at": at.Add(-5 * time.Minute), "walked_through": at,
	}
	for k, v := range over {
		frame[k] = v
	}
	return frame
}

// servedStore is a store with a recording frame and a storefront site of an
// organization of its own bound to it.
func servedStore(t *testing.T) (storeID, siteID string) {
	t.Helper()
	storeID = newStore(t)
	storeFrame(t, storeID, recordingFrame(nil))
	return storeID, siteFor(t, storeID, nil)
}

func accountOf(t *testing.T, siteID string) string {
	t.Helper()
	v, _ := siteAccounts.Load(siteID)
	account, _ := v.(string)
	return account
}

// refusedReadingNothing asserts the call was refused with an error holding
// want, and that no store_sales_ statement ran during it.
func refusedReadingNothing(t *testing.T, label string, read func() []string, answer map[string]any, err error, want string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: answered %v, want a refusal", label, answer)
	} else if !strings.Contains(err.Error(), want) {
		t.Errorf("%s: refused with %q, want %q", label, err, want)
	}
	if got := read(); len(got) != 0 {
		t.Errorf("%s: a store_sales_ table was read during a refused call: %v", label, got)
	}
}

func TestEachRefusalIsTheOneSentence(t *testing.T) {
	storeID := newStore(t)
	storeFrame(t, storeID, recordingFrame(nil))
	read := storeSalesQueries(t)

	// A site that does not read for its caller (deleted, so siteById filters
	// it out), a static site, and a system-owned one: each bound to the store.
	for label, over := range map[string]map[string]any{
		"a site that does not read": {"deleted": true},
		"a static site":             {"kind": "static"},
		"a system-owned site":       {"systemOwned": true},
	} {
		site := siteFor(t, storeID, over)
		answer, err := askStatistics(t, memberCtx(t, site, true), site, storeID, "7d")
		refusedReadingNothing(t, label, read, answer, err, notServed)
	}

	// A store the site does not serve, of the same organization and of another.
	site := siteFor(t, storeID, nil)
	sameOrg := newStore(t)
	storeFrame(t, sameOrg, recordingFrame(nil))
	siteFor(t, sameOrg, map[string]any{"accountId": accountOf(t, site)})
	otherOrg := newStore(t)
	storeFrame(t, otherOrg, recordingFrame(nil))
	siteFor(t, otherOrg, nil)
	member := memberCtx(t, site, true)
	read()
	for label, other := range map[string]string{"a store of the same organization": sameOrg, "a store of another organization": otherOrg} {
		answer, err := askStatistics(t, member, site, other, "7d")
		refusedReadingNothing(t, label, read, answer, err, notServed)
	}

	// A bad period: the period's own sentence, before anything is read.
	answer, err := askStatistics(t, member, site, storeID, "1w")
	refusedReadingNothing(t, "a bad period", read, answer, err, "period must be one of 24h, 7d, 30d, 90d or 12m; got \"1w\"")
}

func TestACallerWithoutThePartIsRefusedAndNothingIsRead(t *testing.T) {
	storeID, site := servedStore(t)
	read := storeSalesQueries(t)

	// Read app:deployables through a group grant, without the sales part.
	answer, err := askStatistics(t, memberCtx(t, site, false), site, storeID, "7d")
	refusedReadingNothing(t, "a member without the part", read, answer, err, memql.CodeCapabilityNotHeld)

	// A signed-in person with no grant at all, and a call with no session.
	answer, err = askStatistics(t, personCtx("dbtest-storestats-nobody-"+uniq(), auth.RoleWriter), site, storeID, "7d")
	refusedReadingNothing(t, "a person with no grant", read, answer, err, memql.CodeCapabilityNotHeld)
	answer, err = askStatistics(t, context.Background(), site, storeID, "7d")
	refusedReadingNothing(t, "no session", read, answer, err, memql.CodeCapabilityNotHeld)
}

func TestAHolderOfThePartInAnotherOrganizationIsRefusedAndNothingIsRead(t *testing.T) {
	storeA, siteA := servedStore(t)
	storeB, siteB := servedStore(t)
	holderInA := memberCtx(t, siteA, true)
	read := storeSalesQueries(t)

	if _, err := askStatistics(t, holderInA, siteA, storeA, "7d"); err != nil {
		t.Fatalf("the holder is refused in their own organization: %v", err)
	}
	read()

	notYours, err := askStatistics(t, holderInA, siteB, storeB, "7d")
	refusedReadingNothing(t, "the part held in another organization", read, notYours, err, memql.CodeCapabilityNotHeld)

	// "No such site" and "not yours" are the same answer.
	_, noSuch := askStatistics(t, holderInA, "dbtest-storestats-no-such-site-"+uniq(), storeB, "7d")
	if err == nil || noSuch == nil || err.Error() != noSuch.Error() {
		t.Errorf("not yours answered %v and no such site %v: want one answer", err, noSuch)
	}
}

func TestASiteThatDoesNotServeTheStoreIsRefusedBeforeAnyFactIsRead(t *testing.T) {
	_, site := servedStore(t)
	other := newStore(t)
	storeFrame(t, other, recordingFrame(nil))
	read := storeSalesQueries(t)
	answer, err := askStatistics(t, memberCtx(t, site, true), site, other, "7d")
	refusedReadingNothing(t, "a store the site does not serve", read, answer, err, notServed)
}

func TestAnUnknownOrMalformedSiteIsRefusedWithNoFallback(t *testing.T) {
	storeID, _ := servedStore(t)
	eng, _ := statsEngine(t)
	read := storeSalesQueries(t)
	// A cluster owner holds the part everywhere: if any of these fell back to
	// the caller's global capability, the owner would be answered.
	for label, call := range map[string]string{
		"a missing siteId":   `builtin storeStatistics(storeId: ` + q(storeID) + `, period: "7d")`,
		"an empty siteId":    `builtin storeStatistics(siteId: "", storeId: ` + q(storeID) + `, period: "7d")`,
		"a malformed siteId": `builtin storeStatistics(siteId: "v1:platform:site:../ x", storeId: ` + q(storeID) + `, period: "7d")`,
		"an unknown siteId":  `builtin storeStatistics(siteId: ` + q("dbtest-storestats-unknown-"+uniq()) + `, storeId: ` + q(storeID) + `, period: "7d")`,
	} {
		res, err := eng.Execute(ownerCtx(), call)
		var answer map[string]any
		if err == nil {
			if rows := memql.MaterializeRows(res); len(rows) > 0 {
				answer = rows[0]
			}
		}
		refusedReadingNothing(t, label, read, answer, err, memql.CodeCapabilityNotHeld)
	}
}

func TestASiteNoOrganizationOwnsAdmitsOnlyAGlobalHolderOfThePart(t *testing.T) {
	storeID := newStore(t)
	storeFrame(t, storeID, recordingFrame(nil))
	owner := "dbtest-storestats-personal-" + uniq()
	seedRow(t, "v1:identity:user", owner, map[string]any{"displayName": "A site owner", "primaryEmail": owner + "@example.test",
		"role": string(auth.RoleWriter), "active": true})
	site := siteFor(t, storeID, map[string]any{"accountId": nil, "ownerUserId": "v1:identity:user:" + owner})
	read := storeSalesQueries(t)

	// THE EXISTING QUIRK: with no account the engine's organization gate
	// does not handle the call, and it falls through to the global check of
	// the declared part. The site's own owner, at writer, reader or admin,
	// holds no global part and is refused there.
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleReader, auth.RoleAdmin} {
		answer, err := askStatistics(t, personCtx(owner, role), site, storeID, "7d")
		refusedReadingNothing(t, "the personal site's owner as "+string(role), read, answer, err, memql.CodeCapabilityNotHeld)
	}
	// A cluster owner holds the part globally and reads every site.
	if answer := mustAsk(t, ownerCtx(), site, storeID, "7d"); answer["storeId"] != storeID {
		t.Fatalf("a cluster owner was answered %v", answer)
	}
}

func TestAClusterOwnerAndDeveloperAreAdmittedAndAnAdminIsNot(t *testing.T) {
	storeID, site := servedStore(t)
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper} {
		answer, err := askStatistics(t, personCtx("dbtest-storestats-staff-"+uniq(), role), site, storeID, "7d")
		if err != nil || answer["storeId"] != storeID {
			t.Errorf("a cluster %s was answered %v, %v: want the store's frame", role, answer, err)
		}
	}
	read := storeSalesQueries(t)
	answer, err := askStatistics(t, personCtx("dbtest-storestats-admin-"+uniq(), auth.RoleAdmin), site, storeID, "7d")
	refusedReadingNothing(t, "a cluster admin", read, answer, err, memql.CodeCapabilityNotHeld)
}

func TestACallerWhoIsNotAClusterOwnerGetsAnAnswer(t *testing.T) {
	storeID, site := servedStore(t)
	read := storeSalesQueries(t)
	answer := mustAsk(t, memberCtx(t, site, true), site, storeID, "7d")
	if answer["storeId"] != storeID || answer["siteId"] != site || answer["destination"] != "production" {
		t.Fatalf("answer = %v, want the store, the site and production", answer)
	}
	if len(read()) == 0 {
		t.Fatal("the answer read no store_sales_ table")
	}
}

func TestASiteBoundOnlyThroughItsPreviewBindingIsAnsweredAsTesting(t *testing.T) {
	storeID := newStore(t)
	storeFrame(t, storeID, recordingFrame(nil))
	previewOnly := siteFor(t, storeID, map[string]any{"binding": map[string]any{}, "previewBinding": map[string]any{"storeId": storeID}})
	if got := mustAsk(t, ownerCtx(), previewOnly, storeID, "7d")["destination"]; got != "testing" {
		t.Errorf("a store bound through the preview binding is answered as %v, want testing", got)
	}
	both := siteFor(t, storeID, map[string]any{"previewBinding": map[string]any{"storeId": storeID}})
	if got := mustAsk(t, ownerCtx(), both, storeID, "7d")["destination"]; got != "both" {
		t.Errorf("a store both bindings name is answered as %v, want both", got)
	}
}

func TestASiteBoundUnderTheCanonicalFormOfTheStoreIdStillServesIt(t *testing.T) {
	storeID := newStore(t)
	storeFrame(t, storeID, recordingFrame(nil))
	canonical := "v1:shopify:store:" + storeID
	boundCanonical := siteFor(t, canonical, nil)
	if got := mustAsk(t, ownerCtx(), boundCanonical, storeID, "7d"); got["storeId"] != storeID || got["destination"] != "production" {
		t.Errorf("a site bound canonically, asked bare, answered %v", got)
	}
	boundBare := siteFor(t, storeID, nil)
	if got := mustAsk(t, ownerCtx(), boundBare, canonical, "7d"); got["storeId"] != storeID || got["storeName"] != "A city bike shop" {
		t.Errorf("a site bound bare, asked canonically, answered %v; want the bare store and its frame", got)
	}
}

// recordingEngine stands in front of the engine and keeps the context every
// call was made with.
type recordingEngine struct {
	*memql.MemQLEngine
	mu    sync.Mutex
	calls []recordedCall
}

type recordedCall struct {
	query string
	ctx   context.Context
}

func (r *recordingEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, recordedCall{query: query, ctx: ctx})
	r.mu.Unlock()
	return r.MemQLEngine.Execute(ctx, query)
}

// statisticsHandler is the integration's statistics capability, or a fatal
// failure when it offers none.
func statisticsHandler(t *testing.T, i *Integration) func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
	t.Helper()
	for _, c := range i.Capabilities() {
		if c.Name == "statistics" {
			return c.Handler
		}
	}
	t.Fatal("the integration offers no statistics capability")
	return nil
}

func TestTheReadCarriesNoInternalOrigin(t *testing.T) {
	storeID, site := servedStore(t)
	eng, db := statsEngine(t)
	rec := &recordingEngine{MemQLEngine: eng}
	i := NewIntegration(rec, func() *bun.DB { return db }, quietLogger())
	caller := memberCtx(t, site, true)
	callerID, _ := auth.AccessFromContext(caller)

	if _, err := statisticsHandler(t, i)(caller, map[string]any{"siteId": site, "storeId": storeID, "period": "7d"}, 0); err != nil {
		t.Fatalf("the handler refused a holder of the part: %v", err)
	}
	var siteReads int
	for _, c := range rec.calls {
		if !strings.Contains(c.query, "siteById") {
			continue
		}
		siteReads++
		if auth.OriginFromContext(c.ctx).IsInternal() {
			t.Errorf("the site was read with internal origin: %s", c.query)
		}
		if ac, _ := auth.AccessFromContext(c.ctx); ac == nil || ac.UserId != callerID.UserId {
			t.Errorf("the site was read as %v, want the caller %s", ac, callerID.UserId)
		}
	}
	if siteReads != 1 {
		t.Errorf("the handler read the site %d times through the engine, want once: %v", siteReads, rec.calls)
	}
}

func TestTheFrameSaysTheStoreItsCurrencyItsZoneAndItsKind(t *testing.T) {
	storeID := newStore(t)
	storeFrame(t, storeID, recordingFrame(map[string]any{"development": true, "taxes_included": true}))
	site := siteFor(t, storeID, nil)
	got := mustAsk(t, ownerCtx(), site, storeID, "7d")
	want := map[string]any{"storeName": "A city bike shop", "storeDomain": "acme-widgets.myshopify.com", "currency": "EUR",
		"timezone": "Europe/Madrid", "development": true, "taxesIncluded": true, "olderOrderChangesNotRecorded": false}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}

	// A row that holds none of them: each is null, never "" or false.
	bare := newStore(t)
	storeFrame(t, bare, map[string]any{"verdict_state": "recording"})
	got = mustAsk(t, ownerCtx(), siteFor(t, bare, nil), bare, "7d")
	for _, k := range []string{"storeName", "storeDomain", "currency", "timezone", "development", "taxesIncluded", "historySince", "lastChangeAt"} {
		if v, present := got[k]; !present || v != nil {
			t.Errorf("%s = %v (present %v), want null", k, v, present)
		}
	}
}

func TestThePeriodIsCutInTheStoresZone(t *testing.T) {
	storeID, site := servedStore(t)
	atNow(t, time.Date(2025, 11, 8, 10, 17, 0, 0, time.UTC))
	got := mustAsk(t, ownerCtx(), site, storeID, "7d")
	if got["period"] != "7d" || got["bucket"] != "day" || got["start"] != "2025-11-02T00:00:00+01:00" ||
		got["end"] != "2025-11-09T00:00:00+01:00" || got["prevStart"] != "2025-10-26T00:00:00+02:00" {
		t.Errorf("7d = period %v bucket %v start %v end %v prevStart %v", got["period"], got["bucket"], got["start"], got["end"], got["prevStart"])
	}
	series, _ := got["series"].([]any)
	if len(series) != 7 {
		t.Fatalf("series holds %d entries, want 7", len(series))
	}
	for i, e := range series {
		entry := e.(map[string]any)
		want := fmt.Sprintf("2025-11-%02dT00:00:00+01:00", 2+i)
		if entry["at"] != want {
			t.Errorf("series[%d].at = %v, want %s", i, entry["at"], want)
		}
		for _, k := range []string{"totalSales", "netSales", "orders", "grossSales", "discounts", "refunds", "unitsSold"} {
			if v, present := entry[k]; !present || v != nil {
				t.Errorf("series[%d].%s = %v (present %v), want null", i, k, v, present)
			}
		}
		if len(entry) != 8 {
			t.Errorf("series[%d] = %v, want at and seven figures", i, entry)
		}
	}

	year := mustAsk(t, ownerCtx(), site, storeID, "12m")
	if v, present := year["prevStart"]; !present || v != nil {
		t.Errorf("12m prevStart = %v (present %v), want null", v, present)
	}
	if year["start"] != "2024-11-01T00:00:00+01:00" || len(year["series"].([]any)) != 13 {
		t.Errorf("12m start %v with %d buckets", year["start"], len(year["series"].([]any)))
	}
}

func recordingOf(t *testing.T, answer map[string]any) map[string]any {
	t.Helper()
	r, ok := answer["recording"].(map[string]any)
	if !ok {
		t.Fatalf("recording = %v, want an object", answer["recording"])
	}
	return r
}

func TestABlockedStoreIsAnsweredAsOfItsLastClearRun(t *testing.T) {
	clear := time.Date(2025, 11, 8, 9, 0, 0, 0, time.UTC)
	const clearInMadrid = "2025-11-08T10:00:00+01:00"
	orderCheck := []any{map[string]any{"code": "check_failing", "sentence": "Orders are not checked."}}
	cases := []struct {
		label   string
		frame   map[string]any
		asOf    any
		blocked bool
	}{
		{"a failing order check", map[string]any{"verdict_state": "needs attention", "verdict_reasons": orderCheck, "recording_blocked": true}, clearInMadrid, true},
		{"paused", map[string]any{"verdict_state": "paused", "recording_blocked": true}, clearInMadrid, true},
		{"not connected", map[string]any{"verdict_state": "not connected", "recording_blocked": true}, clearInMadrid, true},
		// The pause and the connection are stored states of their own: a
		// paused store is answered blocked whatever recording_blocked holds.
		{"paused, recording_blocked unset", map[string]any{"verdict_state": "paused"}, clearInMadrid, true},
		{"recording", map[string]any{"verdict_state": "recording"}, nil, false},
		{"needs attention, nothing blocked", map[string]any{"verdict_state": "needs attention", "verdict_reasons": orderCheck}, nil, false},
	}
	for _, c := range cases {
		storeID := newStore(t)
		storeFrame(t, storeID, recordingFrame(c.frame))
		got := mustAsk(t, ownerCtx(), siteFor(t, storeID, nil), storeID, "7d")
		if got["asOf"] != c.asOf {
			t.Errorf("%s: asOf = %v, want %v", c.label, got["asOf"], c.asOf)
		}
		if r := recordingOf(t, got); r["blocked"] != c.blocked || r["state"] != c.frame["verdict_state"] {
			t.Errorf("%s: recording = %v, want state %v and blocked %v", c.label, r, c.frame["verdict_state"], c.blocked)
		}
		if got["rebuilding"] != false {
			t.Errorf("%s: rebuilding = %v, want false", c.label, got["rebuilding"])
		}
	}
	_ = clear

	rebuilding := newStore(t)
	storeFrame(t, rebuilding, recordingFrame(map[string]any{"rebuilding_since": clear}))
	if got := mustAsk(t, ownerCtx(), siteFor(t, rebuilding, nil), rebuilding, "7d"); got["rebuilding"] != true {
		t.Errorf("rebuilding_since set: rebuilding = %v, want true", got["rebuilding"])
	}
}

func TestAReasonThatFreezesNothingIsStillInTheAnswer(t *testing.T) {
	storeID := newStore(t)
	privacy := "A privacy request has been failing for more than a day."
	storeFrame(t, storeID, recordingFrame(map[string]any{"verdict_state": "needs attention",
		"verdict_reasons": []any{map[string]any{"code": "privacy_request_failing", "sentence": privacy}}}))
	got := mustAsk(t, ownerCtx(), siteFor(t, storeID, nil), storeID, "7d")
	if got["asOf"] != nil {
		t.Errorf("asOf = %v, want null: the reason stands in the way of nothing", got["asOf"])
	}
	r := recordingOf(t, got)
	reasons, _ := r["reasons"].([]any)
	if r["blocked"] != false || len(reasons) != 1 {
		t.Fatalf("recording = %v, want not blocked with the one reason", r)
	}
	if reason := reasons[0].(map[string]any); reason["code"] != "privacy_request_failing" || reason["sentence"] != privacy || len(reason) != 2 {
		t.Errorf("reason = %v, want its code and its sentence", reason)
	}
	if r["since"] != "2025-11-07T10:00:00+01:00" {
		t.Errorf("since = %v, want the verdict's own since in the store's zone", r["since"])
	}
}

var eightFigures = []string{"totalSales", "netSales", "orders", "averageOrderValue", "grossSales", "discounts", "refunds", "unitsSold"}

func assertEightNulls(t *testing.T, label string, answer map[string]any) {
	t.Helper()
	figures, ok := answer["figures"].(map[string]any)
	if !ok || len(figures) != len(eightFigures) {
		t.Fatalf("%s: figures = %v, want the eight", label, answer["figures"])
	}
	for _, k := range eightFigures {
		if v, present := figures[k]; !present || v != nil {
			t.Errorf("%s: figures.%s = %v (present %v), want null, never 0", label, k, v, present)
		}
	}
}

func TestABlockedStoreWithNoClearRunAnswersNoFigure(t *testing.T) {
	storeID := newStore(t)
	reasons := []any{map[string]any{"code": "grant_lost", "sentence": "The app's access to this store was removed."}}
	storeFrame(t, storeID, recordingFrame(map[string]any{"verdict_state": "needs attention", "verdict_reasons": reasons,
		"recording_blocked": true, "last_clear_at": nil}))
	got := mustAsk(t, ownerCtx(), siteFor(t, storeID, nil), storeID, "7d")
	if v, present := got["asOf"]; !present || v != nil {
		t.Errorf("asOf = %v (present %v), want null", v, present)
	}
	assertEightNulls(t, "blocked before any clear run", got)
	r := recordingOf(t, got)
	if r["blocked"] != true {
		t.Errorf("recording.blocked = %v, want true", r["blocked"])
	}
	raw, _ := json.Marshal(r["reasons"])
	if string(raw) != `[{"code":"grant_lost","sentence":"The app's access to this store was removed."}]` {
		t.Errorf("recording.reasons = %s, want the stored reasons", raw)
	}
}

func TestAStoreWithNoZoneIsCutInUTC(t *testing.T) {
	atNow(t, time.Date(2025, 11, 8, 10, 17, 0, 0, time.UTC))
	for label, zone := range map[string]string{"no zone": "", "a zone that does not load": "Mars/Olympus_Mons"} {
		storeID := newStore(t)
		storeFrame(t, storeID, recordingFrame(map[string]any{"timezone": zone}))
		got := mustAsk(t, ownerCtx(), siteFor(t, storeID, nil), storeID, "7d")
		if got["periodZone"] != "UTC" {
			t.Errorf("%s: periodZone = %v, want UTC", label, got["periodZone"])
		}
		if got["start"] != "2025-11-02T00:00:00Z" || got["end"] != "2025-11-09T00:00:00Z" {
			t.Errorf("%s: start %v end %v, want UTC midnights", label, got["start"], got["end"])
		}
	}
}

// TestTheAnswerNamesTheZoneItsPeriodWasCutIn: periodZone is the zone every
// day of the answer was cut in, and timezone the zone the store recorded, so
// a page tells "no zone recorded" (timezone null) from "a recorded zone that
// could not be loaded" (timezone set, periodZone UTC), and never shows UTC
// days as the store's own.
func TestTheAnswerNamesTheZoneItsPeriodWasCutIn(t *testing.T) {
	for _, c := range []struct {
		zone             string
		timezone, period any
	}{
		{"Europe/Madrid", "Europe/Madrid", "Europe/Madrid"},
		{"", nil, "UTC"},
		{"Mars/Olympus_Mons", "Mars/Olympus_Mons", "UTC"},
	} {
		storeID := newStore(t)
		storeFrame(t, storeID, recordingFrame(map[string]any{"timezone": c.zone}))
		got := mustAsk(t, ownerCtx(), siteFor(t, storeID, nil), storeID, "7d")
		if got["timezone"] != c.timezone || got["periodZone"] != c.period {
			t.Errorf("recorded %q: timezone %v, periodZone %v; want %v and %v", c.zone, got["timezone"], got["periodZone"], c.timezone, c.period)
		}
	}
	// A store the walk holds no row for has no period cut, and says so.
	storeID := newStore(t)
	got := mustAsk(t, ownerCtx(), siteFor(t, storeID, nil), storeID, "7d")
	if v, present := got["periodZone"]; !present || v != nil {
		t.Errorf("a store with no row: periodZone = %v (present %v), want null", v, present)
	}
}

func TestAGrantWithoutAllOrdersSaysOlderChangesAreNotRecorded(t *testing.T) {
	storeID := newStore(t)
	approval := []any{map[string]any{"code": "approval_outdated", "sentence": "Approve the app's new permissions in Shopify."}}
	storeFrame(t, storeID, recordingFrame(map[string]any{"all_orders_readable": false, "verdict_state": "needs attention",
		"verdict_reasons": approval}))
	got := mustAsk(t, ownerCtx(), siteFor(t, storeID, nil), storeID, "7d")
	if got["olderOrderChangesNotRecorded"] != true {
		t.Errorf("olderOrderChangesNotRecorded = %v, want true", got["olderOrderChangesNotRecorded"])
	}
	if r := recordingOf(t, got); r["blocked"] != false || got["asOf"] != nil {
		t.Errorf("recording = %v, asOf %v: an approval that lacks only read_all_orders freezes nothing", r, got["asOf"])
	}
}

func TestAStoreTheWalkHasNotSeenYetIsAnsweredAsStarting(t *testing.T) {
	storeID := newStore(t) // no store_sales_store row
	site := siteFor(t, storeID, nil)
	got := mustAsk(t, ownerCtx(), site, storeID, "7d")
	if got["storeId"] != storeID || got["siteId"] != site || got["destination"] != "production" {
		t.Errorf("ids = %v %v %v, want the store's, the site's and production", got["storeId"], got["siteId"], got["destination"])
	}
	if r := recordingOf(t, got); r["state"] != "starting" || r["blocked"] != false {
		t.Errorf("recording = %v, want starting", r)
	}
	for _, k := range []string{"storeName", "storeDomain", "currency", "timezone", "development", "taxesIncluded",
		"olderOrderChangesNotRecorded", "bucket", "start", "end", "prevStart", "historySince", "lastChangeAt", "asOf",
		"rebuilding", "previous", "buyers", "wholesale", "productCount"} {
		if v, present := got[k]; !present || v != nil {
			t.Errorf("%s = %v (present %v), want null", k, v, present)
		}
	}
	assertEightNulls(t, "a store the walk has not seen", got)
}

// answerKeys is every key of the answer, as the brief lists them.
var answerKeys = []string{"storeId", "siteId", "storeName", "storeDomain", "destination", "currency", "timezone", "development",
	"taxesIncluded", "olderOrderChangesNotRecorded", "period", "bucket", "start", "end", "prevStart", "historySince",
	"lastChangeAt", "asOf", "periodZone", "recording", "rebuilding", "figures", "previous", "floors", "series", "productsBySales",
	"productsByUnits", "productCount", "refundsWithoutItem", "incompleteOrders", "incompleteRefunds", "refundsNotRead",
	"buyers", "wholesale", "otherCurrencies"}

func TestAStartingStoresFiguresAreNullAndEveryKeyIsThere(t *testing.T) {
	storeID := newStore(t)
	storeFrame(t, storeID, recordingFrame(map[string]any{"verdict_state": "starting", "last_clear_at": nil}))
	got := mustAsk(t, ownerCtx(), siteFor(t, storeID, nil), storeID, "30d")
	for _, k := range answerKeys {
		if _, present := got[k]; !present {
			t.Errorf("the answer has no %s", k)
		}
	}
	if len(got) != len(answerKeys) {
		t.Errorf("the answer has %d keys, want exactly the %d of the brief: %v", len(got), len(answerKeys), got)
	}
	assertEightNulls(t, "a starting store", got)
	for _, k := range []string{"previous", "buyers", "wholesale", "productCount", "refundsWithoutItem", "incompleteOrders", "incompleteRefunds", "refundsNotRead"} {
		if got[k] != nil {
			t.Errorf("%s = %v, want null", k, got[k])
		}
	}
	for _, k := range []string{"productsBySales", "productsByUnits", "otherCurrencies"} {
		if list, ok := got[k].([]any); !ok || len(list) != 0 {
			t.Errorf("%s = %v, want an empty list", k, got[k])
		}
	}
	if floors, ok := got["floors"].(map[string]any); !ok || len(floors) != 0 {
		t.Errorf("floors = %v, want empty", got["floors"])
	}
}

func TestNothingPersonalReachesTheAnswerOrTheLog(t *testing.T) {
	storeID, site := servedStore(t)
	eng, db := statsEngine(t)
	email := "buyer-1-" + uniq() + "@example.test"
	buyerKey := "dbtest-storestats-buyer-key-" + uniq()
	buyerName := "Robin Recognisable"
	gid := "gid://shopify/Order/" + uniq()
	now := time.Now().UTC().Format(time.RFC3339)
	rowID := shopify.MirrorRowID(storeID, gid)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:order' AND id = ?`, "v1:shopify:order:"+rowID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM store_sales_order_fact WHERE store_id = ?`, storeID)
	})
	payload, _ := json.Marshal(map[string]any{"storeId": storeID, "gid": gid, "deleted": false, "email": email, "name": "#1001",
		"currencyCode": "EUR", "processedAt": now, "updatedAt": now, "syncedAt": now,
		"billingAddress": map[string]any{"name": buyerName}})
	connector := auth.ContextWithInternalOrigin(auth.ContextWithConnectorActor(context.Background(), "shopify"))
	if _, err := eng.Execute(connector, fmt.Sprintf(`insert("v1:shopify:order", id=%s, payload=%s)`, langparser.QuoteString(rowID), payload)); err != nil {
		t.Fatalf("write the order: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO store_sales_order_fact (store_id, order_gid, order_version,
		recorded_at, ordered_at, currency, cancelled, test, taxes_included, buyer_key, shipping, tax, duties, expected_quantity)
		VALUES (?, ?, now(), now(), now(), 'EUR', false, false, false, ?, 0, 0, 0, 1)`, storeID, gid, buyerKey); err != nil {
		t.Fatalf("write the order's fact: %v", err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	i := NewIntegration(eng, func() *bun.DB { return db }, logger)
	handler := statisticsHandler(t, i)
	nodes, err := handler(ownerCtx(), map[string]any{"siteId": site, "storeId": storeID, "period": "24h"}, 0)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("the read answered %v, %v", nodes, err)
	}
	// And a refusal, whose sentence and log must hold nothing either.
	_, refusal := handler(ownerCtx(), map[string]any{"siteId": site, "storeId": buyerKey, "period": "24h"}, 0)
	if refusal == nil || refusal.Error() != notServed {
		t.Fatalf("a store the site does not serve answered %v", refusal)
	}
	for _, personal := range []string{email, buyerKey, buyerName, gid} {
		if bytes.Contains(nodes[0].Payload, []byte(personal)) {
			t.Errorf("the answer holds %q", personal)
		}
		if strings.Contains(logs.String(), personal) {
			t.Errorf("the log holds %q", personal)
		}
	}
}

// ---- a construct of another source ----

// fixtureEngine boots a second engine over the shared database with the
// given non-engine domains mounted, as a pack or a client package's domain is
// mounted (memqldsl.RegisterTree, what the reviews pack's live tests use),
// and registers this package's provider on it. The shared engine's registry
// was built at its own boot, so it never sees these domains.
func fixtureEngine(t *testing.T, domains map[string]fs.FS) (*memql.MemQLEngine, *Integration) {
	t.Helper()
	shared, db := statsEngine(t)
	if shared == nil {
		t.FailNow()
	}
	for name, tree := range domains {
		memqldsl.RegisterTree(name, tree)
		domain := name
		t.Cleanup(func() { memqldsl.UnregisterTree(domain) })
	}
	eng, err := memql.New(db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	eng.Logger = quietLogger()
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("Init with the fixture domains: %v", err)
	}
	integ := NewIntegration(eng, func() *bun.DB { return db }, quietLogger())
	integ.now = statsEngineState.integ.now
	if err := eng.RegisterIntegration(integ); err != nil {
		t.Fatalf("RegisterIntegration: %v", err)
	}
	return eng, integ
}

func fixtureDomain(builtins string) fs.FS {
	return fstest.MapFS{
		"memql.toml":     {Data: []byte("memql = \"1.0\"\nedition = \"2026\"\n")},
		"builtins.memql": {Data: []byte(builtins)},
	}
}

func statisticsCall(builtin, siteID, storeID, period string) string {
	return `builtin ` + builtin + `(siteId: ` + q(siteID) + `, storeId: ` + q(storeID) + `, period: ` + q(period) + `)`
}

func TestASecondConstructOnTheSameExecutorIsRefusedByTheHandler(t *testing.T) {
	storeID, site := servedStore(t)
	atNow(t, time.Date(2025, 11, 8, 10, 17, 0, 0, time.UTC))
	eng, _ := fixtureEngine(t, map[string]fs.FS{"storestatsfixturea": fixtureDomain(`@executor("integration.storeStats.statistics")
@args(profile="object")
@description("Another source's builtin on the store statistics executor, with no capability requirement.")
builtin statisticsWithoutAPart {
  siteId   string!
  storeId  string!
  period   string!
}
`)})
	call := statisticsCall("storestatsfixturea.statisticsWithoutAPart", site, storeID, "7d")
	read := storeSalesQueries(t)

	for label, ctx := range map[string]context.Context{
		"a reader with no grant":                       personCtx("dbtest-storestats-reader-"+uniq(), auth.RoleReader),
		"a member without the part":                    memberCtx(t, site, false),
		"a holder of the part in another organization": memberCtx(t, siteFor(t, newStore(t), nil), true),
	} {
		answer, err := askOn(t, eng, ctx, call)
		refusedReadingNothing(t, label, read, answer, err, notServed)
	}

	// A holder of the part in the site's organization is answered exactly as
	// through the engine's own builtin.
	holder := memberCtx(t, site, true)
	fixture, err := askOn(t, eng, holder, call)
	if err != nil {
		t.Fatalf("a holder of the part was refused through the second construct: %v", err)
	}
	engine := mustAsk(t, holder, site, storeID, "7d")
	a, _ := json.Marshal(fixture)
	b, _ := json.Marshal(engine)
	if string(a) != string(b) {
		t.Errorf("the second construct answered\n%s\nand the builtin\n%s", a, b)
	}
}

// TestAConstructOfTheSameNameInAnotherDomainDoesNotOpenTheEnginesBuiltin: a
// domain of another source declares a builtin of the engine's builtin's name,
// on the same executor and with no capability requirement.
//
// What the engine does today, pinned only as "refused, nothing read" so the
// engine's own fix (a construct's name belonging to the engine's constructs,
// and the execution gate failing closed) changes no assertion here:
//   - the bare name names two constructs, and the call is "not found";
//   - the engine's builtin lives in the namespace shopify/overlay, which no
//     call string can name: the parser refuses the slash, and the dotted
//     spellings are not found;
//   - the other domain's construct, named by its own namespace, reaches the
//     handler with no engine gate at all, and the handler refuses it.
func TestAConstructOfTheSameNameInAnotherDomainDoesNotOpenTheEnginesBuiltin(t *testing.T) {
	storeID, site := servedStore(t)
	eng, _ := fixtureEngine(t, map[string]fs.FS{"storestatsfixtureb": fixtureDomain(`@executor("integration.storeStats.statistics")
@args(profile="object")
@description("Another source's builtin of the engine's builtin's name.")
builtin storeStatistics {
  siteId   string!
  storeId  string!
  period   string!
}
`)})
	engines, err := eng.Functions().Get("shopify/overlay.storeStatistics")
	if err != nil || engines == nil || engines.Executor != "integration.storeStats.statistics" || engines.RequiresCapability.Resource != salesPart {
		t.Fatalf("the engine's builtin is %v (%v), want it under shopify/overlay on this executor with the part", engines, err)
	}
	_, lookup := eng.Functions().Get("storeStatistics")
	t.Logf("the bare name in the fixture engine: %v", lookup)

	read := storeSalesQueries(t)
	reader := personCtx("dbtest-storestats-reader-"+uniq(), auth.RoleReader)
	member := memberCtx(t, site, false)
	for _, name := range []string{"storeStatistics", "shopify/overlay.storeStatistics", "shopify.overlay.storeStatistics", "shopify.storeStatistics"} {
		answer, err := askOn(t, eng, reader, statisticsCall(name, site, storeID, "7d"))
		// Today: "not found" for every name the engine resolves, and the
		// parser's refusal for the slash. The engine's own fix may resolve a
		// name to its construct instead, and then its gate or the handler
		// refuses: those are the only refusals this call may meet.
		meant := false
		if err != nil {
			for _, refusal := range []string{"not found", "unexpected token after expression", memql.CodeCapabilityNotHeld, notServed} {
				meant = meant || strings.Contains(err.Error(), refusal)
			}
		}
		if !meant {
			t.Errorf("%s as a reader with no grant: answered %v, %v; want a refusal of the name, of the gate or of the handler", name, answer, err)
			continue
		}
		refusedReadingNothing(t, name+" as a reader with no grant", read, answer, err, err.Error())
		t.Logf("%s as a reader with no grant: %v", name, err)
	}
	for label, ctx := range map[string]context.Context{"a reader with no grant": reader, "a member without the part": member} {
		answer, err := askOn(t, eng, ctx, statisticsCall("storestatsfixtureb.storeStatistics", site, storeID, "7d"))
		refusedReadingNothing(t, "the other domain's storeStatistics as "+label, read, answer, err, notServed)
	}
}

// TestAPurgingOrRebuildingStoreIsNotCurrent: a store whose facts are being
// deleted (purging) or walked again from the start (rebuilding_since) is never
// answered as current, whatever its verdict: it is blocked, with a state of
// its own the page names, and with no asOf, because no figure of it can be
// given yet. A purge comes first: a rebuild sets both, and its facts are
// deleted before it is walked again.
func TestAPurgingOrRebuildingStoreIsNotCurrent(t *testing.T) {
	since := time.Date(2025, 11, 8, 9, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		label   string
		frame   map[string]any
		state   string
		blocked bool
	}{
		{"purging", map[string]any{"purging": true}, "purging", true},
		{"purging for a rebuild", map[string]any{"purging": true, "rebuilding_since": since}, "purging", true},
		{"rebuilding", map[string]any{"rebuilding_since": since}, "rebuilding", true},
		{"both cleared", map[string]any{"purging": false, "rebuilding_since": nil}, "recording", false},
	} {
		storeID := newStore(t)
		storeFrame(t, storeID, recordingFrame(c.frame))
		got := mustAsk(t, ownerCtx(), siteFor(t, storeID, nil), storeID, "7d")
		r := recordingOf(t, got)
		if r["state"] != c.state || r["blocked"] != c.blocked {
			t.Errorf("%s: recording = %v, want state %s and blocked %v", c.label, r, c.state, c.blocked)
		}
		if v, present := got["asOf"]; !present || v != nil {
			t.Errorf("%s: asOf = %v (present %v), want null", c.label, v, present)
		}
	}
}

// TestBorrowedAuthorityIsNeverAnsweredRevenue: the shopper surface runs a
// declared read under the site owner's borrowed authority, in the site's
// account (component/server/shopper_handler.go, shopperContext). Under it the
// read refuses whatever the borrowed person holds: a page's call always
// carries the person's own session.
func TestBorrowedAuthorityIsNeverAnsweredRevenue(t *testing.T) {
	storeID, site := servedStore(t)
	holder := memberOf(t, site, true)
	if _, err := askStatistics(t, personCtx(holder, auth.RoleWriter), site, storeID, "7d"); err != nil {
		t.Fatalf("the holder is refused in their own session: %v", err)
	}
	borrowed := auth.ContextWithCallAccount(auth.ContextWithUserActor(context.Background(), "v1:identity:user:"+holder), accountOf(t, site))
	read := storeSalesQueries(t)
	answer, err := askStatistics(t, borrowed, site, storeID, "7d")
	refusedReadingNothing(t, "the holder's borrowed authority, through the engine", read, answer, err, notServed)
	nodes, err := statisticsHandler(t, testIntegration(t))(borrowed, map[string]any{"siteId": site, "storeId": storeID, "period": "7d"}, 0)
	refusedReadingNothing(t, "the holder's borrowed authority, at the handler", read, map[string]any{"nodes": len(nodes)}, err, notServed)
}

// TestTheHandlerAndTheEnginesGateAgree: for every kind of caller the review
// probed, the handler's answer about the part is the engine's gate's, because
// both ask the engine's one method (memql's RequirementHeldFor). The engine's
// gate is read through the real builtin: refused with capability_not_held, or
// passed. The handler is called directly with the same caller. Two callers
// are left to their own tests: borrowed authority, which the handler refuses
// on purpose (TestBorrowedAuthorityIsNeverAnsweredRevenue), and internal
// origin, which the engine's gate passes before any requirement is judged and
// the handler does not (TestInternalOriginIsJudgedByItsActor).
func TestTheHandlerAndTheEnginesGateAgree(t *testing.T) {
	storeID, site := servedStore(t)
	otherStore, otherSite := servedStore(t)
	_ = otherStore
	// A personal site, owned by a person who holds the part only through a
	// group of an organization: the engine admits them (its global path for a
	// site no organization owns asks every group they are in).
	personalStore := newStore(t)
	storeFrame(t, personalStore, recordingFrame(nil))
	orgHolder := memberOf(t, site, true)
	personal := siteFor(t, personalStore, map[string]any{"accountId": nil, "ownerUserId": "v1:identity:user:" + orgHolder})
	plainOwner := "dbtest-storestats-plain-" + uniq()
	seedRow(t, "v1:identity:user", plainOwner, map[string]any{"displayName": "A site owner", "primaryEmail": plainOwner + "@example.test",
		"role": string(auth.RoleWriter), "active": true})
	plainSite := siteFor(t, personalStore, map[string]any{"accountId": nil, "ownerUserId": "v1:identity:user:" + plainOwner})

	type call struct {
		label       string
		ctx         context.Context
		site, store string
		admitted    bool
	}
	calls := []call{
		{"a holder of the part in the site's organization", memberCtx(t, site, true), site, storeID, true},
		{"a member without the part", memberCtx(t, site, false), site, storeID, false},
		{"a holder of the part in another organization", memberCtx(t, otherSite, true), site, storeID, false},
		{"a writer with no grant", personCtx("dbtest-storestats-w-"+uniq(), auth.RoleWriter), site, storeID, false},
		{"a reader with no grant", personCtx("dbtest-storestats-r-"+uniq(), auth.RoleReader), site, storeID, false},
		{"no session", context.Background(), site, storeID, false},
		{"a cluster owner", personCtx("dbtest-storestats-o-"+uniq(), auth.RoleOwner), site, storeID, true},
		{"a cluster developer", personCtx("dbtest-storestats-d-"+uniq(), auth.RoleDeveloper), site, storeID, true},
		{"a cluster admin", personCtx("dbtest-storestats-a-"+uniq(), auth.RoleAdmin), site, storeID, false},
		{"a personal site's owner who holds no part", personCtx(plainOwner, auth.RoleWriter), plainSite, personalStore, false},
		{"a personal site's owner who holds the part through an organization's group", personCtx(orgHolder, auth.RoleWriter), personal, personalStore, true},
	}
	handler := statisticsHandler(t, testIntegration(t))
	for _, c := range calls {
		_, gateErr := askStatistics(t, c.ctx, c.site, c.store, "7d")
		gate := gateErr == nil || !strings.Contains(gateErr.Error(), memql.CodeCapabilityNotHeld)
		_, handlerErr := handler(c.ctx, map[string]any{"siteId": c.site, "storeId": c.store, "period": "7d"}, 0)
		held := handlerErr == nil
		if handlerErr != nil && handlerErr != errNotServed {
			t.Errorf("%s: the handler answered %v, want the one refusal or an answer", c.label, handlerErr)
		}
		if gate != held {
			t.Errorf("%s: the engine's gate admits %v (%v) and the handler %v (%v)", c.label, gate, gateErr, held, handlerErr)
		}
		if held != c.admitted {
			t.Errorf("%s: admitted %v, want %v", c.label, held, c.admitted)
		}
	}
}

// TestInternalOriginIsJudgedByItsActor: the engine's gate passes a call with
// internal origin before it judges any requirement (refuseBelowRequiredCapability).
// The handler does not take internal origin as a pass: it judges the actor
// the call carries, so the cluster's own maintenance principal is answered and
// an actorless or connector context is not. This is the one disagreement left
// standing, reported rather than settled (the task 17 report, fix round 1).
func TestInternalOriginIsJudgedByItsActor(t *testing.T) {
	storeID, site := servedStore(t)
	handler := statisticsHandler(t, testIntegration(t))
	args := map[string]any{"siteId": site, "storeId": storeID, "period": "7d"}
	for label, c := range map[string]struct {
		ctx      context.Context
		admitted bool
	}{
		"the maintenance principal":           {auth.ContextWithInternalOrigin(principalCtx(t, "storeStatisticsRollupSweep")), true},
		"internal origin with no actor":       {auth.ContextWithInternalOrigin(context.Background()), false},
		"the shopify connector":               {auth.ContextWithInternalOrigin(auth.ContextWithConnectorActor(context.Background(), "shopify")), false},
		"a member without the part, internal": {auth.ContextWithInternalOrigin(memberCtx(t, site, false)), false},
	} {
		_, err := handler(c.ctx, args, 0)
		if (err == nil) != c.admitted || (err != nil && err != errNotServed) {
			t.Errorf("%s: the handler answered %v, want admitted %v", label, err, c.admitted)
		}
	}
}
