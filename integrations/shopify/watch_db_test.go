package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// watch_db_test.go -- the watch's writes against a real engine.
//
// The recording fake parses nothing and enforces nothing, so it cannot say
// that the server staging admits the connector's operator context, that the
// outbox row it stages is the one outboundRequestById reads back, or that a
// verdict recorded through the engine reads back as the same record -- which
// is what keeps an unchanged store from being written every five minutes.

// countingEngine passes every call to the real engine and counts them by name.
type countingEngine struct {
	memql.IntegrationEngineAccess
	mu    stdsync.Mutex
	calls map[string]int
}

func (e *countingEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	e.mu.Lock()
	e.calls[callName(q)]++
	e.mu.Unlock()
	return e.IntegrationEngineAccess.Execute(ctx, q)
}

func (e *countingEngine) count(name string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[name]
}

func TestAWatchedStoreIsStagedForTheServerAndWrittenOnce(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	t.Setenv("MEMQL_DOMAIN", "example.test")
	suffix := fmt.Sprint(time.Now().UnixNano())
	storeID, owner := "dbtest-watch-"+suffix, "dbtest-watch-owner-"+suffix
	domain := storeID + ".myshopify.com"
	payload, _ := json.Marshal(map[string]any{"displayName": "Watch owner", "primaryEmail": owner + "@example.com", "role": "writer", "active": true})
	if _, err := eng.Execute(seedCtx(), fmt.Sprintf("insert(%q,id=%q,payload=%s)", "v1:identity:user", owner, payload)); err != nil {
		t.Fatalf("seed the owner: %v", err)
	}
	if _, err := eng.Execute(seedCtx(), renderCall("createStore", map[string]any{
		"storeId": storeID, "domain": domain, "adminTokenRef": "shopify-admin-" + storeID, "ownerUserId": owner,
	})); err != nil {
		t.Fatalf("seed the store: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM "MemoryNodes" WHERE (concept IN ('v1:shopify:store','v1:identity:user','v1:platform:outboundRequest')) AND (id LIKE $1 OR payload::text LIKE $1)`, "%"+suffix+"%")
	})

	counting := &countingEngine{IntegrationEngineAccess: eng, calls: map[string]int{}}
	c := NewConnector(counting, slog.New(slog.DiscardHandler), NewStoreRegistry(counting, nil), NewAdminClient())
	now := time.Now().UTC().Truncate(time.Second)
	c.now = func() time.Time { return now }
	ctx := context.Background()
	if err := c.recordStoreHealthKey(ctx, storeID, "ordersFrom", "2026-09-12", nil); err != nil {
		t.Fatal(err)
	}
	since := now.Add(-time.Hour)
	v := Verdict{StoreID: storeID, Domain: domain, State: VerdictNeedsAttention, Since: since,
		Reasons: []VerdictReason{{Code: ReasonEventsNotRegistered, Sentence: "Shopify is not sending every change."}}}
	if err := c.recordStoreHealthKey(ctx, storeID, "verdict", recordOf(v, nil).payload(), nil); err != nil {
		t.Fatal(err)
	}
	fresh := func() Store {
		t.Helper()
		s, found, err := c.storeByID(memql.ContextWithFreshRead(operatorContext(ctx)), storeID)
		if err != nil || !found {
			t.Fatalf("read the store: found %t, %v", found, err)
		}
		return s
	}

	// The first pass: the alert is owed, staged through the server mutation,
	// and recorded.
	writes := counting.count("recordStoreHealth")
	if err := c.watchStoreVerdict(ctx, fresh(), v, defaultStoreAlertWait); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if got := counting.count("stageServerOutboundRequest"); got != 1 {
		t.Fatalf("stageServerOutboundRequest ran %d times, want 1", got)
	}
	if got := counting.count("recordStoreHealth") - writes; got != 1 {
		t.Fatalf("the first pass wrote %d times, want 1", got)
	}
	requestID := alertRequestID(storeID, verdictStamp(since))
	res, err := eng.Execute(operatorContext(ctx), renderCall("outboundRequestById", map[string]any{"requestId": requestID}))
	if err != nil {
		t.Fatal(err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		t.Fatalf("outboundRequestById(%s) read %d rows, want the staged alert", requestID, len(rows))
	}
	row := rows[0]
	if mapString(row, "target") != owner+"@example.com" || mapString(row, "medium") != "email" ||
		mapString(row, "requestedBy") != "shopify:storeWatch" || mapString(row, "dedupeKey") != requestID {
		t.Errorf("the staged row = %v", row)
	}
	// The read's shape does not project serverStaged; the stored row says it.
	var serverStaged string
	if err := db.QueryRow(`SELECT COALESCE(payload->>'serverStaged','') FROM "MemoryNodes" WHERE concept = 'v1:platform:outboundRequest' AND id LIKE $1 ORDER BY "createdAt" DESC LIMIT 1`, "%"+requestID).Scan(&serverStaged); err != nil || serverStaged != "true" {
		t.Errorf("the staged row's serverStaged = %q (%v): it was not staged by the server mutation", serverStaged, err)
	}
	if !strings.Contains(mapString(row, "body"), "https://os.example.test/") {
		t.Errorf("the body does not name the OS: %q", mapString(row, "body"))
	}
	store := fresh()
	if got := mapString(store.Health, "ordersFrom"); got != "2026-09-12" {
		t.Errorf("ordersFrom = %q after the verdict was written", got)
	}
	rec := readRecorded(store.Health["verdict"].(map[string]any))
	if rec.MailedFor != verdictStamp(since) || rec.AlertState != AlertStaged {
		t.Errorf("recorded = %+v, want the episode mailed and staged", rec)
	}

	// The second pass, five minutes on: the alert is still pending, the
	// verdict is the same, and nothing is staged or written.
	now = now.Add(5 * time.Minute)
	writes = counting.count("recordStoreHealth")
	if err := c.watchStoreVerdict(ctx, fresh(), v, defaultStoreAlertWait); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := counting.count("outboundRequestById"); got != 1 {
		t.Errorf("the second pass read the alert back %d times, want 1", got)
	}
	if got := counting.count("stageServerOutboundRequest"); got != 1 {
		t.Errorf("the second pass staged again (%d in all)", got)
	}
	if got := counting.count("recordStoreHealth") - writes; got != 0 {
		t.Errorf("the second pass wrote an unchanged verdict %d times", got)
	}
}
