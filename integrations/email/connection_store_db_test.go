package email

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func TestEmailConnectionStateIsEncryptedAndSerializedAcrossReplicas(t *testing.T) {
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ac", 32))
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	defer db.Close()
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "email connections", dbtest.DSN(), err)
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	stores := make([]*connectionStore, 2)
	var engine *memql.MemQLEngine
	for n := range stores {
		e, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		if err = e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		engine = e
		clock := time.Date(2029-n, 1, 1, 0, 0, 0, 0, time.UTC)
		stores[n] = &connectionStore{engine: NewConfigWriter(e), gateDB: replicaGatePool(t), now: func() time.Time { return clock }}
	}
	key := fmt.Sprintf("session-%d", time.Now().UnixNano())
	const credential = "private-azure-credential-never-returned"
	type state struct {
		Count int
		Token string
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, store := range stores {
		wg.Add(1)
		go func(s *connectionStore) {
			defer wg.Done()
			<-start
			var value state
			results <- s.change(context.Background(), key, &value, func(bool) error { value.Count++; value.Token = credential; return nil })
		}(store)
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	var value state
	found, _, err := stores[0].read(context.Background(), key, &value)
	if err != nil || !found || value.Count != 2 || value.Token != credential {
		t.Fatalf("cross-replica read lost state: found=%v count=%d err=%v", found, value.Count, err)
	}
	var stored []memorynodes.MemoryNode
	if err = db.NewSelect().Model(&stored).Where("concept = ?", "v1:email:connectionState").Where("id = ?", "v1:email:connectionState:"+emailStateID(key)).Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(stored))
	}
	for _, row := range stored {
		if strings.Contains(string(row.Payload), credential) {
			t.Fatal("stored plaintext credential")
		}
	}
	actor := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner", Role: auth.RoleOwner})
	if _, err = engine.Execute(actor, `insert("v1:email:connectionState", id="forged-connection", payload={"encryptedValue":"injected"})`); err == nil {
		t.Fatal("raw owner write bypassed connection checks")
	}
}

// A callback that changes nothing writes nothing (connectionStore.change).
// Sealing draws a fresh nonce, so before this every such callback stored a new
// ciphertext and appended a version of a state that had not moved; the email
// maintenance pass makes several per organization on a schedule. Two stores
// stand in for two replicas: the comparison is against the row, not against
// anything one process remembers.
func TestAnUnchangedEmailConnectionStateIsNotRewritten(t *testing.T) {
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ac", 32))
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	defer db.Close()
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "email connections", dbtest.DSN(), err)
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	stores := make([]*connectionStore, 2)
	for n := range stores {
		e, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		if err = e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		clock := time.Date(2029, 1, 1, 0, n, 0, 0, time.UTC)
		stores[n] = &connectionStore{engine: NewConfigWriter(e), gateDB: replicaGatePool(t), now: func() time.Time { return clock }}
	}
	key := fmt.Sprintf("feedback-%d", time.Now().UnixNano())
	rowID := "v1:email:connectionState:" + emailStateID(key)
	versions := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE concept = ? AND id = ?`, "v1:email:connectionState", rowID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = ? AND id = ?`, "v1:email:connectionState", rowID)
	})
	next := time.Date(2029, 1, 1, 0, 5, 0, 0, time.UTC)

	// A state that does not exist yet is written even by a callback that
	// sets nothing: creating the row is the point of that call.
	var created azureFeedbackConnection
	if err := stores[0].change(context.Background(), key, &created, func(bool) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := versions(); got != 1 {
		t.Fatalf("a first change must create the row, got %d version(s)", got)
	}
	configure := func(s *connectionStore, status string) error {
		var c azureFeedbackConnection
		return s.change(context.Background(), key, &c, func(bool) error {
			c.AccountID, c.PlanID, c.Status, c.Applied, c.NextAttemptAt = "acme", "plan-1", status, true, next
			return nil
		})
	}
	if err := configure(stores[0], "pending"); err != nil {
		t.Fatal(err)
	}
	if got := versions(); got != 2 {
		t.Fatalf("a real change must append, got %d version(s)", got)
	}

	// The same values again, from either replica, and a callback that only
	// reads: three no-ops, no versions.
	if err := configure(stores[1], "pending"); err != nil {
		t.Fatal(err)
	}
	if err := configure(stores[0], "pending"); err != nil {
		t.Fatal(err)
	}
	var observed azureFeedbackConnection
	if err := stores[1].change(context.Background(), key, &observed, func(found bool) error {
		if !found || observed.Status != "pending" {
			t.Errorf("the callback must see the stored state: found=%v status=%q", found, observed.Status)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := versions(); got != 2 {
		t.Fatalf("callbacks that changed nothing appended %d version(s)", got-2)
	}

	// A changed field is still a write, and the other replica reads it.
	if err := configure(stores[1], "ready"); err != nil {
		t.Fatal(err)
	}
	if got := versions(); got != 3 {
		t.Fatalf("a changed status must append, got %d version(s)", got)
	}
	var stored azureFeedbackConnection
	if found, _, err := stores[0].read(context.Background(), key, &stored); err != nil || !found || stored.Status != "ready" {
		t.Fatalf("cross-replica read after a change: found=%v status=%q err=%v", found, stored.Status, err)
	}
}
