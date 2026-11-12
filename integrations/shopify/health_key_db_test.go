package shopify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// health_key_db_test.go -- two writers of two keys of one store's health,
// against a real engine.
//
// A store's health is one object, and each writer owns one key of it: the
// watch its `verdict`, the subscription pass its `subscriptions`, a connect
// its `ordersFrom`. Writing a key used to be a read of the whole object and a
// write of it back with the key replaced, with nothing between the two: a
// write of another key that landed in between was put back as it had been
// read. For the watch that is an older verdict, whose mailedFor says the
// episode was never mailed, and the owner is mailed again.
//
// The fake engine enforces no row version, so only a real one can say that the
// second write is refused and decided again.

// betweenEngine runs `between` just before a recordStoreHealth reaches the
// engine, for as many of them as `times` says: another writer of the same row,
// landing after this writer's read and before its write.
type betweenEngine struct {
	memql.IntegrationEngineAccess
	times   int
	between func()
}

func (e *betweenEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if callName(q) == "recordStoreHealth" && e.times > 0 {
		e.times--
		e.between()
	}
	return e.IntegrationEngineAccess.Execute(ctx, q)
}

// healthKeyFixture is one store on a real engine, and its health read fresh.
func healthKeyFixture(t *testing.T) (*memql.MemQLEngine, string, func() map[string]any) {
	t.Helper()
	eng, db := authorityEngine(t)
	if eng == nil {
		return nil, "", nil
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	storeID := "dbtest-health-" + suffix
	if _, err := eng.Execute(seedCtx(), renderCall("createStore", map[string]any{
		"storeId": storeID, "domain": storeID + ".myshopify.com", "adminTokenRef": "shopify-admin-" + storeID,
	})); err != nil {
		t.Fatalf("seed the store: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND id LIKE $1`, "%"+suffix+"%")
	})
	plain := NewConnector(eng, slog.New(slog.DiscardHandler), NewStoreRegistry(eng, nil), NewAdminClient())
	health := func() map[string]any {
		t.Helper()
		s, found, err := plain.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), storeID)
		if err != nil || !found {
			t.Fatalf("read the store: found %t, %v", found, err)
		}
		return s.Health
	}
	return eng, storeID, health
}

func TestTwoWritersOfTwoHealthKeysBothSurvive(t *testing.T) {
	eng, storeID, health := healthKeyFixture(t)
	if eng == nil {
		return
	}
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	other := NewConnector(eng, logger, NewStoreRegistry(eng, nil), NewAdminClient())

	// The watch has recorded that the episode was mailed.
	mailed := map[string]any{"state": VerdictNeedsAttention, "mailedFor": "2026-11-11T10:15:00Z", "alert": map[string]any{"state": AlertStaged, "reason": ""}}
	// The subscription pass read the store before that, and writes after it.
	racing := &betweenEngine{IntegrationEngineAccess: eng, times: 1, between: func() {
		if err := other.recordStoreHealthKey(ctx, storeID, "verdict", mailed, nil); err != nil {
			t.Errorf("the other writer: %v", err)
		}
	}}
	pass := NewConnector(racing, logger, NewStoreRegistry(racing, nil), NewAdminClient())
	subscriptions := map[string]any{"desired": 12, "existing": 12}
	if err := pass.recordStoreHealthKey(ctx, storeID, "subscriptions", subscriptions, map[string]any{"subscriptionsCheckedAt": "2026-11-11T10:20:00Z"}); err != nil {
		t.Fatalf("the writer that was overtaken: %v", err)
	}

	got := health()
	verdict, _ := got["verdict"].(map[string]any)
	if mapString(verdict, "mailedFor") != "2026-11-11T10:15:00Z" {
		t.Errorf("the verdict written between the other writer's read and its write is gone: health = %v", got)
	}
	if sub, _ := got["subscriptions"].(map[string]any); mapInt(sub, "desired") != 12 {
		t.Errorf("the overtaken writer's own key did not land: health = %v", got)
	}

	// And the other way round: the verdict's writer is the one overtaken.
	racing.times, racing.between = 1, func() {
		if err := other.recordStoreHealthKey(ctx, storeID, "ordersFrom", "2026-09-12", nil); err != nil {
			t.Errorf("the other writer: %v", err)
		}
	}
	recovered := map[string]any{"state": VerdictRecording, "mailedFor": "2026-11-11T10:15:00Z", "recoveredFor": "2026-11-11T10:15:00Z"}
	if err := pass.recordStoreHealthKey(ctx, storeID, "verdict", recovered, nil); err != nil {
		t.Fatalf("the verdict's writer, overtaken: %v", err)
	}
	got = health()
	verdict, _ = got["verdict"].(map[string]any)
	if got["ordersFrom"] != "2026-09-12" || mapString(verdict, "recoveredFor") != "2026-11-11T10:15:00Z" || got["subscriptions"] == nil {
		t.Errorf("health = %v, want every writer's key: ordersFrom, the verdict as last written, and subscriptions", got)
	}
}

// A row that keeps changing under the write is given up on after a bounded
// number of tries, with an error that says so: nothing was written.
func TestAHealthKeyWriteOvertakenEveryTimeGivesUpAndSaysSo(t *testing.T) {
	eng, storeID, health := healthKeyFixture(t)
	if eng == nil {
		return
	}
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	other := NewConnector(eng, logger, NewStoreRegistry(eng, nil), NewAdminClient())
	n := 0
	racing := &betweenEngine{IntegrationEngineAccess: eng, times: 1 << 20, between: func() {
		n++
		if err := other.recordStoreHealthKey(ctx, storeID, "ordersFrom", fmt.Sprintf("2026-09-%02d", n), nil); err != nil {
			t.Errorf("the other writer: %v", err)
		}
	}}
	pass := NewConnector(racing, logger, NewStoreRegistry(racing, nil), NewAdminClient())

	err := pass.recordStoreHealthKey(ctx, storeID, "verdict", map[string]any{"state": VerdictRecording}, nil)
	if err == nil || !strings.Contains(err.Error(), "kept changing") {
		t.Fatalf("err = %v, want the write given up with its reason", err)
	}
	if n != maxHealthKeyAttempts {
		t.Errorf("the write was tried %d times, want %d", n, maxHealthKeyAttempts)
	}
	if got := health(); got["verdict"] != nil || got["ordersFrom"] != fmt.Sprintf("2026-09-%02d", n) {
		t.Errorf("health = %v, want the other writer's last value and no verdict: a write given up writes nothing", got)
	}
}
