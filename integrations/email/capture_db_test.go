package email

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	"github.com/znasllc-io/memql/component/secret"
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

func TestInboxPagesExecuteAgainstEncryptedStorage(t *testing.T) {
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("bc", 32))
	reachable, err := dbtest.EnsureSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reachable {
		dbtest.Unreachable(t, "captured inbox pages", dbtest.DSN(), nil)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	defer db.Close()
	engine, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	store := NewConfigWriter(engine)
	marker := "inbox-page-" + time.Now().UTC().Format("150405.000000000")
	// Public Inbox capabilities admit operators before installing capture
	// authority. An operator still cannot call the encrypted source queries
	// directly, even with a valid client identity and a seven-day filter.
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper} {
		client := auth.ContextWithClientOrigin(auth.ContextWithToken(configureCtx(role), &auth.TokenInfo{Subject: "op"}))
		for _, query := range []string{
			fmt.Sprintf("query capturedEmailsPage(since: %q, asOf: %q)", time.Now().UTC().Add(-7*24*time.Hour).Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)),
			fmt.Sprintf("query capturedEmailById(messageId: %q, since: %q)", marker, time.Now().UTC().Add(-7*24*time.Hour).Format(time.RFC3339Nano)),
		} {
			if _, err := store.Execute(client, query); err == nil || !strings.Contains(err.Error(), "server-only") {
				t.Fatalf("encrypted source admitted client role %s: %v", role, err)
			}
		}
	}
	ids := make([]string, 0, 25)
	defer func() {
		if len(ids) > 0 {
			_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", "v1:email:capturedMessage").Where("id IN (?)", bun.In(ids)).Exec(context.Background())
		}
	}()
	for n := 0; n < 25; n++ {
		id := fmt.Sprintf("%s-%02d", marker, n)
		ids = append(ids, id)
		account := "acme"
		if n%2 == 1 {
			account = "other"
		}
		item := capturedMessage{ID: id, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), AccountID: account, From: "sender@example.test", Message: Message{To: "reader@example.test", Subject: fmt.Sprintf("%s MESSAGE %02d", marker, n), TextBody: strings.Repeat("private-body-", 1200)}}
		body, _ := json.Marshal(item)
		sealed, _, err := secret.Encrypt(string(body))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Execute(captureContext(context.Background()), renderConfigCall("recordCapturedEmail", map[string]string{"id": id, "encryptedValue": sealed})); err != nil {
			t.Fatal(err)
		}
	}
	integration := NewIntegration(&CaptureSender{store: store}, nil)
	ctx := configureCtx(auth.RoleDeveloper)
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 3; page++ {
		nodes, err := integration.handleInboxPage(ctx, map[string]any{"search": strings.ToUpper(marker), "cursor": cursor}, 0)
		result := inboxPayload(t, nodes, err)
		rows := result["messages"].([]any)
		want := 10
		if page == 2 {
			want = 5
		}
		if len(rows) != want {
			t.Fatalf("page%d=%d want%d", page, len(rows), want)
		}
		for _, row := range rows {
			id := row.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("cursor repeated %s", id)
			}
			seen[id] = true
		}
		cursor = result["nextCursor"].(string)
	}
	if len(seen) != 25 || cursor != "" {
		t.Fatalf("lost page history: %d, cursor=%q", len(seen), cursor)
	}
	nodes, err := integration.handleInboxMessage(ctx, map[string]any{"messageId": ids[0]}, 0)
	result := inboxPayload(t, nodes, err)
	if !strings.Contains(result["message"].(map[string]any)["TextBody"].(string), "private-body") {
		t.Fatal("detail body lost")
	}
}
