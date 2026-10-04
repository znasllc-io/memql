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
		stores[n] = &connectionStore{engine: NewConfigWriter(e), database: func() *sql.DB { return db.DB }, now: func() time.Time { return clock }}
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
