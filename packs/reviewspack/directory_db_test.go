package reviewspack

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
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
	memqldsl "github.com/znasllc-io/memql/dsl"
)

func TestReviewDirectoriesReadCompletePagedSourcesDB(t *testing.T) {
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	ping, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		dbtest.Unreachable(t, "review directory", dbtest.DSN(), err)
	}
	memqldsl.RegisterTree("reviews", Tree())
	t.Cleanup(func() { memqldsl.UnregisterTree("reviews") })
	boot := func() *memql.MemQLEngine {
		if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
			t.Fatal(err)
		}
		engine, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		engine.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		if err = engine.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		return engine
	}
	engine := boot()
	prefix := fmt.Sprintf("review-directory-%d", time.Now().UnixNano())
	owner := "v1:identity:user:" + prefix
	store := prefix + "-store"
	site := "v1:platform:site:" + prefix
	ids := []string{}
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id=?`, id)
		}
	})
	at := time.Now().UTC().Add(-time.Hour)
	insert := func(concept, id string, clock time.Time, payload map[string]any) {
		ids = append(ids, id)
		raw, _ := json.Marshal(payload)
		_, err := db.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" (id,"createdAt","createdBy",concept,type,schema,payload,metadata,provenance) VALUES (?,?,'review-directory-test',?,'object','{}'::jsonb,?::jsonb,'{}'::jsonb,'{"kind":"direct","name":"review-directory-test"}'::jsonb)`, id, clock, concept, string(raw))
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("v1:identity:user", owner, at, map[string]any{"role": "writer", "active": true, "displayName": "Directory reader", "primaryEmail": prefix + "@example.test"})
	insert("v1:platform:site", site, at, map[string]any{"ownerUserId": owner, "kind": "shopify_storefront", "status": "live", "hostname": "reviews.example.test", "binding": map[string]any{"storeId": store}})
	for n := 0; n < 525; n++ {
		id := "v1:reviews:review:" + fmt.Sprintf("%s-%03d", prefix, n)
		title := fmt.Sprintf("Recent %03d", n)
		if n < 25 {
			title = fmt.Sprintf("Earlier NEEDLE%%_ %03d", n)
		}
		insert("v1:reviews:review", id, at.Add(time.Duration(n)*time.Millisecond), map[string]any{"ownerUserId": owner, "storeId": store, "title": title, "body": "Useful review", "productHandle": "boot", "authorName": "Buyer", "authorEmail": "buyer@example.test", "revision": 1})
		insert("v1:reviews:reviewDecision", "v1:reviews:reviewDecision:"+fmt.Sprintf("%s-d-%03d", prefix, n), at.Add(time.Duration(n)*time.Millisecond), map[string]any{"ownerUserId": owner, "storeId": store, "reviewId": id, "action": "escalate", "note": title, "decidedBy": owner, "revision": 1})
	}
	ctx := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter}), &auth.TokenInfo{Subject: owner})
	probe, probeErr := engine.Execute(ctx, fmt.Sprintf("query siteById(siteId:%s)", langparser.QuoteString(site)))
	if probeErr != nil {
		t.Fatal("site query", probeErr)
	}
	if len(memql.MaterializeRows(probe.OutputPayload())) != 1 {
		t.Fatalf("site query returned %#v", probe.OutputPayload())
	}
	provider := &Provider{reader: &engineRowReader{engine: engine}}
	check := func(p *Provider, log bool, search string, total int) {
		after := ""
		seen := map[string]bool{}
		for {
			args := map[string]any{"siteId": site, "storeId": store, "limit": 10, "search": search, "after": after}
			var nodes []memorynodes.MemoryNode
			var err error
			if log {
				nodes, err = p.decisionLog(ctx, args, 0)
			} else {
				nodes, err = p.list(ctx, args, 0)
			}
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			_ = json.Unmarshal(nodes[0].Payload, &result)
			field := "reviews"
			if log {
				field = "decisions"
			}
			rows := result[field].([]any)
			if result["total"] != float64(total) || len(rows) > 10 {
				t.Fatalf("incomplete source or oversized page: total=%v count=%d want=%d", result["total"], len(rows), total)
			}
			if len(nodes[0].Payload) > 16*1024 {
				t.Fatal("oversized directory", len(nodes[0].Payload))
			}
			for _, raw := range rows {
				id := raw.(map[string]any)["id"].(string)
				if seen[id] {
					t.Fatal("duplicate", id)
				}
				seen[id] = true
			}
			after = result["next"].(string)
			if after == "" {
				break
			}
		}
		if len(seen) != total {
			t.Fatal("missing matches", len(seen), total)
		}
	}
	// Both reducers see beyond the old implicit 500-row source ceiling. Search
	// targets older records that a latest-500 prefix would have omitted.
	check(provider, false, "needle%_", 25)
	check(provider, true, "needle%_", 25)
	nodes, err := provider.list(ctx, map[string]any{"siteId": site, "storeId": store, "limit": 10}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nodes[0].Payload), `"total":525`) {
		t.Fatal("full waiting count truncated")
	}
	// The real append mutation changes derived state, visible on another engine.
	decisionID := "v1:reviews:reviewDecision:" + prefix + "-published"
	ids = append(ids, decisionID)
	system := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), auth.SystemActor("review-directory-test")))
	system = auth.ContextWithToken(system, &auth.TokenInfo{Subject: "review-directory-test"})
	_, err = engine.Execute(system, fmt.Sprintf(`mutation appendReviewDecision(decisionId:%s,ownerUserId:%s,reviewId:%s,storeId:%s,action:"publish",revision:1,decidedBy:%s)`, langparser.QuoteString(decisionID), langparser.QuoteString(owner), langparser.QuoteString("v1:reviews:review:"+prefix+"-000"), langparser.QuoteString(store), langparser.QuoteString(owner)))
	if err != nil {
		t.Fatal(err)
	}
	second := &Provider{reader: &engineRowReader{engine: boot()}}
	check(second, false, "needle%_", 24)
	stranger := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: owner + "-stranger", Role: auth.RoleWriter})
	if _, err := second.list(stranger, map[string]any{"siteId": site, "storeId": store, "limit": 10}, 0); err == nil {
		t.Fatal("unowned site admitted")
	}
}
