package library

import (
	"context"
	"database/sql"
	"encoding/json"
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
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

func TestDocumentEditsOnDifferentReplicasPreserveAuthorityAndVersions(t *testing.T) {
	reachable, err := dbtest.EnsureSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reachable {
		dbtest.Unreachable(t, "document edit replicas", dbtest.DSN(), nil)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	open := func() (*Integration, *memql.MemQLEngine, *bun.DB) {
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
		t.Cleanup(func() { _ = db.Close() })
		e, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		if err = e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		return NewIntegration(e, func() *sql.DB { return db.DB }), e, db
	}
	a, ea, db := open()
	b, _, _ := open()
	marker := fmt.Sprintf("editor-%d", time.Now().UnixNano())
	owner := marker + "-owner"
	ctx := auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: owner}), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	// Seed one owned document, visible to both independent engine instances.
	_, err = ea.Execute(ctx, fmt.Sprintf(`mutation createGeneratedOutput(outputId: %s, title: "Original", body: "original", format: "markdown", source: "user_created")`, langparser.QuoteString(marker)))
	if err != nil {
		t.Fatal(err)
	}
	_ = db

	args := map[string]any{"documentId": marker, "content": "saved", "expectedVersion": 0}
	// Prime A's read cache, then let two separate engine instances contend.
	if doc, err := a.loadGeneratedOutput(ctx, marker); err != nil || doc == nil {
		t.Fatalf("seed: %v %#v", err, doc)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan editResult, 2)
	errs := make(chan error, 2)
	for _, replica := range []*Integration{a, b} {
		wg.Add(1)
		go func(i *Integration) {
			defer wg.Done()
			<-start
			out, err := i.handleEditDocument(ctx, args, 0)
			if err != nil {
				errs <- err
				return
			}
			results <- unwrapResultForReplica(out)
		}(replica)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	wins, conflicts := 0, 0
	for r := range results {
		if r.Conflict {
			conflicts++
		} else if r.NewVersion == 1 {
			wins++
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	doc, err := a.loadGeneratedOutput(memql.ContextWithFreshRead(ctx), marker)
	if err != nil || doc["body"] != "saved" {
		t.Fatalf("clock-skew save invisible: %v %#v", err, doc)
	}
	// A forged owner argument and a reader role must not lend the document owner's authority.
	for _, access := range []*auth.AccessContext{{UserId: marker + "-other", Role: auth.RoleWriter}, {UserId: owner, Role: auth.RoleReader}} {
		forbidden := auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: access.UserId}), access)
		_, err := b.handleEditDocument(forbidden, map[string]any{"documentId": marker, "content": "unauthorized", "ownerUserId": owner, "expectedVersion": 1}, 0)
		if err == nil {
			t.Fatalf("unauthorized caller wrote a document: %#v", access)
		}
	}
	// The two low-level writes cannot bypass the lock through a client DSL call.
	_, err = ea.Execute(ctx, fmt.Sprintf(`mutation appendDocumentVersion(versionId: "forged", documentId: %s, versionNumber: 2, authorKind: "user", versionAt: %s)`, langparser.QuoteString(marker), langparser.QuoteString(time.Now().Format(time.RFC3339Nano))))
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "server") {
		t.Fatalf("direct history write was not refused: %v", err)
	}
}

func unwrapResultForReplica(nodes []memorynodes.MemoryNode) editResult {
	var r editResult
	if len(nodes) == 1 {
		_ = json.Unmarshal(nodes[0].Payload, &r)
	}
	return r
}
