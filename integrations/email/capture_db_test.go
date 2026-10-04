package email

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
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

func TestCapturedMailPersistsAcrossEngineReplicas(t *testing.T) {
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("bc", 32))
	reachable, err := dbtest.EnsureSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reachable {
		dbtest.Unreachable(t, "captured email", dbtest.DSN(), nil)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	open := func() (*memql.MemQLEngine, *bun.DB) {
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
		t.Cleanup(func() { _ = db.Close() })
		engine, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		return engine, db
	}
	a, db := open()
	b, _ := open()
	marker := "private-capture-" + time.Now().Format("150405.000000000")
	if err := (&CaptureSender{store: NewConfigWriter(a)}).Send(context.Background(), Message{To: "person@example.test", Subject: marker, TextBody: "Token " + marker}, SendAs{}); err != nil {
		t.Fatal(err)
	}
	var stored []memorynodes.MemoryNode
	if err := db.NewSelect().Model(&stored).Where("concept = ?", "v1:email:capturedMessage").Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(stored) == 0 {
		t.Fatal("capture reported success without persistence")
	}
	for _, row := range stored {
		if strings.Contains(string(row.Payload), marker) {
			t.Fatal("plaintext credential in stored row")
		}
	}
	result, err := NewIntegration(&CaptureSender{store: NewConfigWriter(b)}, nil).handleInbox(configureCtx(auth.RoleOwner), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(result)
	if !strings.Contains(string(body), marker) {
		t.Fatal("message sent on engine A was invisible on engine B")
	}
}
