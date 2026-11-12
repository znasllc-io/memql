package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// watch_test.go -- the watch over every store's verdict, and the two emails.
//
// What to mail is planWatch's to decide, a pure function of the verdict last
// recorded, the verdict computed now, the alert wait and the time: most of
// these call it with no engine at all. The executor's tests drive one store
// through the harness's recording engine, and the handlers' tests the three
// builtins.

var watchNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

const watchDomain = "acme-widgets.myshopify.com"

// verdictIn is a computed verdict in a state, begun at since.
func verdictIn(state string, since time.Time) Verdict {
	v := Verdict{StoreID: testStoreID, Domain: watchDomain, State: state, Since: since}
	if state == VerdictNeedsAttention {
		v.Reasons = []VerdictReason{
			{Code: ReasonEventsNotRegistered, Sentence: "Shopify is not sending every change.", Detail: "orders/create: refused"},
			{Code: ReasonChangesNotRecorded, Sentence: "3 changes could not be recorded.", Count: 3},
		}
	}
	return v
}

// watchedBefore is the verdict of a store the watch had already recorded,
// recording, before the pass a test is about: a store's first sighting is
// its own case (TestAStoreFirstSeenAlreadyNotConnectedIsNeverMailed).
func watchedBefore(v Verdict) map[string]any {
	known := verdictStamp(v.Since.Add(-24 * time.Hour))
	return map[string]any{"state": VerdictRecording, "since": known, "firstRecordedAt": known}
}

// recordedAs is health.verdict as a pass recorded it for v, a store the watch
// already knew, with edits.
func recordedAs(v Verdict, edits ...func(map[string]any)) map[string]any {
	rec := recordOf(v, watchedBefore(v)).payload()
	for _, edit := range edits {
		edit(rec)
	}
	return rec
}

func mailedFor(since time.Time) func(map[string]any) {
	return func(rec map[string]any) {
		rec["mailedFor"] = verdictStamp(since)
		rec["alert"] = map[string]any{"state": AlertStaged, "reason": ""}
	}
}

func recoveredFor(since time.Time) func(map[string]any) {
	return func(rec map[string]any) { rec["recoveredFor"] = verdictStamp(since) }
}

func TestTheAlertIsStagedOnceAfterTheWait(t *testing.T) {
	since := watchNow.Add(-31 * time.Minute)
	v := verdictIn(VerdictNeedsAttention, since)
	plan := planWatch(recordedAs(v), v, defaultStoreAlertWait, watchNow)
	if !plan.Alert {
		t.Fatalf("a store that has needed attention for 31 minutes was not alerted: %+v", plan)
	}
	plan.Record.alertStaged(verdictStamp(since))
	if got := plan.Record.MailedFor; got != verdictStamp(since) {
		t.Errorf("mailedFor = %q after the alert was staged, want this episode %q", got, verdictStamp(since))
	}

	// The next pass reads what this one recorded.
	again := planWatch(plan.Record.payload(), v, defaultStoreAlertWait, watchNow.Add(5*time.Minute))
	if again.Alert {
		t.Error("the alert was staged again on the next pass for the same episode")
	}

	// A disconnected store is told the same way.
	gone := verdictIn(VerdictNotConnected, since)
	if !planWatch(recordedAs(gone), gone, defaultStoreAlertWait, watchNow).Alert {
		t.Error("a store not connected for 31 minutes was not alerted")
	}
}

func TestNothingIsStagedBeforeTheWait(t *testing.T) {
	since := watchNow.Add(-29 * time.Minute)
	v := verdictIn(VerdictNeedsAttention, since)
	if plan := planWatch(recordedAs(v), v, defaultStoreAlertWait, watchNow); plan.Alert || plan.Recover != "" {
		t.Errorf("a store 29 minutes into needing attention was mailed: %+v", plan)
	}
	// Exactly the wait is the wait.
	v = verdictIn(VerdictNeedsAttention, watchNow.Add(-30*time.Minute))
	if !planWatch(recordedAs(v), v, defaultStoreAlertWait, watchNow).Alert {
		t.Error("a store exactly 30 minutes into needing attention was not alerted")
	}
	// The first pass to see a store records its since as now: nothing is owed yet.
	v = verdictIn(VerdictNeedsAttention, watchNow)
	if plan := planWatch(nil, v, defaultStoreAlertWait, watchNow); plan.Alert {
		t.Error("the first pass over a store alerted at once")
	}
}

func TestAStoreThatRecoversBeforeTheWaitIsNeverMailed(t *testing.T) {
	troubled := verdictIn(VerdictNeedsAttention, watchNow.Add(-10*time.Minute))
	back := verdictIn(VerdictRecording, watchNow)
	plan := planWatch(recordedAs(troubled), back, defaultStoreAlertWait, watchNow)
	if plan.Alert || plan.Recover != "" {
		t.Errorf("a store that recovered 10 minutes in was mailed: %+v", plan)
	}
	if plan.Record.MailedFor != "" || plan.Record.AlertState != AlertNone {
		t.Errorf("record = %+v; nothing was mailed", plan.Record)
	}
}

func TestTheSameEpisodeIsNeverMailedTwice(t *testing.T) {
	since := watchNow.Add(-3 * time.Hour)
	v := verdictIn(VerdictNeedsAttention, since)
	for _, at := range []time.Time{watchNow, watchNow.Add(time.Hour), watchNow.Add(48 * time.Hour)} {
		if planWatch(recordedAs(v, mailedFor(since)), v, defaultStoreAlertWait, at).Alert {
			t.Errorf("at %s the episode already mailed was mailed again", at)
		}
	}
	// A move to not connected is the same episode: no second alert.
	gone := verdictIn(VerdictNotConnected, watchNow.Add(-40*time.Minute))
	if planWatch(recordedAs(v, mailedFor(since)), gone, defaultStoreAlertWait, watchNow).Alert {
		t.Error("a move from needs attention to not connected was alerted as a new episode")
	}
}

func TestRecoveryIsMailedOnceAndOnlyAfterAnAlert(t *testing.T) {
	since := watchNow.Add(-2 * time.Hour)
	troubled := verdictIn(VerdictNeedsAttention, since)
	back := verdictIn(VerdictRecording, watchNow)

	plan := planWatch(recordedAs(troubled, mailedFor(since)), back, defaultStoreAlertWait, watchNow)
	if plan.Recover != verdictStamp(since) {
		t.Fatalf("recover = %q, want the mailed episode %q", plan.Recover, verdictStamp(since))
	}
	plan.Record.recovered(plan.Recover)
	if again := planWatch(plan.Record.payload(), back, defaultStoreAlertWait, watchNow.Add(5*time.Minute)); again.Recover != "" {
		t.Error("the recovery was mailed again on the next pass")
	}

	// No alert, no recovery.
	if plan := planWatch(recordedAs(troubled), back, defaultStoreAlertWait, watchNow); plan.Recover != "" {
		t.Error("a store nobody was alerted about was told it is back")
	}
	// An alert the outbox failed still owes its recovery: the owner may have
	// read it on another path, and the record says one was sent.
	failed := recordedAs(troubled, mailedFor(since), func(rec map[string]any) {
		rec["alert"] = map[string]any{"state": AlertFailed, "reason": "target not on the allowlist"}
	})
	if plan := planWatch(failed, back, defaultStoreAlertWait, watchNow); plan.Recover == "" {
		t.Error("an alert staged and then refused by the outbox owes no recovery")
	}
	// An older episode already recovered owes nothing.
	if plan := planWatch(recordedAs(troubled, mailedFor(since), recoveredFor(since)), back, defaultStoreAlertWait, watchNow); plan.Recover != "" {
		t.Error("an episode already recovered was recovered again")
	}
}

func TestRecoveryStillComesWhenTheStorePassesThroughStarting(t *testing.T) {
	since := watchNow.Add(-2 * time.Hour)
	troubled := verdictIn(VerdictNeedsAttention, since)
	starting := verdictIn(VerdictStarting, watchNow)

	plan := planWatch(recordedAs(troubled, mailedFor(since)), starting, defaultStoreAlertWait, watchNow)
	if plan.Recover != "" || plan.Alert {
		t.Fatalf("a starting store was mailed: %+v", plan)
	}
	if plan.Record.MailedFor != verdictStamp(since) {
		t.Fatalf("starting dropped the mailed episode: %+v", plan.Record)
	}
	back := verdictIn(VerdictRecording, watchNow.Add(20*time.Minute))
	later := planWatch(plan.Record.payload(), back, defaultStoreAlertWait, watchNow.Add(20*time.Minute))
	if later.Recover != verdictStamp(since) {
		t.Errorf("recover = %q after starting then recording, want %q", later.Recover, verdictStamp(since))
	}
}

func TestAPausedOrStartingStoreIsNeverMailed(t *testing.T) {
	for _, state := range []string{VerdictPaused, VerdictStarting} {
		v := verdictIn(state, watchNow.Add(-72*time.Hour))
		if plan := planWatch(recordedAs(v), v, defaultStoreAlertWait, watchNow); plan.Alert || plan.Recover != "" {
			t.Errorf("%s for three days: mailed %+v", state, plan)
		}
	}
}

// alertWaitRow is the global variable's row, as the named-row read answers it.
func alertWaitRow(h *testHarness, value string) {
	h.engine.setRows(namedRowsQuery(conceptGlobalVariable, storeAlertWaitVariable), []map[string]any{{
		"id": variableRowID(storeAlertWaitVariable), "name": storeAlertWaitVariable, "value": value, "active": true,
	}})
}

func TestTheWaitIsThirtyMinutesWhenNothingIsSet(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if got := h.conn.storeAlertWait(ctx); got != 30*time.Minute {
		t.Errorf("unset: wait = %s, want 30m", got)
	}
	for _, bad := range []string{"", "soon", "0", "-5", "1441", "2.5"} {
		alertWaitRow(h, bad)
		if got := h.conn.storeAlertWait(ctx); got != 30*time.Minute {
			t.Errorf("%q: wait = %s, want the default 30m", bad, got)
		}
	}
	h.engine.fail[namedRowsQuery(conceptGlobalVariable, storeAlertWaitVariable)] = errString("unreadable")
	if got := h.conn.storeAlertWait(ctx); got != 30*time.Minute {
		t.Errorf("unreadable: wait = %s, want 30m", got)
	}
	delete(h.engine.fail, namedRowsQuery(conceptGlobalVariable, storeAlertWaitVariable))

	// The read builtin says it is the default, and who may change it.
	h.engine.setRows(namedRowsQuery(conceptGlobalVariable, storeAlertWaitVariable), nil)
	integ := NewIntegration(h.conn)
	for who, maySet := range map[string]bool{"owner": true, "developer": false} {
		role := auth.RoleOwner
		if who == "developer" {
			role = auth.RoleDeveloper
		}
		reply := payloadOf(t, must(integ.handleStoreAlertWait(actorCtx("user-1", role), nil, 0)))
		if reply["minutes"] != float64(30) || reply["isDefault"] != true || reply["maySet"] != maySet {
			t.Errorf("%s: reply = %v, want minutes 30, isDefault true, maySet %t", who, reply, maySet)
		}
	}
}

func TestAWaitOfFiveMinutesIsHonoured(t *testing.T) {
	h := newHarness(t)
	alertWaitRow(h, "5")
	wait := h.conn.storeAlertWait(context.Background())
	if wait != 5*time.Minute {
		t.Fatalf("wait = %s, want 5m", wait)
	}
	v := verdictIn(VerdictNeedsAttention, watchNow.Add(-6*time.Minute))
	if !planWatch(recordedAs(v), v, wait, watchNow).Alert {
		t.Error("a store six minutes into needing attention was not alerted under a five-minute wait")
	}
	reply := payloadOf(t, must(NewIntegration(h.conn).handleStoreAlertWait(actorCtx("user-1", auth.RoleOwner), nil, 0)))
	if reply["minutes"] != float64(5) || reply["isDefault"] != false {
		t.Errorf("reply = %v, want minutes 5, isDefault false", reply)
	}
}

func TestZeroAndTwentyFiveHoursAreRefusedWhenSaved(t *testing.T) {
	for n, want := range map[int]bool{0: false, 1: true, 30: true, 1440: true, 1441: false, 1500: false, -1: false} {
		if validAlertMinutes(n) != want {
			t.Errorf("validAlertMinutes(%d) = %t, want %t", n, !want, want)
		}
	}
	h := newHarness(t)
	integ := NewIntegration(h.conn)
	for _, minutes := range []any{0, 1500, float64(1500), 2.5, "30", nil} {
		_, err := integ.handleStoreAlertWaitSet(actorCtx("user-9", auth.RoleOwner), map[string]any{"minutes": minutes}, 0)
		if err == nil {
			t.Errorf("minutes %v was saved", minutes)
		}
	}
	if calls := h.engine.calls(); len(calls) != 0 {
		t.Errorf("a refused value was read or written: %v", calls)
	}
}

func TestOnlyAClusterOwnerSetsTheWait(t *testing.T) {
	h := newHarness(t)
	integ := NewIntegration(h.conn)
	for _, role := range []auth.Role{auth.RoleDeveloper, auth.RoleAdmin, auth.RoleWriter} {
		if _, err := integ.handleStoreAlertWaitSet(actorCtx("user-1", role), map[string]any{"minutes": 45}, 0); err == nil {
			t.Errorf("%s set the alert wait", role)
		}
	}
	if _, err := integ.handleStoreAlertWaitSet(context.Background(), map[string]any{"minutes": 45}, 0); err == nil {
		t.Error("a caller with no identity set the alert wait")
	}
	if calls := h.engine.calls(); len(calls) != 0 {
		t.Fatalf("a refused caller reached the engine: %v", calls)
	}

	reply := payloadOf(t, must(integ.handleStoreAlertWaitSet(actorCtx("user-9", auth.RoleOwner), map[string]any{"minutes": float64(45)}, 0)))
	writes := h.engine.callsTo("setGlobalVariable")
	if len(writes) != 1 {
		t.Fatalf("setGlobalVariable ran %d times, want 1", len(writes))
	}
	for _, want := range []string{`name: "` + storeAlertWaitVariable + `"`, `value: "45"`, `id: "` + variableRowID(storeAlertWaitVariable) + `"`} {
		if !strings.Contains(writes[0], want) {
			t.Errorf("write %s lacks %s", writes[0], want)
		}
	}
	if reply["minutes"] != float64(45) || reply["isDefault"] != false {
		t.Errorf("reply = %v", reply)
	}
}

// watchHarness is the harness store, owned by user-1 whose address the
// identity read answers, with the verdict last recorded as previous.
func watchHarness(t *testing.T, owner string, previous map[string]any) *testHarness {
	t.Helper()
	t.Setenv("MEMQL_DOMAIN", "example.test")
	h := newHarness(t)
	row := map[string]any{
		"id": testStoreID, "domain": watchDomain, "name": "Acme Widgets", "adminTokenRef": "ACME_ADMIN",
		"status": StatusLive, "ownerUserId": owner,
		"health": map[string]any{"ordersFrom": "2026-09-12"},
	}
	if previous != nil {
		row["health"].(map[string]any)["verdict"] = previous
	}
	h.engine.setRows("stores", []map[string]any{row})
	h.engine.setRows("storeById", []map[string]any{row})
	h.engine.setRows("userByIdSystem", []map[string]any{{
		"id": "v1:identity:user:user-1", "role": "writer", "active": true, "primaryEmail": "owner@example.com", "displayName": "Owner",
	}})
	h.now = watchNow
	return h
}

func TestAnAlertTheOutboxRefusedIsNamedOnTheVerdict(t *testing.T) {
	since := watchNow.Add(-2 * time.Hour)
	v := verdictIn(VerdictNeedsAttention, since)
	h := watchHarness(t, "user-1", recordedAs(v, mailedFor(since)))
	// The outbound worker's own refusal (component/outbound/worker.go).
	refusal := fmt.Errorf("target %q not in email allowlist", "owner@example.com").Error()
	h.engine.setRows("outboundRequestById", []map[string]any{{
		"id": alertRequestID(testStoreID, verdictStamp(since)), "status": "failed",
		"target": "owner@example.com", "lastError": refusal,
	}})
	if err := h.conn.watchStoreVerdict(context.Background(), h.store(t), v, defaultStoreAlertWait); err != nil {
		t.Fatal(err)
	}
	reads := h.engine.callsTo("outboundRequestById")
	if len(reads) != 1 || !strings.Contains(reads[0], alertRequestID(testStoreID, verdictStamp(since))) {
		t.Fatalf("outbound reads = %v, want the episode's alert", reads)
	}
	writes := h.engine.callsTo("recordStoreHealth")
	if len(writes) != 1 {
		t.Fatalf("recordStoreHealth ran %d times, want 1", len(writes))
	}
	want := `"alert": {"reason": "target \"the owner's address\" not in email allowlist", "state": "failed"}`
	if !strings.Contains(writes[0], want) {
		t.Errorf("the outbox's refusal is not on the verdict:\n%s\nwant %s", writes[0], want)
	}
	if strings.Contains(writes[0], "@") {
		t.Errorf("an address reached the store row:\n%s", writes[0])
	}

	// The row's target in another case than the text, and a reason that
	// names no address at all.
	for target, lastError := range map[string]string{
		"Owner@Example.com": refusal,
		"owner@example.com": "medium disabled by deployment config (MEMQL_OUTBOUND_EMAIL_ALLOWLIST empty)",
	} {
		hc := watchHarness(t, "user-1", recordedAs(v, mailedFor(since)))
		hc.engine.setRows("outboundRequestById", []map[string]any{{"id": "x", "status": "failed", "target": target, "lastError": lastError}})
		if err := hc.conn.watchStoreVerdict(context.Background(), hc.store(t), v, defaultStoreAlertWait); err != nil {
			t.Fatal(err)
		}
		w := hc.engine.callsTo("recordStoreHealth")
		if len(w) != 1 || strings.Contains(w[0], "@") {
			t.Errorf("target %s, %q: writes = %v; want one, with no address", target, lastError, w)
			continue
		}
		if !strings.Contains(lastError, "@") && !strings.Contains(w[0], `"reason": "`+lastError+`"`) {
			t.Errorf("a reason with no address was changed: %s", w[0])
		}
		if strings.Contains(lastError, "@") && !strings.Contains(w[0], "not in email allowlist") {
			t.Errorf("the scrubbed reason no longer says the allowlist refused it: %s", w[0])
		}
	}
	if len(h.engine.callsTo("stageServerOutboundRequest")) != 0 {
		t.Error("a refused alert was staged again")
	}

	// Delivered reads sent.
	h2 := watchHarness(t, "user-1", recordedAs(v, mailedFor(since)))
	h2.engine.setRows("outboundRequestById", []map[string]any{{"id": "x", "status": "sent"}})
	if err := h2.conn.watchStoreVerdict(context.Background(), h2.store(t), v, defaultStoreAlertWait); err != nil {
		t.Fatal(err)
	}
	if w := h2.engine.callsTo("recordStoreHealth"); len(w) != 1 || !strings.Contains(w[0], `"alert": {"reason": "", "state": "sent"}`) {
		t.Errorf("a delivered alert does not read sent: %v", w)
	}
	// Still on its way: nothing changes, nothing is written.
	h3 := watchHarness(t, "user-1", recordedAs(v, mailedFor(since)))
	h3.engine.setRows("outboundRequestById", []map[string]any{{"id": "x", "status": "retrying"}})
	if err := h3.conn.watchStoreVerdict(context.Background(), h3.store(t), v, defaultStoreAlertWait); err != nil {
		t.Fatal(err)
	}
	if w := h3.engine.callsTo("recordStoreHealth"); len(w) != 0 {
		t.Errorf("an alert still on its way was rewritten: %v", w)
	}
}

func TestAStagedAlertWhoseRowIsGoneIsFailed(t *testing.T) {
	since := watchNow.Add(-2 * time.Hour)
	v := verdictIn(VerdictNeedsAttention, since)
	h := watchHarness(t, "user-1", recordedAs(v, mailedFor(since)))
	h.engine.setRows("outboundRequestById", nil)
	if err := h.conn.watchStoreVerdict(context.Background(), h.store(t), v, defaultStoreAlertWait); err != nil {
		t.Fatal(err)
	}
	want := `"alert": {"reason": "The outbox holds no record of this email.", "state": "failed"}`
	if w := h.engine.callsTo("recordStoreHealth"); len(w) != 1 || !strings.Contains(w[0], want) {
		t.Errorf("writes = %v, want %s", w, want)
	}

	// A read that failed decides nothing.
	he := watchHarness(t, "user-1", recordedAs(v, mailedFor(since)))
	he.engine.fail["outboundRequestById"] = errString("unreadable")
	if err := he.conn.watchStoreVerdict(context.Background(), he.store(t), v, defaultStoreAlertWait); err == nil {
		t.Error("a failed read-back was not reported")
	}
	if w := he.engine.callsTo("recordStoreHealth"); len(w) != 0 {
		t.Errorf("a failed read-back changed the verdict: %v", w)
	}
}

func TestAStoreWithNoOwnerSaysNobodyWasTold(t *testing.T) {
	since := watchNow.Add(-time.Hour)
	v := verdictIn(VerdictNeedsAttention, since)
	for name, setup := range map[string]func(*testHarness){
		"no owner": func(h *testHarness) {},
		"no address": func(h *testHarness) {
			h.engine.setRows("userByIdSystem", []map[string]any{{"id": "v1:identity:user:user-1", "role": "writer", "active": true, "primaryEmail": ""}})
		},
		"no such person": func(h *testHarness) { h.engine.setRows("userByIdSystem", nil) },
		"deactivated": func(h *testHarness) {
			h.engine.setRows("userByIdSystem", []map[string]any{{"id": "v1:identity:user:user-1", "role": "writer", "active": false, "primaryEmail": "owner@example.com"}})
		},
	} {
		owner := "user-1"
		if name == "no owner" {
			owner = ""
		}
		h := watchHarness(t, owner, recordedAs(v))
		setup(h)
		if err := h.conn.watchStoreVerdict(context.Background(), h.store(t), v, defaultStoreAlertWait); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if staged := h.engine.callsTo("stageServerOutboundRequest"); len(staged) != 0 {
			t.Errorf("%s: an email was staged: %v", name, staged)
		}
		writes := h.engine.callsTo("recordStoreHealth")
		want := `"alert": {"reason": "This store names no owner to tell.", "state": "failed"}`
		if len(writes) != 1 || !strings.Contains(writes[0], want) {
			t.Errorf("%s: writes = %v, want %s", name, writes, want)
		}
		if len(writes) == 1 && strings.Contains(writes[0], `"mailedFor": "`+verdictStamp(since)) {
			t.Errorf("%s: the episode reads mailed although nobody was told", name)
		}
	}
}

func TestTheAlertCarriesNoOrderAndNoCustomer(t *testing.T) {
	since := time.Date(2026, 11, 11, 10, 15, 0, 0, time.UTC)
	v := verdictIn(VerdictNeedsAttention, since)
	v.Reasons = append(v.Reasons, VerdictReason{Code: ReasonCheckFailing, Sentence: "The last check of orders failed", Detail: "HTTP 500."})

	// A store that needs attention is still being recorded, in part: the
	// alert says so, and names what holds it there.
	subject, body := alertEmail(v, verdictStamp(since), "https://os.example.test")
	if subject != "acme-widgets.myshopify.com needs attention" {
		t.Errorf("subject = %q", subject)
	}
	wantBody := "MemQL is not recording everything from acme-widgets.myshopify.com, since Wed, 11 Nov 2026 10:15:00 UTC.\n" +
		"\n" +
		"Shopify is not sending every change.\n" +
		"3 changes could not be recorded.\n" +
		"The last check of orders failed.\n" +
		"\n" +
		"Open the storefront's Store page in MemQL OS to see what fixes it: https://os.example.test/\n" +
		"\n" +
		"This email carries no order and nothing about a customer."
	if body != wantBody {
		t.Errorf("body =\n%s\nwant\n%s", body, wantBody)
	}
	if v.Reasons[2].Sentence != "The last check of orders failed" {
		t.Error("the verdict's own sentence was changed")
	}
	if strings.Contains(body, "orders/create: refused") {
		t.Error("the body carries a reason's detail; only its sentence travels")
	}

	// A store that is not connected is not being recorded at all, and carries
	// no reasons.
	gone := verdictIn(VerdictNotConnected, since)
	subject, body = alertEmail(gone, verdictStamp(since), "https://os.example.test")
	if subject != "acme-widgets.myshopify.com is not being recorded" {
		t.Errorf("not connected: subject = %q", subject)
	}
	wantBody = "MemQL has not been recording acme-widgets.myshopify.com since Wed, 11 Nov 2026 10:15:00 UTC.\n" +
		"\n" +
		"Open the storefront's Store page in MemQL OS to see what fixes it: https://os.example.test/\n" +
		"\n" +
		"This email carries no order and nothing about a customer."
	if body != wantBody {
		t.Errorf("not connected: body =\n%s\nwant\n%s", body, wantBody)
	}
	// The state is the one the store is in when the alert is staged: an
	// episode that began as needs attention and is not connected now is told
	// as not connected, since the episode's start.
	if subject, body := alertEmail(gone, verdictStamp(since.Add(-time.Hour)), ""); subject != "acme-widgets.myshopify.com is not being recorded" ||
		body != "MemQL has not been recording acme-widgets.myshopify.com since Wed, 11 Nov 2026 09:15:00 UTC.\n\nThis email carries no order and nothing about a customer." {
		t.Errorf("with no OS address: subject %q, body\n%s", subject, body)
	}

	rsubject, rbody := recoveryEmail(watchDomain, verdictStamp(since), time.Date(2026, 11, 11, 13, 40, 0, 0, time.UTC))
	if rsubject != "acme-widgets.myshopify.com is being recorded again" {
		t.Errorf("recovery subject = %q", rsubject)
	}
	if want := "MemQL is recording acme-widgets.myshopify.com in full again. It needed attention from Wed, 11 Nov 2026 10:15:00 UTC to Wed, 11 Nov 2026 13:40:00 UTC."; rbody != want {
		t.Errorf("recovery body =\n%s\nwant\n%s", rbody, want)
	}

	// Through the executor: the server staging, to the owner's address, with
	// the episode's ids, and nothing about the owner on the store row.
	h := watchHarness(t, "user-1", recordedAs(v))
	h.now = since.Add(45 * time.Minute)
	if err := h.conn.watchStoreVerdict(context.Background(), h.store(t), v, defaultStoreAlertWait); err != nil {
		t.Fatal(err)
	}
	staged := h.engine.callsTo("stageServerOutboundRequest")
	if len(staged) != 1 {
		t.Fatalf("staged %d, want 1 (calls %v)", len(staged), h.engine.calls())
	}
	id := "shopify-store-alert-acme-20261111T101500Z"
	for _, want := range []string{
		`requestId: "` + id + `"`, `dedupeKey: "` + id + `"`, `medium: "email"`, `target: "owner@example.com"`,
		`requestedBy: "shopify:storeWatch"`, `subject: "acme-widgets.myshopify.com needs attention"`,
		"MemQL is not recording everything from acme-widgets.myshopify.com, since Wed, 11 Nov 2026 10:15:00 UTC.",
		"https://os.example.test/",
	} {
		if !strings.Contains(staged[0], want) {
			t.Errorf("staged call lacks %s:\n%s", want, staged[0])
		}
	}
	writes := h.engine.callsTo("recordStoreHealth")
	if len(writes) != 1 {
		t.Fatalf("recordStoreHealth ran %d times", len(writes))
	}
	if strings.Contains(writes[0], "owner@example.com") {
		t.Error("the owner's address was written to the store row")
	}
	if !strings.Contains(writes[0], `"mailedFor": "2026-11-11T10:15:00Z"`) || !strings.Contains(writes[0], `"alert": {"reason": "", "state": "staged"}`) {
		t.Errorf("the staged alert is not recorded:\n%s", writes[0])
	}

	// A store not connected for the wait is staged with its own copy.
	hg := watchHarness(t, "user-1", recordedAs(gone))
	hg.now = since.Add(45 * time.Minute)
	if err := hg.conn.watchStoreVerdict(context.Background(), hg.store(t), gone, defaultStoreAlertWait); err != nil {
		t.Fatal(err)
	}
	if gstaged := hg.engine.callsTo("stageServerOutboundRequest"); len(gstaged) != 1 ||
		!strings.Contains(gstaged[0], `subject: "acme-widgets.myshopify.com is not being recorded"`) ||
		!strings.Contains(gstaged[0], "MemQL has not been recording acme-widgets.myshopify.com since Wed, 11 Nov 2026 10:15:00 UTC.") {
		t.Errorf("not connected staged = %v", gstaged)
	}

	// And the recovery, once the store is recording again.
	back := verdictIn(VerdictRecording, since.Add(3*time.Hour))
	hr := watchHarness(t, "user-1", recordedAs(v, mailedFor(since)))
	hr.now = since.Add(3*time.Hour + 25*time.Minute)
	if err := hr.conn.watchStoreVerdict(context.Background(), hr.store(t), back, defaultStoreAlertWait); err != nil {
		t.Fatal(err)
	}
	rstaged := hr.engine.callsTo("stageServerOutboundRequest")
	if len(rstaged) != 1 || !strings.Contains(rstaged[0], `requestId: "shopify-store-recovered-acme-20261111T101500Z"`) ||
		!strings.Contains(rstaged[0], `subject: "acme-widgets.myshopify.com is being recorded again"`) ||
		!strings.Contains(rstaged[0], "MemQL is recording acme-widgets.myshopify.com in full again. It needed attention from Wed, 11 Nov 2026 10:15:00 UTC to Wed, 11 Nov 2026 13:40:00 UTC.") {
		t.Errorf("recovery staged = %v", rstaged)
	}
	if w := hr.engine.callsTo("recordStoreHealth"); len(w) != 1 || !strings.Contains(w[0], `"recoveredFor": "2026-11-11T10:15:00Z"`) {
		t.Errorf("the recovery is not recorded: %v", w)
	}
}

func TestWatchStoresRefusesACallWithoutInternalOrigin(t *testing.T) {
	h := newHarness(t)
	integ := NewIntegration(h.conn)
	for _, who := range []context.Context{actorCtx("user-9", auth.RoleOwner), actorCtx("user-1", auth.RoleWriter), context.Background()} {
		if _, err := integ.handleWatchStores(who, nil, 0); err == nil {
			t.Error("a call without internal origin ran the watch")
		}
	}
	if calls := h.engine.calls(); len(calls) != 0 {
		t.Fatalf("a refused call reached the engine: %v", calls)
	}
	if _, err := integ.handleWatchStores(auth.ContextWithInternalOrigin(context.Background()), nil, 0); err != nil {
		t.Fatalf("the automation's call was refused: %v", err)
	}
	if len(h.engine.callsTo("storesForConnector")) == 0 {
		t.Error("the automation's call did not list the stores")
	}
}

func TestAnUnchangedVerdictIsNotWrittenAgain(t *testing.T) {
	since := watchNow.Add(-10 * time.Minute)
	v := verdictIn(VerdictNeedsAttention, since)

	// Times that move on every pass -- what is recorded, the connect time --
	// are not part of the record.
	moved := v
	moved.Recorded = []RecordedKind{{Kind: "orders", LastCheckedAt: watchNow.Format(time.RFC3339)}}
	moved.ConnectedAt = watchNow
	if verdictRecordChanged(recordedAs(v), planWatch(recordedAs(v), moved, defaultStoreAlertWait, watchNow).Record) {
		t.Error("a verdict that differs only in times that move counts as changed")
	}
	// What does count: the state, a reason, since, resumedAt, the mail fields.
	for name, edit := range map[string]func(map[string]any){
		"state":           func(r map[string]any) { r["state"] = VerdictStarting },
		"reasons":         func(r map[string]any) { r["reasons"] = []any{ReasonGrantLost} },
		"since":           func(r map[string]any) { r["since"] = watchNow.Format(time.RFC3339) },
		"resumedAt":       func(r map[string]any) { r["resumedAt"] = watchNow.Format(time.RFC3339) },
		"mailedFor":       func(r map[string]any) { r["mailedFor"] = verdictStamp(since) },
		"recoveredFor":    func(r map[string]any) { r["recoveredFor"] = verdictStamp(since) },
		"alert":           func(r map[string]any) { r["alert"] = map[string]any{"state": AlertSent, "reason": ""} },
		"episode":         func(r map[string]any) { r["episode"] = watchNow.Format(time.RFC3339) },
		"firstRecordedAt": func(r map[string]any) { r["firstRecordedAt"] = watchNow.Format(time.RFC3339) },
	} {
		if !verdictRecordChanged(recordedAs(v, edit), recordOf(v, watchedBefore(v))) {
			t.Errorf("a different %s does not count as changed", name)
		}
	}
	if verdictRecordChanged(recordedAs(v), recordOf(v, watchedBefore(v))) {
		t.Error("the same record counts as changed, so the loop above proves nothing")
	}
	if !verdictRecordChanged(nil, recordOf(v, nil)) {
		t.Error("a store with no verdict recorded counts as unchanged")
	}

	// Through the executor: the recorded verdict is what this pass would
	// write, so nothing is written; with none recorded, one write.
	h := watchHarness(t, "user-1", recordedAs(v))
	if err := h.conn.watchStoreVerdict(context.Background(), h.store(t), moved, defaultStoreAlertWait); err != nil {
		t.Fatal(err)
	}
	if w := h.engine.callsTo("recordStoreHealth"); len(w) != 0 {
		t.Errorf("an unchanged verdict was written: %v", w)
	}
	h2 := watchHarness(t, "user-1", nil)
	if err := h2.conn.watchStoreVerdict(context.Background(), h2.store(t), v, defaultStoreAlertWait); err != nil {
		t.Fatal(err)
	}
	w := h2.engine.callsTo("recordStoreHealth")
	if len(w) != 1 {
		t.Fatalf("a first verdict was written %d times, want 1", len(w))
	}
	for _, want := range []string{
		`"ordersFrom": "2026-09-12"`, `"state": "needs attention"`, `"reasons": ["events_not_registered", "changes_not_recorded"]`,
		`"since": "2026-11-11T11:50:00Z"`, `"episode": "2026-11-11T11:50:00Z"`, `"resumedAt": ""`, `"mailedFor": ""`, `"recoveredFor": ""`,
		`"alert": {"reason": "", "state": "none"}`, `"firstRecordedAt": "2026-11-11T12:00:00Z"`,
	} {
		if !strings.Contains(w[0], want) {
			t.Errorf("the first write lacks %s:\n%s", want, w[0])
		}
	}
}

// TestAResumedStoreKeepsItsResumedAtAcrossTwoPasses: what the watch records
// is what the next computation reads, so a store resumed from paused keeps
// the time it resumed -- and the late-check baseline it sets -- on the pass
// after.
func TestAResumedStoreKeepsItsResumedAtAcrossTwoPasses(t *testing.T) {
	store := verdictStore()
	rows := cleanRows(store)
	paused := verdictIn(VerdictPaused, verdictNow.Add(-6*time.Hour))
	store.Health["verdict"] = recordedAs(paused)

	first := verdictInputOf(store, rows)
	first.Previous = store.Health["verdict"].(map[string]any)
	v1 := computeVerdict(first)
	if v1.State != VerdictRecording || !v1.ResumedAt.Equal(verdictNow) {
		t.Fatalf("first pass: state %q resumedAt %s, want recording resumed at %s", v1.State, v1.ResumedAt, verdictNow)
	}
	p1 := planWatch(first.Previous, v1, defaultStoreAlertWait, verdictNow)
	if p1.Record.ResumedAt != verdictNow.Format(time.RFC3339) {
		t.Fatalf("recorded resumedAt = %q, want %s", p1.Record.ResumedAt, verdictNow.Format(time.RFC3339))
	}

	// What the engine hands back is JSON.
	var back map[string]any
	raw, _ := json.Marshal(p1.Record.payload())
	_ = json.Unmarshal(raw, &back)

	second := verdictInputOf(store, rows)
	second.Now = verdictNow.Add(5 * time.Minute)
	second.Previous = back
	v2 := computeVerdict(second)
	if !v2.ResumedAt.Equal(verdictNow) {
		t.Errorf("second pass: resumedAt %s, want it kept at %s", v2.ResumedAt, verdictNow)
	}
	p2 := planWatch(back, v2, defaultStoreAlertWait, second.Now)
	if verdictRecordChanged(back, p2.Record) {
		t.Errorf("the second pass rewrote the verdict: %+v vs %v", p2.Record, back)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// payloadOf is a builtin's one result node, decoded.
func payloadOf(t *testing.T, nodes []memorynodes.MemoryNode) map[string]any {
	t.Helper()
	if len(nodes) != 1 {
		t.Fatalf("%d result nodes, want 1", len(nodes))
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// failingStoreEngine fails the given-up read of one store, which its
// verdict cannot be computed without.
type failingStoreEngine struct {
	*fakeEngine
	store string
}

func (e *failingStoreEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if callName(q) == "inboundRequestsGivenUp" && strings.Contains(q, `"`+storeInboundSource(e.store)+`"`) {
		return nil, errString("the read failed")
	}
	return e.fakeEngine.Execute(ctx, q)
}

func TestOneStoresFailureDoesNotStopThePass(t *testing.T) {
	h := newHarness(t)
	h.now = watchNow
	stores := []map[string]any{
		{"id": "acme", "domain": watchDomain, "adminTokenRef": "ACME_ADMIN", "status": StatusLive, "ownerUserId": "user-1"},
		{"id": "beta", "domain": "beta.myshopify.com", "adminTokenRef": "BETA_ADMIN", "status": StatusLive, "ownerUserId": "user-1"},
	}
	h.engine.setRows("stores", stores)
	h.engine.setRows("storeById", stores[1:])
	engine := &failingStoreEngine{fakeEngine: h.engine, store: "acme"}
	h.conn.engine = engine
	h.conn.stores.engine = engine

	if err := h.conn.WatchStores(context.Background()); err != nil {
		t.Fatalf("a pass with one failing store failed: %v", err)
	}
	writes := h.engine.callsTo("recordStoreHealth")
	if len(writes) != 1 || !strings.Contains(writes[0], `storeId: "beta"`) {
		t.Errorf("writes = %v, want the second store's verdict recorded", writes)
	}

	// A pass that cannot list the stores fails.
	h.engine.fail["stores"] = errString("unlistable")
	if err := h.conn.WatchStores(context.Background()); err == nil {
		t.Error("a pass that could not list the stores reported success")
	}
	delete(h.engine.fail, "stores")

	// A pass whose context is done stops before the next store.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := len(h.engine.calls())
	if err := h.conn.WatchStores(ctx); err == nil {
		t.Error("a cancelled pass reported success")
	}
	for _, q := range h.engine.calls()[before:] {
		if callName(q) == "inboundRequestsGivenUp" {
			t.Fatalf("a cancelled pass went on to compute a verdict: %s", q)
		}
	}
}

func TestAStagingFailureDoesNotLogTheOwnersAddress(t *testing.T) {
	since := watchNow.Add(-time.Hour)
	v := verdictIn(VerdictNeedsAttention, since)
	h := watchHarness(t, "user-1", recordedAs(v))
	for _, text := range []string{
		`refused: stageServerOutboundRequest(target: "owner@example.com")`,
		`refused near 'OWNER@EXAMPLE.COM', at <owner@example.com>.`,
	} {
		h.engine.fail["stageServerOutboundRequest"] = errString(text)
		err := h.conn.watchStoreVerdict(context.Background(), h.store(t), v, defaultStoreAlertWait)
		if err == nil {
			t.Fatal("a failed staging was not reported")
		}
		if strings.Contains(err.Error(), "@") {
			t.Errorf("the error carries an address: %v", err)
		}
	}
	// The episode stays unmailed, so the next pass tries again.
	if w := h.engine.callsTo("recordStoreHealth"); len(w) != 0 {
		t.Errorf("a verdict that only failed to mail was rewritten: %v", w)
	}
}

// passAt runs planWatch as one pass at now over what the last pass recorded,
// and applies a staged alert or recovery as the executor would.
func passAt(t *testing.T, previous map[string]any, state string, since, now time.Time) (watchPlan, map[string]any) {
	t.Helper()
	plan := planWatch(previous, verdictIn(state, since), defaultStoreAlertWait, now)
	rec := plan.Record
	if plan.Alert {
		rec.alertStaged(rec.Episode)
	}
	if plan.Recover != "" {
		rec.recovered(plan.Recover)
	}
	return plan, rec.payload()
}

func TestAMoveBetweenTheTwoStatesIsOneEpisode(t *testing.T) {
	start := watchNow
	rec := recordedAs(verdictIn(VerdictNeedsAttention, start))
	plan, rec := passAt(t, rec, VerdictNeedsAttention, start, start.Add(35*time.Minute))
	if !plan.Alert || plan.Record.Episode != verdictStamp(start) {
		t.Fatalf("35 minutes in: %+v, want one alert for the episode begun at %s", plan, verdictStamp(start))
	}
	moved := start.Add(40 * time.Minute)
	for _, at := range []time.Time{moved, moved.Add(time.Hour), moved.Add(24 * time.Hour)} {
		plan, rec = passAt(t, rec, VerdictNotConnected, moved, at)
		if plan.Alert {
			t.Fatalf("at %s, not connected after needs attention: a second alert", at)
		}
		if plan.Record.Episode != verdictStamp(start) || plan.Record.Since != verdictStamp(moved) {
			t.Fatalf("episode %q since %q, want the episode kept and since moved", plan.Record.Episode, plan.Record.Since)
		}
	}
	back := moved.Add(25 * time.Hour)
	plan, _ = passAt(t, rec, VerdictRecording, back, back)
	if plan.Recover != verdictStamp(start) {
		t.Fatalf("recover = %q, want the episode begun at %s", plan.Recover, verdictStamp(start))
	}
	if _, body := recoveryEmail(watchDomain, plan.Recover, back); !strings.Contains(body, "from "+start.Format(time.RFC1123)+" to") {
		t.Errorf("the recovery does not count from the first state's start: %s", body)
	}
	if plan.Record.Episode != "" {
		t.Errorf("a recording store carries episode %q", plan.Record.Episode)
	}
}

func TestAPauseOrAStartBreaksTheEpisode(t *testing.T) {
	for _, between := range []string{VerdictPaused, VerdictStarting} {
		start := watchNow
		rec := recordedAs(verdictIn(VerdictNeedsAttention, start))
		_, rec = passAt(t, rec, VerdictNeedsAttention, start, start.Add(35*time.Minute))
		_, rec = passAt(t, rec, between, start.Add(40*time.Minute), start.Add(40*time.Minute))
		again := start.Add(50 * time.Minute)
		plan, rec := passAt(t, rec, VerdictNeedsAttention, again, again)
		if plan.Record.Episode != verdictStamp(again) {
			t.Fatalf("%s: episode %q, want a new one begun at %s", between, plan.Record.Episode, verdictStamp(again))
		}
		if plan, _ = passAt(t, rec, VerdictNeedsAttention, again, again.Add(29*time.Minute)); plan.Alert {
			t.Errorf("%s: the new episode was alerted before its wait", between)
		}
		if plan, _ = passAt(t, rec, VerdictNeedsAttention, again, again.Add(30*time.Minute)); !plan.Alert {
			t.Errorf("%s: the new episode was not alerted after its wait", between)
		}
	}
}

func TestTheWaitCountsFromTheEpisodesStart(t *testing.T) {
	start := watchNow
	rec := recordedAs(verdictIn(VerdictNeedsAttention, start))
	moved := start.Add(25 * time.Minute)
	plan, rec := passAt(t, rec, VerdictNotConnected, moved, moved)
	if plan.Alert {
		t.Fatal("alerted 25 minutes in")
	}
	if plan, _ = passAt(t, rec, VerdictNotConnected, moved, start.Add(29*time.Minute)); plan.Alert {
		t.Error("alerted 29 minutes into the episode")
	}
	plan, _ = passAt(t, rec, VerdictNotConnected, moved, start.Add(30*time.Minute))
	if !plan.Alert {
		t.Fatal("not alerted 30 minutes after the episode began, 5 after the move")
	}
	if id := alertRequestID(testStoreID, plan.Record.Episode); id != "shopify-store-alert-acme-20261111T120000Z" {
		t.Errorf("request id = %s, want it stamped with the episode's start", id)
	}
	if _, body := alertEmail(verdictIn(VerdictNotConnected, moved), plan.Record.Episode, ""); !strings.Contains(body, "since "+start.Format(time.RFC1123)+".") {
		t.Errorf("the alert does not say since the episode began: %s", body)
	}
}

func TestARecordWithoutAnEpisodeReadsItsSince(t *testing.T) {
	start := watchNow.Add(-time.Hour)
	old := recordedAs(verdictIn(VerdictNeedsAttention, start), func(r map[string]any) { delete(r, "episode") })
	plan := planWatch(old, verdictIn(VerdictNotConnected, watchNow), defaultStoreAlertWait, watchNow)
	if plan.Record.Episode != verdictStamp(start) || !plan.Alert {
		t.Errorf("episode %q alert %t, want the old record's since %s and an alert", plan.Record.Episode, plan.Alert, verdictStamp(start))
	}
}

func TestANewEpisodeStartsWithNoAlert(t *testing.T) {
	start := watchNow
	failed := recordedAs(verdictIn(VerdictNeedsAttention, start), func(r map[string]any) {
		r["alert"] = map[string]any{"state": AlertFailed, "reason": noOwnerToTell}
	})
	_, paused := passAt(t, failed, VerdictPaused, start.Add(time.Hour), start.Add(time.Hour))
	if a := paused["alert"].(map[string]any); a["state"] != AlertFailed {
		t.Fatalf("a pause cleared the recorded alert: %v", a)
	}
	again := start.Add(2 * time.Hour)
	plan := planWatch(paused, verdictIn(VerdictNeedsAttention, again), defaultStoreAlertWait, again)
	if plan.Record.AlertState != AlertNone || plan.Record.AlertReason != "" {
		t.Errorf("the new episode's first pass reads alert %q %q, want none", plan.Record.AlertState, plan.Record.AlertReason)
	}

	// The reply serves the alert only while the store is not recorded.
	sent := func(state string) Store {
		return Store{Health: map[string]any{"verdict": map[string]any{"state": state, "alert": map[string]any{"state": AlertSent, "reason": ""}}}}
	}
	for state, want := range map[string]string{VerdictRecording: AlertNone, VerdictPaused: AlertNone, VerdictStarting: AlertNone, VerdictNeedsAttention: AlertSent, VerdictNotConnected: AlertSent} {
		if got := recordedAlert(sent(state), state)["state"]; got != want {
			t.Errorf("%s: the reply's alert = %v, want %s", state, got, want)
		}
	}
}

func TestAnInactiveAlertWaitIsNotRead(t *testing.T) {
	h := newHarness(t)
	h.engine.setRows(namedRowsQuery(conceptGlobalVariable, storeAlertWaitVariable), []map[string]any{{
		"id": variableRowID(storeAlertWaitVariable), "name": storeAlertWaitVariable, "value": "5", "active": false,
	}})
	if got := h.conn.storeAlertWait(context.Background()); got != 30*time.Minute {
		t.Errorf("an inactive wait of 5 read as %s, want the default 30m", got)
	}
}

// watchedEngine keeps what a pass records: a recordStoreHealth write
// replaces the store row's health, so the next pass reads it. It answers
// every sync-state read with a domain swept a minute ago, so a store with no
// other reason records.
type watchedEngine struct {
	*fakeEngine
	h      *testHarness
	row    map[string]any
	states map[string]string
}

func (e *watchedEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	switch callName(q) {
	case "syncStateById":
		var id string
		_ = json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(q, "syncStateById(stateId: "), ")")), &id)
		if concept, ok := e.states[id]; ok {
			at := e.h.now.Add(-time.Minute).Format(time.RFC3339)
			return resultWithRows([]map[string]any{{"conceptId": concept, "partitionKey": testStoreID, "backfillStatus": "complete", "lastReconcileAt": at, "lastAttemptAt": at}}), nil
		}
	case "recordStoreHealth":
		body := strings.TrimPrefix(q, "recordStoreHealth(health: ")
		body = body[:strings.LastIndex(body, ", storeId: ")]
		var health map[string]any
		if err := json.Unmarshal([]byte(body), &health); err != nil {
			return nil, fmt.Errorf("the harness could not read the health it was sent: %w", err)
		}
		e.fakeEngine.mu.Lock()
		e.row["health"] = health
		e.fakeEngine.mu.Unlock()
	}
	return e.fakeEngine.Execute(ctx, q)
}

func TestTheWatchKeepsOneEpisodeAcrossPasses(t *testing.T) {
	h := watchHarness(t, "user-1", nil)
	row := h.engine.rows["stores"][0]
	unreadable := map[string]any{"desired": 1, "existing": 0, "created": []any{}, "removed": []any{}, "failed": []any{subscriptionsUnreadable}}
	row["health"].(map[string]any)["subscriptions"] = unreadable
	engine := &watchedEngine{fakeEngine: h.engine, h: h, row: row, states: map[string]string{}}
	for _, concept := range verdictDomains(h.store(t)) {
		engine.states[memqlsync.StateID(concept, ConnectorName, string(memqlsync.DirectionInbound), testStoreID)] = concept
	}
	h.conn.engine = engine
	h.conn.stores.engine = engine

	start := watchNow
	pass := func(at time.Time) {
		t.Helper()
		h.now = at
		if err := h.conn.WatchStores(context.Background()); err != nil {
			t.Fatalf("pass at %s: %v", at, err)
		}
	}
	staged := func() []string { return h.engine.callsTo("stageServerOutboundRequest") }

	pass(start)
	pass(start.Add(35 * time.Minute))
	if got := staged(); len(got) != 1 || !strings.Contains(got[0], "shopify-store-alert-acme-20261111T120000Z") {
		t.Fatalf("35 minutes of needs attention staged %v, want one alert", got)
	}

	// Disconnected: the same episode.
	row["uninstalledAt"] = start.Add(40 * time.Minute).Format(time.RFC3339)
	pass(start.Add(40 * time.Minute))
	pass(start.Add(3 * time.Hour))
	if got := staged(); len(got) != 1 {
		t.Fatalf("not connected after needs attention staged %d emails, want still the one alert", len(got))
	}

	// Recording again: one recovery, from the first state's start.
	delete(row, "uninstalledAt")
	delete(row["health"].(map[string]any), "subscriptions")
	pass(start.Add(4 * time.Hour))
	pass(start.Add(4*time.Hour + 5*time.Minute))
	got := staged()
	if len(got) != 2 || !strings.Contains(got[1], "shopify-store-recovered-acme-20261111T120000Z") ||
		!strings.Contains(got[1], "from "+start.Format(time.RFC1123)+" to") {
		t.Fatalf("staged %v, want one recovery counted from %s", got, start.Format(time.RFC1123))
	}
}

// TestTheReplyServesTheAlertOnlyWhileBothStatesAreInAnEpisode: for up to one
// pass the recorded state and the live one differ, and the reply pairs a live
// state with an alert only when both are part of an episode.
func TestTheReplyServesTheAlertOnlyWhileBothStatesAreInAnEpisode(t *testing.T) {
	recorded := func(state, alert, reason string) Store {
		return Store{Health: map[string]any{"verdict": map[string]any{"state": state, "alert": map[string]any{"state": alert, "reason": reason}}}}
	}
	for name, c := range map[string]struct {
		store Store
		live  string
	}{
		"recorded needs attention, live recording": {recorded(VerdictNeedsAttention, AlertFailed, noOwnerToTell), VerdictRecording},
		"recorded recording, live needs attention": {recorded(VerdictRecording, AlertSent, ""), VerdictNeedsAttention},
	} {
		got := recordedAlert(c.store, c.live)
		if got["state"] != AlertNone || got["reason"] != "" {
			t.Errorf("%s: the reply's alert = %v, want none with no reason", name, got)
		}
	}
	if got := recordedAlert(recorded(VerdictNeedsAttention, AlertFailed, noOwnerToTell), VerdictNotConnected); got["state"] != AlertFailed || got["reason"] != noOwnerToTell {
		t.Errorf("both states in an episode: the reply's alert = %v, want the recorded one", got)
	}
}

// ---- an outage older than the watch is not news (ruling 71) ----

// A store the watch sees for the first time -- no verdict recorded on its row
// -- that is already not connected was uninstalled or purged before anybody
// watched it. Its owner knows. It is recorded as told and as recovered for
// that episode: no alert however long it lasts, and no recovery when the
// store is connected again. The episode ends with the reconnection: a store
// connected again straight into needs attention begins a new one, told after
// the wait.
func TestAStoreFirstSeenAlreadyNotConnectedIsNeverMailed(t *testing.T) {
	for name, previous := range map[string]map[string]any{"no verdict": nil, "an empty verdict": {}} {
		gone := verdictIn(VerdictNotConnected, watchNow)
		plan := planWatch(previous, gone, defaultStoreAlertWait, watchNow)
		if plan.Alert || plan.Recover != "" || plan.ReadBack {
			t.Fatalf("%s: the first pass planned %+v, want nothing", name, plan)
		}
		rec := plan.Record
		if rec.Episode != verdictStamp(watchNow) || rec.MailedFor != rec.Episode || rec.RecoveredFor != rec.Episode || rec.AlertState != AlertNone {
			t.Fatalf("%s: recorded %+v, want the episode told and recovered, with no alert", name, rec)
		}
		// Every later pass of the same outage: nothing, and nothing to write.
		recorded := rec.payload()
		for _, at := range []time.Time{watchNow.Add(31 * time.Minute), watchNow.Add(3 * time.Hour), watchNow.Add(72 * time.Hour)} {
			later := planWatch(recorded, gone, defaultStoreAlertWait, at)
			if later.Alert || later.Recover != "" || later.ReadBack {
				t.Errorf("%s: at %s the pass planned %+v, want nothing", name, at, later)
			}
			if verdictRecordChanged(recorded, later.Record) {
				t.Errorf("%s: at %s the record changed: %+v", name, at, later.Record)
			}
		}
		// Connected again: through starting to recording, and straight to it.
		at := watchNow.Add(96 * time.Hour)
		starting, afterStarting := passAt(t, recorded, VerdictStarting, at, at)
		recording, _ := passAt(t, afterStarting, VerdictRecording, at.Add(10*time.Minute), at.Add(10*time.Minute))
		straight, _ := passAt(t, recorded, VerdictRecording, at, at)
		for step, p := range map[string]watchPlan{"starting": starting, "recording after starting": recording, "recording": straight} {
			if p.Alert || p.Recover != "" || p.ReadBack {
				t.Errorf("%s: reconnected, %s: the pass planned %+v, want nothing", name, step, p)
			}
		}
		// Connected again straight into needs attention: the outage nobody was
		// told about ENDED when the store was connected again, and a store that
		// needs its owner is news. A new episode begins at the reconnection,
		// and its alert is owed after the wait, as for any other.
		reconnected, afterReconnect := passAt(t, recorded, VerdictNeedsAttention, at, at)
		if reconnected.Alert || reconnected.Record.Episode != verdictStamp(at) {
			t.Fatalf("%s: reconnected into needs attention: %+v, want a new episode begun at %s and nothing yet", name, reconnected, verdictStamp(at))
		}
		if early, _ := passAt(t, afterReconnect, VerdictNeedsAttention, at, at.Add(29*time.Minute)); early.Alert {
			t.Errorf("%s: reconnected into needs attention: alerted before the wait", name)
		}
		late, _ := passAt(t, afterReconnect, VerdictNeedsAttention, at, at.Add(30*time.Minute))
		if !late.Alert || late.Record.Episode != verdictStamp(at) {
			t.Errorf("%s: reconnected into needs attention: %+v, want the alert after the wait, dated from the reconnection", name, late)
		}
		if id := alertRequestID(testStoreID, late.Record.Episode); id != alertRequestID(testStoreID, verdictStamp(at)) {
			t.Errorf("%s: the alert is %s, want it named by the reconnection", name, id)
		}
		// Even when no pass saw the needs attention before the wait was over.
		if straightOn, _ := passAt(t, recorded, VerdictNeedsAttention, at, at.Add(time.Hour)); !straightOn.Alert || straightOn.Record.Episode != verdictStamp(at) {
			t.Errorf("%s: first seen needing attention an hour after the reconnection: %+v, want the alert", name, straightOn)
		}
	}

	// A store first seen NEEDING ATTENTION is told after the wait, as before:
	// a release that asks for more permissions is news to every store.
	troubled := verdictIn(VerdictNeedsAttention, watchNow)
	first := planWatch(nil, troubled, defaultStoreAlertWait, watchNow)
	if first.Alert || first.Record.MailedFor != "" || first.Record.RecoveredFor != "" {
		t.Fatalf("a store first seen needing attention: %+v, want nothing yet and nothing marked told", first)
	}
	if !planWatch(first.Record.payload(), troubled, defaultStoreAlertWait, watchNow.Add(30*time.Minute)).Alert {
		t.Error("a store first seen needing attention was not alerted after the wait")
	}

	// And a store the watch knew that then disconnects is told as before.
	gone := verdictIn(VerdictNotConnected, watchNow)
	known := planWatch(watchedBefore(gone), gone, defaultStoreAlertWait, watchNow)
	if known.Record.MailedFor != "" {
		t.Fatalf("a store the watch knew, now not connected: %+v, want an episode not yet told", known.Record)
	}
	if !planWatch(known.Record.payload(), gone, defaultStoreAlertWait, watchNow.Add(30*time.Minute)).Alert {
		t.Error("a store the watch knew was not alerted for disconnecting")
	}

	// A store the watch knew that goes from not connected to needs attention
	// stays in its one episode, mailed or not: only an episode nobody was told
	// about ends with the reconnection.
	back := watchNow.Add(2 * time.Hour)
	unmailed, _ := passAt(t, known.Record.payload(), VerdictNeedsAttention, back, back)
	if unmailed.Record.Episode != verdictStamp(watchNow) || !unmailed.Alert {
		t.Errorf("a known store, not connected and then needing attention, not yet mailed: %+v, want its one episode begun at %s, alerted", unmailed, verdictStamp(watchNow))
	}
	mailed, afterMail := passAt(t, known.Record.payload(), VerdictNotConnected, watchNow, watchNow.Add(30*time.Minute))
	if !mailed.Alert {
		t.Fatalf("a known store not connected for the wait: %+v, want its alert", mailed)
	}
	if moved, _ := passAt(t, afterMail, VerdictNeedsAttention, back, back); moved.Alert || moved.Record.Episode != verdictStamp(watchNow) {
		t.Errorf("a known store, mailed for not connected, then needing attention: %+v, want the one episode kept and no second alert", moved)
	}
}

// The same through the pass: a store uninstalled before the watch existed.
func TestTheWatchStagesNothingForAStoreItFirstSeesNotConnected(t *testing.T) {
	h := watchHarness(t, "user-1", nil)
	row := h.engine.rows["stores"][0]
	row["uninstalledAt"] = watchNow.Add(-90 * 24 * time.Hour).Format(time.RFC3339)
	engine := &watchedEngine{fakeEngine: h.engine, h: h, row: row, states: map[string]string{}}
	connected := Store{ID: testStoreID, Domain: watchDomain, AdminTokenRef: "ACME_ADMIN", Status: StatusLive}
	for _, concept := range verdictDomains(connected) {
		engine.states[memqlsync.StateID(concept, ConnectorName, string(memqlsync.DirectionInbound), testStoreID)] = concept
	}
	h.conn.engine = engine
	h.conn.stores.engine = engine

	start := watchNow
	pass := func(at time.Time) {
		t.Helper()
		h.now = at
		if err := h.conn.WatchStores(context.Background()); err != nil {
			t.Fatalf("pass at %s: %v", at, err)
		}
	}
	staged := func() []string { return h.engine.callsTo("stageServerOutboundRequest") }

	pass(start)
	for _, at := range []time.Time{start.Add(35 * time.Minute), start.Add(3 * time.Hour), start.Add(48 * time.Hour)} {
		pass(at)
	}
	if got := staged(); len(got) != 0 {
		t.Fatalf("a store first seen not connected staged %v, want nothing however long it stays so", got)
	}
	if w := h.engine.callsTo("recordStoreHealth"); len(w) != 1 {
		t.Errorf("the verdict was written %d times over four passes, want once: nothing changes after the first", len(w))
	}

	// Reconnected, and recording.
	delete(row, "uninstalledAt")
	pass(start.Add(49 * time.Hour))
	pass(start.Add(49*time.Hour + 5*time.Minute))
	if got := staged(); len(got) != 0 {
		t.Fatalf("the reconnect of a store nobody was told about staged %v, want nothing", got)
	}

	// The next time it disconnects, the watch knows it: that is news.
	row["uninstalledAt"] = start.Add(50 * time.Hour).Format(time.RFC3339)
	pass(start.Add(50 * time.Hour))
	pass(start.Add(50*time.Hour + 35*time.Minute))
	if got := staged(); len(got) != 1 || !strings.Contains(got[0], "shopify-store-alert-acme-") || !strings.Contains(got[0], "is not being recorded") {
		t.Fatalf("a later disconnection staged %v, want its one alert", got)
	}
}
