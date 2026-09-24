package sitepreview

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
)

// readiness_guard_parity_db_test.go -- the readiness answer and the write
// guard give the SAME refusal for the same row (Connect Shopify, D5).
//
// Both call memql.SiteGoLiveRefusal over the store as the deployment reads it
// (memql.BoundStoreAsDeployment); readiness then takes only the domain from the
// caller's own read. A fact one side carries and the other drops is exactly the
// drift the shared rule was meant to rule out -- the OS would offer Go live
// because readiness said yes, and the guard would refuse the click, or the
// reverse. So this measures the pair end to end over real rows: an unattached
// storefront, one bound to a store with no Storefront token, the reachable
// positive, one bound to a connected store, and a caller who may go live but
// cannot read the store.
//
// Postgres-gated; MEMQL_REQUIRE_DB=1 turns the skip into a failure.

type parityEngine struct{ e *memql.MemQLEngine }

func (p parityEngine) Execute(ctx context.Context, q string) (any, error) { return p.e.Execute(ctx, q) }

func parityBoot(t *testing.T) (*memql.MemQLEngine, *bun.DB) {
	t.Helper()
	dsn := dbtest.DSN()
	raw := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = raw.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := raw.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "site preview readiness and guard parity", dsn, err)
		return nil, nil
	}
	db := bun.NewDB(raw, pgdialect.New())
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(db)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init: %v", err)
	}
	return eng, db
}

// parityOwner is a real cluster owner -- NOT a `system:` actor, which the
// preview guard exempts and which would make the guard half of this vacuous.
func parityOwner(userId string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userId, Role: auth.RoleOwner})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

func TestReadinessAndTheGuardGiveTheSameGoLiveRefusal(t *testing.T) {
	eng, db := parityBoot(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner := parityOwner("sitepreview-parity-owner-" + suffix)
	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id = ?", id).Exec(context.Background())
		}
	})
	exec := func(ctx context.Context, q string) error {
		_, err := eng.Execute(ctx, q)
		return err
	}

	tokenless, connected := "parity-tokenless-"+suffix, "parity-connected-"+suffix
	for store, token := range map[string]string{tokenless: "", connected: "SHOPIFY_STOREFRONT_TOKEN"} {
		q := fmt.Sprintf(`mutation createStore(storeId: %s, domain: %s`,
			langparser.QuoteString(store), langparser.QuoteString(store+".myshopify.com"))
		if token != "" {
			q += `, storefrontTokenRef: ` + langparser.QuoteString(token)
		}
		if err := exec(auth.ContextWithInternalOrigin(owner), q+")"); err != nil {
			t.Fatalf("seed store %s: %v", store, err)
		}
		ids = append(ids, "v1:shopify:store:"+store)
	}

	integ := NewIntegration(parityEngine{eng}, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	readiness := func(siteId string) (bool, string) {
		t.Helper()
		nodes, err := integ.handleReadiness(owner, map[string]any{"siteId": siteId}, 0)
		if err != nil || len(nodes) != 1 {
			t.Fatalf("readiness for %s: %v (%d nodes)", siteId, err, len(nodes))
		}
		var out struct {
			CanGoLive     bool `json:"canGoLive"`
			GoLiveRefusal struct {
				Code string `json:"code"`
			} `json:"goLiveRefusal"`
		}
		if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
			t.Fatalf("readiness payload: %v", err)
		}
		return out.CanGoLive, out.GoLiveRefusal.Code
	}

	for _, tc := range []struct {
		name, store, wantCode string
	}{
		{"an unattached storefront", "", memql.PreviewRefusalStorefrontNotConnected},
		{"a storefront bound to a store with no Storefront token", tokenless, memql.PreviewRefusalStorefrontNotConnected},
		{"a storefront bound to a connected store", connected, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			siteId := "v1:platform:site:parity-" + fmt.Sprintf("%d", time.Now().UnixNano())
			ids = append(ids, siteId)
			q := fmt.Sprintf(`mutation createSite(siteId: %s, hostname: %s, kind: "shopify_storefront", bundleRef: %s`,
				langparser.QuoteString(siteId),
				langparser.QuoteString(strings.ReplaceAll(memql.BareShortId(siteId), ":", "-")+".parity.example"),
				langparser.QuoteString("blob://sites/"+memql.BareShortId(siteId)+"/v1/"))
			if tc.store != "" {
				q += `, binding: {"storeId": ` + langparser.QuoteString(tc.store) + `}`
			}
			if err := exec(owner, q+")"); err != nil {
				t.Fatalf("createSite: %v", err)
			}

			canGoLive, code := readiness(siteId)
			guard := exec(owner, fmt.Sprintf(`mutation updateSiteStatus(siteId: %s, status: "live")`, langparser.QuoteString(siteId)))

			if code != tc.wantCode {
				t.Errorf("readiness refused with %q, want %q", code, tc.wantCode)
			}
			if canGoLive != (tc.wantCode == "") {
				t.Errorf("readiness canGoLive = %v with refusal %q", canGoLive, code)
			}
			switch {
			case tc.wantCode == "" && guard != nil:
				t.Errorf("readiness said Go live is legal and the guard refused it: %v", guard)
			case tc.wantCode != "" && (guard == nil || !strings.Contains(guard.Error(), code)):
				t.Errorf("readiness refused with %q and the guard answered %v -- the two disagree", code, guard)
			}
		})
	}
}

// A CALLER WHO MAY GO LIVE BUT CANNOT READ THE STORE gets the same answer from
// both. The preview guard judges the store as the deployment
// (memql.BoundStoreAsDeployment), so its go-live rule accepts a connected
// store whoever clicks; readiness must say the same, or the OS hides a Go live
// that rule would take. What the caller cannot read stays undisclosed: the
// store's domain is not in the answer.
//
// The caller is a writer handed the publish part by a grant: below the store
// tier's developer read floor, and holding what updateSiteStatus asks for.
//
// THE GUARD HALF IS THE PREVIEW GUARD'S RULE OVER ITS OWN READ, not a write
// through executeWrite. That write is refused earlier today, by the binding
// guard re-checking the merged payload's inherited binding as the caller --
// the separate defect the design record lists out of scope (section 15). The
// case below is the one that must hold once that is fixed.
func TestReadinessAndTheGuardAgreeForACallerWhoCannotReadTheStore(t *testing.T) {
	eng, db := parityBoot(t)
	eng.InstallGrantResolution()
	t.Cleanup(func() {
		auth.SetGrantSource(nil)
		auth.SetMembershipSource(nil)
	})
	t.Setenv("MEMQL_DOMAIN", "parity.example")
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	seeder := auth.ContextWithInternalOrigin(parityOwner("sitepreview-parity-seeder-" + suffix))
	writerId := "sitepreview-parity-writer-" + suffix
	writer := auth.ContextWithToken(
		auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: writerId, Role: auth.RoleWriter}),
		&auth.TokenInfo{Subject: writerId})
	store := "parity-unreadable-" + suffix
	siteId := "v1:platform:site:parity-w-" + suffix
	grantId := auth.GrantRowID("user", writerId, auth.VerbExecute, "app:deployables/publish")
	account, group := "parity-org-"+suffix, "parity-group-"+suffix
	membership := group + "-" + writerId
	t.Cleanup(func() {
		for _, id := range []string{"v1:shopify:store:" + store, siteId, "v1:rbac:grant:" + grantId,
			"v1:accounts:account:" + account, "v1:identity:group:" + group, "v1:identity:groupMembership:" + membership} {
			_, _ = db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id = ?", id).Exec(context.Background())
		}
	})
	must := func(ctx context.Context, q string) {
		t.Helper()
		if _, err := eng.Execute(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	must(seeder, fmt.Sprintf(`mutation createStore(storeId: %s, domain: %s, storefrontTokenRef: "SHOPIFY_STOREFRONT_TOKEN")`,
		langparser.QuoteString(store), langparser.QuoteString(store+".myshopify.com")))
	must(seeder, fmt.Sprintf(`mutation writeGrant(grantId: %s, subjectKind: "user", subjectId: %s, verb: "execute", resourceType: "app:deployables/publish", effect: "allow", grantedBy: "sitepreview-parity-seeder")`,
		langparser.QuoteString(grantId), langparser.QuoteString(writerId)))
	// A writer creates a site inside an organization they belong to.
	must(seeder, fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Parity organization")`, langparser.QuoteString(account)))
	must(seeder, fmt.Sprintf(`mutation writeGroup(groupId: %s, name: %s, kind: "account", accountId: %s, status: "active")`,
		langparser.QuoteString(group), langparser.QuoteString(group), langparser.QuoteString(account)))
	must(seeder, fmt.Sprintf(`mutation writeGroupMembership(membershipId: %s, groupId: %s, userId: %s, origin: "added", status: "active")`,
		langparser.QuoteString(membership), langparser.QuoteString(group), langparser.QuoteString(writerId)))
	must(writer, fmt.Sprintf(`mutation createSite(siteId: %s, hostname: %s, accountId: %s, kind: "shopify_storefront", bundleRef: "blob://sites/parity-w/v1/")`,
		langparser.QuoteString(siteId), langparser.QuoteString("parity-w-"+suffix+".parity.example"), langparser.QuoteString(account)))
	must(seeder, fmt.Sprintf(`mutation updateSiteStoreBinding(siteId: %s, storeId: %s)`,
		langparser.QuoteString(siteId), langparser.QuoteString(store)))

	// THE PRECONDITION that makes this the case it claims: the writer reads
	// its own site and not the store it is bound to.
	res, err := eng.Execute(writer, fmt.Sprintf(`query storeById(storeId: %s)`, langparser.QuoteString(store)))
	if err != nil {
		t.Fatalf("storeById as the writer: %v", err)
	}
	if rows := memql.MaterializeRows(res); len(rows) != 0 {
		t.Fatalf("the writer reads %d store rows, want 0 -- the case below is not the one it claims", len(rows))
	}

	integ := NewIntegration(parityEngine{eng}, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	nodes, err := integ.handleReadiness(writer, map[string]any{"siteId": siteId}, 0)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("readiness: %v (%d nodes)", err, len(nodes))
	}
	var out struct {
		CanGoLive     bool   `json:"canGoLive"`
		StoreDomain   string `json:"storeDomain"`
		GoLiveRefusal struct {
			Code string `json:"code"`
		} `json:"goLiveRefusal"`
	}
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatalf("readiness payload: %v", err)
	}
	facts, err := memql.BoundStoreAsDeployment(writer, parityEngine{eng}.Execute, store)
	if err != nil {
		t.Fatalf("the guard's store read: %v", err)
	}
	if guard := memql.SiteGoLiveRefusal(true, facts); !guard.Empty() {
		t.Fatalf("the preview guard's rule refused the writer's go-live onto a connected store: %s", guard.Code)
	}
	if !out.CanGoLive || out.GoLiveRefusal.Code != "" {
		t.Errorf("readiness withheld a go-live the guard accepts: canGoLive=%v refusal=%q", out.CanGoLive, out.GoLiveRefusal.Code)
	}
	if out.StoreDomain != "" {
		t.Errorf("readiness disclosed the domain %q of a store the caller cannot read", out.StoreDomain)
	}
}
